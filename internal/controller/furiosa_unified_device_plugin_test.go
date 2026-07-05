// ============================================================
// furiosa_unified_device_plugin_test.go: Furiosa 통합 device-plugin(A') 단위 테스트
// 상세: ensureFuriosaUnifiedDevicePlugin() 의 단일 DS 렌더(공통 PCI nodeSelector·기본 이미지·
//       RNGD_PARTITION_POLICY env·config 마운트)와 Unified 토글 전환/롤백 시 반대편 DS 정리를
//       fake client 로 검증 (envtest 불필요)
// 생성일: 2026-07-20
// ============================================================

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/partition/rngd"
)

// TestFuriosaUnified_DSCreated는 통합 DS 가 공통 PCI nodeSelector·기본 이미지·정책 env 로 생성되는지 검증한다.
func TestFuriosaUnified_DSCreated(t *testing.T) {
	policy := makePolicy("test-policy", npuv1alpha1.NPUClusterPolicySpec{
		Furiosa: npuv1alpha1.FuriosaSpec{
			Enabled:       true,
			Unified:       true,
			ConfigMapName: "npu-device-plugin",
			Rngd:          npuv1alpha1.RngdSpec{PartitionPolicy: "dual-core"},
		},
	})

	r := newNPUReconciler(policy)
	ctx := context.Background()

	if err := r.ensureFuriosaUnifiedDevicePlugin(ctx, policy); err != nil {
		t.Fatalf("ensureFuriosaUnifiedDevicePlugin 오류: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(ctx, types.NamespacedName{Name: furiosaUnifiedDSName, Namespace: "kube-system"}, &ds); err != nil {
		t.Fatalf("통합 DaemonSet 생성 실패: %v", err)
	}

	sel := ds.Spec.Template.Spec.NodeSelector
	if sel[furiosaFamilyNodeLabel] != labelValueTrue {
		t.Errorf("nodeSelector: got %v, want %s=true", sel, furiosaFamilyNodeLabel)
	}

	c := ds.Spec.Template.Spec.Containers[0]
	if c.Image != furiosaUnifiedImgDefault {
		t.Errorf("Image: got %q, want default %q", c.Image, furiosaUnifiedImgDefault)
	}
	if len(c.Command) != 0 {
		t.Errorf("Command 는 비어야 함(이미지 ENTRYPOINT): got %v", c.Command)
	}
	var policyEnv string
	for _, e := range c.Env {
		if e.Name == testPartitionPolicyEnv {
			policyEnv = e.Value
		}
	}
	if policyEnv != "dual-core" {
		t.Errorf("RNGD_PARTITION_POLICY env: got %q, want dual-core", policyEnv)
	}

	// ConfigMapName 지정 시 /etc/furiosa 마운트가 존재해야 한다(Warboy config).
	var hasConfig bool
	for _, vm := range c.VolumeMounts {
		if vm.MountPath == "/etc/furiosa" {
			hasConfig = true
		}
	}
	if !hasConfig {
		t.Errorf("config 마운트(/etc/furiosa) 누락: %v", c.VolumeMounts)
	}
}

// TestFuriosaUnified_DefaultImage는 UnifiedDevicePluginImage 미지정 시 기본 이미지가 사용되는지 검증한다.
func TestFuriosaUnified_OverrideImage(t *testing.T) {
	const custom = "registry.example.com:5000/kcloud/furiosa-unified-device-plugin:0.2.0"
	policy := makePolicy("test-policy", npuv1alpha1.NPUClusterPolicySpec{
		Furiosa: npuv1alpha1.FuriosaSpec{
			Enabled:                  true,
			Unified:                  true,
			UnifiedDevicePluginImage: custom,
		},
	})
	r := newNPUReconciler(policy)
	ctx := context.Background()
	if err := r.ensureFuriosaUnifiedDevicePlugin(ctx, policy); err != nil {
		t.Fatalf("ensureFuriosaUnifiedDevicePlugin 오류: %v", err)
	}
	var ds appsv1.DaemonSet
	if err := r.Get(ctx, types.NamespacedName{Name: furiosaUnifiedDSName, Namespace: "kube-system"}, &ds); err != nil {
		t.Fatalf("통합 DaemonSet 생성 실패: %v", err)
	}
	if got := ds.Spec.Template.Spec.Containers[0].Image; got != custom {
		t.Errorf("Image override: got %q, want %q", got, custom)
	}
}

// TestFuriosaUnified_PreservesPartitionEnvWhenACPPOwns는 live DS 가 ACPP owner 어노테이션을
// 가질 때 NPUClusterPolicy 가 RNGD_PARTITION_POLICY 를 덮어쓰지 않고 live 값을 보존하는지 검증한다
// (두 writer 조정, Task 9).
func TestFuriosaUnified_PreservesPartitionEnvWhenACPPOwns(t *testing.T) {
	existing := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        furiosaUnifiedDSName,
			Namespace:   "kube-system",
			Annotations: map[string]string{rngd.PartitionOwnerAnnotation: "acpp-uid-1"},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": furiosaUnifiedDSName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": furiosaUnifiedDSName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "furiosa-device-plugin",
						Env:  []corev1.EnvVar{{Name: testPartitionPolicyEnv, Value: "quad-core"}},
					}},
				},
			},
		},
	}

	policy := makePolicy("test-policy", npuv1alpha1.NPUClusterPolicySpec{
		Furiosa: npuv1alpha1.FuriosaSpec{
			Enabled: true,
			Unified: true,
			Rngd:    npuv1alpha1.RngdSpec{PartitionPolicy: "dual-core"},
		},
	})

	r := newNPUReconciler(policy, existing)
	ctx := context.Background()

	if err := r.ensureFuriosaUnifiedDevicePlugin(ctx, policy); err != nil {
		t.Fatalf("ensureFuriosaUnifiedDevicePlugin 오류: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(ctx, types.NamespacedName{Name: furiosaUnifiedDSName, Namespace: "kube-system"}, &ds); err != nil {
		t.Fatalf("통합 DaemonSet 조회 실패: %v", err)
	}

	got := ""
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.Name == testPartitionPolicyEnv {
			got = e.Value
		}
	}
	if got != "quad-core" {
		t.Errorf("RNGD_PARTITION_POLICY: got %q, want preserved %q (ACPP owns)", got, "quad-core")
	}
	if ds.Annotations[rngd.PartitionOwnerAnnotation] != "acpp-uid-1" {
		t.Errorf("owner annotation not carried forward: got %v", ds.Annotations)
	}
}

