// ============================================================
// driver_upgrade_controller_test.go: DriverUpgradeReconciler 단위 테스트
// 상세: ensureUpgradeStates()의 currentVersion 동기화 및 findPolicy fallback 로직 검증
//       fake client 기반 — envtest 불필요
// 생성일: 2026-04-21
// ============================================================

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/upgrade"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

// workerNode는 control-plane 라벨이 없는 일반 워커 노드를 반환합니다.
func workerNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{},
		},
	}
}

func makeNDR(nodeName, vendor, model, driverVersion string) *v1alpha1.NodeDeviceReport {
	return &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: nodeName},
		Status: v1alpha1.NodeDeviceReportStatus{
			Devices: []v1alpha1.DeviceEntry{
				{
					Vendor:        vendor,
					Model:         model,
					DriverVersion: driverVersion,
				},
			},
		},
	}
}

func makeDIP(name, vendor, model, version, mode string) *v1alpha1.DriverInstallPolicy {
	return &v1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.DriverInstallPolicySpec{
			Vendor: vendor,
			Model:  model,
			Driver: v1alpha1.DriverSpec{
				Version: version,
				Mode:    mode,
			},
		},
	}
}

func makeDUS(name, nodeName, vendor, model, state, currentVersion, desiredVersion string) *v1alpha1.DriverUpgradeState {
	return &v1alpha1.DriverUpgradeState{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.DriverUpgradeStateSpec{
			NodeName: nodeName,
			Vendor:   vendor,
			Model:    model,
		},
		Status: v1alpha1.DriverUpgradeStateStatus{
			State:          state,
			CurrentVersion: currentVersion,
			DesiredVersion: desiredVersion,
		},
	}
}

func newReconciler(objs ...client.Object) *DriverUpgradeReconciler {
	s := newTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.DriverUpgradeState{}).
		Build()
	return &DriverUpgradeReconciler{
		Client:       c,
		Scheme:       s,
		StateMachine: &upgrade.UpgradeStateMachine{Client: c},
	}
}

// nodeWithUpgradingLabel 은 driver-upgrading 라벨이 붙은 워커 노드를 반환합니다.
func nodeWithUpgradingLabel(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{upgrade.DriverUpgradingLabelKey: "true"},
		},
	}
}

// dusWithTransition 는 임의의 LastTransitionTime 을 가진 DUS 를 만든다.
func dusWithTransition(name, nodeName, vendor, state string, ago time.Duration) *v1alpha1.DriverUpgradeState {
	return &v1alpha1.DriverUpgradeState{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.DriverUpgradeStateSpec{
			NodeName: nodeName,
			Vendor:   vendor,
		},
		Status: v1alpha1.DriverUpgradeStateStatus{
			State:              state,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-ago)),
		},
	}
}

// TestEnsureUpgradeStates_CurrentVersionSyncFromNDR 는 P0-2 버그를 직접 재현합니다.
// 시나리오: DUS가 Idle 상태이고 currentVersion이 빈 문자열인데
//
//	NDR이 desiredVersion과 동일한 버전을 보고하는 경우 → currentVersion이 채워져야 한다.
func TestEnsureUpgradeStates_CurrentVersionSyncFromNDR(t *testing.T) {
	const (
		nodeName = "worker-1"
		vendor   = "furiosa"
		model    = "warboy"
		version  = "1.9.8-3"
		dusName  = "worker-1-furiosa"
	)

	node := workerNode(nodeName)
	ndr := makeNDR(nodeName, vendor, model, version)
	dip := makeDIP("furiosa-warboy", vendor, model, version, "daemonset")
	// DUS는 이미 존재하지만 currentVersion이 비어있음 (버그 재현 상태)
	dus := makeDUS(dusName, nodeName, vendor, model, v1alpha1.UpgradeStateIdle, "", version)

	r := newReconciler(node, ndr, dip, dus)

	ctx := context.Background()
	if err := r.ensureUpgradeStates(ctx); err != nil {
		t.Fatalf("ensureUpgradeStates 오류: %v", err)
	}

	var got v1alpha1.DriverUpgradeState
	if err := r.Get(ctx, types.NamespacedName{Name: dusName}, &got); err != nil {
		t.Fatalf("DUS 조회 실패: %v", err)
	}

	if got.Status.CurrentVersion != version {
		t.Errorf("currentVersion 동기화 실패: got %q, want %q", got.Status.CurrentVersion, version)
	}
	if got.Status.State != v1alpha1.UpgradeStateIdle {
		t.Errorf("state가 변경되어서는 안 됨: got %q", got.Status.State)
	}
}

