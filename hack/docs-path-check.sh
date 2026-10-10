#!/usr/bin/env bash
# ============================================================
# docs-path-check.sh: 형제 docs 저장소 경로가 코드에 들어오는 것을 막는다
# 상세: docs 저장소는 remote 가 없어 이 저장소를 받은 사람이 docs/... 경로에 도달할 수
#       없다. 웹훅 거절 메시지처럼 사용자에게 가는 문자열에 그런 경로가 실리면 받는
#       사람은 열 수 없는 파일을 안내받는다. 근거는 저장소 안 경로(test/live/, *_test.go)
#       로 적는다. 예외는 아래 ALLOW 에 이유와 함께 적는다. --self-test 는 임시 저장소에서
#       상대경로(../docs/)·URL 비매치·같은 줄 URL+위반·위반 0건 통과·git 저장소 아님을
#       자가 검증한다.
# 생성일: 2026-09-30 | 수정일: 2026-10-11 (상대경로 ../docs/ 를 잡도록 정규식 확장, 자가시험 추가; URL과 위반이 같은 줄에 있으면 위반이 묻히던 빈틈 수정;
# URL 지우기 재판정 단계 제거(쉼표·괄호로 붙은 위반 누락 수정), 자가시험 2건 추가; 지워진 단계를 전제하던 주석 정정, 이진 파일 매치 방지용 -I 추가)
# ============================================================
set -euo pipefail

# 예외. 각 줄은 "경로:이유" 다.
#   test/fault/suite_test.go   — 판정표 출력 대상. 디렉터리가 없으면 건너뛰도록 가드돼 있다.
#   cmd/kubectl-npu/main_test.go — 열 정렬 시험의 가짜 값.
#   Makefile                   — capmatrix 가 문서 저장소로 매트릭스를 생성하는 대상 경로.
#   internal/apiserver/routes_doc_test.go — 라우트 수를 문서와 대조하는 로컬 전용 검사.
#                                문서 저장소가 없으면 건너뛰도록 가드돼 있다.
#   hack/docs-path-check.sh    — 자가시험 픽스처 문자열. 검사 대상이 아니라 시험 입력이다.
ALLOW='^(test/fault/suite_test\.go|cmd/kubectl-npu/main_test\.go|Makefile|internal/apiserver/routes_doc_test\.go|hack/docs-path-check\.sh)$'

# 추적 파일만 본다. runs/ 아래는 실행이 그 시점에 낸 전사라 사후에 고치지 않는다.
DIRS='development|validation|feasibility|research|records|operations|features|comparison|history|decisions|architecture'

# 경로 하나짜리 매치식 — scan() 안에서 원본 줄과 URL 제거 사본 양쪽에 같은 식을 쓴다.
# 두 자리에 각자 적으면 한쪽만 고쳐지는 자리가 생긴다.
PATTERN="(^|[^A-Za-z0-9._/-])(\\.\\./)?(docs/)?($DIRS)/[A-Za-z0-9._/-]+\\.(md|json)"

# scan <루트>: 위반 유무를 판정. 종료 0=위반 없음, 1=위반 있음, 2=루트가 git 저장소가 아님.
scan() (
  local root="$1" hits
  cd "$root" || { echo "루트 디렉터리 없음: $root" >&2; return 2; }
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "git 저장소가 아니다: $root" >&2; return 2; }

  # 추적 파일만 본다. runs/ 아래는 실행이 그 시점에 낸 전사라 사후에 고치지 않는다.
  # URL 안의 경로는 정규식이 이미 거부한다 — `scheme://host/dir/x.md` 의 `dir` 앞 글자가 `/`
  # 라 선두 문자 클래스에 걸린다. 종전에는 그 위에 "URL 을 지운 사본으로 재판정" 단계를 뒀는데,
  # 그 치환이 다음 공백까지 먹어 URL 과 쉼표·괄호로 붙은 실제 위반까지 지웠다(자가시험 4·5).
  hits=$(git ls-files \
    | grep -vE '/runs/' \
    | grep -vE "$ALLOW" \
    | xargs grep -nHEI "$PATTERN" 2>/dev/null || true)

  if [ -n "$hits" ]; then
    echo "형제 docs 저장소 경로가 코드에 있다. 그 저장소는 remote 가 없어 도달할 수 없다."
    echo "근거는 test/live/ 하네스나 *_test.go 로 적고, 사실과 날짜는 산문으로 남겨라."
    echo
    echo "$hits"
    return 1
  fi
  echo "docs 경로 검사 통과"
)

