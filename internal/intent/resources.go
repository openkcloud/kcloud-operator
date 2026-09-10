// ============================================================
// resources.go: 벤더↔device-plugin 리소스명 카탈로그
// 상세: 추상 모드를 클러스터가 실제로 광고하는 extended resource 이름으로 옮기는 유일한 지점.
// NVIDIA mixed MIG 만 profile 별 리소스명(nvidia.com/mig-<profile>)을 만들고, 나머지 벤더는
// 파티션을 만들어도 flat 하나로 광고한다 — 그래서 파티션 노드는 리소스명이 아니라 노드 후보
// 목록으로 구분한다.
// furiosa 는 한 벤더 아래 제품이 둘(rngd/warboy)이라 vendor 만으론 리소스명을 못 정한다 — product
// 로 구분한다. 제품이 하나뿐인 벤더(nvidia/rebellions/tenstorrent)는 product 를 안 줘도 풀린다.
// 생성일: 2026-07-29 | 수정일: 2026-07-29
// ============================================================
package intent

import (
	"fmt"
	"sort"
	"strings"

	"kcloud-operator/api/v1alpha1"
)

// migResourcePrefix 는 nvidia device-plugin 이 mig-strategy=mixed 에서 쓰는 접두다.
const migResourcePrefix = "nvidia.com/mig-"

// productResource 는 (벤더, product) → 전체 장치 리소스명이다. product 가 "" 인 항목은
// 그 벤더의 유일한 제품이라 product 생략을 허용한다는 뜻이다. 값은 클러스터가 광고 중인
// 이름이 정본이다(furiosa 는 npuclusterpolicy_controller.go 의 Warboy/RNGD 리소스명과 대조 확인).
var productResource = map[string]map[string]string{
	"nvidia":      {"": "nvidia.com/gpu"},
	"furiosa":     {"rngd": "furiosa.ai/rngd", "warboy": "beta.furiosa.ai/npu"},
	"rebellions":  {"": "rebellions.ai/ATOM"},
	"tenstorrent": {"": "tenstorrent.com/blackhole"},
}

// WholeDeviceResource 는 (vendor, product) 의 전체 장치 리소스명이다. 모르는 벤더는 "", nil 을
// 돌려준다(그냥 미지원). 벤더 아래 제품이 하나뿐이면 product 값은 무시한다 — AcceleratorMapping.
// Product 는 그 벤더에선 그냥 사람이 읽는 표기(예 nvidia 의 "A30")일 뿐 구분할 게 없기 때문이다.
// 제품이 여럿인 벤더(furiosa)만 product 로 실제 분기하며, 비었거나 못 알아보는 값이면 조용히
// 아무거나 고르지 않고 에러를 돌려준다 — furiosa 를 product 없이 물으면 RNGD 인지 Warboy 인지
// 알 길이 없다.
func WholeDeviceResource(vendor, product string) (string, error) {
	products, ok := productResource[strings.ToLower(vendor)]
	if !ok {
		return "", nil
	}
	if len(products) == 1 {
		for _, res := range products {
			return res, nil
		}
	}
	if res, ok := products[strings.ToLower(product)]; ok {
		return res, nil
	}
	names := make([]string, 0, len(products))
	for p := range products {
		names = append(names, p)
	}
	sort.Strings(names)
	return "", fmt.Errorf("intent: %s: cannot determine resource name — product must be one of %v, got %q", vendor, names, product)
}

// KnownVendor 는 이 카탈로그가 그 벤더의 리소스명을 아는지다. 모르는 벤더는 어떤 리소스를
// 봐야 하는지 알 수 없으므로 "그 벤더의 광고가 0 이다" 를 증명할 방법이 없다 — 호출자는
// 찾은 게 없는 것과 0 임을 확인한 것을 구분해야 한다.
func KnownVendor(vendor string) bool {
	_, ok := productResource[strings.ToLower(vendor)]
	return ok
}

// PartitionResource 는 nativeProfile 파티션이 광고되는 리소스명이다.
func PartitionResource(vendor, product, nativeProfile string) (string, error) {
	if strings.EqualFold(vendor, "nvidia") && nativeProfile != "" {
		return migResourcePrefix + nativeProfile, nil
	}
	return WholeDeviceResource(vendor, product)
}

// ResourceFor 는 추상 모드에 맞는 리소스명이다. 분할 계열만 profile 을 본다.
func ResourceFor(m v1alpha1.AcceleratorMapping, mode string) (string, error) {
	switch mode {
	case v1alpha1.AccessModePartitioned, v1alpha1.AccessModePartitionedShared:
		return PartitionResource(m.Vendor, m.Product, m.NativeProfile)
	default:
		return WholeDeviceResource(m.Vendor, m.Product)
	}
}

// VendorForResource 는 리소스명에서 벤더를 되짚는다. 가속기 리소스가 아니면 "".
func VendorForResource(name string) string {
	if strings.HasPrefix(name, migResourcePrefix) {
		return "nvidia"
	}
	for vendor, products := range productResource {
		for _, res := range products {
			if res == name {
				return vendor
			}
		}
	}
	return ""
}
