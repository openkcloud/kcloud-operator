#!/usr/bin/env bash
# ============================================================
# image-tag-check.sh: 차트가 당기는 이미지 태그가 레지스트리에 실재하는지 확인
# 상세: helm 렌더에서 이미지 참조를 모아 레지스트리에 매니페스트 HEAD 를 물어
#       "설치하면 당겨지는 태그" 가 실제로 발행돼 있는지 본다. 태그 문자열이 차트·문서에
#       맞게 적혀 있는지만 보는 기존 검사들은 발행 여부를 보지 않아, 미발행 태그가
#       그대로 통과했다.
#       이미지는 한 자리에만 적히지 않는다 — image: 필드, *Image 키(렌더된
#       NPUClusterPolicy·DriverInstallPolicy 안), 커맨드 인자(--generated-*-image=),
#       환경변수(*_IMAGE) 넷이다. 그래서 이름을 열거하지 않고 두 신호로 잡는다:
#         (1) 이름 패턴 — image 로 끝나는 키, image 를 품은 env 이름·인자 플래그
#         (2) 값 모양 — <호스트>[:<포트>]/<경로>:<태그> 또는 @sha256:...
#       둘 중 하나라도 걸리면 수집한다. 새 환경변수·새 키가 생겨도 빠지지 않는다.
#       이름 신호는 맵에도 적용한다 — kernelImages 는 <커널 버전>: <이미지> 꼴이라 키에
#       신호가 없고 값에 레지스트리 호스트가 없을 수 있다. 그런 값은 어디에 물을지
#       알 수 없어 건너뜀으로 세고 수를 보고한다 — 조용히 빠지는 것과 다르다.
#       조회 대상은 차트 기본값 렌더뿐이다. 다른 프리셋은 사설 미러나 자리표시자를
#       가리켜 익명 조회가 성립하지 않으므로 수집해 수만 보고하고 조회는 건너뛴다.
#       공개 경로 설치가 성립하는지가 이 검사의 목적이다.
#       판정은 hack/registry-head.sh 의 상태 코드로 한다 — 200 은 발행, 404 는 그 태그가
#       없다는 레지스트리의 확답, 그 밖(401·403·무응답)은 조회를 못 한 것이다. 다만 차트가
#       직접 발행하는 레지스트리 접두(global.registry) 아래에서 익명 조회가 거부되면
#       공개 경로로 당길 수 없다는 뜻이라 결함으로 센다 — 그 자리가 2026-10-02 에
#       미발행을 숨겼다. 그 예외에도 ALLOWED_PRIVATE(hack/registry-head.sh 에서 source,
#       refresh-image-digests.sh 와 공유) 목록은 비켠다 — 조건부 기능 전용이라 GHCR 발행
#       계획이 없는 이미지를 두 검사가 같은 말로 "허용된 비공개" 로 센다.
# 용법: hack/image-tag-check.sh
#       hack/image-tag-check.sh --collect-only < <렌더>   (수집기만, 조회 없음 — 회귀 시험용)
# 생성일: 2026-10-04 | 수정일: 2026-10-04 (ALLOWED_PRIVATE 허용 목록을
#         refresh-image-digests.sh 와 공유)
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."
. hack/registry-head.sh

chart=deploy/helm
values=$chart/values.yaml

# 수집기: 렌더(stdin)에서 "<분류><TAB><참조>" 를 자리마다 한 줄씩 낸다.
# snap yq 는 /tmp 를 읽지 못한다. YAML 파싱은 python3 PyYAML 로 한다.
collect() {
  python3 -c '
import sys, re, yaml

# <호스트>[:<포트>]/<경로>:<태그> 또는 <경로>@sha256:... — 호스트에 점이나 포트가 있어야
# 레지스트리로 본다(nvidia.com/gpu 같은 라벨 값이 끼지 않게).
REF = r"(:[A-Za-z0-9][A-Za-z0-9._-]*|@sha256:[0-9a-f]{64})$"
SHAPE = re.compile(r"^[a-z0-9][a-z0-9.:-]*/[a-z0-9][a-z0-9._/-]*" + REF)
# 이름 신호로 잡힌 자리는 레지스트리 호스트가 없어도 받는다(nginx:1.25 꼴). 다만 태그나
# digest 는 있어야 한다 — 그래야 NOT_AN_IMAGE=kube-system 처럼 이름만 걸린 자리가 빠진다.
LOOSE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]*" + REF)
PLACEHOLDER = re.compile(r"[<>${}]")

