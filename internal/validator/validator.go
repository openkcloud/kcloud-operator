// ============================================================
// validator.go: 드라이버 업그레이드 검증 단계 (Validator) 인터페이스 + 구현
// 상세: architectural plan §4.4.3 — 단일 단계 handleValidating 을
//       단계별 책임으로 분리한다 (DriverModule / DevicePlugin / Workload).
//       각 Validator 는 controller-runtime client 와 기본 식별자만 받아
//       단일 검증 시도(Run)를 수행한다. caller 는 timeout 내에서 재시도하고
//       Event/Metric 을 발행한다.
// 생성일: 2026-04-27 | 수정일: 2026-10-09 (WorkloadValidator 를 정책 opt-in 샘플 워크로드 probe 로 활성화)
// ============================================================

package validator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "kcloud-operator/api/v1alpha1"
)

// Result 는 검증 단계의 결과를 표현한다.
//   - Passed=true 면 caller 는 다음 Validator 로 진행한다.
//   - Passed=false 는 "검증 미통과" — error 와 의미 분리. 재시도는 caller 의
//     Timeout() 안에서 결정한다.
//   - Message 는 Event/로그용 사람이 읽을 수 있는 사유.
type Result struct {
	Passed  bool
	Message string
}

// Validator 는 단일 검증 단계 인터페이스.
//
// 의미:
//   - Name 은 metric/event label 로 사용된다 (예: "DriverModule", "DevicePlugin").
//   - Timeout 은 controller wall-clock 예산 — caller 는 LastTransitionTime 부터
//     이 값을 초과하면 Failed/Rollback 으로 전이한다.
//   - Run 은 검증 1회 시도 — 통과/미통과/오류 셋 중 하나를 반환한다.
//     ctx cancel 시 즉시 반환해야 한다 (호출자가 reconcile timeout 으로 차단).
type Validator interface {
	Name() string
	Timeout() time.Duration
	Run(ctx context.Context, c client.Client, nodeName, vendor, desiredVersion string) (Result, error)
}

// ─────────────────────────────────────────────
// DriverModuleValidator
// ─────────────────────────────────────────────

// DriverModuleValidator 는 노드의 드라이버 커널 모듈이 desiredVersion 으로 로드되었는지
// NodeDeviceReport (NDR) 를 통해 검증한다.
//
// 통과 조건: NDR.status.devices[].driverVersion == desiredVersion (vendor 일치)
//   - DriverLoaded=true 도 함께 요구.
//
// timeout 30s — NDR collector 가 보통 수 초 내 갱신.
type DriverModuleValidator struct{}

// Name 은 metric/event label.
func (v *DriverModuleValidator) Name() string { return "DriverModule" }

// Timeout 은 caller 가 wall-clock 으로 사용할 budget.
func (v *DriverModuleValidator) Timeout() time.Duration { return 30 * time.Second }

