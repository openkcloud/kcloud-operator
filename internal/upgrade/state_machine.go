// ============================================================
// state_machine.go: Driver Upgrade 상태 머신
// 상세: NVIDIA GPU Operator 참조, 노드별 드라이버 업그레이드 상태 전이를 관리
//       Idle → UpgradeRequired → PreFlight → Cordoning → Draining →
//       Upgrading → Validating → Uncordoning → Idle (실패 시 Rollback)
// 생성일: 2026-04-13 | 수정일: 2026-04-15
// ============================================================

package upgrade

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/metrics"
	"kcloud-operator/internal/validator"
)

const driverUpgradingLabelKey = "npu.ai/driver-upgrading"

// DriverUpgradingLabelKey 는 외부 패키지(controller 의 stuck-label sweep / defer cleanup)가
// 동일한 라벨을 가리키도록 노출하는 상수다. 절대 다른 키와 혼용 금지.
const DriverUpgradingLabelKey = driverUpgradingLabelKey

// UpgradeStateMachine은 노드별 드라이버 업그레이드 상태 전이를 담당합니다.
type UpgradeStateMachine struct {
	client.Client
	Recorder record.EventRecorder
}

// TransitionState는 현재 DriverUpgradeState를 읽어 다음 상태로 전이합니다.
// 반환값: requeue 여부, requeueAfter 시간, 에러
func (m *UpgradeStateMachine) TransitionState(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (requeue bool, requeueAfter time.Duration, err error) {
	logger := logf.FromContext(ctx).WithValues(
		"node", state.Spec.NodeName,
		"vendor", state.Spec.Vendor,
		"state", state.Status.State,
	)
	logger.Info("TransitionState 호출")

	// 현재 상태를 Prometheus 메트릭에 기록
	metrics.SetUpgradeState(state.Spec.NodeName, state.Spec.Vendor, state.Status.State)

	switch state.Status.State {
	case "", v1alpha1.UpgradeStateIdle:
		return m.handleIdle(ctx, state, policy)
	case v1alpha1.UpgradeStateRequired:
		return m.handleUpgradeRequired(ctx, state, policy)
	case v1alpha1.UpgradeStatePreFlight:
		return m.handlePreFlight(ctx, state, policy)
	case v1alpha1.UpgradeStateCordoning:
		return m.handleCordoning(ctx, state, policy)
	case v1alpha1.UpgradeStateDraining:
		return m.handleDraining(ctx, state, policy)
	case v1alpha1.UpgradeStateUpgrading:
		return m.handleUpgrading(ctx, state, policy)
	case v1alpha1.UpgradeStateValidating:
		return m.handleValidating(ctx, state, policy)
	case v1alpha1.UpgradeStateUncordoning:
		return m.handleUncordoning(ctx, state, policy)
	case v1alpha1.UpgradeStateRollback:
		return m.handleRollback(ctx, state, policy)
	default:
		logger.Info("알 수 없는 상태, Idle로 리셋", "state", state.Status.State)
		return m.transitionTo(ctx, state, v1alpha1.UpgradeStateIdle, "알 수 없는 상태 리셋", 60*time.Second)
	}
}

// ─────────────────────────────────────────────
// 상태 핸들러
// ─────────────────────────────────────────────

// handleIdle: 버전 불일치 감지 시 UpgradeRequired로 전이
func (m *UpgradeStateMachine) handleIdle(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	desiredVersion := policy.Spec.Driver.Version
	currentVersion := state.Status.CurrentVersion

	if desiredVersion == "" || desiredVersion == currentVersion {
		// 버전 일치: Idle 유지, 60s 후 재확인
		return true, 60 * time.Second, nil
	}

	// autoUpgrade가 false이면 이벤트만 발행하고 Idle 유지
	if policy.Spec.UpgradePolicy == nil || !policy.Spec.UpgradePolicy.AutoUpgrade {
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeAvailable",
			"드라이버 버전 불일치: current=%s desired=%s (autoUpgrade 비활성화)", currentVersion, desiredVersion)
		return true, 60 * time.Second, nil
	}

	// 버전 불일치 + autoUpgrade: UpgradeRequired로 전이
	// PreviousImage는 이번 사이클에서 handleUpgrading이 DS로부터 다시 수집하도록 비움.
	// 이전 업그레이드 값이 남아있으면 잘못된 이미지로 롤백될 수 있음.
	// RollbackAttempts는 새 사이클이므로 0으로 초기화 (P2 무한 루프 방지).
	state.Status.DesiredVersion = desiredVersion
	state.Status.PreviousVersion = currentVersion
	state.Status.PreviousImage = ""
	state.Status.RollbackAttempts = 0
	return m.transitionTo(ctx, state, v1alpha1.UpgradeStateRequired,
		fmt.Sprintf("버전 불일치 감지: %s → %s", currentVersion, desiredVersion), 0)
}

