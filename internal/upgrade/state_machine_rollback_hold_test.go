// ============================================================
// state_machine_rollback_hold_test.go: 롤백 뒤 같은 목표 재시도 보류와 롤백 보고 시험
// 생성일: 2026-10-13 | 수정일: 2026-10-14
// ============================================================

package upgrade

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/metrics"
)

// heldGen 은 heldDUS 가 롤백을 기록한 시점의 정책 generation 이다.
const heldGen int64 = 3

// heldDUS 는 롤백을 끝내고 Idle 로 돌아온 DUS 다. cooldown 을 지나도록 전이 시각을 1분 전으로 둔다.
func heldDUS() *v1alpha1.DriverUpgradeState {
	dus := makeJobDUS(v1alpha1.UpgradeStateIdle, verOld, verNew)
	dus.Status.RolledBackVersion = verNew
	dus.Status.RolledBackGeneration = heldGen
	dus.Status.LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Minute))
	return dus
}

func TestRollbackHold_IdleDoesNotRetrySameTarget(t *testing.T) {
	dus := heldDUS()
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Generation = heldGen
	sm := newUpgradeSMWithRecorder(dus, dip, makeUncordonedNode(), makeNDRWithDriver(true, verOld))

	for i := 0; i < 2; i++ {
		if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
			t.Fatalf("TransitionState 실패: %v", err)
		}
	}
	if dus.Status.State != v1alpha1.UpgradeStateIdle {
		t.Fatalf("state=%q, want Idle — 같은 정책에서 이미 롤백한 목표로 사이클을 다시 열면 cordon·롤백이 반복된다", dus.Status.State)
	}
	if !strings.Contains(dus.Status.Message, "재시도 보류") {
		t.Errorf("message=%q — 보류 사유가 status 에 보이지 않는다", dus.Status.Message)
	}
	// 관리자 요청 롤백도 이 보류를 거치므로 원인을 설치 실패로 단정하지 않는다.
	if strings.Contains(dus.Status.Message, "설치 실패") {
		t.Errorf("message=%q — 롤백 원인을 설치 실패로 단정한다", dus.Status.Message)
	}
	// 경고는 메시지가 바뀔 때 한 번만 낸다(60초마다 쌓이지 않는다).
	n := 0
	for len(sm.Recorder.(*record.FakeRecorder).Events) > 0 {
		if strings.Contains(<-sm.Recorder.(*record.FakeRecorder).Events, "UpgradeHeldAfterRollback") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("UpgradeHeldAfterRollback 이벤트 %d건, want 1", n)
	}
	var jobs batchv1.JobList
	_ = sm.List(context.Background(), &jobs)
	if len(jobs.Items) != 0 {
		t.Errorf("install Job %d건 생성 — 보류 중에는 Job 을 만들지 않는다", len(jobs.Items))
	}
}

func TestRollbackHold_ReleasedOnGenerationChange(t *testing.T) {
	dus := heldDUS()
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Generation = heldGen + 1 // 정책 spec 변경
	sm := newUpgradeSMWithRecorder(dus, dip, makeUncordonedNode(), makeNDRWithDriver(true, verOld))

	if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
		t.Fatalf("TransitionState 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRequired {
		t.Errorf("state=%q, want UpgradeRequired — 정책이 바뀌면 한 번 다시 시도한다", dus.Status.State)
	}
	if dus.Status.RolledBackVersion != "" || dus.Status.RolledBackGeneration != 0 {
		t.Errorf("보류 필드가 남음: %q/%d", dus.Status.RolledBackVersion, dus.Status.RolledBackGeneration)
	}
}

func TestRollbackHold_ReleasedWhenTargetChanges(t *testing.T) {
	dus := heldDUS()
	dus.Status.DesiredVersion = verOld
	dip := makeJobDIP(verOld, imgOld, true, true) // 정책을 현재 버전으로 되돌림
	dip.Generation = heldGen + 1
	sm := newUpgradeSMWithRecorder(dus, dip, makeUncordonedNode(), makeNDRWithDriver(true, verOld))

	if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
		t.Fatalf("TransitionState 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateIdle || dus.Status.RolledBackVersion != "" {
		t.Errorf("state=%q rolledBack=%q, want Idle·빈 값", dus.Status.State, dus.Status.RolledBackVersion)
	}
	if !strings.Contains(dus.Status.Message, "보류 해제") {
		t.Errorf("message=%q — 보류 해제가 status 에 보이지 않는다", dus.Status.Message)
	}
	// 해제 원인은 정책 변경만이 아니다(versionSource=Host 에서는 적재된 호스트 버전이 목표를 바꾼다).
	if strings.Contains(dus.Status.Message, "(정책 변경)") || !strings.Contains(dus.Status.Message, "목표 또는 정책 generation 변경") {
		t.Errorf("message=%q — 해제 원인을 정책 변경으로만 적는다", dus.Status.Message)
	}
}

