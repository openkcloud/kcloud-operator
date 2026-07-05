// ============================================================
// acpp_migmode.go: MIG mode enable 오케스트레이션 (quiesce → enable → reboot → 재관측)
// 상세: parity 격차 ②의 "장치 모드 전환" 축. Ampere pending MIG 는 재부팅으로만 확정되므로
//
//	기존 reboot Job 렌더러를 ACPP 소유로 재사용한다. 각 단계는 ApplyRecord.migPhase 로 영속되고
//	(persist-before-mutate), 실패 시 정책이 잠근 노드만 다시 schedulable 로 되돌린다.
//
// 생성일: 2026-07-29 | 수정일: 2026-07-30
// ============================================================
package controller

import (
	"context"
	"fmt"
	"os"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/driverjob"
	"kcloud-operator/internal/naming"
	"kcloud-operator/internal/partition"
	"kcloud-operator/internal/partition/nvidia"
)

// migModeEnableAction 은 mode enable Job 의 결정론적 이름에 들어가는 action 토큰이다(GI apply 와 구분).
const migModeEnableAction = "mig-mode-enable"

// maxMigRebootAttempts 는 MIG mode 확정을 위한 재부팅 시도 상한이다. 재부팅 Job 은 노드와 함께
// 죽어 Failed → TTL GC 되므로, 상한이 없으면 mode 가 확정되지 않는 노드를 TTL 주기로 영원히
// 재부팅시킨다(phase 는 Applying 이라 실패 신호도 없다). 드라이버 업그레이드 경로의
// RebootAttempts/maxReboots 와 같은 규율.
const maxMigRebootAttempts int32 = 2

// isMigModeEnablePhase 는 저널 phase 가 mode 전환 구간(GI 생성 이전)인지다.
// 이 구간은 GI 를 만들지 않았으므로 삭제 경로의 rollback 대상이 아니고(handleNvidiaDeletion),
// runNvidiaTarget 의 in-progress 분류에는 포함돼야 한다 — 어느 쪽도 아닌 phase 는 geometry 가
// 우연히 일치할 때 ExistingMigConfiguration 으로 떨어져 자가치유 경로가 사라진다.
func isMigModeEnablePhase(phase string) bool {
	switch phase {
	case npuv1alpha1.MigPhaseQuiescing, npuv1alpha1.MigPhaseModeEnabling,
		npuv1alpha1.MigPhaseRebootRequested, npuv1alpha1.MigPhaseRebootWaiting:
		return true
	}
	return false
}

