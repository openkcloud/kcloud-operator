// ============================================================
// acceleratorpartitionpolicy.go: AcceleratorPartitionPolicy validating webhook
// 상세: 노드에서 관측해야만 아는 판정(프로파일이 그 제품에서 실제로 지원되는지)은 미룬다 —
// nvidia-smi mig -lgip 관측값이 없으면 판단 근거 자체가 없다(internal/partition/nvidia/backend.go
// 의 Validate 참조). 여기서 막는 것은 노드와 무관한 형식 오류, 대상 노드 부재, 제품이 확실히
// 알려진 경우의 명백한 오타뿐이다. 최종 판정은 컨트롤러와 하드웨어가 한다.
// 생성일: 2026-08-24 | 수정일: 2026-08-24
// ============================================================
package webhook

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition"
)

// ACPPValidator 는 AcceleratorPartitionPolicy 의 입구 검증이다. 클러스터 상태(대상 노드,
// NodeDeviceReport)를 읽어야 하므로 client 를 받는다 — AWValidator(acceleratorworkload.go)와
// 같은 주입 패턴이다.
type ACPPValidator struct{ Client client.Client }

func (v *ACPPValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, validateACPP(ctx, v.Client, obj)
}

func (v *ACPPValidator) ValidateUpdate(ctx context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return nil, validateACPP(ctx, v.Client, newObj)
}

func (v *ACPPValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateACPP 순서: (1) layout·sharing 둘 다 빈 요청 거부 (2) partition.ValidateLayoutShape 로
// 노드 비의존 형식 오류 거부 (3) nodeSelector 가 고르는 노드 존재 확인 (4) nvidia 이고 레이아웃이
// 있으면, 제품이 확실히 아는 경우에만(partition.KnownMIGProfiles) 프로파일 오타를 거부한다.
//
// 클러스터 조회(Node/NodeDeviceReport) 실패는 거부 사유가 아니다 — 입구가 클러스터 상태에
// 의존해 정상 요청을 막으면 안 된다. 조회가 안 되면 그 판정만 건너뛰고 통과시킨다.
func validateACPP(ctx context.Context, c client.Client, obj runtime.Object) error {
	acpp, ok := obj.(*npuv1alpha1.AcceleratorPartitionPolicy)
	if !ok {
		return fmt.Errorf("expected AcceleratorPartitionPolicy, got %T", obj)
	}
	spec := acpp.Spec

	if len(spec.Layout) == 0 && spec.Sharing == nil {
		return fmt.Errorf("spec.layout and spec.sharing are both empty — policy requests nothing")
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

	if spec.Vendor != "nvidia" || len(spec.Layout) == 0 {
		return nil
	}
	requested := spec.Layout[0].Profile
	for _, n := range nodes.Items {
		var ndr npuv1alpha1.NodeDeviceReport
		if err := c.Get(ctx, client.ObjectKey{Name: n.Name}, &ndr); err != nil {
			continue // NodeDeviceReport 조회 실패 — 이 노드는 판정 보류
		}
		for _, d := range ndr.Status.Devices {
			if d.Vendor != "nvidia" || d.Model == "" {
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
