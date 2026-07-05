// ============================================================
// acceleratorpartitionpolicy_controller.go: ACPP reconciler — 상태머신·phase 집계 (spec §3)
// 상세: 벤더 backend 위임(RNGD full / NVIDIA discovery-only). RNGD apply = DS env 전역.
// 생성일: 2026-07-23 | 수정일: 2026-07-31
// ============================================================
package controller

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition"
	"kcloud-operator/internal/partition/nvidia"
	"kcloud-operator/internal/partition/rngd"
)

const acppFinalizer = "npu.ai/acpp-cleanup"

// migOwnerAnnotation 은 nvidia MIG 소유 노드에 찍는 owner-lock 키다(값 = 소유 ACPP 의 UID, spec §15.1).
const migOwnerAnnotation = "npu.ai/mig-partition-owner"

// nvidiaGPUResource 는 노드 allocatable 의 풀-GPU 리소스명(baseline 계산·expected 구성 공용, goconst 회피).
const nvidiaGPUResource = "nvidia.com/gpu"

// MVP-1: RNGD 는 DS 전역이므로 target = DaemonSet 고정(spec §2.3).
// furiosaUnifiedDSName 은 npuclusterpolicy_controller.go 에 이미 정의된 값을 재사용한다.
const (
	rngdUnifiedDSName = furiosaUnifiedDSName
	rngdUnifiedDSNS   = "kube-system"
)

// AcceleratorPartitionPolicyReconciler reconciles a AcceleratorPartitionPolicy object.
type AcceleratorPartitionPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Verifier partition.Verifier // 하드웨어 검증 seam(nil 이면 verify skip)
	// NvidiaExecFactory 는 nvidia MIG apply 의 Executor 를 만드는 seam 이다(nil → 실 JobExecutor).
	// envtest 는 fake executor 를 주입해 실제 Job 없이 apply 경로를 검증한다.
	NvidiaExecFactory func(client.Client) nvidia.Executor
	// NvidiaObserverFactory 는 operator-driven MIG 관측 seam 이다(nil → 실 MigObserver Job, spec §16.3).
	// envtest 는 fake observer 를 주입해 실제 Job 없이 관측 경로를 검증한다.
	NvidiaObserverFactory func(client.Client) nvidia.Observer
}

// +kubebuilder:rbac:groups=npu.ai,resources=acceleratorpartitionpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=npu.ai,resources=acceleratorpartitionpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=npu.ai,resources=acceleratorpartitionpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=npu.ai,resources=nodedevicereports,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

func (r *AcceleratorPartitionPolicyReconciler) backendFor(vendor string) (partition.Backend, error) {
	switch vendor {
	case vendorFuriosa:
		return rngd.New(r.Client).WithVerifier(r.Verifier), nil
	case vendorNvidia:
		return nvidia.New(r.Client).WithExecutor(r.nvidiaExecutor()).WithVerifier(r.Verifier), nil
	}
	return nil, fmt.Errorf("unknown vendor %q", vendor)
}

// nvidiaExecutor 는 apply/rollback 공용 Executor 를 만든다(seam 우선, 없으면 실 JobExecutor).
func (r *AcceleratorPartitionPolicyReconciler) nvidiaExecutor() nvidia.Executor {
	if r.NvidiaExecFactory != nil {
		return r.NvidiaExecFactory(r.Client)
	}
	return nvidia.NewJobExecutor(r.Client, os.Getenv("ACPP_MIG_JOB_IMAGE"))
}

// nvidiaObserver 는 operator-driven MIG 관측기를 만든다(seam 우선, 없으면 실 MigObserver Job, spec §16.3).
func (r *AcceleratorPartitionPolicyReconciler) nvidiaObserver() nvidia.Observer {
	if r.NvidiaObserverFactory != nil {
		return r.NvidiaObserverFactory(r.Client)
	}
	return nvidia.NewMigObserver(r.Client, nvidia.Namespace, os.Getenv("ACPP_MIG_JOB_IMAGE"))
}

// Reconcile 은 discover→validate→diff→(apply)→verify→Ready 상태머신을 target 별로 실행하고 phase 를 집계한다.
func (r *AcceleratorPartitionPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	var acpp npuv1alpha1.AcceleratorPartitionPolicy
	if err := r.Get(ctx, req.NamespacedName, &acpp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// deletion — Retain: 파티션은 유지, owner lock 해제 + finalizer 제거(handleDeletion, spec §7.3).
	if !acpp.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &acpp)
	}
	if !controllerutil.ContainsFinalizer(&acpp, acppFinalizer) {
		controllerutil.AddFinalizer(&acpp, acppFinalizer)
		if err := r.Update(ctx, &acpp); err != nil {
			return ctrl.Result{}, err
		}
	}

	backend, err := r.backendFor(acpp.Spec.Vendor)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, &acpp, err.Error())
	}

	// 충돌 판정 — MVP-1 RNGD 단일 DS target: DS-UID 충돌/scope 부분집합/vendor 불일치.
	target := partition.Target{Ctx: ctx, NodeName: firstNodeName(ctx, r.Client, acpp.Spec.NodeSelector), Owner: acpp.Name,
		DaemonSetName: rngdUnifiedDSName, DaemonSetNamespace: rngdUnifiedDSNS}

	if win, reason := r.resolveConflict(ctx, &acpp); !win {
		ts := npuv1alpha1.TargetStatus{NodeName: target.NodeName, Phase: npuv1alpha1.ACPPPhaseFailed}
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, reason, "conflict/scope guard", acpp.Generation)
		return ctrl.Result{}, r.writeTargetStatus(ctx, &acpp, ts)
	}

	ts, runErr := r.runTarget(&acpp, backend, target)
	if runErr != nil {
		logger.Error(runErr, "runTarget failed")
	}
	// cordon 복원 단일 choke-point — mode enable(Task 6)이 잠근 노드는 종점에서 반드시 풀려야 한다.
	// runTarget 의 종점은 갈래가 많아(터미널 5 + rollback 후 2) 개별 return 마다 흩뿌리면 새 갈래가
	// 생길 때마다 노드가 unschedulable 로 남는다. terminal 갈래는 재시도해도 결과가 같으므로 영구다.
	// restoreSchedulable 은 멱등이고 CordonedByPolicy=false(외부 cordon)면 no-op 이라 여기서 한 번에 닫는다.
	//
	// 단, rollback + non-nil err = 재시도가 예약된 상태다. GI apply 는 cordon 을 전제로 하고(변경시-전제
	// NodeCordoned) 스스로 cordon 하지 않으므로, 여기서 풀면 다음 reconcile 이 NodeNotCordoned 로 영구
	// 차단되고 CordonedByPolicy 까지 지워져 나중에 성공해도 되돌릴 주체가 없다. verify 실패 rollback 은
	// verr 이 nil 이라 재시도가 없으므로 이 조건에서 복원 대상으로 남는다.
	if runErr == nil || (ts.Phase != npuv1alpha1.ACPPPhaseRollingBack && ts.Phase != npuv1alpha1.ACPPPhaseRollbackFailed) {
		switch ts.Phase {
		case npuv1alpha1.ACPPPhaseFailed, npuv1alpha1.ACPPPhaseUnsupported, npuv1alpha1.ACPPPhaseRollingBack,
			npuv1alpha1.ACPPPhaseRollbackFailed, npuv1alpha1.ACPPPhaseRestored:
			if err := r.restoreSchedulable(ctx, &acpp, target.NodeName); err != nil {
				logger.Error(err, "restore schedulable failed", "node", target.NodeName)
			}
		}
	}
	if err := r.writeTargetStatus(ctx, &acpp, ts); err != nil {
		return ctrl.Result{}, err
	}
	// runTarget 의 non-nil err = transient 실패(Discover/Diff/Apply) → 전파해 requeue(자가치유).
	// Validate 거부·Unsupported 등 terminal 결과는 runTarget 이 nil err + phase 로 반환하므로 requeue 안 함.
	// 단, transient 블록 상태(WaitingForDrain: 외부 cordon/drain 대기, VerifyingAllocatable: 재시도 대기)는
	// runErr 이 nil 이라 기본 resync(~10h)까지 잠들어버린다 → 짧은 RequeueAfter 로 능동 재확인한다(자가치유).
	switch acpp.Status.Phase {
	case npuv1alpha1.ACPPPhaseWaitingForDrain:
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	case npuv1alpha1.ACPPPhaseVerifyingAllocatable:
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	case npuv1alpha1.ACPPPhaseApplying:
		// MIG mode 전환 대기(-mig 1 → pending → 재부팅 → 재관측, Task 6). runErr 가 nil 이라
		// 능동 재확인이 없으면 기본 resync(~10h)까지 잠들어 재부팅 후에도 수렴하지 못한다.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	case npuv1alpha1.ACPPPhaseRestored:
		// 미수렴 sharing 이 원복된 종점. verifier 는 미수렴을 에러가 아니라 결과로 돌려주므로
		// runErr 이 nil 이고, 재확인이 없으면 기본 resync(~10h)까지 여기 눌러앉는다.
		//
		// 간격이 긴 이유: 저널이 원복돼 있어 재진입은 멱등 게이트를 못 타고 처음부터 재적용한다
		// (ConfigMap+DS patch → 전 nvidia 노드 rollout → DP 재시작 → 180s 검증 → 실패 → 원복 rollout).
		// 즉 한 번의 재시도가 클러스터 전역 rollout 2회다 — MIG 경로(VerifyingAllocatable)는 재검증만
		// 하므로 15s 로 짧아도 되지만 여기는 재-mutation 이라 대칭이 성립하지 않는다.
		return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
	}
	return ctrl.Result{}, runErr
}

