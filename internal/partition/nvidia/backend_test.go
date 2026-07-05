// ============================================================
// backend_test.go: NVIDIA backend 단위 테스트 — discovery + Apply/Verify/Rollback/Diff
// 생성일: 2026-07-23 | 수정일: 2026-07-31
// ============================================================
package nvidia

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition"
)

// scheme 은 NDR(v1alpha1) + 코어/앱 타입(sharing 이 쓰는 ConfigMap·DaemonSet)을 등록한다.
func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	_ = clientgoscheme.AddToScheme(s)
	return s
}

// fakeExecutor 는 Executor 를 구현해 Apply/Rollback 이 실행하는 opID/steps 를 기록한다.
type fakeExecutor struct {
	err      error
	gotOpID  string
	gotHash  string
	gotNode  string
	gotSteps []CommandStep
}

func (f *fakeExecutor) Run(_ context.Context, opID, cmdHash, nodeName string, steps []CommandStep) error {
	f.gotOpID, f.gotHash, f.gotNode, f.gotSteps = opID, cmdHash, nodeName, steps
	return f.err
}

// fakeVerifier 는 partition.Verifier 를 구현해 Verify 에 넘겨진 expected 맵을 기록한다.
type fakeVerifier struct {
	res         *partition.VerifyResult
	err         error
	gotExpected map[string]int32
}

func (f *fakeVerifier) VerifyAllocatable(_ partition.Target, expected map[string]int32) (*partition.VerifyResult, error) {
	f.gotExpected = expected
	return f.res, f.err
}

func (f *fakeVerifier) VerifyAllocation(partition.Target, string) (*partition.VerifyResult, error) {
	return f.res, f.err
}

const lgipA30 = `|   0  MIG 1g.6gb   19  4/4  5.75 |
|   0  MIG 2g.12gb  14  2/2  11.62 |
|   0  MIG 4g.24gb   5  1/1  23.37 |`

func TestNvidiaDiscover_A30andA2(t *testing.T) {
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "NVIDIA-A30", PCIeAddress: "0000:41:00.0", MigLgipOutput: lgipA30},
			{Vendor: "nvidia", Model: "NVIDIA-A2", PCIeAddress: "0000:c1:00.0"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c)
	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Devices) != 2 {
		t.Fatalf("want 2 devices, got %d", len(res.Devices))
	}
	var a30, a2 *v1alpha1.DeviceStatus
	for i := range res.Devices {
		switch res.Devices[i].Model {
		case "NVIDIA-A30":
			a30 = &res.Devices[i]
		case "NVIDIA-A2":
			a2 = &res.Devices[i]
		}
	}
	if !a30.PartitionCapability.HardwareSupported || a30.PartitionCapability.PartitionModel != "ProfileConstrained" {
		t.Errorf("A30 capability: %+v", a30.PartitionCapability)
	}
	if len(a30.PartitionCapability.Profiles) != 3 {
		t.Errorf("A30 profiles: %+v", a30.PartitionCapability.Profiles)
	}
	if a30.Operations.Apply.Supported || a30.Operations.Apply.Reason != v1alpha1.ReasonApplyBackendNotInstalled {
		t.Errorf("A30 apply should be unsupported: %+v", a30.Operations)
	}
	if a2.PartitionCapability.HardwareSupported {
		t.Errorf("A2 should be hardwareSupported=false")
	}
	if a2.PartitionCapability.PartitionModel != "None" || a2.PartitionCapability.Reason != v1alpha1.ReasonHardwareCapabilityMissing {
		t.Errorf("A2 capability: %+v", a2.PartitionCapability)
	}
	if a2.Operations.Apply.Supported {
		t.Errorf("A2 apply should be unsupported: %+v", a2.Operations)
	}
}

