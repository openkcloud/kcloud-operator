// ============================================================
// acpp_sharing.go: ACPP 공유(sharing) 경로 — apply → DP 재시작 → verify → rollback
// 상세: 파티션 경로(runNvidiaTarget)와 같은 규율(저널 선영속 → mutation, 실패시 원복)을 따르되
//
//	하드웨어는 건드리지 않는다(device-plugin 설정/배선만 변경).
//
// 생성일: 2026-07-29 | 수정일: 2026-07-31
// ============================================================
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition"
	"kcloud-operator/internal/partition/nvidia"
)

// runSharing 은 base(공유 전 기대 allocatable)에 spec.sharing 을 적용한다.
// base 는 파티션 경로가 계산한 값이다: exclusive=장치 수, MIG=프로파일별 GI 수 + 남은 full-GPU.
// time-sliced replica 는 독립 장치가 아니므로 기대 allocatable 은 backend 가 계산하고(가정 금지),
// 검증은 verifier 가 폴링한다(DP 재시작 직후 단발 확인은 조기 false 를 낸다).
//
// 적용 범위 = device-plugin DaemonSet 전체(클러스터 전역)다. ConfigMap 도 DS 도 단일 전역 객체이므로
// nodeSelector 가 노드 하나를 가리켜도 모든 nvidia 노드의 광고가 바뀐다(검증은 대상 노드만 관측).
// 노드별 config(--config-file-src + 노드 라벨)는 후속 과제다.
//
// MIG 저널(MigPhase)은 건드리지 않는다 — 공유는 하드웨어를 바꾸지 않으므로 MIG phase 는 계속 Ready 가
// 사실이고, 여기서 중간 phase 를 쓰면 그 창에서 크래시한 뒤 파티션 상태머신이 소유를 잃는다.
func (r *AcceleratorPartitionPolicyReconciler) runSharing(
	acpp *npuv1alpha1.AcceleratorPartitionPolicy,
	backend partition.Backend,
	t partition.Target,
	ts npuv1alpha1.TargetStatus,
	base map[string]int32,
) (npuv1alpha1.TargetStatus, error) {
	log := logf.FromContext(t.Ctx)
	layout := partition.SharingLayoutFrom(acpp.Spec)
	if layout.Mode == npuv1alpha1.SharingModeExclusive {
		return r.disableSharing(acpp, backend, t, ts)
	}

	sb, rejected, ok, err := r.precheckSharing(t.Ctx, acpp, backend, ts)
	if err != nil {
		return ts, err
	}
	if !ok {
		return r.rejectSharing(acpp, backend, t, rejected)
	}

	// 같은 generation 의 rollback 은 terminal 이다(D-9, MIG GI apply 의 MigPhaseRolledBack 게이트와
	// 대칭). verify 미수렴은 에러가 아니라 결과로 돌아오므로(liveVerifier: 180s 초과 → converged=false,
	// err=nil) rollback 종점의 runErr 이 nil 이라 rate-limited 재시도도 걸리지 않는다. 그 상태로
	// 재진입하면 매 pass 마다 ConfigMap apply → device-plugin 재시작 → verify → rollback 을 무한
	// 반복한다. 입력(spec)이 그대로면 결과도 그대로이므로 여기서 끝내고 spec 변경을 기다린다.
	if rec := getApplyRecord(acpp, t.NodeName); rec.SharingRolledBackGeneration != 0 && rec.SharingRolledBackGeneration == acpp.Generation {
		// D-13 인접 누수: 여기는 terminal 이라 정책이 살아 있는 한 다른 정리 경로가 오지 않는다.
		// rollback 은 ConfigMap·배선을 이미 걷었으므로 daemon 도 남을 이유가 없다(참조 카운트가
		// 다른 정책의 사용을 보호한다). 이 정리에 실패하면 requeue 로 다시 시도한다.
		if err := r.ensureMpsControlDaemon(t.Ctx, npuv1alpha1.SharingModeExclusive, acpp.Name, t.NodeName); err != nil {
			return ts, err
		}
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, "SharingRolledBack",
			"sharing rolled back for this generation after verify failure; update spec to retry",
			acpp.Generation)
		return ts, nil
	}

	expected := sb.ExpectedSharedAllocatable(base, layout)
	rec := getApplyRecord(acpp, t.NodeName)
	prevMode, prevReplicas := rec.SharingMode, rec.SharingReplicas

	// 이미 우리가 같은 요청을 적용해 뒀고 수렴도 유지되면 재적용하지 않는다 — 매 reconcile 마다
	// ConfigMap 을 쓰고 DP 를 죽이면 노드가 계속 흔들린다. 미수렴이면 아래 재적용으로 자가치유.
	if rec.SharingMode == layout.Mode && rec.SharingReplicas == layout.Replicas {
		// 저널이 이미 이 요청을 반영하고 있어 여기서 daemon 을 불러도 참조 카운트 창이 없다 —
		// 그래도 부르는 이유는 daemon 이 외부에서 지워졌을 때(예: 무관한 정책의 삭제 경합) 되살릴
		// 유일한 안전망이 이 호출이기 때문이다. 광고 개수는 daemon 생존과 무관해 아래 verify 로는
		// daemon 부재를 못 잡는다.
		if err := r.ensureMpsControlDaemon(t.Ctx, layout.Mode, acpp.Name, t.NodeName); err != nil {
			return sharingDaemonFailure(ts, acpp, err)
		}
		// 짧은 타임아웃 — 이 분기는 "지금 이미 수렴해 있는가" 를 묻는 것이라 재적용 검증과 같은
		// verifyTimeout(180s)을 기다릴 이유가 없다. 컨트롤러 동시성이 1이라 여기서 3분을 잡으면
		// 다른 ACPP 의 reconcile 은 물론 삭제(finalizer) 처리까지 큐에서 멈춘다.
		pt := t
		ctx, cancel := context.WithTimeout(t.Ctx, sharingProbeTimeout)
		pt.Ctx = ctx
		vr, verr := r.verifySharedAllocatable(pt, expected)
		cancel()
		if verifySucceeded(vr, verr) {
			return sharingReady(ts, acpp, expected, layout.Mode, layout.Replicas), nil
		}
		log.Info("sharing: applied journal present but allocatable not converged; re-applying", "node", t.NodeName)
	}

	// persist-before-mutate — 저널을 먼저 영속해야 크래시 후에도 원복 대상을 안다. daemon 을
	// 만들기 전에 쓴다(review 재검토 4차) — 거꾸로 두면 daemon 을 만든 직후·저널이 아직 이전 값인
	// 순간에 무관한 정책의 reconcile 이 참조 카운트(anyPolicyStillUsesMPS)를 보고 "아무도 안 쓴다"고
	// 오판해 방금 만든 daemon 을 지울 수 있다. 저널을 먼저 쓰면 그 창에서도 참조 카운트가 이 요청을
	// 이미 본다 — 크래시가 나도 daemon 이 없을 뿐 다음 reconcile 이 멱등하게 되살린다(반대는 못 그런다).
	rec.NodeName = t.NodeName
	rec.SharingMode, rec.SharingReplicas = layout.Mode, layout.Replicas
	if err := r.patchApplyRecord(t.Ctx, acpp, rec); err != nil {
		return ts, err
	}

	// sharing config 를 daemon 보다 먼저 쓴다(D-12) — mps-control-daemon 은 이 ConfigMap 을
	// 마운트해 기동하므로, 없으면 pod 이 ContainerCreating 에 갇혀 아래 준비 게이트를 영원히 못
	// 넘는다. 배선(ApplySharing 의 DS 단계)은 반대로 daemon 이 뜬 뒤여야 한다 — DP 가 먼저 뜨면
	// 파이프에 붙지 못한다. 그래서 apply 가 config/배선 두 단계로 갈라져 있다.
	ts.Phase = npuv1alpha1.ACPPPhaseApplying
	var state *partition.SharingRollbackState
	applyFailed := func(err error) (npuv1alpha1.TargetStatus, error) {
		if state != nil {
			_ = sb.RollbackSharing(t, *state) // 부분 적용(ConfigMap 만) 원복.
		}
		r.restoreSharingRecord(t, acpp, prevMode, prevReplicas)
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
			npuv1alpha1.ReasonSharingApplyFailed, err.Error(), acpp.Generation)
		return ts, err
	}
	state, cerr := sb.ApplySharingConfig(t, layout, base)
	if cerr != nil {
		return applyFailed(cerr)
	}

	// control daemon 이 먼저 떠야 DP 가 파이프에 붙는다 — 배선(ApplySharing)보다 앞에 둔다.
	// daemon 이 아직 준비 안 됐으면 여기서 끝낸다(배선도 검증도 하지 않는다).
	if err := r.ensureMpsControlDaemon(t.Ctx, layout.Mode, acpp.Name, t.NodeName); err != nil {
		// 미준비(transient)면 방금 쓴 ConfigMap 을 **남긴다** — daemon 이 그것을 마운트해야 Ready 가
		// 되므로, 지우면 다음 pass 가 다시 쓰고 다시 지우는 사이 pod 은 영원히 ContainerCreating 이다.
		// 그 외(정말 실패)는 daemon 이 안 뜬 채 CM 만 남지 않게 원복한다.
		if errors.Is(err, ErrMpsDaemonNotReady) {
			return sharingDaemonFailure(ts, acpp, err)
		}
		return applyFailed(err)
	}

	if _, err := sb.ApplySharing(t, layout, base); err != nil {
		return applyFailed(err)
	}

	ts.Phase = npuv1alpha1.ACPPPhaseRestartingDevicePlugin
	if err := r.restartNvidiaDevicePlugin(t.Ctx, t.NodeName); err != nil {
		// 재시작 실패는 여기서 끝내지 않는다 — 아래 verify 가 미수렴을 잡아 원복한다.
		log.Error(err, "sharing: device-plugin restart failed")
	}

	ts.Phase = npuv1alpha1.ACPPPhaseVerifyingAllocatable
	vr, verr := r.verifySharedAllocatable(t, expected)
	if !verifySucceeded(vr, verr) {
		ts.Phase = npuv1alpha1.ACPPPhaseRollingBack
		if rbErr := sb.RollbackSharing(t, *state); rbErr != nil {
			ts.Phase = npuv1alpha1.ACPPPhaseRollbackFailed
			setCond(&ts, npuv1alpha1.ACPPCondRolledBack, metav1.ConditionFalse,
				npuv1alpha1.ReasonSharingApplyFailed, rbErr.Error(), acpp.Generation)
			return ts, rbErr
		}
		if err := r.restartNvidiaDevicePlugin(t.Ctx, t.NodeName); err != nil {
			log.Error(err, "sharing rollback: device-plugin restart failed")
		}
		r.restoreSharingRecord(t, acpp, prevMode, prevReplicas)
		// D-9: 저널을 원복 값으로 되돌린 다음, 이 generation 이 이미 rollback 을 겪었다고 마킹한다
		// (restoreSharingRecord 가 방금 되돌린 rec 를 다시 읽어야 하므로 여기서 재조회한다). patch
		// 실패를 error 로 전파하지 않는다 — 이 마킹이 실패해도 다음 reconcile 이 같은 verify-fail 을
		// 다시 겪으며 다시 마킹을 시도하므로 자가치유된다. 반대로 error 로 전파하면 그 자체가 무한
		// requeue 사유가 되어, D-9 를 막으려다 다른 형태의 반복을 만든다.
		if mrec := getApplyRecord(acpp, t.NodeName); mrec.SharingRolledBackGeneration != acpp.Generation {
			mrec.NodeName = t.NodeName
			mrec.SharingRolledBackGeneration = acpp.Generation
			if perr := r.patchApplyRecord(t.Ctx, acpp, mrec); perr != nil {
				log.Error(perr, "sharing: failed to journal rolled-back generation (D-9 gate)")
			}
		}
		// 배선을 걷었으면 daemon 도 회수한다(D-13 인접 누수). 여기서 안 걷으면 Restored 종점은
		// 10분 requeue 라 그 동안 daemon 이 kube-system 에 남는다(라이브 6분+ 관측). 실패해도 종점을
		// 바꾸지 않는다 — 다음 pass 의 D-9 게이트가 같은 정리를 다시 시도한다.
		if derr := r.ensureMpsControlDaemon(t.Ctx, npuv1alpha1.SharingModeExclusive, acpp.Name, t.NodeName); derr != nil {
			log.Error(derr, "sharing rollback: mps control daemon cleanup failed")
		}
		ts.Phase = npuv1alpha1.ACPPPhaseRestored
		// 파티션 경로가 찍은 Verified=True 를 그대로 두면 Restored 인데 검증됨이라는 모순이 남는다.
		setCond(&ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionFalse,
			npuv1alpha1.ReasonSharingNotConverged, "shared allocatable did not converge", acpp.Generation)
		setCond(&ts, npuv1alpha1.ACPPCondRolledBack, metav1.ConditionTrue,
			npuv1alpha1.ReasonSharingNotConverged, "shared allocatable did not converge; sharing restored", acpp.Generation)
		return ts, verr
	}

	return sharingReady(ts, acpp, expected, layout.Mode, layout.Replicas), nil
}

