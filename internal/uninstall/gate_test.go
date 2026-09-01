// ============================================================
// gate_test.go: pre-delete 게이트의 판정과 삭제 순서를 고정한다.
// 상세: 게이트가 틀리는 방향은 둘뿐이다. 사용 중인데 지워 버리거나, 비었는데
//
//	보류해서 삭제가 끝나지 않거나. 두 방향을 각각 시험한다.
//
// 생성일: 2026-09-10 | 수정일: 2026-09-10
// ============================================================
package uninstall

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/naming"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	if err := npuv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("npuv1alpha1: %v", err)
	}
	return s
}

func gpuPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "app",
			Image: "busybox",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"nvidia.com/gpu": resource.MustParse("1"),
			}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func policyObjects() []client.Object {
	return []client.Object{
		&npuv1alpha1.NPUClusterPolicy{ObjectMeta: metav1.ObjectMeta{Name: "cluster-policy"}},
		&npuv1alpha1.DriverInstallPolicy{ObjectMeta: metav1.ObjectMeta{Name: "nvidia"}},
	}
}

func testOptions() Options {
	return Options{Timeout: 50 * time.Millisecond, Poll: 5 * time.Millisecond}
}

// TestGateHoldsWhenPodUsesAccelerator: 가속기를 쓰는 Pod 가 있으면 error 이고
// CR 은 하나도 지워지지 않아야 한다. 보류가 곧 "아무것도 안 했다" 여야 한다.
func TestGateHoldsWhenPodUsesAccelerator(t *testing.T) {
	objs := append(policyObjects(), gpuPod("team-a", "trainer"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	var out bytes.Buffer
	err := Execute(context.Background(), c, testOptions(), &out)
	if err == nil {
		t.Fatal("사용 중인데 게이트가 통과했다")
	}
	if !strings.Contains(out.String(), "team-a") || !strings.Contains(out.String(), "trainer") {
		t.Errorf("보류 사유 표에 Pod 가 없다:\n%s", out.String())
	}

	var pols npuv1alpha1.NPUClusterPolicyList
	if err := c.List(context.Background(), &pols); err != nil {
		t.Fatal(err)
	}
	if len(pols.Items) != 1 {
		t.Errorf("보류인데 NPUClusterPolicy 가 %d 개 남았다 — 지우면 안 된다", len(pols.Items))
	}
}

// TestGateIgnoresTerminatedPod: 끝난 Pod 는 장치를 잡고 있지 않으므로 보류 사유가
// 아니다. 이걸 세면 배치 작업을 한 번 돌린 클러스터는 영영 못 지운다.
func TestGateIgnoresTerminatedPod(t *testing.T) {
	done := gpuPod("team-a", "finished")
	done.Status.Phase = corev1.PodSucceeded
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(done).Build()

	users, err := Consumers(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Errorf("종료된 Pod 를 사용 중으로 셌다: %+v", users)
	}
}

// TestGateCountsResourceClaim: DRA 로 장치를 받은 Pod 는 requests/limits 가 비어
// 있다. claim 참조를 보지 않으면 DRA 클러스터에서 게이트가 전부 통과한다.
func TestGateCountsResourceClaim(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "dra-user"},
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "app", Image: "busybox"}},
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu0"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()

	users, err := Consumers(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || !strings.Contains(users[0].Resource, "gpu0") {
		t.Fatalf("DRA claim 사용을 못 셌다: %+v", users)
	}
}