func TestNvidiaValidate_Scope(t *testing.T) {
	b := New(fake.NewClientBuilder().WithScheme(scheme()).Build()).withSupported(map[string]int32{"2g.12gb": 1})
	// 형식(<digit>g.<digit>gb) + count 경계 + supported profile 모두 충족하면 통과.
	if err := b.Validate([]partition.Layout{{Profile: "2g.12gb", CountPerDevice: 1}}); err != nil {
		t.Errorf("known-format profile rejected: %v", err)
	}
	// 잘못된 형식은 형식 검사 단계에서 즉시 거부(느슨한 substring 매칭 회귀 방지).
	for _, bad := range []string{"g.gb", "foog.barzgb", "2core.12gb", "2g12gb", ""} {
		if err := b.Validate([]partition.Layout{{Profile: bad, CountPerDevice: 1}}); err != partition.ErrUnsupported {
			t.Errorf("malformed profile %q should be ErrUnsupported, got %v", bad, err)
		}
	}
	// resolved=nil(!=1 항목) 은 mixed-placement 미지원으로 거부한다(§14 Task 7 Step 1).
	if _, err := b.Apply(partition.Target{}, nil); err != ErrMixedPlacementUnsupported {
		t.Errorf("Apply with resolved!=1 must be ErrMixedPlacementUnsupported, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	b := New(nil).withSupported(map[string]int32{"1g.6gb": 4})
	cases := []struct {
		name string
		in   []partition.Layout
		ok   bool
	}{
		{"empty", nil, false}, // layout 도 sharing 도 없음 — sharing-only 는 controller 가 Validate 를 부르지 않는다(D-3)
		{"mixed", []partition.Layout{{Profile: "1g.6gb", CountPerDevice: 1}, {Profile: "2g.12gb", CountPerDevice: 1}}, false},
		{"badFormat", []partition.Layout{{Profile: "dual-core", CountPerDevice: 4}}, false},
		{"zeroCount", []partition.Layout{{Profile: "1g.6gb", CountPerDevice: 0}}, false},
		{"overMax", []partition.Layout{{Profile: "1g.6gb", CountPerDevice: 5}}, false},
		{"unknownProfile", []partition.Layout{{Profile: "9g.99gb", CountPerDevice: 1}}, false},
		{"ok", []partition.Layout{{Profile: "1g.6gb", CountPerDevice: 4}}, true},
	}
	for _, c := range cases {
		if err := b.Validate(c.in); (err == nil) != c.ok {
			t.Fatalf("%s: ok=%v err=%v", c.name, c.ok, err)
		}
	}
}

// TestDiscoverFillsCapabilityAxes 는 deviceStatusFor 가 capability 3축(Task 1)을 채우는지 검증한다.
func TestDiscoverFillsCapabilityAxes(t *testing.T) {
	b := New(nil).withSupported(map[string]int32{"1g.6gb": 7})
	d := b.deviceStatusFor(MigDevice{PCI: "0000:18:00.0", ModeCurrent: "Enabled"}, "NVIDIA A30")
	if !d.SharingCapability.TimeSlicing.Supported {
		t.Fatalf("nvidia time-slicing must be supported: %+v", d.SharingCapability)
	}
	if d.SharingCapability.TimeSlicing.Verification != v1alpha1.VerificationRequired {
		t.Fatalf("verification must start as required, got %q", d.SharingCapability.TimeSlicing.Verification)
	}
	if d.IsolationCapability.Compute != v1alpha1.IsolationHardware {
		t.Fatalf("MIG compute isolation = %q, want hardware", d.IsolationCapability.Compute)
	}
	if len(d.AllocationAPIs) != 1 || d.AllocationAPIs[0] != v1alpha1.AllocationAPIDevicePlugin {
		t.Fatalf("allocationAPIs = %v", d.AllocationAPIs)
	}
}

// 실측 주입(MultiProcessEnv)이 없으면 MPS 는 어느 경우에도 verified 가 아니다 — 기본값은
// "모른다" 다. 주입이 있을 때의 승격 규칙은 TestMultiProcessVerification_RequiresMeasurementEnv 가 본다.
func TestSharingCapability_MPSNeverClaimsVerified(t *testing.T) {
	for _, tc := range []struct {
		name          string
		migMode       string
		obsErr        string
		wantSupported bool
	}{
		{"MIG 켜짐", modeEnabled, "", false},
		{"MIG 꺼짐", modeDisabled, "", true},
		{"관측 실패", "", "exec failed", false},
		// review 최종 ①: 에러 없이 mode 가 비어 있는 것은 "MIG 가 꺼졌다" 가 아니라 "아무도
		// 안 봤다" 다. 이전 규칙(ModeCurrent != modeEnabled)은 이 둘을 같게 취급했다.
		{"관측 없음(에러도 없음)", "", "", false},
		{"mode Unknown", modeUnknown, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := New(nil).withSupported(map[string]int32{"1g.6gb": 7})
			got := b.deviceStatusFor(MigDevice{PCI: "0000:18:00.0", ModeCurrent: tc.migMode, ObsError: tc.obsErr}, "NVIDIA A30").SharingCapability.MultiProcess
			if got.Supported != tc.wantSupported {
				t.Errorf("supported: got %v want %v (reason=%q)", got.Supported, tc.wantSupported, got.Reason)
			}
			if got.Verification == v1alpha1.VerificationVerified {
				t.Error("실측 전에 verified 를 주장하면 안 된다")
			}
			// 지원=true 쪽도 사유가 있어야 한다(review 최종 ①): Task 6 F5 가 "미지원 + 빈 사유" 를
			// 결함으로 고쳤는데 반대쪽("MIG 꺼짐 → 지원" 정상 경로)은 빈 사유로 남아 있었고,
			// 검사가 `!Supported && Reason == ""` 라 아무도 그것을 보지 않았다.
			if got.Reason == "" {
				t.Errorf("판정 사유가 없다(supported=%v) — 사용자가 왜인지 알 수 없다", got.Supported)
			}
		})
	}
}

// F5(review): MIG 자체가 없는 장치(A2 등)는 capability 3축 루프를 건너뛰어 MultiProcess 가
// zero-value(Supported:false, Reason:"")로 남는다 — 이 클러스터의 유일한 MPS-eligible GPU(A2,
// A30 은 MIG Enabled)가 "미지원, 이유 없음"으로 보이는 셈이다. MIG 가 없으면 MIG-MPS 상호배타
// 자체가 성립하지 않으므로 지원 가능해야 하고(실측은 없으니 Verification 은 여전히 required), 이유도
// 남아야 한다.
func TestNvidiaDiscover_NonMIGDeviceGetsHonestMPSJudgment(t *testing.T) {
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "NVIDIA-A2", PCIeAddress: "0000:c1:00.0"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c)
	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(res.Devices))
	}
	mp := res.Devices[0].SharingCapability.MultiProcess
	if !mp.Supported {
		t.Errorf("non-MIG 장치는 MIG-MPS 충돌이 없으므로 MPS 지원돼야 한다: %+v", mp)
	}
	if mp.Verification == v1alpha1.VerificationVerified {
		t.Error("실측 전에 verified 를 주장하면 안 된다")
	}
	if mp.Reason == "" {
		t.Error("판정 사유가 없다 — 사용자가 왜인지 알 수 없다")
	}
}

