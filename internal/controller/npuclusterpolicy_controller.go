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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/metrics"
	"kcloud-operator/internal/naming"
	"kcloud-operator/internal/partition/nvidia"
	"kcloud-operator/internal/partition/rngd"
	"kcloud-operator/internal/upgrade"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const finalizerName = "npu.ai/cleanup"

// ownerAnnotation is used to track ownership across namespaces (cross-namespace OwnerReference is not allowed).
const ownerAnnotation = "npu.ai/owner"

// vendorNvidia is the NVIDIA vendor identifier used across the controller package.
const vendorNvidia = "nvidia"

// nvidiaDevicePluginVendorLabel 은 mixed/flat 두 device-plugin DaemonSet 이 공통으로 다는 라벨이다.
// 두 DS 는 selector 충돌을 피하려 서로 다른 app.kubernetes.io/name 을 쓰므로(C-1),
// restartNvidiaDevicePlugin 이 어느 쪽 pod 이든 찾으려면 이 공통 라벨로 List 해야 한다.
const nvidiaDevicePluginVendorLabel = "kcloud.ai/dp-vendor"

// vendorFuriosa is the Furiosa vendor identifier used across the controller package.
const vendorFuriosa = "furiosa"

// Tenstorrent Blackhole device plugin 기본 상수.
// #21: TT plugin 은 공식 벤더 plugin 이 없어 kcloud 가 자체 구현했으므로(3rd party 아님),
// 분류 정합상 namespace 는 operator 네임스페이스(kcloud, naming.OperatorNamespace())로 배치한다.
// DaemonSet 이름 `kcloud-tt-device-plugin`(자체 개발 표기). 타 벤더 plugin 은 3rd party 라 kube-system 유지.
// 이미지는 Harbor(global.registry) 경유로 조립되며,
// helm 미경유(직접 CR) 시를 위해 ttImageDefault 는 registry-relative 기본값을 둔다.
const (
	ttDaemonSetName   = "kcloud-tt-device-plugin"
	ttResourceDefault = "tenstorrent.com/blackhole"
	ttImageDefault    = "tenstorrent/k8s-device-plugin:v0.1.0"
)

// Furiosa 통합 device-plugin(A' 방안) 상수.
// 단일 DS 가 Warboy/RNGD 양 노드에 스케줄되며(공통 PCI vendor 라벨), entrypoint 가
// PCI device ID 로 모델을 감지해 해당 벤더 바이너리를 exec 한다.
const (
	furiosaLegacyWarboyDSName = "furiosa-device-plugin"
	furiosaLegacyRngdDSName   = "furiosa-rngd-device-plugin"
	furiosaUnifiedDSName      = "furiosa-unified-device-plugin"
	furiosaUnifiedImgDefault  = "kcloud/furiosa-unified-device-plugin:0.1.0"
	// 양 Furiosa 노드(Warboy/RNGD)가 공통으로 갖는 자립 라벨(node-manager 부여, PCI 0x1ed2).
	// NFD 비의존 — 통합 DS 공통 셀렉터.
	furiosaFamilyNodeLabel = "kcloud.ai/furiosa-family.present"
)

// NPUClusterPolicyReconciler reconciles a NPUClusterPolicy object
type NPUClusterPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *NPUClusterPolicyReconciler) createOrUpdateDS(ctx context.Context, desired *appsv1.DaemonSet) error {
	var cur appsv1.DaemonSet
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	if err := r.Get(ctx, key, &cur); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(cur.Spec, desired.Spec) ||
		!equality.Semantic.DeepEqual(cur.Labels, desired.Labels) ||
		!equality.Semantic.DeepEqual(cur.Annotations, desired.Annotations) {
		cur.Spec = desired.Spec
		cur.Labels = desired.Labels
		cur.Annotations = desired.Annotations
		return r.Update(ctx, &cur)
	}
	return nil
}

