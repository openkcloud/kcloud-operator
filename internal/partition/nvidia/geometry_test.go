package nvidia

import (
	"strings"
	"testing"
)

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 모델 B(§17.1): preflight current==Enabled(사전조건), -mig 전환 없음, GI 생성.
func TestBuildApplySteps_PreEnabledNoModeSwitch(t *testing.T) {
	steps := BuildApplySteps([]GeometrySpec{{GPUSelector: "00000000:18:00.0", ProfileName: "1g.6gb", Count: 4}})
	var preEnabledOK, cgiOK, sawMigSwitch bool
	for _, s := range steps {
		j := strings.Join(s.Argv, " ")
		if strings.Contains(j, "mig.mode.current") && eq(s.ExpectOneOf, []string{"Enabled"}) {
			preEnabledOK = true
		}
		if strings.Contains(j, "-cgi 1g.6gb,1g.6gb,1g.6gb,1g.6gb") {
			cgiOK = true
		}
		if strings.Contains(j, "-mig 1") || strings.Contains(j, "-mig 0") {
			sawMigSwitch = true
		}
	}
	if !preEnabledOK || !cgiOK {
		t.Fatalf("preflight(current==Enabled)+cgi(name) 필요: pre=%v cgi=%v", preEnabledOK, cgiOK)
	}
	if sawMigSwitch {
		t.Fatal("모델 B 는 mode 전환(-mig)을 하지 않아야 함")
	}
}

func TestBuildDisableSteps_KeepsModeEnabled(t *testing.T) {
	steps := BuildDisableSteps([]string{"PCI"})
	var dgiOK, sawMigSwitch bool
	for _, s := range steps {
		j := strings.Join(s.Argv, " ")
		if strings.Contains(j, "-dgi") {
			dgiOK = true
		}
		if strings.Contains(j, "-mig 0") {
			sawMigSwitch = true
		}
	}
	if !dgiOK {
		t.Fatal("disable 은 GI 제거(-dgi)를 포함해야 함")
	}
	if sawMigSwitch {
		t.Fatal("모델 B 는 mode 를 끄지 않아야 함(-mig 0 금지)")
	}
	last := steps[len(steps)-1]
	if !eq(last.ExpectOneOf, []string{"Enabled"}) {
		t.Fatalf("disable 마지막 step 은 current==Enabled(여전히 enabled) 확인이어야: %+v", last)
	}
}
