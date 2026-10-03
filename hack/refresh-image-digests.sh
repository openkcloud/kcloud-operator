#!/usr/bin/env bash
# ============================================================
# refresh-image-digests.sh: 발행 뒤 image-manifest.yaml 의 digest 를 레지스트리 조회로 갱신
# 상세: deploy/integration/image-manifest.yaml kcloud: 절에서 digest 키를 가진 항목마다
#       레지스트리에 매니페스트 digest 를 물어 파일을 갱신한다. 조회만 하고 push 는 하지 않는다.
#       레지스트리 주소는 deploy/helm/values.yaml 의 global.registry 에서 읽고 KCLOUD_REGISTRY
#       환경변수로 덮어쓴다(사설 미러·내부 레지스트리를 볼 때. 주소를 코드에 적지 않는다).
#       토큰은 레지스트리가 돌려주는 WWW-Authenticate 챌린지의 realm 에서 익명으로 받는다
#       (GHCR 은 https://ghcr.io/token?scope=repository:<repo>:pull 형태).
#       404 는 그 태그가 발행되지 않았다는 레지스트리의 확답이라 어긋남으로 센다. 401·403·
#       그 밖의 응답과 네트워크 실패는 조회를 못 한 것이라 항목을 건드리지 않고 건너뛴 목록으로
#       보고한다 — 조회 실패를 "digest 없음" 으로 적지 않는다.
#       벤더 절은 대상이 아니다(upstream 태그 고정이고 digest 키도 대부분 없다).
#       --check 는 파일을 고치지 않고 어긋난 항목만 세어 종료 코드로 알린다.
# 용법: [KCLOUD_REGISTRY=<host>[:<port>]/<project>] hack/refresh-image-digests.sh [--check]
# 생성일: 2026-10-03
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."

check=0
case "${1:-}" in
  --check) check=1 ;;
  "") ;;
  *) echo "용법: $0 [--check]" >&2; exit 2 ;;
esac

manifest=deploy/integration/image-manifest.yaml
values=deploy/helm/values.yaml

# snap yq 는 /tmp 를 읽지 못한다. YAML 파싱은 python3 PyYAML 로 한다.
registry="${KCLOUD_REGISTRY:-$(python3 -c '
import sys, yaml
print((((yaml.safe_load(open(sys.argv[1])) or {}).get("global") or {}).get("registry") or "").strip("/"))
' "$values")}"
[ -n "$registry" ] || { echo "!! 레지스트리를 알 수 없다 (${values} global.registry 가 비었다)." >&2; exit 1; }
host="${registry%%/*}"
prefix="${registry#*/}"
[ "$prefix" = "$registry" ] && prefix=""

# 멀티아치 index 도 받아야 docker pull 이 보는 digest 와 같은 값이 온다.
ACCEPT='application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json'

