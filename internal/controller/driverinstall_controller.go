package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "npu-operator/api/v1alpha1"
)

// RBAC 권한 (노드 패치 + CRD 접근)
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=npu.ai,resources=nodedevicereports;nodedevicereports/status;driverinstallpolicies,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

type DriverInstallReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// (옵션) 이벤트 레코더 등
}

func (r *DriverInstallReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// NodeDeviceReport 단일 객체 읽기
	var rep npuv1alpha1.NodeDeviceReport
	if err := r.Get(ctx, req.NamespacedName, &rep); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	nodeName := rep.Spec.NodeName
	if nodeName == "" {
		// 잘못된 리포트: 스킵
		return ctrl.Result{}, nil
	}

	// 정책 목록 조회
	var pols npuv1alpha1.DriverInstallPolicyList
	if err := r.List(ctx, &pols); err != nil {
		return ctrl.Result{}, err
	}

	// 대상 장치가 있고 driverLoaded=false인 경우만 설치 시도
	needInstall := false
	var targetVendor, targetModel string
	for _, d := range rep.Status.Devices {
		if !d.DriverLoaded {
			targetVendor, targetModel = d.Vendor, d.Model
			if matchPolicy(&pols, d) != nil {
				needInstall = true
				break
			}
		}
	}

	if !needInstall {
		// 라벨링 처리 (모든 대상이 로드된 경우)
		if allLoaded(rep) {
			if err := r.ensureNodeLabels(ctx, nodeName, map[string]string{
				fmt.Sprintf("accelerator.%s.%s", targetVendor, targetModel): "true",
				// 버전 라벨은 보고서에서 추출
			}, rep); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 설치 Job 존재 여부 확인
	jobName := fmt.Sprintf("npu-driverinstall-%s", sanitize(nodeName))
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: r.jobNamespace()}, &job)
	if err != nil && apierrors.IsNotFound(err) {
		// Job 생성
		p := matchPolicy(&pols, firstUnloaded(rep))
		if p == nil {
			// 정책이 사라졌거나 안맞음
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		job = *r.renderInstallJob(nodeName, p)
		// ownerRef는 Cluster-Scoped Owner를 붙이기 애매하니 생략하거나, NPUClusterPolicy 등과 결합 시 설정
		if err := r.Create(ctx, &job); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Created driver install Job", "node", nodeName, "policy", p.Name)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// 다음 체크까지 약간 대기
	return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
}

func (r *DriverInstallReconciler) jobNamespace() string {
	// manager가 배포된 네임스페이스와 동일하게 두는 게 일반적
	return "kube-system"
}

func (r *DriverInstallReconciler) renderInstallJob(nodeName string, p *npuv1alpha1.DriverInstallPolicy) *batchv1.Job {
	backoff := int32(0)
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("npu-driverinstall-%s", sanitize(nodeName)),
			Namespace: r.jobNamespace(),
			Labels: map[string]string{
				"app":  "npu-driverinstall",
				"node": nodeName,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "npu-operator-controller-manager",
					RestartPolicy:      corev1.RestartPolicyNever,
					// 특정 노드로 고정
					NodeName:    nodeName,
					HostPID:     true,
					HostNetwork: true,
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:            "installer",
						Image:           p.Spec.Driver.Image,
						SecurityContext: &corev1.SecurityContext{Privileged: ptrBool(true)},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "modules", MountPath: "/lib/modules"},
							{Name: "usr-src", MountPath: "/usr/src"},
							{Name: "var-lib", MountPath: "/var/lib/npu-operator"},
							{Name: "host-etc", MountPath: "/host/etc"},
						},
						Env: []corev1.EnvVar{
							{Name: "TARGET_NODE", Value: nodeName},
							{Name: "REBOOT_STRATEGY", Value: p.Spec.RebootStrategy},
							{Name: "DRIVER_VERSION", Value: p.Spec.Driver.Version},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "modules", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules"}}},
						{Name: "usr-src", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/src"}}},
						{Name: "var-lib", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/npu-operator"}}},
						{Name: "host-etc", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc"}}},
					},
				},
			},
		},
	}
	return j
}

func (r *DriverInstallReconciler) ensureNodeLabels(ctx context.Context, nodeName string, labels map[string]string, rep npuv1alpha1.NodeDeviceReport) error {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	patched := false
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	for k, v := range labels {
		if node.Labels[k] != v {
			node.Labels[k] = v
			patched = true
		}
	}
	if ver := matchedDriverVersion(rep); ver != "" {
		k := "driver.furiosa.version" // 필요시 vendor/model 동적으로
		if node.Labels[k] != ver {
			node.Labels[k] = ver
			patched = true
		}
	}
	if !patched {
		return nil
	}
	return r.Patch(ctx, &node, client.MergeFrom(node.DeepCopy()))
}

func matchedDriverVersion(rep npuv1alpha1.NodeDeviceReport) string {
	for _, d := range rep.Status.Devices {
		if d.DriverLoaded && d.DriverVersion != "" {
			return d.DriverVersion
		}
	}
	return ""
}

func allLoaded(rep npuv1alpha1.NodeDeviceReport) bool {
	loaded := false
	for _, d := range rep.Status.Devices {
		if !d.DriverLoaded {
			return false
		}
		loaded = true
	}
	return loaded // 하나라도 있고 모두 true 여야 true
}

func firstUnloaded(rep npuv1alpha1.NodeDeviceReport) npuv1alpha1.DeviceEntry {
	for _, d := range rep.Status.Devices {
		if !d.DriverLoaded {
			return d
		}
	}
	return npuv1alpha1.DeviceEntry{}
}

func sanitize(s string) string {
	// 노드 이름에서 Job 이름으로 안전 변환(간단버전)
	return s
}

func matchPolicy(pols *npuv1alpha1.DriverInstallPolicyList, d npuv1alpha1.DeviceEntry) *npuv1alpha1.DriverInstallPolicy {
	for i := range pols.Items {
		p := &pols.Items[i]
		if p.Spec.Vendor == d.Vendor && p.Spec.Model == d.Model {
			// TODO: 커널/컨테이너런타임 버전 체크 추가
			return p
		}
	}
	return nil
}

func ptrBool(b bool) *bool { return &b }

// SetupWithManager wires the controller.
func (r *DriverInstallReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// NodeDeviceReport 변화를 Watch
	return ctrl.NewControllerManagedBy(mgr).
		For(&npuv1alpha1.NodeDeviceReport{}, builder.WithPredicates()).
		// 생성/관리 리소스로 Job을 등록 (ownerref를 안 쓰는 경우도 allow)
		Owns(&batchv1.Job{}).
		Complete(r)
}
