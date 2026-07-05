// ============================================================
// translate_test.go: 추상 의도 → 벤더 리소스 번역 테스트
// 상세: 벤더 우선순위, DRA 거절, 축을 지목하는 오류 전파, 노드 후보 확정.
// 생성일: 2026-07-30 | 수정일: 2026-07-30
// ============================================================
package intent

import (
	"errors"
	"strings"
	"testing"

	"kcloud-operator/api/v1alpha1"
)

const nodeK8sWorker1 = "k8s-worker1"

const (
	vendorNvidia  = "nvidia"
	vendorFuriosa = "furiosa"
	nodeWorker1   = "worker1"
	nodeRngd1     = "rngd-1"
	resRngd       = "furiosa.ai/rngd"
)

func classFor(mappings ...v1alpha1.AcceleratorMapping) *v1alpha1.AcceleratorClass {
	c := &v1alpha1.AcceleratorClass{}
	c.Name = "inference-medium"
	c.Spec.Mappings = mappings
	return c
}

func plainNvidiaNode(name string) NodeCapability {
	return NodeCapability{NodeName: name, Vendor: vendorNvidia, Advertised: map[string]int32{"nvidia.com/gpu": 1},
		SharingMode: v1alpha1.SharingModeExclusive}
}

func plainRngdNode(name string) NodeCapability {
	return NodeCapability{NodeName: name, Vendor: vendorFuriosa, Advertised: map[string]int32{resRngd: 1},
		SharingMode: v1alpha1.SharingModeExclusive}
}

func TestTranslateExclusive(t *testing.T) {
	req := Request{ClassName: "inference-medium", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	res, err := Translate(req, []NodeCapability{plainNvidiaNode("worker2"), plainNvidiaNode(nodeWorker1)})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.ResourceName != "nvidia.com/gpu" || res.Quantity != 1 || res.AllocationAPI != v1alpha1.AllocationAPIDevicePlugin {
		t.Fatalf("unexpected result %+v", res)
	}
	if len(res.Nodes) != 2 || res.Nodes[0] != nodeWorker1 || res.Nodes[1] != "worker2" {
		t.Fatalf("candidate nodes must be sorted: %v", res.Nodes)
	}
}

// 선호 벤더가 먼저 시도되고, 후보 노드가 생기는 첫 벤더가 이긴다.
func TestTranslateVendorPreferenceWins(t *testing.T) {
	mappings := []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}, rngdMapping()}
	snap := []NodeCapability{plainNvidiaNode(nodeWorker1), plainRngdNode(nodeRngd1)}

	req := Request{ClassName: "c", Mappings: mappings, Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive},
		AllocationAPI: v1alpha1.AllocationAPIAuto}
	res, err := Translate(req, snap)
	if err != nil || res.Vendor != vendorNvidia {
		t.Fatalf("mapping order must decide by default: %+v %v", res, err)
	}

	req.VendorPreference = []string{vendorFuriosa}
	res, err = Translate(req, snap)
	if err != nil || res.Vendor != vendorFuriosa || res.ResourceName != resRngd {
		t.Fatalf("preference ignored: %+v %v", res, err)
	}

	// 선호에 없는 벤더도 후보로 남는다(선호는 요구가 아니다).
	req.VendorPreference = []string{"rebellions"}
	res, err = Translate(req, snap)
	if err != nil || res.Vendor != vendorNvidia {
		t.Fatalf("unmapped preference must fall through to the class order: %+v %v", res, err)
	}
}

// 매핑된 벤더가 노드에 없으면 다음 벤더로 넘어간다(선호가 아니라 후보 유무가 결정한다).
func TestTranslateFallsThroughToVendorThatHasNodes(t *testing.T) {
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}, rngdMapping()},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	res, err := Translate(req, []NodeCapability{plainRngdNode(nodeRngd1)})
	if err != nil || res.Vendor != vendorFuriosa || res.ResourceName != resRngd {
		t.Fatalf("second mapping must win when the first has no node: %+v %v", res, err)
	}
}

