// ============================================================
// job.go: WP-C-1 driver install Job 빌더 (DriverSpec.Mode=job)
// 상세: 상시 privileged driver DaemonSet 대신 "순간 install Job" 을 렌더한다.
//       DS pod spec(privileged/hostPID/host mounts/env)을 재사용하되,
//       Job 전용으로 (1) restartPolicy=OnFailure (2) PreStop rmmod/probe 제거
//       (완료 시 방금 설치한 모듈 언로드 참사 방지) (3) RUN_MODE=job env 주입
//       (entrypoint 가 설치 후 exit 0) 로 변형한다. node-agent(S5-4) 가 그대로
//       흡수할 수 있도록 controller 에 의존하지 않는 독립 패키지로 둔다.
// 생성일: 2026-07-16
// ============================================================

// Package driverjob renders the one-shot driver install Job used by
// DriverSpec.Mode=job (WP-C-1). It is intentionally free of controller-package
// dependencies so the node-agent variant (S5-4) can reuse it directly.
package driverjob

import (
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/naming"
)

// Namespace 는 install Job 이 생성되는 네임스페이스다. driver DaemonSet 과 동일하게
// operator 관리 네임스페이스(OPERATOR_NAMESPACE, 기본 kube-system)에 둔다.
// process 시작 시 1회 평가(operator pod 의 Downward API env). #16 네임스페이스 재편.
var Namespace = naming.OperatorNamespace()

// 기본값 — JobOverrides 로 override 가능.
const (
	defaultBackoffLimit            int32 = 6    // OnFailure 재시도 상한(재부팅/kubelet restart 대비)
	defaultTTLSecondsAfterFinished int32 = 600  // 완료 후 로그/아티팩트 보존 window 후 GC
	defaultActiveDeadlineSeconds   int64 = 1800 // 무한 hang 방지(DKMS 빌드 여유 30m)
)

// RenderInstallJob 은 DriverInstallPolicy 로부터 대상 노드용 install Job 을 빌드한다.
//
//   - pol      : 벤더/모델/시크릿/JobOverrides 등 정책 소스.
//   - nodeName : 설치 대상 노드(스케줄러 우회, spec.nodeName 고정).
//   - image    : 설치에 사용할 드라이버 이미지(desired 또는 rollback 대상).
//   - version  : DRIVER_VERSION env 로 전달할 목표 버전.
//
// 이름은 naming.InstallJobName 으로 결정론적으로 계산되어 동일 (vendor,model,node) 에
// 대해 중복 생성이 불가능하다(de-facto lease). owner 는 DIP(cluster-scoped)로 설정해
// DIP 삭제 시 K8s GC 가 Job 을 cascade 정리한다(TTL GC 와 함께 orphan 방지).
func RenderInstallJob(pol *npuv1alpha1.DriverInstallPolicy, nodeName, image, version string) *batchv1.Job {
	return renderJob(pol, nodeName, image, version, false)
}

// RenderRollbackJob 은 실패한 업그레이드를 되돌리는 Job 을 빌드한다. RenderInstallJob 과 같되
// **다운그레이드를 허용한다.**
//
// 정책의 `driver.allowDowngrade` 는 *정책 변경으로 버전을 낮추는 것*을 막는 장치다. 롤백은
// 그것과 다른 일이다 — 방금 실패한 업그레이드를 원래 자리로 되돌리는 것이고, 정의상 내려간다.
// 둘을 한 값으로 묶으면 installer 자신의 다운그레이드 가드가 롤백을 거부해 되돌릴 수 없는
// 업그레이드가 만들어지고, 노드는 실패한 버전에 갇힌다(2026-08-10 라이브,
// docs/impl/furiosa-version-swap-20260812.md §3.4).
//
// VERSION_SOURCE 도 Policy 로 고정한다. Host 로 두면 installer 가 호스트에 남은 *실패한* 버전을
// desired 로 채택해 롤백이 그대로 무동작이 된다.
func RenderRollbackJob(pol *npuv1alpha1.DriverInstallPolicy, nodeName, image, version string) *batchv1.Job {
	return renderJob(pol, nodeName, image, version, true)
}

