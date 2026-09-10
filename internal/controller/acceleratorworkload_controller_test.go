// ============================================================
// acceleratorworkload_controller_test.go: AcceleratorWorkload 컨트롤러 테스트
// 상세: 번역 성공 시 Deployment 렌더(리소스 limit + nodeAffinity), 거절 시 status Condition,
// 재조정 멱등성(API server 기본값 주입 후에도 재기록 없음), 남의 Deployment 미탈취.
// 생성일: 2026-07-30 | 수정일: 2026-07-30
// ============================================================
package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/intent"
	"kcloud-operator/internal/upgrade"
)

func awScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, npuv1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

func awFixture() *npuv1alpha1.AcceleratorWorkload {
	reps := int32(2)
	return &npuv1alpha1.AcceleratorWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "default", UID: "aw-uid"},
		Spec: npuv1alpha1.AcceleratorWorkloadSpec{
			Accelerator: npuv1alpha1.AcceleratorRequest{
				Class:  "inference-medium",
				Access: npuv1alpha1.AccessSpec{Mode: npuv1alpha1.AccessModeExclusive},
			},
			Workload: npuv1alpha1.WorkloadTemplate{
				Image: "registry.example.com:5000/kcloud/inference:v1", Replicas: &reps,
				Command: []string{"python", "inference.py"},
			},
		},
	}
}

func awClass() *npuv1alpha1.AcceleratorClass {
	return &npuv1alpha1.AcceleratorClass{
		ObjectMeta: metav1.ObjectMeta{Name: "inference-medium"},
		Spec:       npuv1alpha1.AcceleratorClassSpec{Mappings: []npuv1alpha1.AcceleratorMapping{{Vendor: "nvidia"}}},
	}
}

func awNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")}}}
}

func newAWReconciler(t *testing.T, objs ...client.Object) (*AcceleratorWorkloadReconciler, client.Client) {
	t.Helper()
	s := awScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&npuv1alpha1.AcceleratorWorkload{}).Build()
	return &AcceleratorWorkloadReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(16)}, c
}

func awReconcile(t *testing.T, r *AcceleratorWorkloadReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "img", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func awGetDeployment(t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "img", Namespace: "default"}, &dep); err != nil {
		t.Fatalf("deployment not found: %v", err)
	}
	return &dep
}

func awGet(t *testing.T, c client.Client) *npuv1alpha1.AcceleratorWorkload {
	t.Helper()
	var aw npuv1alpha1.AcceleratorWorkload
	if err := c.Get(context.Background(), types.NamespacedName{Name: "img", Namespace: "default"}, &aw); err != nil {
		t.Fatalf("get aw: %v", err)
	}
	return &aw
}

func TestAcceleratorWorkloadCreatesDeployment(t *testing.T) {
	r, c := newAWReconciler(t, awFixture(), awClass(), awNode())
	awReconcile(t, r)
	dep := awGetDeployment(t, c)
	if *dep.Spec.Replicas != 2 {
		t.Fatalf("replicas=%d want 2", *dep.Spec.Replicas)
	}
	q := dep.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
	if q.Value() != 1 {
		t.Fatalf("gpu limit=%s want 1", q.String())
	}
	terms := dep.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || terms[0].MatchExpressions[0].Values[0] != "worker1" {
		t.Fatalf("node affinity wrong: %+v", terms)
	}
	if dep.Spec.Template.Labels[podInjectLabel] != labelValueTrue {
		t.Fatalf("inject label missing: %v", dep.Spec.Template.Labels)
	}
	if len(dep.OwnerReferences) != 1 || dep.OwnerReferences[0].Kind != "AcceleratorWorkload" {
		t.Fatalf("owner reference wrong: %+v", dep.OwnerReferences)
	}

	aw := awGet(t, c)
	if aw.Status.Phase != npuv1alpha1.AWPhaseTranslated {
		t.Fatalf("phase=%q want Translated", aw.Status.Phase)
	}
	if aw.Status.Resolved == nil || aw.Status.Resolved.ResourceName != "nvidia.com/gpu" {
		t.Fatalf("resolved wrong: %+v", aw.Status.Resolved)
	}
	if !apimeta.IsStatusConditionTrue(aw.Status.Conditions, npuv1alpha1.AWCondTranslated) {
		t.Fatalf("Translated condition not true: %+v", aw.Status.Conditions)
	}
	if aw.Status.ObservedGeneration != aw.Generation {
		t.Fatalf("observedGeneration=%d want %d", aw.Status.ObservedGeneration, aw.Generation)
	}
}