def shaped(v):
    if not SHAPE.match(v):
        return False
    host = v.split("/")[0]
    return "." in host or ":" in host

# kernelImages 처럼 맵을 담는 복수형 이름도 같은 신호로 본다.
def named(k):
    return re.search(r"(?i)images?$", k) is not None

# 같은 자리를 두 신호가 함께 잡으면 구체적인 분류를 남긴다(숫자 작은 쪽이 우선).
PRIO = {"image": 0, "key": 1, "map": 2, "env": 3, "arg": 4, "shape": 5}
sites = {}

def record(path, cls, ref):
    ref = ref.strip()
    if not ref or PLACEHOLDER.search(ref):
        return
    cur = sites.get(path)
    if cur is None or PRIO[cls] < PRIO[cur[0]]:
        sites[path] = (cls, ref)

def walk(node, path):
    if isinstance(node, dict):
        name, value = node.get("name"), node.get("value")
        if isinstance(name, str) and isinstance(value, str) and re.search(r"(?i)image", name) \
                and LOOSE.match(value):
            record(path + "/value", "env", value)
        for k, v in node.items():
            k = str(k)
            if isinstance(v, str):
                if named(k) and LOOSE.match(v):
                    record(path + "/" + k, "image" if k == "image" else "key", v)
                elif shaped(v):
                    record(path + "/" + k, "shape", v)
            elif isinstance(v, dict) and named(k):
                # kernelImages 는 <커널 버전>: <이미지> 맵이다. 키가 커널 버전이라 이름
                # 신호가 없고, 값도 레지스트리 호스트 없이 적을 수 있어(CRD 가 허용한다)
                # 값 모양 신호에도 걸리지 않는다. 맵 이름으로 받는다.
                for mk, mv in v.items():
                    if isinstance(mv, str) and LOOSE.match(mv):
                        record("%s/%s/%s" % (path, k, mk), "map", mv)
            walk(v, path + "/" + k)
    elif isinstance(node, list):
        for i, v in enumerate(node):
            p = "%s/%d" % (path, i)
            if isinstance(v, str):
                flag, _, rhs = v.partition("=")
                if rhs and LOOSE.match(rhs) and (re.search(r"(?i)image", flag) or shaped(rhs)):
                    record(p, "arg", rhs)
                elif shaped(v):
                    record(p, "shape", v)
            walk(v, p)

# 자리 식별에 문서 번호를 넣는다. 같은 kind 가 여러 번 나오면 경로 문자열이 겹쳐
# 서로 다른 자리가 하나로 접힌다(Job 셋, DriverInstallPolicy 넷).
for i, doc in enumerate(yaml.safe_load_all(sys.stdin.read())):
    if doc:
        walk(doc, "%d/%s" % (i, doc.get("kind", "?") if isinstance(doc, dict) else "?"))

for cls, ref in sites.values():
    print("%s\t%s" % (cls, ref))
'
}

# helm stderr 는 버리지 않고 모아 둔다. 렌더가 깨진 것과 모을 이미지가 없는 것은 다르다.
renderlog="$(mktemp)"
trap 'rm -f "$renderlog"' EXIT
render() { helm template kcloud "$chart" "$@" 2>"$renderlog"; }

# 수집기만 돌린다. 레지스트리에 닿지 않으므로 hack/image-tag-check_test.sh 가 이 경로로
# 분류·중복 제거·자리표시자 걸러내기를 고정한다.
if [ "${1:-}" = "--collect-only" ]; then
  collect
  exit 0