# self_test: scan() 자체를 임시 저장소에서 검증한다. 정규식을 넓히는 변경은 조용히 안
# 잡게 되기 쉬우므로, 넓힌 뒤에도 이 일곱 경우가 전부 통과해야 한다.
self_test() {
  local tmp out rc expected
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/repo/internal" "$tmp/notgit"
  git -C "$tmp/repo" init -q
  printf '// ../docs/decisions/x.md\n' > "$tmp/repo/internal/a.go"  # doc-cite-check: ignore
  printf '// https://example.com/docs/decisions/x.md\n' > "$tmp/repo/internal/b.go"
  git -C "$tmp/repo" add internal/a.go internal/b.go

  # 1) 상대경로 ../docs/... 는 잡힌다
  expected=$'형제 docs 저장소 경로가 코드에 있다. 그 저장소는 remote 가 없어 도달할 수 없다.\n근거는 test/live/ 하네스나 *_test.go 로 적고, 사실과 날짜는 산문으로 남겨라.\n\ninternal/a.go:1:// ../docs/decisions/x.md'  # doc-cite-check: ignore
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = "$expected" ] || { echo "self-test 1: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 2) URL 안의 경로는 잡히지 않는다 — 결정적 기전은 정규식의 선두 문자 클래스다. docs/
  #    앞 글자가 / 라 매치가 시작되지 못한다. 별도의 URL 제외 단계는 없다 — 정규식만으로 0건이다.
  rm "$tmp/repo/internal/a.go"; git -C "$tmp/repo" add -A
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 0 ] && [ "$out" = "docs 경로 검사 통과" ] || { echo "self-test 2: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 3) 같은 줄에 URL 과 실제 위반이 같이 있으면 그 줄 전체(URL 포함)가 출력된다. 정규식이
  #    위반 쪽을 직접 잡으므로 URL 이 붙어 있어도 가려지지 않는다 — 줄 단위 버리기나 URL
  #    지우기 단계가 있으면 이 경우가 조용히 통과한다.
  rm "$tmp/repo/internal/b.go"
  printf '// 참고: https://example.com/x 와 ../docs/decisions/real.md\n' > "$tmp/repo/internal/c.go"  # doc-cite-check: ignore
  git -C "$tmp/repo" add -A
  expected=$'형제 docs 저장소 경로가 코드에 있다. 그 저장소는 remote 가 없어 도달할 수 없다.\n근거는 test/live/ 하네스나 *_test.go 로 적고, 사실과 날짜는 산문으로 남겨라.\n\ninternal/c.go:1:// 참고: https://example.com/x 와 ../docs/decisions/real.md'  # doc-cite-check: ignore
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = "$expected" ] || { echo "self-test 3: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 4) URL 과 위반이 쉼표로 붙어 있어도(공백 없음) 위반 자체의 선두 경계가 쉼표라 잡힌다 —
  #    URL 을 지운 사본으로 재판정하던 옛 단계라면 쉼표 뒤까지 먹어 놓쳤을 자리다.
  rm "$tmp/repo/internal/c.go"
  printf '// 참고: https://example.com/x,../docs/decisions/real.md 확인\n' > "$tmp/repo/internal/d.go"  # doc-cite-check: ignore
  git -C "$tmp/repo" add -A
  expected=$'형제 docs 저장소 경로가 코드에 있다. 그 저장소는 remote 가 없어 도달할 수 없다.\n근거는 test/live/ 하네스나 *_test.go 로 적고, 사실과 날짜는 산문으로 남겨라.\n\ninternal/d.go:1:// 참고: https://example.com/x,../docs/decisions/real.md 확인'  # doc-cite-check: ignore
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = "$expected" ] || { echo "self-test 4: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 5) 괄호로 붙어도 같은 이유로 잡힌다.
  rm "$tmp/repo/internal/d.go"
  printf '// 참고: (https://example.com/x)../docs/decisions/real.md 확인\n' > "$tmp/repo/internal/e.go"  # doc-cite-check: ignore
  git -C "$tmp/repo" add -A
  expected=$'형제 docs 저장소 경로가 코드에 있다. 그 저장소는 remote 가 없어 도달할 수 없다.\n근거는 test/live/ 하네스나 *_test.go 로 적고, 사실과 날짜는 산문으로 남겨라.\n\ninternal/e.go:1:// 참고: (https://example.com/x)../docs/decisions/real.md 확인'  # doc-cite-check: ignore
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = "$expected" ] || { echo "self-test 5: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 6) 위반 0건이면 통과
  rm "$tmp/repo/internal/e.go"; git -C "$tmp/repo" add -A
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 0 ] && [ "$out" = "docs 경로 검사 통과" ] || { echo "self-test 6: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 7) git 저장소가 아니면 종료 2 — 위반 0건 통과로 둔갑하지 않는다
  rc=0; out="$(scan "$tmp/notgit" 2>&1)" || rc=$?
  [ "$rc" -eq 2 ] || { echo "self-test 7: git 저장소 아닌 곳에서 종료 $rc (기대 2) '$out'" >&2; return 1; }

  echo "self-test ok"
}

case "${1:-}" in
  --self-test) self_test ;;
  *) scan "${1:-$(dirname "$0")/..}" ;;
esac