// runSharingOnly 는 layout 없는(순수 공유) 정책의 전체 경로다(라이브 결함 D-3, Task 5 §5.1).
// 파티션 apply 경로(runNvidiaTarget: owner-lock·cordon·MIG mode enable·GI)는 타지 않는다 —
// 공유는 device-plugin 설정만 바꾸므로 cordon 이 필요 없다(DP 재시작만 필요). backend Validate 도
// 부르지 않는다: 검증할 파티션 레이아웃이 없고, NVIDIA Validate 는 빈 layout 을 fail-closed 로
// 거부하는 것이 맞다(형식 없는 요청을 하드웨어 경로에 들이지 않는다).
func (r *AcceleratorPartitionPolicyReconciler) runSharingOnly(
	acpp *npuv1alpha1.AcceleratorPartitionPolicy,
	backend partition.Backend,
	t partition.Target,
	ts npuv1alpha1.TargetStatus,
	ec *evidenceCtx,
) (npuv1alpha1.TargetStatus, error) {
	if acpp.Spec.EffectiveSharingMode() == npuv1alpha1.SharingModeExclusive {
		// 남은 공유가 있으면 먼저 내린다 — sharing-only 정책을 exclusive 로 patch 한 해제 경로.
		// (CRD CEL 은 layout·sharing 동시 부재를 거부하지만, 명시적 mode: exclusive 나 CEL 이전에
		// 생성된 객체는 여기 도달한다.) 내린 뒤에는 아무것도 요청하지 않는 정책이므로 Failed 다.
		if out, derr := r.disableSharing(acpp, backend, t, ts); derr != nil {
			return out, derr
		}
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionFalse, "ValidationFailed",
			"spec.layout and spec.sharing are both empty; the policy requests nothing", acpp.Generation)
		return ts, nil
	}
	setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionTrue, "ValidationSucceeded",
		"sharing-only policy; no partition layout to validate", acpp.Generation)
	// 공유 요청 자체의 결함(미지원 backend / 형식 위반)은 어떤 mutation 보다 먼저 거른다.
	// 여기를 통과한 backend 는 SharingBackend 구현체다(현재 nvidia 뿐 — runSharing 의 DP 재시작도
	// nvidia 전용이라 대칭이 맞는다).
	sb, rejected, ok, perr := r.precheckSharing(t.Ctx, acpp, backend, ts)
	if perr != nil {
		return ts, perr
	}
	if !ok {
		return r.rejectSharing(acpp, backend, t, rejected)
	}
	base, err := r.sharingOnlyBase(t.Ctx, acpp, t.NodeName)
	if err != nil {
		return ts, err // transient(DP 기동 전 광고 0 등) — requeue 로 자가치유.
	}
	// 이 경로는 파티션 레이아웃이 없어 runTarget 의 partition-Ready 지점(근거 게이트가 Verified/
	// Expectation 을 채우는 곳)을 거치지 않는다 — 채우지 않으면 근거 게이트가 이 pass 를 완전히
	// 건너뛰어, 공유만 요청한 정책이 광고를 한 번도 비교하지 않고 Ready 로 올라간다. 여기서 쓰는
	// 기대 광고량은 runSharing 이 실제로 sharingReady 에 찍는 값과 같은 계산이다.
	ec.Expectation.Allocatable = sb.ExpectedSharedAllocatable(base, partition.SharingLayoutFrom(acpp.Spec))
	ec.Verified = true
	return r.runSharing(acpp, backend, t, ts, base)
}

