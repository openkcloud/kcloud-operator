// ============================================================
// acpp_sharing_test.go: ACPP sharing 경로 단위 테스트
// 상세: apply → allocatable 수렴(fake verifier) → Ready, 미수렴 → rollback → Restored,
//
//	저널 일치 재-reconcile 은 DP 를 다시 재시작하지 않음(멱등).
//
// 생성일: 2026-07-29 | 수정일: 2026-07-31
// ============================================================
package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition"
	"kcloud-operator/internal/partition/nvidia"
)

// allocVerifier 는 노드 allocatable 스냅샷을 흉내내는 검증 seam 이다 — expected 를 모두 충족하면 수렴.
type allocVerifier struct{ alloc map[string]int32 }

func (v allocVerifier) VerifyAllocatable(_ partition.Target, expected map[string]int32) (*partition.VerifyResult, error) {
	for res, want := range expected {
		if v.alloc[res] < want {
			return &partition.VerifyResult{AllocatableConverged: false, Snapshot: v.alloc}, nil
		}
	}
	return &partition.VerifyResult{AllocatableConverged: true, Snapshot: v.alloc}, nil
}

func (v allocVerifier) VerifyAllocation(partition.Target, string) (*partition.VerifyResult, error) {
	return &partition.VerifyResult{TestPodAllocated: true}, nil
}

func sharingScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = npuv1alpha1.AddToScheme(s)
	_ = clientgoscheme.AddToScheme(s)
	return s
}

// readyMpsDaemon 은 mps-control-daemon DS 를 "대상 노드(worker1)에서 Ready" 상태로 미리 만들어
// 둔다. fake client 에는 daemonset 컨트롤러가 없어 DS status 도 pod 도 저절로 생기지 않으므로,
// 준비 게이트(review 최종 ③)를 통과해야 하는 케이스는 픽스처가 그 사실을 명시해야 한다 —
// 게이트를 우회하는 게 아니라 "daemon 이 실제로 떠 있는 클러스터" 를 표현하는 것이다.
func readyMpsDaemon() []client.Object {
	ds := renderMpsControlDaemonDS()
	ds.Status = appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1}
	return []client.Object{ds, mpsDaemonPod("worker1", true)}
}

// mpsDaemonPod 는 daemon DS 가 노드에 낳은 pod 하나를 만든다 — 네임스페이스와 라벨은 kubelet 이
// 그러는 것처럼 렌더러에서 파생한다(복제하면 렌더러 라벨 변경이 게이트를 깨도 초록으로 남는다).
func mpsDaemonPod(node string, ready bool) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	ds := renderMpsControlDaemonDS()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: ds.Name + "-" + node, Namespace: ds.Namespace,
			Labels: ds.Spec.Template.Labels,
		},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "mps-control-daemon", Image: "x"}}},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
		},
	}
}

// sharingFixture 는 DP DaemonSet + DP pod + ACPP 로 구성된 sharing 테스트 환경을 만든다.
// extra 는 케이스별 추가 객체다(예: 이미 Ready 인 mps control daemon).
//
// worker1 은 mig-active 라벨이 없는(flat) 노드다 — 이 픽스처가 만드는 DS 도 flat 이름
// (nvidia.DevicePluginNameFlat)이라 DevicePluginTargetForNode(Task 3)의 라우팅과 일치한다.
// 여기 모인 spec 들은 mps 요청이 apply 단계까지 도달해야 관측할 수 있는 것들이고(저널 순서·daemon
// 게이트·해제 경로), 노드가 mig-active 면 mpsBlockedByMigStrategy(Task 5)가 그보다 먼저 노드 단위로
// 거절하기 때문이다 — 그 거절 판정 자체는 프로덕션 렌더러가 만든 DS/라벨로 envtest 가 고정한다
// (acceleratorpartitionpolicy_controller_test.go, "D-8"·"D-8 root fix"). 이 노드에 mig-active
// 라벨을 걸면 아래 mps spec 들이 한꺼번에 Failed 로 깨져 그 사실을 알린다.
func sharingFixture(t *testing.T, acpp *npuv1alpha1.AcceleratorPartitionPolicy, alloc map[string]int32, extra ...client.Object) (*AcceleratorPartitionPolicyReconciler, client.Client) {
	t.Helper()
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "nvidia-device-plugin"}},
		}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nvidia-device-plugin-abcde", Namespace: "kube-system",
			// restartNvidiaDevicePlugin 은 mixed/flat 공통 라벨(nvidiaDevicePluginVendorLabel)로 List 한다(C-1).
			Labels: map[string]string{"app.kubernetes.io/name": "nvidia-device-plugin", nvidiaDevicePluginVendorLabel: "nvidia"},
		},
		Spec: corev1.PodSpec{NodeName: "worker1", Containers: []corev1.Container{{Name: "c", Image: "x"}}},
	}
	node := &corev1.Node{
		// mig-active 라벨을 걸지 않는다 — DevicePluginTargetForNode(Task 3) 는 그 부재를 flat 으로 읽어
		// 아래 flat DS 를 대상화한다. mig-active=true 를 걸면 mpsBlockedByMigStrategy(Task 5)가 이
		// 노드의 모든 mps 요청을 무조건 거절해, 이 파일이 exercise 하려는 apply 경로 심층 로직
		// (저널 순서·daemon 게이트·거절-후-청소)에 이 fixture 로는 영영 도달할 수 없다 — 그 게이트
		// 자체는 acceleratorpartitionpolicy_controller_test.go 의 "D-8" spec 들이 프로덕션 렌더러로 고정한다.
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker1",
			Annotations: map[string]string{migOwnerAnnotation: string(acpp.UID)},
		},
		Spec: corev1.NodeSpec{Unschedulable: true}, // 삭제 경로(quiesce 전제)용 — apply 경로는 노드를 안 본다.
	}
	// NDR — nvidia 경로(Discover/Diff)가 읽는 관측 소스. geometry 는 요청 layout 과 일치시킨다.
	ndr := &npuv1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: npuv1alpha1.NodeDeviceReportStatus{Devices: []npuv1alpha1.DeviceEntry{{
			Vendor: "nvidia", Model: "NVIDIA-A30", PCIeAddress: sharingPCI, DriverVersion: "580.65.06",
			MigModeCurrent: "Enabled", MigCurrentGeometry: "1g.6gb x4",
			MigLgipOutput: "|   0  MIG 1g.6gb   19  4/4  5.75 |",
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(sharingScheme()).
		WithObjects(append([]client.Object{ds, pod, node, ndr, acpp}, extra...)...).
		WithStatusSubresource(&npuv1alpha1.AcceleratorPartitionPolicy{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).Build()
	return &AcceleratorPartitionPolicyReconciler{Client: c, Scheme: c.Scheme(), Verifier: allocVerifier{alloc: alloc}}, c
}

// sharedACPP 는 4배수 time-slicing 을 요청하는 nvidia ACPP 다(파티션은 이미 Ready 저널).
const sharingPCI = "0000:41:00.0"

func sharedACPP() *npuv1alpha1.AcceleratorPartitionPolicy {
	return &npuv1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "acpp-shared", UID: "uid-shared"},
		Spec: npuv1alpha1.AcceleratorPartitionPolicySpec{
			Vendor:  vendorNvidia,
			Sharing: &npuv1alpha1.SharingSpec{Mode: npuv1alpha1.SharingModeTimeSliced, TimeSlicing: &npuv1alpha1.TimeSlicingSpec{Replicas: 4}},
		},
		Status: npuv1alpha1.AcceleratorPartitionPolicyStatus{
			ApplyRecords: []npuv1alpha1.ApplyRecord{{
				NodeName: "worker1", MigPhase: npuv1alpha1.MigPhaseReady, OwnerUID: "uid-shared",
				Profile: "1g.6gb", Count: 4, GPUPCIs: []string{sharingPCI}, ExpectedMigCount: 4,
			}},
		},
	}
}

func sharingTarget(ctx context.Context, owner string) partition.Target {
	return partition.Target{Ctx: ctx, NodeName: "worker1", Owner: owner}
}

func TestRunSharingAppliesAndReportsReady(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	// base = MIG 4조각 → 4배수 공유 후 16 으로 수렴한다고 보고.
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0})

	// Discover 가 채운 장치 상태 — MIG 는 하드웨어 격리로 보고된다.
	in := npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady,
		Devices: []npuv1alpha1.DeviceStatus{{PCIAddress: sharingPCI, IsolationCapability: npuv1alpha1.IsolationCapability{
			Compute: npuv1alpha1.IsolationHardware, Memory: npuv1alpha1.IsolationHardware, Fault: npuv1alpha1.IsolationDevice,
		}}}}
	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-shared"), in,
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("phase = %q, want Ready", ts.Phase)
	}
	if ts.Advertisement.AdvertisedResources["nvidia.com/mig-1g.6gb"] != 16 {
		t.Fatalf("advertised resources = %v", ts.Advertisement.AdvertisedResources)
	}
	// 정직성: 배수된 광고와 하드웨어 격리를 함께 보고하면 "격리된 장치 16개" 로 읽힌다.
	// 시분할 replica 사이에는 compute/memory 격리가 없다.
	iso := ts.Devices[0].IsolationCapability
	if iso.Compute != npuv1alpha1.IsolationNone || iso.Memory != npuv1alpha1.IsolationNone {
		t.Fatalf("time-sliced 장치가 격리를 그대로 보고한다: %+v", iso)
	}
	if iso.Fault != npuv1alpha1.IsolationDevice {
		t.Fatalf("fault 축은 시분할이 바꾸는 것이 아니다: %+v", iso)
	}
	if !strings.Contains(ts.Advertisement.Reason, "timeSliced x4") {
		t.Fatalf("광고 배수의 근거가 status 에 없다: %q", ts.Advertisement.Reason)
	}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); err != nil {
		t.Fatalf("configmap missing: %v", err)
	}
	if !strings.Contains(cm.Data[nvidia.SharingConfigKey], "replicas: 4") {
		t.Fatalf("configmap = %q", cm.Data[nvidia.SharingConfigKey])
	}

	// 저널이 mutation 을 기록해야 삭제 시 원복 대상을 알 수 있다.
	var got npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-shared"}, &got); err != nil {
		t.Fatal(err)
	}
	rec := getApplyRecord(&got, "worker1")
	if rec.SharingMode != npuv1alpha1.SharingModeTimeSliced || rec.SharingReplicas != 4 {
		t.Fatalf("sharing journal not persisted: %+v", rec)
	}
}