func TestTranslateRejectsDRA(t *testing.T) {
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIDRA}
	_, err := Translate(req, []NodeCapability{plainNvidiaNode(nodeWorker1)})
	var rj *Reject
	if !errors.As(err, &rj) {
		t.Fatalf("expected *Reject, got %v", err)
	}
	if rj.Axis != AxisAllocationAPI || rj.Reason != v1alpha1.AWReasonDRANotEnabled {
		t.Fatalf("unexpected reject %+v", rj)
	}

	// dra 사유는 dra 에만 붙는다 — 모르는 값까지 "DRA 가 꺼져 있다" 로 설명하지 않는다.
	req.AllocationAPI = "acme-api"
	_, err = Translate(req, []NodeCapability{plainNvidiaNode(nodeWorker1)})
	if !errors.As(err, &rj) || rj.Axis != AxisAllocationAPI || rj.Reason != v1alpha1.AWReasonBackendUnsupported {
		t.Fatalf("unknown allocationAPI must not borrow the dra reason: %v", err)
	}
}

// 후보가 없을 때는 "노드가 없다" 가 아니라 실패한 축이 나와야 한다.
func TestTranslatePropagatesTheAxisThatFailed(t *testing.T) {
	shared := NodeCapability{NodeName: nodeRngd1, Vendor: vendorFuriosa, Advertised: map[string]int32{resRngd: 1},
		Devices: []v1alpha1.DeviceStatus{rngdDevice()}, SharingMode: v1alpha1.SharingModeExclusive}
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{rngdMapping()},
		Access:        v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeShared, Implementation: v1alpha1.ImplementationAuto, Replicas: 2},
		AllocationAPI: v1alpha1.AllocationAPIAuto}
	_, err := Translate(req, []NodeCapability{shared})
	var rj *Reject
	if !errors.As(err, &rj) {
		t.Fatalf("expected *Reject, got %v", err)
	}
	if rj.Axis != AxisMode || rj.Reason != v1alpha1.AWReasonBackendUnsupported {
		t.Fatalf("axis lost: %+v", rj)
	}
}

// requirements 게이트도 반드시 돈다 — mode 통과가 승인이 아니다.
func TestTranslateRunsTheRequirementsGateToo(t *testing.T) {
	nc := plainNvidiaNode(nodeWorker1)
	nc.Devices = []v1alpha1.DeviceStatus{{ID: "GPU-0",
		IsolationCapability: v1alpha1.IsolationCapability{Compute: v1alpha1.IsolationProcess, Memory: v1alpha1.IsolationProcess}}}
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}},
		Access:        v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive},
		Requirements:  v1alpha1.AcceleratorRequirements{MinimumIsolation: v1alpha1.IsolationHardware},
		AllocationAPI: v1alpha1.AllocationAPIAuto}
	_, err := Translate(req, []NodeCapability{nc})
	var rj *Reject
	if !errors.As(err, &rj) || rj.Axis != AxisIsolation || rj.Reason != v1alpha1.AWReasonIsolationTooWeak {
		t.Fatalf("requirements gate skipped: %v", err)
	}
}

// 모호한 furiosa product 는 조용히 아무 제품이나 고르지 않고 그 축을 지목해 거절한다.
func TestTranslateRejectsAmbiguousProduct(t *testing.T) {
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorFuriosa}},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	_, err := Translate(req, []NodeCapability{plainRngdNode(nodeRngd1)})
	var rj *Reject
	if !errors.As(err, &rj) || rj.Axis != AxisProduct || rj.Reason != v1alpha1.AWReasonNoVendorMapping {
		t.Fatalf("ambiguous product must name its axis: %v", err)
	}
}

// 카탈로그에 없는 벤더는 "노드가 없다" 가 아니라 클래스를 지목해야 한다.
func TestTranslateUnknownVendorNamesTheClass(t *testing.T) {
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: "acme"}},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	_, err := Translate(req, []NodeCapability{plainNvidiaNode(nodeWorker1)})
	var rj *Reject
	if !errors.As(err, &rj) || rj.Axis != AxisProduct || rj.Reason != v1alpha1.AWReasonNoVendorMapping {
		t.Fatalf("unknown vendor must point at the class: %v", err)
	}
}

