#!/usr/bin/env bash
# ============================================================
# manifest-coverage.sh: images.yaml 의 kcloud 빌드 이미지가 image-manifest.yaml 에 전부 등재됐는지 검사
# 상세: images.yaml(GHCR 발행 빌드 목록 저장소, ../../../images/kcloud-operator-images)의
#       최상위 이미지 이름 집합과 deploy/integration/image-manifest.yaml 의 kcloud: 절
#       image: 이름 집합을 비교해, 빌드 대상인데 릴리스 매니페스트(air-gap 미러·감사용)에
#       빠진 이미지를 실패로 낸다. 정방향은 한쪽 방향이다 — 매니페스트에만 있고 images.yaml
#       에 없는 이름(이 저장소 밖에서 빌드되는 kcloud-operator, probe-* 등)은 대상이 아니다.
#       images 저장소가 없으면(CI) deploy/helm/values.yaml 의 kcloud 이미지 repository 값
#       ("/" 없는 bare 이름 — global.registry 로 조립되는 값. 벤더 이미지는
#       global.vendorRegistry 조립용 "org/repo" 형태라 "/" 를 포함해 자동으로 빠진다)으로
#       대신 대조한다(fallback). tag·digest 의 정확성은 보지 않는다(이름 집합만).
#
#       역방향: 차트 기본값 렌더가 당기는 self-published(global.registry 접두) 이미지가
#       매니페스트 kcloud: 절에 등재돼 있는가. 정방향은 "images.yaml 에 있는데 매니페스트에
#       없는 이름"만 보여서, 두 목록 모두에 없는 이름(통합 Furiosa device-plugin 이
#       2026-10-02 조사에서 바로 이 자리로 빠졌다)을 구조적으로 못 잡는다. 수집은
#       hack/image-tag-check.sh --collect-only 를 그대로 쓴다(그 수집 로직을 다시 만들지
#       않는다 — 다시 만들면 한쪽만 고쳐지는 자리가 또 생긴다). 대상은 global.registry
#       접두가 붙는 참조만이다(images.yaml 과 같은 집합 — 이 저장소가 빌드해 올리는 이미지).
#       벤더 upstream(nvcr.io·ghcr.io/furiosa-ai·docker.io/furiosaai·docker.io/rebellions·
#       ghcr.io/tenstorrent·registry.k8s.io 등)과 범용 이미지(docker.io/library/nginx,
#       ACPP probe 기본값)는 global.registry 접두가 없어 이 경계 하나로 자동 제외된다 —
#       허용 목록을 따로 두지 않는다(벤더가 늘 때마다 관리할 목록이 생기는 것을 피한다).
#       vendor: 절 자체의 커버리지(예: tenstorrent·rebellions 벤더 이미지가 거기 없는 것)는
#       이 검사의 대상이 아니다 — 정방향도 kcloud: 절만 보고 vendor: 절은 보지 않는다, 같은
#       경계를 반대 방향에도 쓴다. 자리표시자는 수집기가 이미 걸러낸다.
#       기본은 미등재를 경고(WARN)로만 낸다 — 발행 전까지 단 하나(통합 Furiosa)는 이미 아는
#       기존 결함이라, 매 실행마다 이 게이트를 빨강으로 만들면 무관한 작업까지 막는다.
#       CHECK_REVERSE_COVERAGE=1 이면 같은 미등재를 실패(exit 1)로 올린다 —
#       release-prepare.sh 는 건너뛰지 않고 이 모드로 돌린다. 경고 줄 자체는 토글과 무관하게
#       항상 찍혀서, 이 단계가 "한 번도 안 돈다" 상태가 되지 않는다.
#       미등재 신호는 "완전히 안 등재"와 "다른 문자열로 등재"(예: kcloud/ 처럼 경로
#       세그먼트가 중복·불일치하는 이름)를 구분하지 않는다 — 차트가 실제로 당기는 참조
#       문자열과 매니페스트 등재 이름이 글자 그대로 같아야 통과한다(정확 문자열 비교,
#       느슨한 매칭 없음). WARN/FAIL 이 뜨면 두 경우 다 살펴야 한다.
#       수집 대상이 0건이면(수집기 실행 실패, global.registry 가 빈 문자열 등) 등재
#       0·미등재 0 으로 조용히 통과하지 않는다 — 아무것도 확인 못 했다는 뜻이라 실패로
#       낸다(hack/image-tag-check.sh 의 "published -eq 0" 가드와 같은 원칙). 수집기 실행
#       자체의 실패도 잡는다 — 그 결과를 herestring 에 바로 물리면 "$(...)" 안의 실패를
#       set -e 가 보지 못해서(대입·리다이렉션 자리에서 exit code 가 버려진다), 변수로
#       먼저 받아 rc 를 직접 확인한다.
# 생성일: 2026-09-21 | 수정일: 2026-10-04 (역방향 커버리지 추가, 수집 0건 조용한 통과 수정)
# ============================================================
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

