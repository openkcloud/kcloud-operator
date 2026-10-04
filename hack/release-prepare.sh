#!/usr/bin/env bash
# ============================================================
# release-prepare.sh: push 직전까지의 발행 준비를 한 번에 돌린다
# 상세: 선행 조건과 정합 검사를 돌리고, 필요한 태그를 로컬에만 만들고, 사용자가 실행할
#       push 명령을 순서대로 출력한다. 이 스크립트는 push 하지 않는다. remote 를 읽는
#       곳은 digest 검사(레지스트리 조회) 하나뿐이고 git remote 는 주소를 읽는 데만 쓴다.
#       기본은 dry-run 이고 --apply 가 로컬 태그를 만든다. 선행 조건이 하나라도 걸리면
#       --apply 는 태그를 만들지 않는다 — dry-run 은 그대로 끝까지 돌아 무엇이 왜 걸렸고
#       무슨 태그·명령이 필요한지 보여준다. 이미 있는 태그는 가리키는 커밋만 대조하고
#       옮기지 않는다.
#       형제 images 저장소 경로 규약은 test/airgap/manifest-coverage.sh 와 같다.
#       공개 트리가 빼는 경로·변환하는 파일 목록은 hack/public-chain.sh 에서 읽는다 —
#       그 파일 자체가 공개 트리에 없으므로, 없으면 그 검사만 건너뛴다(내부 전용 검사).
# 용법: hack/release-prepare.sh [--apply] [<묶음 이름>]
# 생성일: 2026-10-03 | 수정일: 2026-10-04
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."

op="$PWD"
img="$PWD/../images/kcloud-operator-images"
pub=public/main
chain=hack/public-chain.sh

apply=0
bundle=""
for a in "$@"; do
  case "$a" in
    --apply) apply=1 ;;
    --dry-run) apply=0 ;;
    -*) echo "용법: $0 [--apply] [<묶음 이름>]" >&2; exit 2 ;;
    *) bundle="$a" ;;
  esac
done

[ -d "$img/.git" ] || { echo "!! 형제 images 저장소가 없다 — ${img}" >&2; exit 1; }
img="$(cd "$img" && pwd)"   # 출력에 ../ 가 섞인 경로를 내지 않는다

fail=0
ok()   { printf '   [OK]   %s\n' "$*"; }
bad()  { printf '   [FAIL] %s\n' "$*"; fail=$((fail + 1)); }
note() { printf '   [--]   %s\n' "$*"; }
step() { printf '\n%s\n' "$*"; }
indent() { sed 's/^/          /'; }

# 패턴을 코드에 두 번 적지 않는다. 셸 스크립트의 단일 인용 할당에서 그대로 읽는다.
var_of() { grep -oP "^$2='\K[^']+" "$1" 2>/dev/null || true; }

printf '발행 준비 — %s\n' "$([ "$apply" -eq 1 ] && echo '--apply (로컬 태그 생성)' || echo 'dry-run (아무것도 바꾸지 않는다)')"

# ------------------------------------------------------------ 1단계
step "== 1단계 선행 조건 =="

chart=deploy/helm/Chart.yaml
ver="$(grep -oP '^version:\s*\K\S+' "$chart" || true)"
app="$(grep -oP '^appVersion:\s*\K\S+' "$chart" || true)"
op_tag="$app"
# release.yml 이 태그 push 에서 하는 대조와 같은 규칙이다(라인 태그는 -k8s<minor> 접미).
expected="${app#v}"; expected="${expected%-k8s*}"
if [ -z "$app" ] || [ -z "$ver" ]; then
  bad "${chart} 에서 version·appVersion 을 읽지 못했다"
elif [ "$ver" != "$expected" ]; then
  bad "${chart} version ${ver} 가 appVersion ${app} 과 어긋난다 — 태그 push 에서 release.yml 이 거절한다"
else
  ok "차트 version ${ver} / appVersion ${app} — operator 발행 태그는 ${op_tag}"
fi

# 프리셋 image.tag 대조는 chart_version_sync_test.go 가 이미 한다. 같은 대조를 여기에
# 다시 적으면 한쪽만 고쳐 또 어긋난다.
log="$(mktemp)"; blob="$(mktemp)"; trap 'rm -f "$log" "$blob"' EXIT
if go test ./internal/controller/ -count=1 \
     -run 'TestChartAppVersionMatchesPresets|TestDefaultValuesTagMatchesAppVersion' >"$log" 2>&1; then
  ok "작업 트리 프리셋 image.tag 가 차트 선언과 맞는다 (chart_version_sync_test.go)"
