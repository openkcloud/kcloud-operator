// ============================================================
// capabilities_test.go: 노드 단위 capability 조회 테스트
// 상세: 후보에서 빠진 노드가 목록에서 사라지지 않고 "왜 빠졌는지" 를 달고 남는지 본다
//       (cordon / ACPP 미수렴). 미검증 capability 가 verified=false 로 나가는지도 재확인한다.
// 생성일: 2026-07-30 | 수정일: 2026-07-30
// ============================================================

package apiserver

import (
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kcloud-operator/api/v1alpha1"
)

func TestBuildNodeCapabilities_CordonedNodeStaysWithReason(t *testing.T) {
	nodes := []corev1.Node{node("worker1", true, map[string]string{"nvidia.com/gpu": "1"})}
	acpps := []v1alpha1.AcceleratorPartitionPolicy{
		acppReady([]v1alpha1.DeviceStatus{{ID: "GPU-aaa", Model: "A30"}}, v1alpha1.SharingModeExclusive, 0),
	}
	got := BuildNodeCapabilities(nodes, acpps, nil)
	if len(got) != 1 {
		t.Fatalf("cordon 된 노드도 목록에 남아야 함: got %d", len(got))
	}
	if got[0].Candidate {
		t.Fatalf("cordon 노드는 후보가 아니어야 함")
	}
	if got[0].CandidateReason == "" {
		t.Fatalf("후보에서 빠진 이유가 비어 있으면 사용자가 고칠 수 없다")
	}
	if got[0].Schedulable {
		t.Fatalf("Schedulable 은 false 여야 함")
	}
	if len(got[0].Devices) != 1 || got[0].Devices[0].UID != "GPU-aaa" {
		t.Fatalf("장치 capability 가 실려야 함: %#v", got[0].Devices)
	}
}

func TestBuildNodeCapabilities_UnconvergedPolicyIsNotCandidate(t *testing.T) {
	acpp := v1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 2},
		Spec:       v1alpha1.AcceleratorPartitionPolicySpec{Vendor: "nvidia"},
		Status: v1alpha1.AcceleratorPartitionPolicyStatus{
			ObservedGeneration: 1, // 아직 수렴 전
			Targets: []v1alpha1.TargetStatus{{
				NodeName: "worker1", Phase: v1alpha1.ACPPPhaseApplying,
				Devices: []v1alpha1.DeviceStatus{{ID: "GPU-aaa"}},
			}},
		},
	}
	nodes := []corev1.Node{node("worker1", false, map[string]string{"nvidia.com/gpu": "1"})}
	got := BuildNodeCapabilities(nodes, []v1alpha1.AcceleratorPartitionPolicy{acpp}, nil)
	if len(got) != 1 || got[0].Candidate {
		t.Fatalf("미수렴 정책이 붙은 노드는 후보가 아니어야 함: %#v", got)
	}
	if got[0].CandidateReason == "" {
		t.Fatalf("미수렴 사유가 있어야 함")
	}
}

func TestBuildNodeCapabilities_NoAcceleratorNodeIsExcludedWithReason(t *testing.T) {
	nodes := []corev1.Node{{
		ObjectMeta: metav1.ObjectMeta{Name: "master1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			"cpu": resource.MustParse("8"),
		}},
	}}
	got := BuildNodeCapabilities(nodes, nil, nil)
	if len(got) != 1 {
		t.Fatalf("가속기 없는 노드도 이유와 함께 남는다: got %d", len(got))
	}
	if got[0].Candidate || got[0].CandidateReason == "" {
		t.Fatalf("후보 아님 + 사유가 있어야 함: %#v", got[0])
	}
}

func TestCapabilities_Endpoint(t *testing.T) {
	s, _ := newServer(t, okAuth(), invObjects()...)
	w := do(t, s, "GET", "/api/v1/capabilities", "", "tok")
	if w.Code != http.StatusOK {
		t.Fatalf("200 이어야 함: got %d (%s)", w.Code, w.Body.String())
	}
	items, total, _, _ := decodePage[NodeCapabilityView](t, w.Body.Bytes())
	if total != 2 || len(items) != 2 {
		t.Fatalf("노드 2개여야 함: total=%d", total)
	}
}

// review I-2: /accelerators 의 TestAccelerators_DeniedWithoutNDRAuthz 와 대칭 — capabilities 도
// NDR 로 되짚은 값(memoryMiB/sharingReplicas)을 실으므로 nodedevicereports 권한도 필요하다.
func TestCapabilities_DeniedWithoutNDRAuthz(t *testing.T) {
	opts := authnOpts{
		authenticated: true, username: "sa:acpp-only", allowed: true,
		denyResources: map[string]bool{"nodedevicereports": true},
	}
	s, _ := newServer(t, opts, invObjects()...)
	if w := do(t, s, "GET", "/api/v1/capabilities", "", "tok"); w.Code != http.StatusForbidden {
		t.Fatalf("nodedevicereports 권한 없으면 403 이어야 함: got %d (%s)", w.Code, w.Body.String())
	}
}

// TestBuildNodeCapabilities_FailedSiblingDoesNotHideReadyPolicy — 라이브 결함 D-4(Task 5 §5.2):
// 사전순 앞의 Failed 정책("worker1-a2-timeslice")이 같은 노드의 Ready 정책(readySiblingPolicyName)을
// /capabilities 에서 통째로 가리고 노드 전체를 candidate:false 로 만들면 안 된다.
func TestBuildNodeCapabilities_FailedSiblingDoesNotHideReadyPolicy(t *testing.T) {
	failed := v1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1-a2-timeslice", Generation: 1},
		Spec:       v1alpha1.AcceleratorPartitionPolicySpec{Vendor: "nvidia"},
		Status: v1alpha1.AcceleratorPartitionPolicyStatus{
			ObservedGeneration: 1,
			Targets:            []v1alpha1.TargetStatus{{NodeName: "worker1", Phase: v1alpha1.ACPPPhaseFailed}},
		},
	}
	ready := acppReady([]v1alpha1.DeviceStatus{{ID: "PCI-0000:18:00.0", Model: "NVIDIA A30"}}, v1alpha1.SharingModeExclusive, 0)
	ready.Name = readySiblingPolicyName
	ready.Status.Targets[0].ResolvedLayout = []v1alpha1.ResolvedLayoutEntry{{Profile: "1g.6gb", ExpectedCountPerDevice: 4}}

	nodes := []corev1.Node{node("worker1", false, map[string]string{"nvidia.com/gpu": "1", "nvidia.com/mig-1g.6gb": "4"})}
	got := BuildNodeCapabilities(nodes, []v1alpha1.AcceleratorPartitionPolicy{failed, ready}, nil)
	if len(got) != 1 {
		t.Fatalf("rows %d", len(got))
	}
	if !got[0].Candidate {
		t.Fatalf("Ready 정책이 있는 노드가 후보에서 빠졌다: %q", got[0].CandidateReason)
	}
	if got[0].Policy != readySiblingPolicyName {
		t.Fatalf("Ready 정책이 노출돼야 한다: policy=%q", got[0].Policy)
	}
	if len(got[0].Devices) != 1 || got[0].Devices[0].UID != "PCI-0000:18:00.0" {
		t.Fatalf("Ready 정책의 장치가 실려야 한다: %#v", got[0].Devices)
	}
}