// 라이브에서 MPS 동시 실행이 실측됐다(물리 GPU 1장당 CUDA 프로세스 2개, v0.5.78). 그 사실을
// 코드가 스스로 주장하면 "이 클러스터의 A30/A2 에서 됐다"가 "모든 NVIDIA GPU 가 MPS 를
// 지원한다"로 바뀐다 — RNGD 가 같은 문제를 실측 주입 env 로 풀었고(MultiProcessSupportFromEnv),
// 같은 규율을 따른다.
//
// 승격은 **검증 축만** 올린다. Supported 는 장치별 관측(MIG-MPS 상호배타)이 정하는 값이고,
// 실측이 말해 주는 것은 "이 배포에서 이 메커니즘이 동작한다" 뿐이다. 그래서 지원 판정이
// 서지 않은 장치는 주입이 있어도 required 로 남아야 한다 — 그러지 않으면 "MIG 가 켜져 MPS 를
// 쓸 수 없다"는 판정에 "실측 완료" 가 붙는 모순이 status 에 실린다.
func TestMultiProcessVerification_RequiresMeasurementEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		migMode string
		obsErr  string
		env     string
		want    string
	}{
		{"주입 없음 + 지원", modeDisabled, "", "", v1alpha1.VerificationRequired},
		{"주입 + 지원", modeDisabled, "", "verified", v1alpha1.VerificationVerified},
		{"주입 + MIG 켜짐(미지원)", modeEnabled, "", "verified", v1alpha1.VerificationRequired},
		{"주입 + 관측 실패", "", "exec failed", "verified", v1alpha1.VerificationRequired},
		{"주입 + 관측 없음", "", "", "verified", v1alpha1.VerificationRequired},
		// 알 수 없는 값은 미주입과 같게 다룬다(fail-closed) — 오타가 승격으로 읽히면 안 된다.
		{"알 수 없는 주입값", modeDisabled, "", "yes", v1alpha1.VerificationRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(MultiProcessEnv, tc.env)
			b := New(nil).withSupported(map[string]int32{"1g.6gb": 7})
			got := b.deviceStatusFor(MigDevice{PCI: "0000:18:00.0", ModeCurrent: tc.migMode, ObsError: tc.obsErr}, "NVIDIA A30").SharingCapability.MultiProcess
			if got.Verification != tc.want {
				t.Errorf("verification = %q, want %q (supported=%v reason=%q)",
					got.Verification, tc.want, got.Supported, got.Reason)
			}
		})
	}
}

// 비-MIG 장치(A2)는 capability 3축 루프를 건너뛰고 Discover 루프의 else 분기에서 MPS 판정을
// 받는다 — 승격도 그 사이트에 같이 들어가야 한다. 라이브에서 실제로 MPS 를 돌린 장치가 A2 다.
func TestMultiProcessVerification_NonMIGDevicePromotesToo(t *testing.T) {
	t.Setenv(MultiProcessEnv, "verified")
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "NVIDIA-A2", PCIeAddress: "0000:c1:00.0"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	res, err := New(c).Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	mp := res.Devices[0].SharingCapability.MultiProcess
	if !mp.Supported || mp.Verification != v1alpha1.VerificationVerified {
		t.Errorf("★ 비-MIG 장치의 MPS 판정이 승격되지 않았다: %+v", mp)
	}
}

// 장치 단위 게이트는 두 판정 사이트에 각각 있어야 한다. deviceStatusFor 쪽만 테스트가 잡고
// 있어서, 비-MIG 사이트의 게이트를 지워도 아무 테스트도 깨지지 않았다(리뷰 뮤테이션 결과).
// 관측이 mode 를 확정하지 못한 장치는 주입이 있어도 required 다 — "지원 여부 미확정" 에
// "실측 완료" 가 붙으면 status 가 스스로 모순한다.
func TestMultiProcessVerification_NonMIGSiteGatesOnSupport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mc      string
		obsErr  string
		model   string
		wantSup bool
		want    string
	}{
		{"MIG 미지원 장치", modeNA, "", "NVIDIA-A2", true, v1alpha1.VerificationVerified},
		// 관측이 실패해도 model 문자열이 신뢰 가능하면 그것으로 판정한다(최후 폴백) — 지원 판정이
		// 서므로 승격 대상이다. 그 폴백까지 막히는 경우가 아래 required 행들이다.
		{"관측 에러 + 신뢰 가능한 model", modeNA, "exec failed", "NVIDIA-A2", true, v1alpha1.VerificationVerified},
		// 아래는 전부 "지원 여부를 확정하지 못했다" — 승격 대상이 아니다.
		{"관측 에러 + model 불명", modeNA, "exec failed", "generic", false, v1alpha1.VerificationRequired},
		{"mode 미관측 + model 불명", "", "", "generic", false, v1alpha1.VerificationRequired},
		{"mode 미관측 + model 빈값", "", "", "", false, v1alpha1.VerificationRequired},
		{"profile 은 못 읽었는데 mode 는 Enabled", modeEnabled, "", "NVIDIA-A30", false, v1alpha1.VerificationRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(MultiProcessEnv, "verified")
			got := mpsSupportForNonMigDevice(tc.mc, tc.obsErr, tc.model)
			if got.Supported != tc.wantSup {
				t.Fatalf("supported = %v, want %v (reason=%q)", got.Supported, tc.wantSup, got.Reason)
			}
			if got.Verification != tc.want {
				t.Errorf("★ verification = %q, want %q — 지원 판정이 서지 않은 장치는 주입이 있어도 승격 대상이 아니다",
					got.Verification, tc.want)
			}
		})
	}
}

// time-slicing 도 같은 규율이다 — 이전 트랙에서 라이브 검증됐으나 코드가 그것을 스스로 주장할
// 근거는 없다. 별도 env 를 둔다: 두 기능은 서로 다른 실측이라 한 값으로 묶으면 하나만 검증한
// 배포가 둘 다 검증됐다고 말하게 된다.
func TestTimeSlicingVerification_RequiresMeasurementEnv(t *testing.T) {
	b := New(nil).withSupported(map[string]int32{"1g.6gb": 7})
	if got := b.deviceStatusFor(MigDevice{PCI: "0000:18:00.0", ModeCurrent: modeDisabled}, "NVIDIA A30").
		SharingCapability.TimeSlicing.Verification; got != v1alpha1.VerificationRequired {
		t.Errorf("주입 없이 verification = %q, want required", got)
	}
	t.Setenv(TimeSlicingEnv, "verified")
	if got := b.deviceStatusFor(MigDevice{PCI: "0000:18:00.0", ModeCurrent: modeDisabled}, "NVIDIA A30").
		SharingCapability.TimeSlicing.Verification; got != v1alpha1.VerificationVerified {
		t.Errorf("★ 실측 주입 후에도 verification = %q, want verified", got)
	}
}