// handleUpgradeRequired: MaxParallelUpgrades 슬롯 확인 후 PreFlight로 전이
func (m *UpgradeStateMachine) handleUpgradeRequired(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	maxParallel := int32(1)
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.MaxParallelUpgrades > 0 {
		maxParallel = policy.Spec.UpgradePolicy.MaxParallelUpgrades
	} else if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.MaxUnavailable > 0 {
		maxParallel = policy.Spec.UpgradePolicy.MaxUnavailable
	}

	active, err := m.countActiveUpgrades(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("활성 업그레이드 수 조회 실패: %w", err)
	}

	if int32(active) >= maxParallel {
		// 슬롯 없음: 대기
		return true, 30 * time.Second, nil
	}

	// 슬롯 확보: PreFlight로 전이
	return m.transitionTo(ctx, state, v1alpha1.UpgradeStatePreFlight, "업그레이드 슬롯 확보", 0)
}

// handlePreFlight: 커널 버전 allowlist 검사 후 Cordoning으로 전이
func (m *UpgradeStateMachine) handlePreFlight(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	// 노드 커널 버전 조회
	var node corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: state.Spec.NodeName}, &node); err != nil {
		return false, 0, fmt.Errorf("노드 조회 실패: %w", err)
	}

	kernelVersion := node.Status.NodeInfo.KernelVersion
	allowlist := policy.Spec.KernelAllowlist

	if len(allowlist) > 0 {
		if !matchesKernelAllowlist(kernelVersion, allowlist) {
			msg := fmt.Sprintf("커널 버전 %s이 allowlist에 없음: %v", kernelVersion, allowlist)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "PreFlightFailed", msg)
			return m.transitionTo(ctx, state, v1alpha1.UpgradeStateRollback, msg, 0)
		}
	}

	// 노드 Ready 상태 확인
	if !isNodeReady(&node) {
		return true, 10 * time.Second, nil
	}

	return m.transitionTo(ctx, state, v1alpha1.UpgradeStateCordoning, "PreFlight 검사 통과", 0)
}

// handleCordoning: 노드를 Unschedulable로 설정 후 Draining으로 전이
func (m *UpgradeStateMachine) handleCordoning(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	if err := m.cordonNode(ctx, state.Spec.NodeName, state); err != nil {
		return false, 0, fmt.Errorf("노드 cordon 실패: %w", err)
	}

	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeCordoned",
		"업그레이드를 위해 노드 %s cordon 완료", state.Spec.NodeName)

	return m.transitionTo(ctx, state, v1alpha1.UpgradeStateDraining, "노드 cordon 완료", 0)
}

// handleDraining: GPU/NPU 워크로드 Pod 삭제 후 Upgrading으로 전이
func (m *UpgradeStateMachine) handleDraining(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	// drainEnabled가 false면 즉시 Upgrading으로
	if policy.Spec.UpgradePolicy == nil || !policy.Spec.UpgradePolicy.DrainEnabled {
		return m.transitionTo(ctx, state, v1alpha1.UpgradeStateUpgrading, "drain 비활성화, 즉시 업그레이드", 0)
	}

	// drain timeout 체크
	drainTimeout := parseDuration(policy.Spec.UpgradePolicy.DrainTimeout, 5*time.Minute)
	if !state.Status.LastTransitionTime.IsZero() &&
		time.Since(state.Status.LastTransitionTime.Time) > drainTimeout {
		if policy.Spec.UpgradePolicy.RollbackOnFailure {
			return m.transitionTo(ctx, state, v1alpha1.UpgradeStateRollback, "drain 타임아웃: 롤백 시작", 0)
		}
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(ctx, state, "Failed", "drain 타임아웃: 수동 조치 필요", 0)
	}

	// nvidia-persistenced 종료 요청 (실제 중지는 driver-manager initContainer에서 수행)
	if strings.EqualFold(state.Spec.Vendor, "nvidia") {
		m.logPersistencedStop(ctx, state.Spec.NodeName)
	}

	// device-plugin Pod 삭제 (커널 모듈 참조 해제를 위해)
	logger := logf.FromContext(ctx)
	if err := m.deleteDevicePluginPods(ctx, state.Spec.NodeName, state.Spec.Vendor); err != nil {
		logger.Error(err, "device-plugin Pod 삭제 실패")
		// 비치명적: 계속 진행
	}

	// 디바이스 워크로드 확인
	hasWorkloads, err := m.hasDeviceWorkloads(ctx, state.Spec.NodeName, state.Spec.Vendor)
	if err != nil {
		return false, 0, fmt.Errorf("디바이스 워크로드 확인 실패: %w", err)
	}

	if hasWorkloads {
		// 워크로드 Pod 삭제 시도: PDB를 존중하는 Eviction API 사용.
		// ForceUpgrade=true일 때만 PDB 위반 시 직접 Delete로 폴백 (kubectl drain --force 동등).
		force := policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.ForceUpgrade
		if err := m.evictDevicePods(ctx, state, force); err != nil {
			return false, 0, fmt.Errorf("디바이스 Pod eviction 실패: %w", err)
		}
		return true, 15 * time.Second, nil
	}

	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeDrained",
		"노드 %s drain 완료", state.Spec.NodeName)
	return m.transitionTo(ctx, state, v1alpha1.UpgradeStateUpgrading, "drain 완료", 0)
}

