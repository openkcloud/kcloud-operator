// ============================================================
// acceleratordescriptor_apply.go: descriptor 생성물 적용·회수 (D4b/c)
// 상세: backend-source 중재(NPUClusterPolicy 관리 벤더 거절)·발견가능 preflight·
//       SSA apply(ownerRef+라벨)·라벨 기반 능동 prune 을 한곳에 모은다. descriptor 는
//       cluster-scoped 라 namespaced·cluster-scoped 생성물 모두를 소유할 수 있다.
// 생성일: 2026-09-18
// ============================================================

package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// descriptorOwnerLabel 은 이 descriptor 가 만든 생성물을 되짚는 라벨이다. prune 이 이 값으로
// 자기 소유만 골라 지운다 — ownerRef 는 descriptor 삭제 GC 만 처리하고, 살아있는 descriptor 의
// backend 축소는 이 라벨 기반 능동 prune 이 회수한다.
const descriptorOwnerLabel = "npu.ai/generated-by"

// idPlaceholder 는 런타임이 host 를 보고 채우는 장치 ID 자리표시자다.
const idPlaceholder = "{{id}}"

// mergeLabels 는 nil-safe 라벨 병합이다. add 가 base 를 덮는다.
func mergeLabels(base, add map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(add))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}

// descriptorVendorManaged 는 descriptor 의 벤더를 어떤 NPUClusterPolicy 가 관리하는지 본다.
// device-plugin(Enabled, webhook validateNCP 의 5-way + rngd 의 Unified 특례)뿐 아니라
// per-vendor DRA(DRASpec.IsEnabled, dra_driver.go draVendors)도 OR 로 본다 — 충돌은
// 광고 기전과 무관하게 같은 물리 /dev 에서 나므로, 어느 한쪽이라도 켜져 있으면 관리 중이다.
func descriptorVendorManaged(spec npuv1alpha1.AcceleratorDescriptorSpec, policies []npuv1alpha1.NPUClusterPolicy) bool {
	for i := range policies {
		s := policies[i].Spec
		var on bool
		// delegated·external 벤더는 배포는 안 해도 같은 물리 /dev 를 관측하므로 관리 중으로
		// 본다 — descriptor 가 생성하는 광고와 이중 광고가 나는 건 광고 기전과 무관하다.
		// rngd 는 RngdSpec 에 관리 모드가 없어 Furiosa 의 모드를 따른다.
		switch strings.ToLower(spec.Vendor) {
		case vendorNvidia:
			on = s.Nvidia.Enabled || s.Nvidia.DRA.IsEnabled() || s.Nvidia.IsDelegated() || s.Nvidia.IsExternal()
		case vendorFuriosa:
			on = s.Furiosa.Enabled || s.Furiosa.DRA.IsEnabled() || s.Furiosa.IsDelegated() || s.Furiosa.IsExternal()
		case vendorRngd:
			on = s.Furiosa.Rngd.Enabled || (s.Furiosa.Enabled && s.Furiosa.Unified) ||
				s.Furiosa.Rngd.DRA.IsEnabled() || s.Furiosa.IsDelegated() || s.Furiosa.IsExternal()
		case vendorRebellions:
			on = s.Rebellions.Enabled || s.Rebellions.DRA.IsEnabled() || s.Rebellions.IsDelegated() || s.Rebellions.IsExternal()
		case vendorTenstorrent:
			on = s.Tenstorrent.Enabled || s.Tenstorrent.DRA.IsEnabled() || s.Tenstorrent.IsDelegated() || s.Tenstorrent.IsExternal()
		}
		if on {
			return true
		}
	}
	return false
}

// discoverableForAutoApply 는 spec.DeviceNodes 템플릿이 런타임의 문자열 발견(glob 없음)으로
// ID 를 가를 수 있는 형태인지 본다. {{id}} 를 담은 템플릿은 그 자리가 깨끗한 경로 성분이어야
// 한다 — 앞은 '/' 로 끝나고, 뒤는 비었거나 '/' 로 시작. {{id}} 가 하나도 없으면 단일 고정
// 장치라 허용하고, 둘 이상이면 런타임이 ID 를 못 가르므로 거절한다. 위반 시 error 를 낸다.
func discoverableForAutoApply(spec npuv1alpha1.AcceleratorDescriptorSpec) error {
	for _, tmpl := range spec.DeviceNodes {
		idx := strings.Index(tmpl, idPlaceholder)
		if idx < 0 {
			continue
		}
		if strings.Count(tmpl, idPlaceholder) > 1 {
			return fmt.Errorf("deviceNode 템플릿 %q 에 %s 가 둘 이상이다 — "+
				"런타임의 문자열 발견이 ID 를 가르지 못한다", tmpl, idPlaceholder)
		}
		prefix := tmpl[:idx]
		suffix := tmpl[idx+len(idPlaceholder):]
		if !strings.HasSuffix(prefix, "/") || (suffix != "" && !strings.HasPrefix(suffix, "/")) {
			return fmt.Errorf("deviceNode 템플릿 %q 의 %s 가 깨끗한 경로 성분이 아니다 — "+
				"런타임의 문자열 발견이 ID 를 가르지 못한다(앞은 '/' 로 끝나고 뒤는 비거나 '/' 로 시작해야 함)",
				tmpl, idPlaceholder)
		}
	}
	return nil
}

