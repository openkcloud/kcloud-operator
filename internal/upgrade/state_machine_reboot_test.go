// state_machine_reboot_test.go: S2-5 cross-major reboot 전이 단위 테스트
package upgrade

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/driverjob"
	"kcloud-operator/internal/naming"
)

func rebootNDR(needsReboot bool) *v1alpha1.NodeDeviceReport {
	return &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: jNode},
		Status: v1alpha1.NodeDeviceReportStatus{
			Devices: []v1alpha1.DeviceEntry{
				{Vendor: jVendor, Model: jModel, DriverLoaded: true, DriverVersion: verNew, NeedsReboot: needsReboot, Count: 1},
			},
		},
	}
}

func readyNode(ready bool) *corev1.Node {
	cond := corev1.ConditionTrue
	if !ready {
		cond = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: cond}}},
	}
}

// bootIDPre 는 재부팅 직전 캡처된 bootID 를 모사하는 테스트 상수.
const bootIDPre = "boot-A"

// readyNodeBootID 는 Ready 상태 + 지정한 bootID 를 가진 노드를 만든다(P2 premature-Ready 가드 테스트용).
func readyNodeBootID(bootID string) *corev1.Node {
	n := readyNode(true)
	n.Status.NodeInfo.BootID = bootID
	return n
}

// P2: reboot Job 생성 직전에 node bootID 를 DUS status 에 persist 한다(persist-then-Job).
func TestHandleRebootRequired_PersistsBootID(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebootRequired, verNew, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(true), readyNodeBootID(bootIDPre))

	if _, _, err := sm.handleRebootRequired(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRebootRequired 실패: %v", err)
	}
	if dus.Status.RebootBootID != bootIDPre {
		t.Errorf("RebootBootID=%q, want boot-A (재부팅 전 캡처)", dus.Status.RebootBootID)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRebooting {
		t.Errorf("state=%q, want Rebooting", dus.Status.State)
	}
	var job batchv1.Job
	if err := sm.Get(context.Background(), types.NamespacedName{Name: naming.RebootJobName(jNode), Namespace: driverjob.Namespace}, &job); err != nil {
		t.Errorf("reboot Job 미생성: %v", err)
	}
}

// P2 premature-Ready 가드: Ready 이나 bootID 가 캡처값과 동일 → 전진 보류(Rebooting 유지).
func TestHandleRebooting_BootIDUnchanged_Waits(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebooting, verNew, verNew)
	dus.Status.RebootRequestedTime = metav1.Now()
	dus.Status.RebootBootID = bootIDPre
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(false), readyNodeBootID(bootIDPre))

	requeue, _, err := sm.handleRebooting(context.Background(), dus, dip)
	if err != nil {
		t.Fatalf("handleRebooting 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRebooting {
		t.Errorf("state=%q, want Rebooting (bootID 미변경 → premature-Ready 보류)", dus.Status.State)
	}
	if !requeue {
		t.Error("bootID 미변경인데 requeue=false")
	}
}

// P2 premature-Ready 가드: Ready + bootID 변경 관측 → Upgrading 전진.
func TestHandleRebooting_BootIDChanged_ToUpgrading(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebooting, verNew, verNew)
	dus.Status.RebootRequestedTime = metav1.Now()
	dus.Status.RebootBootID = bootIDPre
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(false), readyNodeBootID("boot-B"))

	if _, _, err := sm.handleRebooting(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRebooting 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateUpgrading {
		t.Errorf("state=%q, want Upgrading (bootID 변경 관측 후 전진)", dus.Status.State)
	}
}

// P2 fallback: bootID 빈 값(캡처 실패) → 기존 isNodeReady 로 보수 전진(교착 방지).
func TestHandleRebooting_EmptyBootID_Fallback(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebooting, verNew, verNew)
	dus.Status.RebootRequestedTime = metav1.Now()
	dus.Status.RebootBootID = "" // 캡처 실패 fallback
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(false), readyNodeBootID("boot-B"))

	if _, _, err := sm.handleRebooting(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRebooting 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateUpgrading {
		t.Errorf("state=%q, want Upgrading (빈 bootID fallback 은 isNodeReady 로 전진)", dus.Status.State)
	}
}

// needsReboot=true → handleRebootRequired 가 reboot Job 생성 + Rebooting 전이 + attempts=1.
func TestHandleRebootRequired_TriggersRebootJob(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebootRequired, verNew, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(true))

	if _, _, err := sm.handleRebootRequired(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRebootRequired 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRebooting {
		t.Errorf("state=%q, want Rebooting", dus.Status.State)
	}
	if dus.Status.RebootAttempts != 1 {
		t.Errorf("RebootAttempts=%d, want 1", dus.Status.RebootAttempts)
	}
	var job batchv1.Job
	if err := sm.Get(context.Background(), types.NamespacedName{Name: naming.RebootJobName(jNode), Namespace: driverjob.Namespace}, &job); err != nil {
		t.Errorf("reboot Job 미생성: %v", err)
	}
}

