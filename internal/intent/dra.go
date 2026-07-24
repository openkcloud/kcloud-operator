// ============================================================
// dra.go: DRA 가용성 실측
// 상세: resource.k8s.io/v1 이 서빙되는가와, 서빙되더라도 특정 드라이버가 실제로 노드에
// ResourceSlice 를 냈는가는 다른 사실이다(전자는 클러스터 업그레이드, 후자는 드라이버 설치가
// 답이다) — 그래서 두 관측을 한 구조체 안에서도 분리해 담는다.
// 생성일: 2026-08-05
// ============================================================
package intent

import (
	resourcev1 "k8s.io/api/resource/v1"
)

// DRACapability 는 이 클러스터가 지금 DRA 로 무엇을 줄 수 있는가다.
type DRACapability struct {
	// APIServed 는 resource.k8s.io/v1 이 서빙되는가다. false 면 드라이버 유무를 물을 수조차 없다.
	APIServed bool
	// DeviceClasses 는 존재하는 DeviceClass 이름 집합이다.
	DeviceClasses map[string]bool
	// SlicesByNodeDriver 는 노드 → 드라이버 → 그 노드가 그 드라이버로 내놓는 장치 수다.
	SlicesByNodeDriver map[string]map[string]int32
}

// BuildDRACapability 는 DeviceClass·ResourceSlice 목록을 노드별 DRA capability 로 접는다(순수 함수).
func BuildDRACapability(apiServed bool, classes []resourcev1.DeviceClass, slices []resourcev1.ResourceSlice) DRACapability {
	out := DRACapability{APIServed: apiServed, DeviceClasses: map[string]bool{}, SlicesByNodeDriver: map[string]map[string]int32{}}
	for i := range classes {
		out.DeviceClasses[classes[i].Name] = true
	}
	for i := range slices {
		s := &slices[i]
		// 노드에 묶이지 않은 슬라이스는 "어느 노드가 후보인가" 에 답하지 못한다.
		// 세지 않는다 — 세면 노드 없는 용량이 후보 판정을 통과시킨다.
		if s.Spec.NodeName == nil || *s.Spec.NodeName == "" || s.Spec.Driver == "" {
			continue
		}
		node := *s.Spec.NodeName
		if out.SlicesByNodeDriver[node] == nil {
			out.SlicesByNodeDriver[node] = map[string]int32{}
		}
		out.SlicesByNodeDriver[node][s.Spec.Driver] += int32(len(s.Spec.Devices))
	}
	return out
}
