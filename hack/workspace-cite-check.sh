#!/usr/bin/env bash
# ============================================================
# workspace-cite-check.sh: 추적 파일이 .superpowers/ 작업공간이나 task-N 보고서 경로를
#                           인용하는 것을 막는다
# 상세: .superpowers/ 는 .gitignore 로 막혀 어디에도 커밋되지 않는다. 추적 파일이
#       디렉터리 접두가 붙어도 그 경로나 task-N-*.md 파일명(접미 임의)을 인용하면
#       받는 사람은 열 수 없는 경로를 안내받는다. 패턴은 아래 PATTERN 한 곳에만
#       정의한다. --self-test 는 임시 저장소에서 자가시험 여섯 경우(bare task-N 인용·
#       .superpowers/ 경로 인용·디렉터리 접두가 붙은 경로 인용·task-N 접미 확장 인용·
#       위반 0건 통과·git 저장소 아님)로 자가 검증한다.
# 생성일: 2026-10-11 | 수정일: 2026-10-11 (디렉터리 접두·task-N 접미 확장; 자가시험 경우 추가; 머리말을 패턴과 맞춤)
# ============================================================
set -euo pipefail

# 예외. 각 줄은 "경로 — 이유" 다.
#   .gitignore                 — 작업공간을 무시하는 그 규칙 자체. 검사 대상이 아니라 검사의 전제다.
#   hack/workspace-cite-check.sh — 자가시험 픽스처 문자열. 검사 대상이 아니라 시험 입력이다.
ALLOW='^(\.gitignore|hack/workspace-cite-check\.sh)$'

# 패턴은 한 곳에만 정의한다. (\.\./)* 는 상위참조를 거친 인용도 잡고, 디렉터리 접두
# 그룹은 몇 단계든 허용해 docs/.superpowers/… 처럼 디렉터리 뒤에 붙은 인용도 잡는다.
PATTERN='(^|[^A-Za-z0-9._/-])(\.\./)*([A-Za-z0-9._-]+/)*(\.superpowers/|task-[0-9]+-[a-z0-9-]+\.md)'

# scan <루트>: 위반 유무를 판정. 종료 0=위반 없음, 1=위반 있음, 2=루트가 git 저장소가 아님.
scan() (
  local root="$1" hits
  cd "$root" || { echo "루트 디렉터리 없음: $root" >&2; return 2; }
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "git 저장소가 아니다: $root" >&2; return 2; }

  # 추적 파일만, /runs/ 아래는 실행이 그 시점에 낸 전사라 제외. 후처리 단계는 두지 않는다 —
  # URL 지우기 같은 재판정이 같은 줄의 위반을 놓치는 빈틈이었다(docs-path-check 의 선례).
  hits=$(git ls-files \
    | grep -vE '/runs/' \
    | grep -vE "$ALLOW" \
    | xargs grep -nHEI "$PATTERN" 2>/dev/null || true)

  if [ -n "$hits" ]; then
    echo "추적 파일이 .superpowers/ 작업공간이나 task-N 보고서 경로를 인용한다."
    echo ".gitignore 로 막힌 경로라 받는 사람은 열 수 없는 경로를 안내받는다."
    echo
    echo "$hits"
    return 1
  fi
  echo "작업공간 인용 검사 통과"
)

# self_test: scan() 자체를 임시 저장소에서 검증한다. PATTERN 을 좁히는 변경은 조용히 안
# 잡게 되기 쉬우므로, 고친 뒤에도 이 여섯 경우가 전부 통과해야 한다.
self_test() {
  local tmp out rc expected
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/repo/internal" "$tmp/notgit"
  git -C "$tmp/repo" init -q

  # 1) bare task-N-report.md 인용 → 종료 1, 그 줄이 출력에 있다
  printf '// 참고: task-2-report.md 확인\n' > "$tmp/repo/internal/a.go"
  git -C "$tmp/repo" add internal/a.go
  expected=$'추적 파일이 .superpowers/ 작업공간이나 task-N 보고서 경로를 인용한다.\n.gitignore 로 막힌 경로라 받는 사람은 열 수 없는 경로를 안내받는다.\n\ninternal/a.go:1:// 참고: task-2-report.md 확인'
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = "$expected" ] || { echo "self-test 1: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 2) .superpowers/ 경로 인용 → 종료 1
  rm "$tmp/repo/internal/a.go"
  printf '// 근거: .superpowers/sdd/x/task-1-brief.md 참고\n' > "$tmp/repo/internal/b.go"
  git -C "$tmp/repo" add -A
  expected=$'추적 파일이 .superpowers/ 작업공간이나 task-N 보고서 경로를 인용한다.\n.gitignore 로 막힌 경로라 받는 사람은 열 수 없는 경로를 안내받는다.\n\ninternal/b.go:1:// 근거: .superpowers/sdd/x/task-1-brief.md 참고'
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = "$expected" ] || { echo "self-test 2: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 3) 디렉터리 접두가 붙은 .superpowers/ 경로 인용 → 종료 1(Critical: 선두 문자 클래스가
  #    / 를 거부해 놓치던 바로 그 모양)
  rm "$tmp/repo/internal/b.go"
  printf '// 참고: docs/.superpowers/worklog/x.md 확인\n' > "$tmp/repo/internal/c.go"  # doc-cite-check: ignore
  git -C "$tmp/repo" add -A
  expected=$'추적 파일이 .superpowers/ 작업공간이나 task-N 보고서 경로를 인용한다.\n.gitignore 로 막힌 경로라 받는 사람은 열 수 없는 경로를 안내받는다.\n\ninternal/c.go:1:// 참고: docs/.superpowers/worklog/x.md 확인'  # doc-cite-check: ignore
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = "$expected" ] || { echo "self-test 3: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 4) report|review|brief|addendum 밖의 task-N 파일명 접미 인용 → 종료 1(접미 확장 확인)
  rm "$tmp/repo/internal/c.go"
  printf '// 참고: task-3-fix-1.md 확인\n' > "$tmp/repo/internal/d.go"
  git -C "$tmp/repo" add -A
  expected=$'추적 파일이 .superpowers/ 작업공간이나 task-N 보고서 경로를 인용한다.\n.gitignore 로 막힌 경로라 받는 사람은 열 수 없는 경로를 안내받는다.\n\ninternal/d.go:1:// 참고: task-3-fix-1.md 확인'
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = "$expected" ] || { echo "self-test 4: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 5) 위반 0건이면 통과
  rm "$tmp/repo/internal/d.go"; git -C "$tmp/repo" add -A
  rc=0; out="$(scan "$tmp/repo")" || rc=$?
  [ "$rc" -eq 0 ] && [ "$out" = "작업공간 인용 검사 통과" ] || { echo "self-test 5: 종료 $rc 출력 '$out'" >&2; return 1; }

  # 6) git 저장소가 아니면 종료 2 — 위반 0건 통과로 둔갑하지 않는다
  rc=0; out="$(scan "$tmp/notgit" 2>&1)" || rc=$?
  [ "$rc" -eq 2 ] || { echo "self-test 6: git 저장소 아닌 곳에서 종료 $rc (기대 2) '$out'" >&2; return 1; }

  echo "self-test ok"
}

case "${1:-}" in
  --self-test) self_test ;;
  *) scan "${1:-$(dirname "$0")/..}" ;;
esac
