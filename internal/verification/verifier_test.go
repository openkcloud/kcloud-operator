// ============================================================
// verifier_test.go: Verifier 배선 테스트
// 상세: 노드·NDR·정책을 읽어 판정하고 evidence 를 남기는 이음매를 확인한다. 지문 계산·판정·
//
//	TTL 이 각각 맞아도 이음매가 틀리면 evidence 가 거짓을 말하므로 여기서 함께 본다.
//
// 생성일: 2026-07-31
// ============================================================
package verification

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition/nvidia"
)

func resourceQuantity(v int64) *resource.Quantity {
	return resource.NewQuantity(v, resource.DecimalSI)
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeProber struct {
	allocated bool
	called    int
}

func (p *fakeProber) Probe(context.Context, string, string) (bool, string, error) {
	p.called++
	return p.allocated, "", nil
}

func seedNodeAndReport(t *testing.T, geom string, allocatable map[string]int64) (*corev1.Node, *v1alpha1.NodeDeviceReport) {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1", Labels: map[string]string{"role": "gpu"}},
		Status: corev1.NodeStatus{
			NodeInfo:    corev1.NodeSystemInfo{BootID: "boot-1", KernelVersion: "5.15.0-181-generic"},
			Allocatable: corev1.ResourceList{},
		},
	}
	for k, v := range allocatable {
		node.Status.Allocatable[corev1.ResourceName(k)] = *resourceQuantity(v)
	}
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Spec:       v1alpha1.NodeDeviceReportSpec{NodeName: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{
			Devices: []v1alpha1.DeviceEntry{{
				Vendor: "nvidia", PCIeAddress: testPCI, DriverVersion: "535.104.05",
				MigCurrentGeometry: geom,
			}},
		},
	}
	return node, ndr
}

func verifyRequest(geom string) Request {
	return Request{
		NodeName: "worker1", Vendor: "nvidia", SourcePolicy: "acpp-1", Generation: 3,
		Expectation: Expectation{
			Profile: "1g.6gb", CountPerDevice: 4, Geometry: geom,
			Allocatable:   map[string]int32{"nvidia.com/mig-1g.6gb": 4},
			ProbeResource: "nvidia.com/mig-1g.6gb",
		},
		ObservedGeometry:  map[string]string{testPCI: geom},
		ObservationErrors: map[string]string{},
	}
}

func TestVerifyWritesEvidenceWithFingerprintAndExpiry(t *testing.T) {
	geom := nvidia.GeometrySummary("1g.6gb", 4)
	node, ndr := seedNodeAndReport(t, geom, map[string]int64{"nvidia.com/mig-1g.6gb": 4})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, ndr).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()

	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	v := &Verifier{Client: c, Now: func() time.Time { return now }}
	ev, err := v.Verify(context.Background(), verifyRequest(geom))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status.Level != v1alpha1.EvidenceLevelFunctionallyVerified {
		t.Fatalf("level = %q, reason=%q checks=%+v", ev.Status.Level, ev.Status.Reason, ev.Status.Checks)
	}
	if ev.Status.Fingerprint.BootID != "boot-1" || ev.Status.Fingerprint.DriverVersion != "535.104.05" {
		t.Fatalf("지문이 노드·보고에서 오지 않았다: %+v", ev.Status.Fingerprint)
	}
	if ev.Status.Fingerprint.Generation != 3 {
		t.Fatalf("generation = %d", ev.Status.Fingerprint.Generation)
	}
	if ev.Status.ExpiresAt == nil || !ev.Status.ExpiresAt.Time.Equal(now.Add(DefaultPolicy().TTL)) {
		t.Fatalf("만료 시각이 TTL 과 다르다: %+v", ev.Status.ExpiresAt)
	}
	if ev.Status.AdvertisedResources["nvidia.com/mig-1g.6gb"] != 4 {
		t.Fatalf("검증 시점 광고량이 보존되지 않았다: %+v", ev.Status.AdvertisedResources)
	}
	if ev.Spec.NodeName != "worker1" || ev.Spec.Vendor != "nvidia" {
		t.Fatalf("spec = %+v", ev.Spec)
	}
}

