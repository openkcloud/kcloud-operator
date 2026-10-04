#!/usr/bin/env bash
# ============================================================
# helm-values-option-check.sh: helm upgrade 의 --reuse-values 단독 사용을 금지한다
# 상세: Helm v4.0.1 에서 --reuse-values 는 직전 릴리스가 계산해 둔 값을 통째로 재사용해,
#       사용자 값으로 덮지 않은 차트 기본값 변경이 반영되지 않는다. 그래서 차트만 고치고
#       반영한 변경이 조용히 빠진다. 대신 --reset-then-reuse-values 를 쓴다.
#       --reset-then-reuse-values 는 --reuse-values 를 부분 문자열로 포함하지 않는다
#       ("then-" 뒤가 대시 하나짜리 "-reuse-values"가 되기 때문) — 그래서 별도 제외 없이도
#       오탐이 없다. 검사 대상은 *.sh 파일뿐이고, 같은 셸 문장(줄 끝 \ 로 이어진 줄 포함)에
#       helm 과 --reuse-values 가 함께 있을 때만 위반으로 센다 — 금지를 설명하는 문서
#       문장까지 걸리지 않게 하기 위해서다. 물리적 줄만 보면 `helm upgrade ... \` 다음
#       줄에 `--reuse-values` 가 오는 실제 흔한 형태(이 저장소의 실제 결함이 이 모양이었다)를
#       놓치므로, \ 로 이어진 물리적 줄들을 하나의 셸 문장으로 합친 뒤에 판정한다.
# 상세(검사 2): 문서가 권하는 값 키(`--set`/`--set-string` 과 Configuration 표의 키
#       칸)가 실재하는지 본다 — 문서가 없는 키를 적으면 helm 이 조용히 버리거나
#       values.schema.json 이 helm upgrade 자체를 거절하는데, 운영자는 둘 다 모른 채
#       바꿨다고 믿는다. 대상은 README.md·deploy/helm/README.md·deploy/helm/INSTALL.md·
#       deploy/helm/UPGRADE.md·deploy/helm/templates/NOTES.txt·deploy/helm/install.sh —
#       없는 파일은 건너뛴다.
#
#       "실재"의 판정 기준은 values.yaml 데이터(기본값이 선언된 키) **또는**
#       values.schema.json(기본값은 없지만 스키마가 허용하는 키, 예: toolkit.image)
#       중 하나라도 있으면 허용이다(합집합) — 한쪽만 보면 두 방향으로 틀린다. values.yaml
#       만 보면 "기본값은 없지만 스키마가 허용하는" 키를 오탐으로 잡고(예:
#       driverInstallPolicies.nvidia.toolkit.image), 스키마만 보면 스키마 쪽이 빠뜨린
#       키를 못 잡는다. 둘 다 python3 PyYAML/json 으로 읽는다(snap yq 는 /tmp 를 못
#       읽는다). 둘 중 하나가 없으면(예: 이 스크립트 자신의 시험 픽스처) 검사 2 를
#       건너뛴다 — values.yaml 이 없으면 비교할 기준이 없다.
#
#       스키마 쪽은 `$ref`/`allOf` 를 따라간다. `allOf` 가지 중 하나가 어떤 키를
#       `false` 로 닫아 두면(벤더별 제외, 예: dipWithoutToolkit) 다른 가지가 그 키를
#       허용해도 교집합 의미로 거절 — 그 외에는 가지 중 하나라도 허용하면 허용.
#       properties 가 없는 리프(또는 "열어 둔" 블록 — affinity, resources.limits/
#       requests 같은 임의 키 블록)에서 더 깊은 세그먼트가 남으면 통과시킨다(오탐보다
#       누락이 낫다는 원칙, 못 알아듣는 형태는 보수적으로 검사 제외).
#
#       키 형태 둘을 지원한다. "a.b[0].c" 처럼 배열 인덱스가 섞이면 인덱스가 나온
#       세그먼트에서 자르고 그 앞까지만 본다(리스트 원소 하나의 스키마까지는 모른다 —
#       리스트 키 자체의 존재만 확인). "<vendor>" 처럼 꺾쇠로 감싼 세그먼트와
#       "kubectl*" 처럼 끝에 별표가 붙은 세그먼트는 그 위치의 자식 키 중 하나(또는
#       그 접두사로 시작하는 키 중 하나)라도 나머지 경로를 만족하면 허용으로 본다
#       (Configuration 표가 벤더 공통 키·접두사 묶음 키를 이렇게 적는다).
#
#       값("=" 뒤, `<자리표시자>` 포함)은 --set/--set-string 양쪽 모두 애초에 보지
#       않는다 — 키만 본다. `--set "key=${VAR}"` 처럼 인용부호로 감싸인 경우를 위해
#       추출한 키의 앞뒤 인용부호를 벗긴다.
#
#       Configuration 표는 헤더 셀(구분선 `|---|...` 바로 위 행)의 첫 칸이
#       "파라미터"/"값"/"values 키"/"values key" 일 때만 그 표의 데이터 행을 본다 —
#       이 저장소에는 키가 아닌 것(CRD Kind 이름, Reconciler 이름, 리네임 전/후 이름)을
#       첫 칸에 코드체로 적는 표도 있어서, 헤더로 거르지 않으면 그 표들이 전부 오탐이
#       된다. 데이터 행의 첫 칸에서 코드체(`` ` ``)로 감싼 조각을 전부 뽑는다 — 첫
#       조각은 그대로 키 경로, 그 뒤 조각("`b`"/"`.b`" 처럼 "/" 로 이어 붙는 같은 칸의
#       나머지)은 그 자체로 독립 경로를 먼저 시도하고(예: "furiosa.devicePluginImage"
#       처럼 뒤 조각도 완결된 경로인 경우), 아니면 앞 조각의 마지막 세그먼트 k=1개부터
#       차례로 더 떼어내고 그 자리에 이 조각을 끼워 넣어 본다(예: "driverInstallPolicies.
#       furiosa.driver" / ".rngd.driver" 는 k=2 에서 "driverInstallPolicies.rngd.driver"
#       로 성립). 이 저장소 문서에는 이 규칙으로 못 푸는 형태가 없어 예외 목록은 비어
#       있다 — 오탐이 나오면 그때 이 블록 안에 사유와 함께 추가한다.
# 용법: helm-values-option-check.sh [검사할 디렉터리(기본: 저장소 루트)]
# 생성일: 2026-09-24 | 수정일: 2026-10-04 (검사 2: Configuration 표 키·--set-string·
#         values.schema.json 대조 추가, 대상 문서에 UPGRADE.md·install.sh 추가)
# ============================================================
set -euo pipefail
root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