// applyGeneratedBackends 는 렌더된 객체를 SSA 로 적용하고 descriptor 를 ownerRef 로 건다.
// descriptor 가 cluster-scoped 라 namespaced(ConfigMap/DaemonSet/SA)·cluster-scoped
// (ClusterRole/ClusterRoleBinding/DeviceClass) 피소유 모두 SetControllerReference 를 통과한다.
func (r *AcceleratorDescriptorReconciler) applyGeneratedBackends(
	ctx context.Context, desc *npuv1alpha1.AcceleratorDescriptor, objs []client.Object,
) error {
	for _, o := range objs {
		if err := controllerutil.SetControllerReference(desc, o, r.Scheme); err != nil {
			return fmt.Errorf("ownerRef 설정 실패(%s): %w", o.GetName(), err)
		}
		o.SetLabels(mergeLabels(o.GetLabels(), map[string]string{descriptorOwnerLabel: desc.Name}))
		if err := r.Patch(ctx, o, client.Apply, client.FieldOwner("kcloud-operator"), client.ForceOwnership); err != nil {
			return fmt.Errorf("적용 실패(%s/%s): %w",
				o.GetObjectKind().GroupVersionKind().Kind, o.GetName(), err)
		}
	}
	return nil
}

// pruneGeneratedBackends 는 이 descriptor 소유 라벨이 붙었지만 desired 집합에 없는 생성물을
// 지운다(backend 축소·비적격 전이 회수). desired 가 nil 이면 이 descriptor 의 생성물을 전부
// 회수한다(거절·비적격). 종류별 이름집합은 SSA apply 후 TypeMeta 가 비므로 Kind 문자열이
// 아니라 구체 타입으로 가른다.
func (r *AcceleratorDescriptorReconciler) pruneGeneratedBackends(
	ctx context.Context, desc *npuv1alpha1.AcceleratorDescriptor, desired []client.Object,
) error {
	ns := r.GeneratedNamespace
	cm, ds, sa := map[string]bool{}, map[string]bool{}, map[string]bool{}
	cr, crb, dc := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, o := range desired {
		switch o.(type) {
		case *corev1.ConfigMap:
			cm[o.GetName()] = true
		case *appsv1.DaemonSet:
			ds[o.GetName()] = true
		case *corev1.ServiceAccount:
			sa[o.GetName()] = true
		case *rbacv1.ClusterRole:
			cr[o.GetName()] = true
		case *rbacv1.ClusterRoleBinding:
			crb[o.GetName()] = true
		case *resourcev1.DeviceClass:
			dc[o.GetName()] = true
		}
	}
	if err := r.pruneKind(ctx, &corev1.ConfigMapList{}, ns, desc.Name, cm); err != nil {
		return err
	}
	if err := r.pruneKind(ctx, &appsv1.DaemonSetList{}, ns, desc.Name, ds); err != nil {
		return err
	}
	if err := r.pruneKind(ctx, &corev1.ServiceAccountList{}, ns, desc.Name, sa); err != nil {
		return err
	}
	if err := r.pruneKind(ctx, &rbacv1.ClusterRoleList{}, "", desc.Name, cr); err != nil {
		return err
	}
	if err := r.pruneKind(ctx, &rbacv1.ClusterRoleBindingList{}, "", desc.Name, crb); err != nil {
		return err
	}
	// DeviceClass(resource.k8s.io)는 DRA 미지원 클러스터에선 서빙되지 않는다. 그 경우
	// List 가 NoMatchError 를 내는데, prune 은 회수할 것이 없다는 뜻이라 조용히 넘긴다.
	return r.pruneKind(ctx, &resourcev1.DeviceClassList{}, "", desc.Name, dc)
}

// pruneKind 는 한 종류에서 owner 라벨이 붙었지만 want 에 없는 객체를 지운다. 클러스터가 그
// 종류를 아직 내지 않으면(NoMatchError) 회수할 것이 없으므로 무시한다.
func (r *AcceleratorDescriptorReconciler) pruneKind(
	ctx context.Context, list client.ObjectList, ns, owner string, want map[string]bool,
) error {
	opts := []client.ListOption{client.MatchingLabels{descriptorOwnerLabel: owner}}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := r.List(ctx, list, opts...); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("생성물 목록 조회 실패: %w", err)
	}
	return meta.EachListItem(list, func(o runtime.Object) error {
		obj, ok := o.(client.Object)
		if !ok || want[obj.GetName()] {
			return nil
		}
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("생성물 삭제 실패(%s): %w", obj.GetName(), err)
		}
		return nil
	})
}

// draServedByMapper 는 클러스터가 resource.k8s.io/v1 의 DeviceClass 를 서빙하는지 본다.
// DRA 안정 API(resource.k8s.io/v1)는 k8s 1.34 부터 제공된다 — 미서빙 클러스터에 DRA
// backend 를 적용하면 DeviceClass SSA 가 NoMatch 로 실패해 reconcile 이 백오프 루프에
// 빠지고 status 에는 능력 부재 사유가 남지 않는다. 버전을 하드코딩하지 않고 실제 서빙
// 여부를 REST mapper 로 물어 판정한다.
func draServedByMapper(m meta.RESTMapper) bool {
	if m == nil {
		return false
	}
	_, err := m.RESTMapping(schema.GroupKind{Group: "resource.k8s.io", Kind: "DeviceClass"}, "v1")
	return err == nil
}

// clusterServesDRA 는 이 reconciler 의 REST mapper 로 DRA 서빙 여부를 본다.
func (r *AcceleratorDescriptorReconciler) clusterServesDRA() bool {
	return draServedByMapper(r.RESTMapper())
}
