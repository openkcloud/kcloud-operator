// control.go: pkg/npuctl 제어(control) 로직 — 모두 CR patch로 귀결
// 상세: DriverInstallPolicy.spec.driver.version 변경(업그레이드)과
//
//	NPUClusterPolicy.spec.<vendor>.enabled 토글. 두 동작 모두 k8s API patch만 수행하며
//	operator 재조정(reconcile)이 실제 작업을 수행한다 — 외부 상태 이원화 없음
//	(.omc/plans/management-api.md §7 리스크 완화 원칙).
//	⚠️ 라이브 클러스터에서 이 경로를 실행하는 검증은 금지되어 있다(가드레일) — fake client
//	단위 테스트로만 검증한다.
//
// 생성일: 2026-07-16
package npuctl

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// UpgradeOptions는 UpgradeVendor 호출 시 부가 동작을 제어합니다.
type UpgradeOptions struct {
	// Force는 true면 upgradePolicy.forceUpgrade(+autoUpgrade/drainEnabled)를 함께 설정하고,
	// 매칭되는 NPUClusterPolicy에 npu.ai/force-upgrade=true annotation을 남깁니다
	// (기존 kubectl-npu upgrade --force 동작과 동일).
	Force bool
	// Auto는 true면 upgradePolicy.autoUpgrade + drainEnabled를 설정합니다.
	Auto bool
	// Model은 동일 벤더에 DIP가 둘 이상 있을 때(예: furiosa=warboy/rngd) 대상을 특정합니다.
	// 비우면 해당 벤더의 DIP가 정확히 하나일 때만 허용되고, 둘 이상이면 에러로 후보를 안내합니다.
	Model string
}

// UpgradeResult는 UpgradeVendor의 결과 요약입니다.
type UpgradeResult struct {
	PolicyName         string `json:"policyName"`
	Vendor             string `json:"vendor"`
	Model              string `json:"model,omitempty"`
	PreviousVersion    string `json:"previousVersion,omitempty"`
	NewVersion         string `json:"newVersion"`
	NoChange           bool   `json:"noChange"`
	ForceAnnotationSet bool   `json:"forceAnnotationSet,omitempty"`
}

// UpgradeVendor는 vendor(+옵션 model)에 대응하는 단일 DriverInstallPolicy.spec.driver.version을
// version으로 patch합니다. 매칭 DIP가 없거나 둘 이상(모델 미지정)이면 에러를 반환합니다.
// 모든 쓰기는 DIP(및 --force 시 NPUClusterPolicy annotation) patch로 귀결됩니다.
func (c *Client) UpgradeVendor(
	ctx context.Context,
	vendor, version string,
	opts UpgradeOptions,
) (*UpgradeResult, error) {
	if version == "" {
		return nil, fmt.Errorf("version은 비워둘 수 없습니다")
	}

	dip, err := c.resolveDIP(ctx, vendor, opts.Model)
	if err != nil {
		return nil, err
	}

	prevVersion := dip.Spec.Driver.Version
	result := &UpgradeResult{
		PolicyName:      dip.Name,
		Vendor:          dip.Spec.Vendor,
		Model:           dip.Spec.Model,
		PreviousVersion: prevVersion,
		NewVersion:      version,
	}

	if prevVersion == version {
		result.NoChange = true
		return result, nil
	}

	// MergeFrom(obj)는 obj를 deep-copy하지 않고 그대로 참조로 저장하므로(controller-runtime
	// client.mergeFromPatch.from), 반드시 mutate 이전에 DeepCopy()로 스냅샷을 떠야 diff가 발생한다.
	patch := client.MergeFrom(dip.DeepCopy())
	dip.Spec.Driver.Version = version
	if opts.Auto || opts.Force {
		up := dip.Spec.UpgradePolicy
		if up == nil {
			up = &npuv1alpha1.UpgradePolicy{}
		}
		up.AutoUpgrade = true
		up.DrainEnabled = true
		if opts.Force {
			up.ForceUpgrade = true
		}
		dip.Spec.UpgradePolicy = up
	}
	if err := c.c.Patch(ctx, dip, patch); err != nil {
		return nil, fmt.Errorf("DriverInstallPolicy %q patch 실패: %w", dip.Name, err)
	}

	if opts.Force {
		set, err := c.setForceUpgradeAnnotation(ctx)
		if err != nil {
			return result, fmt.Errorf("DIP patch는 성공했으나 force annotation 설정 실패: %w", err)
		}
		result.ForceAnnotationSet = set
	}

	return result, nil
}

