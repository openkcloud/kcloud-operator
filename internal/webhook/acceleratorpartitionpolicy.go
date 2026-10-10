// ============================================================
// acceleratorpartitionpolicy.go: AcceleratorPartitionPolicy validating webhook
// 상세: 노드에서 관측해야만 아는 판정(프로파일이 그 제품에서 실제로 지원되는지)은 미룬다 —
// nvidia-smi mig -lgip 관측값이 없으면 판단 근거 자체가 없다(internal/partition/nvidia/backend.go
// 의 Validate 참조). 여기서 막는 것은 노드와 무관한 형식 오류, 대상 노드 부재, 제품이 확실히
// 알려진 경우의 명백한 오타뿐이다. 최종 판정은 컨트롤러와 하드웨어가 한다.
// vendorcap 레지스트리(internal/vendorcap)가 그 벤더의 partition/sharing 을 Native/Software 로
// 선언하지 않으면(Unsupported/Unknown) 형식 검사보다 먼저 사유·근거와 함께 거절한다.
// ACPP 경로 자체는 partition backend 가 있는 벤더만 쓸 수 있다 — sharing 만 있고
// partition 은 Unknown/Unsupported 인 벤더(예: tenstorrent)는 AcceleratorDescriptor
// spec.sharing 경로로 보낸다.
// 삭제 중인 오브젝트(deletionTimestamp)는 재검증하지 않는다 — finalizer 제거를 막으면 안 된다.
// 생성일: 2026-08-24 | 수정일: 2026-10-11 (requirePartitionBackendForSharing 주석에 게이트 순서 명시; 머리말 동어반복 수정 및 계획 항목 토큰 제거)
// ============================================================
package webhook

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition"
	"kcloud-operator/internal/vendorcap"
)

// vendorNvidia 는 이 파일에서 반복 비교하는 nvidia 벤더 식별자다.
const vendorNvidia = "nvidia"

// ACPPValidator 는 AcceleratorPartitionPolicy 의 입구 검증이다. 클러스터 상태(대상 노드,
// NodeDeviceReport)를 읽어야 하므로 client 를 받는다 — AWValidator(acceleratorworkload.go)와
// 같은 주입 패턴이다.
type ACPPValidator struct{ Client client.Client }

func (v *ACPPValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, recordRejection("AcceleratorPartitionPolicy", validateACPP(ctx, v.Client, obj))
}

// ValidateUpdate 는 삭제 중인 오브젝트를 재검증하지 않는다. 컨트롤러가 finalizer 를 지우려고
// 부르는 Update(acceleratorpartitionpolicy_controller.go:1488, :1512)도 UPDATE 로 들어오는데,
// 그 사이 레지스트리가 벤더·능력을 재분류했거나 nodeSelector 가 고르던 노드가 사라졌으면
// 검증이 실패해 finalizer 를 못 지우고 기존 ACPP 가 Terminating 에서 못 빠져나온다. 삭제를
// 막는 것은 입구 검증의 일이 아니다.
func (v *ACPPValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	if o, ok := newObj.(client.Object); ok && o.GetDeletionTimestamp() != nil {
		return nil, nil
	}
	return nil, recordRejection("AcceleratorPartitionPolicy", validateACPP(ctx, v.Client, newObj))
}

