#!/usr/bin/env bash
# ============================================================
# run.sh: 복구 반복 실패 → 격리(Quarantined) kind 재현 — 자동 Healthy 복귀 없음 확인
# 상세: test/fault/sequence_test.go 의 Sequence D(:141, degraded→recovery→verifier 실패→
#       quarantine)와 fault_test.go F10(:347, 보상 한도 후 ManualRecoveryRequired)이 envtest
#       로 증명한 "반복 실패 → 자동 실행 중단" 결론을 실제 apiserver 위에서 재현한다.
#       신호 자체는 가짜 NodeDeviceReport 하나(driverLoaded=false 고정)뿐이고, 그 뒤로는
#       손을 대지 않는다 — AcceleratorHealth.status 도, AcceleratorOperation 의 결과도
#       전부 실제 컨트롤러가 계산한다("hand-patching the state under test" 회피).
#       driverLoaded=false 는 health 를 Unhealthy/DriverNotLoaded 로 보내고
#       (evaluate.go), ensureRecovery 가 RecoverDevice AcceleratorOperation 을 만든다
#       (remediation.go operationForReason). kind 랩에는 이 벤더의 드라이버 pod 이 실제로
#       하나도 없으므로, RecoverDevice 의 본체(recoverdevice_participant.go Apply)는
#       "지울 pod 이 없다" 는 이유로 **항상** ApplyFailed 를 낸다 — 이것은 이 스크립트가
#       흉내 낸 것이 아니라 실제 장치가 없는 랩 환경이 자연스럽게 만드는 실패다.
#       ApplyFailed 는 곧장 RollingBack 으로(state_machine.go:53-66), Rollback() 은
#       "되돌릴 것이 없다"로 곧장 RolledBack 으로 넘어간다 — 작업 하나가 완결되는 데 실제
#       참가자 로직이 필요 없다. health 는 RolledBack/ManualRecoveryRequired 로 끝난
#       자기 소유 작업 수를 세고(recoveryHistory), 그 수가 RepeatedFailureThreshold 에
#       닿으면 Quarantined 로 올린다 — 그 뒤로는 AllowedTransition(Quarantined, ...) 이
#       Recovering 만 허용해 신호가 좋아져도(이 스크립트가 끝에 driverLoaded=true 로 되돌려
#       확인한다) 자동으로 Healthy 로 안 돌아간다.
#       기본 정책(RepeatedFailureThreshold=3, RecoveryCooldown=10분)을 그대로 쓰면 한
#       사이클에 최대 10분이 걸려 라이브 랩 시간 예산을 넘긴다 — 그래서 이 시나리오만 전용
#       AcceleratorHealthPolicy 로 monitorIntervalSeconds·repeatedFailureThreshold·
#       DriverNotLoaded 의 recoveryCooldownSeconds 를 줄인다(정책 파라미터일 뿐, 상태기계
#       로직은 기본값과 동일하다). 같은 정책에 ndrFreshnessSeconds 도 넉넉히 올린다 — 이
#       시나리오도 실행 시간이 수 분 걸릴 수 있어 이 스크립트가 한 번만 심어 둔 NDR
#       observedAt 이 도중에 stale 이 될 수 있다(01 번 시나리오의 1차 라이브 실행에서 실측
#       확인).
#       상태 확인은 jsonpath 가 아니라 jq 로 한다 — AcceleratorHealthStatus.AllocationAllowed
#       는 `omitempty` 라 컨트롤러가 실제로 false 를 쓰면 status 에 그 키 자체가 빠진다
#       (Go 구조체 마샬링). jsonpath 로 읽으면 "false" 문자열이 아니라 빈 문자열이 된다 —
#       01 번 시나리오 헤더 주석에 상세 설명이 있다.
#       DRY_RUN=1 이면 클러스터 상태를 바꾸는 명령을 하나도 실행하지 않는다.
# 생성일: 2026-09-20 | 수정일: 2026-09-20(01 번 1차 라이브 실행에서 드러난 두 가지 발견 반영)
# ============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
source "$SCRIPT_DIR/../lib.sh"