// Run 은 NDR 의 driverVersion 이 desiredVersion 과 일치하는지 1회 확인한다.
//   - desiredVersion 이 비어 있으면 검증 skip — Passed=true.
//   - NDR 미존재 시 Passed=false (collector 가 아직 갱신 못 함, caller 재시도).
//   - vendor 매칭되는 device 가 없거나 driverVersion 이 다르면 Passed=false.
//   - API 오류는 error 로 반환 — caller 재시도 가능.
func (v *DriverModuleValidator) Run(
	ctx context.Context,
	c client.Client,
	nodeName, vendor, desiredVersion string,
) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if desiredVersion == "" {
		return Result{Passed: true, Message: "desiredVersion 미지정으로 모듈 검증 skip"}, nil
	}

	var ndr v1alpha1.NodeDeviceReport
	// NDR 의 metadata.name 은 nodeName 과 동일하게 관리됨 (driver_upgrade_controller_test.go 참고).
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &ndr); err != nil {
		// NotFound 는 미통과로 처리, 그 외 API 오류는 caller 가 재시도하도록 error 반환.
		if isNotFound(err) {
			return Result{Passed: false, Message: "NDR 미존재 (collector 갱신 대기)"}, nil
		}
		return Result{}, err
	}

	// host 의 (loaded) driver 버전을 캡처해 actionable error 메시지에 사용한다.
	// 동일 vendor 의 device 가 여러 개여도 driverVersion 은 동일하다고 가정 — 첫 발견값을 사용.
	hostVer := ""
	hostDetail := ""
	hostNeedsReboot := false
	for _, d := range ndr.Status.Devices {
		if !strings.EqualFold(d.Vendor, vendor) {
			continue
		}
		if !d.DriverLoaded {
			continue
		}
		if hostVer == "" {
			hostVer = d.DriverVersion
			hostDetail = d.DriverVersionDetail
			hostNeedsReboot = d.NeedsReboot
		}
		if d.DriverVersion == desiredVersion {
			return Result{Passed: true, Message: "NDR.driverVersion 이 desiredVersion 과 일치"}, nil
		}
	}
	if hostVer != "" && hostVer != desiredVersion {
		// host kernel module 이 desiredVersion 과 다르면 driver-ds Pod 의 init container
		// (driver-manager) 가 모듈 swap 에 실패했음을 의미. 운영자에게 직접 조치 절차를 제공한다.
		msg := fmt.Sprintf(
			"host kernel module=%s ≠ desired=%s — driver-ds Pod 의 driver-manager init container 가 swap 실패. "+
				"조치: ssh <user>@%s; sudo systemctl stop dcgm-exporter; "+
				"sudo rmmod nvidia_uvm nvidia_drm nvidia_modeset nvidia; 그 후 driver-ds Pod 재시작",
			hostVer, desiredVersion, nodeName,
		)
		// needsReboot 이면 위 안내는 틀렸다. 목표 버전 모듈은 이미 디스크에 준비돼 있고 구 모듈이
		// 사용 중이라 삽입만 못 한 상태(cross-major)라서 rmmod 로는 풀리지 않는다 — 그 지시를
		// 따르는 사람은 시간만 버린다(2026-08-07 라이브). 교체 주체는 노드 재부팅뿐이고,
		// 그 재부팅은 상태기계가 RebootRequired 로 진행한다.
		if hostNeedsReboot {
			msg = fmt.Sprintf(
				"host kernel module=%s ≠ desired=%s — 목표 버전 모듈은 디스크에 준비됐고 구 모듈이 "+
					"사용 중이라 삽입만 못 한 상태(cross-major). 노드 재부팅으로만 교체되며 rmmod 로는 "+
					"풀리지 않는다 — operator 가 재부팅을 진행하므로 수동 조치 불필요 (node=%s)",
				hostVer, desiredVersion, nodeName,
			)
		}
		if hostDetail != "" {
			msg = fmt.Sprintf("%s (detail=%s)", msg, hostDetail)
		}
		return Result{Passed: false, Message: msg}, nil
	}
	return Result{
		Passed:  false,
		Message: "NDR.driverVersion 미보고",
	}, nil
}

// ─────────────────────────────────────────────
// DevicePluginValidator
// ─────────────────────────────────────────────

// DevicePluginValidator 는 device-plugin Pod 가 노드에서 ContainersReady 인지 검증한다.
//
// 통과 조건: kube-system namespace 에서 vendor 라벨/이름 매칭되는 Pod 중
//
//	노드에 스케줄된 인스턴스가 ContainersReady=True 면 PASS.
//
// timeout 60s — Pod 재시작/시작 시간 고려.
type DevicePluginValidator struct{}

// Name 은 metric/event label.
func (v *DevicePluginValidator) Name() string { return "DevicePlugin" }

// Timeout 은 caller wall-clock budget.
func (v *DevicePluginValidator) Timeout() time.Duration { return 60 * time.Second }

// Run 은 device-plugin Pod 의 Ready 여부를 1회 확인한다.
//   - vendor 가 비면 vendor 매칭 skip.
//   - Pod 미존재 → Passed=false (caller 재시도).
//   - Terminating Pod (DeletionTimestamp) 는 무시.
func (v *DevicePluginValidator) Run(
	ctx context.Context,
	c client.Client,
	nodeName, vendor, _ string,
) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// device-plugin 은 벤더별로 다른 네임스페이스에 상주한다(3rd-party=kube-system,
	// TT=kcloud=OperatorNamespace, #21). 네임스페이스를 하드코딩하지 않고 전 네임스페이스를
	// 조회하되 아래 nodeName + isDevicePluginPod 필터로 정확히 스코프한다(ClusterRole pods list).
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil {
		return Result{}, err
	}
	found := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if !isDevicePluginPod(pod, vendor) {
			continue
		}
		found = true
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.ContainersReady && cond.Status == corev1.ConditionTrue {
				return Result{Passed: true, Message: "device-plugin Pod ContainersReady"}, nil
			}
		}
	}
	if !found {
		return Result{Passed: false, Message: "device-plugin Pod 미스케줄 (재시도 대기)"}, nil
	}
	return Result{Passed: false, Message: "device-plugin Pod 가 ContainersReady 가 아님"}, nil
}

// ─────────────────────────────────────────────
// WorkloadValidator
// ─────────────────────────────────────────────

