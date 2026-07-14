// ============================================================
// driver_upgrade_controller.go: Driver Upgrade 컨트롤러 (Reconciler)
// 상세: DriverUpgradeState CRD를 감시하고 UpgradeStateMachine을 호출하여
//       노드별 드라이버 업그레이드 상태 전이를 수행합니다.
//       ensureUpgradeStates()로 NodeDeviceReport 기반 DUS CR을 자동 생성합니다.
// 생성일: 2026-04-13 | 수정일: 2026-04-15
// ============================================================

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/metrics"
	"kcloud-operator/internal/operation"
	"kcloud-operator/internal/upgrade"
)

// +kubebuilder:rbac:groups=npu.ai,resources=driverupgradestates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=npu.ai,resources=driverupgradestates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=npu.ai,resources=driverinstallpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=npu.ai,resources=nodedevicereports,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// DriverUpgradeReconciler는 DriverUpgradeState CR을 감시하고 업그레이드 상태 머신을 구동합니다.
type DriverUpgradeReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     record.EventRecorder
	StateMachine *upgrade.UpgradeStateMachine
}

func (r *DriverUpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	metrics.RecordReconcile() // reconcile 호출 시각 기록 (liveness probe 용)
	logger := logf.FromContext(ctx).WithValues("driverupgradestate", req.Name)

	// 0. Stuck-label sweep — 6일 stuck invariant root cause 재발 방지.
	// 사이클이 비정상 종료(panic, ctx cancel, controller 재시작 도중 crash)되어
	// `npu.ai/driver-upgrading` 라벨이 노드에 남으면 detector DS nodeAffinity 가
	// 영구 차단 → NDR stale → 모든 사이클 zombie. 매 reconcile 진입마다 점검.
	if err := r.sweepStuckUpgradingLabels(ctx); err != nil {
		// 비치명적 오류: 본 reconcile 흐름은 계속 진행
		logger.Error(err, "stuck driver-upgrading 라벨 sweep 실패")
	}

	// 1. NodeDeviceReport 기반 DUS 자동 생성/동기화 (부트스트랩 포함 — Get 이전에 실행)
	if err := r.ensureUpgradeStates(ctx); err != nil {
		logger.Error(err, "UpgradeState 동기화 실패")
		// 비치명적 오류: 계속 진행
	}

	// 2. DriverUpgradeState CR 조회 (부트스트랩 더미 요청이면 NotFound → 재시도)
	var state v1alpha1.DriverUpgradeState
	if err := r.Get(ctx, req.NamespacedName, &state); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Defer cleanup — Reconcile 함수가 panic 으로 중단되거나 ctx 가 cancel 된 직후라도
	// 사이클 종료 상태(Idle / Failed / "")에서는 라벨이 절대 남지 않도록 보장한다.
	// reconcile ctx 와 분리된 background ctx 를 사용해야 ctx cancel 시에도 cleanup 이 실행됨.
	defer func() {
		// 패닉 자체는 그대로 전파하되, 라벨 cleanup 은 시도한다.
		if state.Spec.NodeName == "" {
			return
		}
		switch state.Status.State {
		case v1alpha1.UpgradeStateIdle, "", v1alpha1.UpgradeStateFailed:
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := r.StateMachine.EnsureUpgradingLabelRemoved(cleanupCtx, state.Spec.NodeName); err != nil {
				logger.Error(err, "defer cleanup: driver-upgrading 라벨 제거 실패",
					"node", state.Spec.NodeName, "state", state.Status.State)
			}
			// 옵션 A: 두 라벨 모두 제거 invariant — defer 도 동일하게 처리.
			// blocking 라벨이 잔류하면 detector 가 spawn 안 되므로 sweep 15s 까지의 window
			// 가 생김. 이 호출로 window 를 0 에 가깝게 줄임.
			if err := r.StateMachine.EnsureUpgradingBlockingLabelRemoved(cleanupCtx, state.Spec.NodeName); err != nil {
				logger.Error(err, "defer cleanup: driver-upgrading-blocking 라벨 제거 실패",
					"node", state.Spec.NodeName, "state", state.Status.State)
			}
		}
	}()

	// 3. 매칭 DriverInstallPolicy 조회 (vendor/model 기준)
	policy, err := r.findMatchingPolicy(ctx, state.Spec.Vendor, state.Spec.Model)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("DriverInstallPolicy 조회 실패: %w", err)
	}
	if policy == nil {
		logger.Info("매칭 DriverInstallPolicy 없음, 스킵", "vendor", state.Spec.Vendor)
		return ctrl.Result{}, nil
	}

	// 3.5. 위임 모드 — 상태머신을 여기서 직접 돌리는 대신 트랜잭션을 만든다(분기가 아니라
	// 호출자의 이동이므로 조정자와 이 컨트롤러가 동시에 상태머신을 돌릴 수 없다).
	if delegateModeEnabled() {
		if handled, res, derr := r.delegateDriverUpgrade(ctx, &state); handled {
			return res, derr
		}
	}

	// 4. 상태 머신 실행
	requeue, requeueAfter, smErr := r.StateMachine.TransitionState(ctx, &state, policy)

	// 5. DriverUpgradeState 상태 업데이트
	if err := r.Status().Update(ctx, &state); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("상태 업데이트 실패: %w", err)
	}

	if smErr != nil {
		logger.Error(smErr, "상태 머신 오류")
		return ctrl.Result{}, smErr
	}

	if requeue {
		if requeueAfter > 0 {
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}
	// Bug #6 fix: Furiosa DUS가 watch event를 받지 못하는 경우를 위한 주기적 재체크 (30초)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// ensureUpgradeStates는 NodeDeviceReport 기반으로 DriverUpgradeState CR을 자동 생성합니다.
// 버전 불일치 감지 시 State를 UpgradeRequired로 설정합니다.
func (r *DriverUpgradeReconciler) ensureUpgradeStates(ctx context.Context) error {
	logger := logf.FromContext(ctx)

	var ndrList v1alpha1.NodeDeviceReportList
	if err := r.List(ctx, &ndrList); err != nil {
		return err
	}

	var dipList v1alpha1.DriverInstallPolicyList
	if err := r.List(ctx, &dipList); err != nil {
		return err
	}

	for _, ndr := range ndrList.Items {
		nodeName := ndr.Spec.NodeName
		if nodeName == "" {
			nodeName = ndr.Name
		}

		// Control plane/master 노드 제외
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
			continue
		}
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			continue
		}
		if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
			continue
		}

		for _, device := range ndr.Status.Devices {
			// 매칭 DIP 찾기
			policy := findPolicy(dipList.Items, device.Vendor, device.Model)
			if policy == nil {
				continue
			}
			// nodeSelector 매칭 확인
			if !nodeMatchesSelector(&node, policy.Spec.NodeSelector) {
				continue
			}

			// TrackOnly 정책: DUS 를 Idle 로 추적만 한다(설치/업그레이드 전이 없음).
			// installer 이미지 미비 벤더(예: Tenstorrent, 실제 설치는 DKMS 2차)의 버전 관측용.
			if policy.Spec.Driver.TrackOnly {
				if err := r.ensureTrackOnlyState(ctx, nodeName, device, policy); err != nil {
					logger.Error(err, "TrackOnly DUS 동기화 실패", "node", nodeName, "vendor", device.Vendor)
				}
				continue
			}

			r.syncUpgradeState(ctx, nodeName, device, policy)
		}
	}
	return nil
}

