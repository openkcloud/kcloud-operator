// ============================================================
// quiesce.go: 장치 변경 전 노드 quiesce 오케스트레이션 (cordon → 배출 → 검증 → 복원)
// 상세: MIG mode enable·파티션 변경처럼 "실행 중 워크로드가 있으면 안 되는" 변경의 공통 선결 절차.
//
//	DaemonSet 소유·static/mirror pod 은 배출 대상이 아니다(device-plugin 자신 포함 + API 서버로
//	애초에 evict 불가능하므로).
//	점유 판정은 device-plugin(resources.limits)과 DRA(ResourceClaim) 두 축을 모두 본다 — 한 축만
//	보면 다른 축으로 뜬 워크로드 위에서 장치를 재구성하게 된다.
//
// 생성일: 2026-07-29 | 수정일: 2026-08-05
// ============================================================
package partition

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Quiescer 는 한 벤더의 장치 리소스를 점유한 워크로드를 노드에서 비우는 오케스트레이터다.
type Quiescer struct {
	Client client.Client
	// ResourcePrefixes 는 접두사 매칭 리소스(예: "nvidia.com/mig-").
	ResourcePrefixes []string
	// ExtraResources 는 정확 일치 리소스(예: "nvidia.com/gpu").
	ExtraResources []string
}

// NvidiaQuiescer 는 nvidia.com/gpu 및 MIG 조각 점유 워크로드를 대상으로 한다.
func NvidiaQuiescer(c client.Client) *Quiescer {
	return &Quiescer{Client: c, ResourcePrefixes: []string{"nvidia.com/mig-"}, ExtraResources: []string{"nvidia.com/gpu"}}
}

// RngdQuiescer 는 furiosa.ai/rngd 점유 워크로드를 대상으로 한다.
func RngdQuiescer(c client.Client) *Quiescer {
	return &Quiescer{Client: c, ExtraResources: []string{"furiosa.ai/rngd"}}
}

// holdsDevice 는 pod 이 대상 리소스를 점유하는지다(containers + initContainers).
//
// DRA 로 뜬 pod 은 resources.limits 가 비어 있다 — 장치를 extended resource 가 아니라
// pod.spec.resourceClaims 로 쥐기 때문이다. limits 만 보면 그런 pod 은 "아무것도 안 쥔 pod" 으로
// 보여 배출 대상에서 빠지고, 그대로 -mig 1 이 나간다.
// claim 을 DeviceClass→driver 로 되짚어 벤더를 가리지는 않는다: 이 함수는 안전 게이트라 과하게
// 잡는 쪽이 옳고(과하면 배출이 한 번 더 될 뿐, 모자라면 장치가 밑에서 재구성된다), ResourceClaim
// 축(claimAllocatedOnNode)도 이미 벤더 무관으로 판정한다.
func (q *Quiescer) holdsDevice(p *corev1.Pod) bool {
	if len(p.Spec.ResourceClaims) > 0 {
		return true
	}
	for _, ctrs := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
		for _, ctr := range ctrs {
			for name := range ctr.Resources.Limits {
				n := string(name)
				for _, e := range q.ExtraResources {
					if n == e {
						return true
					}
				}
				for _, pre := range q.ResourcePrefixes {
					if strings.HasPrefix(n, pre) {
						return true
					}
				}
			}
		}
	}
	return false
}

// mirrorPodAnnotationKey 는 kubelet 이 static pod 미러에 붙이는 annotation 이다
// (k8s.io/kubernetes/pkg/kubelet/types.ConfigMirrorAnnotationKey 와 동일 값 — 의존성 추가 없이 리터럴 사용).
const mirrorPodAnnotationKey = "kubernetes.io/config.mirror"

// isExemptPod 는 배출 대상에서 제외되는 pod 인지다: DaemonSet 소유(device-plugin 자신 등, 삭제해도
// 즉시 재생성) + static/mirror pod(Node 소유이거나 mirror annotation 보유 — API 서버로 evict/delete 불가).
// (I-1) 이 예외를 놓치면 그런 pod 이 장치를 점유할 때 EvictDeviceWorkloads 가 evict 실패로 전체
// 중단되거나 AssertQuiesced 가 영구히 실패한다.
func isExemptPod(p *corev1.Pod) bool {
	if _, mirrored := p.Annotations[mirrorPodAnnotationKey]; mirrored {
		return true
	}
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" || o.Kind == "Node" {
			return true
		}
	}
	return false
}