// ConfigMap 공통 보장
func (r *NPUClusterPolicyReconciler) createOrUpdateCM(ctx context.Context, desired *corev1.ConfigMap) error {
	var cur corev1.ConfigMap
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	if err := r.Get(ctx, key, &cur); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(cur.Data, desired.Data) {
		cur.Data = desired.Data
		return r.Update(ctx, &cur)
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
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *NPUClusterPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	metrics.RecordReconcile() // reconcile 호출 시각 기록 (liveness probe 용)
	logger := logf.FromContext(ctx)
	logger.Info("Reconciling NPUClusterPolicy", "name", req.NamespacedName)

	// -- Get CR
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

	// -- node-manager(detector) — 리소스명은 kcloud-node-manager, 이미지/기능은 detector 그대로(#21 리네임)
	if err := r.ensureDetector(ctx, &policy); err != nil {
		logger.Error(err, "failed to ensure kcloud-node-manager")
		r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "kcloud-node-manager", err)
		r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "DetectorFailed", err.Error())
		return ctrl.Result{}, err
	}

	// -- NVIDIA
	if policy.Spec.Nvidia.Enabled {
		logger.Info("Ensuring NVIDIA Device Plugin DaemonSet")
		if err := r.ensureNvidiaDevicePlugin(ctx, &policy); err != nil {
			logger.Error(err, "failed to ensure NVIDIA Device Plugin")
			r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "NvidiaDevicePlugin", err)
			r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "NvidiaDevicePluginFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	// -- NVIDIA GPU 텔레메트리(dcgm-exporter). 토글 off 면 기존 DS 를 제거하므로 항상 호출한다.
	if err := r.ensureDcgmExporter(ctx, &policy); err != nil {
		logger.Error(err, "failed to ensure dcgm-exporter")
		r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "DcgmExporter", err)
		r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "DcgmExporterFailed", err.Error())
		return ctrl.Result{}, err
	}

	// -- Furiosa
	// Unified=true 면 Warboy/RNGD 를 단일 통합 DS 로(A' 방안), 기존 2-DS 는 제거(전환).
	// Unified=false(기본) 면 기존 2-DS 경로 유지 + 통합 DS 제거(롤백).
	if policy.Spec.Furiosa.Enabled && policy.Spec.Furiosa.Unified {
		logger.Info("Ensuring Furiosa Unified Device Plugin DaemonSet")
		if err := r.ensureFuriosaUnifiedDevicePlugin(ctx, &policy); err != nil {
			logger.Error(err, "failed to ensure Furiosa Unified Device Plugin")
			r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "FuriosaUnifiedDevicePlugin", err)
			r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "FuriosaUnifiedDevicePluginFailed", err.Error())
			return ctrl.Result{}, err
		}
		// 전환: 기존 2-DS 제거(존재 시). 통합 DS 스케줄 후 정리해 순단 최소화.
		if err := r.deleteDaemonSetIfExists(ctx, furiosaLegacyWarboyDSName); err != nil {
			logger.Error(err, "failed to delete legacy Warboy DS during unified transition")
		}
		if err := r.deleteDaemonSetIfExists(ctx, furiosaLegacyRngdDSName); err != nil {
			logger.Error(err, "failed to delete legacy RNGD DS during unified transition")
		}
	} else {
		if policy.Spec.Furiosa.Enabled {
			logger.Info("Ensuring Furiosa Device Plugin DaemonSet")
			if err := r.ensureFuriosaDevicePlugin(ctx, &policy); err != nil {
				logger.Error(err, "failed to ensure Furiosa Device Plugin")
				r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "FuriosaDevicePlugin", err)
				r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "FuriosaDevicePluginFailed", err.Error())
				return ctrl.Result{}, err
			}
		}

		// -- Furiosa RNGD (second-gen; separate DS, NFD-based node affinity)
		if policy.Spec.Furiosa.Rngd.Enabled {
			logger.Info("Ensuring Furiosa RNGD Device Plugin DaemonSet")
			if err := r.ensureFuriosaRngdDevicePlugin(ctx, &policy, policy.Spec.Furiosa.Rngd.PartitionPolicy); err != nil {
				logger.Error(err, "failed to ensure Furiosa RNGD Device Plugin")
				r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "FuriosaRngdDevicePlugin", err)
				r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "FuriosaRngdDevicePluginFailed", err.Error())
				return ctrl.Result{}, err
			}
		}
		// 롤백: 통합 DS 가 남아있으면 제거(furiosaUnified=false 복원).
		if err := r.deleteDaemonSetIfExists(ctx, furiosaUnifiedDSName); err != nil {
			logger.Error(err, "failed to delete unified Furiosa DS during rollback")
		}
	}

	// -- Rebellions ATOM+ (separate namespace rbln-system + PSA privileged + ClusterRole/Binding)
	if policy.Spec.Rebellions.Enabled {
		logger.Info("Ensuring Rebellions ATOM+ Device Plugin")
		for _, step := range []struct {
			name string
			fn   func(context.Context, *npuv1alpha1.NPUClusterPolicy) error
		}{
			{"RebellionsNamespace", r.ensureRbllnsNamespace},
			{"RebellionsServiceAccount", r.ensureRbllnsServiceAccount},
			{"RebellionsRBAC", r.ensureRbllnsRBAC},
			{"RebellionsConfigMap", r.ensureRbllnsConfigMap},
			{"RebellionsDevicePlugin", r.ensureRebellionsDevicePlugin},
		} {
			if err := step.fn(ctx, &policy); err != nil {
				logger.Error(err, "failed to ensure Rebellions step", "step", step.name)
				r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", step.name, err)
				r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, step.name+"Failed", err.Error())
				return ctrl.Result{}, err
			}
		}
	}

	// -- Tenstorrent Blackhole (single DaemonSet; nodeSelector=kcloud.ai/tenstorrent.present 자립 라벨, /sys hostPath 감지)
	if policy.Spec.Tenstorrent.Enabled {
		logger.Info("Ensuring Tenstorrent Blackhole Device Plugin DaemonSet")
		if err := r.ensureTenstorrentDevicePlugin(ctx, &policy); err != nil {
			logger.Error(err, "failed to ensure Tenstorrent Device Plugin")
			r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "ReconcileFailed", "Failed to ensure %s: %v", "TenstorrentDevicePlugin", err)
			r.setReadyCondition(ctx, &policy, metav1.ConditionFalse, "TenstorrentDevicePluginFailed", err.Error())
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

// ownedScanNamespaces 는 owner-annotation 소유 리소스를 스캔할 네임스페이스 목록을 반환한다.
// device-plugin(kube-system) + operator 부속(detector, kcloud)이 서로 다른 ns 에 존재하므로,
// #16 이관 과도기·정상 모두 양쪽을 스캔해야 orphan 이 남지 않는다. env 미설정(둘이 동일)이면 1개.
func ownedScanNamespaces() []string {
	opNS := naming.OperatorNamespace()
	if opNS == naming.KubeSystemNamespace {
		return []string{naming.KubeSystemNamespace}
	}
	return []string{naming.KubeSystemNamespace, opNS}
}

// isDevicePluginResource 는 리소스 이름으로 3rd-party device-plugin(nvidia/furiosa/rngd/tt/rbln)을 식별한다.
// 5종 DS/CM 이름이 모두 "device-plugin" 을 포함(operator 관리 driver/toolkit/detector 는 미포함).
// ponytail: 이름 기반. 이름 규약이 갈라지면 생성 시 보존 어노테이션(npu.ai/preserve)으로 승격.
func isDevicePluginResource(name string) bool {
	return strings.Contains(name, "device-plugin")
}

// cleanupOwnedResources deletes all DaemonSets and ConfigMaps with the owner annotation matching this policy.
// device-plugin(3rd party)은 보존한다(#19 무중단 이관 — isDevicePluginResource).
func (r *NPUClusterPolicyReconciler) cleanupOwnedResources(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	ownerValue := fmt.Sprintf("%s/%s", policy.Namespace, policy.Name)

	for _, ns := range ownedScanNamespaces() {
		// Cleanup DaemonSets
		var dsList appsv1.DaemonSetList
		if err := r.List(ctx, &dsList, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range dsList.Items {
			ds := &dsList.Items[i]
			// device-plugin(3rd party, kube-system 고정)은 무중단 이관을 위해 보존 —
			// NCP 삭제(operator 이관/제거) 시 device-plugin 을 지우면 전벤더 allocatable 순단(#16 §5).
			if ds.Annotations[ownerAnnotation] == ownerValue && !isDevicePluginResource(ds.Name) {
				if err := r.Delete(ctx, ds); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
		}

		// Cleanup ConfigMaps
		var cmList corev1.ConfigMapList
		if err := r.List(ctx, &cmList, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range cmList.Items {
			cm := &cmList.Items[i]
			// device-plugin CM 도 보존(DS 가 참조하므로 함께 남긴다).
			if cm.Annotations[ownerAnnotation] == ownerValue && !isDevicePluginResource(cm.Name) {
				if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
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

// kcloud-node-manager ServiceAccount 상수. Namespace 는 operator 부속으로 kcloud(#16)로 이동 —
// naming.OperatorNamespace()(OPERATOR_NAMESPACE env, 미설정 시 kube-system)를 SA/DS 에 공통 사용.
const detectorServiceAccountName = "kcloud-node-manager"

// applyImagePullSecrets 는 policy 레벨 imagePullSecrets 를 pod spec 에 부착한다.
// 빈 목록(미지정)이면 no-op — 노드레벨(containerd) 인증 경로(하위호환)를 유지한다.
// imagePullPolicy 는 건드리지 않는다(air-gap 프리로드용 IfNotPresent 불변식 보존).
func applyImagePullSecrets(spec *corev1.PodSpec, secrets []corev1.LocalObjectReference) {
	if len(secrets) > 0 {
		spec.ImagePullSecrets = secrets
	}
}

// -- ensureNvidiaDevicePlugin creates the mixed(MIG 관리 노드) 와 flat(그 외) device-plugin
// DaemonSet 두 개를 렌더한다 — MPS 근본해결(D-8) 4단계. mixed 는 --mig-strategy=mixed 로 MIG
// 프로파일을, flat 은 그 플래그 없이 순정 GPU 를 광고하며 MigActiveNodeLabel 부재 노드만 맡는다.
func (r *NPUClusterPolicyReconciler) ensureNvidiaDevicePlugin(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	// 기본 selector (자립 라벨 — node-manager 부여, NFD/gpu-operator 비의존)
	sel := map[string]string{"kcloud.ai/nvidia.present": "true"}
	if len(policy.Spec.Nvidia.NodeSelector) > 0 {
		sel = policy.Spec.Nvidia.NodeSelector
	}

	mixed := r.buildNvidiaDevicePluginDS(policy, nvidia.DevicePluginNameMixed, sel, true)
	flat := r.buildNvidiaDevicePluginDS(policy, nvidia.DevicePluginNameFlat, sel, false)

	for _, ds := range []*appsv1.DaemonSet{mixed, flat} {
		setOwnerAnnotation(&ds.ObjectMeta, policy)
		applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)
		applyControlPlaneExclusion(&ds.Spec.Template.Spec)
		applyImagePullSecrets(&ds.Spec.Template.Spec, policy.Spec.ImagePullSecrets)

		// 두 writer 조정: ACPP 가 sharing config 를 소유하면 live 배선을 보존한다(Task 3).
		var live appsv1.DaemonSet
		if err := r.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, &live); err == nil {
			preserveNvidiaSharing(&live, ds)
		}
		if err := r.createOrUpdateDS(ctx, ds); err != nil {
			log.Error(err, "failed to ensure nvidia device plugin daemonset", "name", ds.Name)
			return err
		}
	}
	log.Info("NVIDIA device plugin daemonsets ensured (mixed + flat)")
	return nil
}

// buildNvidiaDevicePluginDS 는 mixed(MIG 관리 노드 전용) 또는 flat(그 외) device-plugin
// DaemonSet 정의를 만든다. selector 는 immutable 이라 mixed(라이브 기존 리소스와 같은 이름)의
// selector 는 과거 값 {app.kubernetes.io/name: nvidia-device-plugin} 그대로 둔다 — 바꾸면 기존
// 클러스터에서 422 로 영구 실패한다. flat 은 자기 이름을 selector 값으로 써 자동으로 비겹침이다.
// 두 DS 는 nvidiaDevicePluginVendorLabel 을 공통으로 달아 restartNvidiaDevicePlugin 의 노드별
// pod 삭제가 어느 쪽이든 찾는다 — MPS 근본해결(D-8).
func (r *NPUClusterPolicyReconciler) buildNvidiaDevicePluginDS(policy *npuv1alpha1.NPUClusterPolicy, name string, baseSel map[string]string, mixed bool) *appsv1.DaemonSet {
	labels := map[string]string{"app.kubernetes.io/name": name, nvidiaDevicePluginVendorLabel: "nvidia"}
	nvidiaRuntime := vendorNvidia
	args := []string{}
	if mixed {
		// worker1 은 A30(MIG 가능)+A2(비 MIG) 혼재 노드라 single 전략은 무효.
		// mixed 는 A30 에 nvidia.com/mig-<profile> 을, A2 에는 기존 nvidia.com/gpu 를 광고한다.
		args = []string{"--mig-strategy=mixed"}
	}

	spec := corev1.PodSpec{
		RuntimeClassName: &nvidiaRuntime,
		Tolerations:      []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		Containers: []corev1.Container{{
			Name:            "nvidia-device-plugin",
			Image:           policy.Spec.Nvidia.DevicePluginImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Args:            args,
			Env: []corev1.EnvVar{
				{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"},
				{Name: "NVIDIA_DRIVER_CAPABILITIES", Value: "all"},
				// MIG(mixed) 조각 메모리/용량 조회는 /dev/nvidia-caps 접근이 필요하다.
				// MIG_MONITOR_DEVICES=all 로 nvidia 런타임이 caps 디바이스를 주입하게 한다.
				{Name: "NVIDIA_MIG_MONITOR_DEVICES", Value: "all"},
			},
			// MIG 조각 조회는 특권이 필요하다("Insufficient Permissions" 회피). NVIDIA GPU
			// Operator 의 device-plugin 도 MIG 에서 privileged 로 동작한다.
			SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
			VolumeMounts:    []corev1.VolumeMount{{Name: "device-plugin", MountPath: "/var/lib/kubelet/device-plugins"}},
		}},
		Volumes: []corev1.Volume{{
			Name:         "device-plugin",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"}},
		}},
	}

	if mixed {
		merged := map[string]string{nvidia.MigActiveNodeLabel: "true"}
		for k, v := range baseSel {
			merged[k] = v
		}
		spec.NodeSelector = merged
	} else {
		keys := make([]string, 0, len(baseSel))
		for k := range baseSel {
			keys = append(keys, k)
		}
		sort.Strings(keys) // map 순회는 무작위 — 정렬 없으면 매 reconcile 마다 렌더가 달라져 DS 가 롤링 재시작한다.
		var terms []corev1.NodeSelectorRequirement
		for _, k := range keys {
			terms = append(terms, corev1.NodeSelectorRequirement{Key: k, Operator: corev1.NodeSelectorOpIn, Values: []string{baseSel[k]}})
		}
		terms = append(terms, corev1.NodeSelectorRequirement{Key: nvidia.MigActiveNodeLabel, Operator: corev1.NodeSelectorOpDoesNotExist})
		spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: terms}},
				},
			},
		}
	}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       spec,
			},
		},
	}
}