func (v *ACPPValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateACPP 순서: (1) layout·sharing 둘 다 빈 요청 거부, 이어서 layout 없는 exclusive
// 되돌리기 요청은 되돌릴 능력이 하나도 없는 벤더에서만 거부 (2) layout 이 있으면 vendorcap
// 레지스트리가 그 벤더의 partition 을 Native/Software 로 선언하는지 확인 (3) sharing 이 있으면
// partition backend 가 없는 벤더를 거부한다(ACPP 경로 자체가 partition backend 전용,
// exclusive 포함) (4) sharing 모드가 timeSliced/mps 면 같은 레지스트리로 그 능력을 확인 —
// (2)(3)(4) 모두 아니면(Unsupported/Unknown) 사유 있는 거절이 이겨야 하므로 형식 검사보다
// 먼저 거절한다 (5) partition.ValidateLayoutShape 로 노드 비의존 형식 오류 거부 (6) nodeSelector
// 가 고르는 노드 존재 확인 (7) nvidia 이고 레이아웃이 있으면, 제품이 확실히 아는 경우에만
// (partition.KnownMIGProfiles) 프로파일 오타를 거부한다.
//
// 클러스터 조회(Node/NodeDeviceReport) 실패는 거부 사유가 아니다 — 입구가 클러스터 상태에
// 의존해 정상 요청을 막으면 안 된다. 조회가 안 되면 그 판정만 건너뛰고 통과시킨다.
// nolint:gocyclo
func validateACPP(ctx context.Context, c client.Client, obj runtime.Object) error {
	acpp, ok := obj.(*npuv1alpha1.AcceleratorPartitionPolicy)
	if !ok {
		return fmt.Errorf("expected AcceleratorPartitionPolicy, got %T", obj)
	}
	spec := acpp.Spec

	if len(spec.Layout) == 0 && spec.Sharing == nil {
		return fmt.Errorf("spec.layout and spec.sharing are both empty — policy requests nothing")
	}

	// deletionPolicy=RestoreMode 는 nvidia MIG 삭제 경로(handleNvidiaDeletion)에만 구현돼 있다.
	// 다른 벤더의 handleDeletion 은 정책값과 무관하게 Retain 으로 동작하므로(rngd 는 DS 의
	// RNGD_PARTITION_POLICY 를 그대로 둔다), 지킬 수 없는 약속은 입구에서 거절한다.
	if spec.DeletionPolicy == npuv1alpha1.DeletionPolicyRestoreMode && spec.Vendor != vendorNvidia {
		return fmt.Errorf("spec.deletionPolicy %q: supported only for spec.vendor \"nvidia\" — vendor %q deletion keeps the current partition (Retain)",
			spec.DeletionPolicy, spec.Vendor)
	}

	// layout 없이 sharing.mode=exclusive 만 준 요청은 "되돌리기" 다 — 남아 있는 파티션·공유를
	// 내리고 장치를 통째로 쓰게 한다(pkg/npuctl.BuildPartitionPolicy 의 intent=exclusive 가
	// 만드는 모양이고, 콘솔의 첫 선택지가 이 경로다). 그래서 파티션이든 공유든 되돌릴 능력이
	// 하나라도 선언된 벤더에서는 정당한 관리 조작이다. 셋 다 없는 벤더(오늘 rebellions·
	// tenstorrent)에서만 내릴 것이 없어 요청이 무의미하므로, 그때만 사유와 함께 거절한다.
	if len(spec.Layout) == 0 && spec.EffectiveSharingMode() == npuv1alpha1.SharingModeExclusive {
		p := vendorCapability(ctx, c, spec.Vendor, spec.NodeSelector, vendorcap.CapPartition)
		if !declaredSupported(p) &&
			!declaredSupported(vendorCapability(ctx, c, spec.Vendor, spec.NodeSelector, vendorcap.CapTimeSlicing)) &&
			!declaredSupported(vendorCapability(ctx, c, spec.Vendor, spec.NodeSelector, vendorcap.CapMultiProcess)) {
			return fmt.Errorf("spec.vendor %q: no partition or sharing capability to revert — %s%s",
				spec.Vendor, p.Reason, sourceSuffix(p.Source))
		}
	}

	// 명세서 층 검사: 레지스트리가 그 벤더의 파티션을 Native/Software 로 선언하지 않으면
	// 형식 검사보다 먼저, 사유·근거와 함께 거절한다(spec §5.6). Unknown 도 거절한다. furiosa 는
	// 벤더 아래 제품이 둘(rngd/warboy)이라 파티션 능력이 갈리므로 nodeSelector 가 고르는
	// 노드의 NDR 로 제품을 알아낼 수 있으면 product-aware 로 본다(vendorCapability).
	if len(spec.Layout) > 0 {
		if s := vendorCapability(ctx, c, spec.Vendor, spec.NodeSelector, vendorcap.CapPartition); !declaredSupported(s) {
			return fmt.Errorf("spec.vendor %q: partition %s — %s%s", spec.Vendor, s.State, s.Reason, sourceSuffix(s.Source))
		}
	}

	// ACPP 는 partition backend 가 있는 벤더만 쓸 수 있다. validateACPP 인라인 대신
	// 헬퍼로 뺀 것은 gocyclo 상한(30) 때문이다(acceleratordescriptor_controller.go 의
	// sharingRefusal 과 같은 이유).
	if err := requirePartitionBackendForSharing(ctx, c, spec); err != nil {
		return err
	}

	// 같은 원칙을 sharing 능력 자체에도 적용한다 — layout 유무와 무관하게(sharing-only 정책
	// 포함) 그 벤더가 요청된 공유 모드를 Native/Software 로 선언하지 않으면 사유와 함께
	// 거절한다. 능력표에 없는 모드(exclusive 등)는 여기서 판정하지 않고 기존 sharing 검증에
	// 맡긴다.
	if spec.Sharing != nil {
		var sharingCap vendorcap.Capability
		switch spec.Sharing.Mode {
		case npuv1alpha1.SharingModeTimeSliced:
			sharingCap = vendorcap.CapTimeSlicing
		case npuv1alpha1.SharingModeMPS:
			sharingCap = vendorcap.CapMultiProcess
		}
		if sharingCap != "" {
			if s := vendorCapability(ctx, c, spec.Vendor, spec.NodeSelector, sharingCap); !declaredSupported(s) {
				return fmt.Errorf("spec.vendor %q: sharing %s %s — %s%s", spec.Vendor, spec.Sharing.Mode, s.State, s.Reason, sourceSuffix(s.Source))
			}
		}
	}

	layouts := make([]partition.Layout, len(spec.Layout))
	for i, l := range spec.Layout {
		layouts[i] = partition.Layout{Profile: l.Profile, CountPerDevice: l.CountPerDevice}
	}
	if err := partition.ValidateLayoutShape(spec.Vendor, layouts); err != nil {
		return fmt.Errorf("spec.layout: %w", err)
	}

	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabels(spec.NodeSelector)); err != nil {
		return nil // 클러스터 조회 실패 — 판정 보류
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("spec.nodeSelector %v matches no node", spec.NodeSelector)
	}

	if spec.Vendor != vendorNvidia || len(spec.Layout) == 0 {
		return nil
	}
	requested := spec.Layout[0].Profile
	for _, n := range nodes.Items {
		var ndr npuv1alpha1.NodeDeviceReport
		if err := c.Get(ctx, client.ObjectKey{Name: n.Name}, &ndr); err != nil {
			continue // NodeDeviceReport 조회 실패 — 이 노드는 판정 보류
		}
		for _, d := range ndr.Status.Devices {
			if d.Vendor != vendorNvidia || d.Model == "" {
				continue
			}
			profiles, known := partition.KnownMIGProfiles(d.Model)
			if !known {
				continue // 제품 미확인 — fail-open
			}
			if !slices.Contains(profiles, requested) {
				return fmt.Errorf("spec.layout[0].profile %q is not a MIG profile of product %q on node %s", requested, d.Model, n.Name)
			}
		}
	}
	return nil
}