// mpsACPP 는 4배수 mps 공유를 요청하는 nvidia ACPP 다(파티션은 이미 Ready 저널).
func mpsACPP() *npuv1alpha1.AcceleratorPartitionPolicy {
	return &npuv1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "acpp-mps", UID: "uid-mps"},
		Spec: npuv1alpha1.AcceleratorPartitionPolicySpec{
			Vendor:  vendorNvidia,
			Sharing: &npuv1alpha1.SharingSpec{Mode: npuv1alpha1.SharingModeMPS, MPS: &npuv1alpha1.MPSSpec{Replicas: 4}},
		},
		Status: npuv1alpha1.AcceleratorPartitionPolicyStatus{
			ApplyRecords: []npuv1alpha1.ApplyRecord{{
				NodeName: "worker1", MigPhase: npuv1alpha1.MigPhaseReady, OwnerUID: "uid-mps",
				Profile: "1g.6gb", Count: 4, GPUPCIs: []string{sharingPCI}, ExpectedMigCount: 4,
			}},
		},
	}
}

// F1(review): SharingLayoutFrom 이 spec.Sharing.MPS.Replicas 를 안 읽으면 Replicas 가 0으로 남아
// ValidateSharing 이 매번 거부한다 — daemon 유무와 무관하게 mps 요청이 apply 단계에 절대 도달 못 한다.
// 이 테스트는 실 컨트롤러 경로(runSharing)로 그 경로가 살아있는지 고정한다. F3(sharingReady 의
// Reason 이 mps 에도 "timeSliced" 라고 말하는 결함)도 같이 고정한다.
func TestRunSharingAppliesMPSAndReportsReady(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	// base = MIG 4조각 → 4배수 공유 후 16 으로 수렴한다고 보고.
	// daemon 은 이미 떠 있다 — 안 떠 있으면 종점은 Ready 가 아니라 Applying 이어야 한다(③).
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0}, readyMpsDaemon()...)

	in := npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}
	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-mps"), in,
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("phase = %q, want Ready — mps 요청이 apply 단계에 도달하지 못했다(F1)", ts.Phase)
	}
	if ts.Advertisement.AdvertisedResources["nvidia.com/mig-1g.6gb"] != 16 {
		t.Fatalf("advertised resources = %v", ts.Advertisement.AdvertisedResources)
	}
	if !strings.Contains(ts.Advertisement.Reason, "mps x4") {
		t.Fatalf("mps 인데 reason 이 실제 모드를 말하지 않는다(F3): %q", ts.Advertisement.Reason)
	}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); err != nil {
		t.Fatalf("configmap missing: %v", err)
	}
	if !strings.Contains(cm.Data[nvidia.SharingConfigKey], "mps:") || !strings.Contains(cm.Data[nvidia.SharingConfigKey], "replicas: 4") {
		t.Fatalf("configmap = %q", cm.Data[nvidia.SharingConfigKey])
	}

	var got npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-mps"}, &got); err != nil {
		t.Fatal(err)
	}
	rec := getApplyRecord(&got, "worker1")
	if rec.SharingMode != npuv1alpha1.SharingModeMPS || rec.SharingReplicas != 4 {
		t.Fatalf("sharing journal not persisted: %+v", rec)
	}
}

// D-7(라이브 결함): 준비 게이트가 DaemonSet 전체(NumberReady == DesiredNumberScheduled)를 보면,
// MPS 를 요청하지도 않은 무관한 노드의 daemon pod 이 영구히 못 뜰 때 대상 노드의 daemon 이 Ready
// 여도 정책이 영원히 Applying 에 갇힌다(quiescePolicy.timeoutSeconds 는 drain/quiesce 단계용이라
// Failed 전이도 없다 — 자동 복구 경로 없는 hang).
// 라이브 재현: DS 셀렉터(kcloud.ai/nvidia.present=true)가 k8s-master 도 매칭했는데 그 노드는
// nvidia RuntimeClass 미구성으로 FailedCreatePodSandBox 를 영구 반복했다 —
// numberReady=1 desiredNumberScheduled=2 고정, 대상 노드(worker1)의 daemon 은 Running/Ready.
func TestRunSharingMPSIgnoresUnrelatedNodeDaemonFailure(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	ds := renderMpsControlDaemonDS()
	ds.Status = appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 1}
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0},
		ds, mpsDaemonPod("worker1", true), mpsDaemonPod("k8s-master", false))

	in := npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}
	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-mps"), in,
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})
	if err != nil {
		t.Fatal(err)
	}
	if cond := apimeta.FindStatusCondition(ts.Conditions, npuv1alpha1.ACPPCondApplied); cond != nil &&
		cond.Reason == npuv1alpha1.ReasonSharingDaemonNotReady {
		t.Fatalf("대상 노드의 daemon 은 Ready 인데 무관한 노드 실패로 차단됐다: %s", cond.Message)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("phase = %q, want Ready", ts.Phase)
	}
}

// 위 완화가 게이트 자체를 무력화하면 안 된다 — 대상 노드의 daemon pod 이 Ready 가 아니면
// (pod 은 있는데 Ready=False — 실제 ImagePullBackOff 형상) 여전히 배선·검증 전에 막아야 한다
// (review 최종 ③).
func TestRunSharingMPSStillBlocksWhenTargetNodeDaemonNotReady(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	ds := renderMpsControlDaemonDS()
	ds.Status = appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 1}
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0},
		ds, mpsDaemonPod("k8s-master", true), mpsDaemonPod("worker1", false))

	in := npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}
	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-mps"), in,
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})
	if err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(ts.Conditions, npuv1alpha1.ACPPCondApplied)
	if cond == nil || cond.Reason != npuv1alpha1.ReasonSharingDaemonNotReady {
		t.Fatalf("대상 노드 daemon 미준비인데 게이트가 통과했다: phase=%q cond=%+v", ts.Phase, cond)
	}
}