// handleUpgrading: DaemonSet 이미지 업데이트 후 Validating으로 전이 (requeue 20s)
func (m *UpgradeStateMachine) handleUpgrading(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	// DaemonSet 이름 결정 (driver_daemonset_controller.go line 98 패턴과 일치)
	dsName := fmt.Sprintf("npu-op-driver-%s-%s", strings.ToLower(state.Spec.Vendor), strings.ToLower(state.Spec.Model))
	if state.Spec.Model == "" {
		dsName = fmt.Sprintf("npu-op-driver-%s", strings.ToLower(state.Spec.Vendor))
	}

	var ds appsv1.DaemonSet
	if err := m.Get(ctx, types.NamespacedName{Name: dsName, Namespace: "kube-system"}, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			return true, 20 * time.Second, nil
		}
		return false, 0, fmt.Errorf("DaemonSet 조회 실패: %w", err)
	}

	desiredImage := policy.Spec.Driver.Image
	// 이미지 업데이트
	base := ds.DeepCopy()
	updated := false
	prevImage := ""
	for i := range ds.Spec.Template.Spec.Containers {
		if ds.Spec.Template.Spec.Containers[i].Name == fmt.Sprintf("%s-driver", state.Spec.Vendor) ||
			i == 0 {
			prevImage = ds.Spec.Template.Spec.Containers[i].Image
			if prevImage != desiredImage {
				ds.Spec.Template.Spec.Containers[i].Image = desiredImage
				updated = true
			}
			break
		}
	}

	if updated {
		// 롤백 시 빌드 접미사 포함 원본 이미지를 복구하기 위해 패치 직전에 기록
		if state.Status.PreviousImage == "" && prevImage != "" && prevImage != desiredImage {
			state.Status.PreviousImage = prevImage
		}
		if err := m.Patch(ctx, &ds, client.MergeFrom(base)); err != nil {
			return false, 0, fmt.Errorf("DaemonSet 이미지 업데이트 실패: %w", err)
		}
	}

	// 해당 노드의 기존 드라이버 Pod 삭제 (OnDelete 전략 트리거)
	if err := m.deleteDriverPodOnNode(ctx, dsName, state.Spec.NodeName); err != nil {
		return false, 0, fmt.Errorf("드라이버 Pod 삭제 실패: %w", err)
	}

	m.Recorder.Eventf(state, corev1.EventTypeNormal, "UpgradeStarted",
		"노드 %s 드라이버 업그레이드 시작: %s", state.Spec.NodeName, state.Status.DesiredVersion)

	return m.transitionTo(ctx, state, v1alpha1.UpgradeStateValidating, "DaemonSet 이미지 업데이트 완료", 20*time.Second)
}

// validators 는 handleValidating 이 순차 실행할 Validator 체인이다.
// architectural plan §4.4.3 에 따라 단일 NDR 대기였던 단계를 단계별 책임으로 분리:
//   1) DriverModule  : 노드의 드라이버 커널 모듈 로드 (NDR.driverVersion 매칭)
//   2) DevicePlugin  : kube-system 의 device-plugin Pod ContainersReady
//   3) Workload      : sample 워크로드 ResourceAllocated (skeleton — 후속 PR 에서 활성)
//
// 변수로 두어 테스트에서 주입 가능 — 단, 본 작업에서는 stub 미사용.
var defaultValidators = []validator.Validator{
	&validator.DriverModuleValidator{},
	&validator.DevicePluginValidator{},
	// &validator.WorkloadValidator{}, // 후속 PR 에서 활성
}