// sharingOnlyBase 는 순수 공유 정책의 기준 allocatable 이다 — 노드가 광고 중인 full-GPU 수만
// 배수 대상으로 삼는다. mig-* 조각 리소스는 파티션 정책(같은 노드의 다른 ACPP) 소유이므로 여기서
// 배수하면 남의 광고를 바꾼다.
//
// base 불변식(C-1): 공유가 이미 적용된 뒤의 라이브 allocatable 은 물리 수가 아니라 배수된
// 광고량이다 — 그대로 base 로 쓰면 replicas 편집/크래시 재진입마다 expected 가 배수의 배수로
// 부풀어 verify 실패 → rollback(자기 자신의 직전 설정 복원) → 재진입 무한 flap 이 된다.
// partitioned-shared 경로가 expectedAllocatable(rec)로 지키는 불변식과 같게, 첫 적용 때 물리 수를
// 저널(BaselineGPUCount — MIG 경로 전용 필드였으나 sharing-only 는 MIG 경로에 진입하지 않아 충돌
// 없음)에 선영속하고, 공유가 활성인 재진입은 라이브 대신 저널을 읽는다.
func (r *AcceleratorPartitionPolicyReconciler) sharingOnlyBase(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, nodeName string) (map[string]int32, error) {
	rec := getApplyRecord(acpp, nodeName)
	if rec.SharingMode != "" && rec.BaselineGPUCount > 0 {
		return map[string]int32{nvidiaGPUResource: rec.BaselineGPUCount}, nil
	}
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return nil, err
	}
	q := node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResource)]
	n := int32(q.Value())
	if n <= 0 {
		return nil, fmt.Errorf("sharing-only: node %s advertises no %s to share", nodeName, nvidiaGPUResource)
	}
	// persist-before-mutate: runSharing 의 mutation(DP 재시작) 이후에는 물리 수를 다시 관측할 수 없다.
	rec.NodeName = nodeName
	rec.BaselineGPUCount = n
	if err := r.patchApplyRecord(ctx, acpp, rec); err != nil {
		return nil, err
	}
	return map[string]int32{nvidiaGPUResource: n}, nil
}