// review 최종 ②: 적용 경로가 장치별 MPS capability 를 한 번도 읽지 않았다. Discover 가 "이 장치는
// MIG 가 켜져 MPS 불가" 라고 판정해 status 에 적어 둔 뒤에도 ValidateSharing 은 replica 개수만 보고
// 통과시켜, 그 장치의 MIG 조각에 대해 MPS 배수를 요청했다. 실 하드웨어(worker1: MIG Enabled A30)의
// 시나리오다 — 다섯 라운드에 걸쳐 정교해진 판정이 적용 결정에 전혀 입력되지 않았다.
func TestRunSharingRejectsMPSOnDeviceThatCannotRunIt(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0}, readyMpsDaemon()...)

	// Discover 가 실제로 내는 판정(MIG Enabled A30) — deviceStatusFor 의 문장 그대로.
	in := npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady,
		Devices: []npuv1alpha1.DeviceStatus{{
			ID: "PCI-" + sharingPCI, PCIAddress: sharingPCI,
			SharingCapability: npuv1alpha1.SharingCapability{
				MultiProcess: npuv1alpha1.SharingModeSupport{
					Supported: false, Verification: npuv1alpha1.VerificationRequired,
					Reason: "MIG 가 켜진 장치에서는 MPS 를 쓸 수 없다(상호 배타)",
				},
			},
		}}}
	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-mps"), in,
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseFailed {
		t.Fatalf("★ phase = %q — MPS 불가로 판정된 장치에 mps 를 적용했다", ts.Phase)
	}
	cond := apimeta.FindStatusCondition(ts.Conditions, npuv1alpha1.ACPPCondValidated)
	if cond == nil || cond.Reason != npuv1alpha1.ReasonSharingUnsupported {
		t.Fatalf("Validated 거절 조건이 없다: %+v", ts.Conditions)
	}
	// 진짜 이유가 사용자에게 가야 한다 — "shared allocatable did not converge" 로는 알 수 없다.
	if !strings.Contains(cond.Message, "MIG 가 켜진 장치") || !strings.Contains(cond.Message, sharingPCI) {
		t.Errorf("거절 사유가 장치와 이유를 말하지 않는다: %q", cond.Message)
	}
	// 거절은 mutation 이전이어야 한다(criterion 9).
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("거절 경로가 ConfigMap 을 만들었다: err=%v", err)
	}
}

// review 최종 ③: ensureMpsControlDaemon 은 Get/Create/Update 만 했고, 광고 개수는 daemon 생존과
// 무관하므로 verify 로도 daemon 부재를 잡을 수 없다(acpp_sharing.go 주석이 이미 인정한 구멍).
// 그래서 air-gap 에서 이미지를 못 당기면 daemon 이 한 번도 뜨지 못한 채 Ready + Verified + "mps x4"
// 가 찍혔다. 종점을 막는 것이 핵심이다 — 두 케이스 모두 sharingReady 에 도달하면 안 된다.
// 실패가 아니라 대기다(이미지 pull 은 transient): Applying + requeue 로 받는다.
func TestRunSharingNeverReadyWithoutMpsDaemon(t *testing.T) {
	for _, tc := range []struct {
		name    string
		journal bool
	}{
		{"첫 적용", false},
		{"저널 일치 + 이미 수렴(멱등 게이트)", true}, // sharingReady 로 직행하는 경로
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			acpp := mpsACPP()
			if tc.journal {
				acpp.Status.ApplyRecords[0].SharingMode = npuv1alpha1.SharingModeMPS
				acpp.Status.ApplyRecords[0].SharingReplicas = 4
			}
			// daemon DS 를 시드하지 않는다 = 클러스터에 daemon 이 없다.
			r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0})

			ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-mps"),
				npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
				map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})
			if err != nil {
				t.Fatalf("미준비는 transient 대기다 — 에러로 끝내면 안 된다: %v", err)
			}
			if ts.Phase == npuv1alpha1.ACPPPhaseReady {
				t.Fatalf("★ daemon 이 한 번도 뜨지 않았는데 Ready 를 찍었다: %+v", ts.Advertisement)
			}
			// Applying = Reconcile 이 30s 뒤 재확인하는 phase(자가치유). Failed 면 재시도가 없다.
			if ts.Phase != npuv1alpha1.ACPPPhaseApplying {
				t.Fatalf("phase = %q, want Applying(+requeue)", ts.Phase)
			}
			if cond := apimeta.FindStatusCondition(ts.Conditions, npuv1alpha1.ACPPCondVerified); cond != nil && cond.Status == metav1.ConditionTrue {
				t.Errorf("★ daemon 없이 Verified=True 를 찍었다: %+v", cond)
			}
			cond := apimeta.FindStatusCondition(ts.Conditions, npuv1alpha1.ACPPCondApplied)
			if cond == nil || cond.Reason != npuv1alpha1.ReasonSharingDaemonNotReady {
				t.Fatalf("대기 사유가 status 에 없다: %+v", ts.Conditions)
			}
			// 다음 reconcile 이 준비를 확인할 수 있도록 DS 자체는 만들어져 있어야 한다.
			var ds appsv1.DaemonSet
			if err := c.Get(ctx, types.NamespacedName{Name: mpsControlDaemonDSName, Namespace: "kube-system"}, &ds); err != nil {
				t.Fatalf("daemon DS 가 생성되지 않았다: %v", err)
			}
		})
	}
}

// journalAtEnsureRecorder 는 mps-control-daemon DS 를 만드는 순간(ensureMpsControlDaemon 의
// Create 호출)의 ACPP 저널 상태를 그대로 스냅샷한다 — 타이밍 경합을 재현하지 않고도 "daemon 을
// 만들 때 저널이 이미 mps 를 반영하고 있는가"라는 불변식을 직접 확인한다(review 재검토 4차).
type journalAtEnsureRecorder struct {
	client.Client
	acppName, node string
	sawCreate      bool
	modeAtCreate   string
}

func (s *journalAtEnsureRecorder) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if ds, ok := obj.(*appsv1.DaemonSet); ok && ds.Name == mpsControlDaemonDSName {
		s.sawCreate = true
		var acpp npuv1alpha1.AcceleratorPartitionPolicy
		if err := s.Get(ctx, types.NamespacedName{Name: s.acppName}, &acpp); err == nil {
			s.modeAtCreate = getApplyRecord(&acpp, s.node).SharingMode
		}
	}
	return s.Client.Create(ctx, obj, opts...)
}

// review 재검토 4차(Major): daemon 을 저널보다 먼저 만들면, 그 사이 창에서 무관한 정책의
// reconcile 이 참조 카운트를 봐도 이 요청을 못 보고 방금 만든 daemon 을 지울 수 있다.
// 저널을 daemon 보다 먼저 써야 한다.
func TestRunSharingPersistsJournalBeforeEnsuringDaemon(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0})
	spy := &journalAtEnsureRecorder{Client: c, acppName: acpp.Name, node: "worker1"}
	r.Client = spy

	in := npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}
	if _, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-mps"), in,
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0}); err != nil {
		t.Fatal(err)
	}

	if !spy.sawCreate {
		t.Fatal("daemon 생성이 호출되지 않았다 — 테스트 전제가 깨졌다")
	}
	if spy.modeAtCreate != npuv1alpha1.SharingModeMPS {
		t.Errorf("daemon 을 만드는 시점의 저널 SharingMode = %q, want %q — 저널이 daemon 보다 먼저 영속돼야 한다",
			spy.modeAtCreate, npuv1alpha1.SharingModeMPS)
	}
}

// configAtEnsureRecorder 는 mps-control-daemon DS 를 만드는 순간의 sharing ConfigMap 존재
// 여부를 스냅샷한다(journalAtEnsureRecorder 와 같은 수법, 다른 불변식).
type configAtEnsureRecorder struct {
	client.Client
	sawCreate      bool
	configAtCreate bool
}

func (s *configAtEnsureRecorder) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if ds, ok := obj.(*appsv1.DaemonSet); ok && ds.Name == mpsControlDaemonDSName {
		s.sawCreate = true
		var cm corev1.ConfigMap
		s.configAtCreate = s.Get(ctx, types.NamespacedName{
			Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm) == nil
	}
	return s.Client.Create(ctx, obj, opts...)
}

// D-12 순서: daemon 은 sharing ConfigMap 을 마운트하므로 CM 이 **먼저** 있어야 한다. 거꾸로면
// pod 이 ContainerCreating(ConfigMap not found)에 갇혀 Ready 게이트를 영원히 못 넘는다.
// 요구 순서는 저널 → ConfigMap → daemon → DP 배선이다.
func TestRunSharingWritesSharingConfigBeforeEnsuringDaemon(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0})
	spy := &configAtEnsureRecorder{Client: c}
	r.Client = spy

	if _, err := r.runSharing(acpp, nvidia.New(spy), sharingTarget(ctx, "acpp-mps"),
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0}); err != nil {
		t.Fatal(err)
	}
	if !spy.sawCreate {
		t.Fatal("daemon 생성이 호출되지 않았다 — 테스트 전제가 깨졌다")
	}
	if !spy.configAtCreate {
		t.Errorf("★ daemon 을 만드는 시점에 sharing ConfigMap 이 없다 — pod 이 ContainerCreating 에 갇힌다")
	}
}