// TestEnsureUpgradeStates_CreatesDUSWhenAbsent 는 NDR만 있고 DUS가 없을 때 DUS를 자동 생성하는지 검증합니다.
func TestEnsureUpgradeStates_CreatesDUSWhenAbsent(t *testing.T) {
	const (
		nodeName = "worker-2"
		vendor   = "furiosa"
		model    = "warboy"
		version  = "1.9.8-3"
	)

	node := workerNode(nodeName)
	ndr := makeNDR(nodeName, vendor, model, version)
	dip := makeDIP("furiosa-warboy", vendor, model, version, "daemonset")

	r := newReconciler(node, ndr, dip)

	ctx := context.Background()
	if err := r.ensureUpgradeStates(ctx); err != nil {
		t.Fatalf("ensureUpgradeStates 오류: %v", err)
	}

	dusName := driverUpgradeStateName(nodeName, vendor)
	var got v1alpha1.DriverUpgradeState
	if err := r.Get(ctx, types.NamespacedName{Name: dusName}, &got); err != nil {
		t.Fatalf("DUS 자동 생성 실패: %v", err)
	}

	if got.Status.CurrentVersion != version {
		t.Errorf("currentVersion: got %q, want %q", got.Status.CurrentVersion, version)
	}
	if got.Status.State != v1alpha1.UpgradeStateIdle {
		t.Errorf("초기 state: got %q, want %q", got.Status.State, v1alpha1.UpgradeStateIdle)
	}
}

// TestEnsureUpgradeStates_UpgradeRequiredOnVersionMismatch 는 NDR 버전이 DIP와 다를 때
// DUS가 UpgradeRequired 상태로 전이하는지 검증합니다.
func TestEnsureUpgradeStates_UpgradeRequiredOnVersionMismatch(t *testing.T) {
	const (
		nodeName     = "worker-3"
		vendor       = "furiosa"
		model        = "warboy"
		installedVer = "1.8.0"
		desiredVer   = "1.9.8-3"
		dusName      = "worker-3-furiosa"
	)

	node := workerNode(nodeName)
	ndr := makeNDR(nodeName, vendor, model, installedVer)
	dip := makeDIP("furiosa-warboy", vendor, model, desiredVer, "daemonset")
	dus := makeDUS(dusName, nodeName, vendor, model, v1alpha1.UpgradeStateIdle, installedVer, desiredVer)

	r := newReconciler(node, ndr, dip, dus)

	ctx := context.Background()
	if err := r.ensureUpgradeStates(ctx); err != nil {
		t.Fatalf("ensureUpgradeStates 오류: %v", err)
	}

	var got v1alpha1.DriverUpgradeState
	if err := r.Get(ctx, types.NamespacedName{Name: dusName}, &got); err != nil {
		t.Fatalf("DUS 조회 실패: %v", err)
	}

	if got.Status.State != v1alpha1.UpgradeStateRequired {
		t.Errorf("state: got %q, want %q", got.Status.State, v1alpha1.UpgradeStateRequired)
	}
	if got.Status.CurrentVersion != installedVer {
		t.Errorf("currentVersion: got %q, want %q", got.Status.CurrentVersion, installedVer)
	}
}

// TestFindPolicy_FallbackFirstWins 는 DIP가 2개일 때 첫 번째 fallback이 선택되는지 검증합니다.
func TestFindPolicy_FallbackFirstWins(t *testing.T) {
	dip1 := v1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "dip-first"},
		Spec: v1alpha1.DriverInstallPolicySpec{
			Vendor: "furiosa",
			Model:  "",
			Driver: v1alpha1.DriverSpec{Version: "1.9.8-3"},
		},
	}
	dip2 := v1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "dip-second"},
		Spec: v1alpha1.DriverInstallPolicySpec{
			Vendor: "furiosa",
			Model:  "",
			Driver: v1alpha1.DriverSpec{Version: "2.0.0"},
		},
	}

	got := findPolicy([]v1alpha1.DriverInstallPolicy{dip1, dip2}, "furiosa", "warboy")
	if got == nil {
		t.Fatal("findPolicy: nil 반환")
	}
	if got.Name != "dip-first" {
		t.Errorf("첫 번째 fallback이 선택되어야 함: got %q", got.Name)
	}
}

