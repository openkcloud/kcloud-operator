// ============================================================
// webhook.go: Admission webhook 핸들러 (DIP/NCP validating + Pod mutating)
// 상세: DIP/NCP spec 검증 거부 + opt-in 라벨 Pod 에 NVIDIA runtimeClass 주입 (S4-1)
// 생성일: 2026-07-20 | 수정일: 2026-07-20
// ============================================================

package webhook

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// InjectLabel: 이 라벨이 "true" 인 Pod 만 mutating 대상(opt-in). 그 외 Pod 무개입.
const InjectLabel = "kcloud.ai/inject"

// nvidiaGPUResource: NVIDIA GPU 요청 리소스명.
const nvidiaGPUResource = corev1.ResourceName("nvidia.com/gpu")

// knownVendors: DriverInstallPolicy 에서 허용하는 vendor 집합.
var knownVendors = map[string]bool{
	"nvidia":      true,
	"furiosa":     true,
	"tenstorrent": true,
	"rebellions":  true,
}

// Setup 은 세 개의 admission webhook(DIP/NCP validating, Pod mutating)을 매니저에 등록한다.
// WebhookConfiguration(외부)이 없으면 admission 호출이 서버에 도달하지 않으므로,
// webhook.enabled=false 배포에서는 무해하게 미사용 상태로 남는다.
func Setup(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&npuv1alpha1.DriverInstallPolicy{}).
		WithValidator(&DIPValidator{}).
		Complete(); err != nil {
		return fmt.Errorf("register DriverInstallPolicy validator: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&npuv1alpha1.NPUClusterPolicy{}).
		WithValidator(&NCPValidator{}).
		Complete(); err != nil {
		return fmt.Errorf("register NPUClusterPolicy validator: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(&PodMutator{}).
		Complete(); err != nil {
		return fmt.Errorf("register Pod mutator: %w", err)
	}
	return nil
}

// DIPValidator 는 DriverInstallPolicy 의 cross-field/의미 검증을 수행한다.
// (CRD structural schema 가 잡지 못하는 규칙만: vendor 화이트리스트, duration 파싱,
// trackOnly/mode 조합, 이미지 태그 형식.)
type DIPValidator struct{}

func (v *DIPValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, validateDIP(obj)
}

func (v *DIPValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return nil, validateDIP(newObj)
}

func (v *DIPValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateDIP(obj runtime.Object) error {
	dip, ok := obj.(*npuv1alpha1.DriverInstallPolicy)
	if !ok {
		return fmt.Errorf("expected DriverInstallPolicy, got %T", obj)
	}
	s := dip.Spec

	if !knownVendors[strings.ToLower(s.Vendor)] {
		return fmt.Errorf("spec.vendor %q is not a known vendor (allowed: nvidia, furiosa, tenstorrent, rebellions)", s.Vendor)
	}
	if err := validateImageRef(s.Driver.Image); err != nil {
		return fmt.Errorf("spec.driver.image: %w", err)
	}
	// trackOnly=true 는 설치 경로를 만들지 않으므로 job 모드와만 정합(계획 P3).
	if s.Driver.TrackOnly && s.Driver.Mode != "" && s.Driver.Mode != "job" {
		return fmt.Errorf("spec.driver.trackOnly=true requires spec.driver.mode=job (got %q)", s.Driver.Mode)
	}
	if up := s.UpgradePolicy; up != nil {
		if up.MaxReboots < 0 {
			return fmt.Errorf("spec.upgradePolicy.maxReboots must be >= 0 (got %d)", up.MaxReboots)
		}
		for name, val := range map[string]string{
			"validationTimeout": up.ValidationTimeout,
			"drainTimeout":      up.DrainTimeout,
			"rebootTimeout":     up.RebootTimeout,
		} {
			if val == "" {
				continue
			}
			if _, err := time.ParseDuration(val); err != nil {
				return fmt.Errorf("spec.upgradePolicy.%s %q is not a valid duration: %w", name, val, err)
			}
		}
	}
	return nil
}

// validateImageRef 는 이미지 참조에 명시적 태그가 있는지 검증한다.
// (CRD Pattern 이 1차 방어하나, webhook 은 명확한 거부 메시지를 제공한다.)
func validateImageRef(img string) error {
	img = strings.TrimSpace(img)
	if img == "" {
		return fmt.Errorf("image must not be empty")
	}
	name := img
	if i := strings.LastIndex(img, "/"); i >= 0 {
		name = img[i+1:]
	}
	tagSep := strings.LastIndex(name, ":")
	if tagSep < 0 {
		return fmt.Errorf("image %q must include an explicit tag (name:tag)", img)
	}
	if strings.TrimSpace(name[tagSep+1:]) == "" {
		return fmt.Errorf("image %q has an empty tag", img)
	}
	return nil
}

// NCPValidator 는 NPUClusterPolicy 를 검증한다: enabled vendor 는 devicePluginImage 필수.
type NCPValidator struct{}

func (v *NCPValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, validateNCP(obj)
}

func (v *NCPValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return nil, validateNCP(newObj)
}

func (v *NCPValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateNCP(obj runtime.Object) error {
	ncp, ok := obj.(*npuv1alpha1.NPUClusterPolicy)
	if !ok {
		return fmt.Errorf("expected NPUClusterPolicy, got %T", obj)
	}
	s := ncp.Spec
	checks := []struct {
		name    string
		enabled bool
		img     string
	}{
		{"nvidia", s.Nvidia.Enabled, s.Nvidia.DevicePluginImage},
		{"furiosa", s.Furiosa.Enabled, s.Furiosa.DevicePluginImage},
		{"furiosa.rngd", s.Furiosa.Rngd.Enabled, s.Furiosa.Rngd.DevicePluginImage},
		{"rebellions", s.Rebellions.Enabled, s.Rebellions.DevicePluginImage},
		{"tenstorrent", s.Tenstorrent.Enabled, s.Tenstorrent.DevicePluginImage},
	}
	for _, c := range checks {
		if c.enabled && strings.TrimSpace(c.img) == "" {
			return fmt.Errorf("spec.%s.enabled=true but devicePluginImage is empty", c.name)
		}
	}
	return nil
}

// PodMutator 는 opt-in 라벨(kcloud.ai/inject=true) Pod 가 NVIDIA GPU 를 요청하고
// runtimeClassName 미지정일 때 nvidia runtimeClass 를 주입한다. 비라벨 Pod 는 무개입.
type PodMutator struct{}

func (m *PodMutator) Default(_ context.Context, obj runtime.Object) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected Pod, got %T", obj)
	}
	// opt-in 라벨 없으면 개입하지 않음(objectSelector 이중 방어).
	if pod.Labels[InjectLabel] != "true" {
		return nil
	}
	if podRequestsNvidiaGPU(pod) && (pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName == "") {
		rc := "nvidia"
		pod.Spec.RuntimeClassName = &rc
	}
	return nil
}

func podRequestsNvidiaGPU(pod *corev1.Pod) bool {
	containers := append([]corev1.Container{}, pod.Spec.Containers...)
	containers = append(containers, pod.Spec.InitContainers...)
	for _, c := range containers {
		if _, ok := c.Resources.Limits[nvidiaGPUResource]; ok {
			return true
		}
		if _, ok := c.Resources.Requests[nvidiaGPUResource]; ok {
			return true
		}
	}
	return false
}