// 보류 중인 노드가 다른 경로(호스트에서 직접 설치 등)로 보류 목표에 도달하면 보류를 푼다.
// 풀지 않으면 desired 와 current 가 같은 노드가 보류로 계속 표시된다.
func TestRollbackHold_ReleasedWhenNodeReachesTarget(t *testing.T) {
	dus := heldDUS()
	dus.Status.CurrentVersion = verNew
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Generation = heldGen
	sm := newUpgradeSMWithRecorder(dus, dip, makeUncordonedNode(), makeNDRWithDriver(true, verNew))

	if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
		t.Fatalf("TransitionState 실패: %v", err)
	}
	if dus.Status.RolledBackVersion != "" || dus.Status.RolledBackGeneration != 0 {
		t.Errorf("보류 목표에 도달했는데 보류 필드가 남음: %q/%d", dus.Status.RolledBackVersion, dus.Status.RolledBackGeneration)
	}
	if want := "롤백 보류 해제(노드가 보류 목표 " + verNew + " 에 도달)"; dus.Status.Message != want {
		t.Errorf("message=%q, want %q", dus.Status.Message, want)
	}
	if dus.Status.State != v1alpha1.UpgradeStateIdle {
		t.Errorf("state=%q, want Idle", dus.Status.State)
	}
}

func TestRollbackHold_SelfHealStillRuns(t *testing.T) {
	dus := heldDUS()
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Generation = heldGen
	sm := newUpgradeSMWithRecorder(dus, dip, makeUncordonedNode(), makeNDRWithDriver(false, verOld))

	if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
		t.Fatalf("TransitionState 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateRequired {
		t.Errorf("state=%q, want UpgradeRequired — 드라이버가 내려간 노드는 보류와 무관하게 다시 설치한다", dus.Status.State)
	}
	if dus.Status.RolledBackVersion != "" {
		t.Errorf("self-heal 사이클 시작 뒤 보류 필드가 남음: %q", dus.Status.RolledBackVersion)
	}
}

func TestMarkRolledBack_RecordsTargetAndGeneration(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRollback, verNew, verNew)
	dus.Status.PreviousVersion = verOld
	dus.Status.PreviousImage = imgOld
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Generation = 7
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleRollbackJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRollbackJob 실패: %v", err)
	}
	if dus.Status.RolledBackVersion != verNew || dus.Status.RolledBackGeneration != 7 {
		t.Errorf("rolledBack=%q/%d, want %q/7", dus.Status.RolledBackVersion, dus.Status.RolledBackGeneration, verNew)
	}
}

func TestMarkRolledBack_SecondRollbackKeepsTarget(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRollback, verNew, verOld) // 롤백이 DesiredVersion 을 이전 버전으로 바꾼 뒤
	dus.Status.PreviousVersion = verOld
	dus.Status.PreviousImage = imgOld
	dus.Status.RolledBackVersion = verNew
	dus.Status.RolledBackGeneration = 7
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Generation = 7
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleRollbackJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRollbackJob 실패: %v", err)
	}
	if dus.Status.RolledBackVersion != verNew {
		t.Errorf("rolledBack=%q, want %q — 두 번째 롤백이 보류 대상을 이전 버전으로 덮으면 보류가 풀린다", dus.Status.RolledBackVersion, verNew)
	}
}

func TestHandleUncordoning_AfterRollback_ReportsRollback(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateUncordoning, verOld, verOld)
	dus.Status.RolledBackVersion = verNew
	sm := newUpgradeSMWithRecorder(dus, makeUncordonedNode())
	before := testutil.ToFloat64(metrics.UpgradeTotal.WithLabelValues(jVendor, "success"))

	if _, _, err := sm.handleUncordoning(context.Background(), dus); err != nil {
		t.Fatalf("handleUncordoning 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateIdle {
		t.Fatalf("state=%q, want Idle", dus.Status.State)
	}
	if !strings.Contains(dus.Status.Message, "롤백 완료") || strings.Contains(dus.Status.Message, "업그레이드 완료") {
		t.Errorf("message=%q — 롤백으로 끝난 사이클을 업그레이드 완료로 보고한다", dus.Status.Message)
	}
	if want := "롤백 완료: " + verNew + " 대신 " + verOld + " 로 복원"; dus.Status.Message != want {
		t.Errorf("message=%q, want %q — 관리자 요청 롤백도 거치므로 원인을 단정하지 않는다", dus.Status.Message, want)
	}
	if d := testutil.ToFloat64(metrics.UpgradeTotal.WithLabelValues(jVendor, "success")) - before; d != 0 {
		t.Errorf("success 지표 +%v, want 0", d)
	}
}