// TestFindPolicy_RngdModelMatch 는 같은 vendor(furiosa) 아래 warboy/rngd 2개 DIP 중
// vendor=furiosa, model=rngd 요청에 rngd DIP 가 정확히 매칭되는지 검증합니다.
// (B-5 RNGD 호환성 확인: findPolicy 가 model 기반 분기를 지원해야 함)
func TestFindPolicy_RngdModelMatch(t *testing.T) {
	warboy := *makeDIP("furiosa-warboy", "furiosa", "warboy", "1.9.8-3", "daemonset")
	rngd := *makeDIP("furiosa-rngd", "furiosa", "rngd", "2026.1.0", "daemonset")

	// rngd 요청 → rngd DIP 반환
	got := findPolicy([]v1alpha1.DriverInstallPolicy{warboy, rngd}, "furiosa", "rngd")
	if got == nil {
		t.Fatal("findPolicy(furiosa, rngd): nil 반환")
	}
	if got.Name != "furiosa-rngd" {
		t.Errorf("furiosa/rngd 매칭 실패: got %q, want furiosa-rngd", got.Name)
	}

	// warboy 요청 → warboy DIP 반환 (회귀 방지)
	got = findPolicy([]v1alpha1.DriverInstallPolicy{warboy, rngd}, "furiosa", "warboy")
	if got == nil {
		t.Fatal("findPolicy(furiosa, warboy): nil 반환")
	}
	if got.Name != "furiosa-warboy" {
		t.Errorf("furiosa/warboy 매칭 실패: got %q, want furiosa-warboy", got.Name)
	}
}

// TestEnsureUpgradeStates_RngdCreatesDUS 는 vendor=furiosa, model=rngd 인 NDR+DIP 가
// 주어졌을 때 DUS 가 정확히 rngd model 로 생성되는지 검증합니다.
func TestEnsureUpgradeStates_RngdCreatesDUS(t *testing.T) {
	const (
		nodeName = "rngd-1"
		vendor   = "furiosa"
		model    = "rngd"
		version  = "2026.1.0"
	)

	node := workerNode(nodeName)
	ndr := makeNDR(nodeName, vendor, model, version)
	dip := makeDIP("furiosa-rngd", vendor, model, version, "daemonset")

	r := newReconciler(node, ndr, dip)

	ctx := context.Background()
	if err := r.ensureUpgradeStates(ctx); err != nil {
		t.Fatalf("ensureUpgradeStates 오류: %v", err)
	}

	dusName := driverUpgradeStateName(nodeName, vendor)
	var got v1alpha1.DriverUpgradeState
	if err := r.Get(ctx, types.NamespacedName{Name: dusName}, &got); err != nil {
		t.Fatalf("RNGD DUS 자동 생성 실패: %v", err)
	}

	if got.Spec.Model != model {
		t.Errorf("DUS.Spec.Model: got %q, want %q", got.Spec.Model, model)
	}
	if got.Status.CurrentVersion != version {
		t.Errorf("currentVersion: got %q, want %q", got.Status.CurrentVersion, version)
	}
}

// TestEnsureUpgradeStates_SkipsControlPlaneNode 는 control-plane 라벨이 있는 노드를 건너뛰는지 검증합니다.
func TestEnsureUpgradeStates_SkipsControlPlaneNode(t *testing.T) {
	const nodeName = "master-1"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
	}
	ndr := makeNDR(nodeName, "furiosa", "warboy", "1.9.8-3")
	dip := makeDIP("furiosa-warboy", "furiosa", "warboy", "1.9.8-3", "daemonset")

	r := newReconciler(node, ndr, dip)

	ctx := context.Background()
	if err := r.ensureUpgradeStates(ctx); err != nil {
		t.Fatalf("ensureUpgradeStates 오류: %v", err)
	}

	dusName := driverUpgradeStateName(nodeName, "furiosa")
	var got v1alpha1.DriverUpgradeState
	err := r.Get(ctx, types.NamespacedName{Name: dusName}, &got)
	if err == nil {
		t.Error("control-plane 노드에 DUS가 생성되어서는 안 됨")
	}
}

// ─────────────────────────────────────────────
// L4 stuck-label sweep / defer cleanup 시나리오
// ─────────────────────────────────────────────

// hasUpgradingLabel 은 노드의 driver-upgrading 라벨 보유 여부를 반환합니다.
func hasUpgradingLabel(t *testing.T, r *DriverUpgradeReconciler, nodeName string) bool {
	t.Helper()
	var node corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("노드 조회 실패: %v", err)
	}
	_, ok := node.Labels[upgrade.DriverUpgradingLabelKey]
	return ok
}

