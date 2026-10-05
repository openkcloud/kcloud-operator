// ============================================================
// helm_presence_test.go: 차트 렌더 시험이 기대는 helm 전제를 한 자리에서 판정한다
// 상세: 차트를 렌더하는 시험들은 helm 이 PATH 에 없으면 건너뛴다. 로컬에서는 그게 맞지만
//
//	CI 에서 건너뛰는 것은 "환경이 없어 건너뜀" 이 아니라 "있어야 할 것이 없음" 이다 —
//	그 상태로 워크플로가 초록이면 차트 검사 묶음이 아무 신호 없이 사라진다. Round 9 에
//	실제로 그랬다(helm 미설치로 시험 11종이 건너뛰고 있었다).
//	그래서 판정을 helmBinary 하나로 모았다: CI 면 하드 실패, 아니면 건너뛴다.
//	TestCIWorkflowInstallsHelm 은 그 전제의 다른 쪽 — 워크플로 선언 자체가 사라지는 경우다.
//
// 생성일: 2026-10-05
// ============================================================
package controller

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// helmBinary 는 helm 실행 파일 경로를 돌려준다. 없으면 CI 에서는 하드 실패하고 로컬에서는
// 건너뛴다. **helm 부재를 판정하는 자리는 이 함수 하나뿐이다** — 자리를 나눠 두면 한 곳만
// 고치고 나머지가 옛 동작으로 남는다.
//
// internal/upgrade 에 같은 판정의 사본이 있다(kernel_image_targets_test.go 의
// upgradeHelmBinary). 패키지가 달라 공유할 수 없다 — 한쪽을 고치면 다른 쪽도 고칠 것.
func helmBinary(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("helm")
	if err == nil {
		return bin
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("CI 인데 helm 이 PATH 에 없다 — 차트 렌더 시험이 건너뛰면 그 검사들은 " +
			"신호를 내지 않는다. 워크플로가 helm 을 깔아야 한다(.github/workflows/test.yml 의 " +
			"설치 단계). 다른 CI 를 붙일 때도 그쪽에 같은 설치 단계가 필요하다")
	}
	t.Skip("helm 이 PATH 에 없다 — 차트 렌더 시험 건너뜀(CI 에서는 하드 실패한다)")
	return ""
}

// helmInstallPatterns 는 run 스크립트가 helm 을 **설치** 한다고 볼 모양이다. `helm` 과
// `install` 이 같은 줄에 있다는 것만으로는 받지 않는다 — `helm install <chart>` 는 차트를
// 설치하는 명령이고 helm 자신은 이미 깔려 있어야 하므로, 그것을 설치 단계로 집계하면 가드가
// 거짓 초록을 준다(실측으로 확인된 경로다).
var helmInstallPatterns = []*regexp.Regexp{
	// 패키지 매니저: `apt-get install -y helm`, `snap install helm`, `brew install helm`,
	// `choco install kubernetes-helm`. install 뒤에 플래그가 몇 개 끼어도 받는다.
	// helm 뒤가 단어 문자·`.`·`-` 면 받지 않는다 — `install helm-test` 같은 차트 이름 제외.
	regexp.MustCompile(`install\s+(?:-\S+\s+)*(?:kubernetes-)?helm(?:$|[^\w.\-])`),
	// 공식 설치 스크립트와 tarball: get-helm-3, get_helm.sh, get.helm.sh.
	regexp.MustCompile(`get[-_.]helm`),
	// setup-helm 류를 action 대신 run 으로 쓰는 경우.
	regexp.MustCompile(`setup-helm`),
}