// RebootAttempts 가 MaxReboots(기본1) 도달 후 재요구 → Failed(무한 재부팅 방지).
func TestHandleRebootRequired_ExceedsMaxReboots_Failed(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebootRequired, verNew, verNew)
	dus.Status.RebootAttempts = 1 // 이미 1회(=maxReboots 기본)
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(true))

	if _, _, err := sm.handleRebootRequired(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRebootRequired 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateFailed {
		t.Errorf("state=%q, want Failed (MaxReboots 초과)", dus.Status.State)
	}
}

// kured executor 는 1차 미구현 → reboot Job 미생성, 상태 불변(대기).
func TestHandleRebootRequired_KuredUnsupported(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebootRequired, verNew, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Spec.RebootExecutor = "kured"
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(true))

	if _, _, err := sm.handleRebootRequired(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRebootRequired 실패: %v", err)
	}
	if dus.Status.State == v1alpha1.UpgradeStateRebooting {
		t.Error("kured 미구현인데 Rebooting 전이됨")
	}
	if dus.Status.RebootAttempts != 0 {
		t.Errorf("RebootAttempts=%d, want 0 (kured 미실행)", dus.Status.RebootAttempts)
	}
}

// 노드 Ready → Rebooting → Upgrading(재설치로 마커 소거·재수렴).
func TestHandleRebooting_Ready_ToUpgrading(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebooting, verNew, verNew)
	dus.Status.RebootRequestedTime = metav1.Now()
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(false), readyNode(true))

	if _, _, err := sm.handleRebooting(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRebooting 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateUpgrading {
		t.Errorf("state=%q, want Upgrading (재부팅 후 재설치로 마커 소거)", dus.Status.State)
	}
}

// 노드 NotReady(재부팅 중) → Rebooting 유지(대기 requeue).
func TestHandleRebooting_NotReady_Waits(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebooting, verNew, verNew)
	dus.Status.RebootRequestedTime = metav1.Now()
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(true), readyNode(false))

	requeue, _, err := sm.handleRebooting(context.Background(), dus, dip)
	if err != nil {
		t.Fatalf("handleRebooting 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRebooting {
		t.Errorf("state=%q, want Rebooting (NotReady 대기)", dus.Status.State)
	}
	if !requeue {
		t.Error("NotReady 인데 requeue=false")
	}
}

// rebootTimeout 초과 → Failed.
func TestHandleRebooting_Timeout_Failed(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRebooting, verNew, verNew)
	dus.Status.RebootRequestedTime = metav1.NewTime(time.Now().Add(-20 * time.Minute)) // 15m 초과
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(true), readyNode(true))

	if _, _, err := sm.handleRebooting(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRebooting 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateFailed {
		t.Errorf("state=%q, want Failed (rebootTimeout)", dus.Status.State)
	}
}

// 레이스 가드: 재부팅 후(RebootAttempts>=1) Validating 에서 needsReboot 가 stale-true 여도
// RebootRequired 로 재진입하지 않는다(NDR 지연 오판→MaxReboots Failed 방지). validator 체인이 결정.
func TestTransitionState_PostReboot_NoRetriggerOnStaleNDR(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verNew, verNew)
	dus.Status.RebootAttempts = 1 // 이미 1회 재부팅함
	dip := makeJobDIP(verNew, imgNew, true, true)
	// NDR 은 stale-true(마커 소거가 아직 NDR 에 미반영된 상태 모사) + install Job 완료.
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(true), readyNode(true), makeInstallJob(true, false))

	if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
		t.Fatalf("TransitionState 실패: %v", err)
	}
	if dus.Status.State == v1alpha1.UpgradeStateRebootRequired {
		t.Error("재부팅 후 stale needsReboot 로 RebootRequired 재진입됨 (레이스 가드 실패)")
	}
}

// 첫 검증(RebootAttempts==0)에서는 needsReboot=true 가 정상적으로 RebootRequired 를 트리거한다.
func TestTransitionState_FirstValidation_TriggersReboot(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateValidating, verNew, verNew)
	dip := makeJobDIP(verNew, imgNew, true, true)
	sm := newUpgradeSMWithRecorder(dus, dip, rebootNDR(true), readyNode(true), makeInstallJob(true, false))

	if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
		t.Fatalf("TransitionState 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRebootRequired {
		t.Errorf("state=%q, want RebootRequired (첫 검증 needsReboot 트리거)", dus.Status.State)
	}
}

// nodeNeedsReboot: NDR 필드 read + maxReboots 기본.
func TestNodeNeedsReboot_And_MaxReboots(t *testing.T) {
	sm := newUpgradeSMWithRecorder(rebootNDR(true))
	need, err := sm.nodeNeedsReboot(context.Background(), jNode, jVendor, jModel)
	if err != nil || !need {
		t.Errorf("nodeNeedsReboot=%v err=%v, want true", need, err)
	}
	if got := maxReboots(makeJobDIP(verNew, imgNew, true, true)); got != 1 {
		t.Errorf("maxReboots default=%d, want 1", got)
	}
}
