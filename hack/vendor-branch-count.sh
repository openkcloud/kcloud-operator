#!/usr/bin/env bash
# ============================================================
# vendor-branch-count.sh: 컨트롤러에서 벤더 이름이 나오는 줄 수를 세어 상한과 비교
# 상세: 벤더 판단이 컨트롤러 본문에 흩어지면 새 벤더를 더할 때마다 그 본문을 고쳐야
#       한다. 계획 M 이 그중 옮길 수 있는 것만 internal/driveradapter 로 옮겼고, 이
#       검사는 옮기지 않은 나머지가 다시 늘지 않는지를 지킨다.
#
#       이 스크립트가 세는 것은 실제 조건 분기 수가 아니라 벤더 상수·
#       "Spec.Vendor ==" 패턴이 나온 줄 수다. 그 패턴을 설명하는 주석과 그 상수를
#       선언하는 줄도 함께 잡힌다(예: npuclusterpolicy_controller.go 의 vendorNvidia
#       상수 선언과 그 위 설명 주석). 그래서 출력값은 실제 분기 수보다 많다 — 실제
#       분기는 이 수보다 적다.
#
#       상한은 0 이 목표가 아니다. 남은 줄의 대부분은 이미 각자의 경계에 속한
#       판단이다 — partition 정책 판정(이미 partition.Backend 뒤), 정책 spec 하위
#       구조 탐색, 벤더별 DRA 렌더, descriptor↔registry 어휘 매핑, 벤더 목록
#       순회·호출부, 기능 검증 probe(세부 내역·판단 근거는
#       .superpowers/sdd/2026-09-26-plan-m-vendor-adapter/task-1-report.md 5절).
#       범주별 정확한 줄 수는 여기 적지 않는다 — 재확인 과정에서 문서마다 같은
#       범주를 다른 수로 적고 있어 하나로 확정하지 못했다. 이 수를 낮추려고
#       이들을 어댑터에 밀어넣지 않는다 — 맞지 않는 경계에 욱여넣는 실수가 이
#       검사가 막으려는 실수보다 나쁘다.
# 용법: hack/vendor-branch-count.sh
#   환경변수: MAX_BRANCHES=상한(기본 70). 2026-09-28 측정값 62(주석·선언 포함) 에
#             여유 8 을 더한 값 — 통상 작업이 기존 벤더 분기에 한두 줄을 더해도
#             걸리지 않을 만큼이면서, 어댑터로 옮긴 것과 같은 규모(8줄)의 벤더
#             판단이 다시 흩어져 생기면 잡을 만큼으로 잡았다. 주석·선언이 섞여
#             있어 실제 분기 기준 여유는 8 보다 크다.
# 생성일: 2026-09-28 | 수정일: 2026-09-28
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."

MAX_BRANCHES="${MAX_BRANCHES:-70}"

n="$(grep -rn 'vendorNvidia\|vendorFuriosa\|vendorRebellions\|vendorTenstorrent\|Spec.Vendor ==' \
      internal/controller/*.go 2>/dev/null | grep -v '_test\.go:' | wc -l | tr -d ' ')"

printf '컨트롤러 벤더 이름 일치 줄 %s (상한 %s) — 주석·상수 선언 포함, 실제 분기는 더 적음. 대부분 partition·spec 탐색·DRA 렌더·어휘 매핑·벤더 목록·verify-probe 등 다른 경계 소속, 목표 0 아님\n' \
  "$n" "$MAX_BRANCHES"

if [ "$n" -le "$MAX_BRANCHES" ]; then
  echo "vendor-branch-count: OK"
  exit 0
fi
echo "vendor-branch-count: FAIL — 어댑터로 옮기지 않은 벤더 이름 줄이 상한을 넘는다"
exit 1
