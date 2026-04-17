/**
 * driver_daemonset_controller.go: Driver DaemonSet 컨트롤러
 * 상세: DriverInstallPolicy.spec.driver.mode="daemonset"인 정책에 대해
 *       컨테이너화 드라이버 DaemonSet을 생성/업데이트합니다.
 * 생성일: 2026-04-13 | 수정일: 2026-04-15
 */

package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// DriverDaemonSetReconciler는 Mode="daemonset"인 DriverInstallPolicy에 대해
// 드라이버 DaemonSet을 관리합니다.
type DriverDaemonSetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=npu.ai,resources=driverinstallpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *DriverDaemonSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("Reconciling DriverDaemonSet", "name", req.NamespacedName)

	// DriverInstallPolicy 목록 조회
	var pols npuv1alpha1.DriverInstallPolicyList
	if err := r.List(ctx, &pols); err != nil {
		return ctrl.Result{}, err
	}

	for i := range pols.Items {
		pol := &pols.Items[i]
		if pol.Spec.Driver.Mode != "daemonset" {
			continue
		}
		if err := r.createOrUpdateDriverDS(ctx, pol); err != nil {
			logger.Error(err, "failed to ensure driver DaemonSet", "policy", pol.Name)
			r.Recorder.Eventf(pol, corev1.EventTypeWarning, "ReconcileFailed",
				"Failed to ensure driver DaemonSet for policy %s: %v", pol.Name, err)
			return ctrl.Result{}, err
		}
		logger.Info("Driver DaemonSet ensured", "policy", pol.Name, "vendor", pol.Spec.Vendor)
	}

	return ctrl.Result{}, nil
}

// createOrUpdateDriverDS는 DriverInstallPolicy에 맞는 드라이버 DaemonSet을 생성하거나 업데이트합니다.
func (r *DriverDaemonSetReconciler) createOrUpdateDriverDS(ctx context.Context, pol *npuv1alpha1.DriverInstallPolicy) error {
	ds := renderDriverDaemonSet(pol)
	return r.createOrUpdateDS(ctx, ds)
}

// createOrUpdateDS는 DaemonSet을 생성하거나 스펙/레이블/어노테이션 변경 시 업데이트합니다.
func (r *DriverDaemonSetReconciler) createOrUpdateDS(ctx context.Context, desired *appsv1.DaemonSet) error {
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

// renderDriverDaemonSet은 DriverInstallPolicy 스펙을 기반으로 드라이버 DaemonSet을 빌드합니다.
func renderDriverDaemonSet(pol *npuv1alpha1.DriverInstallPolicy) *appsv1.DaemonSet {
	name := fmt.Sprintf("npu-op-driver-%s-%s", strings.ToLower(pol.Spec.Vendor), sanitize(pol.Spec.Model))
	labels := map[string]string{
		"app.kubernetes.io/name":      "npu-op-driver",
		"app.kubernetes.io/component": "driver",
		"npu.ai/vendor":               strings.ToLower(pol.Spec.Vendor),
	}

	// 벤더별 nodeSelector
	nodeSelector := vendorNodeSelector(pol.Spec.Vendor)
	// DriverInstallPolicy에 nodeSelector가 지정된 경우 우선 사용
	if len(pol.Spec.NodeSelector) > 0 {
		nodeSelector = pol.Spec.NodeSelector
	}

	// 벤더별 rmmod 명령
	rmmodCmd := vendorRmmodCommand(pol.Spec.Vendor)

	image := pol.Spec.Driver.Image

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kube-system",
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.OnDeleteDaemonSetStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					HostPID:      true,
					HostNetwork:  true,
					NodeSelector: nodeSelector,
					Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					InitContainers: []corev1.Container{
						{
							Name:    "driver-manager",
							Image:   image,
							Command: []string{"/usr/local/bin/driver-manager.sh"},
							Env: []corev1.EnvVar{
								{Name: "DRIVER_VERSION", Value: pol.Spec.Driver.Version},
								{Name: "REBOOT_STRATEGY", Value: pol.Spec.RebootStrategy},
								{Name: "VENDOR", Value: pol.Spec.Vendor},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(true),
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-modules", MountPath: "/lib/modules"},
								{Name: "host-var", MountPath: "/var/lib/npu-operator"},
							},
						},
						{
							Name:  "check-kernel-headers",
							Image: image,
							Command: []string{
								"/usr/local/bin/check-kernel-headers.sh",
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(true),
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-modules", MountPath: "/lib/modules"},
								{Name: "host-src", MountPath: "/usr/src"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "driver",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{Name: "DRIVER_VERSION", Value: pol.Spec.Driver.Version},
								{Name: "VENDOR", Value: pol.Spec.Vendor},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(true),
							},
							StartupProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"cat", "/var/lib/npu-operator/driver.ready"},
									},
								},
								InitialDelaySeconds: 60,
								PeriodSeconds:       10,
								FailureThreshold:    180,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/usr/local/bin/healthcheck.sh"},
									},
								},
								PeriodSeconds:    30,
								FailureThreshold: 3,
							},
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c",
											"rm -f /var/lib/npu-operator/driver.ready /tmp/driver-ready; " + rmmodCmd},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-modules", MountPath: "/lib/modules"},
								{Name: "host-src", MountPath: "/usr/src"},
								{Name: "host-etc", MountPath: "/etc"},
								{Name: "host-var", MountPath: "/var/lib/npu-operator"},
								{Name: "device-plugins", MountPath: "/var/lib/kubelet/device-plugins"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "host-modules",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules"},
							},
						},
						{
							Name: "host-src",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/usr/src"},
							},
						},
						{
							Name: "host-etc",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/etc"},
							},
						},
						{
							Name: "host-var",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/npu-operator"},
							},
						},
						{
							Name: "device-plugins",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet/device-plugins"},
							},
						},
					},
				},
			},
		},
	}

	// Furiosa 전용 Secret 마운트 (APT 인증)
	if strings.EqualFold(pol.Spec.Vendor, "furiosa") {
		ds.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			ds.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "furiosa-auth", MountPath: "/secrets", ReadOnly: true},
		)
		ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: "furiosa-auth",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: "furiosa-apt-auth"},
				},
			},
		)
	}

	return ds
}

// vendorNodeSelector는 벤더별 기본 노드 셀렉터를 반환합니다.
func vendorNodeSelector(vendor string) map[string]string {
	switch strings.ToLower(vendor) {
	case "nvidia":
		return map[string]string{"nvidia.com/gpu.present": "true"}
	case "furiosa":
		return map[string]string{"furiosa": "true"}
	default:
		return map[string]string{}
	}
}

// vendorRmmodCommand는 벤더별 커널 모듈 언로드 명령을 반환합니다.
func vendorRmmodCommand(vendor string) string {
	switch strings.ToLower(vendor) {
	case "nvidia":
		return "rmmod nvidia_uvm nvidia_drm nvidia || true"
	case "furiosa":
		return "rmmod npu_pdma npu_mgmt || true"
	default:
		return "true"
	}
}

// SetupWithManager는 DriverDaemonSetReconciler를 Manager에 등록합니다.
func (r *DriverDaemonSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&npuv1alpha1.DriverInstallPolicy{}).
		Named("driverdaemonset").
		Complete(r)
}