// handleValidating 은 validator 체인을 순차 실행해 새 드라이버가 정상 동작하는지 검증한다.
//
// 각 validator 의 의미:
//   - Run 이 (Passed=true) 면 다음 validator 로 진행.
//   - (Passed=false) 면 caller wall-clock budget 안에서 재시도 (requeueAfter 10s).
//   - error 는 controller-runtime 으로 전파 (재시도 가능 API 오류).
//
// 추가 안전장치:
//   - 전체 ValidationTimeout 초과 → Rollback (정책에 따라) / Failed.
//   - 드라이버 Pod CrashLoopBackOff 즉시 감지 → Rollback / Failed (재시도 무의미).
func (m *UpgradeStateMachine) handleValidating(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	logger := logf.FromContext(ctx)

	// ─── 1. 전체 validation timeout 체크 ─────────────────────
	validationTimeout := parseDuration("", 15*time.Minute)
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.ValidationTimeout != "" {
		validationTimeout = parseDuration(policy.Spec.UpgradePolicy.ValidationTimeout, 15*time.Minute)
	}
	if !state.Status.LastTransitionTime.IsZero() &&
		time.Since(state.Status.LastTransitionTime.Time) > validationTimeout {
		if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.RollbackOnFailure {
			return m.transitionTo(ctx, state, v1alpha1.UpgradeStateRollback, "검증 타임아웃: 롤백 시작", 0)
		}
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logger.Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(ctx, state, "Failed", "검증 타임아웃: 수동 조치 필요", 0)
	}

	// ─── 2. CrashLoop 즉시 감지 (재시도 무의미한 hard failure) ───
	// driver-installer Pod 가 CrashLoopBackOff 라면 어떤 validator 도 통과 못 하므로
	// 빠르게 Rollback / Failed 로 전이한다 (legacy 동작 보존).
	dsName := fmt.Sprintf("npu-op-driver-%s-%s", strings.ToLower(state.Spec.Vendor), strings.ToLower(state.Spec.Model))
	if state.Spec.Model == "" {
		dsName = fmt.Sprintf("npu-op-driver-%s", strings.ToLower(state.Spec.Vendor))
	}
	desiredImage := policy.Spec.Driver.Image
	_, crashLoop, err := m.isDriverPodReadyOnNode(ctx, dsName, state.Spec.NodeName, desiredImage)
	if err != nil {
		return false, 0, fmt.Errorf("드라이버 Pod 상태 확인 실패: %w", err)
	}
	if crashLoop {
		if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.RollbackOnFailure {
			return m.transitionTo(ctx, state, v1alpha1.UpgradeStateRollback, "드라이버 Pod CrashLoopBackOff: 롤백 시작", 0)
		}
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logger.Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(ctx, state, "Failed", "드라이버 Pod CrashLoopBackOff: 수동 조치 필요", 0)
	}

	// ─── 3. validator 체인 순차 실행 ─────────────────────────
	desiredVersion := state.Status.DesiredVersion
	for _, v := range defaultValidators {
		// validator 시작 Event (재시도마다 발행되지 않도록 "처음" 진입 시점만 emit 하기 어렵기에
		// per-attempt event 로 발행 — 운영자에 진행 상황 가시성 우선).
		m.Recorder.Eventf(state, corev1.EventTypeNormal,
			fmt.Sprintf("UpgradeValidator-%s-Started", v.Name()),
			"validator 실행: %s (timeout=%s, node=%s)", v.Name(), v.Timeout(), state.Spec.NodeName)

		res, err := v.Run(ctx, m.Client, state.Spec.NodeName, state.Spec.Vendor, desiredVersion)
		if err != nil {
			// 재시도 가능한 API 오류 — controller-runtime 으로 전파.
			return false, 0, fmt.Errorf("validator %s 실행 실패: %w", v.Name(), err)
		}
		if !res.Passed {
			// 검증 미통과 — 상위 ValidationTimeout 안에서 재시도.
			m.Recorder.Eventf(state, corev1.EventTypeNormal,
				fmt.Sprintf("UpgradeValidator-%s-Failed", v.Name()),
				"validator %s 미통과 (재시도 대기): %s", v.Name(), res.Message)
			return true, 10 * time.Second, nil
		}
		m.Recorder.Eventf(state, corev1.EventTypeNormal,
			fmt.Sprintf("UpgradeValidator-%s-Passed", v.Name()),
			"validator %s 통과: %s", v.Name(), res.Message)
	}

	// ─── 4. 모든 validator 통과 → 검증 성공 ──────────────────
	state.Status.CurrentVersion = state.Status.DesiredVersion
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "UpgradeValidated",
		"노드 %s 드라이버 검증 성공: %s", state.Spec.NodeName, state.Status.DesiredVersion)
	return m.transitionTo(ctx, state, v1alpha1.UpgradeStateUncordoning, "검증 성공", 0)
}

// handleUncordoning: 노드를 Schedulable로 복구 후 Idle로 전이
func (m *UpgradeStateMachine) handleUncordoning(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	if err := m.uncordonNode(ctx, state.Spec.NodeName, state); err != nil {
		return false, 0, fmt.Errorf("노드 uncordon 실패: %w", err)
	}

	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeUncordoned",
		"노드 %s uncordon 완료, 업그레이드 성공", state.Spec.NodeName)

	metrics.RecordUpgradeComplete(state.Spec.Vendor, "success")

	return m.transitionTo(ctx, state, v1alpha1.UpgradeStateIdle, "업그레이드 완료", 0)
}