// TestSweepStuckUpgradingLabels_RemovesIdleStuckLabel 는 6일 invariant root cause 재현 시나리오:
// 사이클 mid-flight 에서 reconcile 이 panic 등으로 중단되어 DUS state 가 Idle 로 복귀했지만
// 노드에 driver-upgrading 라벨이 남아있는 경우, sweep 이 자동 제거하는지 검증한다.
func TestSweepStuckUpgradingLabels_RemovesIdleStuckLabel(t *testing.T) {
	const (
		nodeName = "worker-stuck"
		vendor   = "furiosa"
		dusName  = "worker-stuck-furiosa"
	)
	node := nodeWithUpgradingLabel(nodeName)
	dus := dusWithTransition(dusName, nodeName, vendor, v1alpha1.UpgradeStateIdle, 5*time.Minute)
	r := newReconciler(node, dus)

	if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
		t.Fatalf("sweep 오류: %v", err)
	}
	if hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("Idle + grace 경과 stuck 라벨이 제거되지 않음 (root cause 재발)")
	}
}

// TestSweepStuckUpgradingLabels_RemovesFailedStuckLabel 는 Failed 종료 상태 + grace 경과 시
// 라벨이 제거되는지 검증한다.
func TestSweepStuckUpgradingLabels_RemovesFailedStuckLabel(t *testing.T) {
	const (
		nodeName = "worker-failed"
		vendor   = "furiosa"
		dusName  = "worker-failed-furiosa"
	)
	node := nodeWithUpgradingLabel(nodeName)
	dus := dusWithTransition(dusName, nodeName, vendor, "Failed", 5*time.Minute)
	r := newReconciler(node, dus)

	if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
		t.Fatalf("sweep 오류: %v", err)
	}
	if hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("Failed + grace 경과 stuck 라벨이 제거되지 않음")
	}
}

// TestSweepStuckUpgradingLabels_PreservesActiveCycleLabel 는 mid-cycle (Cordoning, Draining,
// Upgrading, Validating, Uncordoning, Rollback, PreFlight, UpgradeRequired) 라벨은
// grace 경과와 무관하게 절대 제거하지 않음을 검증한다 — 정상 사이클 보호 invariant.
func TestSweepStuckUpgradingLabels_PreservesActiveCycleLabel(t *testing.T) {
	activeStates := []string{
		v1alpha1.UpgradeStateRequired,
		v1alpha1.UpgradeStatePreFlight,
		v1alpha1.UpgradeStateCordoning,
		v1alpha1.UpgradeStateDraining,
		v1alpha1.UpgradeStateUpgrading,
		v1alpha1.UpgradeStateValidating,
		v1alpha1.UpgradeStateUncordoning,
		v1alpha1.UpgradeStateRollback,
	}
	for _, st := range activeStates {
		t.Run(st, func(t *testing.T) {
			const (
				nodeName = "worker-active"
				vendor   = "furiosa"
				dusName  = "worker-active-furiosa"
			)
			node := nodeWithUpgradingLabel(nodeName)
			// 일부러 grace 를 한참 넘긴 시각 — mid-cycle 이므로 절대 제거되어선 안 됨
			dus := dusWithTransition(dusName, nodeName, vendor, st, 30*time.Minute)
			r := newReconciler(node, dus)

			if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
				t.Fatalf("sweep 오류: %v", err)
			}
			if !hasUpgradingLabel(t, r, nodeName) {
				t.Errorf("mid-cycle (%s) 라벨이 잘못 제거됨 — 정상 사이클 invariant 위반", st)
			}
		})
	}
}

// TestSweepStuckUpgradingLabels_PreservesWithinGracePeriod 는 DUS 가 종료 상태이지만
// grace period 이내인 경우 transient 보호를 위해 라벨을 제거하지 않음을 검증한다.
func TestSweepStuckUpgradingLabels_PreservesWithinGracePeriod(t *testing.T) {
	const (
		nodeName = "worker-fresh"
		vendor   = "furiosa"
		dusName  = "worker-fresh-furiosa"
	)
	node := nodeWithUpgradingLabel(nodeName)
	dus := dusWithTransition(dusName, nodeName, vendor, v1alpha1.UpgradeStateIdle, 5*time.Second)
	r := newReconciler(node, dus)

	if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
		t.Fatalf("sweep 오류: %v", err)
	}
	if !hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("grace period (30s) 이내 라벨이 잘못 제거됨 — transient 보호 위반")
	}
}