// ensureMigModeEnabled 는 관측된 장치가 전부 MIG mode Enabled 가 될 때까지 단계를 진행한다.
// done=false 면 호출자는 status 를 기록하고 requeue 한다(하드웨어 변경은 여러 reconcile 에 걸친다).
//
// 단계: (관측 검증) → pending 이면 reboot → 아니면 quiesce → -mig 1 → pending 대기.
// 각 mutation 은 저널 영속 이후에만 수행한다 — 크래시해도 재진입이 같은 지점을 다시 밟는다.
func (r *AcceleratorPartitionPolicyReconciler) ensureMigModeEnabled(
	acpp *npuv1alpha1.AcceleratorPartitionPolicy,
	t partition.Target,
	devs []nvidia.MigDevice,
	ts npuv1alpha1.TargetStatus,
) (bool, npuv1alpha1.TargetStatus, error) {
	log := logf.FromContext(t.Ctx)
	if !nvidia.NeedsModeEnable(devs) && !nvidia.NeedsReboot(devs) {
		// 모델 B 기존 경로 — 이미 Enabled(또는 우리가 켤 대상이 아님).
		// 전환이 끝났으므로 재부팅 예산을 되돌려주고 전환 phase 도 지운다. 예산을 안 지우면 나중에
		// mode 가 다시 꺼졌을 때(spec 변경·수동 disable) 남은 카운터 탓에 재부팅 한 번 만에 상한에
		// 걸리고, 전환 phase 를 안 지우면 저널이 "전환 중"으로 남아 이후 분류(no-diff)가 끝난 전환을
		// 진행 중으로 읽는다 — 그 조합은 수렴도 실패도 없이 cordon 된 채 같은 자리를 반복한다.
		// 지운 뒤의 분류는 apply 경로가 소유한다(computeBaseline 이 Applying 으로 덮는다).
		rec := getApplyRecord(acpp, t.NodeName)
		if rec.RebootAttempts > 0 || isMigModeEnablePhase(rec.MigPhase) {
			rec.NodeName = t.NodeName
			rec.RebootAttempts = 0
			if isMigModeEnablePhase(rec.MigPhase) {
				rec.MigPhase = ""
			}
			if err := r.patchApplyRecord(t.Ctx, acpp, rec); err != nil {
				return false, ts, err
			}
		}
		return true, ts, nil
	}
	// fail-closed: 상태를 모르면 cordon 도 -mig 1 도 하지 않는다.
	if !nvidia.ModeObservable(devs) {
		ts.Phase = npuv1alpha1.ACPPPhaseWaitingForDrain
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
			npuv1alpha1.ReasonObservationUnavailable, "mig mode not observable; refusing to enable", acpp.Generation)
		return false, ts, nil
	}

	// 1) 재부팅 대기: pending 이 Enabled 면 이미 -mig 1 이 먹은 상태다 — 재부팅으로만 확정된다.
	if nvidia.NeedsReboot(devs) {
		if getApplyRecord(acpp, t.NodeName).RebootAttempts >= maxMigRebootAttempts {
			// 재부팅해도 mode 가 확정되지 않는다 — 더 재부팅해봐야 노드만 왕복한다(수동 조치).
			if rerr := r.restoreSchedulable(t.Ctx, acpp, t.NodeName); rerr != nil {
				log.Error(rerr, "restore schedulable failed", "node", t.NodeName)
			}
			ts.Phase = npuv1alpha1.ACPPPhaseFailed
			setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, npuv1alpha1.ReasonMigModeNotEnabled,
				fmt.Sprintf("MIG mode still pending after %d reboots; manual intervention required", maxMigRebootAttempts),
				acpp.Generation)
			return false, ts, nil // terminal — err 로 재시도 폭주시키지 않는다.
		}
		// review-i1 M-1/M-3: 재부팅 전에도 소유권을 저널하고 노드를 cordon 한다 — 이전에는 이
		// 갈래가 cordon 없이 재부팅 Job 을 예약해 (a) 새 워크로드가 곧 재부팅될 노드로 계속
		// 들어오고 (b) OwnerUID/GPUPCIs 가 비어 있어 D-5 급 오판 재발 창이 생겼다.
		if err := r.cordonForApply(t.Ctx, acpp, t.NodeName, devs); err != nil {
			return false, ts, err
		}
		if err := r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseRebootRequested); err != nil {
			return false, ts, err
		}
		if err := r.ensureAcppRebootJob(t, acpp); err != nil {
			if rerr := r.restoreSchedulable(t.Ctx, acpp, t.NodeName); rerr != nil {
				log.Error(rerr, "restore schedulable failed", "node", t.NodeName)
			}
			ts.Phase = npuv1alpha1.ACPPPhaseFailed
			setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
				npuv1alpha1.ReasonMigModeNotEnabled, "reboot job: "+err.Error(), acpp.Generation)
			return false, ts, err
		}
		if err := r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseRebootWaiting); err != nil {
			return false, ts, err
		}
		ts.Phase = npuv1alpha1.ACPPPhaseApplying
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
			npuv1alpha1.ReasonMigModeNotEnabled, "reboot requested to activate pending MIG mode", acpp.Generation)
		return false, ts, nil
	}

	// 2) quiesce: cordon + 장치 점유 워크로드 배출. -mig 1 은 GPU 를 쥔 클라이언트가 있으면 실패한다.
	if err := r.cordonForApply(t.Ctx, acpp, t.NodeName, devs); err != nil {
		return false, ts, err // 영속 실패 시 cordon 하지 않는다(우리가 잠갔다는 사실이 durable 해야 복원 가능).
	}
	q := partition.NvidiaQuiescer(r.Client)
	// remaining 은 정보성이다 — 완료 판정은 오직 AssertQuiesced 로 한다(Task 5 I-2).
	if _, err := q.EvictDeviceWorkloads(t.Ctx, t.NodeName); err != nil {
		return false, ts, err
	}
	if err := q.AssertQuiesced(t.Ctx, t.NodeName); err != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseWaitingForDrain
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
			npuv1alpha1.ReasonQuiesceRequired, err.Error(), acpp.Generation)
		return false, ts, nil
	}

	// 3) mode enable 실행(-mig 1). 성공해도 Ampere 는 pending 이므로 done 이 아니다.
	pcis := nvidia.ModeEnableTargets(devs)
	steps := nvidia.ModeEnableSteps(pcis)
	opID, cmdHash := nvidia.OperationID(string(acpp.UID), acpp.Generation, t.NodeName, migModeEnableAction, steps)
	if err := r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseModeEnabling); err != nil {
		return false, ts, err
	}
	if err := r.nvidiaExecutor().Run(t.Ctx, opID, cmdHash, t.NodeName, steps); err != nil {
		// 전환에 실패했으면 GPU 는 원래(Disabled) 상태 그대로다 — 노드를 묶어둘 이유가 없다.
		if rerr := r.restoreSchedulable(t.Ctx, acpp, t.NodeName); rerr != nil {
			log.Error(rerr, "restore schedulable failed", "node", t.NodeName)
		}
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
			npuv1alpha1.ReasonMigModeNotEnabled, fmt.Sprintf("mig mode enable failed: %v", err), acpp.Generation)
		return false, ts, err
	}
	log.Info("MIG mode enable issued; reboot required to activate", "node", t.NodeName, "pcis", pcis)
	ts.Phase = npuv1alpha1.ACPPPhaseApplying
	setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
		npuv1alpha1.ReasonMigModeNotEnabled, "mig mode enable issued; waiting for pending state", acpp.Generation)
	return false, ts, nil
}

