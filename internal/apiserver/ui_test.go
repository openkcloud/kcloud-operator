// ============================================================
// ui_test.go: 정적 대시보드 서빙 테스트
// 상세: 셸은 무인증 200 이어야 하고(브라우저가 헤더를 못 싣는다), 그 안에 클러스터 데이터가
//       한 글자도 없어야 하며, 기존 /api 경로를 가로채지 않아야 한다.
// 생성일: 2026-07-30 | 수정일: 2026-07-30
// ============================================================

package apiserver

import (
	"net/http"
	"strings"
	"testing"
)

func TestUI_ServedWithoutToken(t *testing.T) {
	s, _ := newServer(t, authnOpts{}) // 인증 실패 설정 — 그래도 200 이어야 한다.
	w := do(t, s, "GET", "/ui/", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("정적 셸은 무인증 200 이어야 함: got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type 이 text/html 이어야 함: %q", ct)
	}
	if !strings.Contains(w.Body.String(), "kcloud accelerator console") {
		t.Fatalf("대시보드 HTML 이 아님")
	}
}

func TestUI_RootAlsoServesShell(t *testing.T) {
	s, _ := newServer(t, authnOpts{})
	if w := do(t, s, "GET", "/", "", ""); w.Code != http.StatusOK {
		t.Fatalf("루트도 셸을 내야 함: got %d", w.Code)
	}
}

// 셸에는 클러스터 사실이 들어 있으면 안 된다 — 무인증으로 나가기 때문이다.
func TestUI_ShellCarriesNoClusterData(t *testing.T) {
	s, _ := newServer(t, authnOpts{}, invObjects()...)
	body := do(t, s, "GET", "/ui/", "", "").Body.String()
	for _, secret := range []string{"GPU-aaa", "worker1", "rngd-1", "0000:3b:00.0"} {
		if strings.Contains(body, secret) {
			t.Fatalf("정적 셸에 클러스터 데이터가 새어나감: %q", secret)
		}
	}
}

// UI 라우트가 API 라우트를 가로채지 않아야 한다.
func TestUI_DoesNotShadowAPI(t *testing.T) {
	s, _ := newServer(t, authnOpts{})
	if w := do(t, s, "GET", "/api/v1/status", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("API 경로는 여전히 인증을 요구해야 함: got %d", w.Code)
	}
}

// 외부 리소스를 부르면 air-gap 에서 화면이 깨진다 — 절대경로 http(s) 참조가 없어야 한다.
func TestUI_HasNoExternalReferences(t *testing.T) {
	body := string(indexHTML)
	for _, bad := range []string{"http://", "https://", "//cdn.", "integrity="} {
		if strings.Contains(body, bad) {
			t.Fatalf("외부 리소스 참조가 있으면 air-gap 에서 깨진다: %q", bad)
		}
	}
}

// stale/pending 행은 정상 행과 시각적으로 달라야 한다(§ inventory.go 정직성 계약). 셸 JS 를
// 직접 실행할 수는 없으니, 그 처리 코드가 셸에 존재한다는 사실 자체를 고정한다 — 이렇게 해 두면
// 이후 누군가 이 분기를 지워도(예: 표 리팩터 중 실수로) 이 테스트가 즉시 깨진다.
func TestUI_RendersStaleAndPending(t *testing.T) {
	body := string(indexHTML)
	for _, want := range []string{"x.pending", "x.stale", "x.staleReason", "d.pending", "d.stale", "d.staleReason"} {
		if !strings.Contains(body, want) {
			t.Fatalf("셸 JS 가 %q 를 다뤄야 함(stale/pending 은 정상 행과 구분되어야 한다)", want)
		}
	}
}

// 검증 등급(Verified/Documented/LegacyDocumented) 을 뭉개면 "지원" 과 "문서상 지원" 이 같아
// 보인다 — partitionBadge 가 profiles[].supportLevel 을 실제로 소비하는지 고정한다.
func TestUI_RendersPartitionSupportLevel(t *testing.T) {
	body := string(indexHTML)
	for _, want := range []string{"supportLevel", "hardwareSupported", "partitionBadge"} {
		if !strings.Contains(body, want) {
			t.Fatalf("셸 JS 가 %q 를 다뤄야 함(파티션 지원 등급이 안 보이면 안 됨)", want)
		}
	}
}

// replica 는 광고 배수일 뿐 물리 장치 개수가 아니다 — "×N" 배지만 있으면 "장치 N 개" 로 오독된다.
// 대시보드 표에 공유·격리 없음 문구가 실제로 붙는지 고정한다(리뷰 I-1).
func TestUI_RendersSharedNoIsolationWording(t *testing.T) {
	body := string(indexHTML)
	if !strings.Contains(body, "물리 장치 1개 공유") {
		t.Fatalf("time-sliced 배지에 '물리 장치 1개 공유' 문구가 없음 — replica 를 독립 장치로 오독하게 둔다")
	}
}

