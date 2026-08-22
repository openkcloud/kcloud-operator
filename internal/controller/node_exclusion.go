// ============================================================
// node_exclusion.go: 배제 노드 판정을 kcloud.ai/* 라벨로 발행
// 상세: 검증·CLI·콘솔은 지금 allocatable=0 같은 "결과"만 보고 "의도"(배포 대상 제외)를
//       모른다. nodeExclusion 이 배제 판정의 정본이다 — applyControlPlaneExclusion
//       (npuclusterpolicy_controller.go)은 같은 조건을 렌더 시점에 affinity 로 걸고,
//       이 파일은 같은 조건을 라벨로 알린다. 두 곳이 같은 상수(controlPlaneNodeLabel·
//       masterNodeLabel)를 보므로 조건이 갈라지지 않는다. detector·npuctl 은 이 라벨을
//       읽기만 한다 — 판정 규칙을 복제하지 않는다. 벤더별 excludeNodeSelector(policy 사유)는
//       exclusionConditions 가 펼친 조건 목록을 렌더 경로(applyExcludeNodeSelector)와
//       공유하므로 두 경로가 같은 노드를 뺀다.
// 생성일: 2026-08-12 | 수정일: 2026-08-12
// ============================================================

package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

const (
	// nodeExcludedLabel 은 이 노드가 배포 대상에서 제외됐는지다. 값은 항상 "true" — 배제가
	// 풀리면 라벨 자체를 지운다(상태를 남기지 않는다).
	nodeExcludedLabel = "kcloud.ai/excluded"
	// nodeExcludedReasonLabel 의 값은 고정 어휘: exclusionReasonControlPlane·exclusionReasonPolicy.
	nodeExcludedReasonLabel = "kcloud.ai/excluded-reason"

	// exclusionReasonControlPlane 은 control-plane/master 배제다. 항상 켜져 있고 정책으로
	// 끌 수 없다(applyControlPlaneExclusion 과 동일 조건).
	exclusionReasonControlPlane = "control-plane"
	// exclusionReasonPolicy 는 벤더별 excludeNodeSelector 배제 사유다.
	exclusionReasonPolicy = "policy"
)

// nodeExclusion 은 배제 판정의 정본이다. control-plane 이 policy 보다 강하다 — 둘 다
// 걸리면 control-plane 을 돌려준다(사용자 정책으로 안전 규칙을 못 끄게 한다).
//
// ncp 가 nil 이면 배제가 아니다(control-plane 도 아니다). 배제는 "이 정책의 배포 대상에서
// 뺐다" 는 사실이라 정책이 없으면 성립하지 않는다 — 정책 삭제 경로가 이 판정으로
// 라벨을 회수한다(cleanupOwnedResources). 삭제 후에도 control-plane 사유를 남기면
// 정책이 없는 클러스터에서 CLI·콘솔이 계속 "제외됨" 을 보고한다.
//
// 정책이 아직 만들어지지 않은 경우도 같은 의미론이다: NPUClusterPolicy 가 없으면 이 함수가
// 아예 안 불려 라벨도 안 붙는다(reconcile 자체가 NCP 로 트리거된다). 삭제와 미생성을
// 다르게 다루면 같은 클러스터 상태가 "정책을 만들었다 지웠는가" 에 따라 다르게 보인다.
// 대가를 적어 둔다 — 신규 설치에서 사용자가 NCP 를 만들기 전까지, 가속기 달린
// control-plane 노드는 배제 표시가 없어 detector 검증에 실패로 남는다. 그 창을 없애려면
// 정책 없이도 도는 트리거(별도 컨트롤러나 노드 단독 reconcile)가 필요한데, 배포가 하나도
// 없는 구간을 위해 그 구조를 두지 않았다. 검증 쪽에서 "정책 부재" 를 따로 표현하는 것이
// 더 싼 해결이다.
func nodeExclusion(node *corev1.Node, ncp *npuv1alpha1.NPUClusterPolicy) (bool, string) {
	if ncp == nil {
		return false, ""
	}
	if _, ok := node.Labels[controlPlaneNodeLabel]; ok {
		return true, exclusionReasonControlPlane
	}
	if _, ok := node.Labels[masterNodeLabel]; ok {
		return true, exclusionReasonControlPlane
	}
	if matchesAnyExcludeNodeSelector(node, ncp) {
		return true, exclusionReasonPolicy
	}
	return false, ""
}