// sharingDaemonFailure 는 ensureMpsControlDaemon 실패를 종점 status 로 옮긴다.
// daemon 미준비(ErrMpsDaemonNotReady)는 실패가 아니라 대기다 — 이미지 pull·스케줄링은 transient 라
// Failed 로 끝내면 재시도 없이 죽는다. Applying 으로 두면 Reconcile 이 30s 뒤 다시 확인한다.
// 어느 쪽이든 호출자는 여기서 반환하므로 배선·검증으로 진행하지 않는다 — daemon 없이
// sharingReady 에 도달하는 경로를 막는 것이 이 함수의 존재 이유다.
func sharingDaemonFailure(ts npuv1alpha1.TargetStatus, acpp *npuv1alpha1.AcceleratorPartitionPolicy, err error) (npuv1alpha1.TargetStatus, error) {
	if errors.Is(err, ErrMpsDaemonNotReady) {
		ts.Phase = npuv1alpha1.ACPPPhaseApplying
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
			npuv1alpha1.ReasonSharingDaemonNotReady, err.Error(), acpp.Generation)
		return ts, nil
	}
	ts.Phase = npuv1alpha1.ACPPPhaseFailed
	setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
		npuv1alpha1.ReasonSharingApplyFailed, err.Error(), acpp.Generation)
	return ts, err
}

