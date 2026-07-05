// ============================================================
// webhook_test.go: admission webhook 핸들러 단위 테스트
// 상세: DIP/NCP 거부·통과, Pod 주입/무개입 케이스
// 생성일: 2026-07-20 | 수정일: 2026-07-30
// ============================================================

package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

func goodDIP() *npuv1alpha1.DriverInstallPolicy {
	return &npuv1alpha1.DriverInstallPolicy{
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "nvidia",
			Driver: npuv1alpha1.DriverSpec{
				Image: "registry.example.com:5000/kcloud/nvidia-driver-ds:580.159.03-v179",
				Mode:  "job",
			},
			UpgradePolicy: &npuv1alpha1.UpgradePolicy{
				MaxReboots:        1,
				ValidationTimeout: "10m",
			},
		},
	}
}

func TestValidateDIP(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*npuv1alpha1.DriverInstallPolicy)
		wantErr bool
	}{
		{"valid", func(*npuv1alpha1.DriverInstallPolicy) {}, false},
		{"unknown vendor", func(d *npuv1alpha1.DriverInstallPolicy) { d.Spec.Vendor = "banana" }, true},
		{"empty image", func(d *npuv1alpha1.DriverInstallPolicy) { d.Spec.Driver.Image = "" }, true},
		{"image no tag", func(d *npuv1alpha1.DriverInstallPolicy) { d.Spec.Driver.Image = "repo/nvidia-driver-ds" }, true},
		{"image empty tag", func(d *npuv1alpha1.DriverInstallPolicy) { d.Spec.Driver.Image = "repo/x:" }, true},
		{"bad duration", func(d *npuv1alpha1.DriverInstallPolicy) { d.Spec.UpgradePolicy.ValidationTimeout = "10x" }, true},
		{"trackOnly non-job", func(d *npuv1alpha1.DriverInstallPolicy) {
			d.Spec.Driver.TrackOnly = true
			d.Spec.Driver.Mode = "daemonset"
		}, true},
		{"trackOnly job ok", func(d *npuv1alpha1.DriverInstallPolicy) {
			d.Spec.Driver.TrackOnly = true
			d.Spec.Driver.Mode = "job"
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := goodDIP()
			c.mutate(d)
			err := validateDIP(d)
			if (err != nil) != c.wantErr {
				t.Fatalf("wantErr=%v got err=%v", c.wantErr, err)
			}
		})
	}
}

func TestValidateNCP(t *testing.T) {
	good := &npuv1alpha1.NPUClusterPolicy{
		Spec: npuv1alpha1.NPUClusterPolicySpec{
			Nvidia: npuv1alpha1.NvidiaSpec{Enabled: true, DevicePluginImage: "repo/dp:v1"},
		},
	}
	if err := validateNCP(good); err != nil {
		t.Fatalf("valid NCP rejected: %v", err)
	}
	bad := good.DeepCopy()
	bad.Spec.Nvidia.DevicePluginImage = ""
	if err := validateNCP(bad); err == nil {
		t.Fatalf("expected rejection for enabled vendor with empty image")
	}
	// disabled vendor with empty image is fine.
	disabled := &npuv1alpha1.NPUClusterPolicy{
		Spec: npuv1alpha1.NPUClusterPolicySpec{
			Nvidia: npuv1alpha1.NvidiaSpec{Enabled: false},
		},
	}
	if err := validateNCP(disabled); err != nil {
		t.Fatalf("disabled vendor should pass: %v", err)
	}
}

func gpuPod(labels map[string]string, gpu bool, rc string) *corev1.Pod {
	return resourcePod(labels, map[string]bool{"nvidia.com/gpu": gpu}, rc)
}

// resourcePod 는 요청 리소스명을 골라 쓸 수 있는 Pod 이다(true 인 이름만 limits 에 담는다).
func resourcePod(labels map[string]string, want map[string]bool, rc string) *corev1.Pod {
	c := corev1.Container{Name: "c"}
	for name, on := range want {
		if !on {
			continue
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Limits[corev1.ResourceName(name)] = resource.MustParse("1")
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{c}},
	}
	if rc != "" {
		p.Spec.RuntimeClassName = &rc
	}
	return p
}

func TestPodMutator(t *testing.T) {
	m := &PodMutator{}
	optIn := map[string]string{InjectLabel: "true"}

	// opt-in + GPU + no runtimeClass → inject nvidia
	p := gpuPod(optIn, true, "")
	if err := m.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != nvidiaRuntimeClass {
		t.Fatalf("expected nvidia runtimeClass injected, got %v", p.Spec.RuntimeClassName)
	}

	// no label → untouched
	p = gpuPod(nil, true, "")
	if err := m.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.RuntimeClassName != nil {
		t.Fatalf("non-labeled pod must not be mutated, got %v", *p.Spec.RuntimeClassName)
	}

	// opt-in but no GPU → untouched
	p = gpuPod(optIn, false, "")
	if err := m.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.RuntimeClassName != nil {
		t.Fatalf("non-GPU pod must not be mutated")
	}

	// opt-in + GPU but explicit runtimeClass → preserved
	p = gpuPod(optIn, true, "custom")
	if err := m.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if *p.Spec.RuntimeClassName != "custom" {
		t.Fatalf("existing runtimeClass must be preserved, got %v", *p.Spec.RuntimeClassName)
	}
}

// mixed MIG 조각(nvidia.com/mig-*)도 GPU 요청이다. 리터럴 nvidia.com/gpu 만 보면 MIG Pod 이
// runtimeClass 없이 떠서 /dev/nvidia* 를 못 받는데도 Ready 가 된다.
func TestPodMutatorInjectsForMIGAndInitContainerResources(t *testing.T) {
	m := &PodMutator{}
	optIn := map[string]string{InjectLabel: "true"}

	p := resourcePod(optIn, map[string]bool{"nvidia.com/mig-1g.6gb": true}, "")
	if err := m.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != nvidiaRuntimeClass {
		t.Fatalf("MIG resource must inject nvidia runtimeClass, got %v", p.Spec.RuntimeClassName)
	}

	// requests 만, 그리고 initContainer 에만 있는 경우도 같은 경로다.
	p = resourcePod(optIn, nil, "")
	p.Spec.InitContainers = []corev1.Container{{Name: "init", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceName("nvidia.com/mig-2g.12gb"): resource.MustParse("1")}}}}
	if err := m.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != nvidiaRuntimeClass {
		t.Fatalf("initContainer request must inject nvidia runtimeClass, got %v", p.Spec.RuntimeClassName)
	}

	// 다른 벤더는 여전히 무개입.
	p = resourcePod(optIn, map[string]bool{"furiosa.ai/rngd": true}, "")
	if err := m.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.RuntimeClassName != nil {
		t.Fatalf("non-nvidia resource must not be mutated, got %v", *p.Spec.RuntimeClassName)
	}
}