// Cordon 은 노드를 unschedulable 로 만든다(멱등).
func (q *Quiescer) Cordon(ctx context.Context, node string) error {
	var n corev1.Node
	if err := q.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return err
	}
	if n.Spec.Unschedulable {
		return nil
	}
	base := n.DeepCopy()
	n.Spec.Unschedulable = true
	return q.Client.Patch(ctx, &n, client.MergeFrom(base))
}

// Uncordon 은 Cordon 의 역이다(멱등).
func (q *Quiescer) Uncordon(ctx context.Context, node string) error {
	var n corev1.Node
	if err := q.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return err
	}
	if !n.Spec.Unschedulable {
		return nil
	}
	base := n.DeepCopy()
	n.Spec.Unschedulable = false
	return q.Client.Patch(ctx, &n, client.MergeFrom(base))
}

// EvictDeviceWorkloads 는 장치 점유 pod 을 Eviction API 로 배출하고(PDB 존중), 배출 대상에서
// 제외된(isExemptPod) 장치 점유 pod 수를 참고용으로 돌려준다.
//
// (I-2) remaining 은 정보성 카운트일 뿐 완료·재시도 신호가 아니다 — DaemonSet/static pod 은
// 영구히 배출되지 않으므로 이 값이 0 이 되기를 기다리며 requeue 하면 무한 대기할 수 있다.
// 완료 판정은 오직 AssertQuiesced(같은 예외 규칙 적용)가 nil 을 돌려주는지로만 한다.
func (q *Quiescer) EvictDeviceWorkloads(ctx context.Context, node string) (int, error) {
	var pods corev1.PodList
	if err := q.Client.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node}); err != nil {
		return 0, err
	}
	remaining := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if !q.holdsDevice(p) {
			continue
		}
		if isExemptPod(p) {
			remaining++
			continue
		}
		eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}}
		if err := q.Client.SubResource("eviction").Create(ctx, p, eviction); err != nil {
			if apierrors.IsNotFound(err) {
				continue // 이미 사라짐 — 배출 완료로 취급.
			}
			return remaining, fmt.Errorf("evict pod %s/%s: %w", p.Namespace, p.Name, err)
		}
	}
	return remaining, nil
}

// AssertQuiesced 는 cordon 됨 + 장치 점유 non-exempt(isExemptPod) pod 이 없음 + 이 노드에 할당된
// ResourceClaim 이 없음을 검증한다.
//
// claim 축을 pod 축과 함께 여기서 보는 이유: DRA 는 장치를 Pod 이 아니라 ResourceClaim 이 쥔다.
// Pod 이 사라져도 claim 이 아직 deallocate 되지 않았으면 장치는 여전히 잡혀 있고, 반대로
// 다른 컨트롤러가 만든 claim 은 대응하는 Pod 이 이 노드에 없을 수도 있다.
func (q *Quiescer) AssertQuiesced(ctx context.Context, node string) error {
	var n corev1.Node
	if err := q.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return err
	}
	if !n.Spec.Unschedulable {
		return fmt.Errorf("node %s not cordoned", node)
	}
	var pods corev1.PodList
	if err := q.Client.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node}); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if q.holdsDevice(p) && !isExemptPod(p) {
			return fmt.Errorf("node %s still has device-holding pod %s", node, p.Name)
		}
	}
	return q.assertNoAllocatedResourceClaims(ctx, &n)
}

