// ============================================================
// dra_test.go: DRA 가용성 실측 테스트
// 상세: API 서빙 여부와 드라이버별 노드 장치 수를 분리해 수집하는지, 노드에 묶이지 않은
// ResourceSlice 를 후보 판정에서 제외하는지 본다.
// 생성일: 2026-08-05
// ============================================================
package intent

import (
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestBuildDRACapability(t *testing.T) {
	classes := []resourcev1.DeviceClass{{ObjectMeta: metav1.ObjectMeta{Name: "gpu.nvidia.com"}}}
	slices := []resourcev1.ResourceSlice{
		{Spec: resourcev1.ResourceSliceSpec{
			Driver:   "gpu.nvidia.com",
			NodeName: ptr.To("k8s-worker1"),
			Devices:  []resourcev1.Device{{Name: "gpu-0"}, {Name: "gpu-1"}},
		}},
	}
	got := BuildDRACapability(true, classes, slices)
	if !got.APIServed {
		t.Fatal("APIServed should be true")
	}
	if !got.DeviceClasses["gpu.nvidia.com"] {
		t.Fatalf("DeviceClasses missing gpu.nvidia.com: %+v", got.DeviceClasses)
	}
	if n := got.SlicesByNodeDriver["k8s-worker1"]["gpu.nvidia.com"]; n != 2 {
		t.Fatalf("device count = %d, want 2", n)
	}
}

// 노드에 묶이지 않은 슬라이스(NodeName=nil)는 노드 후보 판정에 쓸 수 없다.
func TestBuildDRACapabilitySkipsNodelessSlices(t *testing.T) {
	slices := []resourcev1.ResourceSlice{
		{Spec: resourcev1.ResourceSliceSpec{Driver: "d", Devices: []resourcev1.Device{{Name: "x"}}}},
	}
	got := BuildDRACapability(true, nil, slices)
	if len(got.SlicesByNodeDriver) != 0 {
		t.Fatalf("nodeless slice must not produce a node entry: %+v", got.SlicesByNodeDriver)
	}
}
