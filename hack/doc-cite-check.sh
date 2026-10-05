#!/usr/bin/env bash
# ============================================================
# doc-cite-check.sh: 코드·설정·문서가 가리키는 docs/*.md 경로가 실재하는지 검사
# 상세: git 추적 파일에서 `docs/<경로>.md` 와 `../docs/<경로>.md` 토큰을 찍어 파일 존재를
#       확인한다. URL 안의 경로와 픽스처 문자열 파일(ALLOW_FILES)은 제외한다. `../docs` 는
#       형제 저장소가 있는 로컬에서만 검사한다. 인용 0건은 통과다. 트리 안 경로는 git 추적
#       여부로 실재를 판정한다(디스크에만 있는 문서는 CI 에 없다). --self-test 는 임시
#       저장소에서 부재·미추적·URL·형제 부재·0건을 자가 검증한다.
# 생성일: 2026-10-05
# ============================================================
set -euo pipefail

# 인용을 데이터로 들고 있는 시험 픽스처 — 경로가 아니라 표 정렬을 시험하는 문자열이다.
# 자가시험 픽스처 문자열을 들고 있는 이 스크립트 자신도 제외한다 — 아니면 git 추적 대상
# *.sh 라서 스캔이 자기 self_test 의 픽스처 경로를 인용으로 집계한다.
ALLOW_FILES='^(cmd/kubectl-npu/main_test\.go|hack/doc-cite-check\.sh)$'
# 형제 저장소 참조 ../docs/…md 는 앞이 영숫자·_·. 만 아니면 인용으로 본다 — 쉘 변수 뒤의
# $(DIR)/../docs 도 잡지만, ../../docs 처럼 연쇄된 상위참조는 바로 앞 세 글자가 "../" 라
# 제외한다(report.write("../../docs/x.md") 같은 쓰기 경로를 인용으로 오인하지 않는다).
# 트리 안 bare docs/…md 는 앞 글자가 경로 문자(영숫자 _ / . -)가 아니어야 한다 — URL 의
# /docs/x.md 가 빠진다. 둘 다 저장소 루트 기준 상대경로로 해석한다.
CITE_RE='(?:(?<!\.\./)(?<![[:alnum:]_.])\.\./|(?<![[:alnum:]_/.-]))docs/[[:alnum:]_./-]+\.md'

# scan <루트>: 표준출력에 없는 경로만 "파일:줄: 경로", 표준오류에 집계.
# 종료 0=없는 경로 없음(인용 0건 포함), 1=없는 경로 있음. ../docs 인용은 <루트>/../docs 가
# 있을 때만 검사하고 없으면 건너뛴 수만 적는다(CI 체크아웃에는 형제 저장소가 없다).
scan() (
  local root="$1" total=0 missing=0 skipped=0 sibling=0 f n p
  cd "$root"
  [ -d ../docs ] && sibling=1
  while IFS=: read -r f n p; do
    [[ "$f" =~ $ALLOW_FILES ]] && continue
    if [[ "$p" == ../docs/* ]] && [ "$sibling" -eq 0 ]; then
      skipped=$((skipped + 1))
      continue
    fi
    total=$((total + 1))
    # 트리 안 경로는 "추적 중" 이어야 실재다 — 디스크에만 있는(추적 제외) 문서는 CI 체크아웃에 없다.
    if [[ "$p" == ../docs/* ]]; then
      [ -e "$p" ] || { echo "$f:$n: $p"; missing=$((missing + 1)); }
    elif ! git ls-files --error-unmatch -- "$p" >/dev/null 2>&1; then
      echo "$f:$n: $p"
      missing=$((missing + 1))
    fi
  done < <(git ls-files -z -- '*.go' '*.sh' '*.yaml' '*.yml' '*.md' '*.tpl' 'Makefile' '*/Makefile' \
           | xargs -0 grep -HnoP "$CITE_RE" 2>/dev/null || true)
  echo "인용 ${total}건, 없는 경로 ${missing}건, 형제 저장소 부재로 건너뜀 ${skipped}건" >&2
  [ "$missing" -eq 0 ]
)

self_test() {
  local tmp out err rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/repo/docs" "$tmp/repo/hack" "$tmp/docs"
  git -C "$tmp/repo" init -q
  echo '# ok' > "$tmp/repo/docs/ok.md"
  echo '# 디스크에만 있고 추적하지 않는 문서' > "$tmp/repo/docs/untracked.md"
  echo '# ok' > "$tmp/docs/sib-ok.md"
  printf '// 근거: docs/ok.md 와 docs/missing.md 와 docs/untracked.md, ../docs/sib-ok.md 와 ../docs/sib-missing.md, https://example.com/x/docs/url.md\n' > "$tmp/repo/x.go"
  # ALLOW_FILES 제외 분기 — 이 이름의 파일은 자기 자신처럼 픽스처 문자열을 들고 있어도
  # 인용으로 집계되면 안 된다(C-1 과 같은 종류의 재발을 막는다).
  printf '// 픽스처: docs/excluded-missing.md\n' > "$tmp/repo/hack/doc-cite-check.sh"
  git -C "$tmp/repo" add x.go docs/ok.md hack/doc-cite-check.sh
  # 1) 트리 안 부재 1건 + 미추적 1건 + 형제 부재 1건, URL 제외, 제외파일 인용은 집계 밖
  rc=0; out="$(scan "$tmp/repo" 2>"$tmp/err")" || rc=$?
  [ "$rc" -eq 1 ] || { echo "self-test 1: 종료 $rc (기대 1)" >&2; return 1; }
  [ "$out" = $'x.go:1: docs/missing.md\nx.go:1: docs/untracked.md\nx.go:1: ../docs/sib-missing.md' ] || { echo "self-test 1: 출력 '$out'" >&2; return 1; }
  [[ "$out" != *excluded-missing* ]] || { echo "self-test 1: 제외 파일의 인용이 섞임 '$out'" >&2; return 1; }
  grep -q '형제 저장소 부재로 건너뜀 0건' "$tmp/err" || { echo "self-test 1: 건너뜀 집계 '$(cat "$tmp/err")'" >&2; return 1; }
  # 2) 형제 저장소가 없으면 ../docs 인용은 건너뛰고(sib-ok·sib-missing 둘) 트리 안 인용만 본다
  rm -rf "$tmp/docs"
  rc=0; out="$(scan "$tmp/repo" 2>"$tmp/err")" || rc=$?
  [ "$rc" -eq 1 ] && [ "$out" = $'x.go:1: docs/missing.md\nx.go:1: docs/untracked.md' ] || { echo "self-test 2: 종료 $rc 출력 '$out'" >&2; return 1; }
  grep -q '형제 저장소 부재로 건너뜀 2건' "$tmp/err" || { echo "self-test 2: 건너뜀 집계 '$(cat "$tmp/err")'" >&2; return 1; }
  # 3) 인용 0건은 통과
  : > "$tmp/repo/x.go"; git -C "$tmp/repo" add x.go
  rc=0; err="$(scan "$tmp/repo" 2>&1 >/dev/null)" || rc=$?
  [ "$rc" -eq 0 ] && [[ "$err" == *"인용 0건"* ]] || { echo "self-test 3: 종료 $rc 표준오류 '$err'" >&2; return 1; }
  echo "self-test ok"
}

case "${1:-}" in
  --self-test) self_test ;;
  *) scan "${1:-$(git rev-parse --show-toplevel)}" ;;
esac
