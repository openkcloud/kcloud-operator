// ============================================================
// workflow_steps_test.go: GitHub Actions 워크플로 YAML 의 단계 목록을 읽는 시험 헬퍼
// 상세: 워크플로를 읽는 시험이 둘(helm 설치 단계, make 게이트 호출)이라 Jobs/Steps 구조체와
//
//	"job 0개면 실패" 가드를 한 곳에 둔다. 같은 질문의 답이 두 곳에 있으면 한쪽만 고치고
//	고쳤다고 믿게 된다.
//
// 생성일: 2026-10-06
// ============================================================
package controller

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// workflowStep 은 워크플로 단계 중 시험이 보는 세 필드다.
type workflowStep struct {
	Name string `json:"name"`
	Uses string `json:"uses"`
	Run  string `json:"run"`
}

// workflowSteps 는 워크플로 파일의 모든 job 의 단계를 한 목록으로 돌려준다. job 이 0개면
// 대조가 비었으므로 실패한다 — 빈 목록으로 돌아가면 호출자의 "단계가 없다" 판정이 참이 된다.
func workflowSteps(t *testing.T, wfPath string) []workflowStep {
	t.Helper()
	raw, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("%s 읽기 실패: %v", wfPath, err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []workflowStep `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("%s 파싱 실패: %v", wfPath, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s: job 이 0개다 — 대조가 비었으면 통과가 아니다", wfPath)
	}
	var out []workflowStep
	for _, job := range wf.Jobs {
		out = append(out, job.Steps...)
	}
	return out
}