// sharingProbeTimeout 은 "이미 적용·수렴돼 있는가" 를 확인하는 멱등 게이트의 상한이다.
// 재적용 이후 검증(verifyTimeout)과 달리 DP 재시작을 기다리는 상황이 아니므로 짧게 끊는다.
const sharingProbeTimeout = 10 * time.Second

// disableSharing 은 spec.sharing 이 제거되거나 exclusive 로 바뀌었을 때 이미 적용된 공유를 내린다.
// 저널(SharingMode)이 유일한 근거다 — 이 경로가 없으면 공유는 켤 수는 있는데 끌 수가 없다:
// spec 은 exclusive 인데 ConfigMap·DS 배선·저널이 그대로 남아 클러스터는 배수를 계속 광고하고,
// 검증기는 배수가 빠진 기대값과 `>=` 로 비교하므로 수렴으로 읽어 status 는 Ready 를 보고한다.
// 삭제 경로(handleNvidiaDeletion)와 같은 3단계다: RollbackSharing(zero-state) → DP 재시작 → 저널 해제.
func (r *AcceleratorPartitionPolicyReconciler) disableSharing(
	acpp *npuv1alpha1.AcceleratorPartitionPolicy,
	backend partition.Backend,
	t partition.Target,
	ts npuv1alpha1.TargetStatus,
) (npuv1alpha1.TargetStatus, error) {
	if mode := getApplyRecord(acpp, t.NodeName).SharingMode; mode == npuv1alpha1.SharingModeTimeSliced || mode == npuv1alpha1.SharingModeMPS {
		// 공유를 구현하지 않는 backend 는 애초에 적용된 적이 없다.
		if sb, err := partition.SharingFor(backend); err == nil {
			if err := sb.RollbackSharing(t, partition.SharingRollbackState{}); err != nil {
				return ts, err
			}
			if err := r.restartNvidiaDevicePlugin(t.Ctx, t.NodeName); err != nil {
				// 재시작 실패로 종점을 바꾸지 않는다 — 배선은 이미 끊겼고 다음 reconcile 이 재확인한다.
				logf.FromContext(t.Ctx).Error(err, "sharing disable: device-plugin restart failed")
			}
			r.restoreSharingRecord(t, acpp, "", 0)
		}
	}
	// control daemon 회수는 저널 상태와 무관하게 멱등이라 항상 부르되, **배선을 걷은 뒤**다
	// (삭제 경로와 같은 순서 규약). 거꾸로 두면 원복이 실패해 위에서 반환할 때 device-plugin 이
	// 죽은 daemon 의 파이프를 문 채 다음 requeue 까지 남는다.
	if err := r.ensureMpsControlDaemon(t.Ctx, npuv1alpha1.SharingModeExclusive, acpp.Name, t.NodeName); err != nil {
		return ts, err
	}
	return ts, nil
}