// preserveNvidiaSharing 는 live DS 가 ACPP 소유(sharing owner annotation)면 desired 에 배선을 복원한다.
// RNGD 의 RNGD_PARTITION_POLICY 보존과 동일한 2-writer 조정 규칙이다.
func preserveNvidiaSharing(live, desired *appsv1.DaemonSet) {
	owner, owned := live.Annotations[nvidia.SharingOwnerAnnotation]
	if !owned {
		return
	}
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	desired.Annotations[nvidia.SharingOwnerAnnotation] = owner
	mode := npuv1alpha1.SharingModeTimeSliced
	if nvidia.HasMPSVolume(live) {
		mode = npuv1alpha1.SharingModeMPS
	}
	nvidia.WireSharing(desired, mode, nvidia.SharingConfigMapNameOf(live))
}

// -- ensureFuriosaDevicePlugin creates a DaemonSet for Furiosa
func (r *NPUClusterPolicyReconciler) ensureFuriosaDevicePlugin(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	// nodeSelector (자립 라벨 — node-manager 부여, NFD/수동 라벨 비의존)
	sel := map[string]string{"kcloud.ai/furiosa.present": "true"}
	if len(policy.Spec.Furiosa.NodeSelector) > 0 {
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

	labels := map[string]string{"app.kubernetes.io/name": "furiosa-device-plugin"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "furiosa-device-plugin",
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
	applyControlPlaneExclusion(&ds.Spec.Template.Spec)
	applyImagePullSecrets(&ds.Spec.Template.Spec, policy.Spec.ImagePullSecrets)

	if err := r.createOrUpdateDS(ctx, ds); err != nil {
		log.Error(err, "failed to ensure furiosa device plugin daemonset")
		return err
	}
	log.Info("Furiosa device plugin daemonset ensured")
	return nil
}

// rngdDevicePluginArgs returns the binary args for the Furiosa RNGD device plugin DaemonSet.
// 빈 문자열 또는 "none" 이면 --policy flag 미추가 (회귀 0). 그 외 (single-core/dual-core/quad-core)
// 면 `--policy=<value>` 를 append 한다 (libfuriosa-kubernetes PartitioningPolicy enum 과 1:1).
// upstream v2026.1.0 image 는 --policy flag 를 노출하지 않으므로, 비-none 정책은
// partition-aware custom image (helm values.furiosa.rngd.devicePluginImage 로 override) 필요.
func rngdDevicePluginArgs(partitionPolicy string) []string {
	args := []string{"--debugMode"}
	if partitionPolicy != "" && partitionPolicy != "none" {
		args = append(args, "--policy="+partitionPolicy)
	}
	return args
}

// -- ensureFuriosaRngdDevicePlugin creates a DaemonSet for the Furiosa RNGD (2nd-gen) NPU device plugin.
// NodeSelector uses self-reliant label kcloud.ai/rngd.present=true by default (node-manager applied, NFD-free);
// override via Spec.Furiosa.Rngd.NodeSelector.
//
// Pod spec는 Furiosa 공식 helm chart (furiosa-device-plugin:2026.1.0) 의 DaemonSet 템플릿을 따른다:
//   - entrypoint: ./main (working dir 기준). 바이너리가 PCI scan 으로 RNGD 디바이스를 자동 인식하므로
//     --resource-name 등 인자는 불필요.
//   - /dev 전체 + /sys + device-plugin 소켓 디렉토리를 마운트.
//   - Privileged=false, drop=ALL capabilities, priorityClassName=system-node-critical.
//
// Spec.Furiosa.Rngd.ResourceName / ConfigMapName 필드는 CRD 에 남아 있지만, 현재 공식 이미지가
// 이를 자동 처리하므로 이 함수에서 참조하지 않는다 (backward-compat: 필드 존재는 허용).
//
// partitionPolicy: NPUClusterPolicy.Spec.Furiosa.Rngd.PartitionPolicy 의 string 값
// ("none"/"single-core"/"dual-core"/"quad-core"/"" 중 하나).
// 빈 문자열 또는 "none" 이면 args 변경 없이 기존 1:1 카드 동작 유지 (회귀 0).
// 그 외 값이면 `--policy=<value>` 가 binary args 에 append 된다.
//
// 주의 (2026-04-29 worker-pa 분석): upstream image `docker.io/furiosaai/furiosa-device-plugin:2026.1.0`
// 는 cobra binary 가 `--debugMode` flag 만 노출 — `--policy` flag 미지원. cobra 는 unknown flag 거부.
// 따라서 v2026.1.0 image 로 partitionPolicy="dual-core" 운영 시 binary 시작 자체 실패.
// dual-core/single-core/quad-core 운영을 위해서는 별도 빌드된 partition-aware device plugin image
// (helm values.furiosa.rngd.devicePluginImage 로 override) 가 필요하다.
func (r *NPUClusterPolicyReconciler) ensureFuriosaRngdDevicePlugin(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy, partitionPolicy string) error {
	log := logf.FromContext(ctx)

	rngdSpec := policy.Spec.Furiosa.Rngd

	// Image (default to upstream release tag when unset; registry path 은 `furiosaai`, 하이픈 없음)
	image := rngdSpec.DevicePluginImage
	if image == "" {
		image = "docker.io/furiosaai/furiosa-device-plugin:2026.1.0"
	}

	// nodeSelector: 자립 라벨(node-manager 부여, NFD 비의존); override via Spec.Furiosa.Rngd.NodeSelector
	sel := map[string]string{"kcloud.ai/rngd.present": "true"}
	if len(rngdSpec.NodeSelector) > 0 {
		sel = rngdSpec.NodeSelector
	}

	labels := map[string]string{"app.kubernetes.io/name": "furiosa-rngd-device-plugin"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "furiosa-rngd-device-plugin",
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
				NodeSelector:      sel,
				Tolerations:       []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				PriorityClassName: "system-node-critical",
				Containers: []corev1.Container{{
					Name:            "furiosa-device-plugin",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"./main"},
					// --debugMode 는 Furiosa device plugin 이 device 를 인식·등록하는 데
					// 필요 (기본 모드에서는 "couldn't recognize any furiosa devices" 출력
					// 후 종료됨. v1.5 follow-up F-1 반영.
					// partitionPolicy 가 비어있거나 "none" 이면 --policy flag 미추가 (기존 동작).
					// 그 외 (single-core/dual-core/quad-core) 면 partition-aware image 가
					// libfuriosa-kubernetes 의 PartitioningPolicy 와 1:1 매핑되는 flag 로 받는다.
					// (upstream v2026.1.0 image 는 미지원 — partition-aware custom image 필요)
					Args: rngdDevicePluginArgs(partitionPolicy),
					Env: []corev1.EnvVar{
						{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
						}},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged:               boolPtr(false),
						AllowPrivilegeEscalation: boolPtr(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "kubelet-socket", MountPath: "/var/lib/kubelet/device-plugins"},
						{Name: "dev-fs", MountPath: "/dev"},
						{Name: "sys-fs", MountPath: "/sys"},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "kubelet-socket", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"}}},
					{Name: "dev-fs", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev"}}},
					{Name: "sys-fs", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}},
				},
			},
		},
	}

	applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)
	applyControlPlaneExclusion(&ds.Spec.Template.Spec)
	applyImagePullSecrets(&ds.Spec.Template.Spec, policy.Spec.ImagePullSecrets)

	if err := r.createOrUpdateDS(ctx, ds); err != nil {
		log.Error(err, "failed to ensure furiosa rngd device plugin daemonset")
		return err
	}
	log.Info("Furiosa RNGD device plugin daemonset ensured")
	return nil
}

