#!/usr/bin/env bash
# ============================================================
# docs-path-check.sh: 형제 docs 저장소 경로가 코드에 들어오는 것을 막는다
# 상세: docs 저장소는 remote 가 없어 이 저장소를 받은 사람이 docs/... 경로에 도달할 수
#       없다. 웹훅 거절 메시지처럼 사용자에게 가는 문자열에 그런 경로가 실리면 받는
#       사람은 열 수 없는 파일을 안내받는다. 근거는 저장소 안 경로(test/live/, *_test.go)
#       로 적는다. 예외는 아래 ALLOW 에 이유와 함께 적는다.
# 생성일: 2026-09-30
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."

# 예외. 각 줄은 "경로:이유" 다.
#   test/fault/suite_test.go   — 판정표 출력 대상. 디렉터리가 없으면 건너뛰도록 가드돼 있다.
#   cmd/kubectl-npu/main_test.go — 열 정렬 시험의 가짜 값.
#   Makefile                   — capmatrix 가 문서 저장소로 매트릭스를 생성하는 대상 경로.
#   internal/apiserver/routes_doc_test.go — 라우트 수를 문서와 대조하는 로컬 전용 검사.
#                                문서 저장소가 없으면 건너뛰도록 가드돼 있다.
ALLOW='^(test/fault/suite_test\.go|cmd/kubectl-npu/main_test\.go|Makefile|internal/apiserver/routes_doc_test\.go)$'

# 추적 파일만 본다. runs/ 아래는 실행이 그 시점에 낸 전사라 사후에 고치지 않는다.
DIRS='development|validation|feasibility|research|records|operations|features|comparison|history|decisions|architecture'

# 추적 파일만 본다. runs/ 아래는 실행이 그 시점에 낸 전사라 사후에 고치지 않는다.
# URL 은 제외한다 — 상위 프로젝트 문서 링크는 도달 가능하다.
hits=$(git ls-files \
  | grep -vE '/runs/' \
  | grep -vE "$ALLOW" \
  | xargs grep -nE "(^|[^A-Za-z0-9._/-])(docs/)?($DIRS)/[A-Za-z0-9._/-]+\\.(md|json)" 2>/dev/null \
  | grep -v 'http' || true)

if [ -n "$hits" ]; then
  echo "형제 docs 저장소 경로가 코드에 있다. 그 저장소는 remote 가 없어 도달할 수 없다."
  echo "근거는 test/live/ 하네스나 *_test.go 로 적고, 사실과 날짜는 산문으로 남겨라."
  echo
  echo "$hits"
  exit 1
fi
echo "docs 경로 검사 통과"