// resetRebootTracking 은 새 업그레이드 사이클 진입 시 이전 사이클의 재부팅 추적 상태를 초기화한다.
// RebootAttempts 가 stale 이면 Validating→RebootRequired 게이트(need && RebootAttempts==0)가 막혀
// cross-major 자연 재부팅이 발화하지 않는다(P2-2 결함). RequestedTime/BootID 는 handleRebootRequired 가
// 재부팅 트리거 시 덮어쓰지만 사이클 경계에서 함께 리셋해 stale 관측을 없앤다.
func resetRebootTracking(s *v1alpha1.DriverUpgradeStateStatus) {
	s.RebootAttempts = 0
	s.RebootRequestedTime = metav1.Time{}
	s.RebootBootID = ""
}

// syncUpgradeState 는 설치형(비-TrackOnly) 정책에 대해 device 하나의 DUS 를 생성/전이합니다.
// 신규 생성(버전 비교로 초기 State), 정책 버전 변경, 버전 불일치→UpgradeRequired,
// currentVersion 동기화를 처리합니다. 에러는 로깅 후 skip(기존 동작 보존).
func (r *DriverUpgradeReconciler) syncUpgradeState(ctx context.Context, nodeName string, device v1alpha1.DeviceEntry, policy *v1alpha1.DriverInstallPolicy) {
	logger := logf.FromContext(ctx)
	dusName := driverUpgradeStateName(nodeName, device.Vendor)

	var existing v1alpha1.DriverUpgradeState
	err := r.Get(ctx, types.NamespacedName{Name: dusName}, &existing)
	if apierrors.IsNotFound(err) {
		// 신규 생성: 버전 비교로 초기 State 결정
		initialState := v1alpha1.UpgradeStateIdle
		if policy.Spec.Driver.Version != "" && device.DriverVersion != policy.Spec.Driver.Version {
			initialState = v1alpha1.UpgradeStateRequired
		}
		dus := v1alpha1.DriverUpgradeState{
			ObjectMeta: metav1.ObjectMeta{
				Name: dusName,
			},
			Spec: v1alpha1.DriverUpgradeStateSpec{
				NodeName: nodeName,
				Vendor:   device.Vendor,
				Model:    device.Model,
			},
			Status: v1alpha1.DriverUpgradeStateStatus{
				State:              initialState,
				CurrentVersion:     device.DriverVersion,
				DesiredVersion:     policy.Spec.Driver.Version,
				LastTransitionTime: metav1.Now(),
			},
		}
		if err := r.Create(ctx, &dus); err != nil && !apierrors.IsAlreadyExists(err) {
			logger.Error(err, "DriverUpgradeState 생성 실패", "name", dusName)
		}
		return
	}
	if err != nil {
		logger.Error(err, "DriverUpgradeState 조회 실패", "name", dusName)
		return
	}

	// Bug #7 fix: desiredVersion이 정책과 다르면 업데이트 (상태에 무관)
	desiredVersion := policy.Spec.Driver.Version
	if desiredVersion != "" && existing.Status.DesiredVersion != desiredVersion {
		oldDesired := existing.Status.DesiredVersion
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Status.DesiredVersion = desiredVersion
		existing.Status.CurrentVersion = device.DriverVersion
		if existing.Status.State == v1alpha1.UpgradeStateIdle {
			existing.Status.State = v1alpha1.UpgradeStateRequired
			// 새 업그레이드 사이클이므로 이전 사이클에서 남은 PreviousImage 비움
			existing.Status.PreviousImage = ""
		}
		// 새 사이클 진입: 이전 사이클의 재부팅 카운터/마커를 리셋(durable persist).
		// handleIdle 의 리셋(state_machine.go)은 이 sync 경로가 이미 Idle→Required 로 전이시키면
		// 실행되지 않으므로, cross-major 재부팅 게이트(need && RebootAttempts==0)가 stale 값에 막히지 않도록
		// 여기서도 리셋한다(P2-2 stale RebootAttempts 하드닝).
		resetRebootTracking(&existing.Status)
		existing.Status.LastTransitionTime = metav1.Now()
		existing.Status.Message = fmt.Sprintf("정책 버전 변경: %s → %s", oldDesired, desiredVersion)
		if err := r.Status().Patch(ctx, &existing, patch); err != nil {
			logger.Error(err, "DriverUpgradeState 상태 패치 실패 (desiredVersion 변경)", "name", dusName)
		}
		return
	}

	// 버전 불일치 감지: Idle 상태에서만 UpgradeRequired 전이
	if existing.Status.State == v1alpha1.UpgradeStateIdle &&
		desiredVersion != "" &&
		device.DriverVersion != desiredVersion &&
		existing.Status.CurrentVersion != desiredVersion {

		patch := client.MergeFrom(existing.DeepCopy())
		existing.Status.State = v1alpha1.UpgradeStateRequired
		existing.Status.CurrentVersion = device.DriverVersion
		existing.Status.DesiredVersion = desiredVersion
		existing.Status.PreviousVersion = device.DriverVersion
		// 새 업그레이드 사이클이므로 이전 사이클에서 남은 PreviousImage 비움
		existing.Status.PreviousImage = ""
		// 새 사이클 진입: 재부팅 카운터/마커 리셋(P2-2 stale RebootAttempts 하드닝) — 위 정책변경 경로와 동일.
		resetRebootTracking(&existing.Status)
		existing.Status.LastTransitionTime = metav1.Now()
		existing.Status.Message = fmt.Sprintf("버전 불일치: %s → %s", device.DriverVersion, desiredVersion)
		if err := r.Status().Patch(ctx, &existing, patch); err != nil {
			logger.Error(err, "DriverUpgradeState 상태 패치 실패", "name", dusName)
		}
		return
	}

	// currentVersion 동기화: Idle 상태에서 NDR이 갱신되었지만 버전이 일치하는 경우
	// (desiredVersion == device.DriverVersion 이나 existing.Status.CurrentVersion이 stale)
	if existing.Status.State == v1alpha1.UpgradeStateIdle &&
		device.DriverVersion != "" &&
		existing.Status.CurrentVersion != device.DriverVersion {

		patch := client.MergeFrom(existing.DeepCopy())
		existing.Status.CurrentVersion = device.DriverVersion
		existing.Status.LastTransitionTime = metav1.Now()
		existing.Status.Message = fmt.Sprintf("NDR 버전 동기화: %s", device.DriverVersion)
		if err := r.Status().Patch(ctx, &existing, patch); err != nil {
			logger.Error(err, "DriverUpgradeState currentVersion 동기화 실패", "name", dusName)
		}
	}
}