// deleteDaemonSetIfExists deletes a DaemonSet in kube-system by name, ignoring NotFound.
// 통합 전환/롤백 시 반대편 경로의 DS 를 정리하는 데 사용한다.
func (r *NPUClusterPolicyReconciler) deleteDaemonSetIfExists(ctx context.Context, name string) error {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"}}
	if err := r.Delete(ctx, ds); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// -- ensureFuriosaUnifiedDevicePlugin creates a single DaemonSet serving both Warboy and RNGD (A' 방안).
// 동봉 이미지 entrypoint 가 PCI vendor 0x1ed2 + device ID(0x0000=Warboy, 0x0001=RNGD)로 모델을 감지해
// 해당 벤더 바이너리를 exec 한다(proxy 없음, 노드당 한 모델). 리소스명은 각 바이너리가 광고하므로 불변
// (Warboy=beta.furiosa.ai/npu, RNGD=furiosa.ai/rngd). nodeSelector 는 양 노드 공통 PCI vendor 라벨.
// Warboy config 는 ConfigMapName(있으면 /etc/furiosa 마운트), RNGD partition 정책은 Rngd.PartitionPolicy
// 를 RNGD_PARTITION_POLICY env 로 전달한다.
func (r *NPUClusterPolicyReconciler) ensureFuriosaUnifiedDevicePlugin(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	image := policy.Spec.Furiosa.UnifiedDevicePluginImage
	if image == "" {
		image = furiosaUnifiedImgDefault
	}

	// Warboy config ConfigMap (옵션) — 통합 이미지의 Warboy 바이너리가 --config-file 로 참조.
	cmName := policy.Spec.Furiosa.ConfigMapName
	if cmName != "" {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: "kube-system"},
		}
		setOwnerAnnotation(&cm.ObjectMeta, policy)
		cm.Data = map[string]string{
			"config.yaml": `defaultPe: Fusion
disabledDevices: []
interval: 10`,
		}
		if err := r.createOrUpdateCM(ctx, cm); err != nil {
			log.Error(err, "failed to ensure furiosa unified device plugin configmap")
			return err
		}
	}

	labels := map[string]string{"app.kubernetes.io/name": furiosaUnifiedDSName}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      furiosaUnifiedDSName,
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
				// 양 Furiosa 노드 공통 자립 라벨(node-manager 부여)로 스케줄. NFD 비의존.
				NodeSelector:      map[string]string{furiosaFamilyNodeLabel: "true"},
				Tolerations:       []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				PriorityClassName: "system-node-critical",
				Containers: []corev1.Container{{
					Name:            "furiosa-device-plugin",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					// Command 미지정 — 통합 이미지 ENTRYPOINT(entrypoint.sh)가 PCI 감지 후 exec.
					Env: []corev1.EnvVar{
						{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
						}},
						{Name: "RUST_LOG", Value: "info"},
						{Name: "RNGD_PARTITION_POLICY", Value: policy.Spec.Furiosa.Rngd.PartitionPolicy},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged:               boolPtr(false),
						AllowPrivilegeEscalation: boolPtr(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "sys", MountPath: "/sys"},
						{Name: "dev", MountPath: "/dev"},
						{Name: "dp", MountPath: "/var/lib/kubelet/device-plugins"},
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

	if cmName != "" {
		ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: "config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
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
	applyControlPlaneExclusion(&ds.Spec.Template.Spec)
	applyImagePullSecrets(&ds.Spec.Template.Spec, policy.Spec.ImagePullSecrets)

	// 두 writer 조정: ACPP 가 partition env 를 소유하면(owner 어노테이션) live 값을 보존한다.
	// (ACPP 가 RNGD_PARTITION_POLICY 의 권위 — Task 9. 어노테이션 없으면 기존 동작.)
	var live appsv1.DaemonSet
	if err := r.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, &live); err == nil {
		if owner, owned := live.Annotations[rngd.PartitionOwnerAnnotation]; owned {
			if v := unifiedEnvValue(&live, "RNGD_PARTITION_POLICY"); v != "" {
				setUnifiedEnvValue(ds, "RNGD_PARTITION_POLICY", v)
			}
			if ds.Annotations == nil {
				ds.Annotations = map[string]string{}
			}
			ds.Annotations[rngd.PartitionOwnerAnnotation] = owner
		}
	}

	if err := r.createOrUpdateDS(ctx, ds); err != nil {
		log.Error(err, "failed to ensure furiosa unified device plugin daemonset")
		return err
	}
	log.Info("Furiosa unified device plugin daemonset ensured")
	return nil
}

// unifiedEnvValue 는 furiosa-device-plugin 컨테이너에서 name env 값을 읽는다(없으면 "").
func unifiedEnvValue(ds *appsv1.DaemonSet, name string) string {
	for _, ctr := range ds.Spec.Template.Spec.Containers {
		if ctr.Name != furiosaLegacyWarboyDSName {
			continue
		}
		for _, e := range ctr.Env {
			if e.Name == name {
				return e.Value
			}
		}
	}
	return ""
}

// setUnifiedEnvValue 는 furiosa-device-plugin 컨테이너의 name env 를 value 로 설정한다(없으면 append).
func setUnifiedEnvValue(ds *appsv1.DaemonSet, name, value string) {
	for ci := range ds.Spec.Template.Spec.Containers {
		ctr := &ds.Spec.Template.Spec.Containers[ci]
		if ctr.Name != furiosaLegacyWarboyDSName {
			continue
		}
		for ei := range ctr.Env {
			if ctr.Env[ei].Name == name {
				ctr.Env[ei].Value = value
				return
			}
		}
		ctr.Env = append(ctr.Env, corev1.EnvVar{Name: name, Value: value})
		return
	}
}

// ensureTenstorrentDevicePlugin creates a DaemonSet for the Tenstorrent Blackhole NPU device plugin.
// Pod spec: /dev + /sys(ReadOnly) + /var/lib/kubelet/device-plugins 마운트, privileged=true (PCIe 디바이스 직접 접근).
// /sys 는 plugin discovery 가 /sys/class/tenstorrent 를 참조하므로 ReadOnly 로 마운트해 장치 감지 가능하게 함 (commit eddfb49).
// NodeSelector: 기본값 tenstorrent-blackhole=true (수동 라벨 전략); Spec.Tenstorrent.NodeSelector 로 재정의 가능.
// ResourceName: 기본값 "tenstorrent.com/blackhole"; Spec.Tenstorrent.ResourceName 으로 재정의 가능 (TT_RESOURCE_NAME env 로 주입).
func (r *NPUClusterPolicyReconciler) ensureTenstorrentDevicePlugin(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	tt := policy.Spec.Tenstorrent

	// 이미지: spec 지정 없으면 registry-relative 기본값 사용 (helm 은 global.registry 로 조립)
	image := tt.DevicePluginImage
	if image == "" {
		image = ttImageDefault
	}

	// resourceName: spec 지정 없으면 기본값 사용 (device plugin 이 env 에서 읽음)
	resourceName := tt.ResourceName
	if resourceName == "" {
		resourceName = ttResourceDefault
	}

	// nodeSelector: 자립 라벨 기본값(node-manager 부여, 수동 라벨 비의존); spec 지정 시 override
	sel := map[string]string{"kcloud.ai/tenstorrent.present": "true"}
	if len(tt.NodeSelector) > 0 {
		sel = tt.NodeSelector
	}

	labels := map[string]string{"app.kubernetes.io/name": ttDaemonSetName}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ttDaemonSetName,
			Namespace: naming.OperatorNamespace(),
			Labels:    labels,
		},
	}
	setOwnerAnnotation(&ds.ObjectMeta, policy)
	ds.Spec = appsv1.DaemonSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				NodeSelector:      sel,
				Tolerations:       []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				PriorityClassName: "system-node-critical",
				Containers: []corev1.Container{{
					Name:            "tenstorrent-device-plugin",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Env: []corev1.EnvVar{
						{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
						}},
						// device plugin 이 K8s 리소스 이름을 env 에서 읽을 수 있도록 주입
						{Name: "TT_RESOURCE_NAME", Value: resourceName},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: boolPtr(true),
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "kubelet-socket", MountPath: "/var/lib/kubelet/device-plugins"},
						{Name: "dev-fs", MountPath: "/dev"},
						// plugin discovery 가 /sys/class/tenstorrent 를 참조 (commit eddfb49)
						{Name: "sys-fs", MountPath: "/sys", ReadOnly: true},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "kubelet-socket", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"}}},
					{Name: "dev-fs", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev"}}},
					{Name: "sys-fs", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}},
				},
			},
		},
	}

	applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)
	applyControlPlaneExclusion(&ds.Spec.Template.Spec)
	applyImagePullSecrets(&ds.Spec.Template.Spec, policy.Spec.ImagePullSecrets)

	if err := r.createOrUpdateDS(ctx, ds); err != nil {
		log.Error(err, "failed to ensure tenstorrent device plugin daemonset")
		return err
	}
	log.Info("Tenstorrent Blackhole device plugin daemonset ensured")
	return nil
}

