// ============================================================
// validate.go: descriptor 정합 검증
// 상세: CRD 스키마(enum·MinItems)가 못 잡는 필드 간 정합만 본다. 클러스터를 보지 않는
//
//	순수 함수라 어느 호출부(webhook·컨트롤러·CLI)에서든 같은 판정을 낸다.
//
// 생성일: 2026-08-10
// ============================================================
package descriptor

import (
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

var (
	validUnits = map[string]bool{
		npuv1alpha1.AllocationUnitDevice:    true,
		npuv1alpha1.AllocationUnitPartition: true,
	}
	validSources = map[string]bool{
		npuv1alpha1.IdentitySourcePCIAddress: true,
		npuv1alpha1.IdentitySourceUUID:       true,
		npuv1alpha1.IdentitySourceSerial:     true,
	}
	validBackends = map[string]bool{
		npuv1alpha1.BackendDevicePlugin: true,
		npuv1alpha1.BackendDRA:          true,
	}
)

// Validate 는 descriptor spec 의 필드 간 정합을 본다. 통과하면 빈 목록.
func Validate(spec npuv1alpha1.AcceleratorDescriptorSpec) field.ErrorList {
	var errs field.ErrorList
	p := field.NewPath("spec")

	if strings.TrimSpace(spec.Vendor) == "" {
		errs = append(errs, field.Required(p.Child("vendor"), "벤더 키가 필요하다"))
	} else if msgs := validation.IsDNS1123Subdomain(spec.Vendor); len(msgs) > 0 {
		// 벤더·제품이 그대로 확장 리소스명 vendor/product 가 된다. 여기서 안 막으면
		// kubelet 이 광고를 거절하는 자리까지 가서야 드러난다.
		errs = append(errs, field.Invalid(p.Child("vendor"), spec.Vendor, msgs[0]))
	}
	if strings.TrimSpace(spec.Product) == "" {
		errs = append(errs, field.Required(p.Child("product"), "제품 키가 필요하다"))
	} else if msgs := validation.IsQualifiedName(spec.Product); len(msgs) > 0 {
		// product 는 리소스명의 뒤 토막이다. 쿠버네티스는 이 자리에 대문자를 허용하므로
		// DNS-1123 label 로 좁히면 A30 같은 정당한 이름을 거절하게 된다(false reject).
		errs = append(errs, field.Invalid(p.Child("product"), spec.Product, msgs[0]))
	}
	// product 자체는 "a/30" 처럼 슬래시 하나를 담아도 IsQualifiedName 을 홀로 통과한다
	// (prefix/name 두 토막으로 읽히므로). 실제로 광고되는 값은 vendor+"/"+product 이고
	// 거기엔 이미 구분자가 하나 있어 product 안에 또 슬래시가 있으면 세 토막이 된다.
	if strings.Contains(spec.Product, "/") {
		errs = append(errs, field.Invalid(p.Child("product"), spec.Product,
			"product 에 '/' 를 담을 수 없다 — vendor 와 합친 리소스명의 구분자가 둘이 된다"))
	}
	if !validUnits[spec.AllocationUnit] {
		errs = append(errs, field.NotSupported(p.Child("allocationUnit"), spec.AllocationUnit,
			[]string{npuv1alpha1.AllocationUnitDevice, npuv1alpha1.AllocationUnitPartition}))
	}
	if !validSources[spec.Identity.Source] {
		errs = append(errs, field.NotSupported(p.Child("identity").Child("source"), spec.Identity.Source,
			[]string{npuv1alpha1.IdentitySourcePCIAddress, npuv1alpha1.IdentitySourceUUID,
				npuv1alpha1.IdentitySourceSerial}))
	}

	// 주입할 장치가 없으면 어느 backend 로도 갈 수 없다.
	if len(spec.DeviceNodes) == 0 {
		errs = append(errs, field.Required(p.Child("deviceNodes"), "주입할 장치 노드가 최소 하나 필요하다"))
	}
	for i, n := range spec.DeviceNodes {
		np := p.Child("deviceNodes").Index(i)
		switch {
		case !strings.HasPrefix(n, "/"):
			errs = append(errs, field.Invalid(np, n, "절대경로여야 한다"))
		// path.Clean 이 원문과 다르면 .. 나 중복 슬래시가 섞여 있다는 뜻이다.
		// glob 문자는 Clean 이 건드리지 않으므로 그대로 비교할 수 있다.
		case path.Clean(n) != n:
			errs = append(errs, field.Invalid(np, n,
				"정규화된 경로여야 한다 — 상위 참조나 중복 슬래시가 섞이면 "+
					"선언한 것과 다른 파일이 주입될 수 있다"))
		case !strings.HasPrefix(n, "/dev/"):
			errs = append(errs, field.Invalid(np, n, "/dev 아래여야 한다"))
		}
	}
	// 중복 선언은 배포물에서 중복 항목이 된다. 조용히 접지 않고 거절한다 -
	// 관리자가 두 번 쓴 것은 대개 다른 것을 쓰려던 실수다.
	if dup := firstDuplicate(spec.DeviceNodes); dup != "" {
		errs = append(errs, field.Duplicate(p.Child("deviceNodes"), dup))
	}

	if len(spec.Backends) == 0 {
		errs = append(errs, field.Required(p.Child("backends"), "대상 backend 가 최소 하나 필요하다"))
	}
	for i, b := range spec.Backends {
		if !validBackends[b] {
			errs = append(errs, field.NotSupported(p.Child("backends").Index(i), b,
				[]string{npuv1alpha1.BackendDevicePlugin, npuv1alpha1.BackendDRA}))
		}
	}
	if dup := firstDuplicate(spec.Backends); dup != "" {
		errs = append(errs, field.Duplicate(p.Child("backends"), dup))
	}

	// 리셋이 필요하다고 선언했으면 수단이 있어야 한다. 없으면 반납 후 상태를 만들 길이 없다.
	if spec.Cleanup.RequiresDeviceReset && len(spec.Cleanup.ResetCommand) == 0 {
		errs = append(errs, field.Required(p.Child("cleanup").Child("resetCommand"),
			"장치 리셋이 필요하다고 선언했으면 리셋 수단이 있어야 한다"))
	}

	return errs
}

// firstDuplicate 는 처음 중복된 원소를 낸다. 없으면 빈 문자열.
func firstDuplicate(xs []string) string {
	seen := make(map[string]bool, len(xs))
	for _, x := range xs {
		if seen[x] {
			return x
		}
		seen[x] = true
	}
	return ""
}
