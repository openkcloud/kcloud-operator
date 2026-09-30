#!/usr/bin/env bash
# ============================================================
# adopt-helm-resources.sh: 기존 helm hook 객체를 릴리스 자원으로 인계한다(일회성)
# 상세: hook 으로 만들어진 객체에는 helm 의 소유 표시가 없어, 템플릿에서 hook 주석을 지우고
#       업그레이드하면 helm 이 "already exists" 로 거절한다. helm 3 이후는 아래 라벨 하나와
#       주석 두 개가 붙어 있으면 기존 객체를 인계한다. 이 스크립트는 그 셋을 붙인다.
#       기본은 미리보기이며 --apply 를 줘야 실제로 붙인다. 삭제는 하지 않는다.
# 용법: adopt-helm-resources.sh [--apply] [--release <이름>] [--namespace <이름>]
# 생성일: 2026-09-24 | 수정일: 2026-09-24
# ============================================================
set -euo pipefail
APPLY=0
RELEASE="kcloud-operator"
NAMESPACE="kcloud"
KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-kubernetes-admin@cluster.local}"
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --release) RELEASE="${2:?}"; shift 2 ;;
    --namespace) NAMESPACE="${2:?}"; shift 2 ;;
    *) echo "알 수 없는 인자: $1" >&2; exit 2 ;;
  esac
done
kc() { kubectl --context "$KUBECTL_CONTEXT" "$@"; }

names="$(kc get driverinstallpolicies.npu.ai -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"
[ -n "$names" ] || { echo "인계할 DriverInstallPolicy 가 없다"; exit 0; }

while IFS= read -r n; do
  [ -n "$n" ] || continue
  cur="$(kc get driverinstallpolicy "$n" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}')"
  if [ "$cur" = "$RELEASE" ]; then
    echo "이미 인계됨: $n"
    continue
  fi
  echo "인계 대상: $n"
  if [ "$APPLY" = 1 ]; then
    kc label driverinstallpolicy "$n" app.kubernetes.io/managed-by=Helm --overwrite
    kc annotate driverinstallpolicy "$n" \
      meta.helm.sh/release-name="$RELEASE" \
      meta.helm.sh/release-namespace="$NAMESPACE" --overwrite
  fi
done <<< "$names"

if [ "$APPLY" = 1 ]; then
  echo "인계 완료. 이제 helm upgrade 를 --reset-then-reuse-values 로 실행한다"
else
  echo "미리보기였다. 실제로 붙이려면 --apply 를 준다"
fi