// 같은 spec 을 다시 조정해도 Deployment 를 다시 쓰지 않는다. API server 가 채워 넣는 기본값
// (imagePullPolicy·dnsPolicy·strategy 등)을 흉내 낸 뒤에도 그대로여야 한다 — 이 비교를 빼먹으면
// 렌더값과 저장값이 영원히 달라 보여 재조정마다 Update 가 나가고 Pod 이 재시작한다.
func TestAcceleratorWorkloadIsIdempotent(t *testing.T) {
	r, c := newAWReconciler(t, awFixture(), awClass(), awNode())
	awReconcile(t, r)

	dep := awGetDeployment(t, c)
	dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType}
	dep.Spec.RevisionHistoryLimit = ptrInt32(10)
	dep.Spec.ProgressDeadlineSeconds = ptrInt32(600)
	dep.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	dep.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	dep.Spec.Template.Spec.SchedulerName = "default-scheduler"
	dep.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	dep.Spec.Template.Spec.Containers[0].TerminationMessagePath = corev1.TerminationMessagePathDefault
	dep.Spec.Template.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	if err := c.Update(context.Background(), dep); err != nil {
		t.Fatalf("simulate defaulting: %v", err)
	}
	before := awGetDeployment(t, c).ResourceVersion

	awReconcile(t, r)
	if after := awGetDeployment(t, c).ResourceVersion; after != before {
		t.Fatalf("deployment rewritten on unchanged reconcile: rv %s → %s", before, after)
	}

	// 반대 방향: spec 이 실제로 바뀌면 반영돼야 한다(멱등성을 "아무것도 안 함" 으로 오해하지 않도록).
	aw := awGet(t, c)
	aw.Spec.Workload.Image = "registry.example.com:5000/kcloud/inference:v2"
	if err := c.Update(context.Background(), aw); err != nil {
		t.Fatalf("update aw: %v", err)
	}
	awReconcile(t, r)
	if got := awGetDeployment(t, c).Spec.Template.Spec.Containers[0].Image; got != "registry.example.com:5000/kcloud/inference:v2" {
		t.Fatalf("image not propagated: %q", got)
	}
}

func ptrInt32(v int32) *int32 { return &v }

// 남이 만든 같은 이름의 Deployment 를 뺏지 않는다.
func TestAcceleratorWorkloadDoesNotAdoptForeignDeployment(t *testing.T) {
	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "default", Labels: map[string]string{"owner": "someone-else"}},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "legacy"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "legacy"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "legacy", Image: "legacy:v1"}}},
			},
		},
	}
	r, c := newAWReconciler(t, awFixture(), awClass(), awNode(), foreign)
	awReconcile(t, r)

	dep := awGetDeployment(t, c)
	if dep.Spec.Template.Spec.Containers[0].Image != "legacy:v1" || len(dep.OwnerReferences) != 0 {
		t.Fatalf("foreign deployment was hijacked: %+v", dep.Spec.Template.Spec.Containers[0])
	}
	aw := awGet(t, c)
	cond := apimeta.FindStatusCondition(aw.Status.Conditions, npuv1alpha1.AWCondWorkloadReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != npuv1alpha1.AWReasonDeploymentConflict {
		t.Fatalf("conflict not reported: %+v", cond)
	}
}

