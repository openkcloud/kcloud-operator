#!/usr/bin/env bash
# ============================================================
# comment-only-check.sh: BASE..HEAD 변경이 주석과 문서에만 머물렀는지 검사한다
# 상세: Go 는 주석을 뗀 출력과 표식 주석 순서, 셸·Dockerfile·Makefile·Python 은 주석 줄을 뗀
#       본문, YAML 은 적재한 데이터, 차트 템플릿은 {{/* */}} 와 # 전체 줄을 뗀 소스를 1차로
#       보고 렌더 결과(인증서 필드 제외)를 보조로, CRD 는 description 을 뗀 데이터를 두
#       판본에서 비교한다. 검사 대상은 현재 디렉터리의 git 루트다. --self-test 지원.
# 생성일: 2026-10-11 | 수정일: 2026-10-11
# ============================================================
set -euo pipefail

toolroot="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0
bad() { echo "   [FAIL] $*"; fail=$((fail + 1)); }

build_tool() { (cd "$toolroot" && go build -o "$tmp/stripcomments" ./hack/stripcomments); }
go_code()    { "$tmp/stripcomments" - < "$1"; }
go_markers() { grep -E '^[[:space:]]*//[[:space:]]*(\+[A-Za-z]|go:|nolint)' "$1" || true; }
# 주석 줄 전체와, 따옴표가 없는 꼬리 주석만 뗀다. 따옴표가 있으면 본문으로 보고 그대로 비교한다.
strip_hash() { sed -E -e '1{/^#!/d}' -e '/^[[:space:]]*#/d' -e "s/[[:space:]]+#[^\"'\`]*\$//" "$1"; }
yaml_data() {
  python3 - "$1" "$2" <<'PY'
import sys, yaml
drop = sys.argv[2] == 'nodesc'
def scrub(o):
    if isinstance(o, dict):
        return {k: scrub(v) for k, v in o.items() if not (drop and k == 'description')}
    if isinstance(o, list):
        return [scrub(x) for x in o]
    return o
print(repr([scrub(d) for d in yaml.safe_load_all(open(sys.argv[1]))]))
PY
}
# tpl_code: {{/* … */}} 블록(공백 트림 표식 포함, 여러 줄 비탐욕)과 # 로 시작하는 전체 줄을
# 뗀 템플릿 소스 — 템플릿 비교의 1차 판정(결정적, 토글과 무관하게 모든 템플릿을 본다). 주석
# 제거 뒤 남는 빈 줄·공백만 있는 줄은 전부 버리고 남은 줄만 rstrip 해서 찍는다 — 주석
# 블록의 줄 수 자체가 바뀌면(압축·확장) 그 자리에 남는 빈 줄 수도 달라지는데, 그 차이까지
# "코드가 바뀜" 으로 보면 주석만 압축한 커밋도 걸린다.
tpl_code() {
  python3 - "$1" <<'PY'
import re, sys
s = open(sys.argv[1], encoding='utf-8').read()
s = re.sub(r'\{\{-?\s*/\*.*?\*/\s*-?\}\}', '', s, flags=re.S)
s = re.sub(r'^\s*#.*$', '', s, flags=re.M)
for line in s.split('\n'):
    line = line.rstrip()
    if line.strip():
        print(line)
PY
}
# render_scrub: 렌더 출력(여러 YAML 문서)을 적재해 인증서·CA 필드를 재귀로 지운 뒤 비교용으로 찍는다.
render_scrub() {
  python3 - "$1" <<'PY'
import sys, yaml
DROP = {'ca.crt', 'tls.crt', 'tls.key', 'caBundle'}
def scrub(o):
    if isinstance(o, dict):
        return {k: scrub(v) for k, v in o.items() if k not in DROP}
    if isinstance(o, list):
        return [scrub(x) for x in o]
    return o
print(repr([scrub(d) for d in yaml.safe_load_all(open(sys.argv[1], encoding='utf-8'))]))
PY
}

