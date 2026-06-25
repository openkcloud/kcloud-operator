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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/naming"
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
