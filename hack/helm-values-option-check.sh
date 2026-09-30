#!/usr/bin/env bash
# ============================================================
# helm-values-option-check.sh: helm upgrade 의 --reuse-values 단독 사용을 금지한다
# 상세: Helm v4.0.1 에서 --reuse-values 는 직전 릴리스가 계산해 둔 값을 통째로 재사용해,
#       사용자 값으로 덮지 않은 차트 기본값 변경이 반영되지 않는다. 그래서 차트만 고치고
#       반영한 변경이 조용히 빠진다. 대신 --reset-then-reuse-values 를 쓴다.
#       --reset-then-reuse-values 는 --reuse-values 를 부분 문자열로 포함하지 않는다
#       ("then-" 뒤가 대시 하나짜리 "-reuse-values"가 되기 때문) — 그래서 별도 제외 없이도
#       오탐이 없다. 검사 대상은 *.sh 파일뿐이고, 같은 셸 문장(줄 끝 \ 로 이어진 줄 포함)에
#       helm 과 --reuse-values 가 함께 있을 때만 위반으로 센다 — 금지를 설명하는 문서
#       문장까지 걸리지 않게 하기 위해서다. 물리적 줄만 보면 `helm upgrade ... \` 다음
#       줄에 `--reuse-values` 가 오는 실제 흔한 형태(이 저장소의 실제 결함이 이 모양이었다)를
#       놓치므로, \ 로 이어진 물리적 줄들을 하나의 셸 문장으로 합친 뒤에 판정한다.
# 용법: helm-values-option-check.sh [검사할 디렉터리(기본: 저장소 루트)]
# 생성일: 2026-09-24 | 수정일: 2026-09-24
# ============================================================
set -euo pipefail
root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

hits="$(find "$root" -type f -name '*.sh' \
    -not -path '*/.git/*' -not -path '*/.superpowers/*' \
    -not -name 'helm-values-option-check*' -print0 \
  | xargs -0 -r awk '
      function flush() {
        if (buf != "" && buf ~ /helm/ && buf ~ /--reuse-values/) {
          print FILENAME ":" startline ":" buf
        }
        buf = ""
      }
      FNR == 1 { flush() }
      {
        if (buf == "") startline = FNR
        line = $0
        if (sub(/\\[ \t]*$/, " ", line)) { buf = buf line }
        else { buf = buf line; flush() }
      }
      END { flush() }
    ' || true)"

if [ -n "$hits" ]; then
  echo "helm-values-option-check: --reuse-values 단독 사용 발견 — --reset-then-reuse-values 로 바꾼다" >&2
  printf '%s\n' "$hits" >&2
  exit 1
fi
echo "helm-values-option-check: --reuse-values 단독 사용 0건"
