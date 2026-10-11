#!/usr/bin/env bash
# ============================================================
# vendor-branch-count.sh: 컨트롤러에서 벤더 이름이 나오는 줄 수를 세어 상한과 비교
# 상세: internal/driveradapter 로 옮기지 못한, 컨트롤러에 남은 벤더 이름 줄 수를 상한과
#       비교한다(0 이 목표가 아니다). 세는 방식·상한의 근거는 아래 본문 주석 참고.
# 용법: hack/vendor-branch-count.sh (MAX_BRANCHES 로 상한 조정)
# 생성일: 2026-09-28 | 수정일: 2026-10-11
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."

# 벤더 판단이 컨트롤러 본문에 흩어지면 새 벤더를 더할 때마다 그 본문을 고쳐야 한다.
# 옮길 수 있는 것만 internal/driveradapter 로 옮겼고, 이 검사는 나머지가 다시 늘지
# 않는지를 지킨다(목표는 0 이 아니다 — partition·policy spec 탐색·벤더 DRA 렌더·
# descriptor↔registry 매핑·벤더 목록 순회·verify-probe 등은 이미 각자의 경계다).
# MAX_BRANCHES=70: 측정값 62(주석·선언 포함) + 여유 8 — 통상 한두 줄 추가는 안 걸리고
# 어댑터로 옮긴 것과 같은 규모(8줄)가 다시 흩어지면 잡는다.
MAX_BRANCHES="${MAX_BRANCHES:-70}"

# 세는 것은 실제 분기 수가 아니라 벤더 상수·"Spec.Vendor ==" 패턴이 나온 줄 수다 — 그
# 패턴을 설명하는 주석과 상수 선언 줄도 함께 잡혀 출력값이 실제 분기보다 많다.
n="$(grep -rn 'vendorNvidia\|vendorFuriosa\|vendorRebellions\|vendorTenstorrent\|Spec.Vendor ==' \
      internal/controller/*.go 2>/dev/null | grep -v '_test\.go:' | wc -l | tr -d ' ')"

printf '컨트롤러 벤더 이름 일치 줄 %s (상한 %s) — 주석·상수 선언 포함, 실제 분기는 더 적음. 대부분 partition·spec 탐색·DRA 렌더·어휘 매핑·벤더 목록·verify-probe 등 다른 경계 소속, 목표 0 아님\n' \
  "$n" "$MAX_BRANCHES"

# 범주별 정확한 줄 수는 적지 않는다 — 문서마다 같은 범주를 다른 수로 적어 하나로
# 확정할 수 없다. 이 수를 낮추려고 위 경계들을 어댑터에 밀어넣지 않는다 — 맞지 않는
# 경계에 욱여넣는 실수가 이 검사가 막으려는 실수보다 나쁘다.
if [ "$n" -le "$MAX_BRANCHES" ]; then
  echo "vendor-branch-count: OK"
  exit 0
fi
echo "vendor-branch-count: FAIL — 어댑터로 옮기지 않은 벤더 이름 줄이 상한을 넘는다"
exit 1