// shouldReverify 는 재검증 필요 여부를 판정한다(spec §3 evidence 재사용 게이트).
// observedGeneration·resolved config 가 직전과 같고 직전 phase 가 Ready 면 false(재검증 skip).
// 하나라도 다르면(또는 prev 가 없으면) true — 매 reconcile 마다 테스트 Pod 를 새로 만들지 않기 위함(MVP-1).
func (r *AcceleratorPartitionPolicyReconciler) shouldReverify(acpp *npuv1alpha1.AcceleratorPartitionPolicy, prev *npuv1alpha1.TargetStatus) bool {
	if prev == nil || prev.Phase != npuv1alpha1.ACPPPhaseReady {
		return true
	}
	if acpp.Status.ObservedGeneration != acpp.Generation {
		return true
	}
	// backend template hash 변화 감지는 Discover 의 backend.version/observed 로 근사(MVP-1).
	return false
}

// runTarget 은 한 target 에 대해 discover→validate→diff→apply→verify→Ready 상태머신을 실행한다.
func (r *AcceleratorPartitionPolicyReconciler) runTarget(acpp *npuv1alpha1.AcceleratorPartitionPolicy, backend partition.Backend, t partition.Target) (npuv1alpha1.TargetStatus, error) {
	var prev *npuv1alpha1.TargetStatus
	if len(acpp.Status.Targets) > 0 {
		prev = &acpp.Status.Targets[0]
	}
	if !r.shouldReverify(acpp, prev) {
		return *prev, nil // evidence 재사용 — Discover/Diff/Apply/Verify 전부 skip(idempotency).
	}

	ts := npuv1alpha1.TargetStatus{NodeName: t.NodeName, RequestedLayout: acpp.Spec.Layout}

	// 1. Discover
	dres, err := backend.Discover(t)
	if err != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondCapabilitiesDiscovered, metav1.ConditionFalse, "DiscoveryFailed", err.Error(), acpp.Generation)
		return ts, err
	}
	ts.Backend, ts.DriverVersion, ts.Devices = dres.Backend, dres.DriverVersion, dres.Devices
	ts.Operations, ts.Advertisement, ts.ObservedLayout = dres.Operations, dres.Advertisement, dres.Observed
	setCond(&ts, npuv1alpha1.ACPPCondCapabilitiesDiscovered, metav1.ConditionTrue, "DiscoverySucceeded", "capabilities discovered", acpp.Generation)

	// 1.5 nvidia MIG 관측(spec §16.3) — detector 가 못 채우는 MIG mode/geometry/lgip 를 operator
	// observe Job 으로 채워 backend 에 주입·재Discover 한다. Validate 가 관측 기반 supported 를 쓰도록
	// 반드시 Validate 전에 수행한다(관측 실패/미충족은 stop=true 로 조기 반환).
	if acpp.Spec.Vendor == vendorNvidia {
		if nvb, ok := backend.(*nvidia.Backend); ok {
			if stop, oerr := r.observeNvidiaMIG(acpp, backend, nvb, t, &ts); stop {
				return ts, oerr
			}
		}
	}

	// 2. Validate — 매 reconcile 마다 방금 관측한 supported 로 다시 판정한다. mode enable 이 끝나
	// profile 목록이 보이기 시작하면 Task 6 의 형식-only 완화가 자동으로 풀리고, 지원되지 않는
	// profile 요청은 여기서(GI 생성 전에) 거절된다 — -cgi 를 쏴보고 실패하는 경로로 새지 않는다.
	layouts := toPartitionLayouts(acpp.Spec.Layout)
	if len(layouts) == 0 {
		return r.runSharingOnly(acpp, backend, t, ts)
	}
	if err := backend.Validate(layouts); err != nil {
		// mode enable 때문에 우리가 잠근 노드라면 되돌린다 — 만들다 만 GI 는 없고, 검증 거부는
		// 재시도해도 같은 결과라 노드를 계속 묶어둘 이유가 없다(MIG mode 는 모델 B 대로 켠 채 유지).
		if rerr := r.restoreSchedulable(t.Ctx, acpp, t.NodeName); rerr != nil {
			return ts, rerr
		}
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionFalse, "ValidationFailed", err.Error(), acpp.Generation)
		return ts, nil // 검증 거부는 적용 전 종료(criterion 9)
	}
	setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionTrue, "ValidationSucceeded", "layout valid", acpp.Generation)

	// 공유 요청의 결함(미지원 backend / replicas 형식)도 적용 전에 거절한다 — 파티션을 바꿔놓고
	// 나서 거절하면 되돌릴 것만 늘어난다(criterion 9: 검증 거부는 적용 전 종료).
	if _, rejected, ok, perr := r.precheckSharing(t.Ctx, acpp, backend, ts); perr != nil {
		return ts, perr
	} else if !ok {
		return r.rejectSharing(acpp, backend, t, rejected)
	}

	// NVIDIA discovery-only: apply 미지원 → Unsupported(status 만 기록).
	if !ts.Operations.Apply.Supported {
		ts.Phase = npuv1alpha1.ACPPPhaseUnsupported
		return ts, nil
	}

	// nvidia MIG: durable-journal 상태머신(전제 2종 분리·소유 no-diff·baseline). RNGD 는 아래 DS-env 흐름 유지.
	if acpp.Spec.Vendor == vendorNvidia {
		nts, nerr := r.runNvidiaTarget(acpp, backend, t, ts)
		if nerr != nil || nts.Phase != npuv1alpha1.ACPPPhaseReady {
			return nts, nerr
		}
		// 파티션이 Ready 면 mode enable 을 위해 우리가 잠근 노드를 되돌린다(Task 6). GI apply 는
		// cordon 을 요구하므로 여기(성공 종점)가 유일하게 안전한 복원 지점이다. 외부 cordon 은 유지.
		if err := r.restoreSchedulable(t.Ctx, acpp, t.NodeName); err != nil {
			return nts, err
		}
		// 파티션이 Ready 인 뒤에만 공유를 얹는다(Task 4). MIG Ready 로 가는 세 경로(첫 apply/
		// managed no-diff/크래시 복구) 모두 여기를 통과하므로 spec.sharing 만 바뀌어도 반영된다.
		return r.runSharing(acpp, backend, t, nts, expectedAllocatable(getApplyRecord(acpp, t.NodeName)))
	}

	// 3. Resolve + Diff
	resolved, resolvedStatus := resolveRngd(acpp.Spec.Layout)
	ts.ResolvedLayout = resolvedStatus
	diff, err := backend.Diff(t, resolved)
	if err != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		return ts, err
	}

	// 4. Apply (no-diff 면 skip — 재사용/멱등 정교화는 Task 14). happy-path 는 no-diff.
	if diff.Changed {
		rbState, err := backend.Apply(t, resolved)
		if err != nil {
			ts.Phase = npuv1alpha1.ACPPPhaseFailed
			setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, "ApplyFailed", err.Error(), acpp.Generation)
			return ts, err
		}
		ts.Backend.Rollback = npuv1alpha1.RollbackInfo{
			PrevPolicy: rbState.PrevPolicy, Generation: rbState.Generation,
			ResourceVersion: rbState.ResourceVersion, TemplateHash: rbState.TemplateHash,
		}
		msg := fmt.Sprintf("policy %s applied (changed=%v)", diff.ToPolicy, diff.Changed)
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionTrue, "DevicePluginConfigured", msg, acpp.Generation)

		// 5. Verify — 실패/에러 시 rollback(spec §3: RollingBack → Failed/PreviousConfigurationRestored, Ready 로 승격 안 함).
		vr, verr := backend.Verify(t)
		if !verifySucceeded(vr, verr) {
			ts.Phase = npuv1alpha1.ACPPPhaseRollingBack
			if rerr := backend.Rollback(t, *rbState); rerr != nil {
				ts.Phase = npuv1alpha1.ACPPPhaseFailed
				setCond(&ts, npuv1alpha1.ACPPCondRolledBack, metav1.ConditionFalse, npuv1alpha1.ACPPPhaseRollbackFailed, rerr.Error(), acpp.Generation)
				return ts, rerr
			}
			ts.Phase = npuv1alpha1.ACPPPhaseFailed // PreviousConfigurationRestored = Failed(Ready 아님)
			setCond(&ts, npuv1alpha1.ACPPCondRolledBack, metav1.ConditionTrue, "RollbackSucceeded", "restored previous policy", acpp.Generation)
			return ts, nil
		}
	} else {
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionTrue, "DevicePluginConfigured", "no-diff; policy already applied (changed=false)", acpp.Generation)
		// no-diff 여도 verify 는 수행하며, error 뿐 아니라 all-false 결과도 실패로 처리한다.
		// (RollbackFailed 후 requeue 시 DS 가 이미 applied 값이라 no-diff 로 진입 → 여기서 all-false 를
		//  Ready 로 잘못 승격하면 수동개입 필요 신호가 사라진다. rollback state 는 없으므로 Failed 로만 표시.)
		vr, verr := backend.Verify(t)
		if !verifySucceeded(vr, verr) {
			ts.Phase = npuv1alpha1.ACPPPhaseFailed
			setCond(&ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionFalse, "VerificationFailed", "no-diff verify failed; manual intervention may be required", acpp.Generation)
			return ts, verr
		}
	}
	setCond(&ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionTrue, "AllocationTestSucceeded", "test pod allocated", acpp.Generation)
	ts.Phase = npuv1alpha1.ACPPPhaseReady
	// owner-lock 확정 — changed-apply 경로는 backend.Apply 가 이미 찍지만, no-diff 채택 경로는
	// 여기서만 찍힌다(finding #1: no-diff happy-path 도 소유권을 주장해야 NCP 재탈환을 막는다).
	if acpp.Spec.Vendor == vendorFuriosa {
		r.ensureOwnerLock(t.Ctx, acpp)
	}
	// 공유 요청은 여기서도 통과시킨다 — 미지원 backend 는 조용히 무시하지 않고 Unsupported 로 보고한다.
	return r.runSharing(acpp, backend, t, ts, ts.Advertisement.AdvertisedResources)
}