// F7(review, 회귀): HardwareSupported=false 는 "MIG 자체가 없다"와 "MIG-capable 인데 관측이
// 실패해 상태를 확정 못 했다"를 둘 다 포괄한다(migCap = len(profiles)>0 || (mc==Disabled &&
// obsErr=="") — 관측 주입 모드에서 MIG-capable 장치도 관측 실패로 false 가 될 수 있다). F5 의
// 수정이 이 둘을 구분 안 하면 실제로 MIG Enabled 인 A30 이 "MIG 없음, 그러니 MPS 확실히 지원"
// 이라는 confident false positive 를 낸다 — PartitionCapability 축이 이미 지키는 원칙
// (TestPartitionCapability_ObservationFailureIsNotHardwareVerdict)을 MultiProcess 축에서 어기는 것.
//
// 시나리오는 "mode-pending 파싱 실패" 다(review 최종 ⑦): 이전 픽스처는
// {ModeCurrent: Enabled, Err: "enabled but lgi unparseable"} 를 손으로 꽂았는데, observe.go:93 은
// 정확히 그 실패에서 ModeCurrent 를 modeUnknown 으로 강등하므로 parseObservation 이 절대 내놓지
// 않는 삼중항이었다 — 성립하지 않는 전제를 주석으로 문서화하고 있었다. 이제 픽스처를
// parseObservation 으로 구동해 실재하는 조합만 검증한다.
func TestNvidiaDiscover_MIGCapableObservationFailureStaysUnsupported(t *testing.T) {
	pci := obsTestPCI
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "NVIDIA-A30", PCIeAddress: pci},
		}},
	}
	// {Enabled, Unknown, Err:"unrecognized mig.mode.pending"} — 실제로 나오는 부분 실패 관측.
	obs := parseObservation(pci, "Enabled, bogus", "", "")
	if obs.ModeCurrent != modeEnabled || obs.Err == "" {
		t.Fatalf("픽스처 전제가 깨졌다: %+v", obs)
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c).WithObservations(map[string]Observation{pci: obs})
	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	mp := res.Devices[0].SharingCapability.MultiProcess
	if mp.Supported {
		t.Errorf("MIG-capable 장치(A30)는 관측 실패만으로 MPS 지원을 확신하면 안 된다: %+v", mp)
	}
	if mp.Verification == v1alpha1.VerificationVerified {
		t.Error("실측 전에 verified 를 주장하면 안 된다")
	}
	if mp.Reason == "" {
		t.Error("판정 사유가 없다 — 사용자가 왜인지 알 수 없다")
	}
}

// F7(review, 재발) — 이 클러스터의 실제 상태: detector 가 host nvidia-smi 를 못 읽어 A30 을
// model="generic" 으로 보고한다. migCapable 은 모델명 정규식이라 "generic" 은 항상 false —
// 관측된 mode(Enabled/Disabled)가 있는데도 모델만으로 판정하면 이 케이스를 여전히 놓친다.
// 관측이 실제 모드를 말해주면(Enabled/Disabled) 모델 문자열보다 그 신호를 신뢰해야 한다.
//
// ModeCurrent 는 반드시 parseObservation 이 실제로 내놓는 값을 써야 한다 — "enabled but lgi
// unparseable"(observe.go:93)은 ModeCurrent 를 Enabled 가 아니라 Unknown 으로 낮춘다(fail-closed
// 파싱 규칙). 이전 버전의 이 테스트는 ModeCurrent:modeEnabled 를 직접 꽂아 넣어(parseObservation
// 을 우회) 실제로는 절대 안 나오는 조합을 검증했고, 그래서 진짜 결함(model=generic + Unknown)을
// 못 잡았다.
func TestNvidiaDiscover_GenericModelWithFailedObservationStaysUnsupported(t *testing.T) {
	pci := obsTestPCI
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "generic", PCIeAddress: pci},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c).WithObservations(map[string]Observation{
		pci: {PCI: pci, ModeCurrent: modeUnknown, ModePending: modeEnabled, Err: "enabled but lgi unparseable: boom"},
	})
	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	mp := res.Devices[0].SharingCapability.MultiProcess
	if mp.Supported {
		t.Errorf("★ model=generic(모델 자체를 모름) + 관측 실패는 MPS 지원을 확신하면 안 된다: %+v", mp)
	}
	if mp.Verification == v1alpha1.VerificationVerified {
		t.Error("실측 전에 verified 를 주장하면 안 된다")
	}
	if mp.Reason == "" {
		t.Error("판정 사유가 없다")
	}
}

// review 최종 ④(테스트 갭): 라운드 4·5 가 만든 "관측 mode 우선" 두 arm 은 삭제해도 스위트가
// 초록이었다 — 남는 코드가 정확히 라운드 3(컨트롤러가 broken 으로 판정한 버전)인데도 그렇다.
// 여기 두 케이스가 그 두 arm 을 각각 고정한다. 픽스처는 반드시 parseObservation 을 구동해 만든다
// (라운드 4 교훈: 손으로 조립한 Observation 은 실제 파서가 내놓지 않는 상태일 수 있다).
//
// 1) mc == modeNA arm — 프로덕션의 A2 가 항상 밟는 경로다(controller.go:504 가 전 nvidia PCI 를
// 관측하므로 A2 도 언제나 진짜 mig.mode.current == N/A 를 받는다). 즉 이 클러스터에서 실제로
// 가장 많이 실행되는 arm 인데 테스트가 하나도 없었다. model 은 "generic" 으로 둬서 model 문자열
// arm 이 답을 낼 수 없게 한다 — 답을 낸 것이 modeNA arm 임을 증명한다.
// 두 번째 행은 review 최종 ⑥ 이다: 형제 판정 사이트(deviceStatusFor)는 ObsError 가 있으면
// 무조건 판정을 거부하는데, 이쪽만 부분 실패한 관측의 N/A 를 신뢰해 supported=true 를 냈다.
func TestNvidiaDiscover_ModeNAFromDriverIsTrustedForMPS(t *testing.T) {
	for _, tc := range []struct {
		name, modeCsv, lgi string
		wantSupported      bool
	}{
		{"A2 정상 관측", "[N/A], [N/A]", "No MIG-enabled devices", true},
		{"N/A + pending 파싱 실패", "[N/A], bogus", "No MIG-enabled devices", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pci := obsTestPCI
			obs := parseObservation(pci, tc.modeCsv, tc.lgi, "")
			if obs.ModeCurrent != modeNA {
				t.Fatalf("픽스처 전제가 깨졌다(current 는 N/A 여야 한다): %+v", obs)
			}
			if (obs.Err == "") != tc.wantSupported {
				t.Fatalf("픽스처 전제가 깨졌다(관측 성패가 케이스와 다르다): %+v", obs)
			}
			ndr := &v1alpha1.NodeDeviceReport{
				ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
				Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
					{Vendor: "nvidia", Model: "generic", PCIeAddress: pci}, // detector 는 실 모델명을 못 본다
				}},
			}
			c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
			res, err := New(c).WithObservations(map[string]Observation{pci: obs}).
				Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
			if err != nil {
				t.Fatal(err)
			}
			mp := res.Devices[0].SharingCapability.MultiProcess
			if mp.Supported != tc.wantSupported {
				t.Errorf("★ supported = %v, want %v: %+v", mp.Supported, tc.wantSupported, mp)
			}
			if mp.Verification == v1alpha1.VerificationVerified {
				t.Error("실측 전에 verified 를 주장하면 안 된다")
			}
			if mp.Reason == "" {
				t.Error("판정 사유가 없다")
			}
		})
	}
}