// TestFuriosaUnified_OverwritesPartitionEnvWithoutOwner는 owner 어노테이션이 없는 기존 DS 는
// 종전대로 NPUClusterPolicy 값으로 덮이는지 검증한다(회귀 방지).
func TestFuriosaUnified_OverwritesPartitionEnvWithoutOwner(t *testing.T) {
	existing := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      furiosaUnifiedDSName,
			Namespace: "kube-system",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": furiosaUnifiedDSName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": furiosaUnifiedDSName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "furiosa-device-plugin",
						Env:  []corev1.EnvVar{{Name: testPartitionPolicyEnv, Value: "quad-core"}},
					}},
				},
			},
		},
	}

	policy := makePolicy("test-policy", npuv1alpha1.NPUClusterPolicySpec{
		Furiosa: npuv1alpha1.FuriosaSpec{
			Enabled: true,
			Unified: true,
			Rngd:    npuv1alpha1.RngdSpec{PartitionPolicy: "dual-core"},
		},
	})

	r := newNPUReconciler(policy, existing)
	ctx := context.Background()

	if err := r.ensureFuriosaUnifiedDevicePlugin(ctx, policy); err != nil {
		t.Fatalf("ensureFuriosaUnifiedDevicePlugin 오류: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(ctx, types.NamespacedName{Name: furiosaUnifiedDSName, Namespace: "kube-system"}, &ds); err != nil {
		t.Fatalf("통합 DaemonSet 조회 실패: %v", err)
	}

	got := ""
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.Name == testPartitionPolicyEnv {
			got = e.Value
		}
	}
	if got != "dual-core" {
		t.Errorf("RNGD_PARTITION_POLICY: got %q, want overwritten %q (no owner)", got, "dual-core")
	}
}

// TestDeleteDaemonSetIfExists는 존재/부재 DS 삭제가 멱등적으로 동작하는지 검증한다.
func TestDeleteDaemonSetIfExists(t *testing.T) {
	existing := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: furiosaLegacyWarboyDSName, Namespace: "kube-system"},
	}
	r := newNPUReconciler(existing)
	ctx := context.Background()

	// 존재하는 DS 삭제 성공
	if err := r.deleteDaemonSetIfExists(ctx, furiosaLegacyWarboyDSName); err != nil {
		t.Fatalf("존재 DS 삭제 오류: %v", err)
	}
	var ds appsv1.DaemonSet
	if err := r.Get(ctx, types.NamespacedName{Name: furiosaLegacyWarboyDSName, Namespace: "kube-system"}, &ds); err == nil {
		t.Errorf("DS 가 삭제되지 않음")
	}

	// 부재 DS 삭제는 NotFound 무시(멱등)
	if err := r.deleteDaemonSetIfExists(ctx, furiosaLegacyRngdDSName); err != nil {
		t.Errorf("부재 DS 삭제는 nil 이어야 함: %v", err)
	}
}
