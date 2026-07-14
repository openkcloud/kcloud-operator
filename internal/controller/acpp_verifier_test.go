// ============================================================
// acpp_verifier_test.go: allocatableMet 순수 함수 + allocationProber 어댑터 unit 테스트
// 생성일: 2026-07-23 | 수정일: 2026-07-31
// ============================================================
package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"kcloud-operator/internal/partition"
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

// stubVerifier 는 partition.Verifier 를 고정 응답으로 구현한다(allocationProber 어댑터 전용 테스트 seam).
type stubVerifier struct {
	res *partition.VerifyResult
	err error
}

func (s stubVerifier) VerifyAllocatable(partition.Target, map[string]int32) (*partition.VerifyResult, error) {
	return nil, nil
}
func (s stubVerifier) VerifyAllocation(partition.Target, string) (*partition.VerifyResult, error) {
	return s.res, s.err
}

// TestAllocationProber_Probe 는 NewAllocationProber 가 기존 partition.Verifier.VerifyAllocation 을
// verification.AllocationProber 계약(allocated, message, err)으로 정확히 옮기는지 확인한다 —
// nil verifier(미설정), 프로브 자체 에러, nil 결과, 정상 결과 네 갈래.
func TestAllocationProber_Probe(t *testing.T) {
	t.Run("nil verifier reports not-allocated without error", func(t *testing.T) {
		p := NewAllocationProber(nil)
		ok, msg, err := p.Probe(context.Background(), "node-1", "nvidia.com/mig-1g.6gb")
		if ok || msg == "" || err != nil {
			t.Fatalf("Probe() = (%v, %q, %v), want (false, non-empty, nil)", ok, msg, err)
		}
	})
	t.Run("verifier error propagates", func(t *testing.T) {
		wantErr := errors.New("probe pod scheduling failed")
		p := NewAllocationProber(stubVerifier{err: wantErr})
		ok, _, err := p.Probe(context.Background(), "node-1", "nvidia.com/mig-1g.6gb")
		if ok || !errors.Is(err, wantErr) {
			t.Fatalf("Probe() = (%v, _, %v), want (false, %v)", ok, err, wantErr)
		}
	})
	t.Run("nil result is not-allocated without error", func(t *testing.T) {
		p := NewAllocationProber(stubVerifier{res: nil})
		ok, msg, err := p.Probe(context.Background(), "node-1", "nvidia.com/mig-1g.6gb")
		if ok || msg == "" || err != nil {
			t.Fatalf("Probe() = (%v, %q, %v), want (false, non-empty, nil)", ok, msg, err)
		}
	})
	t.Run("successful allocation reports true", func(t *testing.T) {
		p := NewAllocationProber(stubVerifier{res: &partition.VerifyResult{TestPodAllocated: true}})
		ok, _, err := p.Probe(context.Background(), "node-1", "nvidia.com/mig-1g.6gb")
		if !ok || err != nil {
			t.Fatalf("Probe() = (%v, _, %v), want (true, nil)", ok, err)
		}
	})
	t.Run("unallocated result reports false", func(t *testing.T) {
		p := NewAllocationProber(stubVerifier{res: &partition.VerifyResult{TestPodAllocated: false}})
		ok, _, err := p.Probe(context.Background(), "node-1", "nvidia.com/mig-1g.6gb")
		if ok || err != nil {
			t.Fatalf("Probe() = (%v, _, %v), want (false, nil)", ok, err)
		}
	})
}