// runNvidiaTarget 은 nvidia MIG apply 의 durable-journal 상태머신이다(spec §15.1 전제분리 + §15.2 journal).
// 전제는 2종(항상: PCI·관측·driver / 변경시: cordon·idle·MIG-safe)으로 분리되며, 소유+Ready+geometry 일치
// 는 no-diff 로 재검증만 하고(변경시-전제 불요구), mutation 은 반드시 ApplyRecord(Applying) 영속 후에만 한다.
func (r *AcceleratorPartitionPolicyReconciler) runNvidiaTarget(acpp *npuv1alpha1.AcceleratorPartitionPolicy, backend partition.Backend, t partition.Target, ts npuv1alpha1.TargetStatus) (npuv1alpha1.TargetStatus, error) {
	nvb, ok := backend.(*nvidia.Backend)
	if !ok {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, "ApplyFailed", "nvidia backend type assertion failed", acpp.Generation)
		return ts, nil
	}
	// MIG 관측(spec §16.3)은 runTarget 이 Validate 전에 이미 수행해 backend 에 주입/재Discover 했다.

	// (1) 항상-전제(어느 상태서나).
	if reason, msg, blocked := nvidiaAlwaysBlock(nvidia.CheckAlways(nvb.Targets(), ts.DriverVersion != "")); blocked {
		ts.Phase = npuv1alpha1.ACPPPhaseWaitingForDrain
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, reason, msg, acpp.Generation)
		return ts, nil
	}

	// (2) owner-lock 선취득(UID) — Diff/Apply 이전에 소유를 확정한다.
	owned, err := r.ensureNodeOwnerLockUID(t.Ctx, t.NodeName, string(acpp.UID))
	if err != nil {
		return ts, err
	}
	if !owned {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, npuv1alpha1.ReasonTargetConflict, "node MIG owned by another ACPP", acpp.Generation)
		return ts, nil
	}

	// (2.5) MIG mode enable(Task 6) — mode 가 꺼져 있으면 GI 생성 전에 quiesce→-mig 1→reboot 를 완주한다.
	// Diff 이전에 두는 이유: mode 가 Disabled 인 동안의 geometry 비교는 의미가 없고, 무엇보다 전환
	// 중(mode-enable phase) 저널이 아래 !diff.Changed 분류를 통과하면 ExistingMigConfiguration 으로
	// 떨어져 자가치유 없이 정책이 굳는다. 이미 Enabled 인 노드는 즉시 done 이라 기존 경로는 무회귀.
	if done, out, merr := r.ensureMigModeEnabled(acpp, t, nvb.Targets(), ts); merr != nil || !done {
		return out, merr
	}
	// 저널은 ensureMigModeEnabled 가 방금 갱신했을 수 있다(전환 완료 시 mode-전환 phase 제거).
	// 그 이전 스냅샷으로 아래 no-diff 를 분류하면 이미 끝난 전환을 진행 중으로 오인해, geometry 가
	// 일치하는 경우 수렴도 실패도 없이 WaitingForDrain 을 반복한다(노드는 cordon 된 채).
	rec := getApplyRecord(acpp, t.NodeName)

	// (3) Resolve + Diff + 소유 no-diff 술어(§14.2).
	resolved, resolvedStatus := resolveNvidia(acpp.Spec.Layout)
	ts.ResolvedLayout = resolvedStatus
	t.Generation = acpp.Generation
	diff, derr := backend.Diff(t, resolved)
	if derr != nil {
		return ts, derr
	}
	if handled, out, herr := r.routeNvidiaNoDiff(acpp, backend, nvb, t, ts, rec, resolved, diff.Changed); handled {
		return out, herr
	}

	// (3.5) 같은 generation 의 rollback 은 terminal 이다. verify 미수렴은 에러가 아니라 결과로
	// 돌아오므로(liveVerifier: 180s 초과 → converged=false, err=nil) rollback 종점의 runErr 이 nil 이고
	// rate-limited 재시도도 걸리지 않는다. 그 상태로 재진입하면 아래 cordon 게이트가 노드를 다시 잠그고
	// GI 를 다시 만들어 또 부순다 — 실물 파티션 생성·파괴와 device-plugin 재시작이 무한 왕복한다.
	// 입력(spec)이 그대로면 결과도 그대로이므로 여기서 끝내고 spec 변경을 기다린다(노드는 종점
	// choke-point 가 풀어 준다). 재시도 경로는 generation 증가뿐이다.
	if rec.MigPhase == npuv1alpha1.MigPhaseRolledBack && rec.Generation == acpp.Generation {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, "ApplyRolledBack",
			"apply rolled back for this generation; update spec to retry", acpp.Generation)
		return ts, nil
	}

	// (4) hardware 변경 전제 — 진행 중(applyInFlight) 복구는 우리가 만든 Enabled MIG 상태로 차단하면 안 되므로 skip.
	// mode 전환 저널만 남은 경우는 skip 하지 않는다 — GI 를 만들기 전이므로 cordon·idle 을 다시 확인해야 한다.
	if !isApplyInFlightPhase(rec.MigPhase) {
		hw, herr := nvidia.CheckHardwareChange(t.Ctx, r.Client, t.NodeName, nvb.Targets())
		if herr != nil {
			return ts, herr
		}
		// cordon 주체는 정책이다. mode 가 이미 Enabled 인 노드는 유일한 cordon 주체였던
		// ensureMigModeEnabled 를 건너뛰므로, 여기서 cordon 을 남에게 맡기면 오지 않는 외부 주체를
		// 기다리며 WaitingForDrain 을 영원히 반복한다(U-1). 진행 가능한 상태(mode Enabled + 관리 밖
		// GI 없음)에서 cordon 만 없다면 스스로 잠근다. 배출은 하지 않으므로 GPU 를 쥔 pod 가 남아
		// 있으면 아래 idle 전제가 그대로 걸러 "배출 대기" 로 정직하게 보고된다 — 달라지는 것은
		// "누군가 cordon 해 주기를 기다리는" 상태가 더는 없다는 점이다.
		if !hw.NodeCordoned && !hw.MigModeNotEnabled && !hw.MigUnsafe {
			if cerr := r.cordonForApply(t.Ctx, acpp, t.NodeName, nvb.Targets()); cerr != nil {
				return ts, cerr
			}
			hw.NodeCordoned = true
		}
		if reason, msg, blocked := nvidiaHWBlock(hw); blocked {
			ts.Phase = npuv1alpha1.ACPPPhaseWaitingForDrain
			setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, reason, msg, acpp.Generation)
			return ts, nil
		}
	}

	// (5) persist-before-mutate: baseline 계산 → ApplyRecord(Applying) 을 mutation 이전에 영속(crash-safety).
	newRec, berr := r.computeBaseline(t.Ctx, t.NodeName, acpp, resolved, nvb.Targets())
	if berr != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, npuv1alpha1.ReasonBaselineInconsistent, berr.Error(), acpp.Generation)
		return ts, nil
	}
	newRec.MigPhase = npuv1alpha1.MigPhaseApplying
	if err := r.patchApplyRecord(t.Ctx, acpp, newRec); err != nil {
		return ts, err // 영속 실패 시 mutation 하지 않는다(복원 대상 PCI 가 durable 하지 않으면 위험).
	}
	if err := r.syncMigActiveLabel(t.Ctx, t.NodeName, true); err != nil {
		return ts, err
	}

	// (6) Apply → 실패 시 rollback(rb 는 부분적용 후에도 non-nil).
	nvb.WithExpected(expectedAllocatable(newRec))
	rb, aerr := backend.Apply(t, resolved)
	if aerr != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseRollingBack
		if rb != nil {
			_ = backend.Rollback(t, *rb)
		}
		_ = r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseRolledBack)
		_ = r.syncMigActiveLabel(t.Ctx, t.NodeName, false)
		setCond(&ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, "ApplyFailed", aerr.Error(), acpp.Generation)
		return ts, aerr
	}
	_ = r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseApplied)

	// (7) device-plugin 재시작 → Verify → Ready.
	ts.Phase = npuv1alpha1.ACPPPhaseRestartingDevicePlugin
	if err := r.restartNvidiaDevicePlugin(t.Ctx, t.NodeName); err != nil {
		return ts, err
	}
	_ = r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseVerifying)
	vr, verr := backend.Verify(t)
	if !verifySucceeded(vr, verr) {
		ts.Phase = npuv1alpha1.ACPPPhaseRollingBack
		if rb != nil {
			_ = backend.Rollback(t, *rb)
		}
		_ = r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseRolledBack)
		_ = r.syncMigActiveLabel(t.Ctx, t.NodeName, false)
		setCond(&ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionFalse, "VerificationFailed", "mig verify failed", acpp.Generation)
		return ts, verr
	}
	_ = r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseReady)
	setNvidiaReady(&ts, acpp)
	return ts, nil
}

// isApplyInFlightPhase 는 GI mutation 이 실제로 진행 중인 저널 phase 인지다(baseline 영속 이후).
// 변경시-전제(cordon·idle) skip 은 이 구간에만 허용한다 — 우리가 만든 Enabled MIG 상태로 복구를
// 차단하면 안 되기 때문이다. mode 전환 구간은 GI 이전이라 여기 포함되지 않는다(전제 재확인 필요).
func isApplyInFlightPhase(phase string) bool {
	switch phase {
	case npuv1alpha1.MigPhaseApplying, npuv1alpha1.MigPhaseApplied,
		npuv1alpha1.MigPhaseRestartingDP, npuv1alpha1.MigPhaseVerifying:
		return true
	}
	return false
}

