// ============================================================
// inventory_test.go: 인벤토리 집약(BuildInventory) 단위 테스트
// 상세: 정직성 불변식 세 개를 못박는다 — (1) time-slicing replica 는 행이 되지 않는다,
//       (2) supported 주장만으로 Verified 가 켜지지 않는다, (3) stale/미수렴 데이터는 숨기지
//       않는다(미수렴 ACPP 는 Stale=true 로 드러나고, 아직 장치를 보고하지 않은 관리 노드는
//       미관리로 오분류되지 않는다). 더불어 ACPP 미관리 노드의 합성 ID·출처 표기, cordon 노드
//       포함 여부, 멀티벤더 노드의 장치별 벤더 매칭을 본다.
// 생성일: 2026-07-30 | 수정일: 2026-07-30
// ============================================================

package apiserver

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kcloud-operator/api/v1alpha1"
)

func node(name string, cordoned bool, alloc map[string]string) corev1.Node {
	n := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{}},
	}
	for k, v := range alloc {
		n.Status.Allocatable[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return n
}

func ndr(nodeName string, entries ...v1alpha1.DeviceEntry) v1alpha1.NodeDeviceReport {
	return v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: nodeName},
		Status:     v1alpha1.NodeDeviceReportStatus{Devices: entries},
	}
}

// acppReady 는 수렴 완료(observedGeneration==generation, phase=Ready)한 정책이다. 정책/노드
// 이름은 이 테스트 파일에서 항상 "p"/"worker1" 하나뿐이라(unparam) 파라미터로 두지 않고 고정한다.
// readySiblingPolicyName 은 D-4 재현 픽스처의 Ready 정책명이다(라이브 실측명, "a2" < "a30" 사전순).
const readySiblingPolicyName = "worker1-a30-4x1g"

func acppReady(devices []v1alpha1.DeviceStatus, sharingMode string, replicas int32) v1alpha1.AcceleratorPartitionPolicy {
	return v1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 1},
		Spec:       v1alpha1.AcceleratorPartitionPolicySpec{Vendor: "nvidia"},
		Status: v1alpha1.AcceleratorPartitionPolicyStatus{
			ObservedGeneration: 1,
			Targets: []v1alpha1.TargetStatus{{
				NodeName: "worker1", Phase: v1alpha1.ACPPPhaseReady, Devices: devices,
			}},
			ApplyRecords: []v1alpha1.ApplyRecord{{
				NodeName: "worker1", SharingMode: sharingMode, SharingReplicas: replicas,
			}},
		},
	}
}

// TestBuildInventory_TimeSlicedReplicaIsNotADeviceRow: 정직성 계약 (2).
func TestBuildInventory_TimeSlicedReplicaIsNotADeviceRow(t *testing.T) {
	devs := []v1alpha1.DeviceStatus{{ID: "GPU-aaa", Model: "A30"}}
	// 물리 1장을 4배로 광고 중인 노드.
	nodes := []corev1.Node{node("worker1", false, map[string]string{"nvidia.com/gpu": "4"})}
	acpps := []v1alpha1.AcceleratorPartitionPolicy{acppReady(devs, v1alpha1.SharingModeTimeSliced, 4)}
	ndrs := []v1alpha1.NodeDeviceReport{ndr("worker1", v1alpha1.DeviceEntry{Vendor: "nvidia", Model: "A30", Count: 1})}

	got := BuildInventory(nodes, acpps, ndrs)
	if len(got) != 1 {
		t.Fatalf("물리 장치 1개 = 1행이어야 함(replica 는 행이 아니다): got %d rows", len(got))
	}
	if got[0].SharingMode != v1alpha1.SharingModeTimeSliced || got[0].SharingReplicas != 4 {
		t.Fatalf("복제 사실은 필드로만 나타나야 함: mode=%q replicas=%d", got[0].SharingMode, got[0].SharingReplicas)
	}
	if got[0].NodeAdvertised["nvidia.com/gpu"] != 4 {
		t.Fatalf("노드 광고량은 그대로 보고해야 함: %v", got[0].NodeAdvertised)
	}
}