// vendorExcludeSelectors 는 벤더별 excludeNodeSelector 전부다. 라벨 경로·경고 로그가 같은
// 목록을 봐야 벤더 하나가 조용히 빠지지 않는다.
func vendorExcludeSelectors(ncp *npuv1alpha1.NPUClusterPolicy) []*metav1.LabelSelector {
	if ncp == nil {
		return nil
	}
	return []*metav1.LabelSelector{
		ncp.Spec.Nvidia.ExcludeNodeSelector,
		ncp.Spec.Furiosa.ExcludeNodeSelector,
		ncp.Spec.Furiosa.Rngd.ExcludeNodeSelector,
		ncp.Spec.Tenstorrent.ExcludeNodeSelector,
		ncp.Spec.Rebellions.ExcludeNodeSelector,
	}
}

// exclusionConditions 는 셀렉터를 조건 목록으로 펼친다. 렌더 경로(applyExcludeNodeSelector)와
// 라벨 경로(matchesAnyExcludeNodeSelector)가 같은 목록을 소비해야 두 경로가 같은 노드를
// 뺀다 — 셀렉터를 통째로 AND 로 평가하면 라벨이 붙는 조건이 렌더보다 좁아진다.
// 배제 의미론은 "조건 하나라도 맞으면 뺀다" 다.
//
// kept 는 두 경로가 쓸 정규화된 조건, dropped 는 버린 조건이다. 버리는 이유: 조건을 그대로
// 렌더하면 nodeAffinity 가 오브젝트 검증에서 거부되고(예: In 인데 values 없음), 그 오류가
// Reconcile 로 올라가 관계없는 벤더와 side operand 까지 멈춘다 — 배제 하나를 못 거는 것보다
// 나쁘다. NPUClusterPolicy 에는 validating webhook 이 없고 CRD 스키마도 metav1.LabelSelector
// 를 그대로 임베드하므로(minItems·enum 없음) 이런 입력이 실제로 저장된다.
//
// 정규화 둘:
//   - matchLabels 는 키를 정렬해 In 조건으로 바꾼다. map 순회 순서가 비결정적이면 렌더된
//     DaemonSet 의 nodeAffinity 순서가 매 reconcile 마다 달라져 불필요한 갱신이 반복된다.
//   - Exists·DoesNotExist 의 Values 는 지운다. 렌더는 Values 를 버리는데(그래서 지금도
//     동작한다) LabelSelectorAsSelector 는 거부해서, 지우지 않으면 두 경로가 갈라진다.
func exclusionConditions(sel *metav1.LabelSelector) (kept, dropped []metav1.LabelSelectorRequirement) {
	if sel == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(sel.MatchLabels))
	for k := range sel.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	all := make([]metav1.LabelSelectorRequirement, 0, len(keys)+len(sel.MatchExpressions))
	for _, k := range keys {
		all = append(all, metav1.LabelSelectorRequirement{
			Key: k, Operator: metav1.LabelSelectorOpIn, Values: []string{sel.MatchLabels[k]},
		})
	}
	for _, e := range sel.MatchExpressions {
		if e.Operator == metav1.LabelSelectorOpExists || e.Operator == metav1.LabelSelectorOpDoesNotExist {
			e.Values = nil
		}
		all = append(all, e)
	}

	for _, c := range all {
		// 유효성은 metav1 에 맡긴다 — 알 수 없는 operator, values 없는 In/NotIn, 잘못된
		// 라벨 키가 한 검사로 걸린다(각각을 따로 열거하면 규칙이 갈라진다).
		if _, err := conditionSelector(c); err != nil {
			dropped = append(dropped, c)
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

// conditionSelector 는 조건 하나를 labels.Selector 로 만든다. 렌더 쪽 NOT 변환과 같은
// 규칙(metav1 의 operator 해석)을 쓰기 위해 조건을 셀렉터 하나로 감싼다.
func conditionSelector(c metav1.LabelSelectorRequirement) (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{c},
	})
}