// Rebellions ATOM+ default constants (Rebellions 공식 daemonset.yaml / configmap.yaml 스펙 준수)
// 2026-04-22: namespace/name 을 기존 `npu-op-*` 컨벤션 (kube-system) 으로 통일.
// Rebellions 공식 default `rbln-system/rbln-device-plugin` 대신 Warboy/RNGD/NVIDIA 와 동일 위치.
const (
	rbllnsNamespaceDefault     = "kube-system"
	rbllnsServiceAccountName   = "rbln-device-plugin"
	rbllnsClusterRoleName      = "rbln-device-plugin"
	rbllnsConfigMapNameDefault = "rbln-device-plugin-config"
	rbllnsDaemonSetName        = "rbln-device-plugin"
	rbllnsResourceNameDefault  = "ATOM"
	rbllnsResourcePrefixDfault = "rebellions.ai"
)

// rbllnsResolveNamespace returns the configured Rebellions namespace or default.
func rbllnsResolveNamespace(policy *npuv1alpha1.NPUClusterPolicy) string {
	if policy.Spec.Rebellions.Namespace != "" {
		return policy.Spec.Rebellions.Namespace
	}
	return rbllnsNamespaceDefault
}

// ensureRbllnsNamespace creates/ensures the Rebellions device plugin namespace with Pod
// Security Admission (PSA) privileged labels when a dedicated namespace is configured.
//
// 2026-04-22: `kube-system` 및 기타 시스템 네임스페이스는 early-return 한다. 이유:
//  1. kube-system 은 이미 존재하며 kubelet/kube-proxy 등 시스템 컴포넌트를 위한 PSA
//     설정이 클러스터 운영자에 의해 관리됨. operator 가 PSA 라벨을 덮어쓰면 전체
//     클러스터 보안 경계가 흔들림.
//  2. PSA 가 기본 `privileged` 가 아닌 `baseline`/`restricted` 로 설정된 환경이라도,
//     kube-system 에 배포되는 DS/DaemonSet 들은 대개 예외 규칙 (system-node-critical
//     priority, legitimate privileged) 으로 허용된다. 별도 namespace label 갱신이
//     필요하지 않다.
func (r *NPUClusterPolicyReconciler) ensureRbllnsNamespace(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)
	name := rbllnsResolveNamespace(policy)

	// Skip system namespaces — do not mutate their PSA labels or attempt Create.
	if name == "kube-system" || name == "kube-public" || name == "kube-node-lease" || name == "default" {
		log.V(1).Info("Rebellions namespace is a system namespace — skipping Create/PSA label mutation", "namespace", name)
		return nil
	}

	desiredLabels := map[string]string{
		"pod-security.kubernetes.io/enforce": "privileged",
		"pod-security.kubernetes.io/audit":   "privileged",
		"pod-security.kubernetes.io/warn":    "privileged",
	}

	var cur corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &cur); apierrors.IsNotFound(err) {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: desiredLabels,
			},
		}
		setOwnerAnnotation(&ns.ObjectMeta, policy)
		if err := r.Create(ctx, ns); err != nil {
			log.Error(err, "failed to create rebellions namespace")
			return err
		}
		return nil
	} else if err != nil {
		return err
	}

	// Merge PSA labels (do not overwrite user-added labels)
	if cur.Labels == nil {
		cur.Labels = map[string]string{}
	}
	changed := false
	for k, v := range desiredLabels {
		if cur.Labels[k] != v {
			cur.Labels[k] = v
			changed = true
		}
	}
	if changed {
		if err := r.Update(ctx, &cur); err != nil {
			log.Error(err, "failed to update rebellions namespace PSA labels")
			return err
		}
	}
	return nil
}