hits="$(find "$root" -type f -name '*.sh' \
    -not -path '*/.git/*' -not -path '*/.superpowers/*' \
    -not -name 'helm-values-option-check*' -print0 \
  | xargs -0 -r awk '
      function flush() {
        if (buf != "" && buf ~ /helm/ && buf ~ /--reuse-values/) {
          print FILENAME ":" startline ":" buf
        }
        buf = ""
      }
      FNR == 1 { flush() }
      {
        if (buf == "") startline = FNR
        line = $0
        if (sub(/\\[ \t]*$/, " ", line)) { buf = buf line }
        else { buf = buf line; flush() }
      }
      END { flush() }
    ' || true)"

if [ -n "$hits" ]; then
  echo "helm-values-option-check: --reuse-values 단독 사용 발견 — --reset-then-reuse-values 로 바꾼다" >&2
  printf '%s\n' "$hits" >&2
  exit 1
fi
echo "helm-values-option-check: --reuse-values 단독 사용 0건"

# 검사 2: 문서가 권하는 값 키(--set/--set-string, Configuration 표)가 실재하는지.
python3 - "$root" <<'PY'
import json
import os
import re
import sys

import yaml

root = sys.argv[1]
values_path = os.path.join(root, "deploy/helm/values.yaml")
if not os.path.isfile(values_path):
    print("helm-values-option-check: values.yaml 없음 — 검사 2 건너뜀")
    sys.exit(0)

