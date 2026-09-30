#!/usr/bin/env bash
# ============================================================
# chart-default-installable.sh: 차트 기본값만으로 드라이버 설치가 가능한 조합인지 검사
# 상세: 검사마다 막는 실수가 다르다.
#   1) 기본 렌더에 kernelImages 없음(항상) — 사설 레지스트리 주소와 커널 버전이 박힌
#      사이트별 값이 배포 차트 기본값에 섞여 들어가는 실수를 잡는다.
#   2) 벤더별 driver.version 이 그 벤더 verifiedVersions 에 포함(항상) — 기본 버전만
#      바꾸고 화이트리스트 갱신을 빠뜨리면 그 벤더 설치가 UnverifiedVersion 으로
#      거부되는 실수를 잡는다.
#   3) nvidia driver.version 의 우분투 아카이브 실재(CHECK_ARCHIVE=1 일 때만) — PPA 에만
#      있고 아카이브에는 없는 버전을 기본값에 두면, 이 차트를 그대로 쓰는 다른
#      클러스터가 설치에 실패한다(이번 계획의 발단 결함). apt-cache madison 은 호출
#      시점에 네트워크를 타지 않고 사전에 apt-get update 로 받아 둔 로컬 apt 목록만
#      읽는다. air-gap 이거나 그 목록이 오래된 호스트에서는 실재하는 버전도 FAIL 로
#      나올 수 있어 이 게이트를 기본으로 강제하지 않는다. 꺼진 경우 SKIP 으로
#      표시해 OK 와 구분한다 — SKIP 을 통과로 읽으면 "확인 안 함"과 "확인해서
#      문제 없음"을 혼동하게 된다.
# 참고: 검사 1·3 은 후보 문자열을 변수로 받아 bash 자체 패턴 매칭([[ == * ]])으로
#       판정한다. "생성 명령 | grep -q 패턴" 형태는 set -o pipefail 아래에서 grep 이
#       일치를 찾자마자 먼저 끝나 버리면 아직 쓰는 중이던 생성 명령이 SIGPIPE 로
#       죽어 판정이 뒤집힐 수 있다(있는데 없다고 나옴) — 그 파이프를 아예 두지 않는다.
# 용법: chart-default-installable.sh
#   환경변수: VALUES=검사할 values 파일 경로(기본 deploy/helm/values.yaml)
#             CHECK_ARCHIVE=1 이면 검사 3을 켠다(기본 0=꺼짐, 로컬 apt 목록 최신 필요)
# 생성일: 2026-09-26
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."
VALUES="${VALUES:-deploy/helm/values.yaml}"
rendered="$(helm template t deploy/helm -f "${VALUES}")"

fail=0
note() { printf '%s: %s\n' "$1" "$2"; }

# 1) 기본 렌더에 커널 이미지가 없어야 한다.
if [[ "${rendered}" == *"kernelImages:"* ]]; then
  note "기본 렌더 커널 이미지" "FAIL(기본값에 사이트 값이 있다)"; fail=1
else
  note "기본 렌더 커널 이미지" "OK(없음)"
fi

# 2) 벤더별 driver.version 이 그 벤더 verifiedVersions 에 있어야 한다.
python3 - "${VALUES}" <<'PY' || fail=1
import sys, yaml
v = yaml.safe_load(open(sys.argv[1]))
bad = []
for name, p in (v.get('driverInstallPolicies') or {}).items():
    if not isinstance(p, dict) or not p.get('enabled'):
        continue
    ver = ((p.get('driver') or {}).get('version') or '')
    verified = p.get('verifiedVersions') or []
    if ver and verified and ver not in verified:
        bad.append('%s: version %s 가 verifiedVersions 에 없다' % (name, ver))
for b in bad:
    print('  ' + b)
print('%s: %s' % ('벤더별 버전 화이트리스트', 'FAIL' if bad else 'OK'))
sys.exit(1 if bad else 0)
PY

# 3) 선택: nvidia 버전이 우분투 아카이브에 실재하는지(로컬 apt 목록 필요, 기본 꺼짐).
if [ "${CHECK_ARCHIVE:-0}" = "1" ]; then
  nv="$(python3 - "${VALUES}" <<'PY'
import sys, yaml
v = yaml.safe_load(open(sys.argv[1]))
p = (v.get('driverInstallPolicies') or {}).get('nvidia') or {}
print(((p.get('driver') or {}).get('version') or ''))
PY
)"
  madison_out="$(apt-cache madison "nvidia-kernel-common-${nv%%.*}" 2>/dev/null || true)"
  if [[ "${madison_out}" == *"${nv}"* ]]; then
    note "아카이브 실재(${nv})" "OK"
  else
    note "아카이브 실재(${nv})" "FAIL(아카이브에 없다)"; fail=1
  fi
else
  note "아카이브 실재" "SKIP(CHECK_ARCHIVE=1 로 켠다 — 미실행이며 통과 아님)"
fi

[ "${fail}" -eq 0 ] && { echo "chart-default-installable: OK"; exit 0; }
echo "chart-default-installable: FAIL"; exit 1