func TestVerifyRecordsFailureWithoutLevelAndWithoutError(t *testing.T) {
	geom := nvidia.GeometrySummary("1g.6gb", 4)
	// 광고가 부족한 노드 — 판정은 실패하지만 이것은 API 에러가 아니다.
	node, ndr := seedNodeAndReport(t, geom, map[string]int64{"nvidia.com/mig-1g.6gb": 1})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, ndr).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()

	v := &Verifier{Client: c, Now: time.Now}
	ev, err := v.Verify(context.Background(), verifyRequest(geom))
	if err != nil {
		t.Fatalf("판정 실패를 에러로 올렸다: %v", err)
	}
	if ev.Status.Level != "" {
		t.Fatalf("불일치인데 등급이 붙었다: %q", ev.Status.Level)
	}
	if ev.Status.Reason == "" {
		t.Fatalf("실패 사유가 비었다")
	}
	if len(ev.Status.AdvertisedResources) != 0 {
		t.Fatalf("검증 실패인데 광고량을 기록했다 — drift 기준선이 오염된다: %+v", ev.Status.AdvertisedResources)
	}
}

func TestVerifyIsIdempotentAndUpdatesInPlace(t *testing.T) {
	geom := nvidia.GeometrySummary("1g.6gb", 4)
	node, ndr := seedNodeAndReport(t, geom, map[string]int64{"nvidia.com/mig-1g.6gb": 4})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, ndr).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()
	v := &Verifier{Client: c, Now: time.Now}
	if _, err := v.Verify(context.Background(), verifyRequest(geom)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), verifyRequest(geom)); err != nil {
		t.Fatal(err)
	}
	var list v1alpha1.AcceleratorEvidenceList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("evidence 가 %d 개다 — 노드당 하나여야 한다", len(list.Items))
	}
}

func TestVerifyRunsProbeOnlyWhenPolicyAsksForIt(t *testing.T) {
	geom := nvidia.GeometrySummary("1g.6gb", 4)
	node, ndr := seedNodeAndReport(t, geom, map[string]int64{"nvidia.com/mig-1g.6gb": 4})
	avpObj := &v1alpha1.AcceleratorVerificationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "with-probe"},
		Spec: v1alpha1.AcceleratorVerificationPolicySpec{
			NodeSelector: map[string]string{"role": "gpu"},
			Checks: []string{
				v1alpha1.EvidenceCheckDeviceObservation,
				v1alpha1.EvidenceCheckNodeDeviceReport,
				v1alpha1.EvidenceCheckAdvertisement,
				v1alpha1.EvidenceCheckAllocationProbe,
			},
		},
	}
	p := &fakeProber{allocated: true}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, ndr, avpObj).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()
	v := &Verifier{Client: c, Prober: p, Now: time.Now}
	ev, err := v.Verify(context.Background(), verifyRequest(geom))
	if err != nil {
		t.Fatal(err)
	}
	if p.called != 1 {
		t.Fatalf("프로브 호출 %d회", p.called)
	}
	if ev.Status.Level != v1alpha1.EvidenceLevelAllocationVerified {
		t.Fatalf("level = %q", ev.Status.Level)
	}
}

func TestVerifyDoesNotProbeWithoutAProber(t *testing.T) {
	// 정책이 프로브를 요구해도 프로버가 없으면 "돌렸다" 고 말하지 않는다 — 통과도 시키지 않는다.
	geom := nvidia.GeometrySummary("1g.6gb", 4)
	node, ndr := seedNodeAndReport(t, geom, map[string]int64{"nvidia.com/mig-1g.6gb": 4})
	avpObj := &v1alpha1.AcceleratorVerificationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "with-probe"},
		Spec: v1alpha1.AcceleratorVerificationPolicySpec{
			Checks: []string{v1alpha1.EvidenceCheckAllocationProbe},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, ndr, avpObj).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()
	v := &Verifier{Client: c, Now: time.Now}
	ev, err := v.Verify(context.Background(), verifyRequest(geom))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status.Level != "" {
		t.Fatalf("프로버 없이 할당 검증 등급을 줬다: %q", ev.Status.Level)
	}
}