with open(values_path, encoding="utf-8") as f:
    data_tree = yaml.safe_load(f) or {}

schema_path = os.path.join(root, "deploy/helm/values.schema.json")
schema_tree = {}
defs = {}
if os.path.isfile(schema_path):
    with open(schema_path, encoding="utf-8") as f:
        schema_tree = json.load(f)
    defs = schema_tree.get("$defs") or {}


def path_exists_data(node, segs):
    """values.yaml 데이터 트리에서 경로가 실재하는지(기본값이 선언된 키)."""
    if not segs:
        return True
    if not isinstance(node, dict):
        return False
    seg, rest = segs[0], segs[1:]
    if seg.startswith("<") and seg.endswith(">"):
        return any(path_exists_data(v, rest) for v in node.values())
    if seg.endswith("*"):
        prefix = seg[:-1]
        return any(path_exists_data(v, rest) for k, v in node.items() if k.startswith(prefix))
    if seg not in node:
        return False
    return path_exists_data(node[seg], rest)


def resolve_ref(node):
    if isinstance(node, dict) and "$ref" in node:
        return defs.get(node["$ref"].split("/")[-1], {})
    return node


def branches_of(node):
    """$ref 를 풀고, allOf 가 있으면 그 가지들을 재귀적으로 펼친 리스트로 돌려준다."""
    node = resolve_ref(node)
    if isinstance(node, dict) and "allOf" in node:
        out = []
        for b in node["allOf"]:
            out.extend(branches_of(b))
        return out
    return [node]


def seg_matches(seg, k):
    if seg.startswith("<") and seg.endswith(">"):
        return True
    if seg.endswith("*"):
        return k.startswith(seg[:-1])
    return k == seg


def merged_child_schemas(branches, seg):
    """allOf 가지 전체에서 seg 하나에 대한 서브스키마를 모은다. 한 가지라도 seg 를
    false 로 닫아 두면(벤더별 제외 정의) None(=금지)을 돌려준다 — 교집합 의미라
    다른 가지가 실스키마를 들고 있어도 이긴다. 아무 가지도 seg 를 적어두지 않았으면
    빈 리스트(=판단 보류, 호출부가 열림/닫힘을 따로 본다)."""
    found = []
    for b in branches:
        if not isinstance(b, dict):
            continue
        props = b.get("properties") or {}
        if seg in props:
            if props[seg] is False:
                return None
            found.append(props[seg])
    return found


def path_exists_schema(node, segs):
    """values.schema.json 에서 경로가 허용되는지(기본값 없이도 스키마가 허용하는 키 포함).

    allOf 가지 중 하나가 어떤 키를 false 로 닫아 두면(벤더별 제외) 다른 가지가 그
    키를 허용해도 거절해야 한다 — 이건 교집합 의미다. 그런데 그 키 밑으로 더 내려갈
    때도 같은 교집합이 다시 적용돼야 한다(예: tenstorrent 전용 제외 정의가
    "driver.trackOnly" 만 따로 닫아 두고 "driver" 자체는 열어 둔다 — 두 가지 모두의
    driver 서브스키마를 "합쳐서" 다음 세그먼트를 봐야 trackOnly 거절이 살아난다).
    그래서 매 깊이마다 가지들을 따로 재귀하지 않고, 이 키를 적어 둔 모든 가지의
    서브스키마를 합쳐 새 allOf 노드로 묶은 뒤 그걸로 재귀한다.
    """
    if not segs:
        return True
    branches = branches_of(node)
    seg, rest = segs[0], segs[1:]
    wildcard = (seg.startswith("<") and seg.endswith(">")) or seg.endswith("*")

    if wildcard:
        names = set()
        for b in branches:
            if isinstance(b, dict):
                names |= set((b.get("properties") or {}).keys())
        for name in names:
            if not seg_matches(seg, name):
                continue
            sub = merged_child_schemas(branches, name)
            if sub:
                if path_exists_schema({"allOf": sub}, rest):
                    return True
        return False

    sub = merged_child_schemas(branches, seg)
    if sub is None:
        return False  # 한 가지가 명시적으로 닫음
    if sub:
        return path_exists_schema({"allOf": sub}, rest)
    # 어느 가지도 이 키를 적어두지 않았다 — properties 자체가 없는 리프, 또는 모든 가지가
    # additionalProperties 를 열어 둔 블록이면 통과(오탐보다 누락이 낫다).
    has_props = any(isinstance(b, dict) and isinstance(b.get("properties"), dict) for b in branches)
    if not has_props:
        return True
    closed = any(isinstance(b, dict) and b.get("additionalProperties", True) is False for b in branches)
    return not closed