// ensureRbllnsServiceAccount creates the ServiceAccount used by the Rebellions
// device plugin DaemonSet.
func (r *NPUClusterPolicyReconciler) ensureRbllnsServiceAccount(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)
	ns := rbllnsResolveNamespace(policy)

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rbllnsServiceAccountName,
			Namespace: ns,
		},
		ImagePullSecrets: policy.Spec.ImagePullSecrets,
	}
	setOwnerAnnotation(&sa.ObjectMeta, policy)

	var cur corev1.ServiceAccount
	key := types.NamespacedName{Name: sa.Name, Namespace: sa.Namespace}
	if err := r.Get(ctx, key, &cur); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, sa); err != nil {
			log.Error(err, "failed to create rebellions serviceaccount")
			return err
		}
		return nil
	} else if err != nil {
		return err
	}
	// imagePullSecrets 는 spec 변경으로 갱신될 수 있으므로 기존 SA 와 동기화한다.
	if !equality.Semantic.DeepEqual(cur.ImagePullSecrets, sa.ImagePullSecrets) {
		cur.ImagePullSecrets = sa.ImagePullSecrets
		if err := r.Update(ctx, &cur); err != nil {
			log.Error(err, "failed to update rebellions serviceaccount imagePullSecrets")
			return err
		}
	}
	return nil
}

// ensureRbllnsRBAC creates the ClusterRole and ClusterRoleBinding granting the
// Rebellions device plugin ServiceAccount permissions to read/patch nodes
// (kubelet socket management).
func (r *NPUClusterPolicyReconciler) ensureRbllnsRBAC(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)
	ns := rbllnsResolveNamespace(policy)

	desiredRules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list", "watch", "patch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"events"},
			Verbs:     []string{"create", "patch"},
		},
	}

	// ClusterRole
	var curCR rbacv1.ClusterRole
	if err := r.Get(ctx, types.NamespacedName{Name: rbllnsClusterRoleName}, &curCR); apierrors.IsNotFound(err) {
		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: rbllnsClusterRoleName},
			Rules:      desiredRules,
		}
		setOwnerAnnotation(&cr.ObjectMeta, policy)
		if err := r.Create(ctx, cr); err != nil {
			log.Error(err, "failed to create rebellions clusterrole")
			return err
		}
	} else if err != nil {
		return err
	} else if !equality.Semantic.DeepEqual(curCR.Rules, desiredRules) {
		curCR.Rules = desiredRules
		if err := r.Update(ctx, &curCR); err != nil {
			log.Error(err, "failed to update rebellions clusterrole")
			return err
		}
	}

	// ClusterRoleBinding
	desiredSubjects := []rbacv1.Subject{{
		Kind:      "ServiceAccount",
		Name:      rbllnsServiceAccountName,
		Namespace: ns,
	}}
	desiredRoleRef := rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "ClusterRole",
		Name:     rbllnsClusterRoleName,
	}

	var curCRB rbacv1.ClusterRoleBinding
	if err := r.Get(ctx, types.NamespacedName{Name: rbllnsClusterRoleName}, &curCRB); apierrors.IsNotFound(err) {
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: rbllnsClusterRoleName},
			RoleRef:    desiredRoleRef,
			Subjects:   desiredSubjects,
		}
		setOwnerAnnotation(&crb.ObjectMeta, policy)
		if err := r.Create(ctx, crb); err != nil {
			log.Error(err, "failed to create rebellions clusterrolebinding")
			return err
		}
		return nil
	} else if err != nil {
		return err
	}
	// RoleRef is immutable; only sync subjects (SA namespace may have changed via spec override).
	if !equality.Semantic.DeepEqual(curCRB.Subjects, desiredSubjects) {
		curCRB.Subjects = desiredSubjects
		if err := r.Update(ctx, &curCRB); err != nil {
			log.Error(err, "failed to update rebellions clusterrolebinding subjects")
			return err
		}
	}
	return nil
}