# "<상태코드> <digest>" 를 돌려준다. 응답이 없으면 둘 다 빈 값이다.
head_manifest() {
  local repo="$1" tag="$2" url="https://${host}/v2/${1}/manifests/${2}"
  local chal realm svc scope tok=""
  chal="$(curl -sS -m 20 -I "$url" 2>/dev/null | tr -d '\r' | grep -i '^www-authenticate:' || true)"
  realm="$(sed -nE 's/.*realm="([^"]*)".*/\1/p' <<<"$chal")"
  if [ -n "$realm" ]; then
    svc="$(sed -nE 's/.*service="([^"]*)".*/\1/p' <<<"$chal")"
    scope="$(sed -nE 's/.*scope="([^"]*)".*/\1/p' <<<"$chal")"
    tok="$(curl -sS -m 20 "${realm}?service=${svc}&scope=${scope:-repository:${repo}:pull}" 2>/dev/null |
      python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("token") or d.get("access_token") or "")' 2>/dev/null || true)"
  fi
  curl -sS -m 20 -o /dev/null -D - ${tok:+-H "Authorization: Bearer ${tok}"} -H "Accept: ${ACCEPT}" "$url" 2>/dev/null |
    tr -d '\r' |
    awk 'tolower($1) ~ /^http\//{c=$2} tolower($1)=="docker-content-digest:"{d=$2} END{print c, d}'
}

entries="$(python3 -c '
import sys, yaml
d = yaml.safe_load(open(sys.argv[1])) or {}
for e in (d.get("kcloud") or []):
    e = e or {}
    if "digest" not in e:
        continue
    print("%s\t%s\t%s" % (e.get("image", ""), e.get("tag", ""), e.get("digest") or ""))
' "$manifest")"
[ -n "$entries" ] || { echo "!! ${manifest} kcloud: 절에 digest 항목이 없다." >&2; exit 1; }

updates=""   # 갱신할 "<이미지>\t<digest>" 줄
ok=0; mismatch=0; stale=0
mis_list=""; skip_list=""

while IFS=$'\t' read -r name tag recorded; do
  [ -n "$name" ] || continue
  read -r code got <<<"$(head_manifest "${prefix:+${prefix}/}${name}" "$tag")"
  case "$code" in
    200)
      if [ -z "$got" ]; then
        skip_list="${skip_list}  - ${name}:${tag} (digest 헤더 없음)
"
      elif [ "$got" = "$recorded" ]; then
        ok=$((ok + 1))
      else
        mismatch=$((mismatch + 1))
        mis_list="${mis_list}  - ${name}:${tag} 기록 ${recorded:-(없음)} -> 레지스트리 ${got}
"
        updates="${updates}${name}	${got}
"
      fi
      ;;
    404)
      # 레지스트리의 확답이다. 기록도 비어 있으면 미발행 사실이 그대로 적힌 것이라 맞는다.
      if [ -z "$recorded" ]; then
        ok=$((ok + 1))
      else
        mismatch=$((mismatch + 1)); stale=$((stale + 1))
        mis_list="${mis_list}  - ${name}:${tag} 기록 ${recorded} 인데 레지스트리에 그 태그 발행본이 없다
"
      fi
      ;;
    *)
      # 401·403 은 자격 증명이 필요한 경로, 빈 값은 응답 자체가 없는 경우다.
      skip_list="${skip_list}  - ${name}:${tag} (조회 못 함: ${code:-응답 없음})
"
      ;;
  esac
done <<< "$entries"

skipped="$(printf '%s' "$skip_list" | grep -c . || true)"
echo ">> ${registry} 기준 — 일치 ${ok}건, 어긋남 ${mismatch}건, 건너뜀 ${skipped}건"
[ -n "$mis_list" ] && { echo "어긋난 항목:"; printf '%s' "$mis_list"; }
[ -n "$skip_list" ] && { echo "건너뛴 항목(파일 미변경):"; printf '%s' "$skip_list"; }

if [ "$check" -eq 1 ]; then
  [ "$mismatch" -eq 0 ] || exit 1
  # 전부 건너뛰었다면 이 검사는 아무것도 확인하지 못했다. 초록으로 넘기지 않는다.
  if [ "$ok" -eq 0 ]; then
    echo "!! 확인한 항목이 0건이다. 레지스트리에 닿지 못했거나 전부 자격 증명이 필요하다." >&2
    exit 1
  fi
  echo ">> digest ${ok}건이 레지스트리와 맞는다 (건너뛴 ${skipped}건은 확인 못 함)"
  exit 0
fi

if [ -n "$updates" ]; then
  # 주석·순서를 보존해야 하므로 YAML 을 다시 쓰지 않고 digest 줄만 바꾼다.
  printf '%s' "$updates" | python3 -c '
import re, sys
path = sys.argv[1]
upd = dict(l.rstrip("\n").split("\t", 1) for l in sys.stdin if l.strip())
out, sect, cur = [], None, None
for line in open(path).read().splitlines(True):
    top = re.match(r"([A-Za-z][A-Za-z0-9_]*):", line)
    if top:
        sect, cur = top.group(1), None
    elif sect == "kcloud":
        img = re.match(r"\s*-\s*image:\s*(\S+)", line)
        if img:
            cur = img.group(1)
        elif cur in upd:
            m = re.match(r"(\s*digest:\s*)(\S*)(.*)", line)
            if m:
                line = "%s%s%s\n" % (m.group(1), upd.pop(cur), m.group(3).rstrip("\n"))
    out.append(line)
if upd:
    sys.exit("!! digest 줄을 찾지 못한 항목: %s" % ", ".join(sorted(upd)))
open(path, "w").write("".join(out))
' "$manifest"
  echo ">> ${manifest} 의 digest $(printf '%s' "$updates" | grep -c .)건 갱신 (push 하지 않는다)"
fi

if [ "$stale" -gt 0 ]; then
  echo "!! 발행본이 없어 갱신할 수 없는 항목 ${stale}건 남았다. 그 태그를 발행해야 한다." >&2
  exit 1
fi
