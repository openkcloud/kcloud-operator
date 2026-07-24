// ============================================================
// npuclusterpolicy_advertise_test.go: 광고 주체 선택 스위치(advertiseBy) 동작 검증
// 상세: dra 로 선언한 벤더의 대상 노드에 라벨이 붙고 되돌리면 떨어지는 것, 그 라벨이 있는
//       노드에서 벤더 device-plugin DaemonSet 이 물러나는 것을 단정한다.
// 생성일: 2026-08-06
// ============================================================

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition/nvidia"
)

// advNode 는 벤더 present 라벨을 가진 노드다.
func advNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// advReconciler 는 pod 인덱스·status 서브리소스를 갖춘 fake client 기반 reconciler 다.
// 전환 판정이 pod 조회와 status 기록을 하므로 둘 다 필요하다.
func advReconciler(objs ...client.Object) *NPUClusterPolicyReconciler {
	s := newTestScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = resourcev1.AddToScheme(s)
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

// TestReconcileDRAOwnedLabels_AppliesToVendorNodes 는 selector 미지정이면 그 벤더 장치가 있는
// 모든 노드에 라벨이 붙는 것을 단정한다.
func TestReconcileDRAOwnedLabels_AppliesToVendorNodes(t *testing.T) {
	policy := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "kcloud"},
		Spec: npuv1alpha1.NPUClusterPolicySpec{
			Nvidia: npuv1alpha1.NvidiaSpec{
				Enabled:       true,
				AdvertiseSpec: npuv1alpha1.AdvertiseSpec{AdvertiseBy: npuv1alpha1.AdvertiseByDRA},
			},
		},
	}
	r := advReconciler(policy,
		advNode("gpu-1", map[string]string{"kcloud.ai/nvidia.present": "true"}),
		advNode("gpu-2", map[string]string{"kcloud.ai/nvidia.present": "true"}),
		advNode("npu-1", map[string]string{"kcloud.ai/tenstorrent.present": "true"}),
		advSlice("s1", "gpu-1"), advSlice("s2", "gpu-2"))

	if err := r.reconcileDRAOwnedLabels(context.Background(), policy); err != nil {
		t.Fatalf("reconcileDRAOwnedLabels 오류: %v", err)
	}
	key := npuv1alpha1.DRAOwnedNodeLabel("nvidia")
	for _, n := range []string{"gpu-1", "gpu-2"} {
		if !nodeHasLabel(t, r, n, key) {
			t.Errorf("%s 에 %s 라벨이 없음", n, key)
		}
	}
	if nodeHasLabel(t, r, "npu-1", key) {
		t.Error("다른 벤더 노드에 nvidia 라벨이 붙음")
	}
}

// TestReconcileDRAOwnedLabels_HonorsNodeSelector 는 selector 가 대상 노드를 좁히는 것을 단정한다.
func TestReconcileDRAOwnedLabels_HonorsNodeSelector(t *testing.T) {
	policy := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "kcloud"},
		Spec: npuv1alpha1.NPUClusterPolicySpec{
			Nvidia: npuv1alpha1.NvidiaSpec{
				Enabled: true,
				AdvertiseSpec: npuv1alpha1.AdvertiseSpec{
					AdvertiseBy: npuv1alpha1.AdvertiseByDRA,
					AdvertiseByNodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kcloud.ai/dra-pilot": "true"},
					},
				},
			},
		},
	}
	r := advReconciler(policy,
		advNode("gpu-1", map[string]string{"kcloud.ai/nvidia.present": "true", "kcloud.ai/dra-pilot": "true"}),
		advNode("gpu-2", map[string]string{"kcloud.ai/nvidia.present": "true"}),
		advSlice("s1", "gpu-1"), advSlice("s2", "gpu-2"))

	if err := r.reconcileDRAOwnedLabels(context.Background(), policy); err != nil {
		t.Fatalf("reconcileDRAOwnedLabels 오류: %v", err)
	}
	key := npuv1alpha1.DRAOwnedNodeLabel("nvidia")
	if !nodeHasLabel(t, r, "gpu-1", key) {
		t.Error("selector 에 맞는 노드에 라벨이 없음")
	}
	if nodeHasLabel(t, r, "gpu-2", key) {
		t.Error("selector 밖 노드에 라벨이 붙음")
	}
}

// TestReconcileDRAOwnedLabels_RevokesWhenReverted 는 spec 을 되돌리면 라벨이 떨어지는 것을
// 단정한다. 되돌아오지 않는 스위치는 스위치가 아니다 — 고아 라벨이 남으면 그 노드의
// device-plugin 이 영원히 돌아오지 못한다.
func TestReconcileDRAOwnedLabels_RevokesWhenReverted(t *testing.T) {
	key := npuv1alpha1.DRAOwnedNodeLabel("nvidia")
	policy := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "kcloud"},
		Spec: npuv1alpha1.NPUClusterPolicySpec{
			Nvidia: npuv1alpha1.NvidiaSpec{Enabled: true},
		},
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

