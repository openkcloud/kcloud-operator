// ============================================================
// rbac_sync_test.go: 생성 RBAC 와 배포 RBAC 가 갈라지지 않는지 고정한다.
// 상세: `make manifests` 는 config/rbac/role.yaml 만 재생성한다 — 실제 배포는 손으로 관리하는
//
//	deploy/helm/templates/rbac.yaml 다. 둘이 갈라지면 kubebuilder 마커를 고쳐도 배포에는
//	반영되지 않는다(2026-08-04 라이브 핫루프의 절반 원인 — nodedevicereports/status 의
//	update/patch 가 생성본에만 있었다). 완전 자동 동기화는 이 시험의 범위 밖이다 — 이
//	컨트롤러가 실제로 쓰는 리소스만 좁혀서, 갈라짐을 드러내는 것만 한다. (npu.ai 전 리소스로
//	넓히면 acceleratorpartitionpolicies/npuclusterpolicies 의 create/delete 처럼 kubebuilder
//	마커에는 있지만 코드가 쓴 적 없는 오래된 항목까지 걸려 노이즈가 된다 — 그건 이 컨트롤러의
//	사고와 무관한 별개 정리 대상이다.)
//
// 생성일: 2026-08-04
// ============================================================
package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"
)

type rbacRule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

type rbacRuleList struct {
	Rules []rbacRule `json:"rules"`
}

// verbsByResource 는 규칙 목록을 리소스 이름 → 정렬된 verb 목록으로 접는다.
func verbsByResource(rules []rbacRule) map[string][]string {
	out := map[string][]string{}
	for _, r := range rules {
		for _, res := range r.Resources {
			verbs := append([]string{}, r.Verbs...)
			sort.Strings(verbs)
			out[res] = verbs
		}
	}
	return out
}

// generatedOperatorRules 는 controller-gen 이 만든 config/rbac/role.yaml 을 그대로 읽는다
// (템플릿 문법이 없는 순수 YAML).
func generatedOperatorRules(t *testing.T) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatalf("생성된 rbac 파일을 못 읽었다: %v", err)
	}
	var parsed rbacRuleList
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("생성된 rbac 파일을 YAML 로 못 읽었다: %v", err)
	}
	return verbsByResource(parsed.Rules)
}

// deployedOperatorRules 는 helm 차트에서 operator ClusterRole 의 rules 블록만 뽑아 읽는다.
// 파일 전체는 helm 템플릿(metadata.name 에 {{ }})이라 그대로 YAML 파싱이 안 된다 — rules
// 블록 자체는 템플릿 문법이 없는 순수 YAML 이라 그 부분만 문서 구분자(---)까지 잘라낸다.
func deployedOperatorRules(t *testing.T) map[string][]string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "helm", "templates", "rbac.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("helm rbac 파일을 못 읽었다: %v", err)
	}
	re := regexp.MustCompile(`(?s)name: \{\{ include "npu-operator\.fullname" \. \}\}-role\nrules:\n(.*?)\n---`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatalf("helm rbac 파일에서 operator ClusterRole 의 rules 블록을 못 찾았다 — " +
			"파일 구조가 바뀌었으면 이 시험의 정규식도 같이 바꿔야 한다")
	}
	var parsed rbacRuleList
	if err := yaml.Unmarshal(append([]byte("rules:\n"), m[1]...), &parsed); err != nil {
		t.Fatalf("helm rbac rules 블록을 YAML 로 못 읽었다: %v", err)
	}
	return verbsByResource(parsed.Rules)
}

// TestHelmRBACGrantsWhatMigObservationNeeds 는 이 컨트롤러의 kubebuilder RBAC 마커
// (migobservation_controller.go 의 nodedevicereports/nodedevicereports/status)가 생성한
// verb 집합이 실제 배포되는 helm 차트에도 그대로 있는지 본다. 완전한 두 파일 비교가 아니다
// (위 파일 주석 참고) — 이 컨트롤러가 실제로 쓰는 리소스만 좁혀서, 마커를 고치고도 배포를
// 잊는 사고(2026-08-04)가 다시 나면 여기서 잡히게 한다.
// 깨는 뮤테이션: helm rbac.yaml 에서 nodedevicereports/status 의 update 나 patch 를 지우면 실패한다.
func TestHelmRBACGrantsWhatMigObservationNeeds(t *testing.T) {
	generated := generatedOperatorRules(t)
	deployed := deployedOperatorRules(t)

	for _, res := range []string{"nodedevicereports", "nodedevicereports/status"} {
		want, ok := generated[res]
		if !ok {
			t.Fatalf("%s: config/rbac/role.yaml 에 없다 — 이 시험의 전제(kubebuilder 마커)가 깨졌다", res)
		}
		got, ok := deployed[res]
		if !ok {
			t.Errorf("%s: 생성본에는 있는데 deploy/helm/templates/rbac.yaml 의 operator role 에 없다", res)
			continue
		}
		for _, v := range want {
			if !contains(got, v) {
				t.Errorf("%s: 생성본 verb %q 가 helm 배포본에 없다 — 생성본 %v, 배포본 %v", res, v, want, got)
			}
		}
	}
}

// TestHelmRBACGrantsWhatDRACapabilityNeeds 는 acceleratorworkload_controller.go 의
// resource.k8s.io kubebuilder 마커(internal/intent/dra.go 가 실제로 쓰는 4개 리소스)가
// 배포 helm 차트에도 그대로 있는지 본다 — 위 MigObservation 시험과 같은 사고(2026-08-04)를
// 이 축에서 미리 막는다.
// 깨는 뮤테이션: helm rbac.yaml 에서 resourceslices 의 get 이나 watch 를 지우면 실패한다.
func TestHelmRBACGrantsWhatDRACapabilityNeeds(t *testing.T) {
	generated := generatedOperatorRules(t)
	deployed := deployedOperatorRules(t)

	for _, res := range []string{"deviceclasses", "resourceslices", "resourceclaims", "resourceclaimtemplates"} {
		want, ok := generated[res]
		if !ok {
			t.Fatalf("%s: config/rbac/role.yaml 에 없다 — 이 시험의 전제(kubebuilder 마커)가 깨졌다", res)
		}
		got, ok := deployed[res]
		if !ok {
			t.Errorf("%s: 생성본에는 있는데 deploy/helm/templates/rbac.yaml 의 operator role 에 없다", res)
			continue
		}
		for _, v := range want {
			if !contains(got, v) {
				t.Errorf("%s: 생성본 verb %q 가 helm 배포본에 없다 — 생성본 %v, 배포본 %v", res, v, want, got)
			}
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
