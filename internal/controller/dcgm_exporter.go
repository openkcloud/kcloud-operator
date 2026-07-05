// ============================================================
// dcgm_exporter.go: NVIDIA GPU 텔레메트리 exporter(dcgm-exporter) DaemonSet 관리
// 상세: NPUClusterPolicy.spec.nvidia.dcgmExporter.enabled=true 인 경우 NVIDIA 노드
//
//	(kcloud.ai/nvidia.present)에 upstream dcgm-exporter DS 를 배포한다. NPU 4벤더는
//	node-manager 가 커널 hwmon 으로 온도/전력을 수집하지만(무특권) NVIDIA 드라이버는
//	hwmon 을 등록하지 않고, node-manager(distroless/nonroot)가 host nvidia-smi 를
//	exec 할 수 없어 NVML 경로가 별도로 필요하다. NVML 을 재구현하는 대신 NVIDIA GPU
//	Operator 와 동일하게 upstream 이미지를 operand 로 배포한다.
//	설계: docs/superpowers/specs/2026-07-29-device-telemetry-design.md §2 D2
//
// 생성일: 2026-07-29 | 수정일: 2026-07-29
// ============================================================

package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

const (
	// dcgmExporterDSName 은 배포되는 DaemonSet 이름이다(kube-system, 3rd party 이미지 규약).
	dcgmExporterDSName = "kcloud-dcgm-exporter"
	// dcgmExporterImageDefault 는 spec 미지정 시 기본 이미지다. air-gap 은 helm values 로
	// Harbor 미러 경로를 넘긴다(운영 기본값은 values.yaml 에서 미러 경로로 지정).
	dcgmExporterImageDefault = "nvcr.io/nvidia/k8s/dcgm-exporter:4.5.2-4.8.1-ubuntu22.04"
	// dcgmExporterPort 는 upstream 기본 metrics 포트다.
	dcgmExporterPort = 9400
	// hostPodResourcesDir 는 dcgm-exporter 가 GPU↔Pod 매핑에 쓰는 kubelet pod-resources 소켓 경로.
	hostPodResourcesDir = "/var/lib/kubelet/pod-resources"
)

// ensureDcgmExporter 는 dcgmExporter.enabled 에 따라 DS 를 생성/갱신하거나 제거한다.
// 비활성(nil/false)일 때 기존 DS 를 지우므로 토글 off 가 곧 정리다(별도 cleanup 불요).
func (r *NPUClusterPolicyReconciler) ensureDcgmExporter(ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	log := logf.FromContext(ctx)

	spec := policy.Spec.Nvidia.DcgmExporter
	if !policy.Spec.Nvidia.Enabled || spec == nil || !spec.Enabled {
		return r.deleteDaemonSetIfExists(ctx, dcgmExporterDSName)
	}

	ds := renderDcgmExporterDS(policy)
	setOwnerAnnotation(&ds.ObjectMeta, policy)
	applyDriverUpgradeAntiAffinity(&ds.Spec.Template.Spec)
	applyControlPlaneExclusion(&ds.Spec.Template.Spec)
	applyImagePullSecrets(&ds.Spec.Template.Spec, policy.Spec.ImagePullSecrets)

	if err := r.createOrUpdateDS(ctx, ds); err != nil {
		log.Error(err, "failed to ensure dcgm-exporter daemonset")
		return err
	}
	log.Info("dcgm-exporter daemonset ensured", "image", ds.Spec.Template.Spec.Containers[0].Image)
	return nil
}

// renderDcgmExporterDS 는 dcgm-exporter DaemonSet 을 만든다. nodeSelector·runtimeClass·
// 특권 설정은 nvidia device-plugin 과 동일 규약을 따른다(MIG 조각 조회 요구사항 동일).
func renderDcgmExporterDS(policy *npuv1alpha1.NPUClusterPolicy) *appsv1.DaemonSet {
	image := dcgmExporterImageDefault
	if policy.Spec.Nvidia.DcgmExporter != nil && policy.Spec.Nvidia.DcgmExporter.Image != "" {
		image = policy.Spec.Nvidia.DcgmExporter.Image
	}

	// device-plugin 과 동일한 자립 라벨 셀렉터(NFD 비의존). spec 의 nodeSelector 를 주면 그것을 쓴다.
	sel := map[string]string{"kcloud.ai/nvidia.present": "true"}
	if len(policy.Spec.Nvidia.NodeSelector) > 0 {
		sel = policy.Spec.Nvidia.NodeSelector
	}

	labels := map[string]string{
		"app.kubernetes.io/name":      dcgmExporterDSName,
		"app.kubernetes.io/component": "telemetry",
	}
	nvidiaRuntime := vendorNvidia
	hostPathDir := corev1.HostPathDirectory

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dcgmExporterDSName,
			Namespace: "kube-system",
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					// node-manager(:9100)와 동일하게 annotation 기반 스크레이프.
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/port":   "9400",
						"prometheus.io/path":   "/metrics",
					},
				},
				Spec: corev1.PodSpec{
					NodeSelector:     sel,
					RuntimeClassName: &nvidiaRuntime,
					Tolerations:      []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:            "dcgm-exporter",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{{
							Name:          "metrics",
							ContainerPort: dcgmExporterPort,
						}},
						Env: []corev1.EnvVar{
							{Name: "DCGM_EXPORTER_LISTEN", Value: ":9400"},
							// pod-resources 소켓으로 GPU↔Pod 를 매핑해 metric 에 pod/namespace 라벨을 붙인다.
							{Name: "DCGM_EXPORTER_KUBERNETES", Value: "true"},
							{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"},
							{Name: "NVIDIA_DRIVER_CAPABILITIES", Value: "all"},
							// MIG 조각 계측은 /dev/nvidia-caps 접근이 필요하다(device-plugin 과 동일 이유).
							{Name: "NVIDIA_MIG_MONITOR_DEVICES", Value: "all"},
						},
						// MIG 계측에 특권이 필요하다("Insufficient Permissions" 회피). NVIDIA GPU
						// Operator 의 dcgm-exporter 도 MIG 환경에서 동일한 권한을 요구한다.
						SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
						VolumeMounts: []corev1.VolumeMount{{
							Name: "pod-gpu-resources", MountPath: hostPodResourcesDir, ReadOnly: true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "pod-gpu-resources",
						VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
							Path: hostPodResourcesDir, Type: &hostPathDir,
						}},
					}},
				},
			},
		},
	}
}
