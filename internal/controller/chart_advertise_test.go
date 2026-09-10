// ============================================================
// chart_advertise_test.go: 광고 주체 축이 차트에서 실제로 노출되는지 고정한다.
//
// 상세: advertiseBy 는 NPUClusterPolicy 의 벤더 블록마다 inline 으로 들어 있다.
//
//	API 에는 있는데 차트 템플릿이 그 값을 렌더하지 않으면, 값 파일에 적어도
//	CR 에 닿지 않아 아무 일도 일어나지 않는다 — 조용히 무시되는 설정이 된다.
//	벤더가 늘어날 때 이 노출을 빠뜨리는 것이 실제로 일어날 수 있는 누락이다.
//
// 범위: 템플릿이 그 값을 참조하는지, 기본 values 에 키가 있는지까지 본다.
//
//	렌더 결과가 API 스키마와 맞는지는 helm 이 필요하므로 여기서 보지 않는다.
//
// 라인 차이: 이 릴리스 라인(k8s 1.28)에는 DRA 배포 프로파일이 없다. 광고 축 자체는
//
//	같은 코드로 받되 dra 요청은 번역에서 거절되므로, 프로파일 파일과 그 시험은
//	1.34 라인에만 있다.
//
// 생성일: 2026-08-07
// ============================================================
package controller

import (
	"os"
	"strings"
	"testing"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// chartAdvertiseVendors 는 광고 축을 가진 벤더의 **values 경로**다. 컨트롤러가 읽는
// advertiseVendors() 맵의 키와 1:1 이어야 한다 — 그쪽 키는 벤더 이름이고 이쪽은
// 값 파일에서의 위치라 이름이 다를 수 있다(rngd 는 furiosa 아래에 있다).
var chartAdvertiseVendors = []string{
	"nvidia",
	"furiosa",
	"furiosa.rngd",
	"tenstorrent",
}

// TestAdvertiseVendorCountMatchesChart 는 컨트롤러가 실제로 읽는 벤더 수와 차트
// 노출 목록의 길이가 같은지 본다. 벤더를 늘리면서 차트 노출을 빠뜨리면 여기서 걸린다.
func TestAdvertiseVendorCountMatchesChart(t *testing.T) {
	got := len(advertiseVendors(&npuv1alpha1.NPUClusterPolicy{}))
	if got != len(chartAdvertiseVendors) {
		t.Errorf("컨트롤러가 읽는 벤더 %d 개, 차트 노출 목록 %d 개 — "+
			"벤더를 늘렸다면 clusterpolicy.yaml·values.yaml·이 목록을 함께 고칠 것",
			got, len(chartAdvertiseVendors))
	}
}

// TestChartExposesAdvertiseBy 는 각 벤더의 advertiseBy 가 템플릿과 기본 values 에
// 모두 있는지 본다. 둘 중 하나만 있으면 값을 적어도 CR 에 닿지 않거나, 값 파일에
// 적을 자리가 없다.
func TestChartExposesAdvertiseBy(t *testing.T) {
	tpl, err := os.ReadFile("../../deploy/helm/templates/clusterpolicy.yaml")
	if err != nil {
		t.Fatalf("clusterpolicy.yaml 읽기 실패: %v", err)
	}
	vals, err := os.ReadFile("../../deploy/helm/values.yaml")
	if err != nil {
		t.Fatalf("values.yaml 읽기 실패: %v", err)
	}
	body := string(tpl)
	for _, v := range chartAdvertiseVendors {
		want := ".Values." + v + ".advertiseBy"
		if !strings.Contains(body, want) {
			t.Errorf("clusterpolicy.yaml 이 %s 를 렌더하지 않는다 — "+
				"값 파일에 적어도 CR 에 닿지 않는다", want)
		}
	}
	// 기본 values 에 키가 몇 개 선언돼 있는지로 본다(중첩 경로라 키 이름은 같다).
	// 주석 줄에도 같은 문자열이 나오므로 실제 선언만 센다.
	declared := 0
	for _, line := range strings.Split(string(vals), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") && strings.HasPrefix(trimmed, "advertiseBy:") {
			declared++
		}
	}
	if declared != len(chartAdvertiseVendors) {
		t.Errorf("values.yaml 의 advertiseBy 선언이 %d 개, 벤더는 %d 개다",
			declared, len(chartAdvertiseVendors))
	}
}