def declared(raw_key):
    segs = raw_key.strip("\"'").split(".")
    return path_exists_data(data_tree, segs) or path_exists_schema(schema_tree, segs)


TARGET_DOCS = [
    "README.md",
    "deploy/helm/README.md",
    "deploy/helm/INSTALL.md",
    "deploy/helm/UPGRADE.md",
    "deploy/helm/templates/NOTES.txt",
    "deploy/helm/install.sh",
]

bad = []
checked = {"set": 0, "table": 0}

set_re = re.compile(r"--set(?:-string)?\s+([^\s=]+)=")
ALLOWED_TABLE_HEADERS = {"파라미터", "값", "values 키", "values key"}


def is_sep_row(line):
    inner = line.replace("|", "")
    return bool(inner.strip()) and all(c in "-: " for c in inner)


def check_table_cell(doc, lineno, cell1):
    frags = re.findall(r"`([^`]+)`", cell1)
    if not frags:
        return
    checked["table"] += len(frags)
    first_segs = frags[0].strip("\"'").split(".")
    if not declared(frags[0]):
        bad.append("%s:%d: 표 키 %s (없음)" % (doc, lineno, frags[0]))
    for cont in frags[1:]:
        frag = cont[1:] if cont.startswith(".") else cont
        if declared(frag):
            continue
        frag_segs = frag.strip("\"'").split(".")
        ok = False
        for k in range(1, len(first_segs)):
            if path_exists_data(data_tree, first_segs[:-k] + frag_segs) or path_exists_schema(
                schema_tree, first_segs[:-k] + frag_segs
            ):
                ok = True
                break
        if not ok:
            bad.append("%s:%d: 표 키 %s (이어붙여도 없음)" % (doc, lineno, cont))


for doc in TARGET_DOCS:
    path = os.path.join(root, doc)
    if not os.path.isfile(path):
        continue
    in_key_table = False
    prev_line = None
    with open(path, encoding="utf-8") as f:
        for lineno, raw_line in enumerate(f, 1):
            line = raw_line.rstrip("\n")

            for m in set_re.finditer(line):
                checked["set"] += 1
                if not declared(m.group(1)):
                    bad.append("%s:%d: --set %s=... (없음)" % (doc, lineno, m.group(1)))

            is_row = line.lstrip().startswith("|") and line.rstrip().endswith("|")
            if is_row:
                if is_sep_row(line):
                    hm = re.match(r"^\s*\|\s*([^|]*?)\s*\|", prev_line) if prev_line else None
                    header1 = hm.group(1).strip() if hm else ""
                    in_key_table = header1 in ALLOWED_TABLE_HEADERS
                elif in_key_table:
                    cm = re.match(r"^\s*\|([^|]*)\|", line)
                    check_table_cell(doc, lineno, cm.group(1) if cm else "")
                prev_line = line
            else:
                in_key_table = False
                prev_line = None

if bad:
    print("helm-values-option-check: 문서가 권하는 값 키 중 없는 것 발견", file=sys.stderr)
    for b in bad:
        print("  " + b, file=sys.stderr)
    sys.exit(1)
print(
    "helm-values-option-check: 문서 값 키 전부 확인됨 (--set %d건, 표 키 %d건)"
    % (checked["set"], checked["table"])
)
PY
