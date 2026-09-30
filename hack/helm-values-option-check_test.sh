#!/usr/bin/env bash
# ============================================================
# helm-values-option-check_test.sh: helm-values-option-check.sh 의 판정을 시험한다
# 상세: 가짜 트리를 만들어 *.sh 파일의 같은 줄에 helm 과 --reuse-values 가 함께 있으면
#       실패하고, 그 외(확장자가 다르거나 같은 줄이 아니거나 --reset-then-reuse-values 만
#       쓴 경우)는 통과하는지 확인한다.
# 생성일: 2026-09-24 | 수정일: 2026-09-24
# ============================================================
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$HERE/helm-values-option-check.sh"
pass=0; fail=0
run() { # run <설명> <기대 종료코드> <트리 준비 함수>
  local desc="$1" want="$2" prep="$3" tmp rc
  tmp="$(mktemp -d)"; "$prep" "$tmp"
  set +e; "$CHECK" "$tmp" >/dev/null 2>&1; rc=$?; set -e
  rm -rf "$tmp"
  if [ "$rc" = "$want" ]; then pass=$((pass+1)); echo "PASS: $desc"
  else fail=$((fail+1)); echo "FAIL: $desc (exit=$rc, want=$want)"; fi
}
prep_clean() { printf 'helm upgrade r c --reset-then-reuse-values\n' > "$1/ok.sh"; }
prep_dirty() { printf 'helm upgrade r c --reuse-values --set a=b\n' > "$1/bad.sh"; }
prep_comment_only() { printf '# --reuse-values 는 쓰지 않는다\n' > "$1/doc.md"; }
prep_doc_with_helm() { printf '값을 바꿔 반영할 때 helm upgrade 에 --reuse-values 를 쓰지 않는다.\n' > "$1/README.md"; }
prep_sh_diff_lines() { printf 'helm upgrade r c --reset-values\n# 주석: --reuse-values 는 쓰지 않는다\n' > "$1/note.sh"; }
run "정상 옵션만 있으면 통과" 0 prep_clean
run "sh 파일 같은 줄에 단독 사용이 있으면 실패" 1 prep_dirty
run "금지를 설명하는 문장(비 sh, helm 없음)은 통과" 0 prep_comment_only
run "금지를 설명하는 문장(비 sh, helm 있음)도 확장자로 통과" 0 prep_doc_with_helm
run "sh 파일이라도 다른 줄이면 통과" 0 prep_sh_diff_lines
echo "PASS $pass / FAIL $fail"
[ "$fail" = 0 ]
