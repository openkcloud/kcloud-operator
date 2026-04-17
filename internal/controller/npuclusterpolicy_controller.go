/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const finalizerName = "npu.ai/cleanup"

// ownerAnnotation is used to track ownership across namespaces (cross-namespace OwnerReference is not allowed).
const ownerAnnotation = "npu.ai/owner"

// NPUClusterPolicyReconciler reconciles a NPUClusterPolicy object
type NPUClusterPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *NPUClusterPolicyReconciler) createOrUpdateDS(ctx context.Context, desired *appsv1.DaemonSet) error {
	var cur appsv1.DaemonSet
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	if err := r.Client.Get(ctx, key, &cur); apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(cur.Spec, desired.Spec) ||
		!equality.Semantic.DeepEqual(cur.ObjectMeta.Labels, desired.ObjectMeta.Labels) ||
		!equality.Semantic.DeepEqual(cur.ObjectMeta.Annotations, desired.ObjectMeta.Annotations) {
		cur.Spec = desired.Spec
		cur.ObjectMeta.Labels = desired.ObjectMeta.Labels
		cur.ObjectMeta.Annotations = desired.ObjectMeta.Annotations
		return r.Client.Update(ctx, &cur)
	}
	return nil
}

// ConfigMap 공통 보장
func (r *NPUClusterPolicyReconciler) createOrUpdateCM(ctx context.Context, desired *corev1.ConfigMap) error {
	var cur corev1.ConfigMap
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	if err := r.Client.Get(ctx, key, &cur); apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(cur.Data, desired.Data) {
		cur.Data = desired.Data
		return r.Client.Update(ctx, &cur)
	}
	return nil
}