// handleRollback: 이전 버전으로 DaemonSet 복구 후 Upgrading으로 전이
func (m *UpgradeStateMachine) handleRollback(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	metrics.RecordRollback(state.Spec.Vendor)

	// P2: 롤백 시도 횟수 제한. 반복 실패 시 무한 루프 방지.
	maxRollbacks := int32(3)
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.MaxRollbackAttempts > 0 {
		maxRollbacks = policy.Spec.UpgradePolicy.MaxRollbackAttempts
	}
	state.Status.RollbackAttempts++
	if state.Status.RollbackAttempts > maxRollbacks {
		metrics.RecordUpgradeComplete(state.Spec.Vendor, "failure")
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackExhausted",
			"롤백 %d회 초과 (max=%d): 수동 조치 필요 (node=%s)",
			state.Status.RollbackAttempts-1, maxRollbacks, state.Spec.NodeName)
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패: node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(ctx, state, "Failed",
			fmt.Sprintf("롤백 %d회 초과: 수동 조치 필요", maxRollbacks), 0)
	}

	prevVersion := state.Status.PreviousVersion
	if prevVersion == "" {
		// 이전 버전 없음: Failed 처리
		metrics.RecordUpgradeComplete(state.Spec.Vendor, "failure")
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackFailed",
			"롤백할 이전 버전 없음: 수동 조치 필요")
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(ctx, state, "Failed", "이전 버전 없음: 수동 조치 필요", 0)
	}

	dsName := fmt.Sprintf("npu-op-driver-%s-%s", strings.ToLower(state.Spec.Vendor), strings.ToLower(state.Spec.Model))
	if state.Spec.Model == "" {
		dsName = fmt.Sprintf("npu-op-driver-%s", strings.ToLower(state.Spec.Vendor))
	}

	var ds appsv1.DaemonSet
	if err := m.Get(ctx, types.NamespacedName{Name: dsName, Namespace: "kube-system"}, &ds); err != nil {
		return false, 0, fmt.Errorf("DaemonSet 조회 실패: %w", err)
	}

	// 이미지를 이전 버전으로 복구
	// PreviousImage가 저장되어 있으면 전체 이미지 레퍼런스를 그대로 복구 (빌드 접미사 보존).
	// 저장되지 않았다면 RollbackTarget 정책에 따라 처리:
	//   previousValidated : 검증된 image 가 없으므로 Failed 로 전이 (안전).
	//   spec / "" (기본)  : 하위 호환 — 태그만 치환 (legacy, broken image 가능).
	prevImage := state.Status.PreviousImage
	if prevImage == "" {
		safeRollback := ""
		if policy.Spec.UpgradePolicy != nil {
			safeRollback = policy.Spec.UpgradePolicy.RollbackTarget
		}
		if safeRollback == "previousValidated" {
			metrics.RecordUpgradeComplete(state.Spec.Vendor, "failure")
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackRefused",
				"RollbackTarget=previousValidated 인데 PreviousImage 미보유: 수동 조치 필요 (node=%s)", state.Spec.NodeName)
			if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
				logf.FromContext(ctx).Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
				m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
					"Failed 전이 중 라벨 제거 실패: node=%s err=%v", state.Spec.NodeName, err)
			}
			return m.transitionTo(ctx, state, "Failed",
				"RollbackTarget=previousValidated: PreviousImage 미보유로 안전 롤백 불가", 0)
		}
	}
	base := ds.DeepCopy()
	for i := range ds.Spec.Template.Spec.Containers {
		if prevImage != "" {
			ds.Spec.Template.Spec.Containers[i].Image = prevImage
		} else {
			img := ds.Spec.Template.Spec.Containers[i].Image
			ds.Spec.Template.Spec.Containers[i].Image = replaceImageTag(img, prevVersion)
		}
		break
	}

	if err := m.Patch(ctx, &ds, client.MergeFrom(base)); err != nil {
		return false, 0, fmt.Errorf("DaemonSet 롤백 이미지 패치 실패: %w", err)
	}

	// 해당 노드 Pod 삭제 (이전 버전 Pod 재생성 트리거)
	if err := m.deleteDriverPodOnNode(ctx, dsName, state.Spec.NodeName); err != nil {
		return false, 0, fmt.Errorf("드라이버 Pod 삭제 실패: %w", err)
	}

	// DesiredVersion을 이전 버전으로 재설정 후 Upgrading(검증)으로
	state.Status.DesiredVersion = prevVersion
	m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackStarted",
		"노드 %s 롤백 시작: → %s", state.Spec.NodeName, prevVersion)

	// 롤백 메트릭 기록
	metrics.RecordRollback(state.Spec.Vendor)
	metrics.RecordUpgradeComplete(state.Spec.Vendor, "rollback")

	return m.transitionTo(ctx, state, v1alpha1.UpgradeStateValidating, fmt.Sprintf("롤백 시작: %s", prevVersion), 20*time.Second)
}

// logPersistencedStop은 nvidia-persistenced 종료가 필요함을 로깅합니다.
// 실제 중지는 driver-manager initContainer에서 nsenter를 통해 수행됩니다.
func (m *UpgradeStateMachine) logPersistencedStop(ctx context.Context, nodeName string) {
	logger := logf.FromContext(ctx)
	logger.Info("nvidia-persistenced 중지 예정 (driver-manager initContainer에서 처리)", "node", nodeName)
}