// webhook 이 꺼져 있거나 통과된 뒤 클러스터가 바뀌어도 컨트롤러가 최종 판정을 남긴다.
func TestAcceleratorWorkloadRejectsAndReportsAxis(t *testing.T) {
	aw := awFixture()
	aw.Spec.Accelerator.Access = npuv1alpha1.AccessSpec{Mode: npuv1alpha1.AccessModeShared, Replicas: 4}
	r, c := newAWReconciler(t, aw, awClass(), awNode())
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "img", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile returned an error instead of recording a rejection: %v", err)
	}
	got := awGet(t, c)
	if got.Status.Phase != npuv1alpha1.AWPhaseRejected {
		t.Fatalf("phase=%q want Rejected", got.Status.Phase)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, npuv1alpha1.AWCondTranslated)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Translated condition wrong: %+v", cond)
	}
	if cond.Reason != npuv1alpha1.AWReasonCapabilityUnverified && cond.Reason != npuv1alpha1.AWReasonBackendUnsupported {
		t.Fatalf("reason=%q must name the failing axis", cond.Reason)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "img", Namespace: "default"}, &dep); err == nil {
		t.Fatal("a rejected workload must not get a Deployment")
	}
}

// 승인 이후 클러스터가 변하면(노드 소멸) 다음 조정이 stale 한 Ready 를 남기지 않는다.
func TestAcceleratorWorkloadRedecidesWhenCapabilityDisappears(t *testing.T) {
	node := awNode()
	r, c := newAWReconciler(t, awFixture(), awClass(), node)
	awReconcile(t, r)
	if awGet(t, c).Status.Phase != npuv1alpha1.AWPhaseTranslated {
		t.Fatal("precondition: first reconcile must translate")
	}
	// Pod 이 실제로 떠서 Running/WorkloadReady=True 가 된 상태를 만든다. 이 단계를 빼면 조건은
	// 처음부터 False 라 "거절인데 Ready 로 남는다" 는 회귀를 잡지 못한다.
	dep := awGetDeployment(t, c)
	dep.Status.Replicas, dep.Status.ReadyReplicas = 2, 2
	if err := c.Status().Update(context.Background(), dep); err != nil {
		t.Fatalf("set deployment status: %v", err)
	}
	awReconcile(t, r)
	if got := awGet(t, c); got.Status.Phase != npuv1alpha1.AWPhaseRunning ||
		!apimeta.IsStatusConditionTrue(got.Status.Conditions, npuv1alpha1.AWCondWorkloadReady) {
		t.Fatalf("precondition: workload must be Running/Ready first: %+v", got.Status)
	}

	if err := c.Delete(context.Background(), node); err != nil {
		t.Fatalf("delete node: %v", err)
	}
	awReconcile(t, r)
	got := awGet(t, c)
	if got.Status.Phase != npuv1alpha1.AWPhaseRejected {
		t.Fatalf("phase=%q want Rejected after the last candidate node disappeared", got.Status.Phase)
	}
	if got.Status.Resolved != nil {
		t.Fatalf("stale resolved allocation kept: %+v", got.Status.Resolved)
	}
	// spec 이 안 바뀌었으므로 observedGeneration 으로는 낡음을 알 수 없다 — 조건 자체가 내려가야 한다.
	ready := apimeta.FindStatusCondition(got.Status.Conditions, npuv1alpha1.AWCondWorkloadReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("rejected workload still reports WorkloadReady: %+v", ready)
	}
}

// driver upgrade quiesce(A6.1)가 scale=0 으로 내린 워크로드를 되살리지 않는다. rmmod 창 안에서
// GPU 를 쥔 Pod 을 다시 띄우면 드라이버 교체가 깨진다. 라벨도 통째로 갈아엎지 않는다 —
// quiesce 대상 선정이 그 라벨로 이뤄지므로 지우면 다시는 quiesce 되지 않는다.
func TestAcceleratorWorkloadRespectsQuiesce(t *testing.T) {
	r, c := newAWReconciler(t, awFixture(), awClass(), awNode())
	awReconcile(t, r)

	dep := awGetDeployment(t, c)
	dep.Labels[upgrade.QuiesceOnDriverUpgradeLabelKey] = labelValueTrue
	dep.Annotations = map[string]string{upgrade.QuiesceReplicasBackupAnnotation: "2"}
	dep.Spec.Replicas = ptrInt32(0)
	if err := c.Update(context.Background(), dep); err != nil {
		t.Fatalf("simulate quiesce: %v", err)
	}

	awReconcile(t, r)
	got := awGetDeployment(t, c)
	if *got.Spec.Replicas != 0 {
		t.Fatalf("quiesced deployment scaled back up to %d during the driver rmmod window", *got.Spec.Replicas)
	}
	if got.Labels[upgrade.QuiesceOnDriverUpgradeLabelKey] != labelValueTrue {
		t.Fatalf("quiesce opt-in label wiped: %v", got.Labels)
	}
	if got.Labels[awWorkloadLabel] != "img" {
		t.Fatalf("own label lost: %v", got.Labels)
	}
}