// TestGateDeletesEverythingWhenIdle: 사용 중인 Pod 가 없으면 CR 도 DaemonSet 도
// 남지 않아야 한다. 이름으로 집는 것과 라벨로 집는 것 양쪽을 함께 둔다.
func TestGateDeletesEverythingWhenIdle(t *testing.T) {
	byName := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: naming.KubeSystemNamespace, Name: "nvidia-device-plugin"}}
	byLabel := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: naming.KubeSystemNamespace, Name: "kcloud-nvidia-driver",
		Labels: map[string]string{componentLabel: "driver"}}}
	// 벤더 device-plugin 부속 오브젝트. ownerReference 도 component 라벨도 없고
	// npu.ai/owner 어노테이션만 있다(라이브 kind 에서 릴리스 삭제 후 남은 것들).
	owned := map[string]string{ownerAnnotation: "kcloud/npuclusterpolicy-sample"}
	vendorCR := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name: "rbln-device-plugin", Annotations: owned}}
	vendorSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: naming.KubeSystemNamespace, Name: "rbln-device-plugin", Annotations: owned}}
	vendorCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: naming.KubeSystemNamespace, Name: "rbln-device-plugin-config", Annotations: owned}}
	// 남의 오브젝트. 표시가 없으므로 건드리면 안 된다.
	foreignCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: naming.KubeSystemNamespace, Name: "kube-root-ca.crt"}}
	// operator 가 만드는 Job 에는 ownerReference 가 없어 CR 을 지워도 남는다.
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: naming.KubeSystemNamespace, Name: "acpp-mig-observe-standing-0",
		Labels: map[string]string{componentLabel: "mig-observe"}}}
	idle := gpuPod("team-a", "cpu-only")
	idle.Spec.Containers[0].Resources = corev1.ResourceRequirements{}

	objs := append(policyObjects(), byName, byLabel, job, idle, vendorCR, vendorSA, vendorCM, foreignCM)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	var out bytes.Buffer
	if err := Execute(context.Background(), c, testOptions(), &out); err != nil {
		t.Fatalf("비어 있는데 게이트가 막았다: %v\n%s", err, out.String())
	}

	var pols npuv1alpha1.NPUClusterPolicyList
	if err := c.List(context.Background(), &pols); err != nil {
		t.Fatal(err)
	}
	if len(pols.Items) != 0 {
		t.Errorf("NPUClusterPolicy 가 %d 개 남았다", len(pols.Items))
	}
	var dips npuv1alpha1.DriverInstallPolicyList
	if err := c.List(context.Background(), &dips); err != nil {
		t.Fatal(err)
	}
	if len(dips.Items) != 0 {
		t.Errorf("DriverInstallPolicy 가 %d 개 남았다", len(dips.Items))
	}
	var dss appsv1.DaemonSetList
	if err := c.List(context.Background(), &dss); err != nil {
		t.Fatal(err)
	}
	if len(dss.Items) != 0 {
		t.Errorf("DaemonSet 이 %d 개 남았다: %+v", len(dss.Items), dss.Items)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("Job 이 %d 개 남았다: %+v", len(jobs.Items), jobs.Items)
	}
	var crs rbacv1.ClusterRoleList
	if err := c.List(context.Background(), &crs); err != nil {
		t.Fatal(err)
	}
	if len(crs.Items) != 0 {
		t.Errorf("ClusterRole 이 %d 개 남았다", len(crs.Items))
	}
	var sas corev1.ServiceAccountList
	if err := c.List(context.Background(), &sas); err != nil {
		t.Fatal(err)
	}
	if len(sas.Items) != 0 {
		t.Errorf("ServiceAccount 가 %d 개 남았다", len(sas.Items))
	}
	var cms corev1.ConfigMapList
	if err := c.List(context.Background(), &cms); err != nil {
		t.Fatal(err)
	}
	if len(cms.Items) != 1 || cms.Items[0].Name != "kube-root-ca.crt" {
		t.Errorf("표시 없는 남의 ConfigMap 을 지웠거나 표시 있는 것을 남겼다: %+v", cms.Items)
	}
}

// TestWaitGoneStripsStuckFinalizer: operator 가 이미 죽어 finalizer 를 처리할 주체가
// 없으면 CR 이 Terminating 으로 고착한다. 상한을 넘기면 벗겨서 지워야 삭제가 끝난다.
func TestWaitGoneStripsStuckFinalizer(t *testing.T) {
	stuck := &npuv1alpha1.NPUClusterPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "cluster-policy", Finalizers: []string{"npu.ai/cleanup"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(stuck).Build()

	var out bytes.Buffer
	if err := Execute(context.Background(), c, testOptions(), &out); err != nil {
		t.Fatalf("게이트 실패: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "finalizer") {
		t.Errorf("finalizer 제거 경고가 없다:\n%s", out.String())
	}
	var pols npuv1alpha1.NPUClusterPolicyList
	if err := c.List(context.Background(), &pols); err != nil {
		t.Fatal(err)
	}
	if len(pols.Items) != 0 {
		t.Errorf("finalizer 를 벗겼는데 %d 개 남았다", len(pols.Items))
	}
}

// TestCRTiersCoverEveryKind: scheme 에 등록된 npu.ai CR 이 순서 목록에서 하나도
// 빠지지 않아야 한다. CRD 를 늘리면서 여기 안 넣으면 그 CR 만 조용히 살아남는다.
func TestCRTiersCoverEveryKind(t *testing.T) {
	s := testScheme(t)
	seen := map[string]bool{}
	for _, tier := range crTiers(s) {
		for _, k := range tier {
			seen[k] = true
		}
	}
	for _, k := range crKinds(s) {
		if !seen[k] {
			t.Errorf("CR 종류 %s 가 삭제 순서에서 빠졌다", k)
		}
	}
	if len(crKinds(s)) < 10 {
		t.Errorf("npu.ai CR 종류가 %d 개로 셌다 — scheme 필터가 너무 좁다", len(crKinds(s)))
	}
}