// 같은 벤더가 두 번 매핑되면 선택이 비결정적이 된다 — webhook 이 이미 이걸 막지만
// failurePolicy=Ignore 라 webhook 이 죽어도 Translate 스스로 걸러야 한다(D1).
func TestTranslateRejectsDuplicateVendor(t *testing.T) {
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}, {Vendor: "NVIDIA"}},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	_, err := Translate(req, []NodeCapability{plainNvidiaNode(nodeWorker1)})
	var rj *Reject
	if !errors.As(err, &rj) || rj.Axis != AxisCandidates || rj.Reason != v1alpha1.AWReasonNoVendorMapping {
		t.Fatalf("duplicate vendor must be rejected by axis/reason: %v", err)
	}
}

func TestTranslateNoMappingAtAll(t *testing.T) {
	req := Request{ClassName: "c", Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	_, err := Translate(req, nil)
	var rj *Reject
	if !errors.As(err, &rj) || rj.Axis != AxisCandidates || rj.Reason != v1alpha1.AWReasonNoVendorMapping {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestTranslateNoNodesAtAll(t *testing.T) {
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	_, err := Translate(req, nil)
	var rj *Reject
	if !errors.As(err, &rj) || rj.Axis != AxisCandidates || rj.Reason != v1alpha1.AWReasonNoCandidateNodes {
		t.Fatalf("unexpected error %v", err)
	}
}

// 공유 결과 설명은 replica 사이에 격리가 없다는 사실을 반드시 담는다(정직성).
func TestTranslateSharedExplanationIsHonest(t *testing.T) {
	nc := NodeCapability{NodeName: nodeWorker1, Vendor: vendorNvidia, Advertised: map[string]int32{"nvidia.com/gpu": 8},
		Devices: []v1alpha1.DeviceStatus{verifiedTimeSlicing(16)}, SharingMode: v1alpha1.SharingModeTimeSliced, SharingReplicas: 4}
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeShared, Replicas: 4}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	res, err := Translate(req, []NodeCapability{nc})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !strings.Contains(res.Explanation, "not isolated") {
		t.Fatalf("explanation hides the lack of isolation: %q", res.Explanation)
	}
	// 시분할 replica 를 독립 장치로 부풀리지 않는다 — 사용자가 받는 것은 요청 1개다.
	if res.Quantity != 1 {
		t.Fatalf("quantity must be the request the user actually gets, got %d", res.Quantity)
	}
}

// 멀티벤더 노드에서도 편의 필드 Vendor 가 아니라 광고 목록으로 벤더를 판정한다.
func TestTranslateMultiVendorNode(t *testing.T) {
	nc := NodeCapability{NodeName: "mixed-1", Vendor: vendorFuriosa,
		Advertised:  map[string]int32{resRngd: 1, "nvidia.com/gpu": 2},
		SharingMode: v1alpha1.SharingModeExclusive}
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{{Vendor: vendorNvidia}},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	res, err := Translate(req, []NodeCapability{nc})
	if err != nil || res.Vendor != vendorNvidia || len(res.Nodes) != 1 {
		t.Fatalf("multi-vendor node lost: %+v %v", res, err)
	}
}

// D-2 라이브 재현: k8s-worker1(Warboy — 같은 furiosa 벤더지만 furiosa.ai/rngd 은 광고하지 않음)과
// rngd-1(RNGD, 물리 1대인데 4개 광고 — oversubscribed, D-1 이후 CheckMode 가 exclusive 를 거절함)이
// 함께 있을 때, exclusive RNGD 요청의 거절은 rngd-1 의 실제 사유여야 한다. 벤더만 보는 낡은 후보
// 필터는 worker1 도 후보로 들여 그 사소한 "광고 안 함" 이 rngd-1 의 진짜 거절을 가린다.
// 두 순서 모두 확인한다 — BuildSnapshot 은 이름 사전순으로 정렬해 "k8s-worker1" 이 항상 먼저
// 오지만("k8s-worker1" < "rngd-1"), 고침이 반복 순서가 아니라 후보 판정 자체를 고쳐야 한다는
// 것을 보이기 위해 반대 순서도 함께 돈다.
func TestTranslateRNGDExclusiveReportsRealRejectionNotWrongNode(t *testing.T) {
	warboy := NodeCapability{NodeName: nodeK8sWorker1, Vendor: vendorFuriosa,
		Advertised: map[string]int32{"beta.furiosa.ai/npu": 1}, SharingMode: v1alpha1.SharingModeExclusive}
	rngd := NodeCapability{NodeName: nodeRngd1, Vendor: vendorFuriosa, Advertised: map[string]int32{resRngd: 4},
		SharingMode: v1alpha1.SharingModeOversubscribed, SharingReplicas: 4}
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{rngdMapping()},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModeExclusive}, AllocationAPI: v1alpha1.AllocationAPIAuto}

	for _, order := range [][]NodeCapability{{warboy, rngd}, {rngd, warboy}} {
		_, err := Translate(req, order)
		var rj *Reject
		if !errors.As(err, &rj) {
			t.Fatalf("expected *Reject, got %v", err)
		}
		if strings.Contains(rj.Message, nodeK8sWorker1) {
			t.Fatalf("wrong node reported — rngd-1's real rejection was masked by k8s-worker1's trivial one: %+v", rj)
		}
		if !strings.Contains(rj.Message, nodeRngd1) {
			t.Fatalf("rejection must name the real candidate node rngd-1: %+v", rj)
		}
	}
}