// routeNvidiaNoDiff 는 geometry 일치(no-diff) 상태의 세 갈래를 라우팅한다(§14.2 소유 술어).
// handled=false 면 호출자가 정상 apply 경로를 계속한다.
func (r *AcceleratorPartitionPolicyReconciler) routeNvidiaNoDiff(acpp *npuv1alpha1.AcceleratorPartitionPolicy, backend partition.Backend, nvb *nvidia.Backend, t partition.Target, ts npuv1alpha1.TargetStatus, rec npuv1alpha1.ApplyRecord, resolved []partition.ResolvedEntry, changed bool) (bool, npuv1alpha1.TargetStatus, error) {
	if changed {
		return false, ts, nil
	}
	owned := rec.OwnerUID == string(acpp.UID)
	if rec.MigPhase == npuv1alpha1.MigPhaseReady && owned && recMatches(rec, resolved, nvb.Targets()) {
		// 이미 적용 완료·소유·geometry 일치 → 재검증만(변경시-전제 불요구, cordon/Disabled 요구 안 함).
		out, err := r.verifyManagedNvidia(acpp, backend, nvb, t, ts, rec)
		return true, out, err
	}
	if owned && isApplyInFlightPhase(rec.MigPhase) {
		// crash-recovery continuation — 우리(this ACPP)가 만든 in-flight apply 인데 hardware mutation 은 이미
		// 끝나 geometry 가 일치(!diff.Changed)한다. baseline 재계산 시 drop 된 allocatable 로 BaselineInconsistent
		// (terminal deadlock)가 되므로, 영속 record 로 상태머신 tail 만 재개한다(resumeNvidiaRecovery).
		out, err := r.resumeNvidiaRecovery(acpp, backend, nvb, t, ts, rec)
		return true, out, err
	}
	if isMigModeEnablePhase(rec.MigPhase) {
		// GI 이전 저널(mode 전환 또는 apply 직전 cordon)이 남은 채 geometry 만 일치 — baseline 이
		// 없어 recovery tail 을 탈 수 없다.
		// 여기서 아래 ExistingMigConfiguration 으로 떨어뜨리면 자가치유 없는 terminal 실패가 되므로
		// (Task 4 교훈), 정상 경로로 내려보내 변경시-전제부터 다시 밟게 한다(전부 transient·requeue).
		return false, ts, nil
	}
	// 외부 수동 설정과 geometry 만 우연히 일치 — 이 ACPP 가 만든 것이 아니므로 채택 거부.
	ts.Phase = npuv1alpha1.ACPPPhaseFailed
	setCond(&ts, npuv1alpha1.ACPPCondValidated, metav1.ConditionFalse, npuv1alpha1.ReasonExistingMigConfiguration, "geometry matches but not managed by this ACPP", acpp.Generation)
	return true, ts, nil
}

// observeNvidiaMIG 는 detector 가 못 채우는 MIG mode/geometry/lgip 를 operator observe Job 으로 fresh
// 관측해 backend 에 주입하고 Discover 를 재실행한다(spec §16.3). 캐시 없이 매 reconcile 관측(MVP) —
// apply 직후 다음 reconcile 이 새 geometry 를 즉시 반영해 no-diff 수렴. 관측 Job 실패는 fail-closed
// (WaitingForDrain). 반환 stop=true 면 호출자는 ts(+err) 로 즉시 종료한다.
func (r *AcceleratorPartitionPolicyReconciler) observeNvidiaMIG(acpp *npuv1alpha1.AcceleratorPartitionPolicy, backend partition.Backend, nvb *nvidia.Backend, t partition.Target, ts *npuv1alpha1.TargetStatus) (bool, error) {
	// 모든 nvidia 장치 PCI 에서 관측한다 — detector 가 model 을 "generic" 으로만 보고해
	// migCapable(model) 로는 A30 을 못 가리므로, MIG-capability 는 관측(lgip profiles)으로 판정한다.
	// ts.Devices 는 첫 Discover 가 채운 전체 nvidia 장치 목록이다.
	var pcis []string
	for _, d := range ts.Devices {
		if d.PCIAddress != "" {
			pcis = append(pcis, d.PCIAddress)
		}
	}
	if len(pcis) == 0 {
		return false, nil
	}
	obs, oerr := r.nvidiaObserver().Observe(t.Ctx, t.NodeName, pcis)
	if oerr != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseWaitingForDrain
		setCond(ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse, npuv1alpha1.ReasonObservationUnavailable, "mig observation unavailable: "+oerr.Error(), acpp.Generation)
		return true, nil
	}
	obsMap := make(map[string]nvidia.Observation, len(obs))
	for _, o := range obs {
		obsMap[o.PCI] = o
	}
	nvb.WithObservations(obsMap)
	dres, derr := backend.Discover(t)
	if derr != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed
		setCond(ts, npuv1alpha1.ACPPCondCapabilitiesDiscovered, metav1.ConditionFalse, "DiscoveryFailed", derr.Error(), acpp.Generation)
		return true, derr
	}
	ts.DriverVersion, ts.Devices = dres.DriverVersion, dres.Devices
	ts.Operations, ts.Advertisement = dres.Operations, dres.Advertisement
	return false, nil
}

// verifyManagedNvidia 는 소유·Ready·geometry 일치(managed no-diff)의 재검증만 수행한다. 실 에러는 전파,
// non-convergence(에러 아님)는 hard-fail 대신 transient(VerifyingAllocatable)로 두고 requeue 해 self-heal 한다.
func (r *AcceleratorPartitionPolicyReconciler) verifyManagedNvidia(acpp *npuv1alpha1.AcceleratorPartitionPolicy, backend partition.Backend, nvb *nvidia.Backend, t partition.Target, ts npuv1alpha1.TargetStatus, rec npuv1alpha1.ApplyRecord) (npuv1alpha1.TargetStatus, error) {
	nvb.WithExpected(expectedAllocatable(rec))
	vr, verr := backend.Verify(t)
	if verr != nil {
		ts.Phase = npuv1alpha1.ACPPPhaseFailed // 실 에러는 rate-limited retry 로 전파.
		setCond(&ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionFalse, "VerificationFailed", "managed mig verify failed", acpp.Generation)
		return ts, verr
	}
	if !verifySucceeded(vr, nil) {
		// non-convergence — plugin 이 잠시 재시작 중일 수 있다. Ready 였던 노드가 mid-restart 로 stuck 되지 않게 재시도.
		ts.Phase = npuv1alpha1.ACPPPhaseVerifyingAllocatable
		setCond(&ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionFalse, "VerificationPending", "managed mig allocatable not yet converged; retrying", acpp.Generation)
		return ts, nil
	}
	setNvidiaReady(&ts, acpp)
	return ts, nil
}

// resumeNvidiaRecovery 는 우리(this ACPP)의 in-flight apply 가 hardware mutation 이후 크래시했을 때
// (geometry 이미 일치) baseline 재계산·재-Apply 없이 영속 record 로 상태머신 tail(DP 재시작→Verify)만
// 재개한다. computeBaseline 을 우회해 drop 된 allocatable 로 인한 BaselineInconsistent 데드락을 막는다.
func (r *AcceleratorPartitionPolicyReconciler) resumeNvidiaRecovery(acpp *npuv1alpha1.AcceleratorPartitionPolicy, backend partition.Backend, nvb *nvidia.Backend, t partition.Target, ts npuv1alpha1.TargetStatus, rec npuv1alpha1.ApplyRecord) (npuv1alpha1.TargetStatus, error) {
	nvb.WithExpected(expectedAllocatable(rec))
	ts.Phase = npuv1alpha1.ACPPPhaseRestartingDevicePlugin
	if rec.MigPhase == npuv1alpha1.MigPhaseApplied {
		// Applied 에서 크래시 → DP 재시작이 아직 안 됐을 수 있다(멱등: 이미 됐어도 안전).
		if err := r.restartNvidiaDevicePlugin(t.Ctx, t.NodeName); err != nil {
			return ts, err
		}
	}
	_ = r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseVerifying)
	vr, verr := backend.Verify(t)
	if !verifySucceeded(vr, verr) {
		ts.Phase = npuv1alpha1.ACPPPhaseRollingBack
		_ = backend.Rollback(t, partition.RollbackState{}) // Rollback 은 target PCI 로 disable(state 무시).
		_ = r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseRolledBack)
		setCond(&ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionFalse, "VerificationFailed", "recovery verify failed", acpp.Generation)
		return ts, verr
	}
	_ = r.setRecordPhase(t.Ctx, acpp, t.NodeName, npuv1alpha1.MigPhaseReady)
	setNvidiaReady(&ts, acpp)
	return ts, nil
}

// resolveNvidia 는 nvidia layout 을 resolve 한다 — profile 은 그대로, BackendPolicy 는 없음(§4.2 ProfileNamed).
func resolveNvidia(ls []npuv1alpha1.PartitionLayout) ([]partition.ResolvedEntry, []npuv1alpha1.ResolvedLayoutEntry) {
	re := make([]partition.ResolvedEntry, 0, len(ls))
	rs := make([]npuv1alpha1.ResolvedLayoutEntry, 0, len(ls))
	for _, l := range ls {
		re = append(re, partition.ResolvedEntry{Profile: l.Profile, ExpectedCountPerDevice: l.CountPerDevice})
		rs = append(rs, npuv1alpha1.ResolvedLayoutEntry{Profile: l.Profile, ExpectedCountPerDevice: l.CountPerDevice})
	}
	return re, rs
}

// nvidiaAlwaysBlock 은 항상-전제(PCI·관측·driver) 위반을 (reason,msg,blocked) 로 매핑한다.
func nvidiaAlwaysBlock(a nvidia.AlwaysResult) (string, string, bool) {
	switch {
	case a.ObservationFailed:
		return npuv1alpha1.ReasonObservationUnavailable, "mig observation unavailable", true
	case a.PCIMissing:
		return npuv1alpha1.ReasonUnstableGpuIdentity, "gpu pci address missing", true
	case !a.DriverReady:
		return npuv1alpha1.ReasonDriverNotReady, "nvidia driver not ready", true
	}
	return "", "", false
}

// nvidiaHWBlock 은 hardware 변경시-전제(cordon·MIG-safe·idle) 위반을 (reason,msg,blocked) 로 매핑한다.
// 순서 주의: 장치 상태(mode·관리 밖 GI)를 cordon 보다 먼저 본다. 정책은 그 두 경우에 노드를 cordon
// 하지 않기로 되어 있으므로(진행 불가한 노드를 잠글 이유가 없다), cordon 을 먼저 보고하면 아무도 하지
// 않을 조치를 지시하는 거짓 이유가 된다. cordon 은 정책이 스스로 취득하므로 !NodeCordoned 는
// "cordon 이 먹지 않았다" 는 좁은 뜻으로만 남는다.
func nvidiaHWBlock(h nvidia.HWResult) (string, string, bool) {
	switch {
	case h.MigModeNotEnabled:
		// 모델 B(§17): MIG mode 는 외부 사전조건. 미enable 이면 차단·requeue(노드 enable 후 자가치유).
		return npuv1alpha1.ReasonMigModeNotEnabled, "MIG mode must be pre-enabled on the target GPU", true
	case h.MigUnsafe:
		return npuv1alpha1.ReasonExistingMigConfiguration, "existing/unsafe mig state present", true
	case !h.NodeCordoned:
		return npuv1alpha1.ReasonNodeNotCordoned, "node not cordoned", true
	case h.GPUBusy:
		return npuv1alpha1.ReasonQuiesceRequired, "gpu workloads still running", true
	}
	return "", "", false
}