// 2) 관측 mode 가 model 문자열보다 우선한다는 라운드 4 의 명제 자체를 고정한다. model 을 실재하는
// 비-MIG 문자열("NVIDIA-A2")로 두면 model arm 은 "MPS 지원" 이라고 말하는데 관측된 mode 는
// Enabled 다 — mode 가 이겨 미지원이어야 한다. 이 명제를 검증하는 테스트가 지금까지 없었다.
func TestNvidiaDiscover_ObservedModeBeatsModelString(t *testing.T) {
	pci := obsTestPCI
	obs := parseObservation(pci, "Enabled, bogus", "", "")
	if obs.ModeCurrent != modeEnabled {
		t.Fatalf("픽스처 전제가 깨졌다(current 는 정상 파싱돼야 한다): %+v", obs)
	}
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "NVIDIA-A2", PCIeAddress: pci}, // 실재 + migCapable=false
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	res, err := New(c).WithObservations(map[string]Observation{pci: obs}).
		Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	mp := res.Devices[0].SharingCapability.MultiProcess
	if mp.Supported {
		t.Errorf("★ mode=Enabled 를 관측했는데 model 문자열(A2)이 이겨 MPS 지원을 주장했다: %+v", mp)
	}
	if mp.Reason == "" {
		t.Error("판정 사유가 없다")
	}
}

// F(테스트 갭, review): TestSharingCapability_MPSNeverClaimsVerified 는 deviceStatusFor 를 직접
// 호출해 Discover()/targetFor() 를 거치지 않는다 — targetFor 를 되돌려도(PCI 만 담긴 빈 MigDevice)
// 그 테스트는 그대로 통과한다. MIG-Enabled A30 을 관측 주입해 실제 Discover 경로로
// MultiProcess.Supported == false 를 고정한다(targetFor 가 반영되지 않으면 이 값은 실패한다).
func TestDiscover_MPSUnsupportedOnMIGEnabledDeviceViaRealPath(t *testing.T) {
	pci := obsTestPCI
	lgip, err := os.ReadFile("testdata/mig_lgip_580.txt")
	if err != nil {
		t.Fatal(err)
	}
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "NVIDIA-A30", PCIeAddress: pci},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c).WithObservations(map[string]Observation{
		pci: {PCI: pci, ModeCurrent: modeEnabled, ModePending: modeEnabled, LgipOutput: string(lgip)},
	})
	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(res.Devices))
	}
	mp := res.Devices[0].SharingCapability.MultiProcess
	if mp.Supported {
		t.Errorf("MIG Enabled 장치는 실제 Discover 경로로도 MPS 미지원이어야 한다: %+v", mp)
	}
	if mp.Reason == "" {
		t.Error("판정 사유가 없다")
	}
}

// TestMigCapable 는 토큰 경계 매칭으로 RTX A3000(비-MIG) 오분류를 방지하는지 검증한다.
func TestMigCapable(t *testing.T) {
	capable := []string{"NVIDIA-A30", "NVIDIA A100-SXM4-40GB", "NVIDIA H100 PCIe", "a30"}
	notCapable := []string{"NVIDIA-A2", "NVIDIA RTX A3000", "NVIDIA A1000", "NVIDIA-A30000", "Tesla T4"}
	for _, m := range capable {
		if !migCapable(m) {
			t.Errorf("%q should be MIG-capable", m)
		}
	}
	for _, m := range notCapable {
		if migCapable(m) {
			t.Errorf("%q should NOT be MIG-capable (substring false-positive)", m)
		}
	}
}

func TestApply_PartialFailReturnsRollbackState(t *testing.T) {
	exec := &fakeExecutor{err: errors.New("boom")}
	b := New(nil).WithExecutor(exec).withTargets([]MigDevice{{PCI: "0000:41:00.0", ModeCurrent: "Disabled"}})
	target := partition.Target{Ctx: context.Background(), NodeName: "worker1", Owner: "acpp-a", Generation: 1}
	resolved := []partition.ResolvedEntry{{Profile: "1g.6gb", ExpectedCountPerDevice: 4}}

	rb, err := b.Apply(target, resolved)
	if err == nil {
		t.Fatal("expected exec error to propagate")
	}
	if rb == nil || rb.PrevPolicy != "disabled" {
		t.Fatalf("rollback state must be non-nil with PrevPolicy=disabled on partial failure, got %+v", rb)
	}
	if len(exec.gotSteps) == 0 {
		t.Fatal("expected steps recorded on executor")
	}
	last := exec.gotSteps[len(exec.gotSteps)-1]
	wantArgv := []string{"nvidia-smi", "mig", "-i", "0000:41:00.0", "-cgi", "1g.6gb,1g.6gb,1g.6gb,1g.6gb", "-C"}
	if !reflect.DeepEqual(last.Argv, wantArgv) {
		t.Errorf("last step argv = %v, want %v", last.Argv, wantArgv)
	}
}