// TestNvidiaDevicePluginExcludesDRAOwnedNodes 는 NVIDIA 의 두 DaemonSet(mixed·flat) 모두가
// dra-owned 노드를 피하는 것을 단정한다. 한쪽만 고치면 MIG 노드에서 계속 광고된다.
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

// advPod 는 nvidia.com/gpu 를 쥔 pod 이다.
func advPod(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "busybox",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// advSlice 는 노드가 DRA 로 장치를 내놓고 있다는 사실이다. 드라이버 이름은 판정에 쓰이지
// 않으므로(발행 유무만 본다) 고정한다.
func advSlice(name, node string) *resourcev1.ResourceSlice {
	const driver = "gpu.nvidia.com"
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			NodeName: &node,
			Driver:   driver,
			Pool:     resourcev1.ResourcePool{Name: node, ResourceSliceCount: 1},
			Devices:  []resourcev1.Device{{Name: "gpu-0"}},
		},
	}
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

// TestReconcileDRAOwnedLabels_RefusesWhileDeviceInUse 는 장치를 쥔 pod 이 있는 노드에서는
// 전환을 거절하고 그 사실을 status 에 남기는 것을 단정한다. 조용히 끄면 광고 붕괴를 우리가
// 일부러 만드는 셈이다 — 돌던 pod 은 재시작되는 순간 스케줄되지 못한다.
func TestReconcileDRAOwnedLabels_RefusesWhileDeviceInUse(t *testing.T) {
	policy := draPolicy()
	r := advReconciler(policy,
		advNode("gpu-1", map[string]string{"kcloud.ai/nvidia.present": "true"}),
		advPod("train", "gpu-1"),
		advSlice("slice-1", "gpu-1"))

	if err := r.reconcileDRAOwnedLabels(context.Background(), policy); err != nil {
		t.Fatalf("reconcileDRAOwnedLabels 오류: %v", err)
	}
	if nodeHasLabel(t, r, "gpu-1", npuv1alpha1.DRAOwnedNodeLabel("nvidia")) {
		t.Error("점유 중인데 라벨이 붙음")
	}
	var got npuv1alpha1.NPUClusterPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p", Namespace: "kcloud"}, &got); err != nil {
		t.Fatalf("정책 조회 실패: %v", err)
	}
	if len(got.Status.AdvertiseSwitch) == 0 {
		t.Fatal("status 에 전환 결과가 없음")
	}
	e := got.Status.AdvertiseSwitch[0]
	if e.Applied || e.Node != "gpu-1" || len(e.BlockingPods) == 0 {
		t.Errorf("거절 사유가 노드·파드를 지목하지 않음: %+v", e)
	}
}

// TestReconcileDRAOwnedLabels_RefusesWithoutResourceSlice 는 DRA 가 아직 그 노드에서
// 발행하지 않으면 전환을 거절하는 것을 단정한다. 이걸 놓치면 device-plugin 은 껐는데 DRA 는
// 아직 없는 — 양쪽 광고가 다 0 인 노드가 만들어진다.
func TestReconcileDRAOwnedLabels_RefusesWithoutResourceSlice(t *testing.T) {
	policy := draPolicy()
	r := advReconciler(policy,
		advNode("gpu-1", map[string]string{"kcloud.ai/nvidia.present": "true"}))

	if err := r.reconcileDRAOwnedLabels(context.Background(), policy); err != nil {
		t.Fatalf("reconcileDRAOwnedLabels 오류: %v", err)
	}
	if nodeHasLabel(t, r, "gpu-1", npuv1alpha1.DRAOwnedNodeLabel("nvidia")) {
		t.Error("ResourceSlice 가 없는데 라벨이 붙음")
	}
}

// TestReconcileDRAOwnedLabels_AppliesWhenIdleAndPublished 는 비어 있고 발행도 있으면
// 전환이 실제로 이뤄지는 것을 단정한다(거절만 하는 코드가 되지 않도록).
func TestReconcileDRAOwnedLabels_AppliesWhenIdleAndPublished(t *testing.T) {
	policy := draPolicy()
	r := advReconciler(policy,
		advNode("gpu-1", map[string]string{"kcloud.ai/nvidia.present": "true"}),
		advSlice("slice-1", "gpu-1"))

	if err := r.reconcileDRAOwnedLabels(context.Background(), policy); err != nil {
		t.Fatalf("reconcileDRAOwnedLabels 오류: %v", err)
	}
	if !nodeHasLabel(t, r, "gpu-1", npuv1alpha1.DRAOwnedNodeLabel("nvidia")) {
		t.Error("비어 있고 발행도 있는데 라벨이 붙지 않음")
	}
}
