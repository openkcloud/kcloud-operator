// ============================================================
// naming_test.go: DriverDSName 단위 테스트
// 상세: 드라이버 DaemonSet 이름 생성 규칙 — 벤더/모델 매핑 테이블, 빈 모델 안전성,
//       대소문자 정규화(내부 lower-case 처리) 검증
// 생성일: 2026-06-02
// ============================================================

package naming

import (
	"strings"
	"testing"
)

// TestDriverDSName 는 DriverDSName 의 전체 매핑 테이블을 검증한다.
//
// 매핑 규칙:
//   - nvidia(any model)      → "kcloud-nvidia-driver"
//   - furiosa + warboy       → "kcloud-furiosa-warboy-driver"
//   - furiosa + rngd         → "kcloud-furiosa-rngd-driver"
//   - furiosa + ""           → "kcloud-furiosa-driver" (이중 하이픈 없음)
//   - other + ""             → "kcloud-<vendor>-driver"
//   - other + model          → "kcloud-<vendor>-<model>-driver"
func TestDriverDSName(t *testing.T) {
	cases := []struct {
		name   string
		vendor string
		model  string
		want   string
	}{

		// ── nvidia: model 무시 ──────────────────────────────
		{"nvidia/generic", "nvidia", "generic", "kcloud-nvidia-driver"},
		{"nvidia/empty-model", "nvidia", "", "kcloud-nvidia-driver"},
		{"nvidia/other-model", "nvidia", "a100", "kcloud-nvidia-driver"},

		// ── furiosa: model 보존 ─────────────────────────────
		{"furiosa/warboy", "furiosa", "warboy", "kcloud-furiosa-warboy-driver"},
		{"furiosa/rngd", "furiosa", "rngd", "kcloud-furiosa-rngd-driver"},
		// 빈 model → 이중 하이픈 없이 단순 형태
		{"furiosa/empty-model no double-hyphen", "furiosa", "", "kcloud-furiosa-driver"},

		// ── default vendor: 빈 model ────────────────────────
		{"rebellions/empty-model", "rebellions", "", "kcloud-rebellions-driver"},
		{"custom-vendor/empty-model", "myvend", "", "kcloud-myvend-driver"},

		// ── default vendor: model 포함 ──────────────────────
		{"rebellions/atom", "rebellions", "atom", "kcloud-rebellions-atom-driver"},
		{"custom/v1", "custom", "v1", "kcloud-custom-v1-driver"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DriverDSName(tc.vendor, tc.model)
			if got != tc.want {
				t.Errorf("DriverDSName(%q, %q) = %q, want %q",
					tc.vendor, tc.model, got, tc.want)
			}
		})
	}
}

// TestDriverDSName_MixedCaseNormalization 는 대소문자 혼합 입력이
// 소문자로 정규화되어 올바른 이름을 반환함을 검증한다.
func TestDriverDSName_MixedCaseNormalization(t *testing.T) {
	cases := []struct {
		name   string
		vendor string
		model  string
		want   string
	}{
		{"NVIDIA/Generic", "NVIDIA", "Generic", "kcloud-nvidia-driver"},
		{"Nvidia/generic", "Nvidia", "generic", "kcloud-nvidia-driver"},
		{"Furiosa/Warboy", "Furiosa", "Warboy", "kcloud-furiosa-warboy-driver"},
		{"FURIOSA/RNGD", "FURIOSA", "RNGD", "kcloud-furiosa-rngd-driver"},
		{"Furiosa/empty", "Furiosa", "", "kcloud-furiosa-driver"},
		{"Rebellions/Atom", "Rebellions", "Atom", "kcloud-rebellions-atom-driver"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DriverDSName(tc.vendor, tc.model)
			if got != tc.want {
				t.Errorf("DriverDSName(%q, %q) = %q, want %q",
					tc.vendor, tc.model, got, tc.want)
			}
		})
	}
}

// TestDriverDSName_NoDoubleHyphen 는 빈 model 입력에서 어떤 벤더든
// 이중 하이픈("--")이 결과에 포함되지 않음을 검증한다 (빈 model 안전성).
func TestDriverDSName_NoDoubleHyphen(t *testing.T) {
	vendors := []string{
		"furiosa", "nvidia", "rebellions", "custom",
	}
	for _, vendor := range vendors {
		got := DriverDSName(vendor, "")
		if strings.Contains(got, "--") {
			t.Errorf("DriverDSName(%q, \"\") = %q: 이중 하이픈 포함 (빈 model 처리 버그)",
				vendor, got)
		}
	}
}

// TestDriverDSName_AlwaysHasKcloudPrefix 는 모든 벤더 입력에 대해
// 반환 이름이 "kcloud-" 로 시작함을 검증한다.
func TestDriverDSName_AlwaysHasKcloudPrefix(t *testing.T) {
	pairs := []struct{ vendor, model string }{
		{"nvidia", "generic"},
		{"furiosa", "warboy"},
		{"furiosa", "rngd"},
		{"furiosa", ""},
		{"rebellions", "atom"},
		{"rebellions", ""},
		{"unknown-vendor", ""},
		{"unknown-vendor", "some-model"},
	}
	for _, p := range pairs {
		got := DriverDSName(p.vendor, p.model)
		if !strings.HasPrefix(got, "kcloud-") {
			t.Errorf("DriverDSName(%q, %q) = %q: \"kcloud-\" 접두사 없음",
				p.vendor, p.model, got)
		}
	}
}