// workflowStepInstallsHelm 은 워크플로 단계 하나가 helm 을 설치하는지 판정한다.
// uses·run 을 글자 그대로 고정하지 않는다 — 보는 것은 "helm 이 설치된다" 는 뜻뿐이다.
func workflowStepInstallsHelm(uses, run string) bool {
	// 이름에 helm 이 든 action 은 helm 을 깔아 주는 action 이다(azure/setup-helm 등).
	if strings.Contains(strings.ToLower(uses), "helm") {
		return true
	}
	lower := strings.ToLower(run)
	for _, re := range helmInstallPatterns {
		if re.MatchString(lower) {
			return true
		}
	}
	return false
}

// 판정이 설치와 사용을 구분하는지 고정한다. 이 구분이 무너지면 가드가 거짓 초록을 준다 —
// 워크플로에 `helm install <chart>` 만 남아도 "helm 을 깐다" 고 집계된다.
func TestWorkflowStepInstallsHelmDistinguishesInstallFromUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		uses string
		run  string
		want bool
	}{
		{"setup action", "azure/setup-helm@v4", "", true},
		{"공식 스크립트", "", "curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash", true},
		{"tarball", "", "wget https://get.helm.sh/helm-v3.14.0-linux-amd64.tar.gz", true},
		{"apt 플래그 포함", "", "sudo apt-get install -y helm\n", true},
		{"apt 플래그 없음", "", "sudo apt-get install helm", true},
		{"snap", "", "sudo snap install helm --classic", true},
		{"choco", "", "choco install kubernetes-helm", true},

		{"차트 설치", "", "helm install my-chart ./deploy/helm", false},
		{"차트 업그레이드", "", "helm upgrade --install my-chart ./deploy/helm", false},
		{"차트 이름이 helm 으로 시작", "", "helm install helm-test ./deploy/helm", false},
		{"렌더만", "", "helm template t ./deploy/helm", false},
		{"helm 무관 설치", "", "sudo apt-get install -y python3-yaml", false},
		{"시험 실행", "", "go mod tidy\nmake test", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowStepInstallsHelm(tc.uses, tc.run); got != tc.want {
				t.Errorf("workflowStepInstallsHelm(%q, %q) = %v, want %v", tc.uses, tc.run, got, tc.want)
			}
		})
	}
}

// helmBinary 는 helm 이 **실제로** 없는 것을 잡는다. 이 시험은 그 전제의 다른 쪽 — 워크플로에
// 설치 **선언** 이 있는지다. 설치 단계를 지우면 CI 에서 helmBinary 가 하드 실패하지만, 그건
// 러너에서야 드러난다. 이 시험은 같은 결함을 로컬 `make test` 에서 먼저 잡는다.
//
// 설치 방식은 글자 그대로 고정하지 않는다 — azure/setup-helm 이든 `apt-get install helm` 이든
// `curl … get-helm-3 | bash` 든 통과한다(판정은 workflowStepInstallsHelm).
//
// lint.yml 은 일부러 보지 않는다. 그쪽 helm 소비자는 footprint-check 이고, 그 게이트는 helm 이
// 없으면 건너뛰지 않고 하드 실패한다("static null 로 게이트를 통과시키지 않도록 종료").
// 조용히 사라지는 전제는 test.yml 쪽뿐이다.
func TestCIWorkflowInstallsHelm(t *testing.T) {
	const wfPath = "../../.github/workflows/test.yml"
	raw, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("%s 읽기 실패: %v", wfPath, err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `json:"name"`
				Uses string `json:"uses"`
				Run  string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("%s 파싱 실패: %v", wfPath, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s: job 이 0개다 — 대조가 비었으면 통과가 아니다", wfPath)
	}
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if workflowStepInstallsHelm(step.Uses, step.Run) {
				t.Logf("%s: helm 설치 단계 발견 — name=%q uses=%q", wfPath, step.Name, step.Uses)
				return
			}
		}
	}
	t.Errorf("%s 에 helm 을 설치하는 단계가 없다 — 차트 렌더 시험이 건너뛰어 조용히 "+
		"빠지고 워크플로는 초록으로 남는다. 설치 단계를 되살릴 것", wfPath)
}
