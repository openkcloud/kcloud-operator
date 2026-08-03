// ============================================================
// compile.go: descriptor 를 backend 런타임 설정으로 컴파일한다
// 상세: 이 파일이 "생성기" 의 본체다. 설정의 모든 값은 descriptor 에서만 나온다 -
//
//	벤더 기본값을 여기서 채우면 명세서가 단일 출처라는 성질이 깨진다.
//
// 생성일: 2026-08-11
// ============================================================
package descriptor

import (
	"encoding/json"
	"fmt"
	"strings"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// devicePlaceholder 는 장치 노드 glob 안에서 장치 식별자로 치환되는 자리표시자다.
const devicePlaceholder = "{{id}}"

// dpDevice 와 dpConfig 는 일반 device-plugin 런타임의 설정 스키마와 1:1 대응한다.
type dpDevice struct {
	ID        string   `json:"id"`
	NodeGlobs []string `json:"nodeGlobs"`
}

type dpConfig struct {
	ResourceName string     `json:"resourceName"`
	Devices      []dpDevice `json:"devices"`
}

// ResourceNameFor 는 descriptor 가 광고할 리소스명이다.
func ResourceNameFor(spec npuv1alpha1.AcceleratorDescriptorSpec) string {
	return spec.Vendor + "/" + spec.Product
}

// CompileDevicePlugin 은 descriptor 와 실측 장치 목록으로 device-plugin 설정을 만든다.
// deviceIDs 는 NodeDeviceReport 가 준 실제 장치 식별자다 - descriptor 는 형식을 정하고
// 개수는 노드가 정한다.
func CompileDevicePlugin(spec npuv1alpha1.AcceleratorDescriptorSpec, deviceIDs []string) ([]byte, error) {
	if errs := Validate(spec); len(errs) > 0 {
		return nil, fmt.Errorf("descriptor 정합 오류: %w", errs.ToAggregate())
	}
	if len(deviceIDs) == 0 {
		return nil, fmt.Errorf("장치 목록이 비었다 — 컴파일할 것이 없다")
	}
	// 치환자가 없으면 장치가 여럿일 때 전부 같은 노드를 잡는다 - 두 장치를 준
	// 것처럼 광고하고 실제로는 하나를 두 번 준다. 장치가 하나면 문제되지 않으므로
	// 여기(장치 목록을 아는 자리)에서만 판정할 수 있다.
	//
	// 언어의 한계: 고정 노드만 가진 descriptor(예: nvidia-a30 archetype, identity.source
	// 가 uuid 라 {{id}} 를 못 쓴다)는 이 거절 때문에 장치 하나까지만 표현할 수 있다.
	// NVIDIA 장치 노드는 실제로 /dev/nvidia0, /dev/nvidia1 처럼 인덱스인데, descriptor
	// 언어에는 식별자(identity.source)와 별개인 "인덱스 치환자" 가 없어 이 인덱스를
	// 표현할 길이 없다. 다장치 NVIDIA 구성을 담으려면 언어에 인덱스 치환자를 추가해야
	// 한다.
	if len(deviceIDs) > 1 && !hasPlaceholder(spec.DeviceNodes) {
		return nil, fmt.Errorf(
			"장치가 %d개인데 장치 노드에 %s 치환자가 없다 — 모든 장치가 같은 노드를 잡는다",
			len(deviceIDs), devicePlaceholder)
	}

	cfg := dpConfig{ResourceName: ResourceNameFor(spec)}
	for _, id := range deviceIDs {
		globs := make([]string, 0, len(spec.DeviceNodes))
		for _, g := range spec.DeviceNodes {
			globs = append(globs, strings.ReplaceAll(g, devicePlaceholder, id))
		}
		cfg.Devices = append(cfg.Devices, dpDevice{ID: id, NodeGlobs: globs})
	}
	return json.Marshal(cfg)
}

// draConfig 는 일반 DRA 런타임의 설정 스키마와 1:1 대응한다.
type draConfig struct {
	DriverName  string     `json:"driverName"`
	DeviceClass string     `json:"deviceClass"`
	Devices     []dpDevice `json:"devices"`
}

// DriverNameFor 는 이 descriptor 가 발행할 DRA 드라이버 이름이다.
// ResourceSlice.spec.driver 와 DeviceClass 셀렉터가 이 값으로 맞물린다.
func DriverNameFor(spec npuv1alpha1.AcceleratorDescriptorSpec) string {
	return spec.Product + "." + spec.Vendor + ".kcloud.ai"
}

// CompileDRA 는 descriptor 와 실측 장치 목록으로 DRA 런타임 설정을 만든다.
// 실현 가능성 판정은 FeasibleBackends 와 같은 규칙을 쓴다 - 두 곳이 갈리면
// 검증기가 통과시킨 것을 컴파일러가 거절하는 일이 생긴다.
func CompileDRA(spec npuv1alpha1.AcceleratorDescriptorSpec, deviceIDs []string) ([]byte, error) {
	probe := spec
	probe.Backends = []string{npuv1alpha1.BackendDRA}
	ok, refusals := FeasibleBackends(probe)
	if len(ok) == 0 {
		if len(refusals) > 0 {
			return nil, fmt.Errorf("DRA backend 생성 불가: %s", refusals[0].Reason)
		}
		return nil, fmt.Errorf("DRA backend 생성 불가")
	}
	if len(deviceIDs) == 0 {
		return nil, fmt.Errorf("장치 목록이 비었다 — 컴파일할 것이 없다")
	}
	if len(deviceIDs) > 1 && !hasPlaceholder(spec.DeviceNodes) {
		return nil, fmt.Errorf(
			"장치가 %d개인데 장치 노드에 %s 치환자가 없다 — 모든 장치가 같은 노드를 잡는다",
			len(deviceIDs), devicePlaceholder)
	}

	cfg := draConfig{
		DriverName:  DriverNameFor(spec),
		DeviceClass: DriverNameFor(spec),
	}
	for _, id := range deviceIDs {
		globs := make([]string, 0, len(spec.DeviceNodes))
		for _, g := range spec.DeviceNodes {
			globs = append(globs, strings.ReplaceAll(g, devicePlaceholder, id))
		}
		cfg.Devices = append(cfg.Devices, dpDevice{ID: id, NodeGlobs: globs})
	}
	return json.Marshal(cfg)
}

// hasPlaceholder 는 장치 노드 목록 중 하나라도 치환자를 담고 있는지 본다.
func hasPlaceholder(nodes []string) bool {
	for _, n := range nodes {
		if strings.Contains(n, devicePlaceholder) {
			return true
		}
	}
	return false
}