fi

if ! default_render="$(render)"; then
  echo "!! 차트 기본값 렌더가 실패했다: $(head -1 "$renderlog")" >&2
  exit 1
fi
default_rows="$(printf '%s\n' "$default_render" | collect | sort)"
[ -n "$default_rows" ] || { echo "!! 차트 기본값 렌더에서 이미지를 하나도 못 모았다 — 수집기 확인" >&2; exit 1; }

label() {
  case "$1" in
    image) echo "image: 필드          " ;;
    key)   echo "*Image 키            " ;;
    map)   echo "*Images 맵 값        " ;;
    arg)   echo "커맨드 인자          " ;;
    env)   echo "환경변수             " ;;
    shape) echo "그 밖(값 모양만 일치)" ;;
  esac
}

echo ">> 차트 기본값 렌더 — 참조 자리 $(printf '%s\n' "$default_rows" | grep -c .)개, 고유 이미지 $(printf '%s\n' "$default_rows" | cut -f2 | sort -u | grep -c .)개"
for cls in image key map arg env shape; do
  rows="$(printf '%s\n' "$default_rows" | awk -F'\t' -v c="$cls" '$1==c')"
  n="$(printf '%s' "$rows" | grep -c . || true)"
  [ "$n" -gt 0 ] || continue
  echo "   $(label "$cls") 자리 ${n}개 / 고유 $(printf '%s\n' "$rows" | cut -f2 | sort -u | grep -c .)개"
done

# 차트가 직접 발행하는 접두. 이 아래는 익명 조회가 되어야 공개 경로 설치가 성립한다.
own="$(python3 -c '
import sys, yaml
print((((yaml.safe_load(open(sys.argv[1])) or {}).get("global") or {}).get("registry") or "").strip("/"))
' "$values")"

published=0; missing=0; skipped=0; allowed=0
# 건너뜀 둘을 가른다 — unresolved 는 다시 돌리면 달라질 수 있는 조회 실패(자격 증명·요율
# 제한·무응답)이고, unqueryable 은 레지스트리 주소가 없어 물을 곳 자체가 없는 자리다.
unresolved=0; unqueryable=0
miss_list=""; skip_list=""; allow_list=""

while read -r ref; do
  [ -n "$ref" ] || continue
  host="${ref%%/*}"
  rest="${ref#*/}"
  if [ "$host" = "$ref" ] || { [ "${host#*.}" = "$host" ] && [ "${host#*:}" = "$host" ]; }; then
    skipped=$((skipped + 1)); unqueryable=$((unqueryable + 1))
    skip_list="${skip_list}  - ${ref} (레지스트리 주소가 없어 어디에 물을지 알 수 없다)
"
    continue
  fi
  case "$rest" in
    *@sha256:*) repo="${rest%@*}"; tag="sha256:${rest##*@sha256:}" ;;
    *) repo="${rest%:*}"; tag="${rest##*:}" ;;
  esac
  read -r code _ <<<"$(head_manifest_retry "$host" "$repo" "$tag")"
  case "$code" in
    200) published=$((published + 1)) ;;
    404)
      missing=$((missing + 1))
      miss_list="${miss_list}  - ${ref} (레지스트리가 그 태그는 없다고 답했다)
"
      ;;
    401|403)
      if [ -n "$own" ] && [ "${ref#"$own"/}" != "$ref" ]; then
        bare="${ref#"$own"/}"
        reason="${ALLOWED_PRIVATE[$bare]:-}"
        if [ -n "$reason" ]; then
          allowed=$((allowed + 1))
          allow_list="${allow_list}  - ${ref} (허용된 비공개: ${reason})
"
        else
          missing=$((missing + 1))
          miss_list="${miss_list}  - ${ref} (익명 조회 거부 ${code} — 공개 경로로 당길 수 없다)
"
        fi
      else
        skipped=$((skipped + 1)); unresolved=$((unresolved + 1))
        skip_list="${skip_list}  - ${ref} (자격 증명이 필요하다: ${code})