// ensureTrackOnlyState 는 TrackOnly 정책에 대해 DUS 를 Idle 상태로만 유지합니다.
// 설치/업그레이드 전이 없이 currentVersion(NDR) 과 desiredVersion(정책)만 동기화하여
// 버전 관측·verifiedVersions 게이트 용도로 추적합니다(installer 미비 벤더 임시 편입).
func (r *DriverUpgradeReconciler) ensureTrackOnlyState(ctx context.Context, nodeName string, device v1alpha1.DeviceEntry, policy *v1alpha1.DriverInstallPolicy) error {
	const trackMsg = "TrackOnly: 버전 관측 전용(설치 경로 없음)"
	dusName := driverUpgradeStateName(nodeName, device.Vendor)

	var existing v1alpha1.DriverUpgradeState
	err := r.Get(ctx, types.NamespacedName{Name: dusName}, &existing)
	if apierrors.IsNotFound(err) {
		dus := v1alpha1.DriverUpgradeState{
			ObjectMeta: metav1.ObjectMeta{Name: dusName},
			Spec: v1alpha1.DriverUpgradeStateSpec{
				NodeName: nodeName,
				Vendor:   device.Vendor,
				Model:    device.Model,
			},
			Status: v1alpha1.DriverUpgradeStateStatus{
				State:              v1alpha1.UpgradeStateIdle,
				CurrentVersion:     device.DriverVersion,
				DesiredVersion:     policy.Spec.Driver.Version,
				LastTransitionTime: metav1.Now(),
				Message:            trackMsg,
			},
		}
		if createErr := r.Create(ctx, &dus); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return createErr
		}
		return nil
	}
	if err != nil {
		return err
	}

	// 기존 DUS: currentVersion/desiredVersion 동기화, State 는 항상 Idle 로 강제.
	if existing.Status.State != v1alpha1.UpgradeStateIdle ||
		existing.Status.CurrentVersion != device.DriverVersion ||
		existing.Status.DesiredVersion != policy.Spec.Driver.Version {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Status.State = v1alpha1.UpgradeStateIdle
		existing.Status.CurrentVersion = device.DriverVersion
		existing.Status.DesiredVersion = policy.Spec.Driver.Version
		existing.Status.LastTransitionTime = metav1.Now()
		existing.Status.Message = trackMsg
		return r.Status().Patch(ctx, &existing, patch)
	}
	return nil
}