// oversubscribed(D-1): 광고 수가 물리 장치 수를 넘는다는 관측만으로 "시분할"·"격리 없음" 이라고
// 단정하면 안 된다(RNGD PE 처럼 하드웨어 격리가 있는 경우가 있다) — shareCell 이 이 상태를 별도
// 문구로 렌더하는지 고정한다. shareCell 의 호출부는 TestUI_HonestyBadgesAreActuallyCalled 가 이미
// 고정하므로(호출부가 지워지면 그쪽이 깨진다), 여기서는 이 상태 전용 문구만 고정한다.
func TestUI_RendersOversubscribedWording(t *testing.T) {
	body := string(indexHTML)
	for _, want := range []string{"oversubscribed", "초과 광고", "원인 미확정"} {
		if !strings.Contains(body, want) {
			t.Fatalf("shareCell 에 oversubscribed 전용 문구 %q 가 없음 — 원인 미확정 상태를 시분할/격리없음으로 단정하면 안 됨", want)
		}
	}
}

// cordoned 행도 stale/pending 과 같은 부류다 — 장치는 있어도 스케줄되지 않는다. 대시보드 표는
// invTree 와 달리 노드 헤더가 없으므로 healthBadge 가 schedulable 을 직접 반영해야 한다(리뷰 I-2).
// 주의: "!x.schedulable" 이나 "cordoned" 만으로는 고정되지 않는다 — 둘 다 집계 카드와
// 인벤토리 탭 노드 헤더에 이미 있어서 healthBadge 분기를 지워도 통과한다(재리뷰가 삭제
// 실험으로 확인). 이 분기에만 있는 캡션을 검사한다.
func TestUI_RendersCordonedRow(t *testing.T) {
	body := string(indexHTML)
	const want = "스케줄 불가 — 배치되지 않음"
	if !strings.Contains(body, want) {
		t.Fatalf("healthBadge 에 cordoned 캡션 %q 가 없음(cordoned 행이 정상 행과 구분되지 않음)", want)
	}
}

// shareBadge 의 verified/supported/미지원 3분기를 깨뜨려도(예: verified 분기를 지워도) 지금은
// 어떤 테스트도 안 깨진다 — 그 세 조건을 실제로 참조하는지 고정한다(리뷰 Minor #1).
func TestUI_ShareBadgeThreeStateIsPinned(t *testing.T) {
	body := string(indexHTML)
	for _, want := range []string{"m.verified", "m.supported", "지원(검증됨)", "미검증(", "미지원"} {
		if !strings.Contains(body, want) {
			t.Fatalf("shareBadge 가 %q 분기를 다뤄야 함(verified/supported 구분이 사라지면 안 됨)", want)
		}
	}
}

// 정직성 배지는 정의만으로는 아무것도 못 한다 — 호출부가 사라지면 화면에서 사라진다.
// 위 테스트들은 전부 헬퍼 "본문"(파라미터명이 다르거나 그 안의 문자열)에 어서션을 걸어서,
// 호출부만 지워도(헬퍼 자체는 죽은 코드로 남아) 계속 통과한다(최종 리뷰 Important #6, 삭제
// 실험으로 확증됨). 여기서는 호출부에만 나타나는 형태를 고정한다.
// healthBadge 는 정의(`function healthBadge(x) {`)와 호출부(`healthBadge(x)`)가 같은 파라미터명을
// 써서 단순 "healthBadge(x)" 로는 구분이 안 된다 — 호출부에만 있는 뒤따르는 인자까지 고정한다.
func TestUI_HonestyBadgesAreActuallyCalled(t *testing.T) {
	body := string(indexHTML)
	for _, want := range []string{
		"healthBadge(x), esc(x.nodeName)", "partitionBadge(c.partition)",
		"shareBadge(s.timeSlicing)", "shareBadge(s.multiProcess)", "shareBadge(s.brokered)",
		"shareCell(x),", "shareCell(d)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("정직성 배지 호출부 %q 가 없음 — 헬퍼가 있어도 화면엔 안 나온다", want)
		}
	}
}

// 논리 파티션 수는 profile 이름 개수가 아니라 장치당 인스턴스 합이다. 카드가 인스턴스를
// 실제로 읽는지, 그리고 미상 행을 조용히 빼지 않고 표시하는지 고정한다. 호출부에서만 나오는
// "countPerDevice" 를 검사한다 — 헬퍼 정의부나 주석에만 있는 경우 삭제해도 이 테스트가
// 계속 통과하는 함정을 피하기 위해서다(TestUI_HonestyBadgesAreActuallyCalled 와 같은 규율).
func TestUI_LogicalPartitionCardUsesInstanceCounts(t *testing.T) {
	body := string(indexHTML)
	for _, want := range []string{"partitionInstances", "countPerDevice", "미상 "} {
		if !strings.Contains(body, want) {
			t.Fatalf("대시보드 카드가 %q 를 다뤄야 함", want)
		}
	}
}

// hardwareSupported=false 는 "측정된 미지원" 과 "미관측" 둘 다일 수 있다(NVIDIA 백엔드가
// 관측 실패에도 false 를 쓴다). 화면이 둘을 갈라야 한다 — verification 분기를 고정한다.
func TestUI_PartitionBadgeSeparatesUnobserved(t *testing.T) {
	body := string(indexHTML)
	for _, want := range []string{`p.verification === "required"`, "미관측 — 판정 불가", "파티션 불가"} {
		if !strings.Contains(body, want) {
			t.Fatalf("partitionBadge 가 %q 를 다뤄야 함(관측 실패를 하드웨어 사실로 단언하면 안 된다)", want)
		}
	}
}