func TestAcceleratorWorkloadMissingClass(t *testing.T) {
	r, c := newAWReconciler(t, awFixture(), awNode())
	awReconcile(t, r)
	cond := apimeta.FindStatusCondition(awGet(t, c).Status.Conditions, npuv1alpha1.AWCondTranslated)
	if cond == nil || cond.Reason != npuv1alpha1.AWReasonClassNotFound {
		t.Fatalf("missing class not reported: %+v", cond)
	}
}

func TestRenderDeploymentIsDeterministic(t *testing.T) {
	res := &intent.Result{Vendor: "nvidia", Mode: npuv1alpha1.AccessModePartitioned,
		ResourceName: "nvidia.com/mig-1g.6gb", Quantity: 1, Nodes: []string{"worker1"}}
	a := renderDeployment(awFixture(), res)
	b := renderDeployment(awFixture(), res)
	if a.Spec.Template.Spec.Containers[0].Image != b.Spec.Template.Spec.Containers[0].Image {
		t.Fatal("render is not deterministic")
	}
	if _, ok := a.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/mig-1g.6gb"]; !ok {
		t.Fatalf("mig resource missing: %+v", a.Spec.Template.Spec.Containers[0].Resources.Limits)
	}
}

// MIG 워크로드는 runtimeClass 를 렌더 시점에 직접 받아야 한다. PodMutator 는 차트 기본값
// webhook.enabled=false 면 아예 불리지 않고, 그러면 컨테이너는 /dev/nvidia* 없이 뜬 채
// Ready 가 되어 status 가 Running 이라고 거짓말한다.
func TestRenderDeploymentSetsNvidiaRuntimeClassWithoutWebhook(t *testing.T) {
	partitioned := renderDeployment(awFixture(), &intent.Result{Vendor: "nvidia", Mode: npuv1alpha1.AccessModePartitioned,
		ResourceName: "nvidia.com/mig-1g.6gb", Quantity: 1, Nodes: []string{"worker1"}})
	if rc := partitioned.Spec.Template.Spec.RuntimeClassName; rc == nil || *rc != vendorNvidia {
		t.Fatalf("partitioned nvidia workload has no runtimeClass: %v", rc)
	}
	// 다른 벤더는 이 클러스터의 기본 런타임으로 뜬다 — 있지도 않은 runtimeClass 를 박지 않는다.
	rngd := renderDeployment(awFixture(), &intent.Result{Vendor: "furiosa", Mode: npuv1alpha1.AccessModeExclusive,
		ResourceName: "furiosa.ai/rngd", Quantity: 1, Nodes: []string{"rngd-1"}})
	if rc := rngd.Spec.Template.Spec.RuntimeClassName; rc != nil {
		t.Fatalf("non-nvidia workload pinned to runtimeClass %q", *rc)
	}
}

// 이 수정 이전에 만들어진 Deployment(runtimeClass 없음)도 다음 조정에서 맞춰져야 한다.
func TestAcceleratorWorkloadBackfillsRuntimeClass(t *testing.T) {
	r, c := newAWReconciler(t, awFixture(), awClass(), awNode())
	awReconcile(t, r)
	dep := awGetDeployment(t, c)
	dep.Spec.Template.Spec.RuntimeClassName = nil
	if err := c.Update(context.Background(), dep); err != nil {
		t.Fatalf("simulate pre-fix deployment: %v", err)
	}
	awReconcile(t, r)
	if rc := awGetDeployment(t, c).Spec.Template.Spec.RuntimeClassName; rc == nil || *rc != vendorNvidia {
		t.Fatalf("runtimeClass not backfilled: %v", rc)
	}
}