else
  bad "프리셋 image.tag 대조 실패 (chart_version_sync_test.go)"
  indent <"$log"
fi

# 두 라인 프리셋이 operator 이미지 말고는 같은 이미지를 쓰는지 본다. 라인별로 갈리는
# 산출물이 operator 하나뿐이라는 것이 릴리스 세트의 전제이고, 프리셋에 다른 이미지가
# 끼면 그 전제가 조용히 깨진다. 반대 라인 프리셋은 이 차트에 없으므로(kubeVersion 선언과
# 실재를 맞추려고 뺐다) 그 브랜치의 blob 을 꺼내 같은 Go 시험에 넘긴다 — 규칙을 두 번
# 적지 않는다. ref 가 없으면 건너뛰지 않고 걸린 것으로 센다. 건너뛰면 이 검사가 한 번도
# 돌지 않은 채 늘 초록이 된다.
other_ref=release/k8s-1.28
other_preset=deploy/helm/values-k8s1.28.yaml
if ! git show "${other_ref}:${other_preset}" >"$blob" 2>"$log"; then
  bad "${other_ref}:${other_preset} 를 읽지 못했다 — 두 라인 프리셋 대조를 할 수 없다"
  indent <"$log"
elif [ ! -s "$blob" ]; then
  # 빈 blob 은 Go 시험을 공허하게 통과시킨다 — 이미지 좌표가 0건이라 아무것도 걸리지 않는다.
  bad "${other_ref}:${other_preset} 가 비었다 — 대조할 내용이 없다"
elif KCLOUD_OTHER_LINE_PRESET="$blob" go test ./internal/controller/ -count=1 \
       -run TestReleaseLinePresetsShareNonOperatorImages >"$log" 2>&1; then
  ok "두 라인 프리셋이 operator 이미지 말고는 같은 이미지를 쓴다 (${other_ref} blob 대조)"
else
  bad "프리셋이 operator 외 이미지를 재정의한다 (${other_ref}:${other_preset} 대조)"
  indent <"$log"
fi

for r in "$op" "$img"; do
  if [ -n "$(git -C "$r" status --porcelain)" ]; then
    bad "$(basename "$r") 작업 트리가 깨끗하지 않다 — 커밋되지 않은 변경은 발행본에 들어가지 않는다"
    git -C "$r" status --porcelain | indent
  else
    ok "$(basename "$r") 작업 트리 깨끗"
  fi
done

priv="$(var_of hack/private-ip-check.sh PRIV_RE)"
[ -n "$priv" ] || bad "hack/private-ip-check.sh 에서 PRIV_RE 를 읽지 못했다 — 사설 문자열 검사를 할 수 없다"

