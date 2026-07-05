// ============================================================
// acpp_verifier_test.go: allocatableMet 순수 함수 unit 테스트
// 생성일: 2026-07-23
// ============================================================
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestNewLiveVerifier_ProbeImage 는 ACPP_PROBE_IMAGE 미설정 시 fallback(registry.k8s.io/pause:3.9),
// 설정 시 override 값을 사용함을 검증한다(finding #3 — air-gap 미러 이미지 지정 seam, pure/no envtest).
func TestNewLiveVerifier_ProbeImage(t *testing.T) {
	t.Run("falls back when unset", func(t *testing.T) {
		v := NewLiveVerifier(nil).(*liveVerifier)
		if v.probeImage != defaultProbeImage {
			t.Errorf("probeImage = %q, want fallback %q", v.probeImage, defaultProbeImage)
		}
	})
	t.Run("uses override when set", func(t *testing.T) {
		t.Setenv("ACPP_PROBE_IMAGE", "registry.example.com:5000/kcloud/pause:3.9")
		v := NewLiveVerifier(nil).(*liveVerifier)
		if v.probeImage != "registry.example.com:5000/kcloud/pause:3.9" {
			t.Errorf("probeImage = %q, want override", v.probeImage)
		}
	})
}

func TestAllocatableMet(t *testing.T) {
	node := &corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		"furiosa.ai/rngd": resource.MustParse("4"),
	}}}

	cases := []struct {
		name     string
		resource string
		want     int32
		expect   bool
	}{
		{"met exactly", "furiosa.ai/rngd", 4, true},
		{"met with margin", "furiosa.ai/rngd", 2, true},
		{"not met", "furiosa.ai/rngd", 8, false},
		{"resource absent", "furiosa.ai/warboy", 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := allocatableMet(node, c.resource, c.want); got != c.expect {
				t.Errorf("allocatableMet(%q, %d) = %v, want %v", c.resource, c.want, got, c.expect)
			}
		})
	}
}
