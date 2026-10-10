// ============================================================
// state_machine.go: Driver Upgrade 상태 머신
// 상세: NVIDIA GPU Operator 참조, 노드별 드라이버 업그레이드 상태 전이를 관리
//       Idle → UpgradeRequired → PreFlight → Cordoning → Draining →
//       Upgrading → Validating → Uncordoning → Idle (실패 시 Rollback)
//       Rollback 도 maxRollbackAttempts 초과 시 터미널 Failed 로 전이 (자동 복구 중단).
// 생성일: 2026-04-13 | 수정일: 2026-10-10 (verifiedVersions 허용 목록 검사를 autoUpgrade 게이트 앞으로 이동;
//       WorkloadProbe 제한시간이 검증 예산을 넘으면 PreFlight 경고 추가;
//       UpgradeVendor FieldManager 명시에 맞춰 ownSpecManagerPrefixes 주석 갱신; 계획서 항목 토큰 제거)
// ============================================================

package upgrade

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/draclaim"
	"kcloud-operator/internal/driverjob"
	"kcloud-operator/internal/intent"
	"kcloud-operator/internal/metrics"
	"kcloud-operator/internal/naming"
	"kcloud-operator/internal/validator"
)

const driverUpgradingLabelKey = "npu.ai/driver-upgrading"

// driverUpgradingBlockingLabelKey 는 detector / device-plugin 등 driver pod 의 rmmod 와
// 충돌할 수 있는 워크로드의 spawn 을 차단하는 좁은 lifecycle 의 라벨이다.
// driver-upgrading 라벨이 사이클 전체 (Cordoning ~ Uncordoning) 에 걸친 추적용이라면,
// 이 라벨은 Cordoning 진입 시 추가되고 Validating 진입 시점에 제거된다 (architectural plan §4.4
// 옵션 A). Validating 부터 detector 가 다시 spawn 가능 → NDR 갱신 → DriverModuleValidator
// 통과 → Validating 데드락 차단.
const driverUpgradingBlockingLabelKey = "npu.ai/driver-upgrading-blocking"

// DriverUpgradingLabelKey 는 외부 패키지(controller 의 stuck-label sweep / defer cleanup)가
// 동일한 라벨을 가리키도록 노출하는 상수다. 절대 다른 키와 혼용 금지.
const DriverUpgradingLabelKey = driverUpgradingLabelKey

// DriverUpgradingBlockingLabelKey 는 좁은 lifecycle 의 detector 차단 라벨을 외부 패키지에
// 노출한다. detector DS nodeAffinity / sweep / defer cleanup 가 이 키를 참조한다.
const DriverUpgradingBlockingLabelKey = driverUpgradingBlockingLabelKey

// quiesceOnDriverUpgradeLabelKey 는 driver upgrade cycle 동안 자동으로 scale=0 으로 quiesce
// 하고 cycle 종료 시 원래 replicas 로 복구할 Deployment 를 식별하는 opt-in 라벨 키이다.
// gpu-stress 같은 NodeAffinity reject 로 Failed Pod 가 누적되는 테스트/내부 워크로드만 라벨을
// 붙이고 production critical workload 는 PDB + drain 표준 흐름을 따른다 (architectural plan §A6.1).
const quiesceOnDriverUpgradeLabelKey = "npu.ai/quiesce-on-driver-upgrade"

// QuiesceOnDriverUpgradeLabelKey 는 외부 패키지(test/manifest)가 동일한 라벨을 가리키도록
// 노출하는 상수다.
const QuiesceOnDriverUpgradeLabelKey = quiesceOnDriverUpgradeLabelKey

// QuiesceReplicasBackupAnnotation 은 quiesce 시 Deployment 의 원래 replicas 값을 dual-write
// 하는 annotation 키다 (followup plan §F2). DUS.Status.QuiescedDeployments 백업과 함께
// 저장되어 operator restart 후 status 손실 시 fallback 복구 경로로 활용된다.
// 값은 base-10 정수 문자열 (예: "3").
const QuiesceReplicasBackupAnnotation = "npu.ai/replicas-backup"

// cordonOwnerAnnotationKey 는 이 상태머신이 직접 노드를 unschedulable 로 뒤집었을 때만 노드에
// 남기는 소유권 표식이며 값은 그 cordon 을 건 DUS 이름이다.
// 왜 라벨이 아니라 별도 annotation 인가: driver-upgrading 라벨은 Failed 전이 시점에
// clearUpgradingLabel 이 지운다(device-plugin 재스케줄 목적). 그래서 Failed 에 도달한 뒤에는
// 라벨로 "이 cordon 이 우리 것인가" 를 물을 수 없다. 표식의 수명이 cordon 의 수명과 같아야 한다.
const cordonOwnerAnnotationKey = "npu.ai/cordoned-by-driver-upgrade"

// failedCordonRecheckInterval 은 Failed 에서 cordon 해제를 보류했을 때(벤더 광고가 아직 0 이
// 아님) 광고를 다시 볼 주기다. 보류에는 재진입이 없으면 의미가 없다 — 광고는 device-plugin 이
// 뒤늦게 재시작하며 떨어질 수 있다.
const failedCordonRecheckInterval = 60 * time.Second

// hostnameLabelKey 는 deploymentTargetsNode 가 nodeSelector / nodeAffinity 매칭에 사용하는
// Kubernetes 표준 노드 hostname 라벨 키다.
const hostnameLabelKey = "kubernetes.io/hostname"

// labelValueTrue 는 boolean 라벨/어노테이션 값으로 사용하는 "true" 문자열이다.
const labelValueTrue = "true"