images_yaml="$script_dir/../../../images/kcloud-operator-images/images.yaml"
manifest="$repo_root/deploy/integration/image-manifest.yaml"
values="$repo_root/deploy/helm/values.yaml"
chart="$repo_root/deploy/helm"

if [[ ! -f "$manifest" ]]; then
  echo "manifest-coverage: 매니페스트 없음 — ${manifest}" >&2
  exit 1
fi

# image-manifest.yaml 의 kcloud: 절에 등재된 image: 이름 집합.
manifest_names() {
  awk '/^kcloud:/{f=1;next} /^[A-Za-z]/{f=0} f' "$manifest" \
    | sed -nE 's/^[[:space:]]*-[[:space:]]*image:[[:space:]]*//p' \
    | sort -u
}

if [[ -f "$images_yaml" ]]; then
  source_desc="$images_yaml"
  # images.yaml 최상위 이미지 키(2-space indent, 콜론으로 끝나는 줄. dir/build_args 등
  # 하위 키는 4-space 이상이라 걸리지 않는다).
  expected_names="$(grep -E '^  [A-Za-z0-9._-]+:[[:space:]]*$' "$images_yaml" \
    | sed -E 's/^  ([A-Za-z0-9._-]+):.*/\1/' | sort -u)"
else
  source_desc="$values (fallback — images 저장소 없음, 커버리지 좁음)"
  # *repository: 키(repository/devicePluginRepository/kubectlRepository 등) 값 중
  # "/" 없는 bare 이름만 — 벤더 조립용 "org/repo" 값은 자동 제외된다. 주석 줄(#로 시작하는
  # 예시 문구, 예: line 668 "image.repository:image.tag")은 먼저 제거해 오탐을 막는다.
  # (말미 $ 앵커는 안 쓴다 — 이 로케일(ko_KR.UTF-8)에서 멀티바이트 뒤 .*$ 매칭이 깨진다.)
  expected_names="$(sed -E 's/#.*//' "$values" \
    | grep -oE '[A-Za-z]*[Rr]epository:[[:space:]]*"?[A-Za-z0-9._/-]+"?' \
    | sed -E 's/.*[Rr]epository:[[:space:]]*"?([A-Za-z0-9._/-]+)"?/\1/' \
    | grep -v '/' | sort -u)"
fi

manifest_set="$(manifest_names)"
fail=0

# ---- 정방향(기존 그대로): ${source_desc} 각 이름이 매니페스트에 있는가 ----
missing=()
while IFS= read -r name; do
  [[ -z "$name" ]] && continue
  if ! grep -qxF "$name" <<< "$manifest_set"; then
    missing+=("$name")
  fi
done <<< "$expected_names"

if [[ "${#missing[@]}" -gt 0 ]]; then
  echo "FAIL: ${source_desc} 의 이미지가 $(basename "$manifest") kcloud: 절에 없다:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  fail=1