// TestBuildInventory_UnverifiedSharingIsNotVerified: 정직성 계약 (1).
func TestBuildInventory_UnverifiedSharingIsNotVerified(t *testing.T) {
	devs := []v1alpha1.DeviceStatus{{
		ID: "GPU-aaa", Model: "A30",
		SharingCapability: v1alpha1.SharingCapability{
			// supported 라고 주장하지만 실측 전이다.
			MultiProcess: v1alpha1.SharingModeSupport{Supported: true, Verification: v1alpha1.VerificationRequired},
			TimeSlicing:  v1alpha1.SharingModeSupport{Supported: true, Verification: v1alpha1.VerificationVerified, MaxReplicas: 8},
		},
	}}
	nodes := []corev1.Node{node("worker1", false, map[string]string{"nvidia.com/gpu": "1"})}
	acpps := []v1alpha1.AcceleratorPartitionPolicy{acppReady(devs, v1alpha1.SharingModeExclusive, 0)}

	got := BuildInventory(nodes, acpps, nil)
	if len(got) != 1 {
		t.Fatalf("행 1개여야 함: got %d", len(got))
	}
	if got[0].Capability.Sharing.MultiProcess.Verified {
		t.Fatalf("verification=required 는 Verified 가 false 여야 함")
	}
	if !got[0].Capability.Sharing.MultiProcess.Supported {
		t.Fatalf("원본 주장(Supported)은 지워지지 않고 그대로 보고되어야 함")
	}
	if !got[0].Capability.Sharing.TimeSlicing.Verified {
		t.Fatalf("verification=verified 는 Verified 가 true 여야 함")
	}
}

// TestBuildInventory_UnmanagedNodeUsesSyntheticID: ACPP 가 없는 노드도 보이되 출처를 밝힌다.
func TestBuildInventory_UnmanagedNodeUsesSyntheticID(t *testing.T) {
	nodes := []corev1.Node{node("rngd-1", false, map[string]string{"furiosa.ai/rngd": "2"})}
	ndrs := []v1alpha1.NodeDeviceReport{ndr("rngd-1",
		v1alpha1.DeviceEntry{Vendor: "furiosa", Model: "rngd", Count: 2, MemoryMiB: 49152})}

	got := BuildInventory(nodes, nil, ndrs)
	if len(got) != 2 {
		t.Fatalf("NDR count=2 는 2행이어야 함: got %d", len(got))
	}
	for i := range got {
		if got[i].Source != SourceNDR {
			t.Fatalf("출처가 NDR 로 표기되어야 함: %q", got[i].Source)
		}
		if got[i].UID == "" {
			t.Fatalf("합성 ID 라도 비어 있으면 안 됨")
		}
	}
	if got[0].UID == got[1].UID {
		t.Fatalf("합성 ID 가 중복됨: %q", got[0].UID)
	}
}

// TestBuildInventory_CordonedNodeStillListed: cordon 은 스케줄 판정이지 장치가 사라진 게 아니다.
func TestBuildInventory_CordonedNodeStillListed(t *testing.T) {
	devs := []v1alpha1.DeviceStatus{{ID: "GPU-bbb", Model: "A2"}}
	nodes := []corev1.Node{node("worker1", true, map[string]string{"nvidia.com/gpu": "1"})}
	acpps := []v1alpha1.AcceleratorPartitionPolicy{acppReady(devs, v1alpha1.SharingModeExclusive, 0)}

	got := BuildInventory(nodes, acpps, nil)
	if len(got) != 1 {
		t.Fatalf("cordon 된 노드의 장치도 목록에 있어야 함: got %d", len(got))
	}
	if got[0].Schedulable {
		t.Fatalf("Schedulable 은 false 로 정직하게 보고해야 함")
	}
	if got[0].Source != SourceACPP || got[0].UID != "GPU-bbb" {
		t.Fatalf("ACPP canonical ID 를 써야 함: source=%q uid=%q", got[0].Source, got[0].UID)
	}
}