DRY_RUN="${DRY_RUN:-0}"

VENDOR="${VENDOR:-nvidia}"
ACCEL_LABEL="kcloud.ai/nvidia.present"
QUARANTINE_LABEL="kcloud.ai/health.quarantined"
POLICY_NAME="${POLICY_NAME:-fault03-fast-policy}"
REPEATED_FAILURE_THRESHOLD="${REPEATED_FAILURE_THRESHOLD:-2}"
MONITOR_INTERVAL_SECONDS="${MONITOR_INTERVAL_SECONDS:-5}"
RECOVERY_COOLDOWN_SECONDS="${RECOVERY_COOLDOWN_SECONDS:-3}"

T_QUARANTINED="${T_QUARANTINED:-300}"
T_NO_SELFHEAL="${T_NO_SELFHEAL:-30}"
IV="${IV:-5}"

NODE=""
CLUSTER_NAME="${CLUSTER_NAME:-}"

OUT="${OUT:-/tmp/fault-03-recovery-failure-evidence}"
mkdir -p "$OUT"

NDR_APPLIED=false
POLICY_APPLIED=false
LABEL_APPLIED=false
DEMO_DONE=false
BLOCKED=false
BLOCKED_REASON=""
CLEANED=false
CHECK_JSON=""
FAILS=0
PHASE="init"
QUARANTINED_AT=""
FINAL_RECOVERY_FAILURES=""

preflight() {
  PHASE="preflight"
  log "### preflight (vendor=$VENDOR repeatedFailureThreshold=$REPEATED_FAILURE_THRESHOLD)"
  preflight_kind_node
}

# ndr_apply LOADED — driverLoaded 를 지정값으로 둔다. pcieAddress 는 비워 장치 단위 판정
# (deviceInputsFromNDR)에는 들어가지 않게 한다 — 이 시나리오가 쓰는 것은 노드 단위
# DriverHeartbeat 신호(anyDriverLoaded)뿐이다.
ndr_apply() {
  local loaded="$1"
  cat <<EOF | kcapply >&2
apiVersion: npu.ai/v1alpha1
kind: NodeDeviceReport
metadata:
  name: $NODE
spec:
  nodeName: $NODE
EOF
  NDR_APPLIED=true
  kc patch nodedevicereport "$NODE" --type=merge --subresource=status -p \
    "{\"status\":{\"observedAt\":\"$(utc_now)\",\"devices\":[{\"vendor\":\"$VENDOR\",\"model\":\"fault-injection-fake\",\"count\":1,\"driverLoaded\":$loaded,\"driverVersion\":\"0.0.0-fake\"}]}}" >&2
}

# policy_apply — 이 노드(ACCEL_LABEL 로 선택)에만 적용되는 전용 빠른 정책. 상태기계 로직은
# 그대로이고 임계·주기·쿨다운 세 숫자만 랩 시간 예산에 맞게 줄인다.
policy_apply() {
  cat <<EOF | kcapply >&2
apiVersion: npu.ai/v1alpha1
kind: AcceleratorHealthPolicy
metadata:
  name: $POLICY_NAME
spec:
  selector:
    matchLabels:
      $ACCEL_LABEL: "true"
  checks:
    monitorIntervalSeconds: $MONITOR_INTERVAL_SECONDS
    repeatedFailureThreshold: $REPEATED_FAILURE_THRESHOLD
    ndrFreshnessSeconds: 900
  remediation:
    DriverNotLoaded:
      action: RequestRecovery
      recoveryCooldownSeconds: $RECOVERY_COOLDOWN_SECONDS
EOF
  POLICY_APPLIED=true
}

