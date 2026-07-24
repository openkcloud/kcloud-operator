// ============================================================
// state_machine_failed_recovered_test.go: 노드가 이미 정책을 만족하는데 Failed 로 남아 있던
//
//	상태의 탈출 경로 단위 테스트
//
// 상세: 라이브 결함(2026-08-07, `.91` k8s-worker3-tenstorrent) — current 와 desired 가 같고
//
//	드라이버도 로드돼 있는데 DUS 가 사흘째 Failed 로 남아 있었다. Failed 는 cordon 해제
//	말고는 아무것도 하지 않는 절대 터미널이라 나갈 길이 없었다. 할 일이 없는 상태에서
//	사람의 손을 기다리는 것은 상태를 잘못 표현하는 것이다.
//
// 경계: 탈출은 "노드가 이미 정책을 만족한다" 가 확인될 때만이다. 버전이 다르거나 드라이버가
//
//	빠져 있으면 Failed 로 남는다 — 그러지 않으면 Idle 이 설치를 다시 걸고 실패해
//	Failed 로 돌아오는 왕복이 생긴다.
//
// 생성일: 2026-08-07
// ============================================================

package upgrade

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "kcloud-operator/api/v1alpha1"
)

// makeFailedDUSWithVersions 는 버전 축을 채운 Failed DUS 를 반환한다.
func makeFailedDUSWithVersions(current, desired string) *v1alpha1.DriverUpgradeState {
	dus := makeFailedDUS(jVendor)
	dus.Status.CurrentVersion = current
	dus.Status.DesiredVersion = desired
	dus.Status.RollbackAttempts = 4
	return dus
}

// makeNDRWithDriver 는 그 벤더 장치 하나를 담은 NDR 을 반환한다.
func makeNDRWithDriver(loaded bool, version string) *v1alpha1.NodeDeviceReport {
	return &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Status: v1alpha1.NodeDeviceReportStatus{
			Devices: []v1alpha1.DeviceEntry{{
				Vendor:        jVendor,
				Model:         jModel,
				DriverVersion: version,
				DriverLoaded:  loaded,
			}},
		},
	}
}

// makeUncordonedNode 는 cordon 되지 않은 노드를 반환한다. 탈출 판정이 cordon 과 무관함을
// 드러내기 위해 이 파일의 기본 노드로 쓴다.
func makeUncordonedNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: jNode},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": qty(2)},
		},
	}
}

// ── 시험 1: 이미 정책을 만족 → Idle 로 탈출 ─────────────────────────────
// 라이브에서 실제로 막혀 있던 조합이다. 목표 버전이 이미 깔려 있고 드라이버도 로드돼 있으니
// 이 상태기계가 할 일은 없다. 할 일이 없으면 Idle 이다.
func TestHandleFailed_AlreadySatisfiesPolicy_ReturnsToIdle(t *testing.T) {
	dus := makeFailedDUSWithVersions(verNew, verNew)
	dip := makeCordonTestDIP()
	sm := newUpgradeSMWithRecorder(dus, dip, makeUncordonedNode(), makeNDRWithDriver(true, verNew))

	if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
		t.Fatalf("TransitionState 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateIdle {
		t.Errorf("state=%q, want Idle — 목표 버전이 이미 깔려 있고 드라이버도 로드된 상태에서 "+
			"Failed 로 남으면 사람이 손을 댈 것이 없는데도 손을 기다리게 된다", dus.Status.State)
	}
	if dus.Status.RollbackAttempts != 0 {
		t.Errorf("rollbackAttempts=%d, want 0 — 다음 사이클이 지난 실패의 카운터를 물려받으면 "+
			"첫 시도부터 한도에 가까워진다", dus.Status.RollbackAttempts)
	}
}

// ── 시험 2: 탈출하지 않아야 하는 조합 ───────────────────────────────────
// 안전 쪽 절반이다. 여기서 탈출시키면 Idle 이 설치를 다시 걸고, 같은 이유로 실패해 Failed 로
// 돌아온다. 그 왕복은 지금의 고착보다 나쁘다 — 노드를 계속 건드리기 때문이다.
func TestHandleFailed_NotSatisfied_StaysFailed(t *testing.T) {
	cases := []struct {
		name    string
		current string
		desired string
		loaded  bool
		why     string
	}{
		{
			name: "버전 불일치(다운그레이드 요구)", current: verNew, desired: verOld, loaded: true,
			why: "정책이 더 낮은 버전을 요구한다 — 설치를 다시 걸어도 같은 이유로 실패한다",
		},
		{
			name: "버전 불일치(업그레이드 미완)", current: verOld, desired: verNew, loaded: true,
			why: "아직 목표에 도달하지 않았다",
		},
		{
			name: "버전은 같으나 드라이버 미로드", current: verNew, desired: verNew, loaded: false,
			why: "버전 문자열만 같고 모듈이 빠져 있다 — 정책을 만족한 것이 아니다",
		},
		{
			name: "목표 버전 미지정", current: verNew, desired: "", loaded: true,
			why: "비교할 목표가 없으면 만족 여부를 판정할 수 없다",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dus := makeFailedDUSWithVersions(tc.current, tc.desired)
			dip := makeCordonTestDIP()
			sm := newUpgradeSMWithRecorder(dus, dip, makeUncordonedNode(),
				makeNDRWithDriver(tc.loaded, tc.current))

			if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
				t.Fatalf("TransitionState 실패: %v", err)
			}
			if dus.Status.State != v1alpha1.UpgradeStateFailed {
				t.Errorf("state=%q, want Failed — %s", dus.Status.State, tc.why)
			}
		})
	}
}

// ── 시험 3: NDR 이 없으면 탈출하지 않는다 ────────────────────────────────
// 드라이버 로드 여부를 확인할 근거가 없다. 근거가 없는 것을 만족으로 읽으면 안 된다.
func TestHandleFailed_NoDeviceReport_StaysFailed(t *testing.T) {
	dus := makeFailedDUSWithVersions(verNew, verNew)
	dip := makeCordonTestDIP()
	sm := newUpgradeSMWithRecorder(dus, dip, makeUncordonedNode()) // NDR 없음

	if _, _, err := sm.TransitionState(context.Background(), dus, dip); err != nil {
		t.Fatalf("TransitionState 실패: %v", err)
	}
	if dus.Status.State != v1alpha1.UpgradeStateFailed {
		t.Errorf("state=%q, want Failed — NDR 이 없으면 드라이버 로드 여부를 확인할 수 없다",
			dus.Status.State)
	}
}