func TestRunSharingRollsBackWhenAllocatableDoesNotConverge(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	// 수렴 실패 — 공유 전 값 그대로.
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})

	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-shared"),
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseRestored {
		t.Fatalf("phase = %q, want PreviousConfigurationRestored", ts.Phase)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("configmap must be removed on rollback, err=%v", err)
	}
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"}, &ds); err != nil {
		t.Fatal(err)
	}
	if _, owned := ds.Annotations[nvidia.SharingOwnerAnnotation]; owned {
		t.Fatalf("rollback must release sharing owner annotation")
	}
	var got npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-shared"}, &got); err != nil {
		t.Fatal(err)
	}
	if rec := getApplyRecord(&got, "worker1"); rec.SharingMode != "" || rec.SharingReplicas != 0 {
		t.Fatalf("rolled-back sharing must be cleared from journal: %+v", rec)
	}
}

// D-13 인접 누수: verify 미수렴으로 rollback 된 mps 정책은 배선을 다 걷고도 daemon 을 남겼다.
// 그 종점(Restored)은 10분 requeue 이고, 다음 pass 는 D-9 게이트(같은 generation 의 rollback 은
// terminal)에서 daemon 정리 전에 early return 하므로 정책이 살아 있는 한 daemon 이 영영 남는다.
// 두 경로 모두에서 회수돼야 한다.
func TestRunSharingRemovesMpsDaemonAfterRollback(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	acpp.Generation = 1 // D-9 게이트는 generation 을 비교한다(0 이면 게이트가 없는 것과 같다).
	// 수렴 실패 — 공유 전 값 그대로.
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0}, readyMpsDaemon()...)
	base := map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0}
	in := npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}

	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-mps"), in, base)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseRestored {
		t.Fatalf("phase = %q, want PreviousConfigurationRestored — rollback 을 거치지 않으면 이 테스트가 무의미하다", ts.Phase)
	}
	var ds appsv1.DaemonSet
	key := types.NamespacedName{Name: mpsControlDaemonDSName, Namespace: "kube-system"}
	if err := c.Get(ctx, key, &ds); !apierrors.IsNotFound(err) {
		t.Errorf("★ rollback 이 배선을 걷고도 daemon 을 남겼다: err=%v", err)
	}

	// 다음 pass = D-9 게이트(terminal Failed). 그 사이 daemon 이 되살아나 있어도 회수해야 한다.
	seedMpsDaemon(t, ctx, r, acpp.Name, "worker1")
	ts, err = r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-mps"), in, base)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseFailed {
		t.Fatalf("phase = %q, want Failed — D-9 게이트를 타지 않으면 이 경로가 검증되지 않는다", ts.Phase)
	}
	if err := c.Get(ctx, key, &ds); !apierrors.IsNotFound(err) {
		t.Errorf("★ D-9 게이트가 daemon 정리 전에 early return 한다: err=%v", err)
	}
}

// TestRunSharingExclusiveIsPassthrough: 공유 요청이 없으면 파티션 결과를 그대로 두고 아무것도 안 만든다.
func TestRunSharingExclusiveIsPassthrough(t *testing.T) {
	ctx := context.Background()
	acpp := &npuv1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "acpp-plain", UID: "uid-plain"},
		Spec:       npuv1alpha1.AcceleratorPartitionPolicySpec{Vendor: vendorNvidia},
	}
	r, c := sharingFixture(t, acpp, map[string]int32{})
	in := npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}

	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-plain"), in, map[string]int32{nvidiaGPUResource: 1})
	if err != nil || ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("exclusive passthrough broken: phase=%q err=%v", ts.Phase, err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("exclusive must not create sharing configmap, err=%v", err)
	}
	// 적용된 공유가 없으면 해제 경로도 아무것도 하지 않는다 — 매 reconcile 마다 DP 를 죽이면 안 된다.
	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "nvidia-device-plugin-abcde", Namespace: "kube-system"}, &pod); err != nil {
		t.Fatalf("device-plugin pod must survive an exclusive reconcile: %v", err)
	}
}

// TestRunSharingDisablesWhenSpecReturnsToExclusive: spec.sharing 을 지우면(=exclusive) 적용해 둔
// 공유가 실제로 내려가야 한다. 저널만 든 채 지나가면 spec 은 exclusive 인데 클러스터는 배수를 계속
// 광고하고(검증기는 `>=` 비교라 과다광고를 못 잡는다) status 는 Ready 를 보고한다 — 되돌릴 방법이
// ACPP 삭제뿐인 "켤 수는 있는데 끌 수 없는" 정책이 된다. 과다광고 상태에서 pod 들이 한 장치에
// co-schedule 되면 GPU OOM/성능 붕괴가 난다.
func TestRunSharingDisablesWhenSpecReturnsToExclusive(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0})
	tgt := sharingTarget(ctx, "acpp-shared")
	base := map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0}

	// 1) 공유를 실제로 적용한다(저널·ConfigMap·배선 모두 생성).
	if _, err := r.runSharing(acpp, nvidia.New(c), tgt,
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}, base); err != nil {
		t.Fatal(err)
	}
	var applied npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-shared"}, &applied); err != nil {
		t.Fatal(err)
	}
	acpp.Status = applied.Status
	acpp.ResourceVersion = applied.ResourceVersion

	// 2) 사용자가 spec.sharing 을 제거한다.
	acpp.Spec.Sharing = nil
	ts, err := r.runSharing(acpp, nvidia.New(c), tgt,
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}, base)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("phase = %q — 해제는 파티션 결과를 바꾸지 않는다", ts.Phase)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("sharing configmap must be removed when sharing is turned off, err=%v", err)
	}
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"}, &ds); err != nil {
		t.Fatal(err)
	}
	if _, owned := ds.Annotations[nvidia.SharingOwnerAnnotation]; owned {
		t.Fatalf("disable must release the sharing owner annotation")
	}
	for _, a := range ds.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(a, "--config-file=") {
			t.Fatalf("disable must unwire --config-file: %v", ds.Spec.Template.Spec.Containers[0].Args)
		}
	}
	// device-plugin 이 새 설정을 읽도록 재시작돼야 한다 — 아니면 배선만 지우고 광고는 그대로다.
	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "nvidia-device-plugin-abcde", Namespace: "kube-system"}, &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("device-plugin must be restarted on disable, err=%v", err)
	}
	var got npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-shared"}, &got); err != nil {
		t.Fatal(err)
	}
	if rec := getApplyRecord(&got, "worker1"); rec.SharingMode != "" || rec.SharingReplicas != 0 {
		t.Fatalf("저널이 해제를 반영하지 않았다: %+v", rec)
	}
}

