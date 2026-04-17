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

// DriverUpgradeReconciler는 DriverUpgradeState CR을 감시하고 업그레이드 상태 머신을 구동합니다.
type DriverUpgradeReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     record.EventRecorder
	StateMachine *upgrade.UpgradeStateMachine
}

func (r *DriverUpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("driverupgradestate", req.Name)

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

	// 3. 매칭 DriverInstallPolicy 조회 (vendor/model 기준)
	policy, err := r.findMatchingPolicy(ctx, state.Spec.Vendor, state.Spec.Model)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("DriverInstallPolicy 조회 실패: %w", err)
	}
	if policy == nil {
		logger.Info("매칭 DriverInstallPolicy 없음, 스킵", "vendor", state.Spec.Vendor)
		return ctrl.Result{}, nil
	}

	// daemonset 모드만 업그레이드 상태 머신 적용
	if policy.Spec.Driver.Mode != "daemonset" {
		return ctrl.Result{}, nil
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
			if policy.Spec.Driver.Mode != "daemonset" {
				continue
			}

			// nodeSelector 매칭 확인
			if !nodeMatchesSelector(&node, policy.Spec.NodeSelector) {
				continue
			}

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
				continue
			}
			if err != nil {
				logger.Error(err, "DriverUpgradeState 조회 실패", "name", dusName)
				continue
			}

			// Bug #7 fix: desiredVersion이 정책과 다르면 업데이트 (상태에 무관)
			desiredVersion := policy.Spec.Driver.Version
			if desiredVersion != "" && existing.Status.DesiredVersion != desiredVersion {
				patch := client.MergeFrom(existing.DeepCopy())
				existing.Status.DesiredVersion = desiredVersion
				existing.Status.CurrentVersion = device.DriverVersion
				if existing.Status.State == v1alpha1.UpgradeStateIdle {
					existing.Status.State = v1alpha1.UpgradeStateRequired
					// 새 업그레이드 사이클이므로 이전 사이클에서 남은 PreviousImage 비움
					existing.Status.PreviousImage = ""
				}
				existing.Status.LastTransitionTime = metav1.Now()
				existing.Status.Message = fmt.Sprintf("정책 버전 변경: %s → %s", existing.Status.DesiredVersion, desiredVersion)
				if err := r.Status().Patch(ctx, &existing, patch); err != nil {
					logger.Error(err, "DriverUpgradeState 상태 패치 실패 (desiredVersion 변경)", "name", dusName)
				}
				continue
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
				existing.Status.LastTransitionTime = metav1.Now()
				existing.Status.Message = fmt.Sprintf("버전 불일치: %s → %s", device.DriverVersion, desiredVersion)
				if err := r.Status().Patch(ctx, &existing, patch); err != nil {
					logger.Error(err, "DriverUpgradeState 상태 패치 실패", "name", dusName)
				}
			}
		}
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
		// model이 비어있거나, 어느 쪽이든 "generic"이면 fallback 매칭
		if p.Spec.Model == "" || model == "generic" || p.Spec.Model == "generic" {
			fallback = p
		}
	}
	return fallback
}

