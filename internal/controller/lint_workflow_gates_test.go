// ============================================================
// lint_workflow_gates_test.go: lint 워크플로가 Makefile 게이트를 실제로 부르는지 고정한다.
// 상세: make 타깃은 있어도 CI 가 부르지 않으면 한 번도 돌지 않은 채 초록이 된다. 워크플로
//
//	YAML 의 run 단계에서 `make <타깃>` 호출을 찾고, 단계가 사라지면 여기서 실패한다.
//
// 생성일: 2026-10-05
// ============================================================
package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// workflowRunsMake 는 워크플로 파일의 어느 run 단계가 `make <target>` 을 부르는지 돌려준다.
// run 블록은 여러 줄일 수 있어 줄 단위로 본다. job 이 0개면 대조가 비었으므로 실패한다.
func workflowRunsMake(t *testing.T, wfPath, target string) bool {
	t.Helper()
	raw, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("%s 읽기 실패: %v", wfPath, err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("%s 파싱 실패: %v", wfPath, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s: job 이 0개다 — 대조가 비었으면 통과가 아니다", wfPath)
	}
	want := "make " + target
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			for _, line := range strings.Split(step.Run, "\n") {
				line = strings.TrimSpace(line)
				if line == want || strings.HasPrefix(line, want+" ") {
					return true
				}
			}
		}
	}
	return false
}

func TestLintWorkflowRunsDocCiteCheck(t *testing.T) {
	const wfPath = "../../.github/workflows/lint.yml"
	if !workflowRunsMake(t, wfPath, "doc-cite-check") {
		t.Errorf("%s 에 `make doc-cite-check` 단계가 없다 — 타깃은 있어도 CI 가 부르지 않으면 "+
			"한 번도 돌지 않은 채 초록이 된다", wfPath)
	}
}

// 증명: 판별은 `make <타깃>` 정확 일치(뒤에 인자가 붙는 것은 허용)이고 접미가 다른 타깃은 아니다.
// 깨는 뮤테이션: HasPrefix 만 남기면 (b) 가 true 가 되어 실패한다.
func TestWorkflowRunsMakeMatchesExactTarget(t *testing.T) {
	cases := []struct {
		name string
		run  string
		want bool
	}{
		{"정확 일치", "make doc-cite-check", true},
		{"인자 동반", "make doc-cite-check OTHER=1", true},
		{"접미 다른 타깃", "make doc-cite-check-extra", false},
		{"여러 줄 블록", "go mod tidy\n  make doc-cite-check\n", true},
		{"다른 타깃만", "make test", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			wf := filepath.Join(dir, "wf.yml")
			body := "jobs:\n  j:\n    steps:\n      - name: s\n        run: |\n"
			for _, line := range strings.Split(strings.TrimRight(c.run, "\n"), "\n") {
				body += "          " + line + "\n"
			}
			if err := os.WriteFile(wf, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := workflowRunsMake(t, wf, "doc-cite-check"); got != c.want {
				t.Fatalf("run=%q: got %v want %v", c.run, got, c.want)
			}
		})
	}
}
