// ============================================================
// npuclusterpolicy_types_test.go: NPUClusterPolicy 타입의 기본값 해석 시험
// 생성일: 2026-08-07
// ============================================================
package v1alpha1

import "testing"

// nil DRASpec 은 "안 켰다" 여야 한다. 이 필드를 모르는 기존 정책이 지금까지와
// 똑같이 동작해야 하므로, nil 을 켜짐으로 읽으면 조용히 드라이버가 깔린다.
func TestDRASpecIsEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *DRASpec
		want bool
	}{
		{"nil 은 꺼짐", nil, false},
		{"빈 구조체는 꺼짐", &DRASpec{}, false},
		{"명시적 켜짐", &DRASpec{Enabled: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.IsEnabled(); got != tc.want {
				t.Errorf("IsEnabled()=%v, want %v", got, tc.want)
			}
		})
	}
}

// 벤더 블록마다 DRA 축이 실제로 붙어 있는지 본다. 필드를 추가하면서 한 곳을
// 빠뜨리는 것이 이 작업에서 실제로 일어날 수 있는 누락이다.
func TestDRASpecInlinedInVendorBlocks(t *testing.T) {
	p := NPUClusterPolicy{}
	p.Spec.Nvidia.DRA = &DRASpec{Enabled: true}
	p.Spec.Furiosa.DRA = &DRASpec{Enabled: true}
	p.Spec.Furiosa.Rngd.DRA = &DRASpec{Enabled: true}
	p.Spec.Tenstorrent.DRA = &DRASpec{Enabled: true}

	for name, s := range map[string]*DRASpec{
		"nvidia":       p.Spec.Nvidia.DRA,
		"furiosa":      p.Spec.Furiosa.DRA,
		"furiosa.rngd": p.Spec.Furiosa.Rngd.DRA,
		"tenstorrent":  p.Spec.Tenstorrent.DRA,
	} {
		if !s.IsEnabled() {
			t.Errorf("%s 블록의 DRA 축이 동작하지 않는다", name)
		}
	}
}
