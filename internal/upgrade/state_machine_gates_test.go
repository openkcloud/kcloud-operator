// state_machine_gates_test.go: idle cooldown defer + kernel allowlist preflight 게이트 단위 테스트
// 상세: 라이브 트리거(cordon/drain/driver 변경) 없이 순수 state-machine 분기를 fake client 로 검증.
//   - IdleCooldown: Idle 진입 직후 재트리거 연기(IdleCooldownDeferred, 상태 불변).
//   - Kernel allowlist: PreFlight 에서 커널 불일치 시 Rollback 전이(PreFlightFailed).
//
// 생성일: 2026-07-21
package upgrade

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	v1alpha1 "kcloud-operator/api/v1alpha1"
)

// drainEvents 는 FakeRecorder 채널에 쌓인 이벤트 문자열을 전부 비워 반환한다("Normal Reason msg" 형식).
func drainEvents(sm *UpgradeStateMachine) []string {
	rec := sm.Recorder.(*record.FakeRecorder)
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(events []string, reason string) bool {
	for _, e := range events {
		if strings.Contains(e, reason) {
			return true
		}
	}
	return false
}

// makeDaemonsetDIP 는 mode 미지정(daemonset) DIP 를 반환한다 — job self-heal 분기를 우회해
// handleIdle 의 버전 불일치+cooldown 경로를 직접 태우기 위함.
func makeDaemonsetDIP(version string, autoUpgrade bool) *v1alpha1.DriverInstallPolicy {
	return &v1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: jVendor},
		Spec: v1alpha1.DriverInstallPolicySpec{
			Vendor: jVendor, Model: jModel,
			Driver:        v1alpha1.DriverSpec{Version: version}, // Mode="" = daemonset
			UpgradePolicy: &v1alpha1.UpgradePolicy{AutoUpgrade: autoUpgrade},
		},
	}
}

// ── 항목1: Idle cooldown defer ──────────────────────────────────────
// Idle 진입(LastTransitionTime=now) 직후 버전 불일치 트리거는 default 10s cooldown 미충족으로
// 연기된다: IdleCooldownDeferred 이벤트 + 잔여시간 requeue + 상태 불변(Idle 유지, cordon 시작 안 함).
func TestHandleIdle_CooldownNotElapsed_Deferred(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, verOld, verOld) // LastTransitionTime=now
	dip := makeDaemonsetDIP(verNew, true)                        // desired=verNew ≠ current=verOld
	sm := newUpgradeSMWithRecorder(dus, dip)

	_, requeue, err := sm.handleIdle(context.Background(), dus, dip)
	if err != nil {
		t.Fatalf("handleIdle 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateIdle {
		t.Errorf("state=%q, want Idle (cooldown 중 전이 금지)", dus.Status.State)
	}
	if requeue <= 0 || requeue > 10*time.Second {
		t.Errorf("requeue=%v, want (0, 10s] 잔여 cooldown", requeue)
	}
	if !hasEvent(drainEvents(sm), "IdleCooldownDeferred") {
		t.Error("IdleCooldownDeferred 이벤트 미발행")
	}
}

// cooldown 비활성화(IdleCooldownSeconds=0)면 즉시 트리거되어 Required(업그레이드 슬롯 확보)로 전이.
func TestHandleIdle_CooldownDisabled_TriggersImmediately(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, verOld, verOld)
	dip := makeDaemonsetDIP(verNew, true)
	zero := int32(0)
	dip.Spec.UpgradePolicy.IdleCooldownSeconds = &zero
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleIdle(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleIdle 실패: %v", err)
	}
	if dus.Status.State == v1alpha1.UpgradeStateIdle {
		t.Error("cooldown=0 인데 Idle 유지됨 (즉시 트리거 실패)")
	}
}

// ── 항목2a: Kernel allowlist preflight 게이트 ───────────────────────
// PreFlight 에서 노드 커널이 allowlist 와 불일치하면 PreFlightFailed 이벤트 + Rollback 전이.
// cordon(Cordoning) 이전 단계이므로 노드는 cordon 되지 않는다.
func TestHandlePreFlight_KernelNotInAllowlist_Rollback(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KernelVersion: "5.15.0-181-generic"}},
	}
	dus := makeJobDUS(v1alpha1.UpgradeStatePreFlight, verOld, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Spec.KernelAllowlist = []string{"6.8.*"} // glob — 5.15 불포함
	sm := newUpgradeSMWithRecorder(dus, dip, node)

	if _, _, err := sm.handlePreFlight(context.Background(), dus, dip); err != nil {
		t.Fatalf("handlePreFlight 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRollback {
		t.Errorf("state=%q, want Rollback (커널 불일치)", dus.Status.State)
	}
	if !hasEvent(drainEvents(sm), "PreFlightFailed") {
		t.Error("PreFlightFailed 이벤트 미발행")
	}
}

// 커널이 allowlist 와 일치하면 PreFlight 통과 → Cordoning 전이.
func TestHandlePreFlight_KernelInAllowlist_Cordoning(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{KernelVersion: "5.15.0-181-generic"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	dus := makeJobDUS(v1alpha1.UpgradeStatePreFlight, verOld, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Spec.KernelAllowlist = []string{"5.15.*"} // glob — 일치
	sm := newUpgradeSMWithRecorder(dus, dip, node)

	if _, _, err := sm.handlePreFlight(context.Background(), dus, dip); err != nil {
		t.Fatalf("handlePreFlight 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateCordoning {
		t.Errorf("state=%q, want Cordoning (커널 일치 통과)", dus.Status.State)
	}
}