// TestBuildInventory_StaleACPPRowIsVisiblyStale: 정직성 계약 (3) 전반부 — 미수렴은 숨기지 않는다.
// spec 이 바뀌어 generation 은 올랐지만(2) 컨트롤러가 아직 반영 못한(observedGeneration=1) 경우,
// status.targets[].phase 에는 낡은 "Ready" 값이 그대로 남아 있다 — 이 낡은 phase/devices 를
// 그대로 믿으면 안 된다는 리뷰 Critical #1 시나리오를 그대로 재현한다.
func TestBuildInventory_StaleACPPRowIsVisiblyStale(t *testing.T) {
	devs := []v1alpha1.DeviceStatus{{ID: "GPU-aaa", Model: "A30"}}
	nodes := []corev1.Node{node("worker1", false, map[string]string{"nvidia.com/gpu": "1"})}
	acpp := acppReady(devs, v1alpha1.SharingModeExclusive, 0)
	acpp.Generation = 2 // observedGeneration 은 acppReady 가 1로 고정 — 재조정 전.
	acpps := []v1alpha1.AcceleratorPartitionPolicy{acpp}

	got := BuildInventory(nodes, acpps, nil)
	if len(got) != 1 {
		t.Fatalf("stale 노드도 1행은 보여야 함: got %d", len(got))
	}
	if !got[0].Stale {
		t.Fatalf("generation 불일치는 Stale=true 로 나와야 함")
	}
	if got[0].StaleReason == "" {
		t.Fatalf("StaleReason 이 비어 있으면 안 됨")
	}
	if got[0].Source != SourceACPP {
		t.Fatalf("stale 여도 관리 주체는 ACPP 로 남아야 함(미관리로 오분류 금지): got %q", got[0].Source)
	}
}

// TestBuildInventory_PendingACPPNodeNotLabeledUnmanaged: 정직성 계약 (3) 후반부 — 리뷰 Critical #2.
// ACPP 가 노드를 타깃하지만 아직 장치를 하나도 보고하지 않은(Phase=Applying) 상태다. 이걸 NDR
// 미관리 경로로 떨어뜨리면 "관리 중" 과 "관리 안 함" 이 API 상 구분 불가능해진다.
func TestBuildInventory_PendingACPPNodeNotLabeledUnmanaged(t *testing.T) {
	nodes := []corev1.Node{node("worker1", false, map[string]string{"nvidia.com/gpu": "1"})}
	acpp := v1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 1},
		Spec:       v1alpha1.AcceleratorPartitionPolicySpec{Vendor: "nvidia"},
		Status: v1alpha1.AcceleratorPartitionPolicyStatus{
			ObservedGeneration: 1,
			Targets:            []v1alpha1.TargetStatus{{NodeName: "worker1", Phase: v1alpha1.ACPPPhaseApplying}},
		},
	}

	got := BuildInventory(nodes, []v1alpha1.AcceleratorPartitionPolicy{acpp}, nil)
	if len(got) != 1 {
		t.Fatalf("진행 중인 관리 노드도 1행은 보여야 함: got %d", len(got))
	}
	if got[0].Source != SourceACPP {
		t.Fatalf("아직 보고 전이라도 미관리(NDR)로 분류하면 안 됨: got %q", got[0].Source)
	}
	if !got[0].Pending {
		t.Fatalf("장치 미보고 상태는 Pending=true 로 드러나야 함(UID 파싱 없이도 식별 가능해야 함)")
	}
	if got[0].UID != "" {
		t.Fatalf("pending 행은 실제 장치 UID 가 없어야 함: got %q", got[0].UID)
	}
	if got[0].Policy != "p" || got[0].Phase != v1alpha1.ACPPPhaseApplying {
		t.Fatalf("policy/phase 는 버려지면 안 됨: policy=%q phase=%q", got[0].Policy, got[0].Phase)
	}
}