// TestSweepStuckUpgradingLabels_PreservesUnknownOwner 는 노드에 라벨이 있지만
// 매칭되는 DUS 가 하나도 없는 경우 (다른 컨트롤러 소유 가능성) 라벨을 보존함을 검증한다.
func TestSweepStuckUpgradingLabels_PreservesUnknownOwner(t *testing.T) {
	const nodeName = "worker-orphan"
	node := nodeWithUpgradingLabel(nodeName)
	r := newReconciler(node) // DUS 없음

	if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
		t.Fatalf("sweep 오류: %v", err)
	}
	if !hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("DUS 부재 노드의 라벨이 잘못 제거됨 — 미상 소유 보호 위반")
	}
}

// TestSweepStuckUpgradingLabels_MixedDUSStateOnSameNode 는 같은 노드에 vendor 가 다른
// 두 DUS 가 있고 한쪽이 mid-cycle 인 경우 보수적으로 라벨을 보존하는지 검증한다.
// (다른 vendor 가 사이클 진행 중이면 그 라벨은 정상 사용 중)
func TestSweepStuckUpgradingLabels_MixedDUSStateOnSameNode(t *testing.T) {
	const nodeName = "worker-mixed"
	node := nodeWithUpgradingLabel(nodeName)
	idleDUS := dusWithTransition("worker-mixed-furiosa", nodeName, "furiosa",
		v1alpha1.UpgradeStateIdle, 10*time.Minute)
	activeDUS := dusWithTransition("worker-mixed-nvidia", nodeName, "nvidia",
		v1alpha1.UpgradeStateUpgrading, 10*time.Minute)
	r := newReconciler(node, idleDUS, activeDUS)

	if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
		t.Fatalf("sweep 오류: %v", err)
	}
	if !hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("mid-cycle DUS 가 공존하는 노드의 라벨이 잘못 제거됨")
	}
}

// TestEnsureUpgradingLabelRemoved_Idempotent 는 라벨이 이미 없을 때 EnsureUpgradingLabelRemoved
// 가 에러 없이 no-op 으로 동작하는지 검증한다 (defer cleanup 의 안전성 보장).
func TestEnsureUpgradingLabelRemoved_Idempotent(t *testing.T) {
	const nodeName = "worker-clean"
	node := workerNode(nodeName) // 라벨 없음
	r := newReconciler(node)

	if err := r.StateMachine.EnsureUpgradingLabelRemoved(context.Background(), nodeName); err != nil {
		t.Fatalf("idempotent 호출 실패: %v", err)
	}
}

// TestEnsureUpgradingLabelRemoved_NodeNotFound 는 노드가 없는 경우 에러 없이 무시함을 검증한다.
func TestEnsureUpgradingLabelRemoved_NodeNotFound(t *testing.T) {
	r := newReconciler() // 노드 없음
	if err := r.StateMachine.EnsureUpgradingLabelRemoved(context.Background(), "nonexistent"); err != nil {
		t.Fatalf("NotFound 무시 실패: %v", err)
	}
}

// ─────────────────────────────────────────────
// 옵션 A: detector phase-aware blocking label 시나리오
// ─────────────────────────────────────────────

// nodeWithBothUpgradingLabels 는 driver-upgrading + driver-upgrading-blocking 라벨 둘 다 붙은
// 워커 노드를 반환한다. cordonNode() 가 추가하는 mid-cycle 상태를 시뮬레이트.
func nodeWithBothUpgradingLabels(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				upgrade.DriverUpgradingLabelKey:         "true",
				upgrade.DriverUpgradingBlockingLabelKey: "true",
			},
		},
	}
}

// hasBlockingLabel 은 노드의 driver-upgrading-blocking 라벨 보유 여부를 반환합니다.
func hasBlockingLabel(t *testing.T, r *DriverUpgradeReconciler, nodeName string) bool {
	t.Helper()
	var node corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("노드 조회 실패: %v", err)
	}
	_, ok := node.Labels[upgrade.DriverUpgradingBlockingLabelKey]
	return ok
}