// findMatchingPolicy는 vendor/model에 맞는 DriverInstallPolicy를 반환합니다.
func (r *DriverUpgradeReconciler) findMatchingPolicy(ctx context.Context, vendor, model string) (*v1alpha1.DriverInstallPolicy, error) {
	var list v1alpha1.DriverInstallPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	return findPolicy(list.Items, vendor, model), nil
}

// SetupWithManager는 컨트롤러를 manager에 등록합니다.
// Primary: DriverUpgradeState, Secondary: DriverInstallPolicy + NodeDeviceReport
func (r *DriverUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.DriverUpgradeState{}).
		Watches(
			&v1alpha1.DriverInstallPolicy{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				// DriverInstallPolicy 변경 시 해당 vendor의 모든 DUS enqueue
				pol, ok := obj.(*v1alpha1.DriverInstallPolicy)
				if !ok {
					return nil
				}
				var dusList v1alpha1.DriverUpgradeStateList
				if err := mgr.GetClient().List(ctx, &dusList); err != nil {
					return nil
				}
				var reqs []reconcile.Request
				for _, dus := range dusList.Items {
					if dus.Spec.Vendor == pol.Spec.Vendor {
						reqs = append(reqs, reconcile.Request{
							NamespacedName: types.NamespacedName{Name: dus.Name},
						})
					}
				}
				return reqs
			}),
		).
		// NDR watch — DUS가 없을 때 부트스트랩 트리거
		Watches(
			&v1alpha1.NodeDeviceReport{},
			handler.EnqueueRequestsFromMapFunc(r.mapNDRToUpgradeStates),
		).
		Named("driverupgradestate").
		Complete(r)
}