func TestVerifyDoesNotProbeWhenPolicyDoesNotAskEvenWithProber(t *testing.T) {
	// 프로버가 배선돼 있어도 정책이 요구하지 않으면 돌리지 않는다 — 세 조건(정책·프로버·자원명)이
	// 각각 독립적으로 게이트여야 한다. 이 레그가 죽으면 프로버를 미리 꽂아 둔 모든 노드가 정책
	// 설정과 무관하게 매 검증마다 테스트 Pod 를 띄우게 된다.
	geom := nvidia.GeometrySummary("1g.6gb", 4)
	node, ndr := seedNodeAndReport(t, geom, map[string]int64{"nvidia.com/mig-1g.6gb": 4})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, ndr).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()
	p := &fakeProber{allocated: true}
	v := &Verifier{Client: c, Prober: p, Now: time.Now} // 정책 미배치 → DefaultPolicy(), probe off
	ev, err := v.Verify(context.Background(), verifyRequest(geom))
	if err != nil {
		t.Fatal(err)
	}
	if p.called != 0 {
		t.Fatalf("정책이 요구하지 않았는데 프로브가 %d회 호출됐다", p.called)
	}
	if ev.Status.Level != v1alpha1.EvidenceLevelFunctionallyVerified {
		t.Fatalf("level = %q", ev.Status.Level)
	}
}

func TestVerifyDoesNotProbeWhenProbeResourceEmpty(t *testing.T) {
	// 정책이 프로브를 요구하고 프로버도 있지만, 이 요청이 확인할 자원명을 안 준 경우 —
	// 프로브를 시도하지 않고, 시도 못 한 것을 실패로 기록한다(관측 부재를 통과로 승격하지 않는다).
	geom := nvidia.GeometrySummary("1g.6gb", 4)
	node, ndr := seedNodeAndReport(t, geom, map[string]int64{"nvidia.com/mig-1g.6gb": 4})
	avpObj := &v1alpha1.AcceleratorVerificationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "with-probe"},
		Spec: v1alpha1.AcceleratorVerificationPolicySpec{
			Checks: []string{v1alpha1.EvidenceCheckAllocationProbe},
		},
	}
	p := &fakeProber{allocated: true}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, ndr, avpObj).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()
	v := &Verifier{Client: c, Prober: p, Now: time.Now}
	req := verifyRequest(geom)
	req.Expectation.ProbeResource = ""
	ev, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if p.called != 0 {
		t.Fatalf("자원명이 없는데 프로브가 %d회 호출됐다", p.called)
	}
	if ev.Status.Level != "" {
		t.Fatalf("프로브를 시도 못 했는데 등급이 붙었다: %q", ev.Status.Level)
	}
}

func TestAllocatableOfClampsLargeQuantitiesInsteadOfTruncating(t *testing.T) {
	// node.status.allocatable 의 memory 는 바이트 단위다 — 270GB 는 int32 범위를 넘어(overflow)
	// int32(q.Value()) 로 그냥 캐스팅하면 음수로 뒤집힌다. drift 감시(다음 태스크)가 이 맵을
	// 현재값과 비교하므로, 뒤집힌 음수가 들어가면 원인을 찾기 어려운 오탐이 된다.
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceMemory: *resourceQuantity(270 * 1024 * 1024 * 1024),
			},
		},
	}
	got := allocatableOf(node)
	if got[string(corev1.ResourceMemory)] < 0 {
		t.Fatalf("큰 수량이 음수로 절단됐다: %d", got[string(corev1.ResourceMemory)])
	}
}