// ensureRbllnsConfigMap creates the ConfigMap consumed by the Rebellions device plugin
// (/etc/pcidp/config.json). Device IDs cover all 12 ATOM+ variants (1eff:0010~1251).
func (r *NPUClusterPolicyReconciler) ensureRbllnsConfigMap(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	ns := rbllnsResolveNamespace(policy)
	name := policy.Spec.Rebellions.ConfigMapName
	if name == "" {
		name = rbllnsConfigMapNameDefault
	}
	resourceName := policy.Spec.Rebellions.ResourceName
	if resourceName == "" {
		resourceName = rbllnsResourceNameDefault
	}
	resourcePrefix := policy.Spec.Rebellions.ResourcePrefix
	if resourcePrefix == "" {
		resourcePrefix = rbllnsResourcePrefixDfault
	}

	configJSON := fmt.Sprintf(
		`{"resourceList":[{"resourceName":"%s","resourcePrefix":"%s","deviceType":"accelerator","selectors":{"vendors":["1eff"],"devices":["0010","0011","1020","1021","1120","1121","1150","1151","1220","1221","1250","1251"],"drivers":["rebellions"]}}]}`,
		resourceName, resourcePrefix,
	)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Data: map[string]string{
			"config.json": configJSON,
		},
	}
	setOwnerAnnotation(&cm.ObjectMeta, policy)
	return r.createOrUpdateCM(ctx, cm)
}

// ensureRebellionsDevicePlugin creates the DaemonSet running the Rebellions device
// plugin. Spec mirrors Rebellions 공식 daemonset.yaml (hostNetwork, privileged, 9 volumes).
// Note: host-driver-usr-bin mounts /usr/local/bin (Phase 0-A 실측: rbln-stat/rbln-smi 는 /usr/local/bin 에 위치).
func (r *NPUClusterPolicyReconciler) ensureRebellionsDevicePlugin(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	ns := rbllnsResolveNamespace(policy)
	cmName := policy.Spec.Rebellions.ConfigMapName
	if cmName == "" {
		cmName = rbllnsConfigMapNameDefault
	}
	image := policy.Spec.Rebellions.DevicePluginImage

	// 자립 라벨(node-manager 부여, 수동 라벨 비의존) + arch 게이트.
	sel := map[string]string{
		"kubernetes.io/arch":           "amd64",
		"kcloud.ai/rebellions.present": "true",
	}
	if len(policy.Spec.Rebellions.NodeSelector) > 0 {
		sel = policy.Spec.Rebellions.NodeSelector
	}

	labels := map[string]string{"app.kubernetes.io/name": "rbln-device-plugin"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rbllnsDaemonSetName,
			Namespace: ns,
			Labels:    labels,
		},
	}
	setOwnerAnnotation(&ds.ObjectMeta, policy)
	ds.Spec = appsv1.DaemonSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				HostNetwork:        true,
				ServiceAccountName: rbllnsServiceAccountName,
				NodeSelector:       sel,
				Tolerations: []corev1.Toleration{
					{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
					{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
				},
				Containers: []corev1.Container{{
					Name:            "rbln-device-plugin",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Args:            []string{"--log-dir=device-plugin", "--log-level=10"},
					SecurityContext: &corev1.SecurityContext{
						Privileged: boolPtr(true),
						RunAsUser:  int64Ptr(0),
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("40Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("200Mi"),
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "devicesock", MountPath: "/var/lib/kubelet/device-plugins"},
						{Name: "plugins-registry", MountPath: "/var/lib/kubelet/plugins_registry"},
						{Name: "log", MountPath: "/var/log"},
						{Name: "config-volume", MountPath: "/etc/pcidp"},
						{Name: "device-info", MountPath: "/var/run/k8s.cni.cncf.io/devinfo/dp"},
						{Name: "host-usr-bin", MountPath: "/host/usr/bin", ReadOnly: true},
						{Name: "host-driver-usr-bin", MountPath: "/host/driver/usr/bin", ReadOnly: true},
						// F-A2 (2026-04-22): Rebellions device plugin 이미지는 `rbln-smi`
						// 를 /usr/bin/rbln-smi 에서 찾음. host 는 /usr/local/bin/rbln-smi
						// 이므로 single-file hostPath 로 경로 bridge. 없어도 동작 자체는
						// 되나 RSD group 생성 경로에서 "rbln-smi not found" 경고 제거.
						{Name: "host-rbln-smi", MountPath: "/usr/bin/rbln-smi", ReadOnly: true},
						{Name: "host-dev", MountPath: "/dev"},
						{Name: "host-sys", MountPath: "/sys"},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "devicesock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"}}},
					{Name: "plugins-registry", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/plugins_registry"}}},
					{Name: "log", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"}}},
					{Name: "config-volume", VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
						},
					}},
					{Name: "device-info", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/k8s.cni.cncf.io/devinfo/dp"}}},
					{Name: "host-usr-bin", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/bin"}}},
					{Name: "host-driver-usr-bin", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/local/bin"}}},
					// F-A2: single-file hostPath (HostPathFile) for /usr/bin/rbln-smi bridge
					{Name: "host-rbln-smi", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
						Path: "/usr/local/bin/rbln-smi",
						Type: hostPathFilePtr(),
					}}},
					{Name: "host-dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev"}}},
					{Name: "host-sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}},
				},
			},
		},
	}

	applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)
	applyControlPlaneExclusion(&ds.Spec.Template.Spec)
	applyImagePullSecrets(&ds.Spec.Template.Spec, policy.Spec.ImagePullSecrets)

	if err := r.createOrUpdateDS(ctx, ds); err != nil {
		log.Error(err, "failed to ensure rebellions device plugin daemonset")
		return err
	}
	log.Info("Rebellions device plugin daemonset ensured")
	return nil
}

// ensureDetectorServiceAccount creates the `kcloud-node-manager` ServiceAccount referenced by
// the detector DaemonSet. 차트는 kcloud-node-manager role/rolebinding 만 만들고 SA 객체는 만들지
// 않으므로(fresh 클러스터에서 detector pod 가 `serviceaccount not found` 로 기동 실패),
// 컨트롤러가 ensureRbllnsServiceAccount 와 대칭으로 SA 를 생성한다.
// policy.Spec.ImagePullSecrets 를 SA 에 부착하여 SA 경유 private pull 을 이중 보장한다.
func (r *NPUClusterPolicyReconciler) ensureDetectorServiceAccount(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      detectorServiceAccountName,
			Namespace: naming.OperatorNamespace(),
		},
		ImagePullSecrets: policy.Spec.ImagePullSecrets,
	}
	setOwnerAnnotation(&sa.ObjectMeta, policy)

	var cur corev1.ServiceAccount
	key := types.NamespacedName{Name: sa.Name, Namespace: sa.Namespace}
	if err := r.Get(ctx, key, &cur); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, sa); err != nil {
			log.Error(err, "failed to create detector serviceaccount")
			return err
		}
		return nil
	} else if err != nil {
		return err
	}
	// imagePullSecrets 는 spec 변경으로 갱신될 수 있으므로 기존 SA 와 동기화한다.
	if !equality.Semantic.DeepEqual(cur.ImagePullSecrets, sa.ImagePullSecrets) {
		cur.ImagePullSecrets = sa.ImagePullSecrets
		if err := r.Update(ctx, &cur); err != nil {
			log.Error(err, "failed to update detector serviceaccount imagePullSecrets")
			return err
		}
	}
	return nil
}