// ensureNodeOwnerLockUID 는 노드 annotation(migOwnerAnnotation)=uid 를 멱등 취득한다.
// 이미 다른 uid 로 잠겨 있으면 (false,nil), 취득/재확인 성공은 (true,nil). patch 충돌은 재시도한다.
func (r *AcceleratorPartitionPolicyReconciler) ensureNodeOwnerLockUID(ctx context.Context, node, uid string) (bool, error) {
	for i := 0; i < 3; i++ {
		var n corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
			return false, err
		}
		switch cur := n.Annotations[migOwnerAnnotation]; {
		case cur == uid:
			return true, nil
		case cur != "":
			return false, nil
		}
		// annotation 하나만 merge-patch — 동시 Node writer(라벨/컨디션 등)를 clobber 하지 않는다.
		base := n.DeepCopy()
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[migOwnerAnnotation] = uid
		if err := r.Patch(ctx, &n, client.MergeFrom(base)); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("owner lock: conflict retries exhausted for node %s", node)
}

// restartNvidiaDevicePlugin 은 대상 노드의 nvidia-device-plugin pod 를 삭제해 재-스캔을 유도한다(§15.2).
func (r *AcceleratorPartitionPolicyReconciler) restartNvidiaDevicePlugin(ctx context.Context, node string) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.MatchingLabels{nvidiaDevicePluginVendorLabel: "nvidia"},
		client.MatchingFields{"spec.nodeName": node}); err != nil {
		return err
	}
	for i := range pods.Items {
		if err := r.Delete(ctx, &pods.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// computeBaseline 은 apply 전 노드 상태에서 baseline·기대 allocatable 을 계산해 ApplyRecord 를 만든다(§15.2).
// 모델 B(§17.1): MIG-enabled GPU 는 이미 nvidia.com/gpu 에서 빠져 있으므로 ExpectedFullGPUCount =
// BaselineGPUCount(빼지 않음). physical-count 대조 검사는 제거(MIG 사전enable시 allocatable ≠ physical).
func (r *AcceleratorPartitionPolicyReconciler) computeBaseline(ctx context.Context, node string, acpp *npuv1alpha1.AcceleratorPartitionPolicy, resolved []partition.ResolvedEntry, targets []nvidia.MigDevice) (npuv1alpha1.ApplyRecord, error) {
	var rec npuv1alpha1.ApplyRecord
	if len(resolved) != 1 {
		return rec, fmt.Errorf("baseline: expected single resolved layout, got %d", len(resolved))
	}
	var n corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		return rec, err
	}
	q := n.Status.Allocatable[corev1.ResourceName(nvidiaGPUResource)]
	baseline := int32(q.Value())
	nt := int32(len(targets))
	r0 := resolved[0]
	// 기존 저널을 승계한 뒤 baseline 항목만 덮어쓴다 — 통째로 새 record 를 만들면 baseline 과
	// 무관한 필드(cordonedByPolicy, sharingMode/Replicas)가 조용히 사라진다. cordon 소유권을
	// 잃으면 mode enable 로 잠근 노드를 Ready 이후에도 삭제 시에도 되돌리지 못한다.
	rec = getApplyRecord(acpp, node)
	rec.NodeName = node
	rec.GPUPCIs = targetPCIs(targets)
	rec.OwnerUID = string(acpp.UID)
	rec.BaselineGPUCount = baseline
	rec.ExpectedMigCount = r0.ExpectedCountPerDevice * nt
	rec.ExpectedFullGPUCount = baseline
	rec.Profile = r0.Profile
	rec.Count = r0.ExpectedCountPerDevice
	rec.Generation = acpp.Generation
	return rec, nil
}

// targetPCIs 는 target 장치들의 PCI 목록이다 — 저널의 GPUPCIs 는 소유 주장 시점(journalQuiescing)과
// baseline 시점(computeBaseline)에 각각 쓰이므로, 같은 유도식을 공유해야 두 write 가 같은 값을 낸다.
func targetPCIs(targets []nvidia.MigDevice) []string {
	pcis := make([]string, 0, len(targets))
	for _, d := range targets {
		pcis = append(pcis, d.PCI)
	}
	return pcis
}

// expectedAllocatable 은 Verify 가 검증할 기대 allocatable 맵(mig + full-gpu)을 만든다.
func expectedAllocatable(rec npuv1alpha1.ApplyRecord) map[string]int32 {
	return map[string]int32{
		"nvidia.com/mig-" + rec.Profile: rec.ExpectedMigCount,
		nvidiaGPUResource:               rec.ExpectedFullGPUCount,
	}
}

// recMatches 는 저장된 ApplyRecord 가 요청 resolved + 현 targets(PCI 집합)와 일치하는지(소유 no-diff 술어)다.
func recMatches(rec npuv1alpha1.ApplyRecord, resolved []partition.ResolvedEntry, targets []nvidia.MigDevice) bool {
	if len(resolved) != 1 {
		return false
	}
	if rec.Profile != resolved[0].Profile || rec.Count != resolved[0].ExpectedCountPerDevice {
		return false
	}
	if len(rec.GPUPCIs) != len(targets) {
		return false
	}
	have := make(map[string]bool, len(rec.GPUPCIs))
	for _, p := range rec.GPUPCIs {
		have[p] = true
	}
	for _, d := range targets {
		if !have[d.PCI] {
			return false
		}
	}
	return true
}

// getApplyRecord 는 node 의 ApplyRecord 를 반환한다(없으면 zero value).
func getApplyRecord(acpp *npuv1alpha1.AcceleratorPartitionPolicy, node string) npuv1alpha1.ApplyRecord {
	for _, r := range acpp.Status.ApplyRecords {
		if r.NodeName == node {
			return r
		}
	}
	return npuv1alpha1.ApplyRecord{}
}

// patchApplyRecord 는 rec 를 status.ApplyRecords 에 upsert 하고 Status().Patch 로 영속한다(durable journal).
func (r *AcceleratorPartitionPolicyReconciler) patchApplyRecord(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, rec npuv1alpha1.ApplyRecord) error {
	base := acpp.DeepCopy()
	found := false
	for i := range acpp.Status.ApplyRecords {
		if acpp.Status.ApplyRecords[i].NodeName == rec.NodeName {
			acpp.Status.ApplyRecords[i] = rec
			found = true
			break
		}
	}
	if !found {
		acpp.Status.ApplyRecords = append(acpp.Status.ApplyRecords, rec)
	}
	return r.Status().Patch(ctx, acpp, client.MergeFrom(base))
}

// setRecordPhase 는 node 의 ApplyRecord.MigPhase 만 갱신해 영속한다(상태 진행 저널링).
func (r *AcceleratorPartitionPolicyReconciler) setRecordPhase(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, node, phase string) error {
	rec := getApplyRecord(acpp, node)
	rec.NodeName = node
	rec.MigPhase = phase
	return r.patchApplyRecord(ctx, acpp, rec)
}

// setNvidiaReady 는 nvidia MIG apply 성공 종점(Ready + Applied/Verified True)을 찍는다.
func setNvidiaReady(ts *npuv1alpha1.TargetStatus, acpp *npuv1alpha1.AcceleratorPartitionPolicy) {
	ts.Phase = npuv1alpha1.ACPPPhaseReady
	setCond(ts, npuv1alpha1.ACPPCondApplied, metav1.ConditionTrue, "DevicePluginConfigured", "mig geometry applied", acpp.Generation)
	setCond(ts, npuv1alpha1.ACPPCondVerified, metav1.ConditionTrue, "AllocatableConverged", "mig allocatable converged", acpp.Generation)
}

// ensureOwnerLock 은 unified DS 의 owner-lock annotation 이 이 acpp 소유로 찍혀 있음을 보장하는 멱등 헬퍼다
// (Task 9 두 writer 조정). DS 부재(NotFound)는 조용히 건너뛴다(관리할 lock 이 없음). 그 외 에러는
// reconcile 자체를 실패시키지 않고 로그만 남긴다 — status 는 이미 Ready 로 확정된 뒤이므로.
func (r *AcceleratorPartitionPolicyReconciler) ensureOwnerLock(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy) {
	logger := logf.FromContext(ctx)
	var ds appsv1.DaemonSet
	if err := r.Get(ctx, client.ObjectKey{Name: rngdUnifiedDSName, Namespace: rngdUnifiedDSNS}, &ds); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "ensureOwnerLock: get DS failed")
		}
		return
	}
	if ds.Annotations[rngd.PartitionOwnerAnnotation] == acpp.Name {
		return
	}
	if ds.Annotations == nil {
		ds.Annotations = map[string]string{}
	}
	ds.Annotations[rngd.PartitionOwnerAnnotation] = acpp.Name
	if err := r.Update(ctx, &ds); err != nil {
		logger.Error(err, "ensureOwnerLock: update DS failed")
	}
}

// verifySucceeded 는 backend.Verify 결과를 성공/실패로 판별한다(양 분기 공통 규칙).
// error → 실패. nil result(verifier 미주입 skip 신호) → 성공(검증 불요).
// non-nil result → AllocatableConverged 또는 TestPodAllocated 중 하나라도 true 면 성공, 모두 false 면 실패.
func verifySucceeded(vr *partition.VerifyResult, err error) bool {
	if err != nil {
		return false
	}
	if vr == nil {
		return true
	}
	return vr.AllocatableConverged || vr.TestPodAllocated
}