// matchesAnyExcludeNodeSelector 는 이 노드가 어느 벤더든 excludeNodeSelector 조건 하나에라도
// 걸리는지 본다. 조건 목록은 exclusionConditions 가 만들어 렌더 경로와 공유한다.
func matchesAnyExcludeNodeSelector(node *corev1.Node, ncp *npuv1alpha1.NPUClusterPolicy) bool {
	for _, sel := range vendorExcludeSelectors(ncp) {
		kept, _ := exclusionConditions(sel)
		for _, c := range kept {
			s, err := conditionSelector(c)
			if err != nil {
				continue // exclusionConditions 가 이미 걸렀다.
			}
			if s.Matches(labels.Set(node.Labels)) {
				return true
			}
		}
	}
	return false
}

// logDroppedExclusionConditions 는 버린 excludeNodeSelector 조건을 알린다. 조용히 무시하면
// 사용자는 배제가 걸린 줄 안다. 노드 수와 무관하게 정책당 한 번만 남긴다.
func logDroppedExclusionConditions(ctx context.Context, ncp *npuv1alpha1.NPUClusterPolicy) {
	logger := logf.FromContext(ctx)
	for _, sel := range vendorExcludeSelectors(ncp) {
		_, dropped := exclusionConditions(sel)
		for _, c := range dropped {
			logger.Info("excludeNodeSelector 조건이 유효하지 않아 무시함 — 이 조건으로는 노드가 배제되지 않는다",
				"key", c.Key, "operator", c.Operator, "values", c.Values)
		}
	}
}

// reconcileNodeExclusionLabels 는 nodeExclusion 판정과 노드 라벨을 맞춘다. 부여와 회수가
// 같은 함수에 있어야 고아 라벨이 남지 않는다(reconcileDRAOwnedLabels 와 같은 이유).
func (r *NPUClusterPolicyReconciler) reconcileNodeExclusionLabels(
	ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy) error {
	logger := logf.FromContext(ctx)
	logDroppedExclusionConditions(ctx, policy)

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return fmt.Errorf("노드 목록 조회 실패: %w", err)
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		excluded, reason := nodeExclusion(node, policy)
		// 라벨 존재가 아니라 값을 본다. 존재만 보면 손으로 붙인 kcloud.ai/excluded=false 가
		// 배제 노드에서 "이미 맞다" 로 판정돼(존재==배제) 값이 영구히 false 로 남고,
		// npuctl·detector 는 "true" 만 배제로 인정하므로 화면은 배제가 아니다.
		// 비배제 노드의 목표값은 빈 문자열이라 그 라벨은 그대로 회수된다.
		want := ""
		if excluded {
			want = labelValueTrue
		}
		if node.Labels[nodeExcludedLabel] == want && node.Labels[nodeExcludedReasonLabel] == reason {
			continue
		}

		patch := client.MergeFrom(node.DeepCopy())
		if excluded {
			if node.Labels == nil {
				node.Labels = map[string]string{}
			}
			node.Labels[nodeExcludedLabel] = labelValueTrue
			node.Labels[nodeExcludedReasonLabel] = reason
		} else {
			delete(node.Labels, nodeExcludedLabel)
			delete(node.Labels, nodeExcludedReasonLabel)
		}
		if err := r.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("노드 %s 배제 라벨 갱신 실패: %w", node.Name, err)
		}
		logger.Info("노드 배제 라벨 갱신", "node", node.Name, "excluded", excluded, "reason", reason)
	}

	return nil
}
