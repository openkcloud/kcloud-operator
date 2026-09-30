// ============================================================
// driverinstallpolicy_cel_test.go: DriverSpec.kernelImages 의 CEL 검증을 envtest 로 고정한다.
// 상세: kernelImages 는 mode=job 에서만 유효하다. mode=daemonset 로 kernelImages 를 준 DIP 생성은
// API 서버가 거절하고, mode=job 이면 통과해야 한다(CRD x-kubernetes-validations).
// 생성일: 2026-09-22
// ============================================================
package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// dipWithKernelImages 는 kernelImages 를 담은 최소 DIP 다. mode 만 케이스별로 다르다.
func dipWithKernelImages(name, mode string) *npuv1alpha1.DriverInstallPolicy {
	return &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "tenstorrent",
			Driver: npuv1alpha1.DriverSpec{
				Image: "reg/kcloud/tenstorrent-driver-ds:2.11.0-v1",
				Mode:  mode,
				KernelImages: map[string]npuv1alpha1.KernelImageRef{
					"5.15.0-190-generic": "reg/kcloud/tenstorrent-driver-ds:2.11.0-k5.15.0-190-generic-v1",
				},
			},
		},
	}
}

var _ = Describe("DriverInstallPolicy kernelImages CEL 검증", func() {
	It("mode=daemonset 로 kernelImages 를 주면 생성이 거절된다", func() {
		err := k8sClient.Create(ctx, dipWithKernelImages("cel-ki-daemonset", "daemonset"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("driver.kernelImages requires driver.mode=job"))
	})

	It("mode=job 이면 kernelImages 를 담은 DIP 생성이 통과한다", func() {
		dip := dipWithKernelImages("cel-ki-job", "job")
		Expect(k8sClient.Create(ctx, dip)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dip) })
	})
})