# render_pass <라벨> [값파일 이름]: $tmp/b·$tmp/h 의 deploy/helm 을 렌더해 인증서 필드를
# 지우고 비교한다 — 템플릿 비교의 보조 판정(렌더 자체 실패는 FAIL). 값파일은 그 파일명을
# 두 트리 각각의 deploy/helm 아래에서 찾는다(경로가 트리마다 다르므로 공유 인자로 못 받는다).
render_pass() {
  local label="$1" valuesfile="${2:-}"
  local -a extra_b=() extra_h=()
  if [ -n "$valuesfile" ]; then
    extra_b=(-f "$tmp/b/deploy/helm/$valuesfile")
    extra_h=(-f "$tmp/h/deploy/helm/$valuesfile")
  fi
  helm template x "$tmp/b/deploy/helm" --kube-version 1.34.0 "${extra_b[@]}" > "$tmp/rb.txt" 2>&1 \
    || { bad "차트 렌더 실패(BASE, $label)"; return; }
  helm template x "$tmp/h/deploy/helm" --kube-version 1.34.0 "${extra_h[@]}" > "$tmp/rh.txt" 2>&1 \
    || { bad "차트 렌더 실패(HEAD, $label)"; return; }
  cmp -s <(render_scrub "$tmp/rb.txt") <(render_scrub "$tmp/rh.txt") || bad "차트 렌더 결과가 바뀜($label)"
}

# check <BASE> [HEAD]: 종료 0=주석·문서만 바뀜, 1=그 밖이 바뀜, 2=git 저장소 아님·리비전을 모름.
check() {
  local base="$1" head="${2:-HEAD}" root templates=0 status path list n=0 rc_b rc_h
  root="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "git 저장소가 아니다" >&2; return 2; }
  cd "$root"
  git rev-parse --verify --quiet "$base^{commit}" >/dev/null || { echo "알 수 없는 리비전: $base" >&2; return 2; }
  git rev-parse --verify --quiet "$head^{commit}" >/dev/null || { echo "알 수 없는 리비전: $head" >&2; return 2; }
  build_tool
  list="$(git diff --name-status "$base" "$head")" \
    || { echo "리비전을 읽을 수 없다: $base..$head" >&2; return 2; }
  while IFS=$'\t' read -r status path; do
    [ -n "$path" ] || continue
    n=$((n + 1))
    case "$status" in
      A|D|R*|C*|T)
        case "$path" in
          *.go|*.sh|*.yaml|*.yml|*.tpl|*.py|*.mk|*.json|*.toml|go.mod|go.sum|*/go.mod|*/go.sum|Makefile|*/Makefile|Dockerfile*|*/Dockerfile*)
            bad "코드 파일 추가·삭제·이름 변경: $status $path" ;;
        esac
        continue ;;
    esac
    git show "$base:$path" > "$tmp/base" 2>/dev/null || { bad "BASE 에 없는 파일: $path"; continue; }
    git show "$head:$path" > "$tmp/head"
    case "$path" in
      *.go)
        rc_b=0; go_code "$tmp/base" > "$tmp/gb.txt" 2>/dev/null || rc_b=$?
        rc_h=0; go_code "$tmp/head" > "$tmp/gh.txt" 2>/dev/null || rc_h=$?
        if [ "$rc_b" -ne 0 ] || [ "$rc_h" -ne 0 ]; then
          bad "파싱 실패: $path"
        else
          cmp -s "$tmp/gb.txt" "$tmp/gh.txt" || bad "Go 코드가 바뀜: $path"
        fi
        cmp -s <(go_markers "$tmp/base") <(go_markers "$tmp/head") || bad "Go 표식 주석이 바뀜: $path" ;;
      deploy/helm/templates/*|*.tpl) templates=1 ;;
      config/crd/bases/*.yaml|internal/crdapply/crd/*.yaml|deploy/helm/crds/*.yaml)
        rc_b=0; yaml_data "$tmp/base" nodesc > "$tmp/yb.txt" 2>/dev/null || rc_b=$?
        rc_h=0; yaml_data "$tmp/head" nodesc > "$tmp/yh.txt" 2>/dev/null || rc_h=$?
        if [ "$rc_b" -ne 0 ] || [ "$rc_h" -ne 0 ]; then
          bad "파싱 실패: $path"
        else
          cmp -s "$tmp/yb.txt" "$tmp/yh.txt" || bad "CRD 가 description 밖에서 바뀜: $path"
        fi ;;
      *.yaml|*.yml)
        rc_b=0; yaml_data "$tmp/base" keep > "$tmp/yb.txt" 2>/dev/null || rc_b=$?
        rc_h=0; yaml_data "$tmp/head" keep > "$tmp/yh.txt" 2>/dev/null || rc_h=$?
        if [ "$rc_b" -ne 0 ] || [ "$rc_h" -ne 0 ]; then
          bad "파싱 실패: $path"
        else
          cmp -s "$tmp/yb.txt" "$tmp/yh.txt" || bad "YAML 데이터가 바뀜: $path"
        fi ;;
      *.sh|*.py|*.mk|Makefile|*/Makefile|Dockerfile*|*/Dockerfile*)
        cmp -s <(strip_hash "$tmp/base") <(strip_hash "$tmp/head") || bad "주석 밖 본문이 바뀜: $path"
        [ "$(head -n1 "$tmp/base")" = "$(head -n1 "$tmp/head")" ] || bad "첫 줄이 바뀜: $path" ;;
      *.md|*.txt|LICENSE*|*/LICENSE*|OWNERS|PROJECT|.gitignore|.dockerignore|*/.gitignore|*/.dockerignore) ;;
      *) bad "검사 규칙이 없는 파일 종류: $path" ;;
    esac
  done <<< "$list"
  if [ "$templates" -eq 1 ]; then
    mkdir -p "$tmp/b" "$tmp/h"
    git archive "$base" deploy/helm | tar -x -C "$tmp/b"
    git archive "$head" deploy/helm | tar -x -C "$tmp/h"
    while IFS= read -r -d '' bf; do
      rel="${bf#"$tmp/b/"}"
      hf="$tmp/h/$rel"
      [ -f "$hf" ] || continue
      cmp -s <(tpl_code "$bf") <(tpl_code "$hf") || bad "템플릿 코드가 바뀜: $rel"
    done < <(find "$tmp/b/deploy/helm" \( -path '*/templates/*' -o -name '*.tpl' \) -type f -print0)
    if command -v helm >/dev/null; then
      render_pass "기본값"
      [ -f "$tmp/b/deploy/helm/values-k8s1.34.yaml" ] && render_pass "values-k8s1.34.yaml" "values-k8s1.34.yaml"
    else
      bad "helm 없음 — 렌더 비교 불가"
    fi
  fi
  if [ "$fail" -ne 0 ]; then echo "주석 밖 변경 $fail 건 ($base..$head)"; return 1; fi
  echo "주석 전용 변경 확인 통과 ($base..$head, 파일 ${n}개)"
}

