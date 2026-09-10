// ============================================================
// npuclusterpolicy_advertise_test.go: 광고 주체 선택 스위치(advertiseBy) 동작 검증 — 1.28 라인
// 상세: 이 라인에는 resource.k8s.io 가 없어 dra 전환이 항상 거절된다. 축 수용·거절 사유·
//       라벨 회수·DaemonSet 제외까지가 1.34 라인과 동등해야 하는 범위다.
// 생성일: 2026-08-06
// ============================================================

package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition/nvidia"
)

func advNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func advReconciler(objs ...client.Object) *NPUClusterPolicyReconciler {
	s := newTestScheme()
	_ = clientgoscheme.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&npuv1alpha1.NPUClusterPolicy{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).Build()
	return &NPUClusterPolicyReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(64)}
}

func nodeHasLabel(t *testing.T, r *NPUClusterPolicyReconciler, node, key string) bool {
	t.Helper()
	var n corev1.Node
	if err := r.Get(context.Background(), types.NamespacedName{Name: node}, &n); err != nil {
		t.Fatalf("노드 조회 실패: %v", err)
	}
	_, ok := n.Labels[key]
	return ok
}

func draPolicy() *npuv1alpha1.NPUClusterPolicy {
	return &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "kcloud"},
		Spec: npuv1alpha1.NPUClusterPolicySpec{
			Nvidia: npuv1alpha1.NvidiaSpec{
				Enabled:       true,
				AdvertiseSpec: npuv1alpha1.AdvertiseSpec{AdvertiseBy: npuv1alpha1.AdvertiseByDRA},
			},
		},
	}
}

// TestReconcileDRAOwnedLabels_RefusesWithoutDRAAPI 는 이 라인에서 dra 전환이 거절되고 그
// 사유가 status 에 남는 것을 단정한다. DRA 가 없는데 device-plugin 을 끄면 그 노드의 광고가
// 양쪽 다 0 이 된다.
func TestReconcileDRAOwnedLabels_RefusesWithoutDRAAPI(t *testing.T) {
	policy := draPolicy()
	r := advReconciler(policy, advNode("gpu-1", map[string]string{"kcloud.ai/nvidia.present": "true"}))

	if err := r.reconcileDRAOwnedLabels(context.Background(), policy); err != nil {
		t.Fatalf("reconcileDRAOwnedLabels 오류: %v", err)
	}
	if nodeHasLabel(t, r, "gpu-1", npuv1alpha1.DRAOwnedNodeLabel("nvidia")) {
		t.Error("DRA 가 없는 라인에서 라벨이 붙음")
	}
	var got npuv1alpha1.NPUClusterPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p", Namespace: "kcloud"}, &got); err != nil {
		t.Fatalf("정책 조회 실패: %v", err)
	}
	if len(got.Status.AdvertiseSwitch) == 0 {
		t.Fatal("status 에 전환 결과가 없음")
	}
	e := got.Status.AdvertiseSwitch[0]
	if e.Applied || !strings.Contains(e.Reason, "resource.k8s.io") {
		t.Errorf("거절 사유가 DRA 부재를 말하지 않음: %+v", e)
	}
}

// TestReconcileDRAOwnedLabels_RevokesWhenReverted 는 남아 있던 라벨이 회수되는 것을 단정한다.
// 회수는 두 라인에서 똑같이 동작해야 한다 — 고아 라벨이 남으면 device-plugin 이 돌아오지 못한다.
func TestReconcileDRAOwnedLabels_RevokesWhenReverted(t *testing.T) {
	key := npuv1alpha1.DRAOwnedNodeLabel("nvidia")
	policy := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "kcloud"},
		Spec:       npuv1alpha1.NPUClusterPolicySpec{Nvidia: npuv1alpha1.NvidiaSpec{Enabled: true}},
	}
	r := advReconciler(policy,
		advNode("gpu-1", map[string]string{"kcloud.ai/nvidia.present": "true", key: "true"}))

	if err := r.reconcileDRAOwnedLabels(context.Background(), policy); err != nil {
		t.Fatalf("reconcileDRAOwnedLabels 오류: %v", err)
	}
	if nodeHasLabel(t, r, "gpu-1", key) {
		t.Error("advertiseBy 를 되돌렸는데 라벨이 남음")
	}
}

// dsExcludesDRAOwned 는 렌더된 PodSpec 이 해당 벤더의 dra-owned 라벨 부재를 요구하는지 본다.
func dsExcludesDRAOwned(ds *appsv1.DaemonSet, vendor string) bool {
	key := npuv1alpha1.DRAOwnedNodeLabel(vendor)
	na := ds.Spec.Template.Spec.Affinity
	if na == nil || na.NodeAffinity == nil ||
		na.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range na.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			if e.Key == key && e.Operator == corev1.NodeSelectorOpDoesNotExist {
				return true
			}
		}
	}
	return false
}

// TestNvidiaDevicePluginExcludesDRAOwnedNodes 는 NVIDIA 의 두 DaemonSet 모두가 dra-owned
// 노드를 피하는 것을 단정한다. 렌더링 동등성은 DRA 유무와 무관하게 성립해야 한다.
func TestNvidiaDevicePluginExcludesDRAOwnedNodes(t *testing.T) {
	r := &NPUClusterPolicyReconciler{}
	base := map[string]string{"kcloud.ai/nvidia.present": "true"}
	for _, tc := range []struct {
		name  string
		mixed bool
	}{{nvidia.DevicePluginNameMixed, true}, {nvidia.DevicePluginNameFlat, false}} {
		ds := r.buildNvidiaDevicePluginDS(&npuv1alpha1.NPUClusterPolicy{}, tc.name, base, tc.mixed)
		if !dsExcludesDRAOwned(ds, "nvidia") {
			t.Errorf("%s: dra-owned 노드를 제외하지 않음", tc.name)
		}
	}
}
