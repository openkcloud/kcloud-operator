#!/usr/bin/env bash
# ============================================================
# manifest-coverage.sh: images.yaml 의 kcloud 빌드 이미지가 image-manifest.yaml 에 전부 등재됐는지 검사
# 상세: images.yaml(GHCR 발행 빌드 목록 저장소, ../../../images/kcloud-operator-images)의
#       최상위 이미지 이름 집합과 deploy/integration/image-manifest.yaml 의 kcloud: 절
#       image: 이름 집합을 비교해, 빌드 대상인데 릴리스 매니페스트(air-gap 미러·감사용)에
#       빠진 이미지를 실패로 낸다. 검사는 한쪽 방향이다 — 매니페스트에만 있고 images.yaml
#       에 없는 이름(이 저장소 밖에서 빌드되는 kcloud-operator, probe-* 등)은 대상이 아니다.
#       images 저장소가 없으면(CI) deploy/helm/values.yaml 의 kcloud 이미지 repository 값
#       ("/" 없는 bare 이름 — global.registry 로 조립되는 값. 벤더 이미지는
#       global.vendorRegistry 조립용 "org/repo" 형태라 "/" 를 포함해 자동으로 빠진다)으로
#       대신 대조한다(fallback). tag·digest 의 정확성은 보지 않는다(이름 집합만).
# 생성일: 2026-09-21
# ============================================================
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

images_yaml="$script_dir/../../../images/kcloud-operator-images/images.yaml"
manifest="$repo_root/deploy/integration/image-manifest.yaml"
values="$repo_root/deploy/helm/values.yaml"

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
  exit 1
fi

echo "PASS: ${source_desc} 의 kcloud 이미지 전부 $(basename "$manifest") 에 등재"