// 재시도해도 소용없는 apply 실패는 백오프에 묻지 않고 CR 에 사유를 남긴다.
func TestAcceleratorWorkloadReportsNonRetryableApplyFailure(t *testing.T) {
	s := awScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(awFixture(), awClass(), awNode()).
		WithStatusSubresource(&npuv1alpha1.AcceleratorWorkload{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return apierrors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, obj.GetName(),
						field.ErrorList{field.Invalid(field.NewPath("spec"), "", "bad resource name")})
				}
				return nil
			},
		}).Build()
	r := &AcceleratorWorkloadReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(16)}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "img", Namespace: "default"}}); err != nil {
		t.Fatalf("non-retryable failure returned as error instead of status: %v", err)
	}
	ready := apimeta.FindStatusCondition(awGet(t, c).Status.Conditions, npuv1alpha1.AWCondWorkloadReady)
	if ready == nil || ready.Reason != npuv1alpha1.AWReasonDeploymentInvalid {
		t.Fatalf("apply failure never reached the CR: %+v", ready)
	}
}

// 영구 거절은 2분마다 같은 Warning 을 다시 내지 않는다(하루 ~720개).
// 축은 intent.Reject 에 구조로 이미 있다 — message 에 접어 넣고 정규식으로 되짚게 두지 않는다.
func TestAcceleratorWorkload_RejectionIsStructured(t *testing.T) {
	aw := awFixture()
	aw.Spec.Accelerator.Access = npuv1alpha1.AccessSpec{Mode: npuv1alpha1.AccessModeShared, Replicas: 4}
	r, c := newAWReconciler(t, aw, awClass(), awNode())
	awReconcile(t, r)

	got := awGet(t, c)
	if got.Status.Rejection == nil {
		t.Fatal("거절인데 status.rejection 이 비었다")
	}
	if got.Status.Rejection.Axis == "" {
		t.Error("축이 비었다 — 사용자가 어느 필드를 고칠지 알 수 없다")
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, npuv1alpha1.AWCondTranslated); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Error("Translated=False 조건이 사라졌다")
	}
}

// 거절됐다가 고쳐서 수용되면 거절 흔적이 남아 있으면 안 된다.
func TestAcceleratorWorkload_RejectionClearedOnAccept(t *testing.T) {
	aw := awFixture()
	aw.Spec.Accelerator.Access = npuv1alpha1.AccessSpec{Mode: npuv1alpha1.AccessModeShared, Replicas: 4}
	r, c := newAWReconciler(t, aw, awClass(), awNode())
	awReconcile(t, r)
	if awGet(t, c).Status.Rejection == nil {
		t.Fatal("사전조건 실패: 거절 상태가 아니다")
	}

	fixed := awGet(t, c)
	fixed.Spec.Accelerator.Access = npuv1alpha1.AccessSpec{Mode: npuv1alpha1.AccessModeExclusive}
	if err := c.Update(context.Background(), fixed); err != nil {
		t.Fatalf("update aw: %v", err)
	}
	awReconcile(t, r)

	if got := awGet(t, c).Status.Rejection; got != nil {
		t.Fatalf("수용됐는데 거절이 남아 있다: %+v", got)
	}
}

func TestAcceleratorWorkloadEmitsRejectEventOnlyOnChange(t *testing.T) {
	aw := awFixture()
	aw.Spec.Accelerator.Access = npuv1alpha1.AccessSpec{Mode: npuv1alpha1.AccessModeShared, Replicas: 4}
	rec := record.NewFakeRecorder(16)
	s := awScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(aw, awClass(), awNode()).
		WithStatusSubresource(&npuv1alpha1.AcceleratorWorkload{}).Build()
	r := &AcceleratorWorkloadReconciler{Client: c, Scheme: s, Recorder: rec}
	for i := 0; i < 3; i++ {
		awReconcile(t, r)
	}
	if got := len(rec.Events); got != 1 {
		t.Fatalf("3 reconciles of one permanent rejection emitted %d events, want 1", got)
	}
}
