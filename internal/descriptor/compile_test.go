// ============================================================
// compile_test.go: descriptor → backend 설정 컴파일 시험
// 상세: 설정의 모든 값이 descriptor 에서만 나와야 한다. 어디선가 벤더 기본값이
//
//	끼어들면 "명세서가 단일 출처" 라는 주장이 깨진다.
//
// 생성일: 2026-08-11
// ============================================================
package descriptor

import (
	"encoding/json"
	"strings"
	"testing"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

func rngdSpec() npuv1alpha1.AcceleratorDescriptorSpec {
	return npuv1alpha1.AcceleratorDescriptorSpec{
		Vendor:         "rngd",
		Product:        "rngd",
		AllocationUnit: npuv1alpha1.AllocationUnitDevice,
		Identity: npuv1alpha1.DescriptorIdentity{
			Source:             npuv1alpha1.IdentitySourcePCIAddress,
			StableAcrossReboot: true,
		},
		DeviceNodes: []string{"/dev/rngd/{{id}}*"},
		Backends:    []string{npuv1alpha1.BackendDevicePlugin, npuv1alpha1.BackendDRA},
	}
}

func TestCompileDevicePluginUsesDescriptorOnly(t *testing.T) {
	out, err := CompileDevicePlugin(rngdSpec(), []string{"npu0", "npu1"})
	if err != nil {
		t.Fatalf("컴파일 실패: %v", err)
	}
	var got struct {
		ResourceName string `json:"resourceName"`
		Devices      []struct {
			ID        string   `json:"id"`
			NodeGlobs []string `json:"nodeGlobs"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("설정이 JSON 이 아니다: %v", err)
	}
	if got.ResourceName != "rngd/rngd" {
		t.Errorf("리소스명이 descriptor 에서 나오지 않았다: %q", got.ResourceName)
	}
	if len(got.Devices) != 2 {
		t.Fatalf("장치 2개여야 한다: %+v", got.Devices)
	}
	// {{id}} 가 장치별로 치환돼야 한다. 치환이 안 되면 두 장치가 같은 노드를 잡는다.
	if got.Devices[0].NodeGlobs[0] != "/dev/rngd/npu0*" {
		t.Errorf("첫 장치 glob 치환 실패: %v", got.Devices[0].NodeGlobs)
	}
	if got.Devices[1].NodeGlobs[0] != "/dev/rngd/npu1*" {
		t.Errorf("둘째 장치 glob 치환 실패: %v", got.Devices[1].NodeGlobs)
	}
}

// 장치 목록이 비면 컴파일하지 않는다. 빈 광고를 내면 노드가 0 을 광고한 것처럼 보인다.
func TestCompileDevicePluginRejectsEmptyDevices(t *testing.T) {
	_, err := CompileDevicePlugin(rngdSpec(), nil)
	if err == nil || !strings.Contains(err.Error(), "장치") {
		t.Fatalf("빈 장치 목록은 거절돼야 한다: %v", err)
	}
}

// 정합이 깨진 descriptor 는 컴파일 자체를 거절한다.
func TestCompileDevicePluginRejectsInvalidSpec(t *testing.T) {
	s := rngdSpec()
	s.Vendor = ""
	if _, err := CompileDevicePlugin(s, []string{"npu0"}); err == nil {
		t.Fatal("정합 오류인데 통과했다")
	}
}

func TestCompileDRAUsesDescriptorOnly(t *testing.T) {
	out, err := CompileDRA(rngdSpec(), []string{"npu0", "npu1"})
	if err != nil {
		t.Fatalf("컴파일 실패: %v", err)
	}
	var got struct {
		DriverName  string `json:"driverName"`
		DeviceClass string `json:"deviceClass"`
		Devices     []struct {
			ID        string   `json:"id"`
			NodeGlobs []string `json:"nodeGlobs"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("설정이 JSON 이 아니다: %v", err)
	}
	if got.DriverName != DriverNameFor(rngdSpec()) {
		t.Errorf("드라이버 이름이 descriptor 에서 나오지 않았다: %q", got.DriverName)
	}
	if len(got.Devices) != 2 || got.Devices[1].NodeGlobs[0] != "/dev/rngd/npu1*" {
		t.Errorf("장치 목록이 잘못 컴파일됐다: %+v", got.Devices)
	}
}

// 재부팅을 건너 안정하지 않은 식별자면 DRA 로 컴파일하지 않는다. 정적 검증기와 같은 판정이어야 한다.
func TestCompileDRARefusesUnstableIdentity(t *testing.T) {
	s := rngdSpec()
	s.Identity.StableAcrossReboot = false
	if _, err := CompileDRA(s, []string{"npu0"}); err == nil {
		t.Fatal("불안정 식별자인데 통과했다")
	}
}

// 장치가 하나면 치환자가 없어도 컴파일된다. 이 경계가 무너지면 고정 노드
// descriptor 가 전부 막힌다.
func TestCompileDevicePluginAcceptsSingleFixedNode(t *testing.T) {
	s := validSpec()
	s.DeviceNodes = []string{"/dev/nvidia0", "/dev/nvidiactl"}
	if _, err := CompileDevicePlugin(s, []string{"only"}); err != nil {
		t.Fatalf("단일 장치는 통과해야 한다: %v", err)
	}
}