// TestEnsureUpgradingBlockingLabelRemoved_RemovesOnlyBlocking 는 EnsureUpgradingBlockingLabelRemoved
// 가 driver-upgrading-blocking 라벨만 제거하고 driver-upgrading 라벨은 보존하는지 검증한다.
// (handleValidating 진입 시점의 핵심 동작 — detector 만 풀어주고 사이클 추적은 유지)
func TestEnsureUpgradingBlockingLabelRemoved_RemovesOnlyBlocking(t *testing.T) {
	const nodeName = "worker-validating-entry"
	node := nodeWithBothUpgradingLabels(nodeName)
	r := newReconciler(node)

	if err := r.StateMachine.EnsureUpgradingBlockingLabelRemoved(context.Background(), nodeName); err != nil {
		t.Fatalf("EnsureUpgradingBlockingLabelRemoved 실패: %v", err)
	}
	if hasBlockingLabel(t, r, nodeName) {
		t.Errorf("driver-upgrading-blocking 라벨이 제거되지 않음 (옵션 A 핵심 동작 깨짐)")
	}
	if !hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("driver-upgrading 라벨이 잘못 제거됨 — 사이클 추적 라벨은 보존되어야 함")
	}
}

// TestSweepStuckUpgradingLabels_BothLabels 는 두 라벨이 모두 stuck 인 노드에서 sweep 가
// 둘 다 정리하는지 검증한다.
func TestSweepStuckUpgradingLabels_BothLabels(t *testing.T) {
	const (
		nodeName = "worker-both-stuck"
		vendor   = "furiosa"
		dusName  = "worker-both-stuck-furiosa"
	)
	node := nodeWithBothUpgradingLabels(nodeName)
	// Idle 상태 + 5min 경과 — 두 라벨 grace (15s, 30s) 모두 초과
	dus := dusWithTransition(dusName, nodeName, vendor, v1alpha1.UpgradeStateIdle, 5*time.Minute)
	r := newReconciler(node, dus)

	if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
		t.Fatalf("sweep 오류: %v", err)
	}
	if hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("driver-upgrading 라벨이 제거되지 않음")
	}
	if hasBlockingLabel(t, r, nodeName) {
		t.Errorf("driver-upgrading-blocking 라벨이 제거되지 않음")
	}
}

// TestSweepStuckUpgradingLabels_BlockingShorterGrace 는 driver-upgrading-blocking 라벨이
// 더 짧은 grace (15s) 로 빠르게 sweep 되는지 검증한다.
//
// 시나리오: DUS 종료 상태로 20s 경과 → blocking grace (15s) 초과 + main grace (30s) 미달
// 기대: blocking 만 제거, main 은 보존 (다음 sweep 에서 풀림)
func TestSweepStuckUpgradingLabels_BlockingShorterGrace(t *testing.T) {
	const (
		nodeName = "worker-blocking-only"
		vendor   = "furiosa"
		dusName  = "worker-blocking-only-furiosa"
	)
	node := nodeWithBothUpgradingLabels(nodeName)
	// 20s 경과 — blocking grace(15s) 초과 + main grace(30s) 미달
	dus := dusWithTransition(dusName, nodeName, vendor, v1alpha1.UpgradeStateIdle, 20*time.Second)
	r := newReconciler(node, dus)

	if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
		t.Fatalf("sweep 오류: %v", err)
	}
	if hasBlockingLabel(t, r, nodeName) {
		t.Errorf("driver-upgrading-blocking 라벨이 짧은 grace 후에도 제거되지 않음")
	}
	if !hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("driver-upgrading 라벨이 30s 미달인데 잘못 제거됨")
	}
}

// TestSweepStuckUpgradingLabels_PreservesActiveCycleBothLabels 는 두 라벨이 모두 있는 mid-cycle
// 노드에서 sweep 가 둘 다 보존하는지 검증한다.
func TestSweepStuckUpgradingLabels_PreservesActiveCycleBothLabels(t *testing.T) {
	const (
		nodeName = "worker-active-both"
		vendor   = "furiosa"
		dusName  = "worker-active-both-furiosa"
	)
	node := nodeWithBothUpgradingLabels(nodeName)
	// mid-cycle (Cordoning) — grace 무관하게 절대 제거되어선 안 됨
	dus := dusWithTransition(dusName, nodeName, vendor, v1alpha1.UpgradeStateCordoning, 30*time.Minute)
	r := newReconciler(node, dus)

	if err := r.sweepStuckUpgradingLabels(context.Background()); err != nil {
		t.Fatalf("sweep 오류: %v", err)
	}
	if !hasUpgradingLabel(t, r, nodeName) {
		t.Errorf("mid-cycle (Cordoning) driver-upgrading 라벨이 잘못 제거됨")
	}
	if !hasBlockingLabel(t, r, nodeName) {
		t.Errorf("mid-cycle (Cordoning) driver-upgrading-blocking 라벨이 잘못 제거됨")
	}
}