// C-1(D-8 리뷰 라운드 2): 가드는 mutation 이전 판정이지만, **이전 빌드가 이미 적용해 둔** mps 배선까지
// 걷어내지 않으면 청소 주체가 없다 — spec 은 여전히 mps 를 요청하므로 disableSharing 도, 삭제 경로도
// 불리지 않는다. 그 결과 전역 DP 는 거절된 요청의 설정 때문에 계속 validateFlags 로 죽고, 라이브에서
// 관측된 간헐 회귀(worker1 allocatable 0↔4 왕복)가 **회복 시도조차 없는 영구 0** 으로 고정된다.
// 도달 경로: pre-fix 빌드가 apply 를 마친 창(ConfigMap + DS 배선 + DP CrashLoopBackOff)에서 이 커밋이
// 배포된다(operator 재시작). 라이브의 그 정책은 sharing-only 였으므로 진입점도 runSharingOnly 다 —
// 거절 판정이 runSharing 이전(precheck)에서 끝나는 경로다.
//
// 검증 상태는 전부 프로덕션 apply 경로가 만든다(저널·ConfigMap·배선을 손으로 조립하지 않는다).
func TestRunSharingCleansUpAppliedMPSWhenGuardRejects(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0}, readyMpsDaemon()...)
	tgt := sharingTarget(ctx, "acpp-mps")
	base := map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0}

	// 1) mps 를 실제로 적용한다 — 이 시점이 라이브의 "적용됨" 창이다.
	if _, err := r.runSharing(acpp, nvidia.New(c), tgt,
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}, base); err != nil {
		t.Fatal(err)
	}
	var applied npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-mps"}, &applied); err != nil {
		t.Fatal(err)
	}
	if getApplyRecord(&applied, "worker1").SharingMode != npuv1alpha1.SharingModeMPS {
		t.Fatalf("전제가 깨졌다 — mps 가 적용되지 않았다: %+v", applied.Status.ApplyRecords)
	}
	acpp.Status = applied.Status
	acpp.ResourceVersion = applied.ResourceVersion

	// 2) 이 노드가 MIG 관리로 전환된다(예: 형제 정책이 GI 를 만들어 syncMigActiveLabel 이 라벨을
	// 건다, Task 2) → 이제 가드(Task 5)가 이 노드의 mig-active 라벨을 보고 거절한다. 적용된 배선은
	// 여전히 flat DS/CM 에 있다 — resolveRollbackTarget 이 owner annotation 으로 찾으므로 라벨이
	// 바뀌어도 청소는 그 실제 배선을 정확히 찾아낸다.
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: "worker1"}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[nvidia.MigActiveNodeLabel] = labelValueTrue
	if err := c.Update(ctx, &node); err != nil {
		t.Fatal(err)
	}

	// 1단계(mps 적용)가 restartNvidiaDevicePlugin 으로 지운 pod 을 kubelet 처럼 같은 이름으로
	// 재생성한다 — 안 그러면 아래 "DP 가 재시작되지 않았다" 어서션이 이미 지워진 pod 을 다시
	// 찾는 것뿐이라 2단계의 청소가 실제로 재시작을 트리거하는지 아무것도 증명하지 못한다(review-d8 I-3).
	recreated := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nvidia-device-plugin-abcde", Namespace: "kube-system",
			Labels: map[string]string{"app.kubernetes.io/name": "nvidia-device-plugin", nvidiaDevicePluginVendorLabel: "nvidia"},
		},
		Spec: corev1.PodSpec{NodeName: "worker1", Containers: []corev1.Container{{Name: "c", Image: "x"}}},
	}
	if err := c.Create(ctx, recreated); err != nil {
		t.Fatal(err)
	}

	ts, err := r.runSharingOnly(acpp, nvidia.New(c), tgt,
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseFailed {
		t.Fatalf("phase = %q — 가드가 거절하지 않았다(전제)", ts.Phase)
	}

	// ★ 거절만 하고 배선을 남기면 DP 는 계속 기동 실패한다 — 광고 0 이 영구 고정된다.
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("★ 거절된 요청의 sharing ConfigMap 이 남았다 — DP 는 계속 죽는다: err=%v data=%q",
			err, cm.Data[nvidia.SharingConfigKey])
	}
	var dp appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"}, &dp); err != nil {
		t.Fatal(err)
	}
	if nvidia.HasMPSVolume(&dp) {
		t.Fatalf("★ DP 에 mps 배선이 남았다: %+v", dp.Spec.Template.Spec.Volumes)
	}
	// 배선을 걷었어도 DP 를 재시작하지 않으면 죽은 pod 이 그대로 남는다(광고 복원 수단이 없다).
	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "nvidia-device-plugin-abcde", Namespace: "kube-system"}, &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("★ DP 가 재시작되지 않았다 — 걷어낸 설정으로 다시 뜨지 못한다: err=%v", err)
	}
	// 저널도 해제돼야 다음 reconcile 이 같은 배선을 되살리지 않는다.
	var got npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-mps"}, &got); err != nil {
		t.Fatal(err)
	}
	if rec := getApplyRecord(&got, "worker1"); rec.SharingMode != "" || rec.SharingReplicas != 0 {
		t.Fatalf("★ 저널이 mps 를 계속 가리킨다 — 청소가 멱등하지 않다: %+v", rec)
	}
	var mpsDS appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: mpsControlDaemonDSName, Namespace: "kube-system"}, &mpsDS); !apierrors.IsNotFound(err) {
		t.Fatalf("mps control daemon 이 남았다: err=%v", err)
	}
}

// review 최종 ⑤(테스트 갭): Task 6 F2 MAJOR 의 mps 판이다. disableSharing 의 게이트에서 mps 를
// 빼도 스위트가 초록이었다 — 같은 형태의 형제 게이트(handleNvidiaDeletion)는 고정돼 있는데
// 이쪽만 회귀 방어가 없었다. 되돌렸을 때의 결과는 timeSliced 판보다 나쁘다: daemon 정리는 게이트
// 밖에서 먼저 하므로 daemon 은 지워지고, DP 는 MPS_ROOT + 파이프 볼륨을 유지한 채 ConfigMap 이
// 계속 ×N 을 광고한다 — 존재하지 않는 daemon 을 가리키는 DP.
func TestRunSharingDisablesMPSWhenSpecReturnsToExclusive(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0}, readyMpsDaemon()...)
	tgt := sharingTarget(ctx, "acpp-mps")
	base := map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0}

	// 1) mps 공유를 실제로 적용한다(저널·ConfigMap·배선 모두 생성).
	if _, err := r.runSharing(acpp, nvidia.New(c), tgt,
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}, base); err != nil {
		t.Fatal(err)
	}
	var applied npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-mps"}, &applied); err != nil {
		t.Fatal(err)
	}
	if getApplyRecord(&applied, "worker1").SharingMode != npuv1alpha1.SharingModeMPS {
		t.Fatalf("전제가 깨졌다 — mps 가 적용되지 않았다: %+v", applied.Status.ApplyRecords)
	}
	acpp.Status = applied.Status
	acpp.ResourceVersion = applied.ResourceVersion

	// 2) 사용자가 spec.sharing 을 제거한다.
	acpp.Spec.Sharing = nil
	ts, err := r.runSharing(acpp, nvidia.New(c), tgt,
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady}, base)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("phase = %q — 해제는 파티션 결과를 바꾸지 않는다", ts.Phase)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("★ mps ConfigMap 이 남았다 — 클러스터가 계속 배수를 광고한다: err=%v", err)
	}
	var dp appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"}, &dp); err != nil {
		t.Fatal(err)
	}
	if _, owned := dp.Annotations[nvidia.SharingOwnerAnnotation]; owned {
		t.Fatalf("해제는 sharing owner 어노테이션을 놓아야 한다")
	}
	// mps 배선(MPS_ROOT env + 파이프 볼륨)이 남으면 DP 가 없는 daemon 을 계속 가리킨다.
	for _, e := range dp.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "MPS_ROOT" {
			t.Fatalf("★ DP 에 mps 배선이 남았다: %+v", dp.Spec.Template.Spec.Containers[0].Env)
		}
	}
	var got npuv1alpha1.AcceleratorPartitionPolicy
	if err := c.Get(ctx, types.NamespacedName{Name: "acpp-mps"}, &got); err != nil {
		t.Fatal(err)
	}
	if rec := getApplyRecord(&got, "worker1"); rec.SharingMode != "" || rec.SharingReplicas != 0 {
		t.Fatalf("★ 저널이 해제를 반영하지 않았다 — 되돌릴 방법이 ACPP 삭제뿐이 된다: %+v", rec)
	}
	// control daemon 도 남지 않아야 한다(이 정책만 mps 를 쓰고 있었다).
	var mpsDS appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: mpsControlDaemonDSName, Namespace: "kube-system"}, &mpsDS); !apierrors.IsNotFound(err) {
		t.Fatalf("mps control daemon 이 남았다: err=%v", err)
	}
}

// TestRunSharingSkipsRestartWhenAlreadyApplied: 저널·수렴이 이미 요청과 같으면 DP 를 다시 죽이지 않는다
// (매 reconcile 마다 device-plugin 을 재시작하면 노드가 계속 흔들린다).
func TestRunSharingSkipsRestartWhenAlreadyApplied(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	acpp.Status.ApplyRecords[0].SharingMode = npuv1alpha1.SharingModeTimeSliced
	acpp.Status.ApplyRecords[0].SharingReplicas = 4
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16, nvidiaGPUResource: 0})

	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-shared"),
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4, nvidiaGPUResource: 0})
	if err != nil || ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("phase=%q err=%v", ts.Phase, err)
	}
	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "nvidia-device-plugin-abcde", Namespace: "kube-system"}, &pod); err != nil {
		t.Fatalf("device-plugin pod must survive an already-applied reconcile: %v", err)
	}
}

// deadlineProbe 는 검증기가 받은 컨텍스트 마감 시각을 기록한다.
type deadlineProbe struct {
	allocVerifier
	deadline time.Time
	hasDL    bool
}

func (p *deadlineProbe) VerifyAllocatable(t partition.Target, expected map[string]int32) (*partition.VerifyResult, error) {
	p.deadline, p.hasDL = t.Ctx.Deadline()
	return p.allocVerifier.VerifyAllocatable(t, expected)
}