// ─────────────────────────────────────────────
// 헬퍼 함수
// ─────────────────────────────────────────────

// transitionTo는 상태를 변경하고 requeue 정보를 반환합니다.
// requeueAfter=0이면 즉시 requeue (Requeue=true).
// 상태 전이 시 Prometheus 메트릭을 기록합니다.
func (m *UpgradeStateMachine) transitionTo(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	nextState string,
	message string,
	requeueAfter time.Duration,
) (bool, time.Duration, error) {
	prevState := state.Status.State
	vendor := state.Spec.Vendor
	nodeName := state.Spec.NodeName

	// 이전 상태의 소요 시간 기록
	if !state.Status.LastTransitionTime.IsZero() {
		elapsed := time.Since(state.Status.LastTransitionTime.Time)
		phase := stateToPhase(prevState)
		if phase != "" {
			metrics.RecordPhaseDuration(vendor, phase, elapsed)
		}
	}

	state.Status.State = nextState
	state.Status.LastTransitionTime = metav1.Now()
	state.Status.Message = message

	// 새 상태 게이지 업데이트
	metrics.SetUpgradeState(nodeName, vendor, nextState)

	// 업그레이드 완료 감지: Uncordoning → Idle
	if prevState == v1alpha1.UpgradeStateUncordoning && nextState == v1alpha1.UpgradeStateIdle {
		metrics.RecordUpgradeComplete(vendor, "success")
	}

	return true, requeueAfter, nil
}

// stateToPhase는 UpgradeState 상수를 메트릭 phase 레이블로 변환합니다.
func stateToPhase(state string) string {
	switch state {
	case v1alpha1.UpgradeStatePreFlight:
		return "preflight"
	case v1alpha1.UpgradeStateCordoning:
		return "cordoning"
	case v1alpha1.UpgradeStateDraining:
		return "draining"
	case v1alpha1.UpgradeStateUpgrading:
		return "upgrading"
	case v1alpha1.UpgradeStateValidating:
		return "validating"
	case v1alpha1.UpgradeStateUncordoning:
		return "uncordoning"
	default:
		return ""
	}
}

// countActiveUpgrades는 Idle/Failed 이외의 상태인 DriverUpgradeState 수를 반환합니다.
func (m *UpgradeStateMachine) countActiveUpgrades(ctx context.Context) (int, error) {
	var list v1alpha1.DriverUpgradeStateList
	if err := m.List(ctx, &list); err != nil {
		return 0, err
	}
	count := 0
	for _, s := range list.Items {
		if isActiveState(s.Status.State) {
			count++
		}
	}
	return count, nil
}

// isActiveState는 업그레이드가 진행 중인 상태(슬롯 점유)인지 반환합니다.
func isActiveState(state string) bool {
	switch state {
	case v1alpha1.UpgradeStatePreFlight,
		v1alpha1.UpgradeStateCordoning,
		v1alpha1.UpgradeStateDraining,
		v1alpha1.UpgradeStateUpgrading,
		v1alpha1.UpgradeStateValidating,
		v1alpha1.UpgradeStateUncordoning,
		v1alpha1.UpgradeStateRollback:
		return true
	}
	return false
}

// cordonNode는 노드를 Unschedulable로 설정하고 driver-upgrading 라벨을 추가합니다.
func (m *UpgradeStateMachine) cordonNode(ctx context.Context, nodeName string, state *v1alpha1.DriverUpgradeState) error {
	var node corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	base := node.DeepCopy()
	changed := false
	if !node.Spec.Unschedulable {
		node.Spec.Unschedulable = true
		changed = true
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if node.Labels[driverUpgradingLabelKey] != "true" {
		node.Labels[driverUpgradingLabelKey] = "true"
		changed = true
	}
	if !changed {
		return nil
	}
	if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return err
	}
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeUpgradeLabelApplied",
		"노드 %s에 npu.ai/driver-upgrading 라벨 추가 (device-plugin 스케줄 차단)", nodeName)
	return nil
}

// uncordonNode는 노드를 Schedulable로 복구하고 driver-upgrading 라벨을 제거합니다.
func (m *UpgradeStateMachine) uncordonNode(ctx context.Context, nodeName string, state *v1alpha1.DriverUpgradeState) error {
	var node corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	base := node.DeepCopy()
	changed := false
	if node.Spec.Unschedulable {
		node.Spec.Unschedulable = false
		changed = true
	}
	if _, ok := node.Labels[driverUpgradingLabelKey]; ok {
		delete(node.Labels, driverUpgradingLabelKey)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return err
	}
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeUpgradeLabelRemoved",
		"노드 %s에서 npu.ai/driver-upgrading 라벨 제거 (device-plugin 재스케줄 허용)", nodeName)
	return nil
}

