/**
 * driver_daemonset_controller.go: Driver DaemonSet 컨트롤러
 * 상세: DriverInstallPolicy.spec.driver.mode="daemonset"인 정책에 대해
 *       컨테이너화 드라이버 DaemonSet을 생성/업데이트합니다.
 * 생성일: 2026-04-13 | 수정일: 2026-04-27
 */

package controller

import (
	"context"
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
	"kcloud-operator/internal/metrics"
	"kcloud-operator/internal/naming"
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
	metrics.RecordReconcile() // reconcile 호출 시각 기록 (liveness probe 용)
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
	if err := r.Get(ctx, key, &cur); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(cur.Spec, desired.Spec) ||
		!equality.Semantic.DeepEqual(cur.ObjectMeta.Labels, desired.ObjectMeta.Labels) ||
		!equality.Semantic.DeepEqual(cur.ObjectMeta.Annotations, desired.ObjectMeta.Annotations) ||
		!equality.Semantic.DeepEqual(cur.ObjectMeta.OwnerReferences, desired.ObjectMeta.OwnerReferences) {
		cur.Spec = desired.Spec
		cur.ObjectMeta.Labels = desired.ObjectMeta.Labels
		cur.ObjectMeta.Annotations = desired.ObjectMeta.Annotations
		// OwnerReferences 동기화 — 기존(ownerRef 없이 생성된) DS 도 업그레이드 시 owner 부여.
		cur.ObjectMeta.OwnerReferences = desired.ObjectMeta.OwnerReferences
		return r.Update(ctx, &cur)
	}
	return nil
}

// renderDriverDaemonSet은 DriverInstallPolicy 스펙을 기반으로 드라이버 DaemonSet을 빌드합니다.
func renderDriverDaemonSet(pol *npuv1alpha1.DriverInstallPolicy) *appsv1.DaemonSet {
	name := naming.DriverDSName(pol.Spec.Vendor, pol.Spec.Model)
	labels := map[string]string{
		"app.kubernetes.io/name":      "kcloud-driver",
		"app.kubernetes.io/component": "driver",
		"npu.ai/vendor":               strings.ToLower(pol.Spec.Vendor),
	}

	// 벤더별 nodeSelector (model 포함, RNGD 분기 지원)
	nodeSelector := vendorNodeSelector(pol.Spec.Vendor, pol.Spec.Model)
	// DriverInstallPolicy에 nodeSelector가 지정된 경우 우선 사용
	if len(pol.Spec.NodeSelector) > 0 {
		nodeSelector = pol.Spec.NodeSelector
	}

	// 벤더별 rmmod 명령 (model 포함, RNGD 분기 지원)
	rmmodCmd := vendorRmmodCommand(pol.Spec.Vendor, pol.Spec.Model)

	image := pol.Spec.Driver.Image

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kube-system",
			Labels:    labels,
			// DIP(cluster-scoped) 를 owner 로 지정 → DIP 삭제 시 K8s GC 가
			// driver DaemonSet 을 cascade 삭제(orphan 방지). cluster-scoped owner +
			// namespaced dependent 조합은 허용됨. BlockOwnerDeletion 은 생략하여
			// driverinstallpolicies/finalizers RBAC 의존을 피한다.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "npu.ai/v1alpha1",
				Kind:       "DriverInstallPolicy",
				Name:       pol.Name,
				UID:        pol.UID,
				Controller: boolPtr(true),
			}},
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
					// PreStop 의 rmmod 가 GPU 워크로드 잔재로 hang 될 가능성에 대비한 grace period.
					// kubelet 은 grace 만료 시 SIGKILL 로 종료하므로 PreStop timeout(30s)+여유 30s = 60s.
					TerminationGracePeriodSeconds: int64Ptr(60),
					// operator pod 와 같은 노드에 driver pod 가 spawn 되지 않도록 유도.
					// 단일 노드 클러스터 호환을 위해 required 대신 preferred 사용.
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 100,
									PodAffinityTerm: corev1.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												"app.kubernetes.io/name":      "npu-operator",
												"app.kubernetes.io/component": "controller",
											},
										},
										TopologyKey: "kubernetes.io/hostname",
									},
								},
							},
						},
					},
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
								// PreStop 은 timeout 으로 강제 종료 한도를 두어 rmmod hang 시에도
								// kubelet grace(60s) 안에 반드시 종료되도록 한다. timeout 종료(124)
								// 이후에도 컨테이너 SIGTERM 처리 시간이 남아야 하므로 timeout 30s.
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c",
											"timeout 30 sh -c 'rm -f /var/lib/npu-operator/driver.ready /tmp/driver-ready; " + rmmodCmd + "' || true"},
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

// vendorNodeSelector는 벤더/모델별 기본 노드 셀렉터를 반환합니다.
// model 이 비어 있거나 "warboy" 인 경우 기존 Furiosa Warboy 셀렉터를 유지하고,
// model="rngd" 인 경우 RNGD 전용 셀렉터를 반환합니다.
func vendorNodeSelector(vendor, model string) map[string]string {
	v := strings.ToLower(vendor)
	m := strings.ToLower(model)
	switch v {
	case "nvidia":
		return map[string]string{"nvidia.com/gpu.present": "true"}
	case "furiosa":
		if m == "rngd" {
			return map[string]string{"furiosa-rngd": "true"}
		}
		return map[string]string{"furiosa": "true"}
	case "rebellions":
		// Rebellions ATOM+ 는 DriverInstallPolicy 를 생성하지 않음 (host-managed driver).
		// 호출 경로가 실제로 도달하지 않지만 분기 완전성을 위해 셀렉터 반환.
		return map[string]string{"rebellions-atom": "true"}
	default:
		return map[string]string{}
	}
}

// vendorRmmodCommand는 벤더/모델별 커널 모듈 언로드 명령을 반환합니다.
// Furiosa RNGD 는 별도 커널 모듈(furiosa_rngd)을 사용하므로 분기합니다.
func vendorRmmodCommand(vendor, model string) string {
	v := strings.ToLower(vendor)
	m := strings.ToLower(model)
	switch v {
	case "nvidia":
		return "rmmod nvidia_uvm nvidia_drm nvidia || true"
	case "furiosa":
		if m == "rngd" {
			return "rmmod furiosa_rngd || true"
		}
		return "rmmod npu_pdma npu_mgmt || true"
	case "rebellions":
		// Rebellions 드라이버는 호스트에서 관리되므로 rmmod 금지 (no-op).
		return "true"
	default:
		return "true"
	}
}

// SetupWithManager는 DriverDaemonSetReconciler를 Manager에 등록합니다.
func (r *DriverDaemonSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&npuv1alpha1.DriverInstallPolicy{}).
		Owns(&appsv1.DaemonSet{}). // owner=DIP 인 driver DS 변경 감시 → 수동 삭제 시 재생성
		Named("driverdaemonset").
		Complete(r)
}