// mapNDRToUpgradeStates는 NDR 변경 시 해당 노드의 DUS를 enqueue합니다.
// DUS가 없으면 부트스트랩 더미 요청을 생성하여 ensureUpgradeStates()를 트리거합니다.
func (r *DriverUpgradeReconciler) mapNDRToUpgradeStates(ctx context.Context, obj client.Object) []reconcile.Request {
	ndr, ok := obj.(*v1alpha1.NodeDeviceReport)
	if !ok {
		return nil
	}

	nodeName := ndr.Spec.NodeName
	if nodeName == "" {
		nodeName = ndr.Name
	}

	var dusList v1alpha1.DriverUpgradeStateList
	if err := r.List(ctx, &dusList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	found := false
	for _, dus := range dusList.Items {
		if dus.Spec.NodeName == nodeName {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: dus.Name},
			})
			found = true
		}
	}

	// DUS가 없으면 더미 request로 Reconcile 트리거 (ensureUpgradeStates가 DUS 생성)
	if !found {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: nodeName + "-bootstrap"},
		})
	}

	return requests
}

// ─────────────────────────────────────────────
// 패키지 내 헬퍼
// ─────────────────────────────────────────────

// driverUpgradeStateName은 노드+벤더 기반 DUS 이름을 생성합니다.
func driverUpgradeStateName(nodeName, vendor string) string {
	return fmt.Sprintf("%s-%s", nodeName, vendor)
}