// TestDriverDSName_AlwaysHasDriverSuffix 는 모든 입력에 대해
// 반환 이름이 "-driver" 로 끝남을 검증한다.
func TestDriverDSName_AlwaysHasDriverSuffix(t *testing.T) {
	pairs := []struct{ vendor, model string }{
		{"nvidia", ""},
		{"furiosa", "warboy"},
		{"furiosa", ""},
		{"rebellions", "atom"},
	}
	for _, p := range pairs {
		got := DriverDSName(p.vendor, p.model)
		if !strings.HasSuffix(got, "-driver") {
			t.Errorf("DriverDSName(%q, %q) = %q: \"-driver\" 접미사 없음",
				p.vendor, p.model, got)
		}
	}
}

// TestInstallJobName 는 WP-C-1 install Job 이름 규약을 검증한다.
//   - "kcloud-<vendor>[-<model>]-install-<8hex>" 형태
//   - 동일 입력에 대해 결정론적(같은 값)
//   - 노드가 다르면 접미사가 달라짐(노드별 분리)
//   - 대소문자 정규화, 빈 model 이중 하이픈 없음
func TestInstallJobName(t *testing.T) {
	cases := []struct {
		name       string
		vendor     string
		model      string
		node       string
		wantPrefix string
	}{
		{"nvidia", "nvidia", "generic", "worker1", "kcloud-nvidia-install-"},
		{"nvidia empty model", "nvidia", "", "worker1", "kcloud-nvidia-install-"},
		{"furiosa warboy", "furiosa", "warboy", "worker1", "kcloud-furiosa-warboy-install-"},
		{"furiosa rngd", "furiosa", "rngd", "rngd-1", "kcloud-furiosa-rngd-install-"},
		{"furiosa empty", "furiosa", "", "n1", "kcloud-furiosa-install-"},
		{"custom model", "custom", "v1", "n1", "kcloud-custom-v1-install-"},
		{"mixed case", "NVIDIA", "Generic", "worker1", "kcloud-nvidia-install-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InstallJobName(tc.vendor, tc.model, tc.node)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("InstallJobName(%q,%q,%q) = %q, want prefix %q",
					tc.vendor, tc.model, tc.node, got, tc.wantPrefix)
			}
			suffix := strings.TrimPrefix(got, tc.wantPrefix)
			if len(suffix) != 8 {
				t.Errorf("InstallJobName(%q,%q,%q) = %q: 접미사 8hex 아님(len=%d)",
					tc.vendor, tc.model, tc.node, got, len(suffix))
			}
			if strings.Contains(got, "--") {
				t.Errorf("InstallJobName(%q,%q,%q) = %q: 이중 하이픈 포함",
					tc.vendor, tc.model, tc.node, got)
			}
		})
	}
}

// TestInstallJobName_Deterministic 는 동일 입력이 항상 같은 이름(=lease)을 내고,
// 노드가 다르면 이름이 달라짐을 검증한다.
func TestInstallJobName_Deterministic(t *testing.T) {
	a1 := InstallJobName("nvidia", "generic", "worker1")
	a2 := InstallJobName("nvidia", "generic", "worker1")
	if a1 != a2 {
		t.Errorf("결정론적이지 않음: %q != %q", a1, a2)
	}
	b := InstallJobName("nvidia", "generic", "worker2")
	if a1 == b {
		t.Errorf("노드별로 이름이 분리되지 않음: worker1=%q worker2=%q", a1, b)
	}
}

// TestToolkitDSName 는 toolkit DS 이름이 driver 규칙과 동일하되 "-toolkit" 접미사만
// 다른지 검증한다(노드에서 driver/toolkit DS 를 짝으로 식별하기 위함).
func TestToolkitDSName(t *testing.T) {
	cases := []struct {
		vendor string
		model  string
		want   string
	}{
		{"nvidia", "generic", "kcloud-nvidia-toolkit"},
		{"nvidia", "", "kcloud-nvidia-toolkit"},
		{"NVIDIA", "a100", "kcloud-nvidia-toolkit"},
		{"furiosa", "warboy", "kcloud-furiosa-warboy-toolkit"},
		{"furiosa", "", "kcloud-furiosa-toolkit"},
		{"rebellions", "", "kcloud-rebellions-toolkit"},
		{"custom", "v1", "kcloud-custom-v1-toolkit"},
	}
	for _, tc := range cases {
		got := ToolkitDSName(tc.vendor, tc.model)
		if got != tc.want {
			t.Errorf("ToolkitDSName(%q, %q) = %q, want %q", tc.vendor, tc.model, got, tc.want)
		}
		if strings.Contains(got, "--") {
			t.Errorf("ToolkitDSName(%q, %q) = %q: 이중 하이픈 포함", tc.vendor, tc.model, got)
		}
	}
}

// TestOperatorNamespace: env 미설정→kube-system(회귀 0), 설정→그 값(#16 ns 재편).
func TestOperatorNamespace(t *testing.T) {
	t.Setenv("OPERATOR_NAMESPACE", "")
	if got := OperatorNamespace(); got != "kube-system" {
		t.Errorf("env 미설정 시 %q, want kube-system", got)
	}
	t.Setenv("OPERATOR_NAMESPACE", "kcloud")
	if got := OperatorNamespace(); got != "kcloud" {
		t.Errorf("env=kcloud 시 %q, want kcloud", got)
	}
}