func TestVerify_UsesInjectedExpected(t *testing.T) {
	fv := &fakeVerifier{res: &partition.VerifyResult{AllocatableConverged: true}}
	expected := map[string]int32{"nvidia.com/mig-1g.6gb": 4, "nvidia.com/gpu": 0}
	b := New(nil).WithVerifier(fv).WithExpected(expected)

	res, err := b.Verify(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fv.gotExpected, expected) {
		t.Errorf("verifier got expected = %v, want %v", fv.gotExpected, expected)
	}
	if res == nil || !res.AllocatableConverged {
		t.Errorf("res = %+v, want AllocatableConverged=true", res)
	}
}

func TestVerify_NilVerifierReturnsNil(t *testing.T) {
	b := New(nil)
	res, err := b.Verify(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil || res != nil {
		t.Errorf("expected (nil, nil), got (%+v, %v)", res, err)
	}
}

func TestRollback_DisablesViaPci(t *testing.T) {
	exec := &fakeExecutor{}
	b := New(nil).WithExecutor(exec).withTargets([]MigDevice{{PCI: "0000:41:00.0"}})
	target := partition.Target{Ctx: context.Background(), NodeName: "worker1", Owner: "acpp-a", Generation: 1}

	if err := b.Rollback(target, partition.RollbackState{}); err != nil {
		t.Fatal(err)
	}
	// 모델 B(§17.1): rollback 은 GI 제거(-dgi)만, mode 는 끄지 않는다(-mig 0 금지).
	wantRemoveGI := []string{"nvidia-smi", "mig", "-i", "0000:41:00.0", "-dgi"}
	foundGI := false
	for _, s := range exec.gotSteps {
		if reflect.DeepEqual(s.Argv, wantRemoveGI) {
			foundGI = true
		}
		if len(s.Argv) >= 2 && s.Argv[len(s.Argv)-2] == "-mig" && s.Argv[len(s.Argv)-1] == "0" {
			t.Errorf("rollback must NOT switch mode off (-mig 0): %+v", exec.gotSteps)
		}
	}
	if !foundGI {
		t.Errorf("rollback steps missing -dgi GI-removal command: %+v", exec.gotSteps)
	}
}

func TestDiff_NoDiffWhenGeometryMatches(t *testing.T) {
	b := New(nil).withTargets([]MigDevice{{PCI: "0000:41:00.0", Geometry: "1g.6gb x4"}})
	res, err := b.Diff(partition.Target{}, []partition.ResolvedEntry{{Profile: "1g.6gb", ExpectedCountPerDevice: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Errorf("expected no diff when geometry matches, got %+v", res)
	}
}

func TestDiff_ChangedWhenDisabled(t *testing.T) {
	b := New(nil).withTargets([]MigDevice{{PCI: "0000:41:00.0", Geometry: ""}})
	res, err := b.Diff(partition.Target{}, []partition.ResolvedEntry{{Profile: "1g.6gb", ExpectedCountPerDevice: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Errorf("expected diff when disabled, got %+v", res)
	}
}

func TestDiscover_UsesInjectedObservations(t *testing.T) {
	lgip, err := os.ReadFile("testdata/mig_lgip_580.txt")
	if err != nil {
		t.Fatal(err)
	}
	pci := obsTestPCI
	// NDR 에 A30 을 시드하되 MIG 필드는 EMPTY — 관측이 진실 소스임을 강제 확인.
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "NVIDIA-A30", PCIeAddress: pci},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c).WithObservations(map[string]Observation{
		pci: {PCI: pci, ModeCurrent: modeDisabled, ModePending: modeDisabled, Geometry: geomDisabled, LgipOutput: string(lgip)},
	})

	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Advertisement.Mode != advProfileNamed {
		t.Errorf("Advertisement.Mode = %q, want ProfileNamed", res.Advertisement.Mode)
	}
	if !res.Operations.Apply.Supported {
		t.Errorf("Apply.Supported should be true with injected MIG target: %+v", res.Operations)
	}
	if len(b.targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(b.targets))
	}
	if b.targets[0].Geometry != geomDisabled {
		t.Errorf("target geometry = %q, want disabled (from observation)", b.targets[0].Geometry)
	}
	if b.supported["1g.6gb"] <= 0 {
		t.Errorf("supported[1g.6gb] = %d, want >0 (from observation lgip)", b.supported["1g.6gb"])
	}

	// injected observation 으로 세운 supported 로 Validate/Apply 가 진행되는지 확인.
	if err := b.Validate([]partition.Layout{{Profile: "1g.6gb", CountPerDevice: 4}}); err != nil {
		t.Errorf("Validate should pass with observed profile: %v", err)
	}
	rb, err := b.Apply(
		partition.Target{Ctx: context.Background(), NodeName: "worker1", Owner: "acpp-a", Generation: 1},
		[]partition.ResolvedEntry{{Profile: "1g.6gb", ExpectedCountPerDevice: 4}},
	)
	if err != nil || rb == nil {
		t.Errorf("Apply should succeed (nil exec no-op) with observed target, rb=%+v err=%v", rb, err)
	}
}

func TestDiscover_TargetsSupportedAndProfileNamed(t *testing.T) {
	lgip, err := os.ReadFile("testdata/mig_lgip_580.txt")
	if err != nil {
		t.Fatal(err)
	}
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "NVIDIA-A30", PCIeAddress: "0000:41:00.0", MigModeCurrent: "Disabled", MigLgipOutput: string(lgip)},
			{Vendor: "nvidia", Model: "NVIDIA-A2", PCIeAddress: "0000:c1:00.0"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c)

	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Operations.Apply.Supported {
		t.Errorf("Apply.Supported should be true when a MIG-capable target is present: %+v", res.Operations)
	}
	if !res.Operations.Rollback.Supported {
		t.Errorf("Rollback.Supported should be true: %+v", res.Operations)
	}
	if res.Advertisement.Mode != "ProfileNamed" {
		t.Errorf("Advertisement.Mode = %q, want ProfileNamed", res.Advertisement.Mode)
	}
	if len(b.targets) != 1 {
		t.Fatalf("want 1 MIG target (A30 only), got %d: %+v", len(b.targets), b.targets)
	}
	if b.targets[0].PCI == "" {
		t.Errorf("target PCI must be set")
	}
	if b.supported["1g.6gb"] <= 0 {
		t.Errorf("supported[1g.6gb] = %d, want >0", b.supported["1g.6gb"])
	}
}

// TestDiscover_ModeDisabledDeviceIsStillAMigTarget 는 MIG mode 가 꺼진 GPU 도 apply 대상으로
// 잡히는지 검증한다. 실 하드웨어에서 `mig -lgip` 는 mode Disabled 면 profile 을 하나도 내놓지
// 않으므로("No MIG-enabled devices found"), profile 유무만으로 capability 를 판정하면 정확히
// mode enable 이 필요한 GPU 가 target 에서 빠져 Unsupported 로 끝난다(닭-달걀).
// mig.mode.current 는 MIG 지원 GPU 에서만 Disabled 로 보고되고 미지원 GPU 는 N/A 이므로,
// Disabled 자체가 정확한 capability 신호다.
func TestDiscover_ModeDisabledDeviceIsStillAMigTarget(t *testing.T) {
	pci := obsTestPCI
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "generic", PCIeAddress: pci},            // detector 는 model 을 generic 으로만 본다
			{Vendor: "nvidia", Model: "generic", PCIeAddress: "0000:c1:00.0"}, // MIG 미지원(A2) — N/A
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c).WithObservations(map[string]Observation{
		// mode Disabled → lgip 없음(실 하드웨어 동작).
		pci:            {PCI: pci, ModeCurrent: modeDisabled, ModePending: modeDisabled, Geometry: geomDisabled},
		"0000:c1:00.0": {PCI: "0000:c1:00.0", ModeCurrent: modeNA, ModePending: modeNA, Geometry: geomDisabled},
	})

	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.targets) != 1 || b.targets[0].PCI != pci {
		t.Fatalf("targets = %+v, want only the mode-disabled MIG GPU", b.targets)
	}
	if !res.Operations.Apply.Supported {
		t.Fatalf("Apply.Supported = false; mode-disabled GPU can be partitioned after enable")
	}
	// profile 목록은 mode 를 켜기 전에는 관측 불가 — 형식이 맞으면 통과시키고 하드웨어에 맡긴다.
	if err := b.Validate([]partition.Layout{{Profile: "1g.6gb", CountPerDevice: 4}}); err != nil {
		t.Fatalf("Validate must not reject on unobservable profiles: %v", err)
	}
	// 형식 자체가 틀린 요청은 여전히 거절한다.
	if err := b.Validate([]partition.Layout{{Profile: "not-a-profile", CountPerDevice: 4}}); err == nil {
		t.Fatalf("malformed profile must still be rejected")
	}
}