// nodeMatchesSelector는 노드 라벨이 selector 조건을 모두 만족하는지 확인합니다.
// selector가 비어있으면 모든 노드에 매칭됩니다.
func nodeMatchesSelector(node *corev1.Node, selector map[string]string) bool {
	if len(selector) == 0 {
		return true
	}
	for k, v := range selector {
		if node.Labels[k] != v {
			return false
		}
	}
	return true
}

// stuckLabelGracePeriod 는 driver-upgrading 라벨 (사이클 추적용) 의 stuck grace 마진이다.
// 정상 사이클의 transient 상태 변화에서 잘못 sweep 하는 것을 방지.
const stuckLabelGracePeriod = 30 * time.Second

// stuckBlockingLabelGracePeriod 는 driver-upgrading-blocking 라벨 (Validating 진입 시 자동 제거되는
// 좁은 lifecycle) 의 grace. 일반 라벨보다 짧음 — Validating 진입 후에는 빨리 풀려야 detector
// 차단 해제까지 시간이 줄어듦.
const stuckBlockingLabelGracePeriod = 15 * time.Second

// sweepStuckUpgradingLabels 는 모든 노드를 점검하여 사이클이 비정상 종료된 채
// driver-upgrading 또는 driver-upgrading-blocking 라벨만 남아있는 경우 자동으로 제거한다.
//
// CRITICAL invariant — 정상 mid-cycle 라벨은 절대 건드리지 않는다:
//  1. 노드의 라벨이 있어야 함
//  2. 해당 노드+vendor 의 DUS state ∈ {Idle, "", Failed} (사이클 종료 상태)
//  3. DUS LastTransitionTime 이 grace period 이상 경과 (transient 보호)
//  4. 매칭 DUS 가 하나도 없으면 (vendor 미상) 라벨 보존 — 다른 컨트롤러 소유 가능성
//
// 위 4 조건을 모두 만족할 때만 cleanup 시도. mid-cycle (PreFlight ~ Uncordoning) 라벨은
// 어떤 경우에도 제거하지 않는다.
//
// driver-upgrading-blocking 라벨은 좁은 lifecycle (Cordoning ~ Validating 진입) 이라
// 더 짧은 grace (15s) 를 적용 — Validating 진입했는데도 라벨이 남아있으면 즉시 sweep.
func (r *DriverUpgradeReconciler) sweepStuckUpgradingLabels(ctx context.Context) error {
	logger := logf.FromContext(ctx)

	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return fmt.Errorf("노드 리스트 조회 실패: %w", err)
	}

	var dusList v1alpha1.DriverUpgradeStateList
	if err := r.List(ctx, &dusList); err != nil {
		return fmt.Errorf("DriverUpgradeState 리스트 조회 실패: %w", err)
	}

	dusByNode := map[string][]v1alpha1.DriverUpgradeState{}
	for _, dus := range dusList.Items {
		dusByNode[dus.Spec.NodeName] = append(dusByNode[dus.Spec.NodeName], dus)
	}

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		_, hasMain := node.Labels[upgrade.DriverUpgradingLabelKey]
		_, hasBlocking := node.Labels[upgrade.DriverUpgradingBlockingLabelKey]
		if !hasMain && !hasBlocking {
			continue
		}

		nodeDUS, found := dusByNode[node.Name]
		if !found || len(nodeDUS) == 0 {
			// DUS 부재: 다른 컨트롤러/벤더 소유 라벨일 수 있음 → 보존
			continue
		}

		// 모든 DUS 가 종료 상태이고 grace 경과한 경우에만 stuck 으로 판정
		allTerminal := true
		oldestTransition := time.Now()
		for _, dus := range nodeDUS {
			switch dus.Status.State {
			case v1alpha1.UpgradeStateIdle, "", v1alpha1.UpgradeStateFailed:
				// 종료 상태
			default:
				// mid-cycle — 절대 건드리지 않음
				allTerminal = false
			}
			if !dus.Status.LastTransitionTime.IsZero() &&
				dus.Status.LastTransitionTime.Time.Before(oldestTransition) {
				oldestTransition = dus.Status.LastTransitionTime.Time
			}
		}
		if !allTerminal {
			continue
		}
		age := time.Since(oldestTransition)

		// blocking 라벨: 더 짧은 grace 로 빠르게 sweep
		if hasBlocking && age >= stuckBlockingLabelGracePeriod {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := r.StateMachine.EnsureUpgradingBlockingLabelRemoved(cleanupCtx, node.Name)
			cancel()
			if err != nil {
				logger.Error(err, "stuck driver-upgrading-blocking 라벨 sweep 실패", "node", node.Name)
			} else {
				logger.Info("stuck driver-upgrading-blocking 라벨 자동 제거", "node", node.Name,
					"dusCount", len(nodeDUS), "ageSeconds", age.Seconds())
				if r.Recorder != nil && len(nodeDUS) > 0 {
					r.Recorder.Eventf(&nodeDUS[0], corev1.EventTypeWarning, "StuckBlockingLabelSwept",
						"노드 %s 의 stuck driver-upgrading-blocking 라벨 자동 제거 (DUS 종료 + %ds 경과)",
						node.Name, int(age.Seconds()))
				}
			}
		}

		// 메인 라벨: 기존 grace (30s)
		if hasMain && age >= stuckLabelGracePeriod {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := r.StateMachine.EnsureUpgradingLabelRemoved(cleanupCtx, node.Name)
			cancel()
			if err != nil {
				logger.Error(err, "stuck driver-upgrading 라벨 sweep 실패", "node", node.Name)
				continue
			}
			logger.Info("stuck driver-upgrading 라벨 자동 제거", "node", node.Name,
				"dusCount", len(nodeDUS), "ageSeconds", age.Seconds())
			if r.Recorder != nil && len(nodeDUS) > 0 {
				r.Recorder.Eventf(&nodeDUS[0], corev1.EventTypeWarning, "StuckUpgradeLabelSwept",
					"노드 %s 의 stuck npu.ai/driver-upgrading 라벨 자동 제거 (DUS state 종료 + %ds 경과)",
					node.Name, int(age.Seconds()))
			}
		}
	}
	return nil
}