for kind in operator images; do
  case "$kind" in
    operator) r="$op"; ex="$(var_of "$chain" OP_EXCLUDE_RE)" ;;
    images)   r="$img"; ex="$(var_of "$chain" EXCLUDE_RE)" ;;
  esac
  if ! git -C "$r" rev-parse --verify -q "$pub" >/dev/null; then
    bad "${kind}: ${pub} 브랜치가 없다"
    continue
  fi
  head="$(git -C "$r" rev-parse --abbrev-ref HEAD)"

  # 공개 브랜치가 내부 작업 내용을 담는가. 커밋 도달 가능성이 아니라 두 트리의 파일
  # 목록 차이를 본다 — 공개 체인은 commit-tree 로 새 커밋을 만들어 해시가 늘 달라지므로
  # 도달 가능성으로 보면 영원히 빨강이다. 공개 체인이 빼는 경로와 내용을 고치는 파일은
  # 재생성 뒤에도 차이가 남으므로 실패로 세지 않는다.
  if [ -z "$ex" ]; then
    note "${kind}: ${chain} 가 없어 제외 경로를 알 수 없다 — 내부 작업 반영 검사는 건너뛴다"
  else
    left="$(git -C "$r" diff --name-only "$head" "$pub" | { grep -vE "$ex" || true; })"
    # 공개 체인이 변환하는 파일 목록도 그 스크립트에서 읽는다(prune_blob 호출 대상).
    xform=""; shown=""
    xform="$(grep -oP '^\s+prune_blob "\$c" \K\S+' "$chain" || true)"
    if [ -n "$xform" ] && [ -n "$left" ]; then
      shown="$(printf '%s\n' "$left" | { grep -Fxf <(printf '%s\n' "$xform") || true; })"
      left="$(printf '%s\n' "$left" | { grep -vFxf <(printf '%s\n' "$xform") || true; })"
    fi
    if [ -n "$left" ]; then
      bad "${kind}: ${head} 의 내용이 ${pub} 에 없다 — 통합과 공개 체인 재생성이 먼저다"
      printf '%s\n' "$left" | indent
    else
      ok "${kind}: ${pub} 가 ${head} 의 내용을 담는다 (공개 체인 제외·변환 대상 빼고)"
    fi
    if [ -n "$shown" ]; then
      note "${kind}: 공개 체인이 변환하는 파일이라 차이가 정상이다 — $(printf '%s' "$shown" | tr '\n' ' ')"
      note "${kind}: 변환 대상에 새 항목이 늘어난 경우는 이 검사가 보지 못한다 — 2단계 bundle-check 가 공개 ref 에서 본다"
    fi
  fi

  # 사설 문자열은 경로만 적는다. 매칭된 줄을 출력하면 그 주소가 이 출력에 실린다.
  if [ -n "$priv" ]; then
    hits="$(git -C "$r" grep -l -I -E "$priv" "$pub" -- . 2>/dev/null || true)"
    if [ -n "$hits" ]; then
      bad "${kind}: ${pub} 트리에 사설 문자열이 있는 파일 $(printf '%s\n' "$hits" | grep -c .)건"
      printf '%s\n' "$hits" | indent
    else
      ok "${kind}: ${pub} 트리 사설 문자열 0건"
    fi
  fi
done

# ------------------------------------------------------------ 2단계
step "== 2단계 정합 검사 =="

out="$(bash "$img/hack/bundle-check.sh" "$pub" 2>&1)" && rc=0 || rc=$?
printf '%s\n' "$out" | indent
if [ "$rc" -eq 0 ]; then
  ok "images: 발행 목록과 ${pub} 트리의 실재가 맞는다"
else
  bad "images: 발행 목록과 ${pub} 트리의 실재가 어긋난다"
fi

echo
out="$(bash hack/refresh-image-digests.sh --check 2>&1)" && rc=0 || rc=$?
printf '%s\n' "$out" | indent
# 두 빨강을 구분한다 — 기록과 다른 digest 가 올라와 있는 것(결함)과, 아직 발행되지 않아
# 대조할 것이 없는 것(발행 뒤 해소)은 다른 사실이다.
mis="$(printf '%s\n' "$out" | LC_ALL=C grep -c ' -> 레지스트리 ' || true)"
pend="$(printf '%s\n' "$out" | LC_ALL=C grep -c '발행본이 없다' || true)"
if [ "$rc" -eq 0 ]; then
  ok "image-manifest digest 가 레지스트리와 맞는다"
elif [ "$mis" -eq 0 ] && [ "$pend" -gt 0 ]; then
  note "미발행 ${pend}건 때문에 빨강이다 — 발행 뒤 해소된다. 선행 조건 실패로 세지 않는다"
else
  bad "image-manifest digest 가 레지스트리와 어긋난다 (불일치 ${mis}건)"
fi

# ------------------------------------------------------------ 3단계
step "== 3단계 태그 =="

plan=""       # "<repo>\t<태그>\t<커밋>\t<설명>" — --apply 가 만들 태그
pushable=""   # "<repo>\t<태그>" — push 명령에 넣을 태그