// clearUpgradingLabel은 Failed 전이 시 device-plugin 재스케줄이 가능하도록
// npu.ai/driver-upgrading 라벨만 제거한다. unschedulable은 수동 조치를 위해 보존.
func (m *UpgradeStateMachine) clearUpgradingLabel(ctx context.Context, nodeName string, state *v1alpha1.DriverUpgradeState) error {
	var node corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	if _, ok := node.Labels[driverUpgradingLabelKey]; !ok {
		return nil
	}
	base := node.DeepCopy()
	delete(node.Labels, driverUpgradingLabelKey)
	if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return err
	}
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeUpgradeLabelRemoved",
		"노드 %s에서 npu.ai/driver-upgrading 라벨 제거 (device-plugin 재스케줄 허용)", nodeName)
	return nil
}

// hasDeviceWorkloads는 노드에 GPU/NPU 리소스를 사용하는 Running Pod가 있는지 확인합니다.
func (m *UpgradeStateMachine) hasDeviceWorkloads(ctx context.Context, nodeName string, vendor string) (bool, error) {
	var podList corev1.PodList
	if err := m.List(ctx, &podList); err != nil {
		return false, err
	}
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for resName := range c.Resources.Limits {
				rn := string(resName)
				if strings.Contains(rn, "nvidia.com/gpu") || strings.Contains(rn, "furiosa.ai/") {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// evictDevicePods는 노드에서 GPU/NPU 리소스를 사용하는 Pod를 PDB를 존중하며 축출합니다.
// policy/v1 Eviction API를 사용하므로 API server가 PodDisruptionBudget을 자동 검사합니다.
//
// 에러 처리:
//   - 429 TooManyRequests: PDB 위반. force=false면 이벤트만 발행하고 다음 주기에 재시도.
//     force=true면 직접 Delete로 폴백 (kubectl drain --force 동등).
//   - 404 NotFound: 이미 삭제된 Pod, 성공 처리.
//   - 기타 에러: 상위로 전파.
func (m *UpgradeStateMachine) evictDevicePods(ctx context.Context, state *v1alpha1.DriverUpgradeState, force bool) error {
	logger := logf.FromContext(ctx)
	var podList corev1.PodList
	if err := m.List(ctx, &podList); err != nil {
		return err
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != state.Spec.NodeName || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if !podUsesDevice(pod) {
			continue
		}

		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
		}
		err := m.SubResource("eviction").Create(ctx, pod, eviction)
		if err == nil || apierrors.IsNotFound(err) {
			// 성공 또는 이미 삭제됨
			continue
		}
		if apierrors.IsTooManyRequests(err) {
			// PDB 위반. force면 Delete로 폴백, 아니면 다음 주기에 재시도.
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "EvictionBlocked",
				"PDB로 인해 Pod %s/%s eviction 차단 (force=%v)", pod.Namespace, pod.Name, force)
			if !force {
				logger.Info("PDB 위반으로 eviction 지연, 재시도 예정",
					"pod", pod.Name, "namespace", pod.Namespace)
				continue
			}
			logger.Info("ForceUpgrade=true, PDB 무시하고 Delete 폴백",
				"pod", pod.Name, "namespace", pod.Namespace)
			if delErr := m.Delete(ctx, pod); client.IgnoreNotFound(delErr) != nil {
				return fmt.Errorf("force delete 실패 (pod=%s/%s): %w", pod.Namespace, pod.Name, delErr)
			}
			continue
		}
		// 기타 에러는 상위로 전파
		return fmt.Errorf("eviction 실패 (pod=%s/%s): %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// podUsesDevice는 Pod가 GPU/NPU 리소스를 요청하는지 반환합니다.
func podUsesDevice(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		for resName := range c.Resources.Limits {
			rn := string(resName)
			if strings.Contains(rn, "nvidia.com/gpu") || strings.Contains(rn, "furiosa.ai/") {
				return true
			}
		}
	}
	return false
}

// deleteDevicePluginPods는 해당 노드의 device-plugin Pod를 삭제합니다.
// device-plugin은 GPU 리소스를 요청하지 않지만 /dev/nvidia*를 직접 마운트하여
// 커널 모듈 참조를 잡으므로, drain 시 삭제해야 rmmod가 가능합니다.
func (m *UpgradeStateMachine) deleteDevicePluginPods(ctx context.Context, nodeName string, vendor string) error {
	var podList corev1.PodList
	if err := m.List(ctx, &podList, client.InNamespace("kube-system")); err != nil {
		return err
	}
	logger := logf.FromContext(ctx)
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName {
			continue
		}
		isDevicePlugin := false
		for key, val := range pod.Labels {
			if key == "app.kubernetes.io/name" && strings.Contains(val, "device-plugin") {
				isDevicePlugin = true
				break
			}
		}
		if !isDevicePlugin && strings.Contains(pod.Name, "device-plugin") {
			isDevicePlugin = true
		}
		if isDevicePlugin {
			logger.Info("device-plugin Pod 삭제", "pod", pod.Name, "node", nodeName)
			if err := m.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// deleteDriverPodOnNode는 해당 노드의 드라이버 DaemonSet Pod를 삭제합니다 (OnDelete 전략 트리거).
func (m *UpgradeStateMachine) deleteDriverPodOnNode(ctx context.Context, dsName string, nodeName string) error {
	var podList corev1.PodList
	if err := m.List(ctx, &podList, client.InNamespace("kube-system")); err != nil {
		return err
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName {
			continue
		}
		// DaemonSet 소유자 확인
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "DaemonSet" && ref.Name == dsName {
				if err := m.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// isDriverPodReadyOnNode는 해당 노드의 드라이버 Pod가 ContainersReady인지 확인합니다.
// desiredImage가 지정된 경우 Pod의 컨테이너 이미지가 일치하는지도 검증합니다.
// 이전 버전 Pod의 Ready 상태를 새 버전으로 오판하는 것을 방지합니다.
// 반환값: (ready bool, crashLoop bool, err error)
func (m *UpgradeStateMachine) isDriverPodReadyOnNode(ctx context.Context, dsName string, nodeName string, desiredImage ...string) (bool, bool, error) {
	var podList corev1.PodList
	if err := m.List(ctx, &podList, client.InNamespace("kube-system")); err != nil {
		return false, false, err
	}
	logger := logf.FromContext(ctx)
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		// Terminating Pod 스킵: 이전 Pod와 새 Pod 공존 구간에서 오판 방지
		if pod.DeletionTimestamp != nil {
			continue
		}
		isDSPod := false
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "DaemonSet" && ref.Name == dsName {
				isDSPod = true
				break
			}
		}
		if !isDSPod {
			continue
		}

		// 이미지 검증: desiredImage가 지정된 경우 Pod 이미지가 일치하는지 확인
		if len(desiredImage) > 0 && desiredImage[0] != "" {
			podImage := ""
			if len(pod.Spec.Containers) > 0 {
				podImage = pod.Spec.Containers[0].Image
			}
			if podImage != desiredImage[0] {
				logger.Info("이전 버전 Pod 감지, 새 Pod 대기 중",
					"pod", pod.Name, "podImage", podImage, "desiredImage", desiredImage[0])
				return false, false, nil
			}
		}

		// CrashLoopBackOff 확인
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				return false, true, nil
			}
		}

		// ContainersReady 확인
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.ContainersReady && cond.Status == corev1.ConditionTrue {
				return true, false, nil
			}
		}
		return false, false, nil
	}
	return false, false, nil
}

// isNodeReady는 노드가 Ready 상태인지 확인합니다.
func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// matchesKernelAllowlist는 커널 버전이 allowlist 패턴 중 하나와 일치하는지 확인합니다.
// 패턴에서 "*"는 정규식 ".*"로 변환됩니다.
func matchesKernelAllowlist(kernelVersion string, allowlist []string) bool {
	for _, pattern := range allowlist {
		// glob 스타일 → 정규식
		regexStr := "^" + regexp.QuoteMeta(pattern) + "$"
		regexStr = strings.ReplaceAll(regexStr, `\*`, `.*`)
		if matched, err := regexp.MatchString(regexStr, kernelVersion); err == nil && matched {
			return true
		}
	}
	return false
}

// parseDuration은 문자열을 time.Duration으로 파싱합니다. 실패 시 defaultDur 반환.
func parseDuration(s string, defaultDur time.Duration) time.Duration {
	if s == "" {
		return defaultDur
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultDur
	}
	return d
}

// replaceImageTag는 이미지 문자열의 태그를 newTag로 교체합니다.
// 예: "registry/img:oldTag" → "registry/img:newTag"
func replaceImageTag(image string, newTag string) string {
	idx := strings.LastIndex(image, ":")
	if idx == -1 {
		return image + ":" + newTag
	}
	return image[:idx+1] + newTag
}

// EnsureUpgradingLabelRemoved 는 노드에서 npu.ai/driver-upgrading 라벨을 idempotent 하게 제거한다.
// controller 의 defer cleanup / stuck-label sweep 가 호출하기 위한 외부 진입점.
//
// 동작:
//   - 라벨 없으면 no-op (라벨 patch 호출 없음 → API 부하 0)
//   - 라벨 있으면 MergeFrom patch 로 제거. Conflict 발생 시 최대 3회 재시도.
//   - reconcile context cancel 와 무관하게 cleanup 이 진행되도록 호출자가 별도 ctx 를 주입할 책임.
func (m *UpgradeStateMachine) EnsureUpgradingLabelRemoved(ctx context.Context, nodeName string) error {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		var node corev1.Node
		if err := m.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if _, ok := node.Labels[driverUpgradingLabelKey]; !ok {
			return nil
		}
		base := node.DeepCopy()
		delete(node.Labels, driverUpgradingLabelKey)
		if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("%d회 conflict 재시도 후 라벨 제거 실패: %w", maxRetries, lastErr)
}