// defaultIdleCooldown 은 UpgradePolicy.IdleCooldownSeconds 가 nil 일 때 적용되는 기본값이다.
// Idle 진입 후 이 시간 동안은 신규 upgrade trigger 를 거부 (requeue) 하여
// rolling-update 테스트에서 연속 trigger 가 mid-state 로 오인되는 것을 막는다.
// IdleCooldownSeconds 를 0 또는 음수로 두면 cooldown 비활성화.
const defaultIdleCooldown = 10 * time.Second

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

	// reconcile 진입 시 broken PreviousImage 를 안전하게 정리한다.
	// operator restart / 이전 버전 배포로 인해 plain tag (예: ":580.142") 가
	// PreviousImage 에 잔류하는 경우 rollback 시 broken Pod 으로 회귀하는 것을 방지.
	if state.Status.PreviousImage != "" && !isVerifiedBuildTag(state.Status.PreviousImage) {
		logger.Info("Broken PreviousImage cleared for safety", "previousImage", state.Status.PreviousImage)
		state.Status.PreviousImage = ""
		if err := m.Status().Update(ctx, state); err != nil {
			return false, 0, err
		}
	}

	// 현재 상태를 Prometheus 메트릭에 기록
	metrics.SetUpgradeState(state.Spec.NodeName, state.Spec.Vendor, state.Status.State)

	// 관리자 롤백 요청(어노테이션)은 Idle·Failed 에서만 받는다 — 전이 중에는 자동 롤백 경로가
	// 이미 있고 둘이 겹치면 진행 상태가 뒤섞인다.
	//
	// 어노테이션은 결과(상태 전이)가 영속된 뒤에 지운다 — 먼저 지우고 나중에 영속하면, 영속이
	// 실패했을 때 요청 신호(어노테이션)는 이미 사라졌는데 상태는 그대로 남아 다음 reconcile 이
	// 재시도할 방법이 없어진다. 그래서 이 분기는 상태를 스스로(m.Status().Update) 영속한다 —
	// 위 broken-PreviousImage 정리와 같은 패턴이며, 호출자(controller)가 뒤이어 하는
	// Status().Update 는 이미 영속된 값을 다시 쓰는 멱등 호출이 된다.
	if requestedAt, ok := state.Annotations[v1alpha1.RollbackRequestedAnnotation]; ok {
		if requestedAt == state.Status.ConsumedRollbackRequest {
			// 이미 소비한 요청이 제거 실패로 남은 것이다(Idle·Failed 재진입 이후까지 미뤄져도) —
			// 다시 받아들이지 않고 어노테이션 정리만 재시도한다.
			if err := m.clearRollbackRequest(ctx, state); err != nil {
				logger.Error(err, "소비된 롤백 요청 어노테이션 제거 재시도 실패")
			}
		} else {
			switch {
			case v1alpha1.RollbackRequestAccepted(state.Status.State):
				state.Status.ConsumedRollbackRequest = requestedAt
				requeue, requeueAfter, _ := m.transitionTo(state, v1alpha1.UpgradeStateRollback,
					"관리자 요청 롤백("+requestedAt+")", 0)
				if err := m.Status().Update(ctx, state); err != nil {
					return false, 0, err
				}
				m.Recorder.Eventf(state, corev1.EventTypeNormal, "RollbackRequested",
					"관리자 요청(%s)으로 Rollback 전이 (node=%s)", requestedAt, state.Spec.NodeName)
				if err := m.clearRollbackRequest(ctx, state); err != nil {
					// 상태는 이미 Rollback 으로 영속됐다 — 아래 case(state==Rollback)가 다음
					// reconcile 에서 어노테이션 제거만 재시도한다(재전이는 하지 않는다).
					logger.Error(err, "롤백 요청 어노테이션 제거 실패 — 다음 reconcile 에서 재시도")
				}
				return requeue, requeueAfter, nil
			case state.Status.State == v1alpha1.UpgradeStateRollback:
				// 이 요청으로 이미 Rollback 에 들어간 뒤, 어노테이션 제거만 실패해 남은 경우다.
				// 위 case 조건이 이미 거짓이라 재전이는 없다 — 제거만 재시도한다. 그러지 않으면
				// 상태가 나중에 Idle·Failed 로 돌아왔을 때 같은 요청이 새 요청으로 오인돼
				// 다시 전이한다.
				if err := m.clearRollbackRequest(ctx, state); err != nil {
					logger.Error(err, "롤백 요청 어노테이션 제거 재시도 실패")
				}
			default:
				logger.Info("롤백 요청 보류 — 전이 중", "requestedAt", requestedAt)
			}
		}
	}

	switch state.Status.State {
	case "", v1alpha1.UpgradeStateIdle:
		return m.handleIdle(ctx, state, policy)
	case v1alpha1.UpgradeStateRequired:
		return m.handleUpgradeRequired(ctx, state, policy)
	case v1alpha1.UpgradeStatePreFlight:
		return m.handlePreFlight(ctx, state, policy)
	case v1alpha1.UpgradeStateCordoning:
		return m.handleCordoning(ctx, state)
	case v1alpha1.UpgradeStateDraining:
		return m.handleDraining(ctx, state, policy)
	case v1alpha1.UpgradeStateUpgrading:
		return m.handleUpgrading(ctx, state, policy)
	case v1alpha1.UpgradeStateValidating:
		// install 성공 후 cross-major 로 커널 모듈 교체에 재부팅이 필요하면(NDR.needsReboot),
		// validator 체인 앞에서 RebootRequired 로 분기(재부팅 전엔 validator 가 구 버전을 보고 실패).
		// 마커가 없으면 이 분기를 절대 타지 않는다 — 근거 없는 재부팅은 고착보다 나쁘다.
		// 마커가 있을 때 남는 문제는 그 관측을 믿어도 되는가다: 재부팅 후 install Job 이 host
		// 마커를 소거해도 NDR 반영은 detector 스캔주기(30s)만큼 지연되어, stale-true 를 읽으면
		// "재부팅이 안 먹혔다"로 오판→MaxReboots 초과→Failed 가 된다(실측 25~30s > requeue 20s).
		// 그래서 rebootMarkerIsFresh 가 "이번 install Job 완료보다 나중 관측인가"로 가른다.
		if need, nerr := m.nodeNeedsReboot(ctx, state.Spec.NodeName, state.Spec.Vendor, state.Spec.Model); nerr == nil &&
			need && m.rebootMarkerIsFresh(ctx, state) {
			return m.transitionTo(state, v1alpha1.UpgradeStateRebootRequired,
				"cross-major: 커널 모듈 교체 위해 노드 재부팅 필요", 0)
		}
		return m.handleValidating(ctx, state, policy)
	case v1alpha1.UpgradeStateRebootRequired:
		return m.handleRebootRequired(ctx, state, policy)
	case v1alpha1.UpgradeStateRebooting:
		return m.handleRebooting(ctx, state, policy)
	case v1alpha1.UpgradeStateUncordoning:
		return m.handleUncordoning(ctx, state)
	case v1alpha1.UpgradeStateRollback:
		return m.handleRollback(ctx, state, policy)
	case v1alpha1.UpgradeStateFailed:
		return m.handleFailed(ctx, state)
	case v1alpha1.UpgradeStateUnverifiedVersion:
		// 터미널이지만 정책이 바뀌면 나간다 — desired 가 verifiedVersions 안으로 들어오거나(목록 추가·버전 되돌림)
		// 목록이 비면 Idle 로 돌아가 정상 판정을 다시 받는다. 전에는 DUS 삭제·재생성만이 출구였고 그 재생성은
		// currentVersion 이 비어 install Job 을 돌렸다.
		desired := policy.Spec.Driver.Version
		if len(policy.Spec.VerifiedVersions) == 0 || containsString(policy.Spec.VerifiedVersions, desired) {
			return m.transitionTo(state, v1alpha1.UpgradeStateIdle,
				fmt.Sprintf("정책 버전 %s 가 verifiedVersions 안으로 들어와 Idle 복귀", desired), 0)
		}
		logger.Info("UnverifiedVersion 터미널 상태 — DIP.spec.verifiedVersions 수정 필요 (수동 조치)",
			"node", state.Spec.NodeName, "desiredVersion", state.Status.DesiredVersion)
		return true, 60 * time.Second, nil
	default:
		logger.Info("알 수 없는 상태, Idle로 리셋", "state", state.Status.State)
		return m.transitionTo(state, v1alpha1.UpgradeStateIdle, "알 수 없는 상태 리셋", 60*time.Second)
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

	// Idle 에서도 선택 결과를 최신으로 유지한다. 호출부(driver_upgrade_controller.go 의
	// Status().Update)가 전이 여부와 무관하게 status 를 저장하므로 여기서 바꾼 값이 영속한다.
	if err := m.refreshResolvedImage(ctx, state, policy); err != nil {
		return false, 0, err
	}

	// TrackOnly 정책: 설치/업그레이드 경로를 절대 트리거하지 않는다(버전 관측 전용).
	// 아래 Job 모드 self-heal(driverLoaded=false) 트리거보다 먼저 차단하여, installer 미비
	// 벤더(예: Tenstorrent — 실제 설치는 DKMS 2차)의 self-heal 엣지에서도 install Job 이
	// 생성되지 않게 한다(DriverSpec.TrackOnly 계약: "install Job 을 생성하지 않습니다").
	// DUS 는 driver_upgrade_controller 의 ensureTrackOnlyState 가 Idle 로 동기화하며, 여기서는
	// state=="" 만 1회 Idle 로 정규화해 메트릭/`kubectl get dus` 가 Idle 을 보고하도록 한다.
	if policy.Spec.Driver.TrackOnly {
		if state.Status.State == "" {
			return m.transitionTo(state, v1alpha1.UpgradeStateIdle,
				"TrackOnly: 버전 관측 전용(설치 경로 없음)", 60*time.Second)
		}
		return true, 60 * time.Second, nil
	}

	// Job 모드는 상주 DS 가 없어 driverLoaded=false(초기 미설치 / 재부팅·수동 rmmod
	// 로 모듈 언로드 / 드라이버 crash) 상태를 스스로 복구할 수 없다. 따라서 desired 버전이 지정돼
	// 있고 (a) 아직 아무 버전도 없거나(currentVersion=="") (b) NDR 이 driverLoaded=false 를 보고하면,
	// 버전 일치 여부와 무관하게 install Job 을 트리거한다. autoUpgrade 무관(설치/복구는 기대된
	// 동작) — daemonset 의 DS pod 가 항상 modprobe 로 재로딩하는 것과 동등성을 맞춘다.
	// 버전 변경 업그레이드(driver 는 로드됐고 desired 만 상이)는 아래 autoUpgrade 게이트를 존중한다.
	if isJobMode(policy) && desiredVersion != "" {
		needInstall := currentVersion == ""
		if !needInstall {
			notLoaded, err := m.driverNotLoaded(ctx, state.Spec.NodeName, state.Spec.Vendor, state.Spec.Model)
			if err != nil {
				return false, 0, err
			}
			needInstall = notLoaded
		}
		if needInstall {
			// verifiedVersions 화이트리스트는 설치/복구에도 적용(안전).
			if len(policy.Spec.VerifiedVersions) > 0 && !containsString(policy.Spec.VerifiedVersions, desiredVersion) {
				msg := fmt.Sprintf("Driver version %s 가 verifiedVersions 화이트리스트에 없음: %v",
					desiredVersion, policy.Spec.VerifiedVersions)
				m.Recorder.Eventf(state, corev1.EventTypeWarning, "UnverifiedVersion", "%s", msg)
				state.Status.DesiredVersion = desiredVersion
				return m.transitionTo(state, v1alpha1.UpgradeStateUnverifiedVersion, msg, 0)
			}
			state.Status.DesiredVersion = desiredVersion
			state.Status.PreviousVersion = ""
			state.Status.PreviousImage = ""
			state.Status.RollbackAttempts = 0
			resetRebootTracking(&state.Status) // 새 사이클: 재부팅 카운터·마커 초기화
			m.Recorder.Eventf(state, corev1.EventTypeNormal, "InstallRequired",
				"Job 모드 설치/복구 필요(driverLoaded=false): 목표 버전 %s (autoUpgrade 무관)", desiredVersion)
			return m.transitionTo(state, v1alpha1.UpgradeStateRequired,
				fmt.Sprintf("Job 모드 설치/복구: %s", desiredVersion), 0)
		}
	}

	if desiredVersion == "" || desiredVersion == currentVersion {
		// 버전 일치: Idle 유지, 60s 후 재확인.
		// 신규 DUS(state="")는 호스트 드라이버가 이미 목표 버전이면 업그레이드 사이클을
		// 거치지 않으므로 status.state 가 빈 문자열로 남는다. 여기서 1회 Idle 로 정규화하여
		// `kubectl get dus` STATE 와 메트릭이 Idle 을 명확히 보고하도록 한다.
		// 이미 Idle 이면 재기록하지 않아 LastTransitionTime/IdleCooldown churn 을 피한다.
		if state.Status.State == "" {
			return m.transitionTo(state, v1alpha1.UpgradeStateIdle,
				"초기 상태 정규화 (버전 일치)", 60*time.Second)
		}
		return true, 60 * time.Second, nil
	}

	// verifiedVersions 화이트리스트 검사는 autoUpgrade 게이트보다 앞이다 — 허용 목록 밖 버전은
	// 자동 업그레이드를 끈 정책에서도 거부 사유를 드러내야 한다. 종전 순서에서는 autoUpgrade=false
	// 가 먼저 돌려보내 UpgradeAvailable("새 버전 있음") 경고만 남고, 진짜 이유(목록 밖)는
	// 어디에도 보이지 않았다(2026-10-09 라이브 확인). 이 전이는 Job 을 만들지 않고 cordon 도
	// 하지 않으며, 버전이 목록에 다시 들어오면 handleUnverifiedVersion 이 Idle 로 되돌린다.
	if len(policy.Spec.VerifiedVersions) > 0 && !containsString(policy.Spec.VerifiedVersions, desiredVersion) {
		msg := fmt.Sprintf("Driver version %s 가 verifiedVersions 화이트리스트에 없음: %v",
			desiredVersion, policy.Spec.VerifiedVersions)
		logger := logf.FromContext(ctx)
		logger.Info("verifiedVersions 검증 실패 — 업그레이드 거부",
			"node", state.Spec.NodeName,
			"desiredVersion", desiredVersion,
			"verifiedVersions", policy.Spec.VerifiedVersions)
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "UnverifiedVersion", "%s", msg)
		state.Status.DesiredVersion = desiredVersion
		return m.transitionTo(state, v1alpha1.UpgradeStateUnverifiedVersion, msg, 0)
	}

	// autoUpgrade가 false이면 이벤트만 발행하고 Idle 유지
	if policy.Spec.UpgradePolicy == nil || !policy.Spec.UpgradePolicy.AutoUpgrade {
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeAvailable",
			"드라이버 버전 불일치: current=%s desired=%s (autoUpgrade 비활성화)", currentVersion, desiredVersion)
		return true, 60 * time.Second, nil
	}

	// IdleCooldown 가드 — Idle 진입 직후 (default 10s) 연속 trigger 거부.
	// rolling-update 테스트에서 사용자가 trigger 를 빠르게 두 번 던지면 첫 사이클의
	// Uncordoning→Idle 직후 다시 Cordoning 으로 진입하여 05_verify 가 mid-state 를
	// PASS 로 오인하는 결함 (followup plan §F3) 을 차단한다.
	// IdleCooldownSeconds 가 nil 이면 default, 0/음수면 비활성화.
	cooldown := defaultIdleCooldown
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.IdleCooldownSeconds != nil {
		s := *policy.Spec.UpgradePolicy.IdleCooldownSeconds
		if s <= 0 {
			cooldown = 0
		} else {
			cooldown = time.Duration(s) * time.Second
		}
	}
	if cooldown > 0 && !state.Status.LastTransitionTime.IsZero() {
		elapsed := time.Since(state.Status.LastTransitionTime.Time)
		if elapsed < cooldown {
			remaining := cooldown - elapsed
			logger := logf.FromContext(ctx)
			logger.Info("Idle cooldown 미충족, 트리거 연기",
				"elapsed", elapsed.Round(time.Second),
				"cooldown", cooldown,
				"remaining", remaining.Round(time.Second),
				"node", state.Spec.NodeName)
			if m.Recorder != nil {
				m.Recorder.Eventf(state, corev1.EventTypeNormal, "IdleCooldownDeferred",
					"Idle 진입 후 %s 미만 (cooldown=%s) — %s 후 재시도",
					elapsed.Round(time.Second), cooldown, remaining.Round(time.Second))
			}
			// state 변경 없이 잔여 시간 후 재시도 (transitionTo 미호출 → LastTransitionTime 보존)
			return true, remaining, nil
		}
	}

	// 버전 불일치 + autoUpgrade: UpgradeRequired로 전이
	// PreviousImage는 이번 사이클에서 handleUpgrading이 DS로부터 다시 수집하도록 비움.
	// 이전 업그레이드 값이 남아있으면 잘못된 이미지로 롤백될 수 있음.
	// RollbackAttempts는 새 사이클이므로 0으로 초기화 (P2 무한 루프 방지).
	state.Status.DesiredVersion = desiredVersion
	state.Status.PreviousVersion = currentVersion
	state.Status.PreviousImage = ""
	state.Status.RollbackAttempts = 0
	resetRebootTracking(&state.Status) // 새 사이클: 재부팅 카운터·마커 초기화
	return m.transitionTo(state, v1alpha1.UpgradeStateRequired,
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
	return m.transitionTo(state, v1alpha1.UpgradeStatePreFlight, "업그레이드 슬롯 확보", 0)
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
	// 노드 커널로 사전 컴파일 이미지를 고른다. 여기서 고른 값은 handleUpgradingJob 이 install Job
	// 에 실을 이미지이며, 호출부(driver_upgrade_controller.go Reconcile 의 Status().Update)가 이번
	// 전이의 status 저장에 함께 실어 영속한다.
	resolvedImage, imageSkipReason, imageSkipEvent := resolveDriverImage(policy, kernelVersion)
	state.Status.ResolvedImage = resolvedImage
	// 정책이 이 커널용 사전 컴파일 이미지를 선언했지만 쓰지 않았다면 그 이유를 드러낸다. 기본
	// 이미지(DKMS 컴파일)로 되돌아가므로 설치는 올바른 버전으로 진행되지만, 운영자가 정책의 태그를
	// 바로잡을 수 있도록 경고 이벤트와 로그를 남긴다.
	if imageSkipReason != "" {
		logf.FromContext(ctx).Info("사전 컴파일 커널 이미지를 쓰지 않고 기본 이미지로 되돌아감",
			"kernel", kernelVersion, "reason", imageSkipReason)
		m.Recorder.Eventf(state, corev1.EventTypeWarning, imageSkipEvent, "%s", imageSkipReason)
	}
	// DIP spec 을 제3 관리자(kubectl patch/edit, ArgoCD·Flux 등)가 함께 쥐고 있으면 다음 helm
	// upgrade 가 Server-Side Apply 충돌로 멈출 수 있다 — PreFlight 마다 드러내 운영자가 helm
	// 값으로만 바꾸도록 유도한다. 기존 imageSkipReason 경고와 같은 자리·같은 빈도.
	// 제품 자신의 제어 경로(CLI·관리 REST·helm)는 foreignSpecManagers 가 이름으로 제외한다.
	// 문구는 지목된 관리자에 대한 조치만 적는다 — 제외된 이름은 %v 에 나오지 않으므로 그쪽 안내를
	// 섞으면 경고와 조치가 어긋난다. 소유 회수까지 적는 이유는 재발 방지만으로는 선 경고가 지워지지 않기 때문이다.
	if foreign := foreignSpecManagers(policy.ManagedFields); len(foreign) > 0 {
		m.Recorder.Eventf(state, corev1.EventTypeWarning, eventPolicySharedOwnership,
			"DriverInstallPolicy %s 의 spec 을 제3 관리자 %v 가 함께 소유한다 — 값은 helm 값 파일로만 바꿀 것(다음 helm upgrade 의 SSA 충돌 예방). 이미 생긴 소유 항목은 helm 이 같은 필드를 같은 값으로 다시 적용해 소유를 회수할 때까지 남고, 그동안 이 경고도 PreFlight 마다 선다",
			policy.Name, foreign)
	}
	// probe 제한시간이 Validating 전체 예산보다 길면 그 probe 는 결론을 낼 수 없다 — 바깥
	// 예산(handleValidating 의 validationTimeout)이 먼저 만료해 Rollback·Failed 로 전이하고
	// WorkloadValidator.Run 은 다시 불리지 않는다. 생성된 probe 파드는 kubelet 의
	// activeDeadlineSeconds(= 이 TimeoutSeconds)까지 남아 그동안 장치 점유자로 집계된다.
	// 거절하지 않고 경고만 낸다 — 기존 객체를 admission 에서 막으면 운영 중 정책이 갱신 불가가 된다.
	// 예산 파싱은 handleValidating 이 이미 쓰는 parseDuration 을 재사용(새 헬퍼 미추가).
	if wp := policy.Spec.WorkloadProbe; wp != nil && wp.Enabled && wp.TimeoutSeconds > 0 {
		var rawTimeout string
		if policy.Spec.UpgradePolicy != nil {
			rawTimeout = policy.Spec.UpgradePolicy.ValidationTimeout
		}
		validationTimeout := parseDuration(rawTimeout, 15*time.Minute)
		if probeTimeout := time.Duration(wp.TimeoutSeconds) * time.Second; probeTimeout > validationTimeout {
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "WorkloadProbeTimeoutExceedsBudget",
				"workloadProbe.timeoutSeconds=%ds 가 upgradePolicy.validationTimeout=%s 보다 길다 — "+
					"probe 결과가 쓰이지 않고 파드가 그 차이만큼 장치를 점유한다. 둘 중 하나를 고칠 것",
				wp.TimeoutSeconds, validationTimeout)
		}
	}
	allowlist := policy.Spec.KernelAllowlist

	if len(allowlist) > 0 {
		if !matchesKernelAllowlist(kernelVersion, allowlist) {
			msg := fmt.Sprintf("커널 버전 %s이 allowlist에 없음: %v", kernelVersion, allowlist)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "PreFlightFailed", msg)
			return m.transitionTo(state, v1alpha1.UpgradeStateRollback, msg, 0)
		}
	}

	// 노드 Ready 상태 확인
	if !isNodeReady(&node) {
		return true, 10 * time.Second, nil
	}

	return m.transitionTo(state, v1alpha1.UpgradeStateCordoning, "PreFlight 검사 통과", 0)
}