// WorkloadValidator 는 장치 1개를 요청하는 probe 파드를 띄워 벤더 명령의 종료 코드로 "진짜 쓸 수 있는가" 를
// 본다. Run 은 한 번 호출이 한 번 관찰이다 — 파드가 없으면 만들고 대기(Passed=false), Succeeded 면 지우고
// 통과, Failed 면 지우고 실패, Pending 이 TimeoutSeconds 를 넘기면 지우고 실패. 체인이 10초마다 다시 부른다.
// 정책 opt-in(DriverInstallPolicySpec.WorkloadProbe) — validatorsFor(internal/upgrade)가 켜졌을 때만 체인에 넣는다.
//
// 파드 이름은 (노드, desiredVersion) 에 고정된다 — 같은 버전으로 재검증하는 동안은 이름이
// 재사용되므로, Succeeded 를 보고도 삭제(cleanup)에 실패하면 그 파드가 다음 Run 에서 다시
// 읽혀 통과를 막는다(의도적 — 지워지지 않는 파드를 판정으로 쓰지 않기 위해).
type WorkloadValidator struct {
	Probe     v1alpha1.WorkloadProbeSpec
	Namespace string
}

// Name 은 metric/event label.
func (v *WorkloadValidator) Name() string { return "Workload" }

// Timeout 은 caller wall-clock budget — Probe.TimeoutSeconds 미지정 시 120s.
func (v *WorkloadValidator) Timeout() time.Duration {
	if v.Probe.TimeoutSeconds > 0 {
		return time.Duration(v.Probe.TimeoutSeconds) * time.Second
	}
	return 120 * time.Second
}

// probeVersionLabel 은 probe 파드가 속한 desiredVersion 사이클을 적는 라벨이다. 이름에도
// desiredVersion 을 해시로 섞어 두지만(probePodName), 이름만으론 사람이 못 읽고 해시 충돌
// 같은 미래 리팩터 실수에도 끄떡없게 라벨로 한 번 더 명시적으로 확인한다(Run 참고).
const probeVersionLabel = "kcloud.ai/probe-version"

// labelValueRe 는 k8s 라벨 값 규칙이다(영숫자로 시작·끝, 중간은 영숫자/._- , 63자 이하).
var labelValueRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

// sanitizeLabelValue 는 desiredVersion 을 라벨 값으로 다듬는다. 드라이버 버전은 보통
// 숫자·점뿐이라 그대로 쓰면 사람이 읽기 좋다 — 라벨 규칙을 벗어나는 문자가 섞이거나
// 63자를 넘으면(식별 목적만 충족하면 되므로) 해시로 대체한다.
func sanitizeLabelValue(s string) string {
	if labelValueRe.MatchString(s) {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("h%x", sum[:8])
}

// probePodName 은 (노드, desiredVersion) 하나당 결정적인 이름이다(해시 접미 — 63자 제한
// 안에서 입력 길이와 무관). desiredVersion 을 해시에 섞는 이유: vendor+nodeName 만 해시하면
// 이전 사이클(다른 버전)의 Succeeded/Failed 파드가 같은 이름으로 남아 있을 때 이번 사이클이
// 그것을 자기 판정으로 잘못 재사용한다 — 아무것도 실행하지 않고 통과/실패를 내보내게 된다.
func probePodName(vendor, nodeName, desiredVersion string) string {
	sum := sha256.Sum256([]byte(nodeName + "\x00" + desiredVersion))
	return fmt.Sprintf("kcloud-%s-upgrade-probe-%x", vendor, sum[:4])
}

// Run 은 probe 파드 1개의 상태를 1회 관찰한다. vendor 별 리소스명 기본값은 두지 않는다 —
// furiosa 는 product(warboy/rngd)가 있어야 리소스명이 정해지는데 이 함수에는 그 정보가 없다
// (internal/intent.WholeDeviceResource 는 product 를 요구). 그래서 Probe.ResourceName 은 필수이고
// 비면 정책 설정 오류로 미통과 처리한다 — CRD 에 MinLength 를 두지 않는 이유는 CR 생성 자체를
// 막기보다 사람이 읽을 수 있는 사유를 Event/상태로 남기기 위해서다.
func (v *WorkloadValidator) Run(ctx context.Context, c client.Client, nodeName, vendor, desiredVersion string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if v.Probe.ResourceName == "" {
		return Result{Passed: false, Message: "workloadProbe.resourceName 이 비어 있다"}, nil
	}
	key := client.ObjectKey{Namespace: v.Namespace, Name: probePodName(vendor, nodeName, desiredVersion)}
	var pod corev1.Pod
	err := c.Get(ctx, key, &pod)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(ctx, v.probePod(key, nodeName, vendor, desiredVersion)); err != nil {
			return Result{}, err
		}
		return Result{Passed: false, Message: "workload probe 파드 생성 — 완료 대기"}, nil
	case err != nil:
		return Result{}, err
	}
	// 이름이 이미 (노드, desiredVersion) 에 묶여 있어 정상 경로로는 못 일어나지만, 해시 충돌이나
	// 수동 생성 같은 예외를 방어적으로 잡는다 — 이름만 믿지 않고 라벨로 한 번 더 확인한다.
	if pod.Labels[probeVersionLabel] != sanitizeLabelValue(desiredVersion) {
		_ = c.Delete(ctx, &pod)
		return Result{Passed: false, Message: "이전 버전의 probe 파드 정리 — 재생성 대기"}, nil
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		// 지우지 못하면 통과시키지 않는다 — 다음 Run 이 같은 이름으로 이 파드를 다시 읽어
		// (또) Succeeded 로 판정하는 것 자체는 무해하지만, 지워지지 않는 파드를 "통과" 로
		// 보고하면 운영자가 정리 실패를 못 보고 넘긴다.
		if err := c.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
			return Result{Passed: false, Message: fmt.Sprintf("probe 통과했으나 정리 실패: %v", err)}, nil
		}
		return Result{Passed: true, Message: "workload probe 종료 코드 0"}, nil
	case corev1.PodFailed:
		msg := "workload probe 실패(Failed) — 드라이버로 장치를 쓸 수 없다"
		if err := c.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
			msg = fmt.Sprintf("%s (정리 실패: %v)", msg, err)
		}
		return Result{Passed: false, Message: msg}, nil
	}
	if time.Since(pod.CreationTimestamp.Time) > v.Timeout() {
		msg := "workload probe 가 제한 시간 안에 끝나지 않음(스케줄·기동 지연)"
		if err := c.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
			msg = fmt.Sprintf("%s (정리 실패: %v)", msg, err)
		}
		return Result{Passed: false, Message: msg}, nil
	}
	return Result{Passed: false, Message: "workload probe 실행 중"}, nil
}

