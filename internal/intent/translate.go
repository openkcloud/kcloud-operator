// ============================================================
// translate.go: 추상 사용자 의도 → 벤더 device-plugin 리소스 번역
// 상세: 이 계획의 유일한 번역 함수다. admission webhook 과 컨트롤러가 같은 함수를 부른다.
// 순수 함수이므로 클러스터 조회는 호출부(Load)가 미리 해 둔다. 실패하면 항상 *Reject 를
// 돌려주며, 절대 다른 모드·다른 벤더로 조용히 갈아끼우지 않는다.
// 생성일: 2026-07-30 | 수정일: 2026-07-30
// ============================================================
package intent

import (
	"fmt"
	"sort"
	"strings"

	"kcloud-operator/api/v1alpha1"
)

// Request 는 번역 입력이다(클래스와 워크로드를 합쳐 평평하게 만든 것).
type Request struct {
	ClassName        string
	Mappings         []v1alpha1.AcceleratorMapping
	Access           v1alpha1.AccessSpec
	Requirements     v1alpha1.AcceleratorRequirements
	VendorPreference []string
	AllocationAPI    string
}

// Result 는 번역 결과다. 이것이 그대로 Pod 의 resource limit + nodeAffinity 가 되고,
// AcceleratorWorkload.status.resolved 에 그대로 실린다 — 그래서 별도 타입을 만들지 않고
// CRD 의 ResolvedAllocation 을 그대로 쓴다(필드 복사 계층이 하나 줄고, 어긋날 일도 없다).
type Result = v1alpha1.ResolvedAllocation

// BuildRequest 는 워크로드와 클래스를 번역 입력으로 접는다.
// 워크로드 요구사항은 클래스를 완화하지 못한다(MergeRequirements 가 강한 쪽을 남긴다).
func BuildRequest(aw *v1alpha1.AcceleratorWorkload, class *v1alpha1.AcceleratorClass) Request {
	acc := aw.Spec.Accelerator
	req := Request{
		ClassName:     class.Name,
		Mappings:      class.Spec.Mappings,
		Access:        acc.Access,
		Requirements:  MergeRequirements(class.Spec.Requirements, acc.Requirements),
		AllocationAPI: v1alpha1.AllocationAPIAuto,
	}
	if acc.Preferences != nil {
		req.VendorPreference = acc.Preferences.Vendors
		if acc.Preferences.AllocationAPI != "" {
			req.AllocationAPI = acc.Preferences.AllocationAPI
		}
	}
	return req
}