// findPolicy는 vendor/model이 일치하는 정책을 찾습니다.
// model이 비어있거나 "generic"인 경우 fallback으로 매칭됩니다.
func findPolicy(policies []v1alpha1.DriverInstallPolicy, vendor, model string) *v1alpha1.DriverInstallPolicy {
	var fallback *v1alpha1.DriverInstallPolicy
	for i := range policies {
		p := &policies[i]
		if !strings.EqualFold(p.Spec.Vendor, vendor) {
			continue
		}
		if p.Spec.Model == model {
			return p
		}
		// model이 비어있거나, 어느 쪽이든 "generic"이면 fallback 매칭 (처음 찾은 것만 사용)
		if fallback == nil && (p.Spec.Model == "" || model == "generic" || p.Spec.Model == "generic") {
			fallback = p
		}
	}
	return fallback
}

// TransactionIDForDriver 는 드라이버 작업의 요청 정체성이다.
// 목표 버전이 바뀌면 다른 트랜잭션이다 — 같은 노드의 다음 업그레이드가 이전 작업을 재사용하면
// 저널이 두 사이클을 섞어 담는다.
func TransactionIDForDriver(state *v1alpha1.DriverUpgradeState) string {
	return fmt.Sprintf("%s-%s-%s", state.UID, state.Status.DesiredVersion, state.Spec.NodeName)
}