// resolveDIP는 vendor(+model)에 대응하는 단일 DriverInstallPolicy를 찾습니다.
// vendorKeyForDIP와 동일 규칙으로 furiosa+model=="rngd"는 vendor="rngd" 조회에 매칭됩니다.
func (c *Client) resolveDIP(ctx context.Context, vendor, model string) (*npuv1alpha1.DriverInstallPolicy, error) {
	var dips npuv1alpha1.DriverInstallPolicyList
	if err := c.c.List(ctx, &dips); err != nil {
		return nil, fmt.Errorf("DriverInstallPolicy 목록 조회 실패: %w", err)
	}

	candidates := make([]npuv1alpha1.DriverInstallPolicy, 0, len(dips.Items))
	for i := range dips.Items {
		dip := dips.Items[i]
		if vendorKeyForDIP(dip.Spec) != vendor {
			continue
		}
		if model != "" && dip.Spec.Model != model {
			continue
		}
		candidates = append(candidates, dip)
	}

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("vendor=%q(model=%q)에 매칭되는 DriverInstallPolicy가 없습니다", vendor, model)
	case 1:
		return &candidates[0], nil
	default:
		names := make([]string, 0, len(candidates))
		for _, d := range candidates {
			names = append(names, fmt.Sprintf("%s(model=%s)", d.Name, d.Spec.Model))
		}
		return nil, fmt.Errorf(
			"vendor=%q에 매칭되는 DriverInstallPolicy가 %d개 있습니다(%v) — --model로 특정하세요",
			vendor, len(candidates), names)
	}
}

// setForceUpgradeAnnotation은 첫 번째 NPUClusterPolicy에 npu.ai/force-upgrade=true를 설정합니다
// (기존 kubectl-npu upgrade --force 동작과 동일: force drain을 위한 신호).
func (c *Client) setForceUpgradeAnnotation(ctx context.Context) (bool, error) {
	var ncps npuv1alpha1.NPUClusterPolicyList
	if err := c.c.List(ctx, &ncps); err != nil {
		return false, fmt.Errorf("NPUClusterPolicy 목록 조회 실패: %w", err)
	}
	if len(ncps.Items) == 0 {
		return false, nil
	}
	ncp := &ncps.Items[0]
	patch := client.MergeFrom(ncp.DeepCopy())
	annot := ncp.GetAnnotations()
	if annot == nil {
		annot = map[string]string{}
	}
	annot["npu.ai/force-upgrade"] = "true"
	ncp.SetAnnotations(annot)
	if err := c.c.Patch(ctx, ncp, patch); err != nil {
		return false, fmt.Errorf("NPUClusterPolicy %q annotation patch 실패: %w", ncp.Name, err)
	}
	return true, nil
}

// ToggleResult는 SetVendorEnabled의 결과 요약입니다.
type ToggleResult struct {
	Vendor          string `json:"vendor"`
	PreviousEnabled bool   `json:"previousEnabled"`
	NewEnabled      bool   `json:"newEnabled"`
	NoChange        bool   `json:"noChange"`
}

// SetVendorEnabled는 (첫 번째) NPUClusterPolicy.spec.<vendor>.enabled를 patch합니다.
// vendor: nvidia | furiosa | rngd(=furiosa.rngd) | rebellions | tenstorrent.
func (c *Client) SetVendorEnabled(ctx context.Context, vendor string, enabled bool) (*ToggleResult, error) {
	var ncps npuv1alpha1.NPUClusterPolicyList
	if err := c.c.List(ctx, &ncps); err != nil {
		return nil, fmt.Errorf("NPUClusterPolicy 목록 조회 실패: %w", err)
	}
	if len(ncps.Items) == 0 {
		return nil, fmt.Errorf("NPUClusterPolicy가 클러스터에 없습니다")
	}
	ncp := &ncps.Items[0]
	patch := client.MergeFrom(ncp.DeepCopy())

	var previous bool
	switch vendor {
	case VendorNvidia:
		previous = ncp.Spec.Nvidia.Enabled
		ncp.Spec.Nvidia.Enabled = enabled
	case VendorFuriosa:
		previous = ncp.Spec.Furiosa.Enabled
		ncp.Spec.Furiosa.Enabled = enabled
	case VendorRngd:
		previous = ncp.Spec.Furiosa.Rngd.Enabled
		ncp.Spec.Furiosa.Rngd.Enabled = enabled
	case VendorRebellions:
		previous = ncp.Spec.Rebellions.Enabled
		ncp.Spec.Rebellions.Enabled = enabled
	case VendorTenstorrent:
		previous = ncp.Spec.Tenstorrent.Enabled
		ncp.Spec.Tenstorrent.Enabled = enabled
	default:
		return nil, fmt.Errorf(
			"알 수 없는 vendor %q (nvidia|furiosa|rngd|rebellions|tenstorrent 중 하나)", vendor)
	}

	result := &ToggleResult{Vendor: vendor, PreviousEnabled: previous, NewEnabled: enabled}
	if previous == enabled {
		result.NoChange = true
		return result, nil
	}
	if err := c.c.Patch(ctx, ncp, patch); err != nil {
		return nil, fmt.Errorf("NPUClusterPolicy %q patch 실패: %w", ncp.Name, err)
	}
	return result, nil
}