// Translate 는 요청을 벤더 리소스로 옮긴다. 실패 시 error 는 항상 *Reject 다.
func Translate(req Request, snap []NodeCapability) (*Result, error) {
	api := req.AllocationAPI
	if api == "" || api == v1alpha1.AllocationAPIAuto {
		api = v1alpha1.AllocationAPIDevicePlugin
	}
	if api == v1alpha1.AllocationAPIDRA {
		// CRD enum 은 dra 를 아직 받아준다(마지막 트랙). 이 클러스터에서 실현할 방법이 없으므로
		// 조용히 devicePlugin 으로 갈아끼우지 않고 여기서 거절한다.
		return nil, &Reject{Axis: AxisAllocationAPI, Reason: v1alpha1.AWReasonDRANotEnabled,
			Message: fmt.Sprintf("allocationAPI %q is not available on this cluster (kubernetes v1.28 has no resource.k8s.io); use devicePlugin or auto", api)}
	}
	if api != v1alpha1.AllocationAPIDevicePlugin {
		// dra 사유를 아무 값에나 붙이지 않는다 — CRD enum 이 오늘 막아 주지만 그 enum 은 다른
		// 파일에 있고 이 분기와 이어진 것이 없다.
		return nil, &Reject{Axis: AxisAllocationAPI, Reason: v1alpha1.AWReasonBackendUnsupported,
			Message: fmt.Sprintf("allocationAPI %q is not a known allocation API; use devicePlugin or auto", api)}
	}

	mappings := orderMappings(req.Mappings, req.VendorPreference)
	if len(mappings) == 0 {
		return nil, &Reject{Axis: AxisCandidates, Reason: v1alpha1.AWReasonNoVendorMapping,
			Message: fmt.Sprintf("AcceleratorClass %s declares no vendor mapping", req.ClassName)}
	}
	// admission webhook 이 이미 벤더 중복을 막지만 failurePolicy=Ignore 라 웹훅이 죽으면
	// 통과할 수 있다(D1) — 여기서도 걸러야 "선택이 비결정적" 인 클래스가 조용히 첫 매핑으로
	// 정해지는 일이 없다.
	if dup := duplicateVendor(mappings); dup != "" {
		return nil, &Reject{Axis: AxisCandidates, Reason: v1alpha1.AWReasonNoVendorMapping,
			Message: fmt.Sprintf("AcceleratorClass %s: vendor %q appears more than once, which would make the vendor choice non-deterministic", req.ClassName, dup)}
	}

	var firstReject *Reject
	for _, m := range mappings {
		name, err := ResourceFor(m, req.Access.Mode)
		if err != nil {
			// 리소스명을 정할 수 없는 매핑이다(furiosa product 모호). 노드를 하나도 못 보므로
			// 노드 루프 밖에서 잡고, 고칠 필드를 그대로 지목한다.
			if firstReject == nil {
				firstReject = &Reject{Axis: AxisProduct, Reason: v1alpha1.AWReasonNoVendorMapping, Message: err.Error()}
			}
			continue
		}
		if name == "" {
			// 카탈로그에 없는 벤더다(CRD enum 은 통과했지만 productResource 에 없음). 거절을
			// 남기지 않으면 이 매핑이 조용히 0개 노드를 내고 사용자는 클러스터를 가리키는
			// NoCandidateNodes 를 받는다 — 고칠 곳은 클래스다.
			if firstReject == nil {
				firstReject = &Reject{Axis: AxisProduct, Reason: v1alpha1.AWReasonNoVendorMapping,
					Message: fmt.Sprintf("vendor %q has no known device-plugin resource name", m.Vendor)}
			}
			continue
		}
		nodes := make([]string, 0, len(snap))
		for _, nc := range snap {
			if !nodeIsCandidateFor(nc, m, name) {
				continue
			}
			// 두 게이트를 모두 통과해야 후보다 — 한쪽 통과는 판정이 아니다.
			if rj := CheckMode(nc, req.Access, m); rj != nil {
				if firstReject == nil {
					firstReject = rj
				}
				continue
			}
			if rj := CheckRequirements(nc, req.Requirements, req.Access, m); rj != nil {
				if firstReject == nil {
					firstReject = rj
				}
				continue
			}
			nodes = append(nodes, nc.NodeName)
		}
		if len(nodes) == 0 {
			continue
		}
		sort.Strings(nodes)
		vendor := strings.ToLower(m.Vendor)
		return &Result{
			Vendor:       vendor,
			Mode:         req.Access.Mode,
			ResourceName: name,
			// 시분할 replica 는 독립 장치가 아니다 — 공유 모드에서도 Pod 이 요청하는 것은 몫 1개다.
			// replicas 는 장치를 몇 갈래로 나눴는지일 뿐이므로 Explanation 이 그 사실을 말한다.
			Quantity:      1,
			AllocationAPI: api,
			Nodes:         nodes,
			Explanation:   explain(vendor, m, req.Access, nodes),
		}, nil
	}
	// 후보가 없으면 "노드가 없다" 로 뭉개지 말고 실제로 실패한 축을 그대로 올린다.
	if firstReject != nil {
		return nil, firstReject
	}
	return nil, &Reject{Axis: AxisCandidates, Reason: v1alpha1.AWReasonNoCandidateNodes,
		Message: fmt.Sprintf("no schedulable node advertises an accelerator for any vendor mapped by AcceleratorClass %s (cordoned nodes are not candidates — check whether every candidate is under a driver upgrade)", req.ClassName)}
}