// cordonForApply 는 하드웨어 변경(GI apply / MIG mode 전환) 전제인 cordon 을 정책 스스로 취득한다.
// 저널을 노드 patch 이전에 영속하므로(persist-before-mutate) 도중에 죽어도 복원 주체가 남는다.
//
// 배출(evict)은 하지 않는다 — cordon 은 "새 워크로드 유입 차단" 이고, 이미 장치를 쥔 워크로드를
// 어떻게 비울지는 호출자의 소관이다(mode 전환은 배출까지 하고, GI apply 는 idle 전제로 대기한다).
func (r *AcceleratorPartitionPolicyReconciler) cordonForApply(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, node string, targets []nvidia.MigDevice) error {
	if err := r.journalQuiescing(ctx, acpp, node, targets); err != nil {
		return err
	}
	return r.cordonNode(ctx, acpp, node, "partition apply")
}

// cordonForRollback 은 삭제 경로(handleNvidiaDeletion)의 rollback 전제인 cordon 을 정책 스스로
// 취득한다. 성공한 apply 는 종점에서 노드를 되돌려 놓으므로(restoreSchedulable), 삭제 시점의 노드는
// 거의 항상 schedulable 이다 — cordon 주체가 없으면 assertNodeQuiesced 가 영구히 막고 finalizer 가
// 빠지지 않는다(D-10). 호출 지점은 owner-lock 이 이 정책 소유임을 확인한 뒤이므로 주장이 정당하다.
//
// 이미 cordon 된 노드는 건드리지 않는다 — 우리 재진입이면 소유 저널(CordonedByPolicy=true)을 뒤집어
// 복원 주체를 잃고, 운영자가 잠근 노드면 정비 중인 노드를 정책이 되살리게 된다.
//
// apply 경로의 journalQuiescing 을 재사용하지 않는 이유: 그 함수는 MigPhase 를 Quiescing 으로
// 덮는데, Quiescing 은 mode 전환 구간(isMigModeEnablePhase)이라 다음 pass 의 hardwareChanged 판정이
// false 로 뒤집힌다 — rollback 을 건너뛴 채 GI 를 남기고 finalizer 가 빠진다. 삭제 경로의 저널은
// "무엇을 되돌려야 하는가" 의 유일한 근거(snapshot)이므로 phase·GPUPCIs 는 불변으로 둔다.
func (r *AcceleratorPartitionPolicyReconciler) cordonForRollback(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, rec npuv1alpha1.ApplyRecord) error {
	var n corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: rec.NodeName}, &n); err != nil {
		return err
	}
	if n.Spec.Unschedulable {
		return nil
	}
	rec.CordonedByPolicy = true
	if err := r.patchApplyRecord(ctx, acpp, rec); err != nil {
		return err // 영속 실패 시 cordon 하지 않는다(우리가 잠갔다는 사실이 durable 해야 복원 가능).
	}
	return r.cordonNode(ctx, acpp, rec.NodeName, "partition rollback")
}