# label_node_last — 입력을 다 갖춘 뒤 마지막에 노드를 라벨링해 첫 reconcile 을 튼다
# (AcceleratorHealthReconciler 는 corev1.Node 감시로만 깨어난다).
label_node_last() {
  kc label node "$NODE" "$ACCEL_LABEL=true" --overwrite >&2
  LABEL_APPLIED=true
}

# recover_op_lines — 이 노드·이 health 소유의 RecoverDevice 작업을 "phase" 한 줄씩 낸다.
recover_op_lines() {
  kubectl get acceleratoroperation -o json 2>/dev/null \
    | jq -r --arg n "$NODE" '.items[] | select(.spec.nodeName==$n and .spec.type=="RecoverDevice" and .spec.owner.kind=="AcceleratorHealth") | .status.phase' 2>/dev/null || true
}

# health_state_line — state/allocationAllowed/recoveryFailures 를 jq 로 읽는다. jsonpath 가
# 아니라 jq 인 이유: allocationAllowed=false 는 Go 구조체의 `omitempty` 때문에 컨트롤러가
# 실제로 쓴 status 에는 키 자체가 빠져 jsonpath 로는 빈 문자열이 된다("false" 문자열이
# 아니다) — scenario 01 의 1차 라이브 실행에서 처음 드러난 사실이며 여기도 같은 필드를 읽는다.
health_state_line() {
  kubectl get acceleratorhealth "$NODE" -o json 2>/dev/null \
    | jq -r '(.status.state // "") + "/" + ((.status.allocationAllowed // false) | tostring) + "/" + ((.status.recoveryFailures // 0) | tostring)' 2>/dev/null || true
}

demo() {
  PHASE="demo"
  log "### demo (node=$NODE vendor=$VENDOR driverLoaded=false 고정 → 반복 RecoverDevice 실패 기대)"
  local log_f="$OUT/transitions.log"; : >"$log_f"

  ndr_apply false
  policy_apply
  label_node_last

  PHASE="demo:quarantine"
  if poll_and_log "$log_f" "$T_QUARANTINED" "health_state_line" "^Quarantined/false/${REPEATED_FAILURE_THRESHOLD}\$" "$IV"; then
    QUARANTINED_AT="$(utc_now)"
    FINAL_RECOVERY_FAILURES="$REPEATED_FAILURE_THRESHOLD"
    record "repeated_recovery_failure_quarantined" true \
      "AcceleratorHealth.status(컨트롤러 계산)=Quarantined/allocationAllowed=false/recoveryFailures=$REPEATED_FAILURE_THRESHOLD, sequence_test.go:174 와 같은 상태 (${QUARANTINED_AT})"
  else
    record "repeated_recovery_failure_quarantined" false "${T_QUARANTINED}s 내 Quarantined/false/${REPEATED_FAILURE_THRESHOLD} 미도달"
  fi

  PHASE="demo:rolledback_ops"
  if [ "$DRY_RUN" = "1" ]; then
    record "recover_device_ops_rolled_back" true "[DRY_RUN]"
  else
    local phases rolledback_count
    phases="$(recover_op_lines)"
    rolledback_count="$(printf '%s\n' "$phases" | grep -c '^RolledBack$' || true)"
    printf '%s  recover_op_phases=%s\n' "$(utc_now)" "$(printf '%s' "$phases" | tr '\n' ',')" >>"$log_f"
    record "recover_device_ops_rolled_back" "$([ "$rolledback_count" -ge "$REPEATED_FAILURE_THRESHOLD" ] && echo true || echo false)" \
      "RolledBack 상태 RecoverDevice 작업 $rolledback_count 건(기대 >= $REPEATED_FAILURE_THRESHOLD) — 참가자가 '지울 드라이버 pod 없음'으로 매번 ApplyFailed(recoverdevice_participant.go)"
  fi

  # 격리는 복구를 거쳐야만 풀린다(AllowedTransition) — 신호를 고쳐도 자동으로 Healthy 로
  # 돌아오지 않아야 한다. driverLoaded 를 true 로 되돌려 확인한다.
  PHASE="demo:no_selfheal"
  if [ "$DRY_RUN" = "1" ]; then
    record "quarantine_no_automatic_healthy_return" true "[DRY_RUN]"
  else
    ndr_apply true
    sleep "$T_NO_SELFHEAL"
    local still; still="$(health_state_line | cut -d/ -f1-2)"
    printf '%s  recheck(after driverLoaded=true) state/alloc=%s\n' "$(utc_now)" "$still" >>"$log_f"
    record "quarantine_no_automatic_healthy_return" "$([ "$still" = "Quarantined/false" ] && echo true || echo false)" \
      "driverLoaded=true 로 되돌린 뒤 ${T_NO_SELFHEAL}s 재확인: state/alloc=$still (AllowedTransition(Quarantined,Healthy)=false, sequence_test.go:176 과 같은 불변식)"
  fi

  kubeget get events --field-selector involvedObject.kind=AcceleratorHealth \
    -o custom-columns=TIME:.lastTimestamp,REASON:.reason,MSG:.message --sort-by=.lastTimestamp \
    >"$OUT/events.txt" 2>/dev/null || true

  DEMO_DONE=true
}