// rejectSharing 은 거절 종점을 확정하기 전에, 이미 적용돼 있는 공유 배선을 걷는다(D-8 리뷰 C-1).
//
// 거절은 mutation 이전 판정이지만, "이전 빌드/이전 spec 이 적용해 둔 배선" 은 그 판정이 만든 것이
// 아니어서 아무도 청소하지 않는다: spec 이 여전히 공유를 요청하므로 disableSharing(exclusive 전환)도
// 삭제 finalizer 도 불리지 않고, ApplySharing 실패 원복 경로에는 애초에 도달하지 않는다. 그 결과
// 전역 device-plugin 은 **거절된** 요청의 설정 때문에 계속 기동 실패하고(D-8 이면 validateFlags),
// 라이브에서 관측된 간헐 회귀(allocatable 0↔4)가 회복 시도 없는 영구 0 으로 고정된다.
//
// 청소 주체는 disableSharing 을 그대로 재사용한다 — 필요한 3단계(RollbackSharing(zero-state) →
// DP 재시작 → 저널 해제 + control daemon 회수)가 이미 그 함수이고, 저널이 적용을 가리키지 않으면
// 멱등 no-op 이다. 그래서 mps 로 좁히지 않았다: 어떤 사유로 거절되든 "정책이 약속하지 않는 전역
// 설정" 이 남아 있으면 안 된다는 규칙은 같다(timeSliced 도 같은 전역 ConfigMap 을 쓴다).
func (r *AcceleratorPartitionPolicyReconciler) rejectSharing(
	acpp *npuv1alpha1.AcceleratorPartitionPolicy,
	backend partition.Backend,
	t partition.Target,
	rejected npuv1alpha1.TargetStatus,
) (npuv1alpha1.TargetStatus, error) {
	if _, err := r.disableSharing(acpp, backend, t, rejected); err != nil {
		// 거절 판정은 그대로 사실이므로 종점은 유지하고, 청소만 requeue 로 다시 시도한다.
		return rejected, err
	}
	return rejected, nil
}

// precheckSharing 은 공유 요청 자체의 결함(미지원 backend / 형식 위반)을 판정한다.
// ok=false 면 두 번째 반환값이 거절 종점 status 다. runTarget 은 apply 이전에, runSharing 은 mutation
// 직전에 같은 판정을 쓴다 — 요청 결함으로 파티션을 바꿔놓고 나서 거절하면 되돌릴 것만 늘어난다.
// err != nil 은 판정 불가(ACPP 목록 조회 실패)이며 호출자는 requeue 로 다시 물어야 한다.
func (r *AcceleratorPartitionPolicyReconciler) precheckSharing(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, backend partition.Backend, ts npuv1alpha1.TargetStatus) (partition.SharingBackend, npuv1alpha1.TargetStatus, bool, error) {
	layout := partition.SharingLayoutFrom(acpp.Spec)
	if layout.Mode == npuv1alpha1.SharingModeExclusive {
		return nil, ts, true, nil // 공유 요청 없음.
	}
	sb, err := partition.SharingFor(backend)
	if err != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseUnsupported
		setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionFalse,
			npuv1alpha1.ReasonSharingUnsupported, err.Error(), acpp.Generation)
		return nil, ts, false, nil
	}
	// 장치별 MPS capability 게이트(review 최종 ②). ValidateSharing 은 replica 개수만 보므로, 이것이
	// 없으면 Discover 가 "이 장치는 MPS 불가" 라고 이미 판정해 status 에 적어 둔 장치에 대해서도
	// 적용 경로가 그대로 배수를 요청한다. 그 결과는 둘 중 하나이고 둘 다 나쁘다: DP 가 거부하면
	// 사용자에게 "shared allocatable did not converge" 만 가고 진짜 이유는 어디에도 안 나오며,
	// DP 가 배수를 광고해 버리면 같은 CR 의 multiProcess.supported: false 와 정면으로 모순되는
	// Ready + Verified 가 찍힌다. 추상 사용자 API(intent/mode.go)는 이 게이트를 이미 갖고 있었다.
	//
	// mps 에만 건다. timeSliced 로 넓히면 비-MIG 장치가 deviceStatusFor 를 건너뛰어 TimeSlicing 이
	// zero-value 로 남는 것(Task 1 스코프 결정, backend.go 참조) 때문에 A2 의 time-slicing 이 막힌다.
	//
	// 도달성 주의(D-8): 아래 장치 단위 판정은 현재 배포에서 **실행되지 않는다** — 바로 위의 D-8 가드가
	// 항상 먼저 거절하기 때문이다(렌더러가 mixed 를 무조건 붙인다). 의도된 도달 불가이며 삭제 대상이
	// 아니다: 노드별 device-plugin config 분리가 들어오면 즉시 다시 유일한 게이트가 되고, 지금 지우면
	// D-5 라이브 검증 결과가 코드에서 사라진다. 유일한 커버리지는 mixed 아닌 DP 픽스처가 지킨다.
	//
	// 검사 대상은 "이 노드의 같은 벤더 전체 장치" 가 아니라 "다른 정책이 소유하지 않은 장치" 다
	// (라이브 결함 D-5, Task 6 §6.3): ts.Devices 는 Discover 가 채운 노드 전체 목록이라, MIG 가 켜진
	// 형제 장치를 다른 ACPP 가 이미 소유·관리 중인 노드(worker1 = MIG A30 + 비-MIG A2)에서는 A2 만
	// 겨냥한 MPS 정책도 A30 때문에 통째로 거부됐다 — 배선(ensureMpsControlDaemon)에 한 번도 도달하지
	// 못했다. 자기 자신이 소유한 장치는 제외 대상이 아니다(파티션+MPS 조합은 여전히 이 게이트가 막는다).
	//
	// 한계(이번 스코프 밖): CRD 에 장치 셀렉터가 없어(nodeSelector=호스트, vendor=벤더) "미소유
	// 장치가 여럿이고 그중 일부만 MPS 지원" 은 여전히 표현할 수 없다 — 그 경우는 fail-closed 로
	// 거절된다. 셀렉터가 생기면 이 판정은 그 집합으로 좁혀야 한다.
	if layout.Mode == npuv1alpha1.SharingModeMPS {
		// D-8(라이브 Task 6 §6d.4) — 장치 단위보다 먼저 프로세스 단위 배타를 본다.
		blocked, berr := r.mpsBlockedByMigStrategy(ctx, ts.NodeName)
		if berr != nil {
			return nil, ts, false, berr
		}
		if blocked {
			ts.Phase = npuv1alpha1.ACPPPhaseFailed
			setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionFalse,
				npuv1alpha1.ReasonSharingUnsupported,
				fmt.Sprintf("node %s runs the MIG-managed device-plugin (nvidia.MigActiveNodeLabel=true): "+
					"upstream k8s-device-plugin refuses to start when --mig-strategy=mixed and sharing.mps "+
					"are combined, so applying MPS here would crash-loop that node's plugin and drop the "+
					"MIG partitions it advertises; remove MIG partitions from this node first, or request "+
					"MPS on a node without active MIG partitions",
					ts.NodeName),
				acpp.Generation)
			return nil, ts, false, nil
		}
		ownedByOthers, oerr := r.devicesOwnedByOtherPolicies(ctx, ts.NodeName, acpp.Name)
		if oerr != nil {
			return nil, ts, false, oerr
		}
		judged := 0
		for _, d := range ts.Devices {
			if ownedByOthers[d.PCIAddress] {
				continue
			}
			judged++
			if d.SharingCapability.MultiProcess.Supported {
				continue
			}
			ts.Phase = npuv1alpha1.ACPPPhaseFailed
			setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionFalse,
				npuv1alpha1.ReasonSharingUnsupported,
				fmt.Sprintf("device %s cannot run MPS: %s", d.ID, d.SharingCapability.MultiProcess.Reason),
				acpp.Generation)
			return nil, ts, false, nil
		}
		if judged == 0 && len(ts.Devices) > 0 {
			// 이 정책이 다룰 장치가 하나도 없다 — 통과시키면 남의 장치에 대해 배수를 광고한다.
			// devicesOwnedByOtherPolicies 는 map[string]bool(PCI→소유여부)만 반환해 소유 정책명까지는
			// 여기서 알 수 없다 — 반환 타입을 map[string]string(PCI→정책명)으로 바꾸면 호출부 3곳에
			// 영향을 주는 더 큰 diff 라 이번엔 문구 안내만 덧붙인다(review-d5 M-1, review-i1 M-2 후속 과제).
			ts.Phase = npuv1alpha1.ACPPPhaseFailed
			setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionFalse,
				npuv1alpha1.ReasonSharingUnsupported,
				fmt.Sprintf("every device on node %s is owned by another AcceleratorPartitionPolicy; "+
					"no device left for MPS (check other ACPPs targeting this node for the owner)",
					ts.NodeName),
				acpp.Generation)
			return nil, ts, false, nil
		}
	}
	if err := sb.ValidateSharing(layout); err != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionFalse,
			npuv1alpha1.ReasonSharingInvalid, err.Error(), acpp.Generation)
		return nil, ts, false, nil
	}
	return sb, ts, true, nil
}