// handleCordoning: 노드를 Unschedulable로 설정 후 Draining으로 전이
func (m *UpgradeStateMachine) handleCordoning(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
) (bool, time.Duration, error) {
	if err := m.cordonNode(ctx, state.Spec.NodeName, state); err != nil {
		return false, 0, fmt.Errorf("노드 cordon 실패: %w", err)
	}

	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeCordoned",
		"업그레이드를 위해 노드 %s cordon 완료", state.Spec.NodeName)

	// architectural plan §A6.1: opt-in 라벨이 붙은 Deployment 를 노드별로 quiesce(scale=0).
	// gpu-stress 등 NodeAffinity reject 로 Failed Pod 가 누적되는 결함을 차단한다.
	// 실패 시 사이클은 계속 진행 (best-effort) — backup 이 비어 있으면 restore 도 no-op.
	if err := m.QuiesceLabeledDeployments(ctx, state); err != nil {
		logf.FromContext(ctx).Error(err, "quiesceLabeledDeployments 실패 (사이클은 계속 진행)",
			"node", state.Spec.NodeName)
	}

	return m.transitionTo(state, v1alpha1.UpgradeStateDraining, "노드 cordon 완료", 0)
}

// handleDraining: GPU/NPU 워크로드 Pod 삭제 후 Upgrading으로 전이
func (m *UpgradeStateMachine) handleDraining(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	// drainEnabled가 false면 즉시 Upgrading으로
	if policy.Spec.UpgradePolicy == nil || !policy.Spec.UpgradePolicy.DrainEnabled {
		return m.transitionTo(state, v1alpha1.UpgradeStateUpgrading, "drain 비활성화, 즉시 업그레이드", 0)
	}

	// drain timeout 체크
	drainTimeout := parseDuration(policy.Spec.UpgradePolicy.DrainTimeout, 5*time.Minute)
	if !state.Status.LastTransitionTime.IsZero() &&
		time.Since(state.Status.LastTransitionTime.Time) > drainTimeout {
		if policy.Spec.UpgradePolicy.RollbackOnFailure {
			return m.transitionTo(state, v1alpha1.UpgradeStateRollback, "drain 타임아웃: 롤백 시작", 0)
		}
		if err := m.RestoreQuiescedDeployments(ctx, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 quiesce 복구 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
		}
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(state, v1alpha1.UpgradeStateFailed, "drain 타임아웃: 수동 조치 필요", 0)
	}

	// nvidia-persistenced 종료 요청 (실제 중지는 driver-manager initContainer에서 수행)
	if strings.EqualFold(state.Spec.Vendor, "nvidia") {
		m.logPersistencedStop(ctx, state.Spec.NodeName)
	}

	// device-plugin Pod 삭제 (커널 모듈 참조 해제를 위해)
	logger := logf.FromContext(ctx)
	if err := m.deleteDevicePluginPods(ctx, state.Spec.NodeName); err != nil {
		logger.Error(err, "device-plugin Pod 삭제 실패")
		// 비치명적: 계속 진행
	}

	// 디바이스 워크로드 확인
	hasWorkloads, err := m.hasDeviceWorkloads(ctx, state.Spec.NodeName)
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

	// Pod 축이 비었어도 claim 축은 따로 본다 — DRA 는 장치를 Pod 이 아니라 ResourceClaim 이
	// 쥔다. Pod 이 사라져도 claim 이 deallocate 되기 전에는 장치가 여전히 잡혀 있고, 반대로
	// 다른 컨트롤러가 만든 claim 은 대응하는 Pod 이 이 노드에 없을 수도 있다.
	//
	// "아직 할당돼 있다" 는 Pod 축의 "아직 Pod 가 남았다" 와 같은 등급이므로 오류가 아니라 정상
	// 재조정으로 돌린다. DRA 는 kubelet NodeUnprepare → reservedFor 비움 → deallocate 순서라
	// Pod 이 사라진 뒤에도 claim 이 한동안 할당 상태로 남는 구간을 정상 drain 이 반드시 지난다.
	// 그 구간을 오류로 올리면 DRA 노드가 정상 drain 될 때마다 오류 로그와 reconcile 오류 지표가
	// 오르고, 15초 고정 주기 대신 controller-runtime 지수 backoff 를 타 drain 타임아웃 안의
	// 재시도 간격이 의도보다 길어진다. 읽기 실패(List·Node Get)만 오류로 올린다.
	//
	// 이 분기는 transitionTo 를 쓰지 않으므로 LastTransitionTime 이 보존되고, Draining 진입점이
	// 하나라 다음 재조정이 머리의 drain 타임아웃 검사를 다시 밟는다 — 끝내 비지 않으면 그 검사가
	// 롤백 또는 Failed 로 보낸다.
	if err := draclaim.AssertNoneAllocatedOnNode(ctx, m.Client, state.Spec.NodeName); err != nil {
		if !errors.Is(err, draclaim.ErrClaimStillAllocated) {
			return false, 0, fmt.Errorf("drain 완료 전 ResourceClaim 검사 실패: %w", err)
		}
		// Status.Message 는 transitionTo 만 쓰므로 Event 가 없으면 왜 멈췄는지 밖에서 보이지
		// 않는다. 재조정 주기가 15초라 Event 폭주 방지 장치는 따로 필요 없다.
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "DrainBlockedByClaim",
			"할당된 ResourceClaim 이 남아 drain 대기 중: %v", err)
		return true, 15 * time.Second, nil
	}

	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeDrained",
		"노드 %s drain 완료", state.Spec.NodeName)
	return m.transitionTo(state, v1alpha1.UpgradeStateUpgrading, "drain 완료", 0)
}

// handleUpgrading: DaemonSet 이미지 업데이트 후 Validating으로 전이 (requeue 20s)
func (m *UpgradeStateMachine) handleUpgrading(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	// Job 모드는 install Job 생성/재생성 경로로 분기(daemonset 본문 무변경).
	if isJobMode(policy) {
		return m.handleUpgradingJob(ctx, state, policy)
	}

	// DaemonSet 이름 결정 (driver_daemonset_controller.go line 98 패턴과 일치)
	dsName := naming.DriverDSName(state.Spec.Vendor, state.Spec.Model)

	var ds appsv1.DaemonSet
	if err := m.Get(ctx, types.NamespacedName{Name: dsName, Namespace: naming.OperatorNamespace()}, &ds); err != nil {
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
		// 롤백 시 빌드 접미사 포함 원본 이미지를 복구하기 위해 패치 직전에 기록.
		// architectural plan §3 (defense-in-depth): broken plain tag (예: ":580.126.09") 이
		// PreviousImage 로 캡처되어 rollback 시 broken Pod 으로 회귀하는 것을 차단.
		// isVerifiedBuildTag 가 true 인 (-v<N> 접미사 또는 검증된) 태그만 저장.
		if state.Status.PreviousImage == "" && prevImage != "" && prevImage != desiredImage {
			if isVerifiedBuildTag(prevImage) {
				state.Status.PreviousImage = prevImage
			} else {
				logger := logf.FromContext(ctx)
				logger.Info("plain tag 감지 — PreviousImage 저장 skip (broken image rollback 차단)",
					"prevImage", prevImage, "node", state.Spec.NodeName)
				m.Recorder.Eventf(state, corev1.EventTypeWarning, "PreviousImagePlainTagSkipped",
					"plain tag (%s) 감지: PreviousImage 저장을 skip 했습니다 (rollback 시 broken image 회귀 방지). "+
						"DIP.spec.driver.image 를 -v<N> 접미사 빌드 태그로 변경하세요.", prevImage)
			}
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

	return m.transitionTo(state, v1alpha1.UpgradeStateValidating, "DaemonSet 이미지 업데이트 완료", 20*time.Second)
}

// validatorsFor 는 정책에 맞는 검증 체인이다. 기본은 DriverModule·DevicePlugin 둘이고, 정책이
// workloadProbe 를 켜면 Workload 를 끝에 더한다(정책 opt-in). 두 실행 경로(DS·Job, 아래
// handleValidating·state_machine_job.go 의 handleValidatingJob)가 같은 체인을 쓴다.
//  1. DriverModule  : 노드의 드라이버 커널 모듈 로드 (NDR.driverVersion 매칭)
//  2. DevicePlugin  : kube-system 의 device-plugin Pod ContainersReady
//  3. Workload      : (opt-in) 샘플 워크로드 probe 파드의 종료 코드
func validatorsFor(policy *v1alpha1.DriverInstallPolicy) []validator.Validator {
	chain := []validator.Validator{&validator.DriverModuleValidator{}, &validator.DevicePluginValidator{}}
	if policy != nil && policy.Spec.WorkloadProbe != nil && policy.Spec.WorkloadProbe.Enabled {
		chain = append(chain, &validator.WorkloadValidator{Probe: *policy.Spec.WorkloadProbe, Namespace: naming.OperatorNamespace()})
	}
	return chain
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
	// Job 모드는 "Job 성공 종료 + NDR 일치" 검증 경로로 분기(daemonset 본문 무변경).
	if isJobMode(policy) {
		return m.handleValidatingJob(ctx, state, policy)
	}

	logger := logf.FromContext(ctx)

	// architectural plan §4.4 옵션 A: Validating 진입 시점에 detector 차단 라벨만 제거.
	// driver-upgrading 라벨은 사이클 추적용으로 보존 (Uncordoning 에서 함께 제거).
	// detector 재spawn → NDR 갱신 → DriverModuleValidator 통과를 가능케 함.
	if err := m.EnsureUpgradingBlockingLabelRemoved(ctx, state.Spec.NodeName); err != nil {
		logger.Error(err, "Validating 진입 시 blocking 라벨 제거 실패", "node", state.Spec.NodeName)
	} else {
		m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeBlockingLabelRemoved",
			"노드 %s 의 driver-upgrading-blocking 라벨 제거 (Validating 진입)", state.Spec.NodeName)
	}

	// ─── 1. 전체 validation timeout 체크 ─────────────────────
	validationTimeout := parseDuration("", 15*time.Minute)
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.ValidationTimeout != "" {
		validationTimeout = parseDuration(policy.Spec.UpgradePolicy.ValidationTimeout, 15*time.Minute)
	}
	if !state.Status.LastTransitionTime.IsZero() &&
		time.Since(state.Status.LastTransitionTime.Time) > validationTimeout {
		if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.RollbackOnFailure {
			return m.transitionTo(state, v1alpha1.UpgradeStateRollback, "검증 타임아웃: 롤백 시작", 0)
		}
		if err := m.RestoreQuiescedDeployments(ctx, state); err != nil {
			logger.Error(err, "Failed 전이 중 quiesce 복구 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
		}
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logger.Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(state, v1alpha1.UpgradeStateFailed, "검증 타임아웃: 수동 조치 필요", 0)
	}

	// ─── 2. CrashLoop 즉시 감지 (재시도 무의미한 hard failure) ───
	// driver-installer Pod 가 CrashLoopBackOff 라면 어떤 validator 도 통과 못 하므로
	// 빠르게 Rollback / Failed 로 전이한다 (legacy 동작 보존).
	dsName := naming.DriverDSName(state.Spec.Vendor, state.Spec.Model)
	desiredImage := policy.Spec.Driver.Image
	ready, crashLoop, err := m.isDriverPodReadyOnNode(ctx, dsName, state.Spec.NodeName, desiredImage)
	if err != nil {
		return false, 0, fmt.Errorf("드라이버 Pod 상태 확인 실패: %w", err)
	}
	if crashLoop {
		if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.RollbackOnFailure {
			return m.transitionTo(state, v1alpha1.UpgradeStateRollback, "드라이버 Pod CrashLoopBackOff: 롤백 시작", 0)
		}
		if err := m.RestoreQuiescedDeployments(ctx, state); err != nil {
			logger.Error(err, "Failed 전이 중 quiesce 복구 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
		}
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logger.Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(state, v1alpha1.UpgradeStateFailed, "드라이버 Pod CrashLoopBackOff: 수동 조치 필요", 0)
	}

	// 새 Pod 이 아직 준비되지 않은 경우 (wrong-image 캐시 / NotReady 등) 단순 requeue.
	// ValidationTimeout 안에 안정되지 않으면 architectural plan §1 의 timeout 가드가 Rollback / Failed 로 전이한다.
	if !ready {
		logger.Info("Validating: 새 Pod 미준비 (이미지 불일치 또는 NotReady)",
			"node", state.Spec.NodeName)
		return true, 10 * time.Second, nil
	}

	// ─── 3. validator 체인 순차 실행 ─────────────────────────
	desiredVersion := state.Status.DesiredVersion
	for _, v := range validatorsFor(policy) {
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
	return m.transitionTo(state, v1alpha1.UpgradeStateUncordoning, "검증 성공", 0)
}

// handleUncordoning: 노드를 Schedulable로 복구 후 Idle로 전이
func (m *UpgradeStateMachine) handleUncordoning(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
) (bool, time.Duration, error) {
	if err := m.uncordonNode(ctx, state.Spec.NodeName, state); err != nil {
		return false, 0, fmt.Errorf("노드 uncordon 실패: %w", err)
	}

	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeUncordoned",
		"노드 %s uncordon 완료, 업그레이드 성공", state.Spec.NodeName)

	// architectural plan §A6.1: 사이클 정상 종료 시 quiesce 된 Deployment 복구.
	// uncordon 후 Idle 진입 직전이 가장 안전한 시점 (노드 schedulable + 새 driver 준비 완료).
	if err := m.RestoreQuiescedDeployments(ctx, state); err != nil {
		logf.FromContext(ctx).Error(err, "restoreQuiescedDeployments 실패 (수동 조치 필요)",
			"node", state.Spec.NodeName)
	}

	metrics.RecordUpgradeComplete(state.Spec.Vendor, "success")

	return m.transitionTo(state, v1alpha1.UpgradeStateIdle, "업그레이드 완료", 0)
}

// handleFailed 는 터미널 Failed 상태를 처리한다. 자동 복구(상태 전이 / DS image patch /
// install Job 재생성)는 여전히 하지 않는다 — rollback infinite-loop 방지(plan §R1)는 그대로다.
// 여기서 하는 일은 단 하나, **이 사이클이 걸어 둔 cordon 을 조건이 맞으면 푸는 것**이다.
// 상태는 Failed 로 남고 사람의 수동 조치는 여전히 필요하다.
//
// 왜 푸는가 — 라이브 실측(2026-08-05):
// worker1 의 Furiosa 드라이버가 사라지자 DUS 가 Draining("노드 cordon 완료") → Failed("롤백할
// 이전 버전 없음(job)") 로 착지했고, cordon 이 남은 채 방치됐다. cordon 된 노드는 BuildSnapshot
// 이 후보에서 통째로 뺀다. 그래서 같은 노드에서 멀쩡히 돌던 NVIDIA GPU 2장까지 배치 불가가
// 되고 NVIDIA 워크로드가 NoCandidateNodes 로 거절됐다 — 한 벤더의 드라이버 소실이 그 노드의
// 모든 벤더를 인질로 잡는다. cordon 은 노드 단위 도구라 "이 벤더만 못 쓴다" 를 표현하지 못한다.
// 실패한 벤더의 배제는 광고(extended resource)가 이미 담당한다.
//
// 왜 무조건 풀지 않는가: 같은 실측에서 device-plugin 이 드라이버 소실과 함께 광고를 0 으로
// 내렸지만 그것은 **관측 하나이지 보장이 아니다**. 드라이버 없이도 광고를 유지하는 플러그인이
// 있다면 cordon 을 푸는 순간 드라이버 없는 장치 위로 워크로드가 떨어진다. 그래서 해제 직전에
// 그 벤더의 광고가 실제 0 인지 확인하고, 확인하지 못하면 cordon 을 유지한다. 가드가 해제
// 자체보다 중요하다.
func (m *UpgradeStateMachine) handleFailed(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
) (bool, time.Duration, error) {
	logger := logf.FromContext(ctx).WithValues("node", state.Spec.NodeName, "vendor", state.Spec.Vendor)

	// 탈출 (2026-08-07 라이브): 노드가 이미 목표를 만족하고 있으면 Failed 로 붙잡아 둘 이유가
	// 없다. `.91` k8s-worker3-tenstorrent 가 current=desired=2.8.0 에 드라이버도 로드된 채로
	// 사흘을 Failed 로 남아 있었다 — 실패를 만든 조건이 이미 사라졌는데 상태에 나가는 길이
	// 없었기 때문이다. 사람이 손댈 것이 없는 상태에서 "수동 조치 필요" 를 말하는 것은 상태를
	// 잘못 표현하는 것이고, 그 노드는 운영자의 확인 목록에 영원히 남는다.
	//
	// 왜 이 조건에서만 여는가: 여기서 Idle 로 보내면 handleIdle 이 다시 판정한다. 버전이 같고
	// 드라이버가 로드돼 있으면 handleIdle 은 아무것도 하지 않는다(Job 모드 self-heal 은
	// driverLoaded=false 로만 발동하고, 버전 일치 분기는 Idle 을 유지한다). 그래서 왕복이
	// 생기지 않는다. 조건을 넓히면 — 예컨대 버전만 보고 열면 — Idle 이 설치를 다시 걸고 같은
	// 이유로 실패해 Failed 로 돌아오는 왕복이 생기고, 그 왕복은 고착보다 나쁘다(노드를 계속
	// 건드린다).
	if recovered, err := m.failedStateRecovered(ctx, state); err != nil {
		return false, 0, err
	} else if recovered {
		m.Recorder.Eventf(state, corev1.EventTypeNormal, "FailedStateRecovered",
			"노드가 목표 버전 %s 를 이미 만족하고 드라이버도 로드됨 — Failed 해제 후 Idle 복귀",
			state.Status.DesiredVersion)
		// 지난 사이클의 재시도 카운터를 물려주지 않는다. 남겨 두면 다음 실패가 첫 시도부터
		// 한도에 걸린다.
		state.Status.RollbackAttempts = 0
		resetRebootTracking(&state.Status)

		// 우리가 건 cordon 이 있으면 푼다. ACPP quiesce(internal/partition/quiesce.go Cordon) 와
		// 운영자의 수동 cordon 은 표식을 남기지 않으므로, 여기서 직접 소유권을 확인해야 한다.
		// 표식이 없거나 남의 이름이면 그 cordon 은 남겨 둔다(파티션 적용 중 / 정비 중).
		var node corev1.Node
		if err := m.Get(ctx, types.NamespacedName{Name: state.Spec.NodeName}, &node); err == nil {
			if err := m.reapStaleCordonClaim(ctx, &node, state); err != nil {
				return false, 0, fmt.Errorf("stale cordon claim 정리 실패: %w", err)
			}
			if node.Spec.Unschedulable && node.Annotations[cordonOwnerAnnotationKey] == state.Name {
				if err := m.uncordonNode(ctx, state.Spec.NodeName, state); err != nil {
					return false, 0, fmt.Errorf("복구 탈출 중 노드 uncordon 실패: %w", err)
				}
			}
		}

		return m.transitionTo(state, v1alpha1.UpgradeStateIdle,
			fmt.Sprintf("복구 확인(%s): 목표 버전 도달·드라이버 로드", state.Status.DesiredVersion),
			60*time.Second)
	}

	var node corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: state.Spec.NodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			// 노드가 없으면 풀 cordon 도 없다(노드 삭제 등) — 터미널 그대로 정지.
			return false, 0, nil
		}
		return false, 0, err
	}

	if err := m.reapStaleCordonClaim(ctx, &node, state); err != nil {
		return false, 0, fmt.Errorf("stale cordon claim 정리 실패: %w", err)
	}

	// 해제 조건 (a) cordon 이 실제로 남아 있고 (b) 그 cordon 의 주인이 이 DUS 여야 한다.
	// 주인 판정은 cordonNode 가 노드를 직접 false→true 로 뒤집었을 때만 남긴 annotation 으로
	// 한다. ACPP quiesce(internal/partition/quiesce.go Cordon)도 운영자의 수동 cordon 도
	// 아무 표식을 남기지 않으므로, 표식이 없거나 남의 이름이면 그 cordon 은 우리 것이 아니다.
	// 남의 cordon 을 우리가 풀면 그 주체의 의도(파티션 적용 중 / 정비 중)를 지우게 된다.
	if !node.Spec.Unschedulable || node.Annotations[cordonOwnerAnnotationKey] != state.Name {
		logger.Info("Failed 터미널 상태 — 해제할 cordon 없음(미소유 또는 이미 해제), 추가 transition 없이 즉시 반환")
		return false, 0, nil
	}

	// 해제 조건 (c) 그 벤더의 광고가 실제 0.
	stillAdvertised, decidable := vendorStillAdvertised(&node, state.Spec.Vendor)
	if !decidable || stillAdvertised != "" {
		// 유지. 보류이지 포기가 아니므로 다시 볼 기회를 남긴다 — 광고는 device-plugin 이
		// 뒤늦게 재시작하며 떨어질 수 있다. 여기서 반환하는 requeue 는 상태 전이도 DS 패치도
		// Job 생성도 하지 않으므로 plan §R1 의 무한 롤백 방지와 충돌하지 않는다(같은 switch 의
		// UnverifiedVersion 이 이미 같은 방식으로 터미널 상태를 재확인한다).
		logger.Info("Failed 터미널 상태 — cordon 유지(벤더 광고 0 미확인)",
			"advertisedResource", stillAdvertised, "vendorKnown", decidable)
		return true, failedCordonRecheckInterval, nil
	}

	// 해제. 소유권 표식도 같은 patch 로 지운다 — 표식이 남으면 사람이 다시 cordon 했을 때
	// 우리가 또 푼다.
	base := node.DeepCopy()
	node.Spec.Unschedulable = false
	delete(node.Annotations, cordonOwnerAnnotationKey)
	if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return false, 0, fmt.Errorf("터미널 Failed 상태의 cordon 해제 실패: %w", err)
	}

	// 관측 가능성: 왜 풀렸는지를 event 와 status message 양쪽에 남긴다. 원래 실패 사유를
	// 덮지 않고 덧붙인다 — 사람이 봐야 하는 것은 "왜 실패했고 왜 cordon 은 풀렸나" 둘 다다.
	// 이 경로는 소유권 표식을 지우며 딱 한 번만 지나므로 message 가 중복 누적되지 않는다.
	m.Recorder.Eventf(state, corev1.EventTypeWarning, "FailedCordonReleased",
		"드라이버 복구 실패(수동 조치 필요)가 지속되나 벤더 %s 의 장치 광고가 0 이므로 노드 %s cordon 해제 — "+
			"같은 노드의 다른 벤더 장치 배치를 복구한다", state.Spec.Vendor, state.Spec.NodeName)
	state.Status.Message = fmt.Sprintf("%s / cordon 해제됨(%s 광고 0, 다른 벤더 배치 복구)",
		state.Status.Message, state.Spec.Vendor)
	logger.Info("Failed 터미널 상태 — cordon 해제 완료(벤더 광고 0)")
	return false, 0, nil
}