// TestRunSharingIdempotencyProbeIsBounded: 저널 일치 분기의 사전 검증은 "지금 이미 수렴해 있는가" 를
// 묻는 것이므로 재적용 검증(verifyTimeout=180s)을 그대로 쓰면 안 된다. 컨트롤러 동시성이 1이라
// 한 번의 reconcile 이 최대 3회 × 180s 를 잡으면 다른 ACPP 는 물론 삭제 처리까지 큐에서 멈춘다.
func TestRunSharingIdempotencyProbeIsBounded(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	acpp.Status.ApplyRecords[0].SharingMode = npuv1alpha1.SharingModeTimeSliced
	acpp.Status.ApplyRecords[0].SharingReplicas = 4
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16})
	probe := &deadlineProbe{allocVerifier: allocVerifier{alloc: map[string]int32{"nvidia.com/mig-1g.6gb": 16}}}
	r.Verifier = probe

	if _, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-shared"),
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4}); err != nil {
		t.Fatal(err)
	}
	if !probe.hasDL {
		t.Fatalf("멱등 게이트가 상한 없는 컨텍스트로 검증한다")
	}
	if budget := time.Until(probe.deadline); budget > sharingProbeTimeout {
		t.Fatalf("멱등 게이트 예산 = %v, want <= %v", budget, sharingProbeTimeout)
	}
}

// TestRunSharingUnsupportedVendorIsHonest: 공유를 구현하지 않은 backend 에 요청하면 조용히 무시하지 않고
// Unsupported 로 보고한다.
func TestRunSharingUnsupportedVendorIsHonest(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	r, c := sharingFixture(t, acpp, map[string]int32{})

	ts, err := r.runSharing(acpp, noSharingBackend{}, sharingTarget(ctx, "acpp-shared"),
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
		map[string]int32{nvidiaGPUResource: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseUnsupported {
		t.Fatalf("phase = %q, want Unsupported", ts.Phase)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("unsupported vendor must not write sharing config, err=%v", err)
	}
}

// noSharingBackend 는 SharingBackend 를 구현하지 않는 backend 다(RNGD 등).
type noSharingBackend struct{ partition.Backend }

func (noSharingBackend) Vendor() string { return "furiosa" }

// emptyObserver 는 GI 가 남지 않은(원복 완료) 노드를 흉내낸다.
type emptyObserver struct{}

func (o emptyObserver) Observe(_ context.Context, _ string, pcis []string) ([]nvidia.Observation, error) {
	obs := make([]nvidia.Observation, 0, len(pcis))
	for _, p := range pcis {
		obs = append(obs, nvidia.Observation{PCI: p, ModeCurrent: "Enabled"})
	}
	return obs, nil
}

// noopExec 는 MIG 명령 실행 seam 을 대체한다(실 Job 생성 방지).
type noopExec struct{}

func (noopExec) Run(context.Context, string, string, string, []nvidia.CommandStep) error { return nil }

// TestHandleNvidiaDeletionRollsBackSharing: ACPP 삭제 시 device-plugin 공유 설정을 원복해야 한다 —
// 배수가 남아 있으면 baseline allocatable 복원 확인이 성립하지 않고, 광고만 부풀린 채 정책이 사라진다.
func TestHandleNvidiaDeletionRollsBackSharing(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	acpp.Status.ApplyRecords[0].SharingMode = npuv1alpha1.SharingModeTimeSliced
	acpp.Status.ApplyRecords[0].SharingReplicas = 4
	r, c := sharingFixture(t, acpp, map[string]int32{})
	r.NvidiaObserverFactory = func(client.Client) nvidia.Observer { return emptyObserver{} }
	r.NvidiaExecFactory = func(client.Client) nvidia.Executor { return noopExec{} }

	// 공유가 실제로 적용돼 있는 상태에서 삭제한다.
	tgt := sharingTarget(ctx, "acpp-shared")
	if _, err := nvidia.New(c).ApplySharing(tgt, partition.SharingLayout{Mode: npuv1alpha1.SharingModeTimeSliced, Replicas: 4},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4}); err != nil {
		t.Fatal(err)
	}

	if err := r.handleNvidiaDeletion(ctx, acpp); err != nil {
		t.Fatalf("deletion cleanup failed: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("sharing configmap must be removed on deletion, err=%v", err)
	}
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"}, &ds); err != nil {
		t.Fatal(err)
	}
	if _, owned := ds.Annotations[nvidia.SharingOwnerAnnotation]; owned {
		t.Fatalf("deletion must release sharing owner annotation")
	}
}

// 추가 발견(F2 와 같은 결함 클래스, review 범위 밖): handleNvidiaDeletion 은 "공유가 적용돼
// 있었는가" 를 SharingMode == SharingModeTimeSliced 하드코딩 비교로만 묻는다. mps 로 적용된
// 채 ACPP 가 삭제되면 이 게이트가 거짓이라 RollbackSharing 을 아예 안 부르고, ConfigMap/DS 의
// mps 배선과 owner 어노테이션이 영구히 남는다.
func TestHandleNvidiaDeletionRollsBackMPSSharing(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	acpp.Status.ApplyRecords[0].SharingMode = npuv1alpha1.SharingModeMPS
	acpp.Status.ApplyRecords[0].SharingReplicas = 4
	r, c := sharingFixture(t, acpp, map[string]int32{})
	r.NvidiaObserverFactory = func(client.Client) nvidia.Observer { return emptyObserver{} }
	r.NvidiaExecFactory = func(client.Client) nvidia.Executor { return noopExec{} }

	tgt := sharingTarget(ctx, "acpp-mps")
	if _, err := nvidia.New(c).ApplySharing(tgt, partition.SharingLayout{Mode: npuv1alpha1.SharingModeMPS, Replicas: 4},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4}); err != nil {
		t.Fatal(err)
	}

	if err := r.handleNvidiaDeletion(ctx, acpp); err != nil {
		t.Fatalf("deletion cleanup failed: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("mps sharing configmap must be removed on deletion, err=%v", err)
	}
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"}, &ds); err != nil {
		t.Fatal(err)
	}
	if _, owned := ds.Annotations[nvidia.SharingOwnerAnnotation]; owned {
		t.Fatalf("deletion must release sharing owner annotation for mps too")
	}
}

// review F3(2nd half): control daemon 이 실제로 뜬 채(ensureMpsControlDaemon 이 apply 때 만든 것을
// 흉내) ACPP 가 삭제되면, 소유 ACPP 가 사라진 뒤에도 kube-system 에 daemon 이 영원히 남아선 안 된다.
func TestHandleNvidiaDeletionRemovesMpsControlDaemon(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	acpp.Status.ApplyRecords[0].SharingMode = npuv1alpha1.SharingModeMPS
	acpp.Status.ApplyRecords[0].SharingReplicas = 4
	acpp.Finalizers = []string{acppFinalizer} // Delete 가 즉시 지우지 않고 DeletionTimestamp 만 찍게.
	r, c := sharingFixture(t, acpp, map[string]int32{})
	r.NvidiaObserverFactory = func(client.Client) nvidia.Observer { return emptyObserver{} }
	r.NvidiaExecFactory = func(client.Client) nvidia.Executor { return noopExec{} }

	tgt := sharingTarget(ctx, "acpp-mps")
	if _, err := nvidia.New(c).ApplySharing(tgt, partition.SharingLayout{Mode: npuv1alpha1.SharingModeMPS, Replicas: 4},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4}); err != nil {
		t.Fatal(err)
	}
	// apply 경로가 이때 이미 daemon 을 띄워 둔 상태를 흉내낸다(runSharing 이 ApplySharing 이전에 호출).
	seedMpsDaemon(t, ctx, r, acpp.Name, "worker1")
	// handleNvidiaDeletion 은 실제로는 DeletionTimestamp 가 찍힌 뒤에만 불린다(Reconcile 의 게이트) —
	// 참조 카운트(anyPolicyStillUsesMPS)가 삭제 중인 자기 자신의 stale mps 기록을 세지 않으려면
	// 이 조건을 픽스처에도 그대로 반영해야 한다.
	if err := c.Delete(ctx, acpp); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: acpp.Name}, acpp); err != nil {
		t.Fatal(err)
	}

	if err := r.handleNvidiaDeletion(ctx, acpp); err != nil {
		t.Fatalf("deletion cleanup failed: %v", err)
	}
	var ds appsv1.DaemonSet
	err := c.Get(ctx, types.NamespacedName{Name: mpsControlDaemonDSName, Namespace: "kube-system"}, &ds)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("mps control daemon must be removed on deletion, err=%v", err)
	}
}