// devicesOwnedByOtherPolicies 는 node 의 장치 중 **다른** ACPP 가 소유를 주장한 PCI 집합이다.
// 근거는 ApplyRecord(GPUPCIs + OwnerUID) — 노드 owner-lock(migOwnerAnnotation)의 장치 단위 대응물이며,
// apply 경로가 mutation 이전에 영속하는 저널이다(persist-before-mutate). Ready 만 소유로 세지 않는
// 이유가 그것이다: 형제 정책이 적용 중(Applying)인 창에서도 그 장치는 이미 그 정책 소유이므로,
// Ready 를 요구하면 이 정책이 그 창 동안 Failed↔대기 를 왕복한다.
func (r *AcceleratorPartitionPolicyReconciler) devicesOwnedByOtherPolicies(ctx context.Context, node, self string) (map[string]bool, error) {
	var list npuv1alpha1.AcceleratorPartitionPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	owned := make(map[string]bool)
	for i := range list.Items {
		p := &list.Items[i]
		if p.Name == self {
			continue
		}
		rec := getApplyRecord(p, node)
		if rec.OwnerUID == "" || rec.OwnerUID != string(p.UID) {
			continue // 소유 주장이 없는(또는 남의 UID 를 든 stale) 저널은 근거가 아니다.
		}
		for _, pci := range rec.GPUPCIs {
			owned[pci] = true
		}
	}
	return owned, nil
}