func renderJob(
	pol *npuv1alpha1.DriverInstallPolicy, nodeName, image, version string, rollback bool,
) *batchv1.Job {
	name := naming.InstallJobName(pol.Spec.Vendor, pol.Spec.Model, nodeName)
	labels := map[string]string{
		"app.kubernetes.io/name":      "kcloud-driver-install",
		"app.kubernetes.io/component": "driver-install",
		"npu.ai/vendor":               strings.ToLower(pol.Spec.Vendor),
		"npu.ai/node":                 nodeName,
	}

	backoffLimit := defaultBackoffLimit
	ttl := defaultTTLSecondsAfterFinished
	saName := ""
	priorityClass := ""
	if jo := pol.Spec.JobOverrides; jo != nil {
		if jo.BackoffLimit != nil {
			backoffLimit = *jo.BackoffLimit
		}
		if jo.TTLSecondsAfterFinished != nil {
			ttl = *jo.TTLSecondsAfterFinished
		}
		saName = jo.ServiceAccountName
		priorityClass = jo.PriorityClassName
	}
	activeDeadline := defaultActiveDeadlineSeconds

	// vendor 셀렉터 — nodeName 고정으로 스케줄은 우회되지만 parity 를 위해 유지.
	nodeSelector := vendorNodeSelector(pol.Spec.Vendor, pol.Spec.Model)
	if len(pol.Spec.NodeSelector) > 0 {
		nodeSelector = pol.Spec.NodeSelector
	}

	// env: DS driver 컨테이너 env(안전장치 a/b/c) + RUN_MODE=job.
	// RUN_MODE=job 이 entrypoint 의 상주 while-loop 를 exit 0 으로 분기시킨다.
	// 롤백은 정의상 내려가는 일이라 정책 가드를 넘어선다(RenderRollbackJob 주석 참조).
	allowDowngrade := pol.Spec.Driver.AllowDowngrade
	versionSource := versionSourceOrDefault(pol.Spec.Driver.VersionSource)
	if rollback {
		allowDowngrade = true
		versionSource = versionSourcePolicy
	}

	env := []corev1.EnvVar{
		{Name: "RUN_MODE", Value: "job"},
		{Name: "DRIVER_VERSION", Value: version},
		{Name: "REBOOT_STRATEGY", Value: pol.Spec.RebootStrategy},
		{Name: "VENDOR", Value: pol.Spec.Vendor},
		{Name: "ALLOW_DOWNGRADE", Value: strconv.FormatBool(allowDowngrade)},
		{Name: "VERSION_SOURCE", Value: versionSource},
		{Name: "SKIP_ON_PASSTHROUGH", Value: strconv.FormatBool(pol.Spec.Driver.SkipOnPassthrough)},
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "host-modules", MountPath: "/lib/modules"},
		{Name: "host-src", MountPath: "/usr/src"},
		{Name: "host-etc", MountPath: "/etc"},
		{Name: "host-var", MountPath: "/var/lib/npu-operator"},
		{Name: "device-plugins", MountPath: "/var/lib/kubelet/device-plugins"},
	}
	volumes := []corev1.Volume{
		hostPathVolume("host-modules", "/lib/modules"),
		hostPathVolume("host-src", "/usr/src"),
		hostPathVolume("host-etc", "/etc"),
		hostPathVolume("host-var", "/var/lib/npu-operator"),
		hostPathVolume("device-plugins", "/var/lib/kubelet/device-plugins"),
	}

	// Furiosa APT 인증 Secret 마운트 — Warboy 전용(DS 경로와 동일 규칙).
	// RNGD 는 공개 repo 라 entrypoint 가 /secrets 를 참조하지 않으므로 마운트하지 않는다
	// (secret 부재 클러스터에서 install Job 파드가 스케줄 실패하는 버그를 Job 경로에 복제하지 않음).
	if furiosaAptAuthRequired(pol.Spec.Vendor, pol.Spec.Model) {
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{Name: "furiosa-auth", MountPath: "/secrets", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "furiosa-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "furiosa-apt-auth"},
			},
		})
	}

	podSpec := corev1.PodSpec{
		// 대상 노드 고정 — scheduler 우회(결정적 단일 노드 타게팅).
		NodeName:      nodeName,
		NodeSelector:  nodeSelector,
		HostPID:       true,
		HostNetwork:   true,
		RestartPolicy: corev1.RestartPolicyOnFailure,
		Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		Containers: []corev1.Container{
			{
				Name:            "driver-install",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env:             env,
				SecurityContext: &corev1.SecurityContext{
					Privileged: boolPtr(true),
				},
				// NOTE: DS 의 StartupProbe/LivenessProbe/PreStop(rmmod) 은 의도적으로 제외한다.
				// Job 은 설치 후 exit 0 하며, PreStop rmmod 가 있으면 종료 시 방금 로드한
				// 커널 모듈을 언로드해 설치를 무효화한다(참사). Job 완료 판정은 exit code +
				// NDR validator 가 담당하므로 probe 불필요.
				VolumeMounts: volumeMounts,
			},
		},
		Volumes: volumes,
	}
	if saName != "" {
		podSpec.ServiceAccountName = saName
	}
	if priorityClass != "" {
		podSpec.PriorityClassName = priorityClass
	}
	applyImagePullSecrets(&podSpec, pol.Spec.ImagePullSecrets)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: Namespace,
			Labels:    labels,
			// DIP(cluster-scoped) 를 owner 로 → DIP 삭제 시 GC cascade. cluster-scoped
			// owner + namespaced dependent 조합은 허용됨. BlockOwnerDeletion 생략으로
			// finalizers RBAC 의존 회피(DS 와 동일 패턴).
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "npu.ai/v1alpha1",
				Kind:       "DriverInstallPolicy",
				Name:       pol.Name,
				UID:        pol.UID,
				Controller: boolPtr(true),
			}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &activeDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}
	return job
}