// D-13(라이브 실측, v0.5.76): sharing-only 정책(layout 없음 → MigPhase 없음, 노드 owner-lock 도
// 잡지 않음)을 삭제하면 handleNvidiaDeletion 의 daemon 정리가 **소유권 분기 뒤에** 있어 항상
// continue 로 건너뛴다. ApplyRecords 가 아예 비면 루프 자체가 안 돈다. 두 경우 모두 daemon 이
// kube-system 에 영원히 남는다(라이브 6분+ 관측, 수동 삭제로 회수).
func TestHandleNvidiaDeletionRemovesMpsControlDaemonForSharingOnlyPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []npuv1alpha1.ApplyRecord
	}{
		{"저널 있음 + 노드 lock 미보유", []npuv1alpha1.ApplyRecord{{
			NodeName: "worker1", SharingMode: npuv1alpha1.SharingModeMPS, SharingReplicas: 2, BaselineGPUCount: 2,
		}}},
		{"저널 없음(rollback 으로 이미 해제)", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			acpp := &npuv1alpha1.AcceleratorPartitionPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "acpp-mps-only", UID: "uid-mps-only",
					Finalizers: []string{acppFinalizer},
				},
				Spec: npuv1alpha1.AcceleratorPartitionPolicySpec{
					Vendor:  vendorNvidia,
					Sharing: &npuv1alpha1.SharingSpec{Mode: npuv1alpha1.SharingModeMPS, MPS: &npuv1alpha1.MPSSpec{Replicas: 2}},
				},
				Status: npuv1alpha1.AcceleratorPartitionPolicyStatus{ApplyRecords: tc.records},
			}
			r, c := sharingFixture(t, acpp, map[string]int32{})
			r.NvidiaObserverFactory = func(client.Client) nvidia.Observer { return emptyObserver{} }
			r.NvidiaExecFactory = func(client.Client) nvidia.Executor { return noopExec{} }
			// sharing-only 정책은 MIG 노드 lock 을 잡지 않는다 — 픽스처가 편의로 걸어 둔 lock 을 걷어
			// 실제 조건(lockUID != acpp.UID)을 만든다. 이게 이 결함의 전제다.
			var node corev1.Node
			if err := c.Get(ctx, types.NamespacedName{Name: "worker1"}, &node); err != nil {
				t.Fatal(err)
			}
			delete(node.Annotations, migOwnerAnnotation)
			if err := c.Update(ctx, &node); err != nil {
				t.Fatal(err)
			}

			seedMpsDaemon(t, ctx, r, acpp.Name, "worker1")
			if err := c.Delete(ctx, acpp); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, types.NamespacedName{Name: acpp.Name}, acpp); err != nil {
				t.Fatal(err)
			}

			if err := r.handleNvidiaDeletion(ctx, acpp); err != nil {
				t.Fatalf("deletion cleanup failed: %v", err)
			}
			var ds appsv1.DaemonSet
			err := c.Get(ctx, types.NamespacedName{Name: mpsControlDaemonDSName, Namespace: "kube-system"}, &ds)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("★ sharing-only 정책 삭제 후 mps control daemon 이 남았다(D-13): err=%v", err)
			}
		})
	}
}

// 소유권 게이트 뒤에 갇혀 있던 두 번째 대상: sharing 원복 블록. daemon 회수만 게이트 밖으로
// 나오고 ConfigMap/배선 원복은 그대로 남아, layout 없는 공유 정책을 지우면 **배수 광고만 남고
// 그것을 받칠 MPS 서버는 사라진** 상태가 된다. 소유 표시(SharingOwnerAnnotation)까지 남아
// 다른 이름의 새 정책은 소유 검사에서 영구 apply 실패하므로 자가치유도 없다.
//
// 픽스처는 실제 apply 성공 종점을 프로덕션 경로(runSharing)로 만든 뒤 삭제한다 — 손으로 조립한
// 저널이 아니라, 이번 수정으로 처음 도달 가능해진 그 상태다.
func TestHandleNvidiaDeletionRollsBackSharingWithoutNodeOwnerLock(t *testing.T) {
	ctx := context.Background()
	acpp := &npuv1alpha1.AcceleratorPartitionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "acpp-mps-only", UID: "uid-mps-only", Finalizers: []string{acppFinalizer},
		},
		Spec: npuv1alpha1.AcceleratorPartitionPolicySpec{
			Vendor:  vendorNvidia,
			Sharing: &npuv1alpha1.SharingSpec{Mode: npuv1alpha1.SharingModeMPS, MPS: &npuv1alpha1.MPSSpec{Replicas: 2}},
		},
	}
	// GPU 2개 → 2배수 공유 후 4로 수렴한다고 보고.
	r, c := sharingFixture(t, acpp, map[string]int32{nvidiaGPUResource: 4}, readyMpsDaemon()...)
	r.NvidiaObserverFactory = func(client.Client) nvidia.Observer { return emptyObserver{} }
	r.NvidiaExecFactory = func(client.Client) nvidia.Executor { return noopExec{} }
	// layout 없는 공유 정책은 MIG 노드 lock 을 잡지 않는다 — 픽스처가 편의로 걸어 둔 lock 을 걷어
	// 실제 조건(lockUID != acpp.UID)을 만든다. 이게 이 결함의 전제다.
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: "worker1"}, &node); err != nil {
		t.Fatal(err)
	}
	delete(node.Annotations, migOwnerAnnotation)
	if err := c.Update(ctx, &node); err != nil {
		t.Fatal(err)
	}

	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, acpp.Name),
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
		map[string]int32{nvidiaGPUResource: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("phase = %q, want Ready — 적용 성공 종점에서 지우는 것이 이 테스트의 전제다", ts.Phase)
	}

	if err := c.Delete(ctx, acpp); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: acpp.Name}, acpp); err != nil {
		t.Fatal(err)
	}
	if err := r.handleNvidiaDeletion(ctx, acpp); err != nil {
		t.Fatalf("deletion cleanup failed: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Errorf("★ 삭제 후 sharing ConfigMap 이 남았다 — 배수 광고를 받칠 MPS 서버는 이미 없다: %q",
			cm.Data[nvidia.SharingConfigKey])
	}
	var dp appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"}, &dp); err != nil {
		t.Fatal(err)
	}
	if owner, owned := dp.Annotations[nvidia.SharingOwnerAnnotation]; owned {
		t.Errorf("★ device-plugin 에 소유 표시가 남았다(%q) — 다른 이름의 새 정책이 영구 apply 실패한다", owner)
	}
	if args := dp.Spec.Template.Spec.Containers[0].Args; len(args) != 0 {
		t.Errorf("★ device-plugin 배선이 남았다: args=%v", args)
	}
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Name: mpsControlDaemonDSName, Namespace: "kube-system"}, &ds); !apierrors.IsNotFound(err) {
		t.Errorf("mps control daemon 이 남았다: err=%v", err)
	}
}

// sharingTeardownOrder 는 공유 해제 시 "배선 해제 → daemon 회수" 순서를 관측한다.
type sharingTeardownOrder struct {
	client.Client
	ops []string
}

func (s *sharingTeardownOrder) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if ds, ok := obj.(*appsv1.DaemonSet); ok && ds.Name == mpsControlDaemonDSName {
		s.ops = append(s.ops, "daemon")
	}
	return s.Client.Delete(ctx, obj, opts...)
}

func (s *sharingTeardownOrder) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if ds, ok := obj.(*appsv1.DaemonSet); ok && ds.Name == nvidia.DevicePluginNameFlat {
		s.ops = append(s.ops, "unwire")
	}
	return s.Client.Patch(ctx, obj, patch, opts...)
}

// 해제 순서는 배선을 먼저 걷고 daemon 을 회수하는 것이다 — 거꾸로 하면 원복이 실패해 early return
// 했을 때 device-plugin 이 죽은 daemon 의 파이프를 문 채 다음 requeue 까지 남는다.
// disableSharing 은 exclusive 전환·거절(rejectSharing) 양쪽이 재사용하는 라이브 경로다.
func TestDisableSharingUnwiresBeforeReclaimingDaemon(t *testing.T) {
	ctx := context.Background()
	acpp := mpsACPP()
	acpp.Status.ApplyRecords[0].SharingMode = npuv1alpha1.SharingModeMPS
	acpp.Status.ApplyRecords[0].SharingReplicas = 4
	r, c := sharingFixture(t, acpp, map[string]int32{}, readyMpsDaemon()...)
	spy := &sharingTeardownOrder{Client: c}
	r.Client = spy

	tgt := sharingTarget(ctx, acpp.Name)
	if _, err := nvidia.New(spy).ApplySharing(tgt, partition.SharingLayout{Mode: npuv1alpha1.SharingModeMPS, Replicas: 4},
		map[string]int32{nvidiaGPUResource: 1}); err != nil {
		t.Fatal(err)
	}
	spy.ops = nil // 적용 단계의 배선 Patch 는 관심 밖이다.

	if _, err := r.disableSharing(acpp, nvidia.New(spy), tgt,
		npuv1alpha1.TargetStatus{NodeName: "worker1"}); err != nil {
		t.Fatal(err)
	}

	unwire, daemon := slices.Index(spy.ops, "unwire"), slices.Index(spy.ops, "daemon")
	if unwire < 0 || daemon < 0 {
		t.Fatalf("해제 두 단계가 모두 일어나야 한다: ops=%v", spy.ops)
	}
	if daemon < unwire {
		t.Errorf("★ daemon 을 배선보다 먼저 회수한다 — 원복 실패 시 DP 가 죽은 daemon 을 문다: ops=%v", spy.ops)
	}
}