// TestDiscover_WidenedCapabilityStaysFailClosed 는 mode Disabled 를 capability 신호로 인정한
// 완화가 "모르는 장치"까지 끌어들이지 않음을 고정한다: N/A(MIG 미지원), Unknown(관측 실패),
// Disabled+ObsError(부분 파싱 실패) 는 모두 target 이 아니다.
func TestDiscover_WidenedCapabilityStaysFailClosed(t *testing.T) {
	ndr := &v1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
		Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
			{Vendor: "nvidia", Model: "generic", PCIeAddress: "0000:01:00.0"},
			{Vendor: "nvidia", Model: "generic", PCIeAddress: "0000:02:00.0"},
			{Vendor: "nvidia", Model: "generic", PCIeAddress: "0000:03:00.0"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
	b := New(c).WithObservations(map[string]Observation{
		"0000:01:00.0": {PCI: "0000:01:00.0", ModeCurrent: modeNA, ModePending: modeNA},
		"0000:02:00.0": {PCI: "0000:02:00.0", ModeCurrent: modeUnknown, ModePending: modeUnknown, Err: "observe job failed"},
		"0000:03:00.0": {PCI: "0000:03:00.0", ModeCurrent: modeDisabled, ModePending: modeUnknown, Err: "unrecognized mig.mode.pending"},
	})

	res, err := b.Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.targets) != 0 {
		t.Fatalf("targets = %+v, want none (N/A, Unknown, and errored observations are not MIG targets)", b.targets)
	}
	if res.Operations.Apply.Supported {
		t.Fatalf("Apply.Supported must stay false with no trustworthy target")
	}
	// 완화 플래그도 서면 안 된다 — Validate 가 형식만 보고 통과시키는 문이 열린다.
	if b.profilesUnobservable {
		t.Fatalf("profilesUnobservable must not be set by untrustworthy observations")
	}
}

// 관측 실패와 "하드웨어가 못 한다" 는 같은 hardwareSupported:false 를 낸다 — Verification 이
// 둘을 갈라야 한다. 갈라지지 않으면 화면이 관측 실패를 하드웨어 사실로 단언한다.
func TestPartitionCapability_ObservationFailureIsNotHardwareVerdict(t *testing.T) {
	// 관측 에러가 있는 경우: 미지원이라고 단언하지 않는다.
	failed := partitionCapabilityFrom(false, nil, "nvidia-smi unreadable", false)
	if failed.HardwareSupported {
		t.Fatalf("관측 실패에 지원을 단언하면 안 된다")
	}
	if failed.Verification != v1alpha1.VerificationRequired {
		t.Errorf("관측 실패는 verification=required 여야 함: got %q", failed.Verification)
	}
	// 관측 성공 + MIG 미지원 하드웨어: 측정된 판정이다.
	measured := partitionCapabilityFrom(false, nil, "", true)
	if measured.Verification != v1alpha1.VerificationVerified {
		t.Errorf("관측 성공은 verification=verified 여야 함: got %q", measured.Verification)
	}
	// review 최종 ①: obsErr 만으로는 부족하다. NDR 필드 경로에서 obsErr == "" 는 "detector 가
	// 에러를 안 적었다" 일 뿐 "detector 가 봤다" 가 아니다 — 아무도 안 본 장치에 verified 를 찍으면
	// 이 축이 막으려고 존재하는 바로 그 단언을 이 축 자신이 하는 셈이다.
	unobserved := partitionCapabilityFrom(false, nil, "", false)
	if unobserved.Verification != v1alpha1.VerificationRequired {
		t.Errorf("★ 관측하지 않은 장치는 에러가 없어도 verified 가 아니다: got %q", unobserved.Verification)
	}
	// supported=true 쪽도 같은 규칙이어야 한다. 빈 문자열로 두면 소비자가 "미기록(구버전)" 과
	// "이번에 관측함" 을 구분할 수 없고, obsErr 가 붙은 부분 실패 관측이 검증된 지원으로 보인다.
	okObs := partitionCapabilityFrom(true, []MigProfile{{Name: "1g.6gb", MaxInstances: 4}}, "", true)
	if !okObs.HardwareSupported || okObs.Verification != v1alpha1.VerificationVerified {
		t.Errorf("관측 성공 + MIG 지원은 verification=verified 여야 함: %+v", okObs)
	}
	partial := partitionCapabilityFrom(true, []MigProfile{{Name: "1g.6gb", MaxInstances: 4}}, "pending parse failed", false)
	if partial.Verification != v1alpha1.VerificationRequired {
		t.Errorf("obsErr 가 붙은 관측은 지원이라도 verified 를 주장하면 안 된다: %+v", partial)
	}
}