# self_test: Go·YAML·셸 픽스처(통과 1·위반 4) 에 템플릿 통과·템플릿 위반·알 수 없는 리비전·
# CRD(통과+위반) 를 더해 아홉 경우를 본다.
self_test() {
  local r rc out
  r="$(mktemp -d)"
  trap 'rm -rf "$r"' RETURN
  git -C "$r" init -q
  commit() { git -C "$r" add -A && git -C "$r" -c user.email=t@t -c user.name=t commit -q -m "chore: $1"; }
  run_check() { ( cd "$r" && check HEAD~1 HEAD ); }
  printf 'package p\n\n// +kubebuilder:validation:Enum=a;b\n// 설명 하나\nvar X = 1\n' > "$r/a.go"
  printf 'a: 1 # 주석\nb:\n  - x\n' > "$r/v.yaml"
  printf '#!/usr/bin/env bash\n# 머리말\necho hi # 꼬리\n' > "$r/s.sh"
  commit base
  # 1) 주석만 바꿈 → 통과
  printf 'package p\n\n// +kubebuilder:validation:Enum=a;b\n// 설명 둘\nvar X = 1\n' > "$r/a.go"
  printf 'a: 1 # 다른 주석\nb:\n  - x\n' > "$r/v.yaml"
  printf '#!/usr/bin/env bash\n# 새 머리말\necho hi # 새 꼬리\n' > "$r/s.sh"
  commit c1
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 0 ] || { echo "self-test 1: 종료 $rc '$out'" >&2; return 1; }
  # 2) Go 코드 변경 → 실패
  printf 'package p\n\n// +kubebuilder:validation:Enum=a;b\n// 설명 둘\nvar X = 2\n' > "$r/a.go"; commit c2
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 1 ] && [[ "$out" == *"Go 코드가 바뀜"* ]] || { echo "self-test 2: 종료 $rc '$out'" >&2; return 1; }
  # 3) 표식 주석 제거 → 실패
  printf 'package p\n\n// 설명 둘\nvar X = 2\n' > "$r/a.go"; commit c3
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 1 ] && [[ "$out" == *"표식 주석이 바뀜"* ]] || { echo "self-test 3: 종료 $rc '$out'" >&2; return 1; }
  # 4) YAML 값 변경 → 실패
  printf 'a: 2 # 다른 주석\nb:\n  - x\n' > "$r/v.yaml"; commit c4
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 1 ] && [[ "$out" == *"YAML 데이터가 바뀜"* ]] || { echo "self-test 4: 종료 $rc '$out'" >&2; return 1; }
  # 5) 셸 본문 변경 → 실패
  printf '#!/usr/bin/env bash\n# 새 머리말\necho bye # 새 꼬리\n' > "$r/s.sh"; commit c5
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 1 ] && [[ "$out" == *"주석 밖 본문이 바뀜"* ]] || { echo "self-test 5: 종료 $rc '$out'" >&2; return 1; }
  # 6) 꺼진 템플릿의 {{/* */}} 문구만 바꿈(3줄→1줄로 압축, 줄 수 변화) → 통과
  mkdir -p "$r/deploy/helm/templates"
  printf 'apiVersion: v2\nname: x\nversion: 0.1.0\n' > "$r/deploy/helm/Chart.yaml"
  printf 'x: false\n' > "$r/deploy/helm/values.yaml"
  printf '{{- if .Values.x }}\n{{/*\n설명 하나\n*/}}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: off-cm\n{{- end }}\n' \
    > "$r/deploy/helm/templates/off.yaml"
  commit c6base
  printf '{{- if .Values.x }}\n{{/* 설명 둘 */}}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: off-cm\n{{- end }}\n' \
    > "$r/deploy/helm/templates/off.yaml"
  commit c6
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 0 ] || { echo "self-test 6: 종료 $rc '$out'" >&2; return 1; }
  # 7) 꺼진 템플릿의 코드(ConfigMap 이름) 변경 → 실패(렌더 결과는 둘 다 빈 채로 같다)
  printf '{{- if .Values.x }}\n{{/* 설명 둘 */}}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: off-cm-renamed\n{{- end }}\n' \
    > "$r/deploy/helm/templates/off.yaml"
  commit c7
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 1 ] && [[ "$out" == *"템플릿 코드가 바뀜"* ]] || { echo "self-test 7: 종료 $rc '$out'" >&2; return 1; }
  # 8) 알 수 없는 리비전 → 종료 2
  rc=0; out="$( (cd "$r" && check deadbeef HEAD) 2>&1 )" || rc=$?
  [ "$rc" -eq 2 ] && [[ "$out" == *"알 수 없는 리비전"* ]] || { echo "self-test 8: 종료 $rc '$out'" >&2; return 1; }
  # 9) CRD 꼴 YAML: description 만 바꾸면 통과, enum 값을 바꾸면 실패
  mkdir -p "$r/config/crd/bases"
  printf 'a:\n  description: 하나\n  enum: [c, d]\n' > "$r/config/crd/bases/x.yaml"
  commit c9base
  printf 'a:\n  description: 둘\n  enum: [c, d]\n' > "$r/config/crd/bases/x.yaml"
  commit c9pass
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 0 ] || { echo "self-test 9a: 종료 $rc '$out'" >&2; return 1; }
  printf 'a:\n  description: 둘\n  enum: [c, e]\n' > "$r/config/crd/bases/x.yaml"
  commit c9fail
  rc=0; out="$(run_check)" || rc=$?
  [ "$rc" -eq 1 ] && [[ "$out" == *"CRD 가 description 밖에서 바뀜"* ]] || { echo "self-test 9b: 종료 $rc '$out'" >&2; return 1; }
  echo "self-test ok"
}

case "${1:-}" in
  --self-test) self_test ;;
  "") echo "용법: $0 BASE [HEAD] | --self-test" >&2; exit 2 ;;
  *) check "$@" ;;
esac
