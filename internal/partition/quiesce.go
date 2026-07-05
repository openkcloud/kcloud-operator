// ============================================================
// quiesce.go: 장치 변경 전 노드 quiesce 오케스트레이션 (cordon → 배출 → 검증 → 복원)
// 상세: MIG mode enable·파티션 변경처럼 "실행 중 워크로드가 있으면 안 되는" 변경의 공통 선결 절차.
//
//	DaemonSet 소유·static/mirror pod 은 배출 대상이 아니다(device-plugin 자신 포함 + API 서버로
//	애초에 evict 불가능하므로).
//
// 생성일: 2026-07-29 | 수정일: 2026-07-29
// ============================================================
package partition

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
func (q *Quiescer) holdsDevice(p *corev1.Pod) bool {
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

// AssertQuiesced 는 cordon 됨 + 장치 점유 non-exempt(isExemptPod) pod 이 없음을 검증한다.
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
	return nil
}
