/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/naming"
	"kcloud-operator/internal/partition/nvidia"
)

var _ = Describe("NPUClusterPolicy Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		npuclusterpolicy := &npuv1alpha1.NPUClusterPolicy{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind NPUClusterPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, npuclusterpolicy)
			if err != nil && errors.IsNotFound(err) {
				resource := &npuv1alpha1.NPUClusterPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: npuv1alpha1.NPUClusterPolicySpec{
						// detector.image 는 ensureDetector 의 필수 필드 — 미지정 시
						// reconcile 이 에러 경로로 진입한다. 벤더(nvidia/furiosa/rebellions)는
						// 기본 Enabled=false 라 reconcile 에서 skip 되어 성공 경로에 도달한다.
						Detector: &npuv1alpha1.DetectorSpec{
							Image: "registry.example.com/npu-op-detector:test",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &npuv1alpha1.NPUClusterPolicy{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance NPUClusterPolicy")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &NPUClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				// Recorder 주입 — 미설정 시 reconcile 의 Eventf 호출에서 nil deref panic.
				Recorder: record.NewFakeRecorder(100),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})

var _ = Describe("ensureNvidiaDevicePlugin renders mixed and flat DaemonSets", func() {
	It("gives the flat DS an affinity that excludes mig-active nodes and no --mig-strategy flag", func() {
		policy := &npuv1alpha1.NPUClusterPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "np-split-test"},
			Spec: npuv1alpha1.NPUClusterPolicySpec{
				Nvidia: npuv1alpha1.NvidiaSpec{DevicePluginImage: "img:v1"},
			},
		}
		r := &NPUClusterPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(r.ensureNvidiaDevicePlugin(ctx, policy)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin", Namespace: "kube-system"}})
			_ = k8sClient.Delete(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin-flat", Namespace: "kube-system"}})
		})

		var mixed appsv1.DaemonSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "nvidia-device-plugin", Namespace: "kube-system"}, &mixed)).To(Succeed())
		Expect(mixed.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--mig-strategy=mixed"))
		Expect(mixed.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue(nvidia.MigActiveNodeLabel, "true"))

		var flat appsv1.DaemonSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "nvidia-device-plugin-flat", Namespace: "kube-system"}, &flat)).To(Succeed())
		Expect(flat.Spec.Template.Spec.Containers[0].Args).NotTo(ContainElement("--mig-strategy=mixed"))
		Expect(flat.Spec.Template.Spec.Affinity).NotTo(BeNil())
		terms := flat.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		found := false
		for _, term := range terms {
			for _, expr := range term.MatchExpressions {
				if expr.Key == nvidia.MigActiveNodeLabel && expr.Operator == corev1.NodeSelectorOpDoesNotExist {
					found = true
				}
			}
		}
		Expect(found).To(BeTrue(), "flat DS must exclude mig-active nodes via DoesNotExist affinity")

		// 둘 다 restartNvidiaDevicePlugin 이 찾는 공통 라벨(kcloud.ai/dp-vendor)을 공유한다(C-1 수정 후).
		Expect(mixed.Spec.Template.Labels).To(HaveKeyWithValue(nvidiaDevicePluginVendorLabel, "nvidia"))
		Expect(flat.Spec.Template.Labels).To(HaveKeyWithValue(nvidiaDevicePluginVendorLabel, "nvidia"))

		// C-1: mixed 의 selector 는 과거(Task 4 이전) 라이브 값 그대로여야 한다 — 바뀌면 기존 클러스터에서
		// immutable 필드라 422 로 영구 거부된다.
		Expect(mixed.Spec.Selector.MatchLabels).To(Equal(map[string]string{"app.kubernetes.io/name": "nvidia-device-plugin"}))

		// I-B: 두 DS 의 selector 는 서로의 template label 과 매치되면 안 된다(pod 을 서로 훔치지 않음).
		mixedSel, err := metav1.LabelSelectorAsSelector(mixed.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		flatSel, err := metav1.LabelSelectorAsSelector(flat.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		Expect(mixedSel.Matches(labels.Set(flat.Spec.Template.Labels))).To(BeFalse(), "mixed selector must not match flat pods")
		Expect(flatSel.Matches(labels.Set(mixed.Spec.Template.Labels))).To(BeFalse(), "flat selector must not match mixed pods")
	})

	// C-1: 기존(Task 4 이전) selector 를 가진 mixed DS 가 라이브에 이미 있는 업그레이드 경로.
	// selector 가 바뀌면 DaemonSet.spec.selector 는 immutable 이라 API 서버가 422 로 영구 거부한다.
	It("succeeds against a pre-existing mixed DS with the old (pre-split) selector", func() {
		oldSelectorLabels := map[string]string{"app.kubernetes.io/name": "nvidia-device-plugin"}
		live := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin", Namespace: "kube-system"},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: oldSelectorLabels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: oldSelectorLabels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nvidia-device-plugin", Image: "old:v0"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, live)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin", Namespace: "kube-system"}})
			_ = k8sClient.Delete(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin-flat", Namespace: "kube-system"}})
		})

		policy := &npuv1alpha1.NPUClusterPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "np-upgrade-test"},
			Spec: npuv1alpha1.NPUClusterPolicySpec{
				Nvidia: npuv1alpha1.NvidiaSpec{DevicePluginImage: "img:v1"},
			},
		}
		r := &NPUClusterPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(r.ensureNvidiaDevicePlugin(ctx, policy)).To(Succeed(), "must not fail with a 422 on an upgrade from the pre-split selector")

		var flat appsv1.DaemonSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "nvidia-device-plugin-flat", Namespace: "kube-system"}, &flat)).To(Succeed(),
			"flat DS must still be created even when mixed already existed")
	})
})

