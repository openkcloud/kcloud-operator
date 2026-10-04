// ============================================================
// judgment_duplication_test.go: 장치 점유 판정 중복 방지 고정 시험
// 상세: podUsesDevice 가 "이 Pod 가 장치를 쥐었는가" 를 묻는 유일한 함수여야 한다.
//   전에는 hasDeviceWorkloads·podUsesDevice 가 가속기 리소스 이름 목록을 각자 적어
//   두고 있었고, 그 목록에 rebellions.ai/ 가 빠져 rebellions 파드가 drain 대상에서
//   빠졌다(state_machine.go 의 hasAcceleratorResourceName 위 주석). 판정을 하나로
//   모은 뒤에도 같은 자리에서 다시 갈라지면 시험이 없는 한 알 수 없다 — go/ast 로
//   internal/upgrade 의 비-테스트 소스를 파싱해, 판정의 표지(Spec.ResourceClaims
//   참조 또는 hasAcceleratorResourceName 호출)를 본문에 가진 함수가 허용 목록
//   하나뿐인지 본다. 표지는 OR 로 묶는다 — AND 로 묶으면 limits 축만 다시 적은
//   복제(이 Round 가 고친 결함의 원래 모양: DRA 축을 모르는 판정)를 잡지 못한다.
//   허용 목록은 패키지 수준 함수에만 적용한다 — Go 는 함수와 동명 메서드를 한 패키지에
//   허용하므로 receiver 를 보지 않으면 메서드 복제가 이름만으로 허용 목록을 통과한다.
// 생성일: 2026-10-04
// ============================================================

package upgrade

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// judgmentFuncAllowlist 는 장치 점유 판정의 표지를 본문에 가져도 되는 유일한
// 패키지 수준 함수 이름이다. hasDeviceWorkloads·evictDevicePods 는 이 함수를 호출만
// 하므로 표지를 갖지 않는다 — 다른 함수가 판정을 다시 적으면 이 시험이 잡는다.
// 메서드는 이름이 같아도 허용하지 않는다(아래 fn.Recv 검사).
const judgmentFuncAllowlist = "podUsesDevice"

// TestPodDeviceJudgmentFuncIsNotDuplicated 는 internal/upgrade 안에서 podUsesDevice
// 외의 함수가 같은 판정을 본문에 복제하면 실패한다.
func TestPodDeviceJudgmentFuncIsNotDuplicated(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("패키지 디렉터리 훑기 실패: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("%s 파싱 실패: %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// receiver 가 있으면 허용 목록을 거치지 않는다 — 메서드는 전부 적발 대상이다.
			if fn.Recv == nil && fn.Name.Name == judgmentFuncAllowlist {
				continue
			}
			if hasDeviceJudgmentMarkers(fn.Body) {
				kind := "func"
				if fn.Recv != nil {
					kind = "method"
				}
				offenders = append(offenders, name+":"+kind+" "+fn.Name.Name)
			}
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("판정이 복제된 함수(허용 목록은 %s 뿐이다): %v",
			judgmentFuncAllowlist, offenders)
	}
}

// hasDeviceJudgmentMarkers 는 body 가 판정의 표지(ResourceClaims 참조 또는
// hasAcceleratorResourceName 호출)를 하나라도 담고 있는지 본다. OR 로 묶는다 —
// AND 로 묶으면 limits 축만 다시 적은 복제를 못 잡는다. hasAcceleratorResourceName
// 호출은 비-테스트 코드에 podUsesDevice 하나뿐이라 CPU·메모리 limits 만 읽는
// 함수는 걸리지 않는다.
func hasDeviceJudgmentMarkers(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "ResourceClaims" {
				found = true
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "hasAcceleratorResourceName" {
				found = true
			}
		}
		return true
	})
	return found
}