// +kubebuilder:rbac:groups=npu.ai,resources=npuclusterpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=npu.ai,resources=npuclusterpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=npu.ai,resources=npuclusterpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *NPUClusterPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("Reconciling NPUClusterPolicy", "name", req.NamespacedName)

	//-- Get CR
	var policy npuv1alpha1.NPUClusterPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		logger.Error(err, "unable to fetch NPUClusterPolicy")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// -- Finalizer: handle deletion
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&policy, finalizerName) {
			if err := r.cleanupOwnedResources(ctx, &policy); err != nil {
				logger.Error(err, "failed to cleanup owned resources")
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&policy, finalizerName)
			if err := r.Update(ctx, &policy); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// -- Finalizer: add if not present
	if !controllerutil.ContainsFinalizer(&policy, finalizerName) {
		controllerutil.AddFinalizer(&policy, finalizerName)
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	// -- Detector
	if err := r.ensureDetector(ctx, &policy); err != nil {
		logger.Error(err, "failed to ensure Detector")
		r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "Detector", err)
		r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "DetectorFailed", err.Error())
		return ctrl.Result{}, err
	}

	//-- NVIDIA
	if policy.Spec.Nvidia.Enabled {
		logger.Info("Ensuring NVIDIA Device Plugin DaemonSet")
		if err := r.ensureNvidiaDevicePlugin(ctx, &policy); err != nil {
			logger.Error(err, "failed to ensure NVIDIA Device Plugin")
			r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "NvidiaDevicePlugin", err)
			r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "NvidiaDevicePluginFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	//-- Furiosa
	if policy.Spec.Furiosa.Enabled {
		logger.Info("Ensuring Furiosa Device Plugin DaemonSet")
		if err := r.ensureFuriosaDevicePlugin(ctx, &policy); err != nil {
			logger.Error(err, "failed to ensure Furiosa Device Plugin")
			r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "FuriosaDevicePlugin", err)
			r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "FuriosaDevicePluginFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	// -- All ensureXxx succeeded: set Ready=True and record success event
	r.setReadyCondition(ctx, &policy, metav1.ConditionTrue, "AllResourcesReady", "All resources reconciled successfully")
	r.Recorder.Eventf(&policy, corev1.EventTypeNormal, "Reconciled", "Successfully reconciled all resources")

	return ctrl.Result{}, nil
}

// setReadyCondition updates the Ready condition on the policy status.
func (r *NPUClusterPolicyReconciler) setReadyCondition(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Update(ctx, policy); err != nil {
		logf.FromContext(ctx).Error(err, "failed to update NPUClusterPolicy status")
	}
}

// cleanupOwnedResources deletes all DaemonSets and ConfigMaps with the owner annotation matching this policy.
func (r *NPUClusterPolicyReconciler) cleanupOwnedResources(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	ownerValue := fmt.Sprintf("%s/%s", policy.Namespace, policy.Name)

	// Cleanup DaemonSets
	var dsList appsv1.DaemonSetList
	if err := r.List(ctx, &dsList, client.InNamespace("kube-system")); err != nil {
		return err
	}
	for i := range dsList.Items {
		ds := &dsList.Items[i]
		if ds.Annotations[ownerAnnotation] == ownerValue {
			if err := r.Delete(ctx, ds); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	// Cleanup ConfigMaps
	var cmList corev1.ConfigMapList
	if err := r.List(ctx, &cmList, client.InNamespace("kube-system")); err != nil {
		return err
	}
	for i := range cmList.Items {
		cm := &cmList.Items[i]
		if cm.Annotations[ownerAnnotation] == ownerValue {
			if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	return nil
}

// setOwnerAnnotation sets the npu.ai/owner annotation on the given ObjectMeta.
func setOwnerAnnotation(obj *metav1.ObjectMeta, policy *npuv1alpha1.NPUClusterPolicy) {
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[ownerAnnotation] = fmt.Sprintf("%s/%s", policy.Namespace, policy.Name)
}

// -- ensureNvidiaDevicePlugin creates a DaemonSet for NVIDIA
func (r *NPUClusterPolicyReconciler) ensureNvidiaDevicePlugin(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	// 기본 selector (수동 라벨 전략)
	sel := map[string]string{"nvidia.com/gpu.present": "true"}
	if policy.Spec.Nvidia.NodeSelector != nil && len(policy.Spec.Nvidia.NodeSelector) > 0 {
		sel = policy.Spec.Nvidia.NodeSelector
	}

	labels := map[string]string{"app.kubernetes.io/name": "npu-op-nvidia-device-plugin"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "npu-op-nvidia-device-plugin",
			Namespace: "kube-system",
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: sel,
					Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:            "nvidia-device-plugin",
						Image:           policy.Spec.Nvidia.DevicePluginImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolPtr(false)},
						VolumeMounts:    []corev1.VolumeMount{{Name: "device-plugin", MountPath: "/var/lib/kubelet/device-plugins"}},
					}},
					Volumes: []corev1.Volume{{
						Name:         "device-plugin",
						VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"}},
					}},
				},
			},
		},
	}
	setOwnerAnnotation(&ds.ObjectMeta, policy)
	applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)

	if err := r.createOrUpdateDS(ctx, ds); err != nil {
		log.Error(err, "failed to ensure nvidia device plugin daemonset")
		return err
	}
	log.Info("NVIDIA device plugin daemonset ensured")
	return nil
}