// TestOwnedScanNamespaces: #16 detector 이동 dual-ns 스캔 고정.
// env 미설정 → kube-system 1개(회귀 0). 설정 → kube-system + kcloud 2개(과도기 orphan 방지).
func TestOwnedScanNamespaces(t *testing.T) {
	t.Setenv("OPERATOR_NAMESPACE", "")
	if got := ownedScanNamespaces(); len(got) != 1 || got[0] != naming.KubeSystemNamespace {
		t.Errorf("env 미설정 시 %v, want [kube-system]", got)
	}
	t.Setenv("OPERATOR_NAMESPACE", "kcloud")
	got := ownedScanNamespaces()
	if len(got) != 2 || got[0] != naming.KubeSystemNamespace || got[1] != "kcloud" {
		t.Errorf("env=kcloud 시 %v, want [kube-system kcloud]", got)
	}
}

// TestIsDevicePluginResource: #19 device-plugin 보존 정책 고정.
// 5종 device-plugin=보존(true), operator 관리(driver/toolkit/detector)=삭제 대상(false).
func TestIsDevicePluginResource(t *testing.T) {
	preserve := []string{"nvidia-device-plugin", "furiosa-device-plugin", "furiosa-rngd-device-plugin",
		"kcloud-tt-device-plugin", "rbln-device-plugin", "rbln-device-plugin-config"}
	for _, n := range preserve {
		if !isDevicePluginResource(n) {
			t.Errorf("%q 는 device-plugin(보존 대상)이어야 함", n)
		}
	}
	del := []string{"kcloud-nvidia-driver", "kcloud-nvidia-toolkit", "kcloud-node-manager", "kcloud-furiosa-warboy-driver"}
	for _, n := range del {
		if isDevicePluginResource(n) {
			t.Errorf("%q 는 operator 관리(삭제 대상)여야 함", n)
		}
	}
}