// vendorCapability 는 product-aware 레지스트리 조회다. furiosa 처럼 벤더 아래 제품이
// 여럿이면(rngd/warboy) partition/sharing 능력이 갈리므로, nodeSelector 가 고르는 노드의
// NodeDeviceReport 로 제품을 알아낼 수 있을 때만 vendorcap.Lookup 을 쓰고, 그 외(단일 제품
// 벤더·제품 불명)엔 기존 LookupVendor 로 그대로 폴백한다 — 단일 제품 벤더는 동작이 그대로다.
// product 판정은 이 capability 기준으로 보수적이다(furiosaProductFromNDR 참고).
func vendorCapability(
	ctx context.Context, c client.Client, vendor string, nodeSelector map[string]string, capability vendorcap.Capability,
) vendorcap.Support {
	if vendor == "furiosa" {
		if product := furiosaProductFromNDR(ctx, c, nodeSelector, capability); product != "" {
			if s, ok := vendorcap.Lookup(vendor, product, capability); ok {
				return s
			}
		}
	}
	return vendorcap.LookupVendor(vendor, capability)
}

// requirePartitionBackendForSharing 은 spec.sharing 이 어느 모드든(exclusive 되돌리기 포함)
// partition 을 Native/Software 로 선언하지 않은 벤더의 ACPP 요청을 sharing 능력 검사보다
// 먼저 거절한다. 그 벤더는 AcceleratorDescriptor spec.sharing → generic device-plugin
// replicas(하드웨어를 나누지 않고 replica 개수만 늘리는 SW 기제)를 대신 써야 한다. 이
// 게이트가 없으면 rebellions 처럼 timeSlicing 은 Software 인데 partition 은 Unsupported 인
// 벤더의 ACPP 요청이 아래 sharing 능력 검사는 통과했다가, partition backend 가 없어
// 컨트롤러의 backendFor 에서 "unknown vendor" 일반 오류로만 죽는다.
func requirePartitionBackendForSharing(
	ctx context.Context, c client.Client, spec npuv1alpha1.AcceleratorPartitionPolicySpec,
) error {
	if spec.Sharing == nil {
		return nil
	}
	if p := vendorCapability(ctx, c, spec.Vendor, spec.NodeSelector, vendorcap.CapPartition); !declaredSupported(p) {
		return fmt.Errorf("spec.vendor %q: sharing %s 는 ACPP 경로(partition backend 필요)에서 지원하지 않는다 — "+
			"AcceleratorDescriptor spec.sharing 을 쓴다", spec.Vendor, spec.Sharing.Mode)
	}
	return nil
}