// OperationNameForDriver 는 드라이버 작업 객체 이름이다. 결정론적이라 같은 의도는 같은 객체를
// 재사용한다 — 시각이나 난수를 넣으면 reconcile 마다 작업이 쌓인다.
func OperationNameForDriver(state *v1alpha1.DriverUpgradeState) string {
	name := fmt.Sprintf("drv-%s-%s", state.Name, state.Status.DesiredVersion)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// delegateDriverUpgrade 는 위임 모드에서 드라이버 상태머신을 직접 돌리는 대신 작업을 만든다.
//
// handled=true 면 호출자는 이 pass 를 여기서 끝낸다 — 상태머신을 부르지 않는다. 그래야 조정자와
// 컨트롤러가 같은 상태머신을 동시에 돌리지 않는다(분기가 아니라 호출자의 이동이다).
//
// 유휴 상태에서는 위임하지 않는다(handled=false). 그 시점에는 상태머신이 아직 아무것도 건드리지
// 않았고 목표 버전조차 계산 전일 수 있다 — "업그레이드가 필요한가" 의 판정은 상태머신 몫이고
// 여기서 다시 구현하지 않는다.
func (r *DriverUpgradeReconciler) delegateDriverUpgrade(ctx context.Context, state *v1alpha1.DriverUpgradeState) (bool, ctrl.Result, error) {
	if state.Status.State != v1alpha1.UpgradeStateRequired {
		// 유휴·진행 중·터미널 — 진행 중이면 이미 만들어 둔 작업이 상태머신을 돌리고 있으므로
		// 여기서는 아무것도 하지 않고 넘긴다.
		if isDriverOperationInFlight(state.Status.State) {
			return true, ctrl.Result{RequeueAfter: opRequeue}, nil
		}
		return false, ctrl.Result{}, nil
	}

	pcis, err := r.devicePCIsForNode(ctx, state.Spec.NodeName, state.Spec.Vendor)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	op := &v1alpha1.AcceleratorOperation{
		ObjectMeta: metav1.ObjectMeta{Name: OperationNameForDriver(state)},
		Spec: v1alpha1.AcceleratorOperationSpec{
			Type: string(operation.DriverUpgrade), NodeName: state.Spec.NodeName,
			Vendor:        state.Spec.Vendor,
			TransactionID: TransactionIDForDriver(state),
			Owner: v1alpha1.OperationOwner{
				Kind: "DriverUpgradeState", Name: state.Name,
				UID: string(state.UID), Generation: state.Generation,
			},
			ResourceKeys: ResourceKeysForDriver(state.Spec.NodeName, pcis),
		},
	}
	if err := r.Create(ctx, op); err != nil && !apierrors.IsAlreadyExists(err) {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{RequeueAfter: opRequeue}, nil
}

// isDriverOperationInFlight 는 이미 만들어진 작업이 상태머신을 돌리고 있는 상태인지다.
func isDriverOperationInFlight(state string) bool {
	switch state {
	case v1alpha1.UpgradeStatePreFlight, v1alpha1.UpgradeStateCordoning,
		v1alpha1.UpgradeStateDraining, v1alpha1.UpgradeStateUpgrading,
		v1alpha1.UpgradeStateValidating, v1alpha1.UpgradeStateRebootRequired,
		v1alpha1.UpgradeStateRebooting, v1alpha1.UpgradeStateUncordoning,
		v1alpha1.UpgradeStateRollback:
		return true
	}
	return false
}

// devicePCIsForNode 는 그 노드의 해당 벤더 장치 PCI 목록이다(자원 키 재료).
// 조회 실패는 오류로 올린다 — 장치를 모르는 채 만든 작업은 장치 단위 충돌을 못 잡는다.
func (r *DriverUpgradeReconciler) devicePCIsForNode(ctx context.Context, node, vendor string) ([]string, error) {
	var ndr v1alpha1.NodeDeviceReport
	if err := r.Get(ctx, types.NamespacedName{Name: node}, &ndr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // 보고가 없으면 노드 키만으로 간다(ResourceKeysForDriver 가 처리한다).
		}
		return nil, err
	}
	pcis := make([]string, 0, len(ndr.Status.Devices))
	for _, d := range ndr.Status.Devices {
		if !strings.EqualFold(d.Vendor, vendor) || d.PCIeAddress == "" {
			continue
		}
		pcis = append(pcis, d.PCIeAddress)
	}
	return pcis, nil
}