// failedStateRecovered 는 Failed 로 남은 DUS 의 노드가 이미 정책을 만족하는지 본다.
// 만족의 정의는 둘 다여야 한다: (a) 목표 버전이 지정돼 있고 현재 버전과 같다,
// (b) 그 벤더 장치의 드라이버가 실제로 로드돼 있다.
//
// (b) 를 함께 보는 이유: 버전 문자열은 마지막 관측의 잔상일 수 있다. 모듈이 빠진 노드를
// 버전만 보고 "만족" 으로 읽으면 Idle 로 보냈다가 self-heal 이 설치를 걸고, 그 설치가 원래
// 실패하던 이유로 다시 실패해 Failed 로 돌아온다. 근거를 하나 더 요구해 그 왕복을 막는다.
//
// NDR 이 없으면 (b) 를 확인할 수 없으므로 false 다 — 확인하지 못한 것을 만족으로 읽지 않는다.
func (m *UpgradeStateMachine) failedStateRecovered(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
) (bool, error) {
	desired := state.Status.DesiredVersion
	if desired == "" || desired != state.Status.CurrentVersion {
		return false, nil
	}
	var ndr v1alpha1.NodeDeviceReport
	if err := m.Get(ctx, types.NamespacedName{Name: state.Spec.NodeName}, &ndr); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	notLoaded, err := m.driverNotLoaded(ctx, state.Spec.NodeName, state.Spec.Vendor, state.Spec.Model)
	if err != nil {
		return false, err
	}
	return !notLoaded, nil
}

// vendorStillAdvertised 는 노드가 그 벤더의 extended resource 를 아직 광고 중인지 본다.
// 반환값은 (0 이 아닌 리소스명, 판정 가능 여부)이며 ("", true) 만이 "광고가 0 임을 확인했다"
// 는 뜻이다.
//
// 카탈로그(internal/intent)가 모르는 벤더는 어떤 리소스명을 봐야 하는지 알 수 없으므로
// decidable=false 를 돌린다 — 찾은 게 없는 것을 0 으로 읽으면 안 된다.
//
// 카탈로그의 고정 이름과 대조하지 않고 allocatable 을 훑어 VendorForResource 로 되짚는 이유:
// NVIDIA mixed MIG 는 프로파일마다 nvidia.com/mig-<profile> 이라는 동적 리소스명으로 광고해서
// nvidia.com/gpu 하나만 보면 MIG 장치가 살아 있는데도 0 으로 읽힌다. furiosa 처럼 한 벤더
// 아래 제품이 둘(rngd/warboy)인 경우도 이 방식이면 product 를 몰라도 둘 다 걸린다 — cordon 은
// 벤더 단위 DUS 가 걸었으므로 해제도 그 벤더 전체가 0 일 때만 해야 한다.
func vendorStillAdvertised(node *corev1.Node, vendor string) (string, bool) {
	if !intent.KnownVendor(vendor) {
		return "", false
	}
	for name, q := range node.Status.Allocatable {
		if !strings.EqualFold(intent.VendorForResource(string(name)), vendor) {
			continue
		}
		if !q.IsZero() {
			return string(name), true
		}
	}
	return "", true
}

