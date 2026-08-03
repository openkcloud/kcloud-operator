// ============================================================
// generated_backend_emit_test.go: 배포물 발행 facade 시험
// 상세: 라이브 재현에 쓰는 경로다. 손으로 쓴 YAML 을 쓰지 않으려면 발행이 코드여야 한다.
// 생성일: 2026-08-10
// ============================================================
package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

func rngdDescriptor() *npuv1alpha1.AcceleratorDescriptor {
	return &npuv1alpha1.AcceleratorDescriptor{
		ObjectMeta: metav1.ObjectMeta{Name: "rngd"},
		Spec: npuv1alpha1.AcceleratorDescriptorSpec{
			Vendor:         "rngd",
			Product:        "rngd",
			AllocationUnit: npuv1alpha1.AllocationUnitDevice,
			Identity: npuv1alpha1.DescriptorIdentity{
				Source:             npuv1alpha1.IdentitySourcePCIAddress,
				StableAcrossReboot: true,
			},
			DeviceNodes: []string{"/dev/rngd/{{id}}*"},
			Backends: []string{
				npuv1alpha1.BackendDevicePlugin, npuv1alpha1.BackendDRA,
			},
		},
	}
}

func TestRenderGeneratedBackendsCoversBothBackends(t *testing.T) {
	objs, err := RenderGeneratedBackends(
		rngdDescriptor(), []string{"npu0"}, "dp:v1", "dra:v1", "kcloud")
	if err != nil {
		t.Fatalf("발행 실패: %v", err)
	}
	kinds := map[string]int{}
	for _, o := range objs {
		kinds[o.GetObjectKind().GroupVersionKind().Kind]++
	}
	// device-plugin: ConfigMap+DaemonSet, DRA: ConfigMap+DaemonSet+DeviceClass+SA+CR+CRB
	for k, want := range map[string]int{
		"ConfigMap": 2, "DaemonSet": 2, "DeviceClass": 1,
		"ServiceAccount": 1, "ClusterRole": 1, "ClusterRoleBinding": 1,
	} {
		if kinds[k] != want {
			t.Errorf("%s 가 %d개여야 한다: %d", k, want, kinds[k])
		}
	}

	// 종류만 세면 두 이미지가 뒤바뀌어도 통과한다. 어느 DaemonSet 에 어느 이미지가
	// 들어갔는지를 직접 본다 - 뒤바뀌면 DRA 드라이버 자리에 device-plugin 이 뜬다.
	images := map[string]string{}
	sas := map[string]string{}
	for _, o := range objs {
		ds, ok := o.(*appsv1.DaemonSet)
		if !ok {
			continue
		}
		images[ds.Labels["app.kubernetes.io/component"]] = ds.Spec.Template.Spec.Containers[0].Image
		sas[ds.Labels["app.kubernetes.io/component"]] = ds.Spec.Template.Spec.ServiceAccountName
	}
	if images["generated-device-plugin"] != "dp:v1" {
		t.Errorf("device-plugin 배포물의 이미지가 다르다: %q", images["generated-device-plugin"])
	}
	if images["generated-dra-driver"] != "dra:v1" {
		t.Errorf("DRA 배포물의 이미지가 다르다: %q", images["generated-dra-driver"])
	}
	// ServiceAccount 가 비면 드라이버는 Running 인데 ResourceSlice 가 하나도 안 나온다.
	if sas["generated-dra-driver"] == "" {
		t.Error("DRA DaemonSet 에 ServiceAccount 가 없다")
	}
}

// 선언하지 않은 backend 의 배포물은 나오지 않는다.
func TestRenderGeneratedBackendsHonorsDeclaredBackends(t *testing.T) {
	d := rngdDescriptor()
	d.Spec.Backends = []string{npuv1alpha1.BackendDevicePlugin}
	objs, err := RenderGeneratedBackends(d, []string{"npu0"}, "dp:v1", "dra:v1", "kcloud")
	if err != nil {
		t.Fatalf("발행 실패: %v", err)
	}
	for _, o := range objs {
		if o.GetObjectKind().GroupVersionKind().Kind == "DeviceClass" {
			t.Fatal("선언하지 않은 DRA backend 의 배포물이 나왔다")
		}
	}
}