// -- ensureFuriosaDevicePlugin creates a DaemonSet for Furiosa
func (r *NPUClusterPolicyReconciler) ensureFuriosaDevicePlugin(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	// nodeSelector
	sel := map[string]string{"furiosa": "true"}
	if policy.Spec.Furiosa.NodeSelector != nil && len(policy.Spec.Furiosa.NodeSelector) > 0 {
		sel = policy.Spec.Furiosa.NodeSelector
	}

	// ConfigMap (옵션)
	if policy.Spec.Furiosa.ConfigMapName != "" {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      policy.Spec.Furiosa.ConfigMapName,
				Namespace: "kube-system",
			},
		}
		setOwnerAnnotation(&cm.ObjectMeta, policy)
		cm.Data = map[string]string{
			"config.yaml": `defaultPe: Fusion
disabledDevices: []
interval: 10`,
		}
		if err := r.createOrUpdateCM(ctx, cm); err != nil {
			log.Error(err, "failed to ensure furiosa device plugin configmap")
			return err
		}
	}

	labels := map[string]string{"app.kubernetes.io/name": "npu-op-furiosa-device-plugin"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "npu-op-furiosa-device-plugin",
			Namespace: "kube-system",
			Labels:    labels,
		},
	}
	setOwnerAnnotation(&ds.ObjectMeta, policy)
	ds.Spec = appsv1.DaemonSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				NodeSelector: sel,
				Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				Containers: []corev1.Container{{
					Name:            "furiosa-device-plugin",
					Image:           policy.Spec.Furiosa.DevicePluginImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"/usr/bin/k8s-device-plugin"},
					Args:            []string{"--config-file", "/etc/furiosa/config.yaml"},
					Env: []corev1.EnvVar{
						{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
						}},
						{Name: "RUST_LOG", Value: "info"},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: boolPtr(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "sys", MountPath: "/sys"},
						{Name: "dev", MountPath: "/dev"},
						{Name: "dp", MountPath: "/var/lib/kubelet/device-plugins"},
						// ConfigMap이 있을 때만 마운트
						// (없으면 이 항목은 빼기)
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}},
					{Name: "dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev"}}},
					{Name: "dp", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"}}},
				},
			},
		},
	}

	if policy.Spec.Furiosa.ConfigMapName != "" {
		// CM 마운트 추가
		ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: "config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: policy.Spec.Furiosa.ConfigMapName},
					},
				},
			},
		)
		ds.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			ds.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "config", MountPath: "/etc/furiosa"},
		)
	}

	applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)

	if err := r.createOrUpdateDS(ctx, ds); err != nil {
		log.Error(err, "failed to ensure furiosa device plugin daemonset")
		return err
	}
	log.Info("Furiosa device plugin daemonset ensured")
	return nil
}

func (r *NPUClusterPolicyReconciler) ensureDetector(ctx context.Context, pol *npuv1alpha1.NPUClusterPolicy) error {
	if pol.Spec.Detector == nil || pol.Spec.Detector.Image == "" {
		return fmt.Errorf("detector image must be specified in NPUClusterPolicy.spec.detector.image")
	}

	image := pol.Spec.Detector.Image
	ds := renderDetectorDS(image)
	setOwnerAnnotation(&ds.ObjectMeta, pol)
	return r.createOrUpdateDS(ctx, ds)
}

func renderDetectorDS(image string) *appsv1.DaemonSet {
	labels := map[string]string{"app.kubernetes.io/name": "npu-op-detector"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "npu-op-detector",
			Namespace: "kube-system",
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "npu-detector",
					Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:            "detector",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env: []corev1.EnvVar{{
							Name:      "NODE_NAME",
							ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
						}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true},
							{Name: "host-dev", MountPath: "/host/dev", ReadOnly: true},
							{Name: "host-var", MountPath: "/host/var", ReadOnly: true},
							{Name: "host-sys", MountPath: "/host/sys", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "host-proc", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/proc"}}},
						{Name: "host-dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev"}}},
						{Name: "host-var", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var"}}},
						{Name: "host-sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}},
					},
				},
			},
		},
	}
	// detector는 /dev를 ReadOnly로 마운트하므로, 드라이버 업그레이드 중 rmmod 간섭을 막기 위해
	// device-plugin과 동일하게 업그레이드 라벨이 붙은 노드에는 스케줄되지 않도록 한다.
	applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)
	return ds
}

// SetupWithManager sets up the controller with the Manager.
func (r *NPUClusterPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&npuv1alpha1.NPUClusterPolicy{}).
		Named("npuclusterpolicy").
		Complete(r)
}

// -- Add
func boolPtr(b bool) *bool {
	return &b
}

// applyDriverUpgradeAntiAffinity는 기존 Affinity를 보존하면서
// driver-upgrading 라벨이 없는 노드에만 스케줄되도록 제약을 추가한다.
func applyDriverUpgradeAntiAffinity(spec *corev1.PodSpec) {
	req := corev1.NodeSelectorRequirement{
		Key:      "npu.ai/driver-upgrading",
		Operator: corev1.NodeSelectorOpDoesNotExist,
	}
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	ns := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if ns == nil {
		ns = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}
	}
	if len(ns.NodeSelectorTerms) == 0 {
		ns.NodeSelectorTerms = append(ns.NodeSelectorTerms, corev1.NodeSelectorTerm{})
	}
	for i := range ns.NodeSelectorTerms {
		ns.NodeSelectorTerms[i].MatchExpressions = append(
			ns.NodeSelectorTerms[i].MatchExpressions, req)
	}
	spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = ns
}