// handleRollback: 이전 버전으로 DaemonSet 복구 후 Upgrading으로 전이
func (m *UpgradeStateMachine) handleRollback(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) (bool, time.Duration, error) {
	// Job 모드는 "이전 버전 install Job 재실행" 경로로 분기(daemonset 본문 무변경).
	if isJobMode(policy) {
		return m.handleRollbackJob(ctx, state, policy)
	}

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
		if err := m.RestoreQuiescedDeployments(ctx, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 quiesce 복구 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
		}
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패: node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(state, v1alpha1.UpgradeStateFailed,
			fmt.Sprintf("롤백 %d회 초과: 수동 조치 필요", maxRollbacks), 0)
	}

	prevVersion := state.Status.PreviousVersion
	if prevVersion == "" {
		// 이전 버전 없음: Failed 처리
		metrics.RecordUpgradeComplete(state.Spec.Vendor, "failure")
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RollbackFailed",
			"롤백할 이전 버전 없음: 수동 조치 필요")
		if err := m.RestoreQuiescedDeployments(ctx, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 quiesce 복구 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
		}
		if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
			logf.FromContext(ctx).Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
				"Failed 전이 중 라벨 제거 실패 (수동 조치 필요): node=%s err=%v", state.Spec.NodeName, err)
		}
		return m.transitionTo(state, v1alpha1.UpgradeStateFailed, "이전 버전 없음: 수동 조치 필요", 0)
	}

	dsName := naming.DriverDSName(state.Spec.Vendor, state.Spec.Model)

	var ds appsv1.DaemonSet
	if err := m.Get(ctx, types.NamespacedName{Name: dsName, Namespace: naming.OperatorNamespace()}, &ds); err != nil {
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
			if err := m.RestoreQuiescedDeployments(ctx, state); err != nil {
				logf.FromContext(ctx).Error(err, "Failed 전이 중 quiesce 복구 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
			}
			if err := m.clearUpgradingLabel(ctx, state.Spec.NodeName, state); err != nil {
				logf.FromContext(ctx).Error(err, "Failed 전이 중 라벨 제거 실패 (수동 조치 필요)", "node", state.Spec.NodeName)
				m.Recorder.Eventf(state, corev1.EventTypeWarning, "UpgradeLabelCleanupFailed",
					"Failed 전이 중 라벨 제거 실패: node=%s err=%v", state.Spec.NodeName, err)
			}
			return m.transitionTo(state, v1alpha1.UpgradeStateFailed,
				"RollbackTarget=previousValidated: PreviousImage 미보유로 안전 롤백 불가", 0)
		}
		// PreviousImage 미보유 + RollbackTarget != previousValidated:
		// DIP.spec.driver.image 의 variant 접미사를 추출해 prevVersion 과 결합한 정상 image 를 구성한다.
		// 이렇게 하면 legacy plain-tag 폴백 (예: ":580.142") 으로 ImagePullBackOff 가 발생하는 것을 방지.
		if dipImage := policy.Spec.Driver.Image; dipImage != "" {
			if variant := extractImageVariantSuffix(dipImage); variant != "" {
				if idx := strings.LastIndex(dipImage, ":"); idx >= 0 {
					prevImage = dipImage[:idx+1] + prevVersion + variant
					logf.FromContext(ctx).Info("PreviousImage 미보유 — DIP image variant 로 폴백 구성",
						"previousImage", prevImage, "variant", variant, "dipImage", dipImage)
				}
			}
		}
	}
	base := ds.DeepCopy()
	if len(ds.Spec.Template.Spec.Containers) > 0 {
		i := 0
		if prevImage != "" {
			ds.Spec.Template.Spec.Containers[i].Image = prevImage
		} else {
			img := ds.Spec.Template.Spec.Containers[i].Image
			ds.Spec.Template.Spec.Containers[i].Image = replaceImageTag(img, prevVersion)
		}
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

	return m.transitionTo(state, v1alpha1.UpgradeStateValidating, fmt.Sprintf("롤백 시작: %s", prevVersion), 20*time.Second)
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

// clearRollbackRequest 는 롤백 요청 어노테이션을 metadata patch 로 지운다(status 갱신과 별개).
func (m *UpgradeStateMachine) clearRollbackRequest(ctx context.Context, state *v1alpha1.DriverUpgradeState) error {
	patch := client.MergeFrom(state.DeepCopy())
	delete(state.Annotations, v1alpha1.RollbackRequestedAnnotation)
	if err := m.Patch(ctx, state, patch); err != nil {
		return fmt.Errorf("롤백 요청 어노테이션 제거 실패: %w", err)
	}
	return nil
}

// transitionTo는 상태를 변경하고 requeue 정보를 반환합니다.
// requeueAfter=0이면 즉시 requeue (Requeue=true).
// 상태 전이 시 Prometheus 메트릭을 기록합니다.
func (m *UpgradeStateMachine) transitionTo(
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
// ─────────────────────────────────────────────
// cross-major reboot 자동화 (RebootRequired / Rebooting)
// ─────────────────────────────────────────────

// nodeNeedsReboot 는 NDR.status.devices[vendor/model].needsReboot 를 읽는다(driverNotLoaded 와 동일 패턴).
// detector(node-agent)가 마커/버전불일치로 산출하며, operator 는 읽기만 한다(트리거/오케스트레이션 분리).
func (m *UpgradeStateMachine) nodeNeedsReboot(ctx context.Context, nodeName, vendor, model string) (bool, error) {
	var ndr v1alpha1.NodeDeviceReport
	if err := m.Get(ctx, types.NamespacedName{Name: nodeName}, &ndr); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, d := range ndr.Status.Devices {
		if !strings.EqualFold(d.Vendor, vendor) {
			continue
		}
		if !DeviceModelMatches(d.Model, model) {
			continue
		}
		if d.NeedsReboot {
			return true, nil
		}
	}
	return false, nil
}

// rebootMarkerIsFresh 는 needsReboot 관측을 이번 사이클의 재부팅 근거로 써도 되는지 판정한다.
//
// 기준은 "관측이 이번 install Job 완료보다 나중인가" 다. install Job 이 끝난 뒤에 남은 마커는
// 그 Job 이 방금 쓴 것이므로 진짜이고, 완료 이전 관측은 소거 전 상태를 보고 있는 stale 이다.
//
// 왜 시도 횟수로 가르지 않는가 — 원래 규율은 RebootAttempts==0 이었다. 그러면 "재부팅 후에
// 다시 돈 install Job 이 새로 남긴 마커" 를 표현할 방법이 없다. 라이브 실측(2026-08-07 `.91`
// k8s-worker1): 05:43 에 시작한 580.173.02 install Job 이 05:49 에 마커를 남기고 정상 종료했는데
// 앞선 재부팅이 예산을 이미 써버려 상태기계가 Validating 에서 나가지 못했고, 사람이 노드를
// 재부팅할 때까지 9분을 멈춰 있었다. 나가는 길이 없는 상태였다.
//
// 근거를 확보하지 못하면(NDR 에 observedAt 없음 / Job 없음 / 완료시각 없음 — daemonset 모드가
// 여기 해당) 기존 규율로 후퇴한다. 재부팅 예산 자체는 여기서 정하지 않는다 —
// handleRebootRequired 의 MaxReboots 한 곳이 갖는다.
func (m *UpgradeStateMachine) rebootMarkerIsFresh(ctx context.Context, state *v1alpha1.DriverUpgradeState) bool {
	firstReboot := state.Status.RebootAttempts == 0

	var ndr v1alpha1.NodeDeviceReport
	if err := m.Get(ctx, types.NamespacedName{Name: state.Spec.NodeName}, &ndr); err != nil || ndr.Status.ObservedAt == nil {
		return firstReboot
	}
	jobName := naming.InstallJobName(state.Spec.Vendor, state.Spec.Model, state.Spec.NodeName)
	var job batchv1.Job
	if err := m.Get(ctx, types.NamespacedName{Name: jobName, Namespace: driverjob.Namespace}, &job); err != nil ||
		job.Status.CompletionTime == nil {
		return firstReboot
	}
	// 첫 재부팅은 기존 동작을 그대로 둔다(회귀 0). 관측이 설치 완료보다 나중이면 그 뒤로도 연다.
	return firstReboot || ndr.Status.ObservedAt.After(job.Status.CompletionTime.Time)
}

// maxReboots 는 정책의 MaxReboots(기본 1)를 반환한다.
func maxReboots(policy *v1alpha1.DriverInstallPolicy) int32 {
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.MaxReboots > 0 {
		return policy.Spec.UpgradePolicy.MaxReboots
	}
	return 1
}

// handleRebootRequired 는 안전 게이트 통과 후 재부팅을 트리거하고 Rebooting 으로 전이한다.
// cordon/drain 은 이미 선행 완료(멱등). 무한 재부팅은 MaxReboots 로 차단.
func (m *UpgradeStateMachine) handleRebootRequired(ctx context.Context, state *v1alpha1.DriverUpgradeState, policy *v1alpha1.DriverInstallPolicy) (bool, time.Duration, error) {
	logger := logf.FromContext(ctx)

	// 게이트: needsReboot 재확인(NDR stale 로 인한 불필요 재부팅 차단).
	need, err := m.nodeNeedsReboot(ctx, state.Spec.NodeName, state.Spec.Vendor, state.Spec.Model)
	if err != nil {
		return false, 0, err
	}
	if !need {
		return m.transitionTo(state, v1alpha1.UpgradeStateValidating, "needsReboot 해소 — 검증으로 복귀", 0)
	}

	// kured executor 는 1차 미구현(계획 §5, 수요 시). 기본 Job 만 실행.
	if policy.Spec.RebootExecutor == "kured" {
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RebootExecutorUnsupported",
			"kured executor 는 1차 미구현(후속 과제) — DIP.driver.rebootExecutor=Job 사용")
		return true, 60 * time.Second, nil
	}

	// 무한 재부팅 방지: 증가 후 초과 시 Failed.
	state.Status.RebootAttempts++
	state.Status.RebootRequestedTime = metav1.Now()
	if state.Status.RebootAttempts > maxReboots(policy) {
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RebootLimitExceeded",
			"재부팅 %d회 후에도 needsReboot 지속 — Failed(수동 조치)", state.Status.RebootAttempts-1)
		return m.transitionTo(state, v1alpha1.UpgradeStateFailed, "재부팅 한도 초과: 수동 조치 필요", 0)
	}

	// P2 premature-Ready 가드: reboot Job 생성 **이전에** 현재 node.status.nodeInfo.bootID 를
	// DUS status 에 persist 한다(persist-then-Job). 재부팅으로 bootID 가 바뀌므로 handleRebooting 은
	// 이 값과의 차이로 "실제 재부팅 완료"를 판정한다(kubelet 이 재부팅 직전 잠깐 Ready 를 유지하는 레이스 차단).
	// 캡처 실패 시 빈 값을 저장 → handleRebooting 이 기존 isNodeReady 로 보수 전진(교착 방지 fallback).
	var rbNode corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: state.Spec.NodeName}, &rbNode); err == nil {
		state.Status.RebootBootID = rbNode.Status.NodeInfo.BootID
	} else {
		logger.Error(err, "bootID 캡처 실패 — 빈 값 fallback(isNodeReady 보수 전진)", "node", state.Spec.NodeName)
		state.Status.RebootBootID = ""
	}
	// persist-then-Job: Job 이 노드를 재부팅하기 전에 bootID 를 반드시 영구 저장.
	// 실패 시 Job 을 만들지 않고 에러 반환 → outer Reconcile 재시도(RebootAttempts 증가분도 미persist 되어 재진입 시 재보정).
	if err := m.Status().Update(ctx, state); err != nil {
		return false, 0, fmt.Errorf("bootID persist 실패(재부팅 전 필수): %w", err)
	}

	// reboot Job 생성(결정론적 이름=de-facto lease, 중복 무해).
	reboot := driverjob.RenderRebootJob(policy, state.Spec.NodeName, policy.Spec.Driver.Image)
	if err := m.Create(ctx, reboot); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, 0, fmt.Errorf("reboot Job 생성 실패: %w", err)
	}
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "RebootTriggered",
		"노드 %s 재부팅 트리거 (attempt %d/%d)", state.Spec.NodeName, state.Status.RebootAttempts, maxReboots(policy))
	logger.Info("노드 재부팅 트리거", "node", state.Spec.NodeName, "attempt", state.Status.RebootAttempts)
	return m.transitionTo(state, v1alpha1.UpgradeStateRebooting, "재부팅 트리거 완료", 30*time.Second)
}

// handleRebooting 은 재부팅 후 노드 Ready 복귀를 관측해 Upgrading 으로 재진입한다(재설치로 모듈 재로드·
// host needs-reboot 마커 소거 → 이후 Validating 재수렴). rebootTimeout 초과 시 Failed.
// 성공 판정은 Job 결과가 아니라 노드 상태(재부팅이 pod 을 죽이므로).
func (m *UpgradeStateMachine) handleRebooting(ctx context.Context, state *v1alpha1.DriverUpgradeState, policy *v1alpha1.DriverInstallPolicy) (bool, time.Duration, error) {
	logger := logf.FromContext(ctx)

	timeout := parseDuration("", 15*time.Minute)
	if policy.Spec.UpgradePolicy != nil && policy.Spec.UpgradePolicy.RebootTimeout != "" {
		timeout = parseDuration(policy.Spec.UpgradePolicy.RebootTimeout, 15*time.Minute)
	}
	if !state.Status.RebootRequestedTime.IsZero() && time.Since(state.Status.RebootRequestedTime.Time) > timeout {
		m.Recorder.Eventf(state, corev1.EventTypeWarning, "RebootTimeout",
			"재부팅 후 %s 내 Ready+모듈교체 미완 — Failed", timeout)
		return m.transitionTo(state, v1alpha1.UpgradeStateFailed, "재부팅 타임아웃: 수동 조치 필요", 0)
	}

	var node corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: state.Spec.NodeName}, &node); err != nil {
		return true, 30 * time.Second, nil // 재부팅 중 API 접근 불가 가능 → 재시도
	}
	if !isNodeReady(&node) {
		return true, 15 * time.Second, nil // 재부팅 중(NotReady) → 대기
	}
	// P2 premature-Ready 가드: bootID 가 재부팅 직전 캡처값과 아직 같으면 실제 재부팅 전(kubelet 이
	// 잠깐 Ready 를 유지하는 레이스) → 전진 보류. bootID 캡처 실패(빈 값)면 기존 isNodeReady 로 보수 전진.
	if state.Status.RebootBootID != "" && node.Status.NodeInfo.BootID == state.Status.RebootBootID {
		logger.Info("노드 Ready 이나 bootID 미변경 — 재부팅 미완료로 판단, 대기",
			"node", state.Spec.NodeName, "bootID", state.Status.RebootBootID)
		return true, 15 * time.Second, nil
	}
	// Ready 복귀 — 재부팅으로 신 모듈이 로드됐다. 구 install Job 을 삭제해 재실행을 강제하고 Upgrading 재진입:
	// 재실행 install Job(entrypoint)이 (a) 모듈 재로드 확인 (b) major 일치 시 host needs-reboot 마커 소거(WP-R2)
	// 를 수행한다 → 이후 Validating 이 needsReboot=false 로 정상 검증. (재부팅만으로는 host 마커를 지울 주체가
	// install Job 재실행뿐이므로 Validating 직행 시 needsReboot 잔존→RebootRequired 재진입→MaxReboots→Failed 가 된다.)
	jobName := naming.InstallJobName(state.Spec.Vendor, state.Spec.Model, state.Spec.NodeName)
	oldJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: driverjob.Namespace}}
	if err := client.IgnoreNotFound(m.Delete(ctx, oldJob, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
		logger.Error(err, "구 install Job 삭제 실패(재실행 강제) — 계속", "job", jobName)
	}
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "RebootSucceeded",
		"노드 %s 재부팅 후 Ready — 드라이버 재설치로 모듈 재로드·마커 소거", state.Spec.NodeName)
	logger.Info("재부팅 완료 — Upgrading 재진입(재설치로 마커 소거·재수렴)", "node", state.Spec.NodeName)
	return m.transitionTo(state, v1alpha1.UpgradeStateUpgrading, "재부팅 후 Ready — 재설치로 재수렴", 0)
}

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
	case v1alpha1.UpgradeStateRebootRequired:
		return "rebootrequired"
	case v1alpha1.UpgradeStateRebooting:
		return "rebooting"
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
		v1alpha1.UpgradeStateRebootRequired,
		v1alpha1.UpgradeStateRebooting,
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
		// 소유권 표식은 우리가 직접 false→true 로 뒤집었을 때만 남긴다. 이미 unschedulable 인
		// 노드는 주인이 따로 있고(ACPP quiesce / 운영자 수동), 거기에 우리 이름을 달면
		// handleFailed 가 나중에 남의 cordon 을 우리 것으로 알고 풀어 버린다.
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[cordonOwnerAnnotationKey] = state.Name
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if node.Labels[driverUpgradingLabelKey] != labelValueTrue {
		node.Labels[driverUpgradingLabelKey] = labelValueTrue
		changed = true
	}
	// 좁은 lifecycle 의 차단 라벨도 같이 추가. Validating 진입 시점에 제거되어
	// detector 가 다시 spawn 가능하게 된다.
	if node.Labels[driverUpgradingBlockingLabelKey] != labelValueTrue {
		node.Labels[driverUpgradingBlockingLabelKey] = labelValueTrue
		changed = true
	}
	if !changed {
		return nil
	}
	if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return err
	}
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeUpgradeLabelApplied",
		"노드 %s에 npu.ai/driver-upgrading 및 driver-upgrading-blocking 라벨 추가 (device-plugin 스케줄 차단)", nodeName)
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
	if _, ok := node.Labels[driverUpgradingBlockingLabelKey]; ok {
		delete(node.Labels, driverUpgradingBlockingLabelKey)
		changed = true
	}
	// 우리 소유권 표식도 함께 회수한다. 정상 종료로 cordon 이 풀렸는데 표식이 남으면
	// handleFailed 가 나중에 남의(또는 다음 사이클의) cordon 을 우리 것으로 오인한다.
	if node.Annotations[cordonOwnerAnnotationKey] == state.Name {
		delete(node.Annotations, cordonOwnerAnnotationKey)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return err
	}
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeUpgradeLabelRemoved",
		"노드 %s에서 npu.ai/driver-upgrading + driver-upgrading-blocking 라벨 모두 제거 (device-plugin 재스케줄 허용)", nodeName)
	return nil
}

