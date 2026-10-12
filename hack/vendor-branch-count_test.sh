#!/usr/bin/env bash
# ============================================================
# vendor-branch-count_test.sh: vendor-branch-count.sh 의 판정을 시험한다
# 상세: 현재 코드가 상한 안에서 OK 를 보고하는지, MAX_BRANCHES=0 이면 반드시 실패하는지
#       확인한다. `_test` 를 담았지만 `_test.go` 로 안 끝나는 파일이 `grep -v _test`
#       부분 문자열 필터에 통째로 새어나가지 않는지, 줄 전체가 // 주석인 줄은 세지 않는지도 본다.
# 생성일: 2026-09-28 | 수정일: 2026-10-13
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."
rc=0
out="$(bash hack/vendor-branch-count.sh 2>&1)" || rc=$?
if [ "${rc}" -eq 0 ] && [[ "${out}" == *"vendor-branch-count: OK"* ]]; then
  echo "PASS: 상한 안에 있다"
else
  echo "FAIL: ${out}"; exit 1
fi
# 상한을 0 으로 낮추면 실패해야 한다 — 검사가 실제로 걸 수 있음을 증명한다
if MAX_BRANCHES=0 bash hack/vendor-branch-count.sh >/dev/null 2>&1; then
  echo "FAIL: 상한 0 인데 통과했다"; exit 1
else
  echo "PASS: 상한을 넘으면 실패한다"
fi
# `_test.go` 로 끝나지 않지만 이름에 `_test` 를 담은 정상 파일은 세어져야 한다.
# MAX_BRANCHES 를 크게 고정해 둔다 — 안 고정하면 실제 분기 수가 상한에 1 차이로
# 붙었을 때 이 호출 자체가 FAIL(비0 종료)해 set -e 아래서 진단 메시지 없이
# 스크립트가 조용히 죽는다. 이 두 호출은 집계 로직만 보는 것이지 상한 판정을
# 보는 게 아니므로 상한을 고정해 그 경로를 아예 없앤다.
decoy="internal/controller/zz_test_filter_regression.go"
cleanup_decoy() { rm -f "$decoy"; }
trap cleanup_decoy EXIT
before="$(MAX_BRANCHES=999 bash hack/vendor-branch-count.sh | grep -oE '[0-9]+' | head -1)"
cat > "$decoy" <<'EOF'
package controller

var _ = vendorNvidia
EOF
after="$(MAX_BRANCHES=999 bash hack/vendor-branch-count.sh | grep -oE '[0-9]+' | head -1)"
cleanup_decoy
trap - EXIT
if [ "${after}" -gt "${before}" ]; then
  echo "PASS: 이름에 _test 를 담아도 _test.go 로 끝나지 않으면 센다(${before} -> ${after})"
else
  echo "FAIL: _test 를 담은 정상 파일이 필터에 새어나갔다(${before} -> ${after})"; exit 1
fi
# 줄 전체가 // 주석인 줄(들여쓰기 포함)은 벤더 이름이 있어도 세지 않는다. 끝 주석이 붙은
# 코드 줄은 센다 — 주석 두 줄과 코드 한 줄을 넣으면 정확히 1 늘어야 한다.
cdecoy="internal/controller/zz_comment_skip_regression.go"
cleanup_cdecoy() { rm -f "$cdecoy"; }
trap cleanup_cdecoy EXIT
before="$(MAX_BRANCHES=999 bash hack/vendor-branch-count.sh | grep -oE '[0-9]+' | head -1)"
cat > "$cdecoy" <<'EOF'
package controller

// vendorNvidia 를 설명하는 주석
	// vendorFuriosa 를 설명하는 들여쓴 주석
var _ = vendorRebellions // 끝 주석이 붙은 코드 줄
EOF
after="$(MAX_BRANCHES=999 bash hack/vendor-branch-count.sh | grep -oE '[0-9]+' | head -1)"
cleanup_cdecoy
trap - EXIT
if [ "${after}" -eq "$((before + 1))" ]; then
  echo "PASS: 주석 줄은 세지 않고 코드 줄만 센다(${before} -> ${after})"
else
  echo "FAIL: 주석 줄 제외가 기대와 다르다(${before} -> ${after}, $((before + 1)) 기대)"; exit 1
fi