func TestLoadReturnsNilWhenAbsent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()
	v := &Verifier{Client: c, Now: time.Now}
	ev, err := v.Load(context.Background(), "nope")
	if err != nil || ev != nil {
		t.Fatalf("ev=%v err=%v", ev, err)
	}
}

// TestVerifyThenFreshnessSeam 은 Task 2·3·5·6 의 이음매다 — 각 단계가 따로 통과해도 이어
// 붙였을 때 재부팅 후 근거가 계속 신선하다고 나오면 아무 소용이 없다.
func TestVerifyThenFreshnessSeam(t *testing.T) {
	geom := nvidia.GeometrySummary("1g.6gb", 4)
	node, ndr := seedNodeAndReport(t, geom, map[string]int64{"nvidia.com/mig-1g.6gb": 4})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, ndr).
		WithStatusSubresource(&v1alpha1.AcceleratorEvidence{}).Build()
	now := time.Now()
	v := &Verifier{Client: c, Now: func() time.Time { return now }}
	ev, err := v.Verify(context.Background(), verifyRequest(geom))
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy()

	same := Compute(node, ndr, "nvidia", 3)
	if verdict, _ := CheckFreshness(ev, now, same, p.InvalidateOn); verdict != FreshValid {
		t.Fatalf("방금 만든 근거가 신선하지 않다: %q", verdict)
	}

	rebooted := node.DeepCopy()
	rebooted.Status.NodeInfo.BootID = "boot-2"
	after := Compute(rebooted, ndr, "nvidia", 3)
	if verdict, reason := CheckFreshness(ev, now, after, p.InvalidateOn); verdict != FreshEnvironmentChanged {
		t.Fatalf("재부팅 후에도 근거가 살아 있다: %q (%s)", verdict, reason)
	}
}

// TestReportGeometryKeepsDevicesWithoutPCI 는 PCI 를 채우지 않는 벤더(Furiosa detector)의 보고서도
// "장치가 있다" 로 읽히는지 고정한다. PCI 없는 항목을 버리면 evalNodeDeviceReport 가 멀쩡한
// 보고서를 "보고서 없음" 으로 판정해 근거가 영영 등급을 못 받는다(2026-08-04 라이브).
func TestReportGeometryKeepsDevicesWithoutPCI(t *testing.T) {
	ndr := &v1alpha1.NodeDeviceReport{Status: v1alpha1.NodeDeviceReportStatus{
		Devices: []v1alpha1.DeviceEntry{{Vendor: "furiosa", Model: "rngd", Count: 1}},
	}}
	got := reportGeometry(ndr, "furiosa", nil)
	if len(got) != 1 {
		t.Fatalf("reportGeometry = %v, want one entry for the PCI-less device", got)
	}
}

// TestReportGeometryScopesToTargets 는 정책이 건드리지 않은 장치를 보고서 대조에서 빼는지 본다.
// 빼지 않으면 같은 노드의 비대상 GPU(A2)가 정책의 기대 geometry 로 판정돼 절대 통과할 수 없다
// (2026-08-04 라이브: "report geometry \"\" != expected \"1g.6gb x4\"" 가 A2 에서도 났다).
func TestReportGeometryScopesToTargets(t *testing.T) {
	ndr := &v1alpha1.NodeDeviceReport{Status: v1alpha1.NodeDeviceReportStatus{
		Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", PCIeAddress: "0000:18:00.0", MigCurrentGeometry: "1g.6gb x4"},
			{Vendor: "nvidia", PCIeAddress: "0000:86:00.0"},
		},
	}}
	got := reportGeometry(ndr, "nvidia", []string{"0000:18:00.0"})
	if len(got) != 1 || got["0000:18:00.0"] != "1g.6gb x4" {
		t.Fatalf("reportGeometry = %v, want only the target device", got)
	}
}