// TestBuildInventory_MultiVendorNodeDeviceVendorMatchesModel: 리뷰 Important #3 — worker1 재현.
// NVIDIA A30/A2 + Furiosa 리소스가 같은 노드에 공존하면 리소스명 사전순(furiosa < nvidia)으로
// nc.Vendor 가 "furiosa" 가 되므로, 장치별 벤더는 반드시 모델 매칭으로 되짚어야 한다.
func TestBuildInventory_MultiVendorNodeDeviceVendorMatchesModel(t *testing.T) {
	devs := []v1alpha1.DeviceStatus{
		{ID: "GPU-a30", Model: "A30"},
		{ID: "GPU-a2", Model: "A2"},
	}
	nodes := []corev1.Node{node("worker1", false, map[string]string{
		"furiosa.ai/rngd": "1",
		"nvidia.com/gpu":  "2",
	})}
	acpps := []v1alpha1.AcceleratorPartitionPolicy{acppReady(devs, v1alpha1.SharingModeExclusive, 0)}
	ndrs := []v1alpha1.NodeDeviceReport{ndr("worker1",
		v1alpha1.DeviceEntry{Vendor: "nvidia", Model: "A30", Count: 1},
		v1alpha1.DeviceEntry{Vendor: "nvidia", Model: "A2", Count: 1},
		v1alpha1.DeviceEntry{Vendor: "furiosa", Model: "rngd", Count: 1},
	)}

	got := BuildInventory(nodes, acpps, ndrs)
	if len(got) != 2 {
		t.Fatalf("ACPP 장치 2개 = 2행: got %d", len(got))
	}
	for _, v := range got {
		if v.Vendor != "nvidia" {
			t.Fatalf("멀티벤더 노드에서 NVIDIA 장치는 모델 매칭으로 vendor=nvidia 여야 함: model=%q got=%q", v.Model, v.Vendor)
		}
	}
}

// TestBuildInventory_SortedDeterministically: UI 가 새로고침마다 순서가 바뀌면 못 읽는다.
func TestBuildInventory_SortedDeterministically(t *testing.T) {
	nodes := []corev1.Node{
		node("worker2", false, map[string]string{"nvidia.com/gpu": "1"}),
		node("worker1", false, map[string]string{"nvidia.com/gpu": "1"}),
	}
	ndrs := []v1alpha1.NodeDeviceReport{
		ndr("worker1", v1alpha1.DeviceEntry{Vendor: "nvidia", Model: "A30", Count: 1}),
		ndr("worker2", v1alpha1.DeviceEntry{Vendor: "nvidia", Model: "A2", Count: 1}),
	}
	got := BuildInventory(nodes, nil, ndrs)
	if len(got) != 2 || got[0].NodeName != "worker1" || got[1].NodeName != "worker2" {
		t.Fatalf("노드 이름 사전순이어야 함: %#v", got)
	}
}

