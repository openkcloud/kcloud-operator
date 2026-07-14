// ============================================================
// verifier.go: 검증 실행과 근거 기록 (R&D base v0.1 §8.5/§8.8)
// 상세: 노드·NodeDeviceReport·정책을 읽어 판정 입력을 모으고, Evaluate 결과를
//
//	AcceleratorEvidence 로 upsert 한다. 판정 실패는 에러가 아니라 등급 없는 근거다.
//
// 생성일: 2026-07-31
// ============================================================
package verification

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "kcloud-operator/api/v1alpha1"
)

// AllocationProber 는 "이 자원을 실제로 할당받을 수 있는가" 를 묻는 seam 이다.
// 실 구현은 테스트 Pod 를 띄우는 기존 liveVerifier 이고, 테스트는 fake 를 넣는다.
type AllocationProber interface {
	Probe(ctx context.Context, nodeName, resourceName string) (allocated bool, message string, err error)
}

// Request 는 한 번의 검증 요청이다. 장치 관측값은 호출자가 이미 갖고 있으므로 그대로 받는다 —
// 이 패키지가 관측 Job 을 또 띄우면 같은 일을 두 번 한다.
type Request struct {
	NodeName     string
	Vendor       string
	SourcePolicy string
	Generation   int64
	Expectation  Expectation

	ObservedGeometry  map[string]string
	ObservationErrors map[string]string
	// TargetDevices 는 이 정책이 실제로 건드린 장치다(PCI). 비어 있으면 벤더 장치 전부를 본다.
	// 채워지면 보고서 대조를 그 장치들로 좁힌다 — 같은 노드의 비대상 GPU(예: 파티션하지 않는 A2)를
	// 정책의 기대 geometry 로 판정하면 절대 통과할 수 없는 실패가 난다(라이브 실측, 2026-08-04).
	TargetDevices []string
}

// Verifier 는 검증을 실행하고 근거를 남긴다.
type Verifier struct {
	Client client.Client
	// Prober 가 nil 이면 정책이 프로브를 요구해도 돌리지 않는다(그리고 통과시키지도 않는다).
	Prober AllocationProber
	// Now 는 만료 시각 계산의 시계 seam 이다(nil 이면 time.Now).
	Now func() time.Time
}

func (v *Verifier) now() time.Time {
	if v.Now == nil {
		return time.Now()
	}
	return v.Now()
}

// Load 는 노드의 근거를 읽는다. 없으면 (nil, nil) — 부재는 오류가 아니라 상태다.
func (v *Verifier) Load(ctx context.Context, nodeName string) (*v1alpha1.AcceleratorEvidence, error) {
	var ev v1alpha1.AcceleratorEvidence
	if err := v.Client.Get(ctx, types.NamespacedName{Name: nodeName}, &ev); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &ev, nil
}

// Verify 는 판정 입력을 모아 Evaluate 를 돌리고 결과를 evidence 로 남긴다.
func (v *Verifier) Verify(ctx context.Context, req Request) (*v1alpha1.AcceleratorEvidence, error) {
	var node corev1.Node
	if err := v.Client.Get(ctx, types.NamespacedName{Name: req.NodeName}, &node); err != nil {
		return nil, fmt.Errorf("verify: get node %q: %w", req.NodeName, err)
	}
	ndr, err := v.loadReport(ctx, req.NodeName)
	if err != nil {
		return nil, err
	}
	policy, err := v.resolvePolicy(ctx, node.Labels)
	if err != nil {
		return nil, err
	}

	in := Inputs{
		Expectation:       req.Expectation,
		ObservedGeometry:  req.ObservedGeometry,
		ObservationErrors: req.ObservationErrors,
		ReportGeometry:    reportGeometry(ndr, req.Vendor, req.TargetDevices),
		ReportErrors:      reportErrors(ndr, req.Vendor, req.TargetDevices),
		Allocatable:       allocatableOf(&node),
	}
	if ndr != nil && ndr.Status.Validation != nil {
		passed := ndr.Status.Validation.Passed
		in.ReportValidationPassed = &passed
	}
	if policy.Enabled(v1alpha1.EvidenceCheckAllocationProbe) && v.Prober != nil && req.Expectation.ProbeResource != "" {
		ok, msg, perr := v.Prober.Probe(ctx, req.NodeName, req.Expectation.ProbeResource)
		if perr != nil {
			// 프로브 자체가 실패한 것은 "할당 못 받았다" 와 다르다 — 시도했고 판정 불가로 기록한다.
			in.ProbeAttempted, in.ProbeAllocated, in.ProbeMessage = true, false, perr.Error()
		} else {
			in.ProbeAttempted, in.ProbeAllocated, in.ProbeMessage = true, ok, msg
		}
	}

	res := Evaluate(in, policy)
	return v.upsert(ctx, req, policy, res, &node, ndr)
}

func (v *Verifier) loadReport(ctx context.Context, nodeName string) (*v1alpha1.NodeDeviceReport, error) {
	var ndr v1alpha1.NodeDeviceReport
	if err := v.Client.Get(ctx, types.NamespacedName{Name: nodeName}, &ndr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("verify: get NodeDeviceReport %q: %w", nodeName, err)
	}
	return &ndr, nil
}

