// ============================================================
// validate_test.go: descriptor 정합 검증 시험
// 상세: 유효한 표본 하나를 두고 필드를 하나씩 무너뜨려 각 규칙이 실제로 잡는지 본다.
//
//	무효 표본을 따로 손으로 짓지 않는다 - 유효 표본에서 파생시켜야 프로덕션이
//	만들 수 있는 조합만 시험한다.
//
// 생성일: 2026-08-10
// ============================================================
package descriptor

import (
	"strings"
	"testing"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// validSpec 은 모든 규칙을 통과하는 표본이다. 무효 표본은 전부 여기서 파생한다.
func validSpec() npuv1alpha1.AcceleratorDescriptorSpec {
	return npuv1alpha1.AcceleratorDescriptorSpec{
		Vendor:         "nvidia",
		Product:        "a30",
		AllocationUnit: npuv1alpha1.AllocationUnitDevice,
		Identity: npuv1alpha1.DescriptorIdentity{
			Source:             npuv1alpha1.IdentitySourceUUID,
			StableAcrossReboot: true,
		},
		DeviceNodes: []string{"/dev/nvidia0", "/dev/nvidiactl"},
		Backends:    []string{npuv1alpha1.BackendDevicePlugin, npuv1alpha1.BackendDRA},
	}
}

func TestValidateAcceptsValidSpec(t *testing.T) {
	if errs := Validate(validSpec()); len(errs) != 0 {
		t.Fatalf("유효 표본이 거절됐다: %v", errs)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*npuv1alpha1.AcceleratorDescriptorSpec)
		wantSub string
	}{
		{
			name:    "벤더 공란",
			mutate:  func(s *npuv1alpha1.AcceleratorDescriptorSpec) { s.Vendor = "" },
			wantSub: "vendor",
		},
		{
			name:    "모르는 할당 단위",
			mutate:  func(s *npuv1alpha1.AcceleratorDescriptorSpec) { s.AllocationUnit = "half" },
			wantSub: "allocationUnit",
		},
		{
			name:    "모르는 식별자 출처",
			mutate:  func(s *npuv1alpha1.AcceleratorDescriptorSpec) { s.Identity.Source = "hostname" },
			wantSub: "identity.source",
		},
		{
			name:    "장치 노드 없음",
			mutate:  func(s *npuv1alpha1.AcceleratorDescriptorSpec) { s.DeviceNodes = nil },
			wantSub: "deviceNodes",
		},
		{
			name:    "장치 노드가 절대경로 아님",
			mutate:  func(s *npuv1alpha1.AcceleratorDescriptorSpec) { s.DeviceNodes = []string{"nvidia0"} },
			wantSub: "deviceNodes[0]",
		},
		{
			name:    "모르는 backend",
			mutate:  func(s *npuv1alpha1.AcceleratorDescriptorSpec) { s.Backends = []string{"cdi"} },
			wantSub: "backends[0]",
		},
		{
			name: "리셋이 필요한데 수단 없음",
			mutate: func(s *npuv1alpha1.AcceleratorDescriptorSpec) {
				s.Cleanup.RequiresDeviceReset = true
			},
			wantSub: "cleanup.resetCommand",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSpec()
			c.mutate(&s)
			errs := Validate(s)
			if len(errs) == 0 {
				t.Fatalf("거절돼야 하는데 통과했다")
			}
			if !strings.Contains(errs.ToAggregate().Error(), c.wantSub) {
				t.Errorf("사유에 %q 가 없다: %v", c.wantSub, errs.ToAggregate())
			}
		})
	}
}