// mpsBlockedByMigStrategy 는 "이 노드에서 지금 MPS 를 켜면 device-plugin 이 기동조차 못 하는가"
// 를 판정한다. MPS 근본해결(노드별 mixed/flat DaemonSet 분리, Task 1~4) 이후에는 노드의
// nvidia.MigActiveNodeLabel 하나만 보면 된다 — 그 라벨이 true 인 노드는 mixed DS(항상
// --mig-strategy=mixed)가 맡으므로 업스트림이 거부하고, 없는 노드는 flat DS(그 플래그를
// 렌더하지 않음)가 맡으므로 MPS 가 통과한다. DS 를 직접 열어 Args 를 파싱하던 이전 판정(D-8
// 1차 수정)은 클러스터 전역 단일 DS 를 전제해 다른 노드의 MIG 사용까지 이 노드를 막았다 —
// 노드별 분리 이후에는 그 전제가 사라졌다.
func (r *AcceleratorPartitionPolicyReconciler) mpsBlockedByMigStrategy(ctx context.Context, node string) (bool, error) {
	var n corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // 노드가 없으면 이 정책의 다른 전제(node selector 매칭 등)가 먼저 걸러낸다.
		}
		return false, err // 판정 불가 — 호출자는 requeue 로 다시 묻는다.
	}
	return n.Labels[nvidia.MigActiveNodeLabel] == labelValueTrue, nil
}

// verifySharedAllocatable 은 공유 기대 allocatable 수렴을 검증한다(Verifier seam — nil 이면 검증 skip).
// liveVerifier 는 타임아웃까지 폴링하므로 DP 재시작 직후의 조기 false 를 내지 않는다.
func (r *AcceleratorPartitionPolicyReconciler) verifySharedAllocatable(t partition.Target, expected map[string]int32) (*partition.VerifyResult, error) {
	if r.Verifier == nil {
		return nil, nil
	}
	return r.Verifier.VerifyAllocatable(t, expected)
}

// restoreSharingRecord 는 공유 저널을 이전 값으로 되돌린다(선재 공유가 없었으면 빈 값 = 해제).
// RollbackSharing 이 이전 ConfigMap 을 복원하므로 저널도 같은 값으로 돌아가야 status 가 사실과 맞는다.
func (r *AcceleratorPartitionPolicyReconciler) restoreSharingRecord(t partition.Target, acpp *npuv1alpha1.AcceleratorPartitionPolicy, prevMode string, prevReplicas int32) {
	rec := getApplyRecord(acpp, t.NodeName)
	rec.NodeName = t.NodeName
	rec.SharingMode, rec.SharingReplicas = prevMode, prevReplicas
	if err := r.patchApplyRecord(t.Ctx, acpp, rec); err != nil {
		logf.FromContext(t.Ctx).Error(err, "sharing: restoring journal failed")
	}
}

// sharingReady 는 공유 적용 성공 종점(Ready + 광고 배수 반영 + 격리 등급 강등)을 찍는다.
//
// 격리 강등이 핵심이다: Discover 가 채운 IsolationCapability 는 "장치가 무엇을 할 수 있는가" 이고,
// 여기서 반영하는 것은 "지금 무엇이 적용돼 있는가" 다. MIG 로 하드웨어 분리된 장치라도 그 위에
// 시분할 replica 를 얹은 순간 replica 사이에는 compute/memory 격리가 없다 — 그 상태에서
// `advertisedResources: 16` + `isolation.compute: hardware` 를 함께 보고하면 status 만 읽는
// 소비자에게는 "하드웨어 격리된 장치 16개" 로 읽힌다(실제로는 4조각을 4중 시분할).
// fault 축은 건드리지 않는다 — 시분할이 바꾸는 것이 아니다.
func sharingReady(ts npuv1alpha1.TargetStatus, acpp *npuv1alpha1.AcceleratorPartitionPolicy, expected map[string]int32, mode string, replicas int32) npuv1alpha1.TargetStatus {
	ts.Advertisement.AdvertisedResources = expected
	ts.Advertisement.Reason = fmt.Sprintf(
		"%s x%d — replicas share one device; no isolation between replicas", mode, replicas)
	for i := range ts.Devices {
		iso := &ts.Devices[i].IsolationCapability
		if iso.Compute != "" {
			iso.Compute = npuv1alpha1.IsolationNone
		}
		if iso.Memory != "" {
			iso.Memory = npuv1alpha1.IsolationNone
		}
	}
	ts.Phase = npuv1alpha1.ACPPPhaseReady
	setCond(&ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionTrue,
		npuv1alpha1.ReasonSharingVerified, "shared allocatable converged", acpp.Generation)
	return ts
}