// reapStaleCordonClaim 은 우리가 노드의 unschedulable==false 를 관측했는데 우리 소유 annotation
// 이 남아 있으면 그 annotation 을 지운다. 사람이 uncordon 한 뒤 stale 로 남아 이후 남의 cordon 을
// 오인 해제하는 것을 막는다(마커 수명==cordon 수명 불변식 복원, EC-1).
func (m *UpgradeStateMachine) reapStaleCordonClaim(ctx context.Context, node *corev1.Node, state *v1alpha1.DriverUpgradeState) error {
	if node.Spec.Unschedulable || node.Annotations[cordonOwnerAnnotationKey] != state.Name {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	delete(node.Annotations, cordonOwnerAnnotationKey)
	return m.Patch(ctx, node, patch)
}

// clearUpgradingLabel은 Failed 전이 시 device-plugin 재스케줄이 가능하도록
// driver-upgrading 라벨과 driver-upgrading-blocking 라벨을 모두 제거한다.
// unschedulable은 수동 조치를 위해 보존.
func (m *UpgradeStateMachine) clearUpgradingLabel(ctx context.Context, nodeName string, state *v1alpha1.DriverUpgradeState) error {
	var node corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	_, hasMain := node.Labels[driverUpgradingLabelKey]
	_, hasBlocking := node.Labels[driverUpgradingBlockingLabelKey]
	if !hasMain && !hasBlocking {
		return nil
	}
	base := node.DeepCopy()
	if hasMain {
		delete(node.Labels, driverUpgradingLabelKey)
	}
	if hasBlocking {
		delete(node.Labels, driverUpgradingBlockingLabelKey)
	}
	if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return err
	}
	m.Recorder.Eventf(state, corev1.EventTypeNormal, "NodeUpgradeLabelRemoved",
		"노드 %s에서 driver-upgrading + driver-upgrading-blocking 라벨 제거 (device-plugin 재스케줄 허용)", nodeName)
	return nil
}

// acceleratorDomains 는 가속기 extended resource 의 도메인 접두다. 리소스명마다 다시
// 유도하지 않도록 한 번만 만든다 — uninstall 게이트(internal/uninstall/gate.go)가 쓰는
// 패턴과 같다.
var acceleratorDomains = intent.AcceleratorResourceDomains()

// hasAcceleratorResourceName 은 리소스명이 등록된 벤더의 가속기 extended resource
// 도메인 밑에 있는지를 본다. intent.AcceleratorResourceDomains 를 그대로 써서 uninstall
// 게이트(internal/uninstall/gate.go)와 같은 fail-closed 판정을 공유한다 — 전에는
// hasDeviceWorkloads·podUsesDevice 가 이 판정을 각자 세 문자열로 따로 적어 두고 있었고,
// 그 목록에 rebellions.ai/ 가 빠져 있어 rebellions ATOM 파드가 드라이버 업그레이드
// drain 에서 PDB 를 존중하는 eviction 대상으로 인식되지 못했다.
func hasAcceleratorResourceName(rn string) bool {
	for _, domain := range acceleratorDomains {
		if strings.Contains(rn, domain) {
			return true
		}
	}
	return false
}