func (v *Verifier) resolvePolicy(ctx context.Context, labels map[string]string) (EffectivePolicy, error) {
	var list v1alpha1.AcceleratorVerificationPolicyList
	if err := v.Client.List(ctx, &list); err != nil {
		return EffectivePolicy{}, fmt.Errorf("verify: list AcceleratorVerificationPolicy: %w", err)
	}
	return Resolve(list.Items, labels), nil
}

// upsert 는 노드당 하나인 evidence 를 만들거나 갱신한다.
func (v *Verifier) upsert(ctx context.Context, req Request, p EffectivePolicy, res Result,
	node *corev1.Node, ndr *v1alpha1.NodeDeviceReport) (*v1alpha1.AcceleratorEvidence, error) {
	ev := &v1alpha1.AcceleratorEvidence{ObjectMeta: metav1.ObjectMeta{Name: req.NodeName}}
	err := v.Client.Get(ctx, types.NamespacedName{Name: req.NodeName}, ev)
	switch {
	case apierrors.IsNotFound(err):
		ev.Spec = v1alpha1.AcceleratorEvidenceSpec{NodeName: req.NodeName, Vendor: req.Vendor}
		if cerr := v.Client.Create(ctx, ev); cerr != nil {
			return nil, fmt.Errorf("verify: create evidence %q: %w", req.NodeName, cerr)
		}
	case err != nil:
		return nil, fmt.Errorf("verify: get evidence %q: %w", req.NodeName, err)
	}

	now := v.now()
	expires := metav1.NewTime(now.Add(p.TTL))
	ev.Status = v1alpha1.AcceleratorEvidenceStatus{
		Level:        res.Level,
		Fingerprint:  Compute(node, ndr, req.Vendor, req.Generation),
		Checks:       res.Checks,
		ObservedAt:   metav1.NewTime(now),
		ExpiresAt:    &expires,
		SourcePolicy: req.SourcePolicy,
		Reason:       res.Reason,
	}
	// 광고량은 **통과했을 때만** 남긴다. 실패한 검증의 기대값을 기준선으로 저장하면 drift 감시가
	// 한 번도 성립한 적 없는 숫자와 현재를 비교하게 된다.
	if res.Agreed {
		ev.Status.AdvertisedResources = req.Expectation.Allocatable
	}
	if uerr := v.Client.Status().Update(ctx, ev); uerr != nil {
		return nil, fmt.Errorf("verify: update evidence status %q: %w", req.NodeName, uerr)
	}
	return ev, nil
}

// reportGeometry 는 NodeDeviceReport 에서 벤더 장치의 geometry 를 뽑는다.
func reportGeometry(ndr *v1alpha1.NodeDeviceReport, vendor string, targets []string) map[string]string {
	if ndr == nil {
		return nil
	}
	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		want[t] = true
	}
	out := map[string]string{}
	for i, d := range ndr.Status.Devices {
		if vendor != "" && !strings.EqualFold(d.Vendor, vendor) {
			continue
		}
		// PCI 가 없는 벤더(Furiosa 는 detector 가 PCI 를 채우지 않는다)도 "보고서에 장치가 있다"
		// 는 사실은 남겨야 한다 — PCI 없는 항목을 버리면 보고서가 멀쩡한데도 "보고서 없음" 으로
		// 읽혀 근거가 영영 등급을 못 받는다(라이브 실측, 2026-08-04).
		if len(want) > 0 && !want[d.PCIeAddress] {
			continue
		}
		key := d.PCIeAddress
		if key == "" {
			key = fmt.Sprintf("%s/%s#%d", d.Vendor, d.Model, i)
		}
		out[key] = d.MigCurrentGeometry
	}
	return out
}

// reportErrors 는 NodeDeviceReport 가 남긴 장치별 관측 실패 사유다(PCI → 사유).
func reportErrors(ndr *v1alpha1.NodeDeviceReport, vendor string, targets []string) map[string]string {
	if ndr == nil {
		return nil
	}
	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		want[t] = true
	}
	out := map[string]string{}
	for _, d := range ndr.Status.Devices {
		if vendor != "" && !strings.EqualFold(d.Vendor, vendor) {
			continue
		}
		if len(want) > 0 && !want[d.PCIeAddress] {
			continue
		}
		if d.MigObservationError != "" && d.PCIeAddress != "" {
			out[d.PCIeAddress] = d.MigObservationError
		}
	}
	return out
}

// allocatableOf 는 노드 allocatable 을 int32 맵으로 옮긴다(판정 입력 형식).
// 가속기 자원 수량은 작지만 memory 처럼 바이트 단위로 큰 값이 섞여 들어올 수 있다 — int32 로
// 그냥 캐스팅하면 조용히 음수로 뒤집힌다(drift 감시가 그 값을 비교하면 원인 불명의 오탐이 된다).
// int32 상한을 넘으면 clamp 한다.
func allocatableOf(node *corev1.Node) map[string]int32 {
	out := make(map[string]int32, len(node.Status.Allocatable))
	for k, q := range node.Status.Allocatable {
		v := q.Value()
		if v > math.MaxInt32 {
			v = math.MaxInt32
		}
		out[string(k)] = int32(v)
	}
	return out
}