cleanup() {
  [ "$CLEANED" = true ] && return 0
  CLEANED=true
  PHASE="cleanup"
  log "### cleanup: RecoverDevice 작업·AcceleratorHealth·정책·NDR·라벨 제거"
  if [ "$DRY_RUN" = "1" ]; then log "  [DRY_RUN] 정리 명령 생략"; return 0; fi

  if [ -n "$NODE" ]; then
    for op in $(kubectl get acceleratoroperation -o json 2>/dev/null \
      | jq -r --arg n "$NODE" '.items[] | select(.spec.nodeName==$n and .spec.owner.kind=="AcceleratorHealth") | .metadata.name' 2>/dev/null || true); do
      kc delete acceleratoroperation "$op" --ignore-not-found >&2 || true
    done
    kc delete acceleratorhealth "$NODE" --ignore-not-found >&2 || true
  fi
  if [ "$POLICY_APPLIED" = true ]; then kc delete acceleratorhealthpolicy "$POLICY_NAME" --ignore-not-found >&2 || true; fi
  if [ "$NDR_APPLIED" = true ]; then kc delete nodedevicereport "$NODE" --ignore-not-found >&2 || true; fi
  if [ "$LABEL_APPLIED" = true ] && [ -n "$NODE" ]; then
    kubectl label node "$NODE" "${ACCEL_LABEL}-" "${QUARANTINE_LABEL}-" >&2 2>/dev/null || true
  fi
}

finish() {
  if [ "$CLEANED" != true ]; then cleanup || true; fi
  summary_json "fault-03-recovery-failure" \
    "\"vendor\":\"$VENDOR\",\"phase\":\"$PHASE\",\"repeatedFailureThreshold\":$REPEATED_FAILURE_THRESHOLD,\"quarantinedAt\":\"$QUARANTINED_AT\",\"finalRecoveryFailures\":\"$FINAL_RECOVERY_FAILURES\"" \
    | tee -a "$OUT/summary-$(date +%H%M%S).json"
  # summary_json 의 반환값이 pipefail 로 여기 넘어온다(lib.sh 참고 — 전역 변수로는 파이프의
  # 서브셸 경계 때문에 못 넘긴다).
  exit $?
}

trap finish EXIT
# 신호로 끊겨도 EXIT 트랩(정리)이 돌도록 신호를 종료 코드로 바꾼다(128+신호 번호) —
# 트랩이 없는 SIGTERM·SIGHUP 은 셸을 바로 끝내 EXIT 트랩이 돌지 않는다.
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

preflight || true
if [ "$BLOCKED" != true ]; then
  demo
fi
# EXIT trap(finish) 이 cleanup 실행 + JSON 요약 + 종료코드를 담당한다.