"
      fi
      ;;
    *)
      skipped=$((skipped + 1)); unresolved=$((unresolved + 1))
      skip_list="${skip_list}  - ${ref} (조회 못 함: ${code:-응답 없음})
"
      ;;
  esac
done <<< "$(printf '%s\n' "$default_rows" | cut -f2 | sort -u)"

echo ">> 조회 — 발행 ${published}건, 미발행 ${missing}건, 건너뜀 ${skipped}건(조회 실패 ${unresolved}, 주소 없음 ${unqueryable}), 허용된 비공개 ${allowed}건"
[ -n "$miss_list" ] && { echo "미발행(설치하면 당기지 못한다):"; printf '%s' "$miss_list"; }
[ -n "$skip_list" ] && { echo "건너뛴 항목(발행 여부를 확인하지 못했다):"; printf '%s' "$skip_list"; }
[ -n "$allow_list" ] && { echo "허용된 비공개 항목(결함 아님):"; printf '%s' "$allow_list"; }

# 다른 프리셋은 수집만 한다. 사설 미러·자리표시자를 가리켜 익명 조회가 성립하지 않는다.
preset_fail=0
for f in "$chart"/values-*.yaml; do
  [ -f "$f" ] || continue
  if ! out="$(render -f "$f")"; then
    preset_fail=$((preset_fail + 1))
    echo ">> $(basename "$f") — 렌더 실패(수집 못 함): $(head -1 "$renderlog")"
    continue
  fi
  rows="$(printf '%s\n' "$out" | collect | cut -f2 | sort -u || true)"
  n="$(printf '%s' "$rows" | grep -c . || true)"
  if [ "$n" -eq 0 ]; then
    echo ">> $(basename "$f") — 자리표시자를 걸러낸 뒤 남은 이미지 0개(조회 대상 아님)"
  else
    extra="$(comm -13 <(printf '%s\n' "$default_rows" | cut -f2 | sort -u) <(printf '%s\n' "$rows") | grep -c . || true)"
    echo ">> $(basename "$f") — 고유 ${n}개(기본값에 없는 것 ${extra}개), 조회 건너뜀: 사설 미러·자리표시자"
  fi
done

if [ "$preset_fail" -gt 0 ]; then
  echo "!! 프리셋 렌더 ${preset_fail}건이 실패했다 — 그 프리셋의 이미지는 수집조차 못 했다." >&2
  exit 1
fi
if [ "$missing" -gt 0 ]; then
  echo "!! 미발행 ${missing}건 — 그 태그를 발행하거나 차트가 가리키는 태그를 고쳐야 한다." >&2
  exit 1
fi
# 전부 건너뛰었다면 이 검사는 아무것도 확인하지 못했다. 초록으로 넘기지 않는다.
if [ "$published" -eq 0 ]; then
  echo "!! 발행을 확인한 항목이 0건이다 — 레지스트리에 닿지 못했다." >&2
  exit 1
fi
# 부분 장애도 초록이 되지 않게 한다. 기준: 레지스트리가 확답을 준 자리(발행+미발행) 대비
# 조회 실패가 4분의 1을 넘으면 확인한 것이 너무 적다고 본다. 레지스트리 주소가 없어
# 애초에 물을 수 없는 자리는 이 비율에서 뺀다 — 일시적 장애가 아니라 영구적 성격이다.
queryable=$((published + missing + unresolved))
if [ "$unresolved" -gt 0 ] && [ $((unresolved * 4)) -gt "$queryable" ]; then
  echo "!! 조회 실패 ${unresolved}건이 조회 대상 ${queryable}건의 4분의 1을 넘는다 — 확인한 것이 너무 적다." >&2
  exit 1
fi
echo ">> 기본값 설치가 당기는 이미지 ${published}건이 레지스트리에 있다 (건너뛴 ${skipped}건은 확인 못 함, 허용된 비공개 ${allowed}건)"
