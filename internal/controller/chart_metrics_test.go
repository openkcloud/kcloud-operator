// ============================================================
// chart_metrics_test.go: metrics 인자가 값에 따라 켜지고 기본은 꺼져 있는지 고정한다
// 상세: 제어면 비용 측정은 controller-runtime metrics 를 스크레이프한다. 그 통로가
//
//	차트에서 열리지 않으면 측정 자체가 불가능하고, 반대로 기본으로 열리면
//	라이브에 인증 없는 지표 포트가 생긴다. 둘 다 막는다.
//
// 생성일: 2026-08-10
// ============================================================
package controller

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func readChartFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/helm/" + rel)
	if err != nil {
		t.Fatalf("%s 읽기 실패: %v", rel, err)
	}
	return string(raw)
}

// metricsValues 는 values.yaml 의 metrics 블록만 파싱한다. 문자열 앵커(strings.Index)는
// "metrics:" 를 설명 주석줄에서도 찾아내므로, 주석과 실제 키 사이에 빈 줄 하나만 들어와도
// 엉뚱한 텍스트를 붙잡는다 — YAML 로 실제 파싱해 그 함정을 없앤다.
type metricsValues struct {
	Metrics *struct {
		Enabled bool `json:"enabled"`
		Port    int  `json:"port"`
		Secure  bool `json:"secure"`
	} `json:"metrics"`
}

// 기본값이 꺼져 있어야 한다. 켜져 있으면 라이브에 지표 포트가 생긴다.
func TestChartMetricsDisabledByDefault(t *testing.T) {
	v := readChartFile(t, "values.yaml")
	var cv metricsValues
	if err := yaml.Unmarshal([]byte(v), &cv); err != nil {
		t.Fatalf("values.yaml 파싱 실패: %v", err)
	}
	if cv.Metrics == nil {
		t.Fatal("values.yaml 에 metrics 블록이 없다")
	}
	if cv.Metrics.Enabled {
		t.Errorf("metrics 기본값이 false 가 아니다: %+v", *cv.Metrics)
	}
}

// 템플릿이 그 값으로 인자를 렌더해야 한다. 조건과 인자 둘 다 있어야 의미가 있다.
func TestChartRendersMetricsArgsWhenEnabled(t *testing.T) {
	d := readChartFile(t, "templates/deployment.yaml")
	for _, want := range []string{
		"if .Values.metrics.enabled",
		"--metrics-bind-address=:{{ .Values.metrics.port }}",
		"--metrics-secure={{ .Values.metrics.secure }}",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("deployment.yaml 에 %q 가 없다", want)
		}
	}
}