// journalProbe 는 verify 시점(= mutation 창 한복판, 크래시가 나면 남는 상태)의 저널을 스냅샷한다.
type journalProbe struct {
	allocVerifier
	c   client.Client
	rec npuv1alpha1.ApplyRecord
}

func (p *journalProbe) VerifyAllocatable(t partition.Target, expected map[string]int32) (*partition.VerifyResult, error) {
	var acpp npuv1alpha1.AcceleratorPartitionPolicy
	if err := p.c.Get(t.Ctx, types.NamespacedName{Name: "acpp-shared"}, &acpp); err == nil {
		p.rec = getApplyRecord(&acpp, t.NodeName)
	}
	return p.allocVerifier.VerifyAllocatable(t, expected)
}

// TestSharingCrashWindowKeepsMigOwnership: 공유 적용 창(apply→DP 재시작→verify, 수 분) 한복판에서
// operator 가 죽었다고 가정하고, 그때 영속돼 있던 저널로 MIG 상태머신을 다시 태운다. 공유가 MIG 저널의
// MigPhase 를 덮어쓰면 managed 도 inProgress 도 아니게 되어 Failed/ExistingMigConfiguration 으로
// 영구 브릭된다(C-1 회귀). 저널에 SharingMode 는 남아야 하고(persist-before-mutate) MigPhase 는 Ready 여야 한다.
func TestSharingCrashWindowKeepsMigOwnership(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	acpp.Spec.Layout = []npuv1alpha1.PartitionLayout{{Profile: "1g.6gb", CountPerDevice: 4}}
	r, c := sharingFixture(t, acpp, map[string]int32{"nvidia.com/mig-1g.6gb": 16})
	probe := &journalProbe{allocVerifier: allocVerifier{alloc: map[string]int32{"nvidia.com/mig-1g.6gb": 16}}, c: c}
	r.Verifier = probe

	tgt := sharingTarget(ctx, "acpp-shared")
	if _, err := r.runSharing(acpp, nvidia.New(c), tgt,
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4}); err != nil {
		t.Fatal(err)
	}
	if probe.rec.SharingMode != npuv1alpha1.SharingModeTimeSliced || probe.rec.SharingReplicas != 4 {
		t.Fatalf("mutation 이전에 공유 저널이 영속되지 않았다: %+v", probe.rec)
	}
	if probe.rec.MigPhase != npuv1alpha1.MigPhaseReady {
		t.Fatalf("MigPhase = %q — 공유가 MIG 저널을 덮었다", probe.rec.MigPhase)
	}

	// 크래시 재개: 그 시점 저널만 들고 MIG 상태머신을 다시 태운다.
	crashed := sharedACPP()
	crashed.Spec.Layout = acpp.Spec.Layout
	crashed.Status.ApplyRecords = []npuv1alpha1.ApplyRecord{probe.rec}
	b := nvidia.New(c)
	if _, err := b.Discover(tgt); err != nil {
		t.Fatal(err)
	}
	ts, err := r.runNvidiaTarget(crashed, b, tgt, npuv1alpha1.TargetStatus{NodeName: "worker1", DriverVersion: "580.65.06"})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Phase != npuv1alpha1.ACPPPhaseReady {
		t.Fatalf("phase = %q, want Ready — 공유 창 크래시가 MIG 정책을 브릭했다", ts.Phase)
	}
	for _, cond := range ts.Conditions {
		if cond.Reason == npuv1alpha1.ReasonExistingMigConfiguration {
			t.Fatalf("MIG 소유 상실: %+v", cond)
		}
	}
}

// TestRunSharingRejectsMissingTimeSlicingBlock: mode=timeSliced 인데 timeSlicing 블록이 없으면
// (CRD 상 허용) replicas=0 이므로 아무것도 건드리기 전에 거절해야 한다(I-2).
func TestRunSharingRejectsMissingTimeSlicingBlock(t *testing.T) {
	ctx := context.Background()
	acpp := sharedACPP()
	acpp.Spec.Sharing.TimeSlicing = nil
	r, c := sharingFixture(t, acpp, map[string]int32{})

	ts, err := r.runSharing(acpp, nvidia.New(c), sharingTarget(ctx, "acpp-shared"),
		npuv1alpha1.TargetStatus{NodeName: "worker1", Phase: npuv1alpha1.ACPPPhaseReady},
		map[string]int32{"nvidia.com/mig-1g.6gb": 4})
	if err != nil || ts.Phase != npuv1alpha1.ACPPPhaseFailed {
		t.Fatalf("phase=%q err=%v, want Failed", ts.Phase, err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}, &cm); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid request must not write config, err=%v", err)
	}
}

// D-9: sharing 경로에는 MIG GI apply(C-1)와 달리 rollback 후 같은 generation 재시도를 막는 게이트가
// 없었다 — verify 미수렴은 에러가 아니라 결과로 돌아오므로(failVerifier 와 같은 모양) rollback 종점의
// runErr 이 nil 이라 rate-limited 재시도도 안 걸리고, 다음 reconcile 이 다시 apply → device-plugin
// 재시작 → verify → rollback 을 무한 반복한다. 같은 generation 의 rollback 저널은 terminal 이어야 한다.
var _ = Describe("ACPP sharing (D-9: rollback does not retry forever within the same generation)", func() {
	It("stops after one rollback and requires a spec change to retry", func() {
		wipeACPPs()
		DeferCleanup(wipeACPPs)
		sel := map[string]string{"kcloud.ai/d9-sharing": "true"}
		seedNvidiaNode("d9-sharing-node", sel, false, npuv1alpha1.DeviceEntry{
			Vendor: "nvidia", Model: "NVIDIA A2", Count: 1,
			DriverLoaded: true, DriverVersion: "535.104.05", PCIeAddress: "0000:86:00.0",
			MigModeCurrent: "N/A", MigModePending: "N/A",
		})
		DeferCleanup(func() { cleanupNvidia("d9-sharing-node") })
		dp := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: nvidia.DevicePluginNameFlat, Namespace: "kube-system"},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "nvidia-device-plugin"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "nvidia-device-plugin"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nvidia-device-plugin", Image: "x"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dp)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dp) })
		DeferCleanup(func() {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: nvidia.SharingConfigMapNameFlat, Namespace: "kube-system"}}
			_ = k8sClient.Delete(ctx, cm)
		})

		acpp := &npuv1alpha1.AcceleratorPartitionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "d9-sharing-acpp"},
			Spec: npuv1alpha1.AcceleratorPartitionPolicySpec{
				NodeSelector:   sel,
				Vendor:         "nvidia",
				DeletionPolicy: "Retain",
				Sharing: &npuv1alpha1.SharingSpec{
					Mode:        npuv1alpha1.SharingModeTimeSliced,
					TimeSlicing: &npuv1alpha1.TimeSlicingSpec{Replicas: 4},
				},
			},
		}
		Expect(k8sClient.Create(ctx, acpp)).To(Succeed())

		r := nvidiaReconciler()
		r.Verifier = failVerifier{} // VerifyAllocatable → (미수렴, nil) = 라이브 180s 초과와 같은 반환

		for i := 0; i < 3; i++ {
			_, err := r.Reconcile(ctx, reconcileReq("d9-sharing-acpp"))
			Expect(err).NotTo(HaveOccurred())
		}

		var got npuv1alpha1.AcceleratorPartitionPolicy
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "d9-sharing-acpp"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(npuv1alpha1.ACPPPhaseFailed),
			"3 회 재진입 후에도 계속 apply→rollback 을 반복하면 안 된다 — 1회 실패 후 terminal Failed 여야 한다")
		Expect(condReasonOf(&got, npuv1alpha1.ACPPCondApplied)).To(Equal("SharingRolledBack"))
		Expect(got.Status.ApplyRecords[0].SharingRolledBackGeneration).To(Equal(got.Generation))
	})
})