// assertNoAllocatedResourceClaims 는 이 노드에 할당된(status.allocation 이 이 노드를 가리키는)
// ResourceClaim 이 없는지 검증한다. List 실패는 "없음" 이 아니라 "모른다" 다 — 이 함수는 안전
// 게이트이므로 읽지 못했으면 오류로 올린다. 유일한 예외는 클러스터가 resource.k8s.io 자체를
// 서빙하지 않는 경우다(그 축이 아예 없으므로 부재를 이유로 게이트를 막을 수 없다).
func (q *Quiescer) assertNoAllocatedResourceClaims(ctx context.Context, node *corev1.Node) error {
	var claims resourcev1.ResourceClaimList
	if err := q.Client.List(ctx, &claims); err != nil {
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("list resourceclaims: %w", err)
	}
	for i := range claims.Items {
		c := &claims.Items[i]
		if claimAllocatedOnNode(c, node) {
			return fmt.Errorf("node %s still has an allocated ResourceClaim %s/%s", node.Name, c.Namespace, c.Name)
		}
	}
	return nil
}

// claimAllocatedOnNode 는 claim 이 이 노드에 할당됐는지를 status.allocation.nodeSelector 로 판정한다.
// nodeSelector 가 nil 이면 "어디서든 쓸 수 있다"(network-attached, AllNodes)는 뜻이지 이 노드에
// 박혔다는 뜻이 아니다 — 그대로 매칭하면 network 장치 claim 하나가 클러스터의 모든 노드 quiesce 를
// 영구히 막는다.
func claimAllocatedOnNode(claim *resourcev1.ResourceClaim, node *corev1.Node) bool {
	alloc := claim.Status.Allocation
	if alloc == nil || alloc.NodeSelector == nil {
		return false
	}
	return nodeSelectorMatchesNode(alloc.NodeSelector, node)
}

// nodeSelectorOperators 는 corev1.NodeSelectorOperator → apimachinery selection.Operator 매핑이다.
// 값을 다시 옮기는 표일 뿐 매칭 로직 자체가 아니다 — 실제 In/NotIn/Exists/DoesNotExist/Gt/Lt 평가는
// labels.Requirement 가 이미 하므로 손으로 다시 구현하지 않는다.
var nodeSelectorOperators = map[corev1.NodeSelectorOperator]selection.Operator{
	corev1.NodeSelectorOpIn:           selection.In,
	corev1.NodeSelectorOpNotIn:        selection.NotIn,
	corev1.NodeSelectorOpExists:       selection.Exists,
	corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
	corev1.NodeSelectorOpGt:           selection.GreaterThan,
	corev1.NodeSelectorOpLt:           selection.LessThan,
}

// nodeSelectorMatchesNode 는 corev1.NodeSelector 의 의미론(term 은 OR, term 안 요구사항은 AND)을
// 평가한다. matchFields 는 DRA 가 노드-로컬 장치를 낼 때 쓰는 유일한 키인 metadata.name 을,
// matchExpressions 는 노드 라벨을 대상으로 한다(AllocationResult.NodeSelector 문서).
func nodeSelectorMatchesNode(sel *corev1.NodeSelector, node *corev1.Node) bool {
	fields := labels.Set{"metadata.name": node.Name}
	nodeLabels := labels.Set(node.Labels)
	for _, term := range sel.NodeSelectorTerms {
		if nodeSelectorRequirementsMatch(term.MatchFields, fields) &&
			nodeSelectorRequirementsMatch(term.MatchExpressions, nodeLabels) {
			return true
		}
	}
	return false
}

func nodeSelectorRequirementsMatch(reqs []corev1.NodeSelectorRequirement, set labels.Set) bool {
	for _, req := range reqs {
		op, ok := nodeSelectorOperators[req.Operator]
		if !ok {
			return false
		}
		r, err := labels.NewRequirement(req.Key, op, req.Values)
		if err != nil || !r.Matches(set) {
			return false
		}
	}
	return true
}

// DeviceHoldingPods 는 이 노드에서 장치를 쥐고 있는 pod 들이다(namespace/name). cordon 여부를
// 묻지 않는다 — "지금 비어 있는가" 만 답한다. 점유 판정 규칙은 holdsDevice 한 곳에만 둔다.
func (q *Quiescer) DeviceHoldingPods(ctx context.Context, node string) ([]string, error) {
	var pods corev1.PodList
	if err := q.Client.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node}); err != nil {
		return nil, err
	}
	var out []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if q.holdsDevice(p) && !isExemptPod(p) {
			out = append(out, p.Namespace+"/"+p.Name)
		}
	}
	return out, nil
}