func (r *NPUClusterPolicyReconciler) ensureDetector(ctx context.Context, pol *npuv1alpha1.NPUClusterPolicy) error {
	if pol.Spec.Detector == nil || pol.Spec.Detector.Image == "" {
		return fmt.Errorf("detector image must be specified in NPUClusterPolicy.spec.detector.image")
	}

	// detector DS 의 ServiceAccountName(kcloud-node-manager) 를 먼저 보장 — DS 보다 선행해야
	// fresh 클러스터에서 `serviceaccount not found` 로 pod 기동이 실패하지 않는다.
	if err := r.ensureDetectorServiceAccount(ctx, pol); err != nil {
		return err
	}

	image := pol.Spec.Detector.Image
	ds := renderDetectorDS(image, pol.Spec.ImagePullSecrets)
	setOwnerAnnotation(&ds.ObjectMeta, pol)
	return r.createOrUpdateDS(ctx, ds)
}

func renderDetectorDS(image string, pullSecrets []corev1.LocalObjectReference) *appsv1.DaemonSet {
	labels := map[string]string{"app.kubernetes.io/name": "kcloud-node-manager"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kcloud-node-manager",
			Namespace: naming.OperatorNamespace(),
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					// node-agent Metrics 능력: Prometheus 스크레이프 대상 표시(:9100/metrics).
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/port":   "9100",
						"prometheus.io/path":   "/metrics",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "kcloud-node-manager",
					Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:            "detector",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						// node-agent Metrics 능력: /metrics 포트(비특권 로컬 서버).
						Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9100}},
						Env: []corev1.EnvVar{
							{
								Name:      "NODE_NAME",
								ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
							},
							// NFD/GFD 라벨링 능력 활성화(node patch RBAC 는 kcloud-node-manager-role 에 부여됨).
							{Name: "NODEAGENT_ENABLE_LABELS", Value: "true"},
						},
						// P6(S4-5) Pod Security: 비특권 설계 확정. detector 이미지는 distroless/static:nonroot
						// (USER 65532)이며 드라이버 버전은 host /proc·/sys 읽기로 감지(root·device 불필요),
						// nvidia-smi 등 host CLI exec 은 best-effort(실패 무해)라 restricted 프로파일과 양립.
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             boolPtr(true),
							AllowPrivilegeEscalation: boolPtr(false),
							ReadOnlyRootFilesystem:   boolPtr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true},
							{Name: "host-dev", MountPath: "/host/dev", ReadOnly: true},
							{Name: "host-var", MountPath: "/host/var", ReadOnly: true},
							{Name: "host-sys", MountPath: "/host/sys", ReadOnly: true},
							// detector binary 가 /usr/bin/nvidia-smi 검사로 nvidia userland 존재 판단.
							// /host/usr mount 없으면 nvidiaUserlandPresent()=false → driverVersion="" 보고.
							{Name: "host-usr", MountPath: "/host/usr", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "host-proc", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/proc"}}},
						{Name: "host-dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev"}}},
						{Name: "host-var", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var"}}},
						{Name: "host-sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}},
						{Name: "host-usr", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr"}}},
					},
				},
			},
		},
	}
	// detector는 /dev를 ReadOnly로 마운트하므로, 드라이버 업그레이드 중 rmmod 간섭을 막기 위해
	// device-plugin과 동일하게 업그레이드 라벨이 붙은 노드에는 스케줄되지 않도록 한다.
	// detector(node-manager)는 control-plane 포함 전 노드에 상주해야 하므로
	// applyControlPlaneExclusion 을 적용하지 않는다(감지·라벨·검증은 master 에서도 계속).
	applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)
	applyImagePullSecrets(&ds.Spec.Template.Spec, pullSecrets)
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

func int64Ptr(i int64) *int64 {
	return &i
}

// hostPathFilePtr returns a pointer to HostPathFile HostPathType for single-file hostPath volumes.
func hostPathFilePtr() *corev1.HostPathType {
	t := corev1.HostPathFile
	return &t
}

// control-plane/master 노드를 식별하는 라벨. driver_upgrade_controller 의 노드 제외
// 로직(control-plane/master 무조건 skip)과 동일한 키를 사용해 정책 일관성을 유지한다.
const (
	controlPlaneNodeLabel = "node-role.kubernetes.io/control-plane"
	masterNodeLabel       = "node-role.kubernetes.io/master"
)

// appendNodeAffinityRequirements는 기존 Affinity를 보존하면서 required nodeAffinity
// term(들)에 match expression을 누적 추가한다. term이 없으면 하나 생성한다.
func appendNodeAffinityRequirements(spec *corev1.PodSpec, reqs ...corev1.NodeSelectorRequirement) {
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
			ns.NodeSelectorTerms[i].MatchExpressions, reqs...)
	}
	spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = ns
}

// applyDriverUpgradeAntiAffinity는 기존 Affinity를 보존하면서
// driver-upgrading-blocking 라벨이 없는 노드에만 스케줄되도록 제약을 추가한다.
// architectural plan §4.4 옵션 A: 좁은 lifecycle 의 blocking 라벨로 phase-aware 차단.
// Cordoning ~ Upgrading 단계: 라벨 활성 → detector / device-plugin 차단 (rmmod 보호).
// Validating 단계: 라벨 자동 제거 → detector spawn 가능 → NDR 갱신 → Validator 통과.
func applyDriverUpgradeAntiAffinity(spec *corev1.PodSpec) {
	appendNodeAffinityRequirements(spec, corev1.NodeSelectorRequirement{
		Key:      upgrade.DriverUpgradingBlockingLabelKey,
		Operator: corev1.NodeSelectorOpDoesNotExist,
	})
}

// applyControlPlaneExclusion는 GPU/NPU 스택(device-plugin·toolkit·driver installer)
// 워크로드가 control-plane/master 노드에 스케줄되지 않게 하여, 제어 평면이 가속기
// 리소스(예: nvidia.com/gpu)를 광고하지 않도록 한다. driver_upgrade_controller 가 이미
// 강제하는 라벨 기반 제외(control-plane/master skip)와 동일 규칙이다. worker 노드는 두
// 라벨이 모두 없으므로 영향 0. detector(node-manager) DS 에는 적용하지 않는다(전 노드 상주).
// ponytail: 라벨 기반 제외(기존 driver-install 정책과 동일)이므로, control-plane taint 를
// 제거하고 라벨만 유지하는 단일 노드 클러스터도 제외된다. 그런 형상이 필요하면 values 토글 추가.
func applyControlPlaneExclusion(spec *corev1.PodSpec) {
	appendNodeAffinityRequirements(spec,
		corev1.NodeSelectorRequirement{Key: controlPlaneNodeLabel, Operator: corev1.NodeSelectorOpDoesNotExist},
		corev1.NodeSelectorRequirement{Key: masterNodeLabel, Operator: corev1.NodeSelectorOpDoesNotExist},
	)
}