// probePod 은 노드에 고정(NodeName)된, 장치 1개를 요청하는 1회성 파드다. driver-upgrading 라벨의
// cordon 상태에서도 뜰 수 있게 모든 taint 를 허용한다(cordon=NoSchedule 은 scheduler 가 아니라
// unschedulable 플래그라 toleration 과 무관하지만, 벤더 노드에 흔한 수동 taint 대비 방어적으로 둔다).
func (v *WorkloadValidator) probePod(key client.ObjectKey, nodeName, vendor, desiredVersion string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name,
			Labels: map[string]string{
				"app.kubernetes.io/name": "kcloud-upgrade-probe",
				"kcloud.ai/vendor":       vendor,
				probeVersionLabel:        sanitizeLabelValue(desiredVersion),
			}},
		Spec: corev1.PodSpec{
			NodeName: nodeName, RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{Name: "probe", Image: v.Probe.Image, Command: v.Probe.Command,
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceName(v.Probe.ResourceName): resource.MustParse("1"),
				}}}},
		},
	}
}

// ─────────────────────────────────────────────
// 헬퍼
// ─────────────────────────────────────────────

// isDevicePluginPod 은 Pod 가 device-plugin 인지 (옵션) 특정 vendor 인지 판단한다.
//
// 매칭 규칙 (state_machine.go deleteDevicePluginPods 와 동일):
//   - app.kubernetes.io/name 라벨에 "device-plugin" 부분 포함, 또는
//   - Pod 이름에 "device-plugin" 포함.
//
// vendor 가 비어있지 않다면 라벨/이름에 vendor 토큰이 포함되어야 한다.
func isDevicePluginPod(pod *corev1.Pod, vendor string) bool {
	matchedDevicePlugin := false
	for key, val := range pod.Labels {
		if key == "app.kubernetes.io/name" && strings.Contains(val, "device-plugin") {
			matchedDevicePlugin = true
			break
		}
	}
	if !matchedDevicePlugin && strings.Contains(pod.Name, "device-plugin") {
		matchedDevicePlugin = true
	}
	if !matchedDevicePlugin {
		return false
	}
	if vendor == "" {
		return true
	}
	// vendor 토큰 별칭: TT device-plugin 은 "kcloud-tt-device-plugin" 로 명명되어
	// "tenstorrent" 문자열을 포함하지 않는다(#21). "tt" 를 별칭으로 허용한다.
	// ponytail: 단일 별칭. 벤더가 늘면 map 으로 승격.
	tokens := []string{strings.ToLower(vendor)}
	if strings.EqualFold(vendor, "tenstorrent") {
		tokens = append(tokens, "tt")
	}
	name := strings.ToLower(pod.Name)
	for _, t := range tokens {
		if strings.Contains(name, t) {
			return true
		}
	}
	for _, val := range pod.Labels {
		lv := strings.ToLower(val)
		for _, t := range tokens {
			if strings.Contains(lv, t) {
				return true
			}
		}
	}
	return false
}

// isNotFound 는 apierrors.IsNotFound 의 thin wrapper.
// 별도 함수로 두는 이유: 미래에 NDR 외 다른 sub-resource NotFound 도 처리할 여지.
func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
