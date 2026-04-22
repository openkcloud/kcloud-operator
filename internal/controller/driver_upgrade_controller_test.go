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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "kcloud-operator/api/v1alpha1"
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
	return &DriverUpgradeReconciler{Client: c, Scheme: s}
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