// TestEnsureNvidiaDevicePluginPreservesSharingConfig: ACPP 가 sharing 을 소유(owner 어노테이션)한 DS 는
// NPUClusterPolicy 재렌더에도 config-file arg/volume/mount 배선과 owner 어노테이션이 살아남아야 한다(Task 3, 2-writer 조정).
func TestEnsureNvidiaDevicePluginPreservesSharingConfig(t *testing.T) {
	live := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "nvidia-device-plugin",
			Namespace:   "kube-system",
			Annotations: map[string]string{nvidia.SharingOwnerAnnotation: "acpp-shared"},
		},
		// live 는 실제 reconcile 산출물을 흉내낸다 — ensureNvidiaDevicePlugin 은 항상 이 컨테이너를 만든다.
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "nvidia-device-plugin"}},
		}}},
	}
	nvidia.WireSharing(live, npuv1alpha1.SharingModeTimeSliced, nvidia.SharingConfigMapNameMixed)

	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "nvidia-device-plugin", Args: []string{"--mig-strategy=mixed"}}},
		}}},
	}
	preserveNvidiaSharing(live, desired)

	if desired.Annotations[nvidia.SharingOwnerAnnotation] != "acpp-shared" {
		t.Fatalf("owner annotation lost: %v", desired.Annotations)
	}
	var hasArg, hasVol, hasMount bool
	for _, a := range desired.Spec.Template.Spec.Containers[0].Args {
		if a == "--config-file="+nvidia.SharingConfigPath {
			hasArg = true
		}
	}
	for _, v := range desired.Spec.Template.Spec.Volumes {
		if v.Name == nvidia.SharingVolumeName {
			hasVol = true
		}
	}
	for _, m := range desired.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == nvidia.SharingVolumeName {
			hasMount = true
		}
	}
	if !hasArg || !hasVol || !hasMount {
		t.Fatalf("sharing wiring lost: arg=%v vol=%v mount=%v", hasArg, hasVol, hasMount)
	}
}

// TestEnsureNvidiaDevicePluginUnownedStaysPlain: owner 어노테이션이 없는(ACPP 미개입) DS 는
// sharing 배선을 얻으면 안 된다(회귀 — 기존 순정 device-plugin 동작 불변).
func TestEnsureNvidiaDevicePluginUnownedStaysPlain(t *testing.T) {
	live := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin", Namespace: "kube-system"}}
	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "nvidia-device-plugin", Args: []string{"--mig-strategy=mixed"}}},
		}}},
	}
	preserveNvidiaSharing(live, desired)
	if len(desired.Spec.Template.Spec.Volumes) != 0 {
		t.Fatalf("unowned DS must not gain sharing volume: %+v", desired.Spec.Template.Spec.Volumes)
	}
}

// TestFlatAffinityRenderIsDeterministic: flat DS 의 NodeAffinity 는 map 순회(무작위 순서)로
// 조립되므로 sort.Strings 없이는 매 reconcile 마다 MatchExpressions 순서가 흔들려 DeepEqual 이
// 불일치 → DS 가 매번 롤링 재시작한다(Task 4 I-A). sort.Strings 자체는 이미 있다 — 이 테스트는
// 그걸 회귀로부터 고정한다.
func TestFlatAffinityRenderIsDeterministic(t *testing.T) {
	r := &NPUClusterPolicyReconciler{}
	base := map[string]string{"a.io/x": "1", "b.io/y": "2", "c.io/z": "3", "d.io/w": "4"}
	seen := map[string]struct{}{}
	for i := 0; i < 200; i++ {
		ds := r.buildNvidiaDevicePluginDS(&npuv1alpha1.NPUClusterPolicy{}, nvidia.DevicePluginNameFlat, base, false)
		key := ""
		for _, e := range ds.Spec.Template.Spec.Affinity.NodeAffinity.
			RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions {
			key += e.Key + ","
		}
		seen[key] = struct{}{}
	}
	if len(seen) != 1 {
		t.Fatalf("flat affinity render must be deterministic, got %d orderings", len(seen))
	}
}