// resolveConflict 는 이 acpp 가 target(DS) 에 적용해도 되는지 판정한다.
// 순서: 1) vendor 불일치(NDR 실제 벤더≠spec.vendor) 2) scope 부분집합(selector 가 DS 관리 노드 전체를 못 덮음)
// 3) 같은 vendor 로 같은(MVP-1 단일) DS 를 노리는 ACPP 중 결정적 승자(creationTimestamp 최소 → 동률 시 name 사전순)인지.
// 비-승자/불일치는 (false, reason) 을 반환하고 runTarget 은 호출되지 않는다.
func (r *AcceleratorPartitionPolicyReconciler) resolveConflict(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy) (bool, string) {
	nodeName := firstNodeName(ctx, r.Client, acpp.Spec.NodeSelector)
	if nodeName != "" {
		var ndr npuv1alpha1.NodeDeviceReport
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &ndr); err == nil && len(ndr.Status.Devices) > 0 {
			// 멀티벤더 노드(예: worker1 = nvidia A30/A2 + Warboy) 지원: 첫 장치가 아니라
			// spec.vendor 장치가 하나라도 존재하는지로 판정한다(Devices[0] 단독 검사는 오분류).
			found := false
			for _, d := range ndr.Status.Devices {
				if d.Vendor == acpp.Spec.Vendor {
					found = true
					break
				}
			}
			if !found {
				return false, npuv1alpha1.ReasonVendorMismatch
			}
		}
	}

	// DS 기반 scope/충돌 판정은 RNGD(furiosa) 전용 — DS 에 apply 하는 backend 만 해당(spec §2.3).
	// NVIDIA 등 discovery-only 벤더는 대상 DS 가 없어 scope·충돌 개념이 없다 → 통과시켜
	// runTarget 이 Unsupported 로 처리하게 한다(Task 12 동작 보존, 혼합 벤더 오분류 방지).
	if acpp.Spec.Vendor != vendorFuriosa {
		return true, ""
	}

	if dsNodes, err := r.dsManagedNodes(ctx); err == nil && !selectorCoversAll(ctx, r.Client, acpp.Spec.NodeSelector, dsNodes) {
		return false, npuv1alpha1.ReasonBackendScopeMismatch
	}

	var all npuv1alpha1.AcceleratorPartitionPolicyList
	if err := r.List(ctx, &all); err != nil {
		return true, "" // 목록 실패 시 보수적으로 진행
	}
	var contenders []npuv1alpha1.AcceleratorPartitionPolicy
	for _, o := range all.Items {
		if o.Spec.Vendor == acpp.Spec.Vendor && o.DeletionTimestamp.IsZero() {
			contenders = append(contenders, o)
		}
	}
	sort.Slice(contenders, func(i, j int) bool {
		a, b := contenders[i], contenders[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Name < b.Name
	})
	if len(contenders) > 0 && contenders[0].Name != acpp.Name {
		return false, npuv1alpha1.ReasonTargetConflict
	}
	return true, ""
}

// rngdPresentNodeLabel 은 node-manager 가 RNGD 하드웨어 보유 노드에 부여하는 자립 라벨이다.
const rngdPresentNodeLabel = "kcloud.ai/rngd.present"

// labelValueTrue 는 노드 라벨의 참 값 "true"(goconst 회피용 공용 상수).
const labelValueTrue = "true"

// dsManagedNodes 는 RNGD 파티션 scope 판정 대상 노드명 목록을 반환한다.
// unified DS(A' 방안)는 Warboy·RNGD 를 한 DaemonSet 으로 관리하므로 pod nodeSelector
// (furiosa-family)만으로는 Warboy 노드까지 포함된다. 그러나 RNGD_PARTITION_POLICY 는
// RNGD 노드에만 영향(Warboy 바이너리는 무시)하므로, scope 는 DS 관리 노드 중
// **RNGD 노드(kcloud.ai/rngd.present=true)**로 한정한다(mixed unified DS 오분류 방지).
// DS 에 nodeSelector 가 없으면 scope 제약을 걸지 않는다(빈 슬라이스 → selectorCoversAll 무조건 통과).
func (r *AcceleratorPartitionPolicyReconciler) dsManagedNodes(ctx context.Context) ([]string, error) {
	var ds appsv1.DaemonSet
	if err := r.Get(ctx, client.ObjectKey{Name: rngdUnifiedDSName, Namespace: rngdUnifiedDSNS}, &ds); err != nil {
		return nil, err
	}
	if len(ds.Spec.Template.Spec.NodeSelector) == 0 {
		return nil, nil
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(ds.Spec.Template.Spec.NodeSelector)}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		if n.Labels[rngdPresentNodeLabel] == labelValueTrue {
			names = append(names, n.Name)
		}
	}
	return names, nil
}

// selectorCoversAll 은 sel 매칭 노드 집합이 dsNodes 전체를 덮는지(부분집합이 아닌지) 확인한다.
func selectorCoversAll(ctx context.Context, c client.Client, sel map[string]string, dsNodes []string) bool {
	if len(dsNodes) == 0 {
		return true
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(sel)}); err != nil {
		return false
	}
	matched := make(map[string]bool, len(nodes.Items))
	for _, n := range nodes.Items {
		matched[n.Name] = true
	}
	for _, name := range dsNodes {
		if !matched[name] {
			return false
		}
	}
	return true
}

// aggregatePhase 는 전 target 최저 수렴 phase 를 반환한다(spec §2.2) — 첫 비-Ready target 이 우선.
func aggregatePhase(targets []npuv1alpha1.TargetStatus) string {
	if len(targets) == 0 {
		return npuv1alpha1.ACPPPhasePending
	}
	for _, t := range targets {
		if t.Phase != npuv1alpha1.ACPPPhaseReady {
			return t.Phase
		}
	}
	return npuv1alpha1.ACPPPhaseReady
}