// syncMigActiveLabel 은 노드의 nvidia.MigActiveNodeLabel 을 실제 GI 존재 여부와 맞춘다.
// device-plugin 이 이 라벨로 mixed/flat DaemonSet 중 어느 쪽이 그 노드를 맡을지 가르므로(MPS
// 근본해결), 라벨이 실제와 어긋나면 D-5/D-8 류 오판이 재발한다. 현재 값과 같으면 patch 하지
// 않는다(불필요한 write·watch 트리거 방지 — U-1 이 남긴 교훈과 같은 원칙).
func (r *AcceleratorPartitionPolicyReconciler) syncMigActiveLabel(ctx context.Context, node string, active bool) error {
	var n corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return err
	}
	cur := n.Labels[nvidia.MigActiveNodeLabel] == labelValueTrue
	if cur == active {
		return nil
	}
	base := n.DeepCopy()
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	if active {
		n.Labels[nvidia.MigActiveNodeLabel] = labelValueTrue
	} else {
		delete(n.Labels, nvidia.MigActiveNodeLabel)
	}
	return r.Patch(ctx, &n, client.MergeFrom(base))
}

// cordonNode 는 cordon 취득의 유일한 지점이다(apply·삭제 공통). 호출 전에 소유 저널
// (CordonedByPolicy)이 영속돼 있어야 한다 — persist-before-mutate 순서는 호출자의 책임이다.
func (r *AcceleratorPartitionPolicyReconciler) cordonNode(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, node, purpose string) error {
	if err := partition.NvidiaQuiescer(r.Client).Cordon(ctx, node); err != nil {
		return err
	}
	// 노드가 조용히 잠기지 않게 한다. GPU 워크로드가 스스로 끝나지 않으면 대기에는 상한이 없고,
	// 그동안 GPU 를 쓰지 않는 워크로드까지 이 노드에서 밀려난다 — 운영자가 status 를 파헤치지 않아도
	// 알 수 있는 신호가 필요하다. 반복 발행은 client-go event aggregation 이 한 객체로 접는다.
	if r.Recorder != nil {
		r.Recorder.Eventf(acpp, corev1.EventTypeNormal, "NodeCordoned",
			"cordoned node %s for %s; waiting for device workloads to drain", node, purpose)
	}
	return nil
}

// journalQuiescing 은 cordon 이전에 Quiescing 저널을 영속한다. 최초 진입에서만 "이 cordon 이
// 정책 소유인지"(CordonedByPolicy)를 기록한다 — 재진입이 우리 cordon 을 외부 cordon 으로 오인해
// 복원 책임을 잃으면, 실패 후 노드가 영영 unschedulable 로 남는다.
//
// 소유 주장(OwnerUID)과 그 대상 장치(GPUPCIs)를 같은 write 로 영속한다. 장치 목록을 뒤(computeBaseline)로
// 미루면, 그 사이에 멈춘 정책(예: MIG mode 이미 Enabled + GPU 점유 pod → WaitingForDrain, 상한 없음)의
// 저널이 "내 것" 이라고만 하고 어느 장치인지는 말하지 않는다 — 장치 단위 소유 판정
// (devicesOwnedByOtherPolicies)이 그 창에서 눈이 멀어 형제 정책이 남의 장치를 자기 판정 대상으로
// 삼는다(I-1). targets 는 computeBaseline 이 쓰는 것과 같은 집합이므로 이후 덮어써도 값이 같다.
func (r *AcceleratorPartitionPolicyReconciler) journalQuiescing(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, node string, targets []nvidia.MigDevice) error {
	rec := getApplyRecord(acpp, node)
	if !isMigModeEnablePhase(rec.MigPhase) {
		var n corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
			return err
		}
		rec.CordonedByPolicy = !n.Spec.Unschedulable
	}
	rec.NodeName = node
	rec.OwnerUID = string(acpp.UID)
	rec.GPUPCIs = targetPCIs(targets)
	rec.MigPhase = npuv1alpha1.MigPhaseQuiescing
	return r.patchApplyRecord(ctx, acpp, rec)
}