// nodeIsCandidateFor 는 이 노드가 이 매핑의 후보일 수 있는지 본다. 벤더만 보고 거르면(구
// nodeHasVendor) 같은 벤더 아래 다른 제품의 노드까지 후보로 들인다 — furiosa 벤더 아래 rngd 와
// warboy 가 다른 리소스명으로 광고되는데도 벤더가 같다는 이유만으로 Warboy 노드가 RNGD 요청의
// 후보가 되고, 그 노드의 사소한 "광고 안 함" 거절이 firstReject 를 선점해 실제 RNGD 노드의
// 거절을 가린다(D-2 라이브 재현: k8s-worker1 이 rngd-1 의 진짜 사유를 가림).
//
// 그래서 벤더가 아니라 리소스로 거른다: (a) 이미 이 리소스를 광고하고 있거나, (b) 아직 이
// 리소스는 광고하지 않지만(파티션 미적용 등 governed-pending) 이 매핑의 전체 장치 리소스는
// 광고한다 — 이 경우 CheckMode 의 ProfileNotApplied 같은 구체적 축이 열리도록 후보에 남긴다.
// 전체 장치조차 광고하지 않는 노드는(k8s-worker1 처럼) 이 매핑의 후보가 아니다 — 그 노드의
// 거절은 이 매핑에서 볼 가치가 없다.
func nodeIsCandidateFor(nc NodeCapability, m v1alpha1.AcceleratorMapping, resolved string) bool {
	if nc.Advertised[resolved] > 0 {
		return true
	}
	whole, err := WholeDeviceResource(m.Vendor, m.Product)
	return err == nil && whole != "" && nc.Advertised[whole] > 0
}

// duplicateVendor 는 두 번 나오는 첫 벤더를 돌려준다(없으면 ""). 대소문자를 구분하지 않는다
// (webhook 의 validateAcceleratorClass 와 동일한 판정).
func duplicateVendor(mappings []v1alpha1.AcceleratorMapping) string {
	seen := make(map[string]bool, len(mappings))
	for _, m := range mappings {
		vendor := strings.ToLower(strings.TrimSpace(m.Vendor))
		if seen[vendor] {
			return vendor
		}
		seen[vendor] = true
	}
	return ""
}

// orderMappings 는 선호 벤더를 앞으로 당긴다. 선호에만 있고 매핑에 없는 벤더는 무시하고,
// 선호에 없는 매핑도 뒤에 남긴다 — 선호는 요구가 아니다.
func orderMappings(mappings []v1alpha1.AcceleratorMapping, preference []string) []v1alpha1.AcceleratorMapping {
	out := make([]v1alpha1.AcceleratorMapping, 0, len(mappings))
	taken := make(map[int]bool, len(mappings))
	for _, want := range preference {
		for i, m := range mappings {
			if taken[i] || !strings.EqualFold(m.Vendor, want) {
				continue
			}
			out = append(out, m)
			taken[i] = true
		}
	}
	for i, m := range mappings {
		if !taken[i] {
			out = append(out, m)
		}
	}
	return out
}

// explain 은 사람이 읽는 근거다. 공유 결과는 격리가 없다는 사실을 반드시 말한다(§19.3).
// shared 분기가 "time-sliced" 라고 단정할 수 있는 것은 checkSharing 이 SharingMode 를
// timeSliced 로 못박아 통과시키기 때문이다(mode.go:132). 그 게이트가 multiProcess/brokered 로
// 넓어지는 날 이 문구도 access.Implementation 기준으로 좁혀야 한다.
func explain(vendor string, m v1alpha1.AcceleratorMapping, access v1alpha1.AccessSpec, nodes []string) string {
	switch access.Mode {
	case v1alpha1.AccessModeShared:
		return fmt.Sprintf("%s whole device on %v, shared by %d time-sliced replicas; replicas contend for compute and memory and are not isolated from each other",
			vendor, nodes, access.Replicas)
	case v1alpha1.AccessModePartitioned:
		return fmt.Sprintf("%s partition %s on %v", vendor, m.NativeProfile, nodes)
	case v1alpha1.AccessModePartitionedShared:
		return fmt.Sprintf("%s partition %s on %v, shared by %d replicas; replicas inside one partition are not isolated from each other",
			vendor, m.NativeProfile, nodes, access.Replicas)
	default:
		return fmt.Sprintf("%s whole device on %v, exclusive", vendor, nodes)
	}
}