# want_tag: 태그가 가리켜야 할 커밋을 대조한다. 있으면 확인만, 없으면 만들 목록에 넣는다.
want_tag() {
  local r="$1" t="$2" target="$3" what="$4" at cur
  if ! at="$(git -C "$r" rev-parse --verify -q "${target}^{commit}")"; then
    bad "$(basename "$r"): ${target} 가 없어 ${t} 의 대상 커밋을 정할 수 없다"
    return
  fi
  if cur="$(git -C "$r" rev-parse --verify -q "refs/tags/${t}^{commit}")"; then
    if [ "$cur" = "$at" ]; then
      ok "${t} 이미 있고 ${target} (${at:0:9}) 를 가리킨다"
      pushable="${pushable}${r}	${t}
"
    else
      bad "${t} 이 ${cur:0:9} 를 가리킨다 — ${target} 는 ${at:0:9} 다. 태그를 움직이지 않는다"
    fi
    return
  fi
  note "${t} 없음 → ${target} (${at:0:9}) 에 만든다 — ${what}"
  plan="${plan}${r}	${t}	${at}	${what}
"
  pushable="${pushable}${r}	${t}
"
}

want_tag "$op" "$op_tag" "$pub" "operator 이미지·차트 발행"

# 묶음 이름은 release-bundle.yaml 의 키에서 읽는다. snap yq 는 /tmp 를 읽지 못해 PyYAML 을 쓴다.
names="$(python3 -c '
import sys, yaml
print("\n".join((yaml.safe_load(open(sys.argv[1])) or {}).get("bundles") or {}))
' "$img/release-bundle.yaml" 2>/dev/null || true)"
count="$(printf '%s\n' "$names" | grep -c . || true)"
if [ "$count" -eq 0 ]; then
  bad "images: release-bundle.yaml 에서 묶음을 읽지 못했다"
elif [ -n "$bundle" ]; then
  if printf '%s\n' "$names" | grep -qxF -- "$bundle"; then
    want_tag "$img" "bundle/${bundle}" "$pub" "묶음 ${bundle} 발행"
  else
    bad "images: release-bundle.yaml 에 ${bundle} 묶음이 없다 (있는 것: $(printf '%s' "$names" | tr '\n' ' '))"
  fi
elif [ "$count" -eq 1 ]; then
  want_tag "$img" "bundle/${names}" "$pub" "묶음 ${names} 발행"
else
  bad "images: 묶음이 ${count}개다 — 인자로 이름을 지정하라 ($(printf '%s' "$names" | tr '\n' ' '))"
fi

if [ "$apply" -eq 1 ]; then
  if [ "$fail" -ne 0 ]; then
    echo "!! 걸린 항목 ${fail}건. 태그를 만들지 않는다." >&2
    exit 1
  fi
  while IFS=$'\t' read -r r t at what; do
    [ -n "$t" ] || continue
    git -C "$r" tag -a "$t" -m "$what" "$at"
    ok "${t} 생성 — $(basename "$r") ${at:0:9} (push 하지 않는다)"
  done <<< "$plan"
elif [ -n "$plan" ]; then
  note "dry-run 이라 만들지 않았다. 만들려면 --apply 를 준다"
fi

# ------------------------------------------------------------ 4단계
step "== 4단계 push 명령 — 사용자가 실행한다 =="
echo "   브랜치를 먼저, 태그를 나중에 올린다. 태그 push 가 발행을 트리거하므로 순서가"
echo "   바뀌면 발행이 태그 커밋 이전의 옛 코드로 돈다."
echo

# 주소는 코드에 적지 않고 remote 에서 읽는다. 공개 발행은 upstream, 없으면 origin 이다.
remote_of() {
  local n
  for n in upstream origin; do
    if git -C "$1" remote get-url "$n" >/dev/null 2>&1; then printf '%s' "$n"; return 0; fi
  done
  return 1
}
for r in "$op" "$img"; do
  if rn="$(remote_of "$r")"; then
    printf '   # %s -> %s\n' "$(basename "$r")" "$(git -C "$r" remote get-url "$rn")"
    printf '   git -C %s push %s %s:main\n' "$r" "$rn" "$pub"
  else
    bad "$(basename "$r"): upstream·origin remote 가 없어 push 명령을 적을 수 없다"
  fi
done
echo
while IFS=$'\t' read -r r t; do
  [ -n "$t" ] || continue
  rn="$(remote_of "$r")" || continue
  printf '   git -C %s push %s %s\n' "$r" "$rn" "$t"
done <<< "$pushable"

step "== 정리 =="
if [ "$fail" -ne 0 ]; then
  echo "   걸린 항목 ${fail}건. 위 [FAIL] 을 해소한 뒤 다시 돌린다."
  exit 1
fi
echo "   선행 조건·정합 검사 통과. push 는 사용자가 한다."
