// ============================================================
// inventory.go: 가속기 인벤토리 집약 — 노드·ACPP·NDR → 물리 장치 목록
// 상세: 정직성 계약(R&D v1.0 §19.3)을 코드로 고정한다. (1) 물리 장치당 정확히 1행 —
//       time-slicing replica 는 절대 행이 되지 않고 sharingReplicas 숫자로만 나타난다.
//       (2) SharingModeView.Verified 는 supported && verification==verified 일 때만 true 라
//       UI 가 "주장" 을 "지원" 으로 렌더할 수 없다. (3) stale/미수렴 데이터는 숨기지 않는다 —
//       ACPP 가 타깃하지만 아직 장치를 보고하지 않은(수렴 중) 노드는 미관리로 떨어뜨리지 않고
//       Source=ACPP + Stale 행으로 남기며, 미수렴 ACPP 의 낡은 장치 데이터는 애초에 노출하지
//       않는다(nc.Devices 가 intent.applyACPPStatus 의 게이트를 통과한 값만 담는다). 장치
//       사실의 1순위 출처는 ACPP status 이고, ACPP 가 관리하지 않는 노드는 NDR 집계 행에서
//       합성 ID 로 채우되 source 로 구분한다.
// 생성일: 2026-07-30 | 수정일: 2026-07-30
// ============================================================

package apiserver

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/intent"
)

// 장치 식별자의 출처. NDR 출처는 합성 ID 이므로 벤더 canonical UUID 가 아니다 — 사용자가
// 그 차이를 알아야 하므로 숨기지 않고 필드로 낸다.
const (
	SourceACPP = "AcceleratorPartitionPolicy"
	SourceNDR  = "NodeDeviceReport"
)