func (r *AcceleratorPartitionPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// nvidia.CheckHardwareChange / restartNvidiaDevicePlugin 이 pod 를 노드명으로 필터하므로 인덱스 등록.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&npuv1alpha1.AcceleratorPartitionPolicy{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}

func toPartitionLayouts(ls []npuv1alpha1.PartitionLayout) []partition.Layout {
	out := make([]partition.Layout, len(ls))
	for i, l := range ls {
		out[i] = partition.Layout{Profile: l.Profile, CountPerDevice: l.CountPerDevice}
	}
	return out
}

// resolveRngd 는 요청 layout 을 backendPolicy 로 resolve 한다(mapping 테이블).
func resolveRngd(ls []npuv1alpha1.PartitionLayout) ([]partition.ResolvedEntry, []npuv1alpha1.ResolvedLayoutEntry) {
	re := make([]partition.ResolvedEntry, 0, len(ls))
	rs := make([]npuv1alpha1.ResolvedLayoutEntry, 0, len(ls))
	for _, l := range ls {
		p, ok := rngd.ResolveProfile(l.Profile)
		if !ok {
			continue
		}
		re = append(re, partition.ResolvedEntry{Profile: p.Profile, BackendPolicy: p.BackendPolicy, ExpectedCountPerDevice: p.CountPerDevice})
		rs = append(rs, npuv1alpha1.ResolvedLayoutEntry{Profile: p.Profile, BackendPolicy: p.BackendPolicy, ExpectedCountPerDevice: p.CountPerDevice})
	}
	return re, rs
}

func setCond(ts *npuv1alpha1.TargetStatus, ctype string, status metav1.ConditionStatus, reason, msg string, gen int64) {
	apimeta.SetStatusCondition(&ts.Conditions, metav1.Condition{
		Type: ctype, Status: status, Reason: reason, Message: msg, ObservedGeneration: gen,
	})
}

// writeTargetStatus 는 target status 1개(MVP-1 단일 target)를 영속하는 유일한 지점이다.
//
// 기록 전에 반드시 LastTransitionTime 을 직전 status 에서 승계한다. runTarget 은 매 reconcile 마다
// TargetStatus 를 새로 만들고(Conditions 는 빈 슬라이스) apimeta.SetStatusCondition 은 목록에 없는
// condition 을 "전이"로 보아 LastTransitionTime=now 를 찍는다 — 그래서 아무것도 바뀌지 않은
// reconcile 조차 타임스탬프만 다른 status 를 만들고, 이 Update 는 매번 실제 write 가 된다. 그 write
// 의 watch 이벤트가 같은 객체를 즉시 재큐잉하고, 다음 reconcile 이 낡은 informer 캐시를 읽으면
// resourceVersion conflict 까지 나면서 정책이 영원히 같은 자리를 돈다(U-1 핫루프). 상태가 그대로면
// 바이트 단위로 같은 status 를 만들어 apiserver 가 write 자체를 no-op 하게 만드는 것이 유일한 차단점이다.
func (r *AcceleratorPartitionPolicyReconciler) writeTargetStatus(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, ts npuv1alpha1.TargetStatus) error {
	carryCondTimes(findTargetStatus(acpp, ts.NodeName), &ts)
	acpp.Status.Targets = []npuv1alpha1.TargetStatus{ts}
	acpp.Status.ObservedGeneration = acpp.Generation
	acpp.Status.Phase = aggregatePhase(acpp.Status.Targets)
	return r.Status().Update(ctx, acpp)
}

// findTargetStatus 는 노드의 직전 target status 를 반환한다(없으면 nil).
func findTargetStatus(acpp *npuv1alpha1.AcceleratorPartitionPolicy, node string) *npuv1alpha1.TargetStatus {
	for i := range acpp.Status.Targets {
		if acpp.Status.Targets[i].NodeName == node {
			return &acpp.Status.Targets[i]
		}
	}
	return nil
}

// carryCondTimes 는 status 가 변하지 않은 condition 의 LastTransitionTime 을 직전 값으로 되돌린다
// (= SetStatusCondition 이 Conditions 를 보존했을 때의 의미). status 가 바뀐 condition 은 실제 전이라
// 새 시각을 유지한다. prev.Conditions 를 통째로 seeding 하지 않는 이유는, 그러면 이번 pass 가 도달하지
// 못한 단계의 condition(예: 성공 후 실패한 reconcile 의 Verified=True, 재적용 성공 후의 RolledBack=True)이
// 지워지지 않고 남아 status 가 거짓을 말하기 때문이다 — 고쳐야 할 것은 타임스탬프뿐이다.
func carryCondTimes(prev *npuv1alpha1.TargetStatus, ts *npuv1alpha1.TargetStatus) {
	if prev == nil {
		return
	}
	for i := range ts.Conditions {
		old := apimeta.FindStatusCondition(prev.Conditions, ts.Conditions[i].Type)
		if old != nil && old.Status == ts.Conditions[i].Status {
			ts.Conditions[i].LastTransitionTime = old.LastTransitionTime
		}
	}
}

func (r *AcceleratorPartitionPolicyReconciler) fail(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, msg string) error {
	acpp.Status.Phase = npuv1alpha1.ACPPPhaseFailed
	_ = msg // 실패 사유는 target 부재로 조건에 기록할 곳이 없음(vendor 판정 이전) — phase 로만 표시.
	return r.Status().Update(ctx, acpp)
}

// firstNodeName 은 selector 매칭 첫 노드명을 반환한다(MVP-1 단일 target; 정렬로 결정적).
func firstNodeName(ctx context.Context, c client.Client, sel map[string]string) string {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(sel)}); err != nil {
		return ""
	}
	names := make([]string, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// handleDeletion 은 deletionPolicy Retain(MVP-1 유일)을 구현한다: owner lock(annotation) 해제 +
// 임시 검증 Pod 정리 후 finalizer 제거. DS 의 RNGD_PARTITION_POLICY env 는 건드리지 않는다 —
// 현재 partition 을 유지(자동 rollback 은 위험, spec §7.3).
func (r *AcceleratorPartitionPolicyReconciler) handleDeletion(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(acpp, acppFinalizer) {
		return ctrl.Result{}, nil
	}

	// nvidia MIG 는 하드웨어를 실제로 바꿨으므로 snapshot 기반 rollback + 복원 검증을 해야 finalizer 를
	// 지운다(spec §15.5). cleanup 실패/차단 시 err 전파 → finalizer 유지(rate-limited requeue).
	if acpp.Spec.Vendor == vendorNvidia {
		if err := r.handleNvidiaDeletion(ctx, acpp); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(acpp, acppFinalizer)
		if err := r.Update(ctx, acpp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	{
		var ds appsv1.DaemonSet
		if err := r.Get(ctx, client.ObjectKey{Name: rngdUnifiedDSName, Namespace: rngdUnifiedDSNS}, &ds); err == nil {
			// 이 ACPP 가 실제 소유자일 때만 lock 해제 — TargetConflict 패자 삭제가 승자의 lock 을
			// 지우면 안 된다(finding #2). value 는 소유 ACPP 의 .metadata.name(Task 9).
			if ds.Annotations[rngd.PartitionOwnerAnnotation] == acpp.Name {
				delete(ds.Annotations, rngd.PartitionOwnerAnnotation)
				if err := r.Update(ctx, &ds); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			// DS 가 진짜 부재(NotFound)면 해제할 lock 이 없으니 진행. 그 외(transient) 에러는
			// owner lock 을 못 지운 채 ACPP 를 지우면 annotation 이 누수되므로 requeue 한다(Retain 규범).
			return ctrl.Result{}, err
		}
		// 검증 Pod 정리: MVP-1 은 verify Pod 를 생성하지 않으므로 no-op(향후 Pod 기반 verify 도입 시 라벨 셀렉터로 정리).
		controllerutil.RemoveFinalizer(acpp, acppFinalizer)
		if err := r.Update(ctx, acpp); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// handleNvidiaDeletion 은 저장된 ApplyRecords(snapshot only — nodeSelector 재평가 금지, §14.3) 만 보고
// 노드별 MIG 복원을 수행한다. 하드웨어를 바꾼(phase≥Applying) 노드는 소유권을 잃었으면 CleanupBlocked
// (finalizer 유지, 운영자 개입). 소유 중이면 quiesce→snapshot PCI rollback→DP 재시작→fresh NDR 로
// MIG Disabled + baseline gpu 복원 확인→lock 해제. 어느 단계든 err 면 finalizer 유지(requeue).
func (r *AcceleratorPartitionPolicyReconciler) handleNvidiaDeletion(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy) error {
	for _, rec := range acpp.Status.ApplyRecords {
		// 공유 원복은 소유권 판정보다 **위**다. 공유는 하드웨어를 바꾸지 않으므로 MIG 노드 lock 을
		// 잡지 않는데(layout 없는 공유 정책이 그렇다), 아래 소유권 분기 뒤에 두면 그런 정책은 항상
		// continue 로 건너뛰어 ConfigMap·배선·소유 표시가 영구히 남는다 — daemon 만 회수되므로
		// 배수 광고는 남고 그것을 받칠 MPS 서버는 사라진 상태가 된다. 소유 표시가 남으면 다른 이름의
		// 새 정책이 소유 검사에서 영구 apply 실패하고, 그 정책의 해제 경로는 저널이 비어 no-op 이라
		// 자가치유도 없다. 안전하다: RollbackSharing 은 DS/CM 의 소유 표시가 남의 것이면 곧바로
		// 반환하므로, 소유하지 않은 노드의 record 를 돌아도 아무것도 건드리지 않는다.
		// 원복이 GI 제거보다 앞서야 하는 이유도 그대로다 — 광고 배수가 남아 있으면 GI 제거 후
		// baseline allocatable 복원 확인(assertMigEmptyAndBaseline)이 성립하지 않는다.
		if rec.SharingMode == npuv1alpha1.SharingModeTimeSliced || rec.SharingMode == npuv1alpha1.SharingModeMPS {
			st := partition.Target{Ctx: ctx, NodeName: rec.NodeName, Owner: acpp.Name}
			if err := nvidia.New(r.Client).RollbackSharing(st, partition.SharingRollbackState{}); err != nil {
				return err
			}
			if err := r.restartNvidiaDevicePlugin(ctx, rec.NodeName); err != nil {
				// 재시작 실패로 정리를 끝내지 않는다(공유 해제 경로들과 같은 규율). 배선을 걷는
				// RollbackSharing 이 DP DaemonSet 의 pod 템플릿을 patch 하므로 DaemonSet 컨트롤러가
				// 스스로 롤아웃한다 — 이 호출은 그것을 앞당기는 가속기일 뿐이라 실패해도 광고는
				// 결국 돌아온다. 반대로 여기서 반환하면 소유권 충돌 판정(CleanupBlocked)조차
				// 기록되지 못해 운영자가 진짜 이유를 못 본다.
				logf.FromContext(ctx).Error(err, "deletion: device-plugin restart failed", "node", rec.NodeName)
			}
		}
		// phase≥Applying 이면 하드웨어가 실제로 mutate 됐다(Prepared/LockAcquired/미기록 은 복원 대상 아님).
		// mode 전환 구간(Task 6)도 복원 대상이 아니다 — GI 를 만들기 전이라 되돌릴 GI 가 없고, 그럼에도
		// rollback 을 태우면 BuildDisableSteps 의 "current==Enabled" assert 가 전환 도중(Disabled)에
		// 실패해 finalizer 가 영구히 남는다. mode 자체는 모델 B 대로 켜진 채 유지한다.
		hardwareChanged := rec.MigPhase != "" && rec.MigPhase != npuv1alpha1.MigPhasePrepared &&
			rec.MigPhase != npuv1alpha1.MigPhaseLockAcquired && !isMigModeEnablePhase(rec.MigPhase)
		lockUID, err := r.nodeOwnerUID(ctx, rec.NodeName)
		if err != nil {
			return err // owner 확인 실패 → finalizer 유지, requeue.
		}
		if lockUID != string(acpp.UID) {
			if !hardwareChanged {
				continue // 하드웨어 미변경 + 소유권 없음 → undo 할 게 없다(타 소유자 노드 미터치).
			}
			// lock 은 사라졌지만 하드웨어가 이미 baseline 일 수 있다 — 예: THIS record 를 이전 pass 에서
			// 완전 정리(lock 해제)했고, 다른 record 의 requeue 로 재진입한 경우. 이미 복원된 하드웨어는
			// 완료로 간주(멱등)하고, 진짜 미복원일 때만 차단한다.
			if err := r.assertMigEmptyAndBaseline(ctx, rec); err == nil {
				continue
			}
			// 하드웨어를 바꿨는데 lock 을 잃었고 복원도 증명 못 함 → 차단(운영자 개입).
			r.setDeletionBlocked(ctx, acpp, rec.NodeName, "ownership conflict during cleanup")
			return fmt.Errorf("cleanup blocked on node %s: ownership conflict (phase=%s)", rec.NodeName, rec.MigPhase)
		}
		if !hardwareChanged {
			// mode 전환 도중 삭제되면 우리가 잠근 노드를 되돌려 준다(cordon 된 채 방치 금지, Task 6).
			if err := r.restoreSchedulable(ctx, acpp, rec.NodeName); err != nil {
				return err
			}
			if err := r.releaseNodeOwnerLockUID(ctx, rec.NodeName, string(acpp.UID)); err != nil {
				return err
			}
			continue
		}
		// cordon 주체는 정책이다(D-10). 성공한 apply 는 종점에서 노드를 되돌려 놓으므로 삭제 시점의
		// 노드는 schedulable 이고, 여기서 cordon 을 남에게 맡기면 assertNodeQuiesced 가 오지 않는
		// 외부 주체를 기다리며 영구히 막는다 — finalizer 가 빠지지 않아 ACPP 가 영원히 Terminating 이다.
		// 배출은 하지 않으므로 GPU 를 쥔 pod 이 남아 있으면 아래 quiesce 전제가 그대로 걸러 낸다.
		if err := r.cordonForRollback(ctx, acpp, rec); err != nil {
			return err
		}
		if err := r.assertNodeQuiesced(ctx, rec.NodeName); err != nil {
			r.emitDeletionBlockedEvent(acpp, rec.NodeName, err)
			return err
		}
		b := nvidia.New(r.Client).WithExecutor(r.nvidiaExecutor()).WithTargetPCIs(rec.GPUPCIs)
		if err := b.Rollback(partition.Target{Ctx: ctx, NodeName: rec.NodeName, Owner: string(acpp.UID), Generation: rec.Generation},
			partition.RollbackState{PrevPolicy: "disabled"}); err != nil {
			r.emitDeletionBlockedEvent(acpp, rec.NodeName, err)
			return err
		}
		if err := r.restartNvidiaDevicePlugin(ctx, rec.NodeName); err != nil {
			return err
		}
		if err := r.assertMigEmptyAndBaseline(ctx, rec); err != nil {
			r.emitDeletionBlockedEvent(acpp, rec.NodeName, err)
			return err // 복원 미확인 → finalizer 유지(never remove on unverified restore).
		}
		if err := r.syncMigActiveLabel(ctx, rec.NodeName, false); err != nil {
			return err
		}
		// 복원이 증명된 뒤에만 노드를 되돌린다 — 그 전에 uncordon 하면 rollback 도중 워크로드가 들어온다.
		if err := r.restoreSchedulable(ctx, acpp, rec.NodeName); err != nil {
			return err
		}
		if err := r.releaseNodeOwnerLockUID(ctx, rec.NodeName, string(acpp.UID)); err != nil {
			return err
		}
	}
	// control daemon 정리는 저널 상태·소유권과 무관하게 멱등이다 — 참조 카운트(anyPolicyStillUsesMPS)가
	// "다른 정책이 아직 쓰는가" 를 이미 지키므로 무조건 불러도 안전하다. 그래서 루프 밖에서 정확히
	// 한 번 부른다(D-13): 루프 안에 두면 sharing-only 정책은 MIG 노드 lock 을 잡지 않아 소유권 분기에서
	// 항상 continue 하고, ApplyRecords 가 비면 루프 자체가 돌지 않아 daemon 이 영구히 남았다.
	// 위치가 루프 뒤인 이유는 순서다 — DP 배선(RollbackSharing)을 먼저 걷어야 클라이언트가 죽은
	// daemon 을 물고 남지 않는다. node 인자는 exclusive 경로에서 쓰이지 않으므로 빈 값이면 된다.
	if err := r.ensureMpsControlDaemon(ctx, npuv1alpha1.SharingModeExclusive, acpp.Name, ""); err != nil {
		return err
	}
	// finding #2: owner-lock 은 Diff/Apply 이전에 선취득(runNvidiaTarget)되나 ApplyRecord 는 mutation
	// 직전에야 영속된다. 그 사이 hardware-block(MigModeNotEnabled/NotCordoned/GPUBusy)으로 조기 반환되면
	// lock 만 남고 record 가 없어, record 순회만으로는 이 노드 lock 을 해제하지 못해 누수된다(삭제 후 재-ACPP
	// 영구 TargetConflict). record 없는 lock 은 hardware 미변경이므로 rollback 없이 lock 만 해제한다.
	if err := r.releaseOrphanOwnerLocks(ctx, string(acpp.UID)); err != nil {
		return err
	}
	return nil
}

// releaseOrphanOwnerLocks 는 이 ACPP UID 로 잠긴 노드 중 ApplyRecord 로 정리되지 않은 lock 을 스캔·해제한다
// (finding #2). nodeSelector 재평가가 아니라 UID annotation 매칭이므로 §14.3(snapshot only)을 위반하지 않고,
// releaseNodeOwnerLockUID 가 값 일치 시에만 지우므로 타 소유자 노드는 건드리지 않는다. 이미 해제된 노드는 no-op.
func (r *AcceleratorPartitionPolicyReconciler) releaseOrphanOwnerLocks(ctx context.Context, uid string) error {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return err
	}
	for i := range nodes.Items {
		if nodes.Items[i].Annotations[migOwnerAnnotation] == uid {
			if err := r.releaseNodeOwnerLockUID(ctx, nodes.Items[i].Name, uid); err != nil {
				return err
			}
		}
	}
	return nil
}

// nodeOwnerUID 는 노드의 migOwnerAnnotation 값(소유 ACPP UID)을 읽는다. 부재면 "".
func (r *AcceleratorPartitionPolicyReconciler) nodeOwnerUID(ctx context.Context, node string) (string, error) {
	var n corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		return "", err
	}
	return n.Annotations[migOwnerAnnotation], nil
}

// releaseNodeOwnerLockUID 는 annotation 값이 uid 와 일치할 때만 owner-lock 을 지운다(merge-patch, 타 소유자 미터치).
func (r *AcceleratorPartitionPolicyReconciler) releaseNodeOwnerLockUID(ctx context.Context, node, uid string) error {
	var n corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		return err
	}
	if n.Annotations[migOwnerAnnotation] != uid {
		return nil
	}
	base := n.DeepCopy()
	delete(n.Annotations, migOwnerAnnotation)
	return r.Patch(ctx, &n, client.MergeFrom(base))
}

// assertNodeQuiesced 는 rollback 안전(노드 cordon + GPU 점유 pod 없음)을 검증한다(§15.3).
// 실제 판정은 partition.Quiescer 로 위임한다(Task 5 — 배출 로직과 판정 규칙을 한 곳에 둔다).
func (r *AcceleratorPartitionPolicyReconciler) assertNodeQuiesced(ctx context.Context, node string) error {
	if err := partition.NvidiaQuiescer(r.Client).AssertQuiesced(ctx, node); err != nil {
		return fmt.Errorf("%w; unsafe to rollback MIG", err)
	}
	return nil
}

// emitDeletionBlockedEvent 는 삭제 rollback 이 막힌 이유를 Warning 이벤트로 남긴다(review-d10 M-1).
// MigPhase/status 는 건드리지 않는다 — 삭제 경로의 저널은 "무엇을 되돌려야 하는가"의 유일한
// 근거라 phase 를 덮으면 D-10 이 막은 것과 같은 종류의 자기유발 상태 오염이 재발한다.
func (r *AcceleratorPartitionPolicyReconciler) emitDeletionBlockedEvent(acpp *npuv1alpha1.AcceleratorPartitionPolicy, node string, err error) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(acpp, corev1.EventTypeWarning, "DeletionRollbackBlocked",
		"node %s: MIG rollback during deletion is blocked: %v (finalizer held until resolved)", node, err)
}

// assertMigEmptyAndBaseline 은 rollback 후 fresh NDR 로 대상 PCI 전부 MIG Enabled + GI 없음
// (geometry 빈 문자열) 이고 노드 nvidia.com/gpu allocatable 이 baseline 으로 복원됐는지 확인한다(모델 B §17.1).
// mode 는 끄지 않는다(GI 만 제거). 하나라도 아니면 err.
func (r *AcceleratorPartitionPolicyReconciler) assertMigEmptyAndBaseline(ctx context.Context, rec npuv1alpha1.ApplyRecord) error {
	// MIG 상태는 detector 가 못 채우므로(spec §16) apply 경로와 동일하게 observer(nsenter Job)로 관측한다.
	obs, err := r.nvidiaObserver().Observe(ctx, rec.NodeName, rec.GPUPCIs)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(obs))
	for _, o := range obs {
		seen[o.PCI] = true
		if o.Err != "" {
			return fmt.Errorf("node %s pci %s observation failed after rollback: %s", rec.NodeName, o.PCI, o.Err)
		}
		if o.ModeCurrent != "Enabled" {
			return fmt.Errorf("node %s pci %s MIG not enabled after rollback (current=%s)", rec.NodeName, o.PCI, o.ModeCurrent)
		}
		if o.Geometry != "" {
			return fmt.Errorf("node %s pci %s still has GPU instances after rollback (geometry=%s)", rec.NodeName, o.PCI, o.Geometry)
		}
	}
	for _, p := range rec.GPUPCIs {
		if !seen[p] {
			return fmt.Errorf("node %s: snapshot MIG pci %s not observed after rollback", rec.NodeName, p)
		}
	}
	var n corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: rec.NodeName}, &n); err != nil {
		return err
	}
	q := n.Status.Allocatable[corev1.ResourceName(nvidiaGPUResource)]
	if int32(q.Value()) != rec.BaselineGPUCount {
		return fmt.Errorf("node %s allocatable %s=%d not restored to baseline %d", rec.NodeName, nvidiaGPUResource, q.Value(), rec.BaselineGPUCount)
	}
	return nil
}