// vendorNodeSelector 는 벤더/모델별 기본 노드 셀렉터를 반환한다.
// controller.vendorNodeSelector 와 동일 규칙(패키지 순환 회피를 위해 최소 복제).
func vendorNodeSelector(vendor, model string) map[string]string {
	v := strings.ToLower(vendor)
	m := strings.ToLower(model)
	switch v {
	case "nvidia":
		return map[string]string{"kcloud.ai/nvidia.present": "true"}
	case "furiosa":
		if m == "rngd" {
			return map[string]string{"kcloud.ai/rngd.present": "true"}
		}
		return map[string]string{"kcloud.ai/furiosa.present": "true"}
	case "rebellions":
		return map[string]string{"kcloud.ai/rebellions.present": "true"}
	default:
		return map[string]string{}
	}
}

// furiosaAptAuthRequired 는 Furiosa APT 인증 secret 마운트 필요 여부를 반환한다.
// Warboy=true(사설 repo 인증 필수), RNGD=false(공개 repo). controller 패키지의 동일 헬퍼와
// 규칙을 일치시킨다(패키지 순환 회피를 위해 최소 복제).
func furiosaAptAuthRequired(vendor, model string) bool {
	return strings.EqualFold(vendor, "furiosa") && !strings.EqualFold(model, "rngd")
}

// versionSourcePolicy 는 정책이 선언한 버전을 그대로 쓰라는 값이다(entrypoint 의 VERSION_SOURCE).
const versionSourcePolicy = "Policy"

// versionSourceOrDefault 는 (c) VersionSource 가 빈 값이면 기존 동작 "Policy" 를 반환한다.
func versionSourceOrDefault(vs string) string {
	if vs == "" {
		return versionSourcePolicy
	}
	return vs
}

// applyImagePullSecrets 는 policy 레벨 imagePullSecrets 를 pod spec 에 부착한다.
// 빈 목록이면 no-op(하위호환 — 노드레벨 인증 경로 유지).
func applyImagePullSecrets(spec *corev1.PodSpec, secrets []corev1.LocalObjectReference) {
	if len(secrets) == 0 {
		return
	}
	spec.ImagePullSecrets = append(spec.ImagePullSecrets, secrets...)
}

func hostPathVolume(name, path string) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path},
		},
	}
}

func boolPtr(b bool) *bool { return &b }