// restoreSchedulable 은 이 정책이 cordon 한 노드만 다시 schedulable 로 되돌린다(멱등).
// 운영자가 미리 cordon 해 둔 노드(CordonedByPolicy=false)는 건드리지 않는다 — 정비 중인 노드를
// 정책이 되살리면 안 된다. 복원 후에는 플래그를 지워 이후 수동 cordon 을 존중한다.
func (r *AcceleratorPartitionPolicyReconciler) restoreSchedulable(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, node string) error {
	if node == "" {
		return nil
	}
	// 예약된 재부팅을 먼저 취소한다 — uncordon 만 하고 Job 을 남기면 방금 스케줄된 워크로드를 얹은
	// 채 노드가 내려간다. 복원은 "이 전환을 포기/완료했다" 는 뜻이므로 재부팅도 함께 거둔다
	// (상한 초과 경로가 Job 을 남기던 누수도 같은 자리에서 닫힌다). 이미 없으면 no-op.
	if err := r.deleteAcppRebootJob(ctx, node); err != nil {
		return err
	}
	rec := getApplyRecord(acpp, node)
	if !rec.CordonedByPolicy {
		return nil
	}
	if err := partition.NvidiaQuiescer(r.Client).Uncordon(ctx, node); err != nil {
		return err
	}
	rec.CordonedByPolicy = false
	return r.patchApplyRecord(ctx, acpp, rec)
}

// ensureAcppRebootJob 은 ACPP 소유 재부팅 Job 을 만든다(이미 있으면 no-op — 재부팅을 두 번 쏘지 않는다).
// 이름은 드라이버 업그레이드 경로와 반드시 달라야 한다(naming.AcppRebootJobName) — 같으면 남의 Job 을
// 자기 것으로 오인해 재부팅을 쏜 적 없이 대기 상태를 영속한다.
//
// 시도 횟수는 Job 생성 **직전에** 증가·영속한다(persist-before-mutate): 재부팅으로 프로세스가 죽어도
// 카운터가 남아야 상한이 의미를 갖는다. 이미 있는 Job 을 재조우할 때는 증가시키지 않는다 —
// 노드가 실제로 내려가기 전 30s requeue 가 몇 번 돌기 때문에 그 경로에서 세면 상한이 즉시 소진된다.
func (r *AcceleratorPartitionPolicyReconciler) ensureAcppRebootJob(t partition.Target, acpp *npuv1alpha1.AcceleratorPartitionPolicy) error {
	name := naming.AcppRebootJobName(t.NodeName)
	var existing batchv1.Job
	err := r.Get(t.Ctx, types.NamespacedName{Name: name, Namespace: driverjob.Namespace}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	image := r.migToolImage()
	if image == "" {
		// 빈 이미지로 Job 을 만들면 API 서버 validation 에러로 뒤늦게 깨진다 — 의도를 담아 먼저 막는다.
		return fmt.Errorf("reboot job for node %s: ACPP_MIG_JOB_IMAGE not configured", t.NodeName)
	}
	rec := getApplyRecord(acpp, t.NodeName)
	rec.NodeName = t.NodeName
	rec.RebootAttempts++
	if err := r.patchApplyRecord(t.Ctx, acpp, rec); err != nil {
		return err
	}
	owner := metav1.OwnerReference{
		APIVersion: "npu.ai/v1alpha1", Kind: "AcceleratorPartitionPolicy", Name: acpp.Name, UID: acpp.UID,
	}
	job := driverjob.RenderRebootJobFor(owner, nil, t.NodeName, image)
	job.Name = name // 렌더러 기본값(드라이버 경로와 공유하는 이름)을 ACPP 전용으로 교체
	if err := r.Create(t.Ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// deleteAcppRebootJob 은 이 노드에 예약된 ACPP 재부팅 Job 을 지운다(없으면 no-op).
// Job 이 아직 pod 을 만들지 않았다면 재부팅 자체가 취소되고, 이미 재부팅한 뒤라면 남은 Job 만 치운다.
func (r *AcceleratorPartitionPolicyReconciler) deleteAcppRebootJob(ctx context.Context, node string) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: naming.AcppRebootJobName(node), Namespace: driverjob.Namespace,
	}}
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// migToolImage 는 nsenter 를 포함한 실행 이미지다 — MIG apply/observe Job 과 같은 소스를 쓴다.
func (r *AcceleratorPartitionPolicyReconciler) migToolImage() string {
	return os.Getenv("ACPP_MIG_JOB_IMAGE")
}