// setDeletionBlocked 는 cleanup 차단 상태를 영속한다: 대상 rec.MigPhase=CleanupBlocked + target
// OwnershipConflict condition. finalizer 는 유지되므로 운영자가 수동 복원 후 개입해야 한다(§15.5).
func (r *AcceleratorPartitionPolicyReconciler) setDeletionBlocked(ctx context.Context, acpp *npuv1alpha1.AcceleratorPartitionPolicy, node, msg string) {
	base := acpp.DeepCopy()
	for i := range acpp.Status.ApplyRecords {
		if acpp.Status.ApplyRecords[i].NodeName == node {
			acpp.Status.ApplyRecords[i].MigPhase = npuv1alpha1.MigPhaseCleanupBlocked
		}
	}
	idx := -1
	for i := range acpp.Status.Targets {
		if acpp.Status.Targets[i].NodeName == node {
			idx = i
			break
		}
	}
	if idx < 0 {
		acpp.Status.Targets = append(acpp.Status.Targets, npuv1alpha1.TargetStatus{NodeName: node})
		idx = len(acpp.Status.Targets) - 1
	}
	setCond(&acpp.Status.Targets[idx], npuv1alpha1.ACPPCondApplied, metav1.ConditionFalse,
		npuv1alpha1.ReasonOwnershipConflict, msg, acpp.Generation)
	logger := logf.FromContext(ctx)
	if err := r.Status().Patch(ctx, acpp, client.MergeFrom(base)); err != nil {
		logger.Error(err, "setDeletionBlocked: status patch failed")
	}
}