// TestBuildInventory_CarriesPartitionInstanceCounts: profile 이름 목록만으로는 논리 파티션 수를
// 셀 수 없다 — layout 항목 1개가 장치당 N개다. expectedCountPerDevice 가 뷰까지 와야 대시보드가
// 정직한 수를 낼 수 있다.
func TestBuildInventory_CarriesPartitionInstanceCounts(t *testing.T) {
	objs := invObjects() // 기존 헬퍼. ACPP TargetStatus.ResolvedLayout 이 들어 있다.
	var nodes []corev1.Node
	var acpps []v1alpha1.AcceleratorPartitionPolicy
	var ndrs []v1alpha1.NodeDeviceReport
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.Node:
			nodes = append(nodes, *v)
		case *v1alpha1.AcceleratorPartitionPolicy:
			acpps = append(acpps, *v)
		case *v1alpha1.NodeDeviceReport:
			ndrs = append(ndrs, *v)
		}
	}

	rows := BuildInventory(nodes, acpps, ndrs)
	var found bool
	for _, r := range rows {
		for _, pi := range r.PartitionInstances {
			if pi.Profile == "" {
				t.Errorf("profile 이 비었다: %+v", pi)
			}
			if pi.CountPerDevice < 1 {
				t.Errorf("countPerDevice 가 1 미만이면 인스턴스가 아니다: %+v", pi)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("파티션이 적용된 행이 있는데 PartitionInstances 가 비어 있다")
	}
}

// 화면이 3상태를 낼 수 있으려면 verification 이 JSON 까지 와야 한다.
func TestToCapabilityView_CarriesPartitionVerification(t *testing.T) {
	dev := v1alpha1.DeviceStatus{
		Model: "A30",
		PartitionCapability: v1alpha1.PartitionCapability{
			HardwareSupported: false,
			Reason:            v1alpha1.ReasonHardwareCapabilityMissing,
			Verification:      v1alpha1.VerificationRequired,
		},
	}
	v := toCapabilityView(dev)
	if v.Partition.Verification != v1alpha1.VerificationRequired {
		t.Fatalf("verification 이 투영되지 않음: %+v", v.Partition)
	}
}

// governed-but-pending 행은 확인된 장치가 0개다. 그 행에 "장치당 N 인스턴스" 를 실으면
// 아직 존재하지 않는 장치에 대해 수를 주장하는 셈이다(stale 을 비우는 것과 같은 이유).
// 대시보드 카드는 pending 을 "미상" 으로 빼므로 화면은 안전하지만 원시 JSON 이 새는 자리다.
func TestBuildInventory_PendingRowCarriesNoInstanceCounts(t *testing.T) {
	nodes := []corev1.Node{node("worker1", false, map[string]string{"nvidia.com/gpu": "1"})}
	acpp := v1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 1},
		Spec:       v1alpha1.AcceleratorPartitionPolicySpec{Vendor: "nvidia"},
		Status: v1alpha1.AcceleratorPartitionPolicyStatus{
			ObservedGeneration: 1,
			Targets: []v1alpha1.TargetStatus{{
				// Ready + generation 일치라 stale 이 아니다. 그런데 Devices 가 비어 있어 pending
				// 이다 — 이 조합에서만 stale 게이트가 못 막고 인스턴스 수가 실린다.
				NodeName: "worker1", Phase: v1alpha1.ACPPPhaseReady,
				ResolvedLayout: []v1alpha1.ResolvedLayoutEntry{{Profile: "1g.6gb", ExpectedCountPerDevice: 4}},
			}},
		},
	}
	rows := BuildInventory(nodes, []v1alpha1.AcceleratorPartitionPolicy{acpp}, nil)
	var sawPending bool
	for _, r := range rows {
		if !r.Pending {
			continue
		}
		sawPending = true
		if len(r.PartitionInstances) != 0 {
			t.Errorf("pending 행이 인스턴스 수를 주장한다: %+v", r.PartitionInstances)
		}
	}
	if !sawPending {
		t.Fatal("픽스처에 pending 행이 없다 — 테스트가 아무것도 검사하지 않았다")
	}
}

// TestBuildInventory_FailedSiblingDoesNotHideReadyRows — 라이브 결함 D-4(Task 5 §5.2):
// devicesForNode 의 첫-매치 단락이 Failed 정책("worker1-a2-timeslice")을 골라 같은 노드의
// Ready 정책(readySiblingPolicyName) 장치 행을 뭉개면 안 된다(/accelerators total 4→ 정상 복원).
func TestBuildInventory_FailedSiblingDoesNotHideReadyRows(t *testing.T) {
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
	got := BuildInventory(nodes, []v1alpha1.AcceleratorPartitionPolicy{failed, ready}, nil)
	if len(got) != 1 {
		t.Fatalf("Ready 정책 장치 1개 = 1행이어야 함: got %d rows: %#v", len(got), got)
	}
	if got[0].Policy != readySiblingPolicyName || got[0].Phase != v1alpha1.ACPPPhaseReady {
		t.Fatalf("행이 Ready 정책 소속이어야 함: policy=%q phase=%q", got[0].Policy, got[0].Phase)
	}
	if got[0].Stale {
		t.Fatalf("Ready 정책 채택 시 stale 이 아니어야 함: %q", got[0].StaleReason)
	}
	if len(got[0].PartitionInstances) != 1 || got[0].PartitionInstances[0].CountPerDevice != 4 {
		t.Fatalf("Ready 정책의 인스턴스 수가 실려야 함: %#v", got[0].PartitionInstances)
	}
}