func TestHandleUncordoning_Success_CountsOnce(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateUncordoning, verNew, verNew)
	sm := newUpgradeSMWithRecorder(dus, makeUncordonedNode())
	before := testutil.ToFloat64(metrics.UpgradeTotal.WithLabelValues(jVendor, "success"))

	if _, _, err := sm.handleUncordoning(context.Background(), dus); err != nil {
		t.Fatalf("handleUncordoning 실패: %v", err)
	}
	if dus.Status.Message != "업그레이드 완료" {
		t.Errorf("message=%q, want 업그레이드 완료", dus.Status.Message)
	}
	if d := testutil.ToFloat64(metrics.UpgradeTotal.WithLabelValues(jVendor, "success")) - before; d != 1 {
		t.Errorf("success 지표 +%v, want 1 — 성공 한 번을 두 번 센다", d)
	}
}

func TestMarkRolledBack_ReentryKeepsFirstGeneration(t *testing.T) {
	dus := makeJobDUS(v1alpha1.UpgradeStateRollback, verNew, verNew)
	dus.Status.PreviousVersion = verOld
	dus.Status.PreviousImage = imgOld
	dus.Status.RolledBackVersion = verNew
	dus.Status.RolledBackGeneration = 7
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Generation = 8 // 롤백 재진입 사이에 정책 spec 이 바뀜
	sm := newUpgradeSMWithRecorder(dus, dip)

	if _, _, err := sm.handleRollbackJob(context.Background(), dus, dip); err != nil {
		t.Fatalf("handleRollbackJob 실패: %v", err)
	}
	if dus.Status.RolledBackGeneration != 7 {
		t.Errorf("rolledBackGeneration=%d, want 7 — 재진입이 롤백 시작 때 값을 덮으면 그 사이의 정책 변경이 보류를 풀지 못한다",
			dus.Status.RolledBackGeneration)
	}
}

// 데몬셋 모드 롤백도 이전 버전을 확인한 직후 보류 대상을 기록한다. 픽스처에 DaemonSet 이 없어
// 호출은 DS 조회 오류로 끝나지만 기록은 그 앞에서 일어나므로 반환 오류는 보지 않는다.
func TestMarkRolledBack_DaemonSetModeRecords(t *testing.T) {
	dus := makeRollbackDUS("k8s-worker1-nvidia", "k8s-worker1", "nvidia", 0)
	dip := makeRollbackDIP("nvidia-generic", "nvidia", 3)
	dip.Generation = 5
	sm := newUpgradeSMWithRecorder(dus, dip)

	_, _, _ = sm.handleRollback(context.Background(), dus, dip)
	if dus.Status.RolledBackVersion != "580.142" || dus.Status.RolledBackGeneration != 5 {
		t.Errorf("rolledBack=%q/%d, want 580.142/5 — 데몬셋 모드 롤백이 보류 대상을 기록하지 않는다",
			dus.Status.RolledBackVersion, dus.Status.RolledBackGeneration)
	}
}

// versionSource=Host 에서는 로드된 호스트 버전이 바뀌면 정책 generation 변경 없이 목표가 바뀐다.
// 목표 변경 하나만으로 보류가 풀려야 한다.
func TestReleaseRollbackHold_TargetChangeAloneReleases(t *testing.T) {
	dip := makeJobDIP(verNew, imgNew, true, true)
	dip.Generation = heldGen

	moved := heldDUS()
	releaseRollbackHold(moved, dip, verOld, verOld)
	if moved.Status.RolledBackVersion != "" || moved.Status.RolledBackGeneration != 0 {
		t.Errorf("목표만 바뀌었는데 보류가 남음: %q/%d", moved.Status.RolledBackVersion, moved.Status.RolledBackGeneration)
	}

	same := heldDUS()
	releaseRollbackHold(same, dip, verNew, verOld)
	if same.Status.RolledBackVersion != verNew || same.Status.RolledBackGeneration != heldGen {
		t.Errorf("같은 목표·같은 generation 인데 보류가 풀림: %q/%d", same.Status.RolledBackVersion, same.Status.RolledBackGeneration)
	}
}
