// control_test.go: UpgradeVendor/SetVendorEnabled 단위 테스트(fake client)
// 상세: 가드레일에 따라 제어 경로는 라이브 클러스터에서 실행하지 않고 fake client로만 검증한다.
// 생성일: 2026-07-16
package npuctl

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

func TestUpgradeVendor_PatchesVersionAndUpgradePolicy(t *testing.T) {
	dip := &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-gpu-ds"},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "nvidia",
			Model:  "generic",
			Driver: npuv1alpha1.DriverSpec{Version: "580.159.03"},
		},
	}
	ncp := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "npuclusterpolicy-sample", Namespace: "kcloud"},
	}
	c := newTestClient(t, dip, ncp)

	result, err := c.UpgradeVendor(context.Background(), "nvidia", "595.58.03", UpgradeOptions{Auto: true, Force: true})
	if err != nil {
		t.Fatalf("UpgradeVendor: %v", err)
	}
	if result.NoChange {
		t.Fatalf("버전이 바뀌었으므로 NoChange=false 여야 함: %+v", result)
	}
	if result.PreviousVersion != "580.159.03" || result.NewVersion != "595.58.03" {
		t.Errorf("버전 필드 불일치: %+v", result)
	}
	if !result.ForceAnnotationSet {
		t.Errorf("force=true 이면 annotation이 설정되어야 함: %+v", result)
	}

	var got npuv1alpha1.DriverInstallPolicy
	if err := c.c.Get(context.Background(), client.ObjectKey{Name: dip.Name}, &got); err != nil {
		t.Fatalf("patch 후 DIP Get 실패: %v", err)
	}
	if got.Spec.Driver.Version != "595.58.03" {
		t.Errorf("DIP.spec.driver.version이 patch 되지 않음: got=%s", got.Spec.Driver.Version)
	}
	if got.Spec.UpgradePolicy == nil || !got.Spec.UpgradePolicy.AutoUpgrade || !got.Spec.UpgradePolicy.ForceUpgrade {
		t.Errorf("upgradePolicy가 auto/force 옵션대로 patch 되지 않음: %+v", got.Spec.UpgradePolicy)
	}

	var gotNCP npuv1alpha1.NPUClusterPolicy
	ncpKey := client.ObjectKey{Name: ncp.Name, Namespace: ncp.Namespace}
	if err := c.c.Get(context.Background(), ncpKey, &gotNCP); err != nil {
		t.Fatalf("patch 후 NCP Get 실패: %v", err)
	}
	if gotNCP.Annotations["npu.ai/force-upgrade"] != "true" {
		t.Errorf("NCP force-upgrade annotation이 설정되지 않음: %+v", gotNCP.Annotations)
	}
}

func TestUpgradeVendor_NoChangeWhenSameVersion(t *testing.T) {
	dip := &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-gpu-ds"},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "nvidia",
			Driver: npuv1alpha1.DriverSpec{Version: "580.159.03"},
		},
	}
	c := newTestClient(t, dip)

	result, err := c.UpgradeVendor(context.Background(), "nvidia", "580.159.03", UpgradeOptions{})
	if err != nil {
		t.Fatalf("UpgradeVendor: %v", err)
	}
	if !result.NoChange {
		t.Errorf("동일 버전이면 NoChange=true 여야 함: %+v", result)
	}
}

func TestUpgradeVendor_AmbiguousVendorRequiresModel(t *testing.T) {
	warboy := &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "furiosa-warboy-ds"},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "furiosa",
			Model:  "warboy",
			Driver: npuv1alpha1.DriverSpec{Version: "1.9.8-3"},
		},
	}
	// rngd는 vendorKeyForDIP 규칙상 "rngd" 버킷으로 분리되므로 furiosa 조회와 충돌하지 않아야 한다.
	rngd := &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "furiosa-rngd-ds"},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "furiosa",
			Model:  "rngd",
			Driver: npuv1alpha1.DriverSpec{Version: "2026.1.0"},
		},
	}
	c := newTestClient(t, warboy, rngd)

	// furiosa 조회는 warboy 하나만 매칭되어야 함(rngd는 별도 vendor key).
	if _, err := c.UpgradeVendor(context.Background(), "furiosa", "1.9.9-1", UpgradeOptions{}); err != nil {
		t.Errorf("furiosa(=warboy만 매칭)는 성공해야 하는데 실패: %v", err)
	}

	if _, err := c.UpgradeVendor(context.Background(), "does-not-exist", "1.0.0", UpgradeOptions{}); err == nil {
		t.Errorf("매칭 없는 vendor는 에러여야 함")
	} else if !strings.Contains(err.Error(), "매칭되는 DriverInstallPolicy가 없습니다") {
		t.Errorf("에러 메시지가 예상과 다름: %v", err)
	}
}

func TestSetVendorEnabled_TogglesAndReportsNoChange(t *testing.T) {
	ncp := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "npuclusterpolicy-sample", Namespace: "kcloud"},
		Spec: npuv1alpha1.NPUClusterPolicySpec{
			Furiosa: npuv1alpha1.FuriosaSpec{Enabled: true, Rngd: npuv1alpha1.RngdSpec{Enabled: false}},
		},
	}
	c := newTestClient(t, ncp)

	// rngd: false -> true (변경)
	result, err := c.SetVendorEnabled(context.Background(), "rngd", true)
	if err != nil {
		t.Fatalf("SetVendorEnabled(rngd): %v", err)
	}
	if result.NoChange || result.PreviousEnabled || !result.NewEnabled {
		t.Errorf("rngd enable 결과 불일치: %+v", result)
	}

	var got npuv1alpha1.NPUClusterPolicy
	ncpKey := client.ObjectKey{Name: ncp.Name, Namespace: ncp.Namespace}
	if err := c.c.Get(context.Background(), ncpKey, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Spec.Furiosa.Rngd.Enabled {
		t.Errorf("NCP.spec.furiosa.rngd.enabled이 patch 되지 않음")
	}
	if !got.Spec.Furiosa.Enabled {
		t.Errorf("furiosa.enabled은 그대로 유지되어야 함(rngd만 patch)")
	}

	// furiosa: true -> true (변경 없음)
	result2, err := c.SetVendorEnabled(context.Background(), "furiosa", true)
	if err != nil {
		t.Fatalf("SetVendorEnabled(furiosa): %v", err)
	}
	if !result2.NoChange {
		t.Errorf("동일 값이면 NoChange=true 여야 함: %+v", result2)
	}
}

func TestSetVendorEnabled_UnknownVendor(t *testing.T) {
	ncp := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "npuclusterpolicy-sample", Namespace: "kcloud"},
	}
	c := newTestClient(t, ncp)

	if _, err := c.SetVendorEnabled(context.Background(), "unknown-vendor", true); err == nil {
		t.Errorf("알 수 없는 vendor는 에러여야 함")
	}
}

func TestSetVendorEnabled_NoClusterPolicy(t *testing.T) {
	c := newTestClient(t)
	if _, err := c.SetVendorEnabled(context.Background(), "nvidia", true); err == nil {
		t.Errorf("NPUClusterPolicy가 없으면 에러여야 함")
	}
}