// 파티션 미적용 상태(전체 장치 리소스만 광고)에서도 partitioned 요청은 후보에서 통째로 빠지지
// 않고 ProfileNotApplied 로 거절되어야 한다 — 벤더만 보고 거르는 옛 필터를 리소스 기준으로
// 좁히면서, "아직 적용 안 됨" 을 "광고 안 함" 으로 뭉개는 회귀가 없는지 함께 지킨다.
func TestTranslatePartitionedPendingNodeStaysACandidate(t *testing.T) {
	nc := plainNvidiaNode(nodeWorker1) // nvidia.com/gpu 만 광고, mig-1g.6gb 는 아직 없음(미적용)
	req := Request{ClassName: "c", Mappings: []v1alpha1.AcceleratorMapping{nvidiaMapping()},
		Access: v1alpha1.AccessSpec{Mode: v1alpha1.AccessModePartitioned}, AllocationAPI: v1alpha1.AllocationAPIAuto}
	_, err := Translate(req, []NodeCapability{nc})
	var rj *Reject
	if !errors.As(err, &rj) || rj.Axis != AxisProfile || rj.Reason != v1alpha1.AWReasonProfileNotApplied {
		t.Fatalf("pending-partition node dropped instead of reporting ProfileNotApplied: %v", err)
	}
}

func TestBuildRequestMergesClassAndWorkload(t *testing.T) {
	class := classFor(v1alpha1.AcceleratorMapping{Vendor: vendorNvidia, NativeProfile: "1g.6gb"})
	class.Spec.Requirements.MinimumIsolation = v1alpha1.IsolationProcess
	aw := &v1alpha1.AcceleratorWorkload{Spec: v1alpha1.AcceleratorWorkloadSpec{
		Accelerator: v1alpha1.AcceleratorRequest{
			Class:        "inference-medium",
			Access:       v1alpha1.AccessSpec{Mode: v1alpha1.AccessModePartitioned},
			Requirements: &v1alpha1.AcceleratorRequirements{MinimumIsolation: v1alpha1.IsolationHardware},
			Preferences:  &v1alpha1.AcceleratorPreferences{Vendors: []string{vendorFuriosa}, AllocationAPI: v1alpha1.AllocationAPIDevicePlugin},
		},
	}}
	req := BuildRequest(aw, class)
	if req.Requirements.MinimumIsolation != v1alpha1.IsolationHardware {
		t.Fatalf("stricter workload requirement lost: %+v", req.Requirements)
	}
	if len(req.VendorPreference) != 1 || req.VendorPreference[0] != vendorFuriosa {
		t.Fatalf("preference lost: %v", req.VendorPreference)
	}
	if req.AllocationAPI != v1alpha1.AllocationAPIDevicePlugin || req.ClassName != "inference-medium" {
		t.Fatalf("unexpected request %+v", req)
	}
	// preferences 가 없으면 auto 로 채운다.
	aw.Spec.Accelerator.Preferences = nil
	if got := BuildRequest(aw, class).AllocationAPI; got != v1alpha1.AllocationAPIAuto {
		t.Fatalf("default allocationAPI=%q want auto", got)
	}
}