// hasDeviceWorkloads는 노드에 GPU/NPU 장치를 쥔 Pod가 남아 있는지 확인합니다.
// 판정은 podUsesDevice 하나에 모아 둔다 — eviction 대상 선정(evictDevicePods)과 같은
// 집합을 봐야 하기 때문이다. 양쪽이 갈리면 drain 이 끝나지 않는다.
func (m *UpgradeStateMachine) hasDeviceWorkloads(ctx context.Context, nodeName string) (bool, error) {
	var podList corev1.PodList
	if err := m.List(ctx, &podList); err != nil {
		return false, err
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName || !podPhaseDrainable(pod) {
			continue
		}
		if podUsesDevice(pod) {
			return true, nil
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
		if pod.Spec.NodeName != state.Spec.NodeName || !podPhaseDrainable(pod) {
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

// podPhaseDrainable 은 drain 판정이 세는 단계인지다 — 종료 단계(Succeeded/Failed)만 제외한다.
// Pending 을 빼면 스케줄만 된 장치 요청 Pod 가 커널 모듈 교체 도중에 시작된다.
// hasDeviceWorkloads 와 evictDevicePods 가 이 함수를 함께 써서 같은 집합을 본다.
func podPhaseDrainable(pod *corev1.Pod) bool {
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// podUsesDevice는 Pod가 GPU/NPU 장치를 쥐고 있는지 반환합니다 — device-plugin 축
// (extended resource limits, containers + initContainers)과 DRA 축(pod.spec.resourceClaims)을
// 모두 봅니다.
//
// DRA 로 뜬 Pod 은 resources.limits 가 비어 있다 — 장치를 extended resource 가 아니라
// pod.spec.resourceClaims 로 쥐기 때문이다. limits 만 보면 그런 Pod 은 "아무것도 안 쥔 Pod"
// 으로 보여 drain 대상에서 빠지고, 그 위에서 커널 모듈이 교체된다.
// claim 을 DeviceClass→driver 로 되짚어 벤더를 가리지는 않는다: 이 판정은 안전 게이트라
// 과하게 잡는 쪽이 옳다(과하면 가속기와 무관한 DRA claim Pod 가 한 번 쫓겨날 뿐,
// 모자라면 장치가 밑에서 바뀐다).
func podUsesDevice(pod *corev1.Pod) bool {
	if len(pod.Spec.ResourceClaims) > 0 {
		return true
	}
	for _, ctrs := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, c := range ctrs {
			for resName := range c.Resources.Limits {
				if hasAcceleratorResourceName(string(resName)) {
					return true
				}
			}
		}
	}
	return false
}

// deleteDevicePluginPods는 해당 노드의 device-plugin Pod를 삭제합니다.
// device-plugin은 GPU 리소스를 요청하지 않지만 /dev/nvidia*를 직접 마운트하여
// 커널 모듈 참조를 잡으므로, drain 시 삭제해야 rmmod가 가능합니다.
func (m *UpgradeStateMachine) deleteDevicePluginPods(ctx context.Context, nodeName string) error {
	// device-plugin 은 벤더별로 다른 네임스페이스에 상주한다(3rd-party=kube-system,
	// TT=kcloud, #21). 하드코딩 대신 전 네임스페이스 조회 후 nodeName+이름/라벨로 필터.
	var podList corev1.PodList
	if err := m.List(ctx, &podList); err != nil {
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
	if err := m.List(ctx, &podList, client.InNamespace(naming.OperatorNamespace())); err != nil {
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
	if err := m.List(ctx, &podList, client.InNamespace(naming.OperatorNamespace())); err != nil {
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

// 커널 이미지를 쓰지 않았을 때 그 사유를 드러낼 이벤트 reason. 둘을 나눈 이유는 상황이 다르기
// 때문이다 — 하나는 이미지 참조가 있는데 검증에서 거절된 것이고, 하나는 참조를 만들지조차 못한
// 것이다. 같은 reason 으로 내면 운영자가 "거절된 이미지" 를 찾아 헤맨다.
const (
	eventKernelImageRejected     = "KernelImageRejected"
	eventKernelImageNotAssembled = "KernelImageNotAssembled"
	// eventKernelImageMissing 은 정책이 kernelImages 를 선언했는데 노드 커널 항목이 없어 기본
	// 이미지(DKMS)로 되돌아갈 때 낸다. 커널이 OS 업데이트로 바뀐 뒤 조용히 느린 경로로 가는 것을 드러낸다.
	eventKernelImageMissing = "KernelImageMissing"
	// eventPolicySharedOwnership 은 DIP spec 을 제3 관리자가 함께 소유할 때 낸다 — 다음 helm upgrade 가
	// Server-Side Apply 충돌로 멈출 수 있다.
	eventPolicySharedOwnership = "PolicySharedOwnership"
)

// assembleKernelImage 는 태그 규약으로 노드 커널용 사전 컴파일 이미지 참조를 조립한다.
// 조립 조건이 서지 않으면 빈 문자열을 돌려준다. 두 번째 반환값은 조립을 시도했으나 포기한
// 이유이며, 애초에 조립할 것이 없으면 빈 문자열이다 — 알릴 것이 없는데 경고를 내면 PreFlight
// 마다 같은 경고가 반복된다.
//
// 조립을 켜는 것은 spec.driver.kernelImageBuildRevision 이다 — 빌드 개정은 발행하는 쪽만 알아
// 유도할 수 없다. 버전 토큰은 spec.driver.version 에서, variant 토큰은
// spec.driver.kernelImageVariant 에서, 커널 토큰은 노드가 보고한 커널에서, 저장소 경로는
// spec.driver.image 에서 온다. variant 유무가 벤더마다 달라 그 차이는 차트가 채운다 —
// 여기서 벤더 이름으로 분기하지 않는다.
func assembleKernelImage(policy *v1alpha1.DriverInstallPolicy, kernelVersion string) (image string, skipReason string) {
	build := policy.Spec.Driver.KernelImageBuildRevision
	version := policy.Spec.Driver.Version
	// 버전 토큰 없이 조립하면 "-k<커널>-vN" 이 되어 규약을 벗어난다. 버전을 비우는 것은 설치기가
	// 버전을 고르는 하위호환 경로이고 그 경로에는 사전 컴파일 이미지가 없으므로, 조립할 것이 없는
	// 상태로 본다(CRD CEL 이 이 조합을 입구에서 거절하므로 여기는 이중 방어다).
	if build == "" || version == "" || policy.Spec.Driver.Image == "" {
		return "", ""
	}
	// 아래 세 토막은 그대로 image reference 의 태그로 들어간다. 태그에 쓸 수 없는 문자가 섞이면
	// Job 에 실을 수 없는 참조가 만들어지므로 조립하지 않는다. 커널은 노드가 보고한 값이고(자체
	// 빌드 커널의 localversion 접미사가 그렇다) 버전은 운영자가 적는 값이다 — 둘 다 본다.
	// 그런 노드·정책은 kernelImages 로 따로 지정한다.
	notAssembled := func(what string) (string, string) {
		return "", fmt.Sprintf(
			"커널 %s 의 사전 컴파일 이미지를 조립하지 못했다: %s에 이미지 태그로 쓸 수 없는 문자가 있다",
			kernelVersion, what)
	}
	if !tagSafeTokenRe.MatchString(kernelVersion) {
		return notAssembled("커널 문자열")
	}
	if !tagSafeTokenRe.MatchString(version) {
		return notAssembled("spec.driver.version")
	}
	tag := version
	if variant := policy.Spec.Driver.KernelImageVariant; variant != "" {
		tag += "-" + variant
	}
	return replaceImageTag(policy.Spec.Driver.Image, tag+"-k"+kernelVersion+"-"+build), ""
}

// tagSafeTokenRe 는 조립에 넣을 토막을 image reference 의 태그 안에 그대로 써도 되는지 본다.
// docker 태그가 받는 문자(영숫자·점·밑줄·하이픈)만 허용한다.
var tagSafeTokenRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// resolveDriverImage 는 노드 커널에 맞는 사전 컴파일 이미지가 정책에 선언돼 있거나 태그 규약으로
// 조립되면 그것을, 아니면 기본 설치 이미지를 돌려준다. 두 번째 반환값은 그 커널 이미지를 쓰지 않은
// 이유이며, 이미지를 썼거나 애초에 커널 이미지가 없으면 빈 문자열이다. 호출부가 이 이유를 경고로
// 드러낸다. 비어 있지 않은 사유를 내는 경로는 셋이다 — 찾은 이미지를 무시하는 둘과, 선언한 맵에서
// 그 커널을 아예 찾지 못하는 하나(아래 "맵에 노드 커널 항목이 없으면" 단락). 무시하는 둘은 이렇다.
//   - 태그가 검증 빌드 태그(…-vN)가 아니다. 존재하지 않는 이미지를 Job 에 싣지 않기 위해서다.
//   - 태그의 버전 토큰이 spec.driver.version 과 다르다. 사전 컴파일 이미지는 자기가 구운 사용자
//     공간과 모듈을 함께 설치하므로 그 둘이 어긋날 수는 없다. 그래도 이 검사를 두는 이유는 정책이
//     선언한 버전과 이미지가 실제로 담은 버전이 다를 때 운영자 의도와 다른 드라이버가 깔리기
//     때문이다. 기본 이미지로 되돌아가면 DKMS 가 spec.driver.version 으로 컴파일하므로 선언과
//     결과가 맞는다.
//
// 맵에 노드 커널 항목이 없으면 kernelImageBuildRevision 이 선언됐을 때만 태그 규약으로 조립한다
// (assembleKernelImage). 조립한 참조도 위 두 검사를 그대로 거친다 — 조립이 검증을 우회하면
// spec.driver.version 을 잘못 적은 정책이 레지스트리에 없는 태그를 Job 에 싣는다.
//
// 세 번째 반환값은 skipReason 을 드러낼 이벤트 reason 이다. skipReason 이 비면 빈 문자열이다.
//
// 두 경로에서 spec.driver.version 의 뜻이 다르다. 사전 컴파일 경로에서는 이미지가 설치 버전을
// 정하고 이 값은 위 대조에만 쓰인다. DKMS 경로에서는 이 값이 설치 버전을 정하므로 노드의 apt
// 저장소에 실재해야 한다 — 그래서 차트 기본값은 우분투 아카이브에 있는 버전이어야 한다.
//
// spec.driver.version 이 비어 있으면 대조 기준이 없으므로 버전 검사를 건너뛴다. 버전을 비우는 것은
// 설치기가 버전을 고르는 하위호환 경로다.
func resolveDriverImage(
	policy *v1alpha1.DriverInstallPolicy, kernelVersion string,
) (image string, skipReason string, skipEventReason string) {
	if kernelVersion == "" {
		return policy.Spec.Driver.Image, "", ""
	}
	// 맵에 적힌 항목이 조립보다 우선한다 — 규약을 벗어난 이미지의 예외 지정 수단이다.
	ref := ""
	origin := "맵에 선언한"
	if img, ok := policy.Spec.Driver.KernelImages[kernelVersion]; ok {
		ref = string(img)
	} else {
		assembled, assembleSkip := assembleKernelImage(policy, kernelVersion)
		if assembled == "" {
			if assembleSkip == "" {
				if len(policy.Spec.Driver.KernelImages) == 0 {
					return policy.Spec.Driver.Image, "", ""
				}
				keys := make([]string, 0, len(policy.Spec.Driver.KernelImages))
				for k := range policy.Spec.Driver.KernelImages {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				return policy.Spec.Driver.Image, fmt.Sprintf(
					"커널 %s 의 사전 컴파일 이미지가 kernelImages 에 없어 기본 이미지(DKMS 컴파일)로 되돌아간다 — 선언된 커널: %s",
					kernelVersion, strings.Join(keys, ", ")), eventKernelImageMissing
			}
			return policy.Spec.Driver.Image, assembleSkip, eventKernelImageNotAssembled
		}
		ref, origin = assembled, "규약으로 조립한"
	}
	if !isVerifiedBuildTag(ref) {
		return policy.Spec.Driver.Image, fmt.Sprintf(
			"커널 %s 의 %s 사전 컴파일 이미지 %q 가 검증 빌드 태그(…-vN)가 아니어서 쓰지 않는다",
			kernelVersion, origin, ref), eventKernelImageRejected
	}
	if want := policy.Spec.Driver.Version; want != "" {
		if got := extractKernelImageVersion(ref); got != want {
			return policy.Spec.Driver.Image, fmt.Sprintf(
				"커널 %s 의 %s 사전 컴파일 이미지 버전 토큰 %q 가 spec.driver.version %q 와 달라서 쓰지 않는다",
				kernelVersion, origin, got, want), eventKernelImageRejected
		}
	}
	return ref, "", ""
}

// ownSpecManagerPrefixes 는 경고에서 제외하는 관리자 이름 접두 목록이다 — helm 과 제품 자신의
// 제어 경로. pkg/npuctl.UpgradeVendor 는 이제 FieldManager 를 `kcloud-operator-upgrade` 로
// 명시한다(= kcloud-operator 접두에 걸림). `manager`·`kubectl-npu` 는 그 전에 FieldManager 없이
// patch 해서 API 서버가 User-Agent(= 바이너리 파일명)에서 이름을 유도했을 때 남은 과거 항목이다 —
// 라이브 .91 조회로 확인한 실제 이름은 operator 바이너리 `manager`(Dockerfile 산출물 이름,
// DriverUpgradeState 항목이 그 이름), CLI `kubectl-npu`. 라이브 managedFields 에 그 이름이
// 더는 없을 때 접두 목록에서 제거한다. 접두로 비교하는 이유는 버전 접미가 붙는 변형을 함께
// 거르기 위해서다. `manager` 는 kubebuilder 기본 바이너리 이름이라 같은 이름의 제3 컨트롤러도 함께 빠진다 —
// 자기 제어 경로를 "외부 개입" 으로 지목하는 쪽이 보고된 결함이라 그 방향을 택했다.
// `kubectl` 전체를 넣지 않는다: 사람이 돌린 kubectl patch/edit 는 그대로 경고 대상이다.
var ownSpecManagerPrefixes = []string{"helm", "kubectl-npu", "kcloud-operator", "manager"}

// isOwnSpecManager 는 관리자 이름이 제외 접두 중 하나로 시작하는지 본다.
func isOwnSpecManager(name string) bool {
	for _, p := range ownSpecManagerPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// foreignSpecManagers 는 spec 을 쥔 제3 field manager 목록이다(정렬·중복 제거). 둘이 같은 spec
// 필드를 쥐면 helm 이 값을 바꾸는 다음 upgrade 가 충돌한다. kubectl-label·kubectl-annotate 처럼
// metadata 만 쥔 관리자는 세지 않는다.
// Operation 은 가리지 않는다: helm 은 라이브 .91 에서 Apply 로, 세방향 병합 경로에서는 Update 로
// 남고 둘 다 이름으로 빠진다. 반대로 SSA 로 소유하는 ArgoCD·Flux·`helm --server-side` 는 Apply 라
// Update 한정이 있으면 전부 빠졌다 — 다음 helm upgrade 를 실제로 멈추는 쪽이 이들이다.
func foreignSpecManagers(entries []metav1.ManagedFieldsEntry) []string {
	seen := map[string]bool{}
	for _, e := range entries {
		if isOwnSpecManager(e.Manager) || e.FieldsV1 == nil {
			continue
		}
		if strings.Contains(string(e.FieldsV1.Raw), `"f:spec"`) {
			seen[e.Manager] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// refreshResolvedImage 는 status.ResolvedImage 를 지금 정책 기준으로 다시 계산한다.
// PreFlight 에서만 계산하면 정책에서 kernelImages 항목을 뺀 뒤에도 옛 태그가 status 에 남아
// 운영자가 그 이미지를 아직 쓰는 것으로 오해한다. 노드 조회는 controller-runtime 캐시에서
// 읽으므로 API 호출이 늘지 않는다. 노드를 찾지 못하면 값을 그대로 둔다 — 노드가 빠지는
// 중일 수 있고 그때 마지막 관측값을 지우는 것은 얻는 것이 없다.
// 이미지를 쓰지 않은 사유는 여기서 버린다. 경고 이벤트는 실제 설치가 일어나는 PreFlight 에서만
// 낸다(Idle 은 60초마다 다시 들어오므로 같은 경고가 반복된다).
func (m *UpgradeStateMachine) refreshResolvedImage(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
	policy *v1alpha1.DriverInstallPolicy,
) error {
	var node corev1.Node
	if err := m.Get(ctx, types.NamespacedName{Name: state.Spec.NodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("노드 조회 실패: %w", err)
	}
	image, _, _ := resolveDriverImage(policy, node.Status.NodeInfo.KernelVersion)
	state.Status.ResolvedImage = image
	return nil
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
// imageVariantSuffixRe 는 verified build tag 의 variant 접미사를 매치한다.
// 예: "580.142-v172" → "-v172", "1.7.8-v2" → "-v2".
var imageVariantSuffixRe = regexp.MustCompile(`-v[0-9]+(\.[0-9]+)?$`)

// extractImageVariantSuffix 는 image reference 의 tag 부분에서 variant 접미사를 추출한다.
// 매치되지 않으면 빈 문자열 반환.
func extractImageVariantSuffix(image string) string {
	idx := strings.LastIndex(image, ":")
	if idx == -1 {
		return ""
	}
	return imageVariantSuffixRe.FindString(image[idx+1:])
}

// kernelImageVersionRe 는 커널 이미지 태그 앞머리의 드라이버 버전 토큰을 매치한다.
// 태그 규약은 "<version>[-<variant>]-k<kernel>-vN" 이며, version 은 숫자로 시작하는 점 구분
// 버전이다. 하위 번호가 있으면 "-<숫자>" 하나까지 포함한다(예 "1.9.8-3"). variant(예 "rngd")
// 와 커널·빌드 접미사는 숫자로 시작하지 않으므로 여기에 걸리지 않는다. variant 쪽 전제는
// CRD 패턴이 강제한다 — kernelImageVariant 는 영문자로 시작해야 한다.
var kernelImageVersionRe = regexp.MustCompile(`^[0-9][0-9.]*(-[0-9]+)?`)

// extractKernelImageVersion 은 image reference 태그 앞머리의 드라이버 버전 토큰만 뽑는다.
// variant·커널·빌드 접미사는 버린다. 예: "…:2026.3.1-rngd-k6.8.0-138-generic-v1" → "2026.3.1".
// 태그가 없거나 버전 토큰이 없으면 빈 문자열을 돌려준다.
func extractKernelImageVersion(image string) string {
	idx := strings.LastIndex(image, ":")
	if idx == -1 {
		return ""
	}
	return kernelImageVersionRe.FindString(image[idx+1:])
}

// precompiledKernelImageRe 는 사전 컴파일 커널 이미지 태그의 커널 토큰(-k<커널버전>)을 매치한다.
// 태그 규약 "<version>[-<variant>]-k<kernel>-vN" 에서 커널 버전은 숫자로 시작하므로 "-k<숫자>"
// 로 식별한다. 버전 비종속 태그(예 580.159.03-v179)에는 이 토큰이 없다.
var precompiledKernelImageRe = regexp.MustCompile(`-k[0-9]`)

// isPrecompiledKernelImage 는 image reference 의 태그가 사전 컴파일 커널 이미지인지 반환한다.
// 커널 이미지는 특정 드라이버 버전의 모듈을 담아 그 버전 전용이다 — DRIVER_VERSION 만 바꿔서는
// 다른 버전을 설치하지 못한다. 되돌리기·재구성 경로가 이 판별로 버전 종속 이미지를 거른다.
func isPrecompiledKernelImage(image string) bool {
	idx := strings.LastIndex(image, ":")
	if idx == -1 {
		return false
	}
	return precompiledKernelImageRe.MatchString(image[idx+1:])
}

// replaceImageTag 는 image reference 의 tag 만 newTag 로 치환한다.
// newTag 가 plain (variant 없음) 이고 기존 tag 에 variant 가 있으면, variant 를 보존하여
// rollback 시 broken plain tag (예: ":580.142") 가 생성되는 것을 차단한다.
func replaceImageTag(image string, newTag string) string {
	idx := strings.LastIndex(image, ":")
	if idx == -1 {
		return image + ":" + newTag
	}
	// newTag 가 이미 variant 포함 (예: "580.142-v172") 이면 그대로 사용
	if !strings.Contains(newTag, "-v") {
		if v := imageVariantSuffixRe.FindString(image[idx+1:]); v != "" {
			newTag = newTag + v
		}
	}
	return image[:idx+1] + newTag
}

// verifiedBuildTagRegexp 는 검증된 빌드 태그 패턴을 정의한다.
// 허용:
//   - "vN"           : v1, v16
//   - ".+-vN"        : 1.9.8-3-v2, 580.126.09-v16, 1.7.8-v3
//   - "latest"       : latest 태그 (개발 환경)
//
// 거부:
//   - plain semver   : 580.126.09, 1.9.8-3, 590.48.01 — broken plain tag (architectural plan §3)
//
// architectural plan §3.4: rollback 시 broken plain tag 가 PreviousImage 로 저장되어
// rollback 발동 시 broken Pod 으로 회귀하는 결함을 defense-in-depth 로 차단.
var verifiedBuildTagRegexp = regexp.MustCompile(`:(v[0-9]+|.+-v[0-9]+|latest)$`)

// isVerifiedBuildTag 는 image reference 의 태그가 검증된 빌드 태그인지 확인한다.
// "<registry>/<repo>:<tag>" 형태의 image reference 에서 tag 부분을 추출해 검사.
// tag 가 없는 reference (e.g., "alpine") 는 false 반환.
func isVerifiedBuildTag(image string) bool {
	idx := strings.LastIndex(image, ":")
	if idx == -1 {
		return false
	}
	return verifiedBuildTagRegexp.MatchString(image[idx:])
}

// EnsureUpgradingLabelRemoved 는 노드에서 npu.ai/driver-upgrading 라벨을 idempotent 하게 제거한다.
// controller 의 defer cleanup / stuck-label sweep 가 호출하기 위한 외부 진입점.
//
// 동작:
//   - 라벨 없으면 no-op (라벨 patch 호출 없음 → API 부하 0)
//   - 라벨 있으면 MergeFrom patch 로 제거. Conflict 발생 시 최대 3회 재시도.
//   - reconcile context cancel 와 무관하게 cleanup 이 진행되도록 호출자가 별도 ctx 를 주입할 책임.
func (m *UpgradeStateMachine) EnsureUpgradingLabelRemoved(ctx context.Context, nodeName string) error {
	return m.ensureLabelRemoved(ctx, nodeName, driverUpgradingLabelKey)
}

// EnsureUpgradingBlockingLabelRemoved 는 노드에서 driver-upgrading-blocking 라벨을 idempotent
// 하게 제거한다. controller 의 stuck-label sweep 가 좁은 lifecycle 의 라벨을 빨리 정리하기 위해 호출.
func (m *UpgradeStateMachine) EnsureUpgradingBlockingLabelRemoved(ctx context.Context, nodeName string) error {
	return m.ensureLabelRemoved(ctx, nodeName, driverUpgradingBlockingLabelKey)
}

// ensureLabelRemoved 는 지정한 키의 라벨을 idempotent 하게 제거한다.
// Conflict 시 최대 3회 재시도. 라벨 미존재 시 no-op (API patch 호출 0).
func (m *UpgradeStateMachine) ensureLabelRemoved(ctx context.Context, nodeName, labelKey string) error {
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
		if _, ok := node.Labels[labelKey]; !ok {
			return nil
		}
		base := node.DeepCopy()
		delete(node.Labels, labelKey)
		if err := m.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("%d회 conflict 재시도 후 라벨 %s 제거 실패: %w", maxRetries, labelKey, lastErr)
}

// ─────────────────────────────────────────────
// A6.1 Quiesce-on-Driver-Upgrade (opt-in label)
// ─────────────────────────────────────────────

// QuiesceLabeledDeployments 는 cordon 직후 호출되어 노드별로 quiesce-on-driver-upgrade 라벨이
// 붙은 Deployment 를 spec.replicas=0 으로 patch 하고, 원래 replicas 를
// state.Status.QuiescedDeployments backup 에 기록한다 (architectural plan §A6.1).
//
// 매칭 기준 (deploymentTargetsNode):
//   - PodTemplate 의 nodeSelector["kubernetes.io/hostname"] == 노드명, 또는
//   - PodTemplate 의 nodeAffinity required term 의 hostname In 매칭
//
// 다른 라벨 기반 selector (예: app=workload + nodeAffinity 가 GPU 라벨) 은 1차 구현 범위
// 밖이며, follow-up 으로 확장한다. quiesce 실패는 best-effort 로 사이클 진행을 막지 않는다.
func (m *UpgradeStateMachine) QuiesceLabeledDeployments(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
) error {
	logger := logf.FromContext(ctx)

	var deploys appsv1.DeploymentList
	if err := m.List(ctx, &deploys, client.MatchingLabels{
		quiesceOnDriverUpgradeLabelKey: labelValueTrue,
	}); err != nil {
		return fmt.Errorf("quiesce 후보 Deployment list 실패: %w", err)
	}

	quiesced := make([]v1alpha1.QuiescedDeployment, 0, len(deploys.Items))
	for i := range deploys.Items {
		d := &deploys.Items[i]
		if !deploymentTargetsNode(d, state.Spec.NodeName) {
			continue
		}
		if d.Spec.Replicas == nil || *d.Spec.Replicas == 0 {
			// 이미 0 (다른 controller 가 scale 한 상태) — 복구 대상 아님.
			continue
		}
		original := *d.Spec.Replicas
		base := d.DeepCopy()
		// dual-write (followup plan §F2): annotation 으로도 backup 저장 →
		// DUS.Status 손실 시 (operator restart 등) annotation 으로 fallback 복구 가능.
		if d.Annotations == nil {
			d.Annotations = make(map[string]string)
		}
		d.Annotations[QuiesceReplicasBackupAnnotation] = strconv.Itoa(int(original))
		zero := int32(0)
		d.Spec.Replicas = &zero
		if err := m.Patch(ctx, d, client.MergeFrom(base)); err != nil {
			logger.Error(err, "Deployment quiesce 실패",
				"deploy", d.Name, "namespace", d.Namespace, "node", state.Spec.NodeName)
			continue
		}
		quiesced = append(quiesced, v1alpha1.QuiescedDeployment{
			Namespace:        d.Namespace,
			Name:             d.Name,
			OriginalReplicas: original,
		})
		logger.Info("Deployment quiesced for driver upgrade",
			"deploy", d.Name, "namespace", d.Namespace, "from", original, "to", 0,
			"node", state.Spec.NodeName)
		if m.Recorder != nil {
			m.Recorder.Eventf(state, corev1.EventTypeNormal, "DeploymentQuiesced",
				"Deployment %s/%s scaled %d → 0 for driver upgrade (node=%s)",
				d.Namespace, d.Name, original, state.Spec.NodeName)
		}
	}
	state.Status.QuiescedDeployments = quiesced

	// followup plan §F2: status backup 을 즉시 영구 저장 — handleCordoning 이 transitionTo
	// 로 다음 상태(Draining)로 넘어가기 전에 commit 해야 operator restart resilience 가 보장됨.
	// 외부 Reconcile 의 최종 Status.Update 가 bumped ResourceVersion 을 받아도 conflict 시
	// requeue 로 처리되므로 race 안전.
	if err := m.Status().Update(ctx, state); err != nil {
		logger.Error(err, "quiesce status backup 영구 저장 실패",
			"node", state.Spec.NodeName, "count", len(quiesced))
		return fmt.Errorf("failed to persist quiesce status backup: %w", err)
	}
	return nil
}

// RestoreQuiescedDeployments 는 cycle 종료(Idle/Failed) 시 호출되어 backup 의 OriginalReplicas
// 로 Deployment.spec.replicas 를 복구하고 backup 을 비운다 (architectural plan §A6.1).
//
// 안전성:
//   - backup 이 비어 있으면 annotation fallback 으로 복구 시도 (followup plan §F2 — operator
//     restart 시 status 손실에 대한 방어).
//   - Deployment NotFound (이미 삭제됨) 는 graceful skip — 이벤트만 발행.
//   - 개별 patch 실패는 로그/이벤트로 보고하지만 다른 entry 처리는 계속.
//   - 모든 entry 처리 후 backup 을 nil 로 비워 다음 cycle 에 누수되지 않게 한다.
func (m *UpgradeStateMachine) RestoreQuiescedDeployments(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
) error {
	logger := logf.FromContext(ctx)
	if len(state.Status.QuiescedDeployments) == 0 {
		// followup plan §F2: status 손실 시 annotation 기반 fallback 복구 시도.
		// 정상 흐름에서는 backup 이 채워져 있으므로 no-op 으로 끝남.
		return m.restoreFromAnnotationFallback(ctx, state)
	}
	for _, q := range state.Status.QuiescedDeployments {
		var d appsv1.Deployment
		if err := m.Get(ctx, types.NamespacedName{Namespace: q.Namespace, Name: q.Name}, &d); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("restore: deployment 미발견 (이미 삭제됨)",
					"deploy", q.Name, "namespace", q.Namespace)
				if m.Recorder != nil {
					m.Recorder.Eventf(state, corev1.EventTypeWarning, "DeploymentRestoreSkipped",
						"Deployment %s/%s 미발견 (이미 삭제됨) — restore skip",
						q.Namespace, q.Name)
				}
				continue
			}
			logger.Error(err, "restore: deployment Get 실패",
				"deploy", q.Name, "namespace", q.Namespace)
			continue
		}
		base := d.DeepCopy()
		original := q.OriginalReplicas
		d.Spec.Replicas = &original
		// dual-write annotation cleanup (followup plan §F2): restore 와 동시에 backup
		// annotation 을 제거하여 다음 cycle 의 fallback 이 stale 값을 사용하지 않게 한다.
		if d.Annotations != nil {
			delete(d.Annotations, QuiesceReplicasBackupAnnotation)
		}
		if err := m.Patch(ctx, &d, client.MergeFrom(base)); err != nil {
			logger.Error(err, "restore patch 실패",
				"deploy", q.Name, "namespace", q.Namespace)
			if m.Recorder != nil {
				m.Recorder.Eventf(state, corev1.EventTypeWarning, "DeploymentRestoreFailed",
					"Deployment %s/%s 복구 실패 (수동 조치 필요): %v",
					q.Namespace, q.Name, err)
			}
			continue
		}
		logger.Info("Deployment restored after driver upgrade",
			"deploy", q.Name, "namespace", q.Namespace, "replicas", original)
		if m.Recorder != nil {
			m.Recorder.Eventf(state, corev1.EventTypeNormal, "DeploymentRestored",
				"Deployment %s/%s scaled 0 → %d after driver upgrade",
				q.Namespace, q.Name, original)
		}
	}
	state.Status.QuiescedDeployments = nil
	return nil
}

// restoreFromAnnotationFallback 은 DUS.Status.QuiescedDeployments 가 비어있는 상황 (operator
// restart 후 status 손실 등) 에서 Deployment annotation (npu.ai/replicas-backup) 으로부터
// replicas 를 복구하는 fallback 경로다 (followup plan §F2).
//
// 매칭: quiesce 라벨 + node hostname 매칭 + annotation 존재. 동작은 idempotent —
// annotation 이 없거나 값이 invalid 한 Deployment 는 graceful skip.
func (m *UpgradeStateMachine) restoreFromAnnotationFallback(
	ctx context.Context,
	state *v1alpha1.DriverUpgradeState,
) error {
	logger := logf.FromContext(ctx)

	var deploys appsv1.DeploymentList
	if err := m.List(ctx, &deploys, client.MatchingLabels{
		quiesceOnDriverUpgradeLabelKey: labelValueTrue,
	}); err != nil {
		return fmt.Errorf("annotation fallback: deployment list 실패: %w", err)
	}

	for i := range deploys.Items {
		d := &deploys.Items[i]
		if !deploymentTargetsNode(d, state.Spec.NodeName) {
			continue
		}
		backupVal, ok := d.Annotations[QuiesceReplicasBackupAnnotation]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(backupVal)
		if err != nil || n < 0 {
			logger.Info("annotation fallback: invalid backup value — skip",
				"deploy", d.Name, "namespace", d.Namespace, "value", backupVal)
			continue
		}
		original := int32(n)
		base := d.DeepCopy()
		d.Spec.Replicas = &original
		delete(d.Annotations, QuiesceReplicasBackupAnnotation)
		if err := m.Patch(ctx, d, client.MergeFrom(base)); err != nil {
			logger.Error(err, "annotation fallback patch 실패",
				"deploy", d.Name, "namespace", d.Namespace)
			continue
		}
		logger.Info("Deployment restored via annotation fallback",
			"deploy", d.Name, "namespace", d.Namespace, "replicas", original,
			"node", state.Spec.NodeName)
		if m.Recorder != nil {
			m.Recorder.Eventf(state, corev1.EventTypeNormal, "DeploymentRestoredFromAnnotation",
				"Deployment %s/%s scaled 0 → %d via annotation backup (status was lost)",
				d.Namespace, d.Name, original)
		}
	}
	return nil
}

// deploymentTargetsNode 는 Deployment 의 PodTemplate 이 특정 노드를 대상으로 함을 표시하는지
// 확인한다 (architectural plan §A6.1 1차 구현 — hostname 기반만 매칭).
//
// 매칭 규칙:
//  1. spec.template.spec.nodeSelector["kubernetes.io/hostname"] == nodeName
//  2. spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution
//     의 NodeSelectorTerms 중 하나의 MatchExpressions 가 hostname In [...nodeName...]
//
// preferredDuringScheduling / 다른 라벨 기반은 false 반환 (false negative 보수적 처리).
// 잘못된 quiesce 위험을 피하기 위함이며, follow-up 으로 확장 가능.
func deploymentTargetsNode(d *appsv1.Deployment, nodeName string) bool {
	if d == nil || nodeName == "" {
		return false
	}
	podSpec := d.Spec.Template.Spec
	// 1. nodeSelector hostname 직접 매칭
	if v, ok := podSpec.NodeSelector[hostnameLabelKey]; ok && v == nodeName {
		return true
	}
	// 2. nodeAffinity required term 의 hostname In 매칭
	if podSpec.Affinity == nil || podSpec.Affinity.NodeAffinity == nil ||
		podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != hostnameLabelKey || expr.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			for _, val := range expr.Values {
				if val == nodeName {
					return true
				}
			}
		}
	}
	return false
}

// containsString 는 slice 에 s 가 포함되어 있으면 true 를 반환한다.
// resetRebootTracking 은 새 업그레이드 사이클에 들어갈 때 이전 사이클의 재부팅 흔적을 지운다.
// 카운터만 지우고 마커(RebootRequestedTime/RebootBootID)를 남기면, cross-major 재부팅 게이트가
// 낡은 bootID 를 현재 부팅으로 착각한다.
func resetRebootTracking(s *v1alpha1.DriverUpgradeStateStatus) {
	s.RebootAttempts = 0
	s.RebootRequestedTime = metav1.Time{}
	s.RebootBootID = ""
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// DeviceModelMatches 는 정책·상태에 적힌 모델 이름(specModel)이 NodeDeviceReport 의 장치
// 모델(deviceModel)과 같은 대상을 가리키는지 판정한다. 모델 매칭 규칙의 정본이다 —
// DriverUpgradeState 소비자 둘(state_machine.go·state_machine_job.go)과 정책 선택
// (findPolicy, controller/driver_upgrade_controller.go)이 모두 이 함수만 부른다.
// 규칙을 복제하면 다음에 또 한 곳만 고친다(2026-08-11 에 그렇게 findPolicy 가 빠졌다).
//
// "generic" 은 이름이 아니라 **모른다는 표시**다 — detector 가 제품을 판정하지 못했을 때
// 남기는 값이라 어느 쪽에 있든 와일드카드로 봐야 한다.
//
// 구체명이 붙는 방향도 같은 문제다. detector 가 표준 PCI DB 계층을 붙이면 장치 이름이
// `a100` 이 아니라 `a100-pcie-40gb`(실측 10de:20f1), GTX 970 은 `geforce-gtx-970` 이 된다.
// 사용자가 쓰는 `spec.model: a100` 은 제품군 이름이라 문자열 비교로는 영영 안 걸리고,
// 양쪽 다 "generic" 이 아니어서 와일드카드도 안 걸린다 — findPolicy 가 nil 을 돌려주고
// 증상은 "아무 일도 안 일어남" 이다. 그래서 한쪽이 다른 쪽의 `-` 경계 접두어면 같은 대상으로
// 본다. 경계를 요구하므로 `a30` 은 `a100-pcie-40gb` 에 안 걸린다(과잉 매칭 방어).
//
// 양방향인 이유: spec.model 은 만들어질 때 한 번 기록된 값이라 detector 가 갱신되면 장치
// 쪽만 구체화되고, detector 를 되돌리거나 `update-pciids` 로 표기가 바뀌면 반대 방향으로
// 같은 일이 생긴다. 한 방향만 보면 그때 모든 장치가 걸러져 재부팅 게이트와 자가복구
// 게이트가 조용히 죽는다.
func DeviceModelMatches(deviceModel, specModel string) bool {
	if specModel == "" || strings.EqualFold(specModel, "generic") {
		return true
	}
	if strings.EqualFold(deviceModel, "generic") {
		return true
	}
	if strings.EqualFold(deviceModel, specModel) {
		return true
	}
	return modelFamilyPrefix(specModel, deviceModel) || modelFamilyPrefix(deviceModel, specModel)
}

// modelFamilyPrefix 는 short 가 long 의 제품군 접두어인지 본다 — `-` 경계까지 요구해
// `a1` 이 `a100-pcie-40gb` 에 걸리지 않게 한다.
func modelFamilyPrefix(short, long string) bool {
	return len(long) > len(short) && long[len(short)] == '-' &&
		strings.EqualFold(long[:len(short)], short)
}