// furiosaProductFromNDR 는 nodeSelector 가 고르는 노드(빈 selector 면 label 제약이 없어
// 전체 노드가 대상이 된다 — client.MatchingLabels(nil) 과 동일)의 NodeDeviceReport 에서
// furiosa 제품(rngd/warboy)을 찾는다. 조회 실패·제품 미확인은 "" 을 돌려주고 호출자가 벤더
// 단위로 폴백한다 — 판정을 클러스터 조회 성패에 기대면 안 되므로(아래 nvidia MIG 프로파일
// 검사의 fail-open 원칙과 동일) 여기서도 에러를 삼킨다.
//
// ponytail: nodeSelector 가 rngd·warboy 노드를 동시에 고르면(또는 selector 가 비어 전체를
// 고르면) 하나의 product 문자열로만 판정해야 하는 자리라, 매칭된 제품 중 이 capability 가
// declaredSupported 가 아닌 제품이 하나라도 있으면 그 제품을 우선 돌려준다(가장 보수적 —
// 거절돼야 할 노드가 섞여 있는데 다른 노드 값으로 뭉개 통과시키지 않는다). 전부 지원되면
// 첫 번째로 찾은 제품(결정적, 노드/디바이스 순회 순서)을 돌려준다. 노드별로 다른 판정을
// 매기려면(예: 일부 노드만 거절) 이 함수를 노드 단위 판정기로 바꿔야 하는데, ACPP 는
// nodeSelector 전체에 하나의 판정만 내리는 구조라 지금은 여기서 그친다.
//
// 이 조회는 아래 nvidia MIG 프로파일 검사용 NodeList/NDR Get 과 중복이다 — admission
// 요청 1건당 여분의 List+Get 이나, 웹훅은 저QPS 라 지금은 합칠 이유가 없다. 합치려면 그
// nodes/NDR 조회를 함수 앞머리로 끌어올려 공유할 것.
func furiosaProductFromNDR(
	ctx context.Context, c client.Client, nodeSelector map[string]string, capability vendorcap.Capability,
) string {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabels(nodeSelector)); err != nil {
		return ""
	}
	first := ""
	for _, n := range nodes.Items {
		var ndr npuv1alpha1.NodeDeviceReport
		if err := c.Get(ctx, client.ObjectKey{Name: n.Name}, &ndr); err != nil {
			continue
		}
		for _, d := range ndr.Status.Devices {
			if !strings.EqualFold(d.Vendor, "furiosa") {
				continue
			}
			var product string
			switch {
			case strings.EqualFold(d.Model, "rngd"):
				product = "rngd"
			case strings.EqualFold(d.Model, "warboy"):
				product = "warboy"
			default:
				continue
			}
			if first == "" {
				first = product
			}
			if s, ok := vendorcap.Lookup("furiosa", product, capability); ok && !declaredSupported(s) {
				return product // 보수적: 막히는 제품 우선
			}
		}
	}
	return first
}

// declaredSupported 는 레지스트리가 그 능력을 우리가 제공한다고 적었는지다. Unsupported 와
// Unknown 은 둘 다 "지금은 못 준다" 이므로 입구에서 같게 다룬다.
func declaredSupported(s vendorcap.Support) bool {
	return s.State == vendorcap.StateNative || s.State == vendorcap.StateSoftware
}

// sourceSuffix 는 " (source)" 형태의 근거 접미사를 만든다. Unknown 상태(vendorcap.unknown)는
// 근거 문서가 없어 Source 가 비어 있으므로, 그 경우 빈 문자열을 돌려줘 거절 메시지 끝에
// 의미 없는 " ()" 가 남지 않게 한다.
func sourceSuffix(source string) string {
	if source == "" {
		return ""
	}
	return " (" + source + ")"
}