// review 최종 ①(fail-open): MIG 관측 Job 이 실패하면 runTarget 은 "관측 없이 돈 첫 Discover" 의
// devices 를 그대로 status 에 영속한다(WaitingForDrain 조기 반환). 그 순간 NDR 의 MIG 필드는
// 비어 있거나 Unknown 인데, 이전 규칙(`ModeCurrent != modeEnabled`)은 그것을 "Enabled 아님 →
// MPS 가능" 으로 읽었고 partitionCapabilityFrom 은 같은 행에 verification: verified 를 찍었다.
// 즉 아무것도 측정하지 못한 바로 그 순간의 CR·REST·웹 콘솔이 "MPS 가능, 측정됨" 을 말했다.
// 이 클러스터에서 아직 안 터지는 이유는 detector 가 A30 을 model="generic" 으로 보고해
// migCap=false 로 떨어지기 때문뿐이다 — detector 가 실 모델명을 보고하는 순간(명시적 목표) 열린다.
func TestNvidiaDiscover_UnobservedMIGCapableDeviceClaimsNothing(t *testing.T) {
	for _, mc := range []string{"", modeUnknown} {
		t.Run("migModeCurrent="+mc, func(t *testing.T) {
			ndr := &v1alpha1.NodeDeviceReport{
				ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
				Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{{
					// 실 모델명을 보고하는 detector(migCap=true 경로) + MIG 관측은 전무.
					Vendor: "nvidia", Model: "NVIDIA A30", PCIeAddress: obsTestPCI,
					MigModeCurrent: mc, MigLgipOutput: lgipA30,
				}}},
			}
			c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
			res, err := New(c).Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
			if err != nil {
				t.Fatal(err)
			}
			mp := res.Devices[0].SharingCapability.MultiProcess
			if mp.Supported {
				t.Errorf("★ MIG mode 를 한 번도 못 읽었는데 MPS 지원을 주장했다: %+v", mp)
			}
			if mp.Reason == "" {
				t.Error("판정 사유가 없다 — 사용자가 왜인지 알 수 없다")
			}
			if got := res.Devices[0].PartitionCapability.Verification; got == v1alpha1.VerificationVerified {
				t.Errorf("★ 아무것도 측정하지 않은 행에 verification=verified: %+v", res.Devices[0].PartitionCapability)
			}
		})
	}
}

// review 재리뷰 NEW-1: mode 가 Enabled 로 읽힌 장치는 드라이버가 MIG capability 를 확인해 준
// 것이다(Disabled 가 MIG 지원 GPU 에서만 나온다는 것과 같은 논리 — Enabled 는 더욱 그렇다).
// 그 장치의 lgip 관측만 실패하면 profile 이 0이라 migCap=false 로 떨어지는데, 그때
// verification=verified 를 찍으면 "측정했고 이 하드웨어는 파티션을 못 한다" 를 단언하게 된다.
// 픽스처는 parseObservation 을 구동해 만든다 — 손으로 조립한 조합은 실제 경로가 내지 않는
// 상태일 수 있고, 이 트랙에서 그 함정에 두 번 빠졌다. 아래 입력이 실제로 내는 값은
// {ModeCurrent: Enabled, Err: "", profiles: 0} 이다(lgi 는 파싱되고 lgip 만 깨진 경우).
func TestNvidiaDiscover_EnabledModeWithUnreadableLgipStaysUnverified(t *testing.T) {
	lgi, err := os.ReadFile("testdata/mig_lgi_580.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, lgip := range []string{
		"Unable to determine GPU instance profiles: Not Supported",
		"",
		"Failed to query GPU instance profiles: Insufficient Permissions",
	} {
		t.Run(lgip, func(t *testing.T) {
			pci := obsTestPCI
			obs := parseObservation(pci, "Enabled, Enabled", string(lgi), lgip)
			if obs.ModeCurrent != modeEnabled || obs.Err != "" {
				t.Fatalf("픽스처 전제 불성립 — 실제 경로가 이 상태를 내지 않는다: %+v", obs)
			}
			ndr := &v1alpha1.NodeDeviceReport{
				ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
				Status: v1alpha1.NodeDeviceReportStatus{Devices: []v1alpha1.DeviceEntry{
					{Vendor: "nvidia", Model: "generic", PCIeAddress: pci},
				}},
			}
			c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(ndr).Build()
			res, err := New(c).WithObservations(map[string]Observation{pci: obs}).
				Discover(partition.Target{Ctx: context.Background(), NodeName: "worker1"})
			if err != nil {
				t.Fatal(err)
			}
			pc := res.Devices[0].PartitionCapability
			if pc.Verification == v1alpha1.VerificationVerified {
				t.Errorf("드라이버가 MIG Enabled 라고 답한 장치에 측정된 판정을 단언한다: %+v", pc)
			}
		})
	}
}