// SharingModeView 는 한 공유 방식의 지원 상태다.
type SharingModeView struct {
	Supported    bool   `json:"supported"`
	Verification string `json:"verification,omitempty"`
	// Verified 는 Supported && Verification=="verified" 다. UI 가 "지원" 배지를 켤 수 있는
	// 유일한 조건이며, 이 계산을 API 쪽에 두는 이유는 클라이언트마다 다시 판단하다 한 곳에서
	// 완화될 여지를 없애기 위해서다.
	Verified    bool   `json:"verified"`
	MaxReplicas int32  `json:"maxReplicas,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// SharingView 는 SharingCapability 세 축의 뷰다.
type SharingView struct {
	TimeSlicing  SharingModeView `json:"timeSlicing"`
	MultiProcess SharingModeView `json:"multiProcess"`
	Brokered     SharingModeView `json:"brokered"`
}

// PartitionInstanceView 는 profile 하나가 장치당 몇 인스턴스인지다. PartitionProfiles 는
// 이름만 담아 "항목 1개 = 인스턴스 1개" 로 오독되므로(예: 1g.6gb×4 는 인스턴스 4개), 세야 하는
// 소비자는 이 필드를 쓴다.
type PartitionInstanceView struct {
	Profile        string `json:"profile"`
	CountPerDevice int32  `json:"countPerDevice"`
}

// CapabilityView 는 장치 하나의 capability 전체다.
type CapabilityView struct {
	Partition      v1alpha1.PartitionCapability `json:"partition"`
	Sharing        SharingView                  `json:"sharing"`
	Isolation      v1alpha1.IsolationCapability `json:"isolation"`
	AllocationAPIs []string                     `json:"allocationAPIs,omitempty"`
}

// AcceleratorView 는 물리 장치 하나다(단, ACPP 가 아직 장치를 보고하지 않은 governed-pending
// 상태에서는 예외적으로 장치 없는 노드 단위 placeholder 행이 된다 — Pending 으로 구분되고 UID 는
// 비운다. 실제 장치 UID 와 매직 스트링으로 충돌시키지 않기 위해서다).
type AcceleratorView struct {
	// UID 는 실제 장치 식별자다(canonical: ACPP DeviceStatus.ID, 또는 NDR 합성 ID). Pending 행은
	// 아직 장치가 없으므로 비운다.
	UID    string `json:"uid,omitempty"`
	Source string `json:"source"`
	// Schedulable 은 노드가 cordon 되지 않았는가다. false 여도 장치는 목록에 남는다.
	Schedulable bool   `json:"schedulable"`
	NodeName    string `json:"nodeName"`
	// Vendor 는 장치 모델을 NDR 집계 행과 매칭해 되짚은 값이다(deviceVendor). 매칭되는 NDR 행이
	// 없으면 노드 단위 벤더(vendorForNode, 멀티벤더 노드에선 리소스명 사전순이라 부정확할 수 있음)
	// 로 폴백한다 — 이 경우 오답 가능성이 남는다.
	Vendor     string `json:"vendor"`
	Model      string `json:"model,omitempty"`
	PCIAddress string `json:"pciAddress,omitempty"`
	// Policy/Phase 는 이 장치를 관리하는 ACPP 다(없으면 빈 문자열 = 미관리).
	Policy string `json:"policy,omitempty"`
	Phase  string `json:"phase,omitempty"`
	// Pending 은 ACPP 가 이 노드를 타깃하지만 아직 장치를 보고하지 않았다는 뜻이다(수렴 중 또는
	// stale). true 면 이 행엔 UID/Model/PCIAddress/Capability 가 없다 — 노드가 governed 상태라는
	// 사실만 전달한다.
	Pending bool `json:"pending,omitempty"`
	// Stale 은 이 노드를 타깃한 ACPP 가 수렴하지 않았음을 뜻한다(status.observedGeneration 이
	// metadata.generation 을 못 따라갔거나 target phase != Ready). true 면 Model/PCIAddress/
	// SharingMode/Capability 가 비어 있거나(수렴 중) 낡은 값(스펙 변경 후 미반영)일 수 있다 —
	// SharingMode 는 특히 stale 행에서 초기화 기본값(exclusive)로 채워진 채 나갈 수 있다(applyACPPStatus
	// 가 실제 ApplyRecord 를 읽기 전에 반환하므로, 리뷰 Important #2). UI 는 이 값들을 "정상" 으로
	// 렌더하면 안 된다. 미관리(Source=NodeDeviceReport) 행은 항상 false 다.
	Stale       bool   `json:"stale"`
	StaleReason string `json:"staleReason,omitempty"`
	// SharingMode/SharingReplicas 는 노드 단위 사실이다. replicas 는 광고 배수이지 장치 개수가
	// 아니다 — 이 값이 4 여도 이 행은 여전히 물리 장치 1개를 가리킨다.
	SharingMode       string   `json:"sharingMode"`
	SharingReplicas   int32    `json:"sharingReplicas,omitempty"`
	PartitionProfiles []string `json:"partitionProfiles,omitempty"`
	// PartitionInstances 는 profile 별 장치당 인스턴스 수다. PartitionProfiles 는 이름만 담아
	// "항목 1개 = 인스턴스 1개" 로 오독되므로, 세야 하는 소비자는 이 필드를 쓴다.
	// +optional
	PartitionInstances []PartitionInstanceView `json:"partitionInstances,omitempty"`
	// NodeAdvertised 는 노드 범위 값이다(장치 범위가 아니다). 이름에 Node 를 넣어 오해를 막는다.
	NodeAdvertised map[string]int32 `json:"nodeAdvertised,omitempty"`
	MemoryMiB      int64            `json:"memoryMiB,omitempty"`
	Capability     CapabilityView   `json:"capability"`
}

// toSharingModeView 는 정직성 게이트다 — supported 주장만으로 Verified 를 켜지 않는다.
func toSharingModeView(s v1alpha1.SharingModeSupport) SharingModeView {
	return SharingModeView{
		Supported:    s.Supported,
		Verification: s.Verification,
		Verified:     s.Supported && s.Verification == v1alpha1.VerificationVerified,
		MaxReplicas:  s.MaxReplicas,
		Reason:       s.Reason,
	}
}

// toCapabilityView 는 DeviceStatus 의 capability 축을 뷰로 옮긴다.
func toCapabilityView(d v1alpha1.DeviceStatus) CapabilityView {
	return CapabilityView{
		Partition: d.PartitionCapability,
		Sharing: SharingView{
			TimeSlicing:  toSharingModeView(d.SharingCapability.TimeSlicing),
			MultiProcess: toSharingModeView(d.SharingCapability.MultiProcess),
			Brokered:     toSharingModeView(d.SharingCapability.Brokered),
		},
		Isolation:      d.IsolationCapability,
		AllocationAPIs: d.AllocationAPIs,
	}
}

// devicesForNode 는 이 노드를 타깃한 ACPP 의 이름·phase·ResolvedLayout 이다(없으면 빈 값). 장치
// 목록은 여기서 돌려주지 않는다 — 호출자는 전부 nc.Devices(intent.BuildSnapshot 산출, 수렴 게이트를
// 이미 통과한 값)를 대신 쓴다. 선택 규칙은 intent.applyACPPStatus 의 채택 규칙과 반드시 같아야
// 한다(수렴+Ready 정책 우선, 사전순) — 다르면 여기서 고른 정책의 layout 이 저쪽이 채택한 Devices
// 와 다른 TargetStatus 에서 와 짝이 어긋난다(라이브 결함 D-4: 사전순 앞의 Failed 정책이 Ready
// 정책의 행을 뭉갰다). Ready 가 없으면 사전순 첫 정책으로 폴백해 governed-but-pending/failed
// 상태를 정직하게 드러낸다. 어느 쪽이든 결정론적 선택일 뿐 소유권 판정이 아니다(owner-lock 은
// 컨트롤러 몫).
func devicesForNode(acpps []v1alpha1.AcceleratorPartitionPolicy, nodeName string) (policy, phase string, layout []v1alpha1.ResolvedLayoutEntry) {
	sorted := make([]v1alpha1.AcceleratorPartitionPolicy, len(acpps))
	copy(sorted, acpps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	found := false
	for i := range sorted {
		for j := range sorted[i].Status.Targets {
			t := &sorted[i].Status.Targets[j]
			if t.NodeName != nodeName {
				continue
			}
			if sorted[i].Status.ObservedGeneration == sorted[i].Generation && t.Phase == v1alpha1.ACPPPhaseReady {
				return sorted[i].Name, t.Phase, t.ResolvedLayout
			}
			if !found {
				found = true
				policy, phase, layout = sorted[i].Name, t.Phase, t.ResolvedLayout
			}
		}
	}
	return policy, phase, layout
}

// partitionInstancesFor 는 ResolvedLayoutEntry 를 뷰로 옮긴다. count<=0 인 항목은 애초에
// 인스턴스가 아니므로 뺀다(ndrEntriesFor 의 Count>0 필터와 같은 규율). profile 이름 오름차순으로
// 고정한다 — UI 가 새로고침마다 순서가 바뀌면 못 읽는다(TestBuildInventory_SortedDeterministically
// 와 같은 이유).
func partitionInstancesFor(entries []v1alpha1.ResolvedLayoutEntry) []PartitionInstanceView {
	out := make([]PartitionInstanceView, 0, len(entries))
	for _, e := range entries {
		if e.ExpectedCountPerDevice <= 0 {
			continue
		}
		out = append(out, PartitionInstanceView{Profile: e.Profile, CountPerDevice: e.ExpectedCountPerDevice})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out
}

// uncordonedCopy 는 스냅샷 입력용으로 unschedulable 만 지운 복사본이다. intent.BuildSnapshot 은
// cordon 된 노드를 스케줄 후보에서 빼지만, 인벤토리는 cordon 된 노드의 장치도 보여야 한다 —
// 스케줄 가능 여부는 원본 노드에서 따로 읽어 Schedulable 로 정직하게 보고한다.
func uncordonedCopy(nodes []corev1.Node) []corev1.Node {
	out := make([]corev1.Node, len(nodes))
	copy(out, nodes)
	for i := range out {
		out[i].Spec.Unschedulable = false
	}
	return out
}

// BuildInventory 는 노드·ACPP·NDR 을 물리 장치 목록으로 접는다(순수 함수).
// 노드 단위 값(광고량·공유 상태·파티션 프로파일)은 intent.BuildSnapshot 을 그대로 재사용한다 —
// 특히 "광고 수 > 물리 장치 수이면 timeSliced" 정직성 override 를 여기서 다시 구현하지 않는다.
func BuildInventory(nodes []corev1.Node, acpps []v1alpha1.AcceleratorPartitionPolicy, ndrs []v1alpha1.NodeDeviceReport) []AcceleratorView {
	snap := intent.BuildSnapshot(uncordonedCopy(nodes), acpps, ndrs)
	byNode := make(map[string]intent.NodeCapability, len(snap))
	for i := range snap {
		byNode[snap[i].NodeName] = snap[i]
	}

	sorted := make([]corev1.Node, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	out := make([]AcceleratorView, 0, len(sorted))
	for i := range sorted {
		name := sorted[i].Name
		nc := byNode[name] // 없으면 zero value — 광고가 없는 노드다.
		base := AcceleratorView{
			Schedulable:       !sorted[i].Spec.Unschedulable,
			NodeName:          name,
			SharingMode:       v1alpha1.SharingModeExclusive,
			PartitionProfiles: nc.PartitionProfiles,
			NodeAdvertised:    nc.Advertised,
			MemoryMiB:         nc.MemoryMiB,
			Stale:             nc.Stale,
			StaleReason:       nc.StaleReason,
		}
		if nc.SharingMode != "" {
			base.SharingMode = nc.SharingMode
		}
		base.SharingReplicas = nc.SharingReplicas

		// policy 와 layout 은 반드시 같은 TargetStatus 에서 나와야 한다. layout 만 채워지고
		// policy 가 비면 아래 할당이 미관리 노드 행에까지 인스턴스 수를 실어버린다(지금은
		// devicesForNode 가 둘을 같은 매칭에서 내므로 짝이 보장된다 — 여기를 고칠 사람 주의).
		policy, phase, layout := devicesForNode(acpps, name)
		// stale 이면 ResolvedLayout 도 신뢰 구간 밖이다 — 수를 주장하지 않는다.
		if !nc.Stale {
			base.PartitionInstances = partitionInstancesFor(layout)
		}
		if policy != "" {
			// ACPP 가 이 노드를 타깃한다. 장치 목록은 devicesForNode 의 원시 TargetStatus.Devices
			// 가 아니라 nc.Devices(intent.BuildSnapshot 산출)를 쓴다 — applyACPPStatus 가 이미
			// generation 불일치·phase!=Ready 를 걸러 두었으므로(리뷰 Critical #1) 여기서 같은
			// 수렴 판정을 다시 구현하면 판정이 어긋날 여지가 생긴다.
			if len(nc.Devices) > 0 {
				for _, d := range nc.Devices {
					v := base
					v.UID, v.Source = d.ID, SourceACPP
					v.Model, v.PCIAddress = d.Model, d.PCIAddress
					v.Policy, v.Phase = policy, phase
					v.Vendor = deviceVendor(nc, ndrs, name, d.Model)
					v.Capability = toCapabilityView(d)
					out = append(out, v)
				}
			} else {
				// 장치를 아직 보고하지 않았다(수렴 중) 또는 stale 하다 — 미관리(NDR)로 오분류
				// 하지 않고 governed-but-pending 상태를 한 행으로 정직하게 낸다(리뷰 Critical #2).
				// base.Stale/StaleReason 이 이미 이 상태를 담고 있다.
				v := base
				v.Source = SourceACPP
				v.Policy, v.Phase = policy, phase
				v.Pending = true
				// 장치가 하나도 확인되지 않은 행에 "장치당 N 인스턴스" 를 실으면, 아직 없는
				// 장치에 대해 수를 주장하는 셈이다 — stale 과 같은 이유로 비운다.
				v.PartitionInstances = nil
				v.Vendor = vendorForNode(nc, ndrs, name)
				out = append(out, v)
			}
			continue
		}
		// 어떤 ACPP 도 이 노드를 타깃하지 않는다 — 진짜 미관리 노드다. NDR 집계 행을 개수만큼
		// 펼치고 합성 ID 를 준다.
		for _, e := range ndrEntriesFor(ndrs, name) {
			for k := int32(0); k < e.Count; k++ {
				v := base
				v.UID = fmt.Sprintf("%s/%s/%s#%d", name, e.Vendor, e.Model, k)
				v.Source = SourceNDR
				v.Vendor, v.Model, v.PCIAddress = e.Vendor, e.Model, e.PCIeAddress
				if e.MemoryMiB > 0 {
					v.MemoryMiB = e.MemoryMiB
				}
				out = append(out, v)
			}
		}
	}
	return out
}

// ndrEntriesFor 는 이 노드의 NDR 장치 집계 행이다(벤더·모델 사전순으로 고정).
func ndrEntriesFor(ndrs []v1alpha1.NodeDeviceReport, nodeName string) []v1alpha1.DeviceEntry {
	var out []v1alpha1.DeviceEntry
	for i := range ndrs {
		if ndrs[i].Spec.NodeName != nodeName {
			continue
		}
		for _, d := range ndrs[i].Status.Devices {
			if d.Count > 0 {
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Vendor != out[j].Vendor {
			return out[i].Vendor < out[j].Vendor
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// deviceVendor 는 ACPP 장치 한 대의 벤더다. DeviceStatus 에는 벤더 필드가 없어 모델명으로
// NDR 집계 행과 매칭해 되짚는다 — 멀티벤더 노드(예: NVIDIA A30/A2 + Furiosa RNGD 카드가 한 노드에
// 공존)에서 vendorForNode(노드 단위 벤더 하나, 리소스명 사전순 첫 값)를 장치마다 그대로 복사하면
// 오답이 난다(task-2-review Important #3 — worker1 에서 NVIDIA 장치에 furiosa 가 찍히는 재현
// 사례). 매칭되는 NDR 행이 없으면(detector 미관측 등) vendorForNode 로 폴백한다 — 그 경우엔
// 여전히 오답 가능성이 남는다(DeviceStatus 에 device 단위 벤더 필드가 없는 한 완전한 해결은
// 불가 — Task 1 스키마 범위 밖).
func deviceVendor(nc intent.NodeCapability, ndrs []v1alpha1.NodeDeviceReport, nodeName, model string) string {
	for _, e := range ndrEntriesFor(ndrs, nodeName) {
		if e.Model == model {
			return e.Vendor
		}
	}
	return vendorForNode(nc, ndrs, nodeName)
}

// vendorForNode 는 노드 단위 벤더 폴백이다(governed-pending placeholder 행처럼 특정 장치가 없는
// 경우 전용). 스냅샷의 벤더(광고 리소스명에서 되짚은 값, 멀티벤더 노드에선 사전순 첫 값이라
// 부정확할 수 있음)를 쓰고, 그것도 없으면(예: 이 노드가 가속기를 전혀 광고하지 않아 스냅샷에
// 없는데도 ACPP 가 타깃하는 드문 경우) NDR 첫 행으로 떨어진다 — 이 마지막 분기는 사실상 죽은
// 코드가 아니라 그 드문 경우를 위해 남겨 둔다.
func vendorForNode(nc intent.NodeCapability, ndrs []v1alpha1.NodeDeviceReport, nodeName string) string {
	if nc.Vendor != "" {
		return nc.Vendor
	}
	if e := ndrEntriesFor(ndrs, nodeName); len(e) > 0 {
		return e[0].Vendor
	}
	return ""
}