else
  echo "PASS: ${source_desc} 의 kcloud 이미지 전부 $(basename "$manifest") 에 등재"
fi

# ---- 역방향(신규): 차트 기본값 렌더의 self-published 참조가 매니페스트에 있는가 ----
own="$(python3 -c '
import sys, yaml
print((((yaml.safe_load(open(sys.argv[1])) or {}).get("global") or {}).get("registry") or "").strip("/"))
' "$values")"

render_log="$(mktemp)"
trap 'rm -f "$render_log"' EXIT
if ! render="$(helm template kcloud "$chart" 2>"$render_log")"; then
  echo "FAIL: 차트 기본값 렌더가 실패했다 — 역방향 검사를 못 돌렸다: $(head -1 "$render_log")" >&2
  exit 1
fi

# 수집기 결과를 변수로 먼저 받고 rc 를 직접 확인한다 — "$(...)" 를 herestring 에 바로
# 물리면(done <<< "$(cmd)") 그 안의 실패가 대입·리다이렉션 자리라 set -e 가 못 본다.
if ! collected="$(printf '%s\n' "$render" | bash "$repo_root/hack/image-tag-check.sh" --collect-only | cut -f2 | sort -u)"; then
  echo "FAIL: 수집기(hack/image-tag-check.sh --collect-only) 실행이 실패했다 — 역방향 검사를 못 돌렸다" >&2
  exit 1
fi

reverse_missing=()
reverse_covered=0
reverse_total=0
while IFS= read -r ref; do
  [[ -n "$ref" ]] || continue
  case "$ref" in
    "$own"/*) ;;
    *) continue ;;  # global.registry 접두가 없다 — 벤더 upstream·범용 이미지, 범위 밖
  esac
  name="${ref#"$own"/}"
  case "$name" in
    *@sha256:*) name="${name%@sha256:*}" ;;
    *) name="${name%:*}" ;;
  esac
  reverse_total=$((reverse_total + 1))
  if grep -qxF "$name" <<< "$manifest_set"; then
    reverse_covered=$((reverse_covered + 1))
  else
    reverse_missing+=("$name")
  fi
done <<< "$collected"

# 대상이 0건이면 이 검사는 아무것도 확인 못 한 것이다 — global.registry 가 빈 문자열이라
# 모든 참조가 범위 밖으로 걸러진 경우가 그렇다(수집기 자체 실패는 위에서 이미 잡았다).
# 초록으로 넘기지 않는다.
if [[ "$reverse_total" -eq 0 ]]; then
  echo "FAIL: 역방향 검사 대상이 0건이다 — global.registry(\"${own}\") 값을 확인하라" >&2
  fail=1
else
  reverse_missing_u=()
  if [[ "${#reverse_missing[@]}" -gt 0 ]]; then
    while IFS= read -r n; do reverse_missing_u+=("$n"); done \
      <<< "$(printf '%s\n' "${reverse_missing[@]}" | sort -u)"
  fi

  echo ">> 역방향(렌더 → 매니페스트) — self-published 참조 ${reverse_total}개 중 등재 ${reverse_covered}개, 미등재 ${#reverse_missing_u[@]}개"
  if [[ "${#reverse_missing_u[@]}" -gt 0 ]]; then
    label=WARN
    [[ "${CHECK_REVERSE_COVERAGE:-0}" = "1" ]] && label=FAIL
    echo "${label}: 차트가 당기는데 $(basename "$manifest") kcloud: 절에 없는 이미지:" >&2
    printf '  - %s\n' "${reverse_missing_u[@]}" >&2
    if [[ "$label" = "FAIL" ]]; then
      fail=1
    else
      echo "   (CHECK_REVERSE_COVERAGE=1 로 돌리면 이 미등재는 실패로 올라간다 — release-prepare 는 그렇게 돌린다)" >&2
    fi
  fi
fi

[[ "$fail" -eq 0 ]]
