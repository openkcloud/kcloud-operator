// ============================================================
// dcgm_exporter_test.go: dcgm-exporter DaemonSet 단위 테스트
// 상세: ensureDcgmExporter() 의 생성/토글 off 정리/이미지 override/기본 이미지·
//       nodeSelector·runtimeClass·특권 설정을 fake client 로 검증 (envtest 불필요)
// 생성일: 2026-07-29 | 수정일: 2026-07-29
// ============================================================

package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

// dcgmKey 는 생성되는 DS 의 위치다(kube-system 고정 — 3rd party 이미지 규약).
var dcgmKey = types.NamespacedName{Name: dcgmExporterDSName, Namespace: "kube-system"}

func dcgmPolicy(exp *npuv1alpha1.DcgmExporterSpec) *npuv1alpha1.NPUClusterPolicy {
	return makePolicy("test-policy", npuv1alpha1.NPUClusterPolicySpec{
		Nvidia: npuv1alpha1.NvidiaSpec{
			Enabled:           true,
			DevicePluginImage: "nvcr.io/nvidia/k8s-device-plugin:v0.17.1",
			DcgmExporter:      exp,
		},
	})
}

// enabled=true → DS 생성. 기본 이미지·nodeSelector·runtimeClass·포트·특권을 확인한다.
func TestDcgmExporter_DSCreated(t *testing.T) {
	policy := dcgmPolicy(&npuv1alpha1.DcgmExporterSpec{Enabled: true})
	r := newNPUReconciler(policy)
	ctx := context.Background()

	if err := r.ensureDcgmExporter(ctx, policy); err != nil {
		t.Fatalf("ensureDcgmExporter 오류: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := r.Get(ctx, dcgmKey, &ds); err != nil {
		t.Fatalf("DaemonSet 생성 실패: %v", err)
	}
	c := ds.Spec.Template.Spec.Containers[0]
	if c.Image != dcgmExporterImageDefault {
		t.Errorf("image = %s, want %s", c.Image, dcgmExporterImageDefault)
	}
	if got := ds.Spec.Template.Spec.NodeSelector["kcloud.ai/nvidia.present"]; got != labelValueTrue {
		t.Errorf("nodeSelector = %v, want kcloud.ai/nvidia.present=true", ds.Spec.Template.Spec.NodeSelector)
	}
	if rc := ds.Spec.Template.Spec.RuntimeClassName; rc == nil || *rc != vendorNvidia {
		t.Errorf("runtimeClassName = %v, want %s", rc, vendorNvidia)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != dcgmExporterPort {
		t.Errorf("ports = %v, want %d", c.Ports, dcgmExporterPort)
	}
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("MIG 계측을 위해 privileged=true 여야 한다")
	}
	// pod-resources 소켓 마운트가 있어야 GPU↔Pod 매핑 라벨이 붙는다.
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != hostPodResourcesDir || !c.VolumeMounts[0].ReadOnly {
		t.Errorf("volumeMounts = %v, want RO %s", c.VolumeMounts, hostPodResourcesDir)
	}
	if ds.Spec.Template.Annotations["prometheus.io/port"] != "9400" {
		t.Errorf("scrape annotation missing: %v", ds.Spec.Template.Annotations)
	}
}

// image 지정 시 override 되어야 한다(air-gap 미러 경로).
func TestDcgmExporter_ImageOverride(t *testing.T) {
	const mirror = "registry.example.com:5000/kcloud/nvidia/dcgm-exporter:4.5.2-4.8.1-ubuntu22.04"
	policy := dcgmPolicy(&npuv1alpha1.DcgmExporterSpec{Enabled: true, Image: mirror})
	r := newNPUReconciler(policy)
	ctx := context.Background()

	if err := r.ensureDcgmExporter(ctx, policy); err != nil {
		t.Fatalf("ensureDcgmExporter 오류: %v", err)
	}
	var ds appsv1.DaemonSet
	if err := r.Get(ctx, dcgmKey, &ds); err != nil {
		t.Fatalf("DaemonSet 조회 실패: %v", err)
	}
	if got := ds.Spec.Template.Spec.Containers[0].Image; got != mirror {
		t.Errorf("image = %s, want %s", got, mirror)
	}
}

// 토글 off / spec 미지정 / nvidia 비활성 → 기존 DS 제거(정리 경로).
func TestDcgmExporter_DisabledRemovesDS(t *testing.T) {
	cases := []struct {
		name  string
		mutMe func(*npuv1alpha1.NPUClusterPolicy)
	}{
		{"enabled=false", func(p *npuv1alpha1.NPUClusterPolicy) {
			p.Spec.Nvidia.DcgmExporter = &npuv1alpha1.DcgmExporterSpec{Enabled: false}
		}},
		{"spec nil", func(p *npuv1alpha1.NPUClusterPolicy) {
			p.Spec.Nvidia.DcgmExporter = nil
		}},
		{"nvidia disabled", func(p *npuv1alpha1.NPUClusterPolicy) {
			p.Spec.Nvidia.Enabled = false
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := dcgmPolicy(&npuv1alpha1.DcgmExporterSpec{Enabled: true})
			r := newNPUReconciler(policy)
			ctx := context.Background()

			// 먼저 생성해 두고
			if err := r.ensureDcgmExporter(ctx, policy); err != nil {
				t.Fatalf("최초 생성 실패: %v", err)
			}
			// 토글을 내리면 사라져야 한다
			tc.mutMe(policy)
			if err := r.ensureDcgmExporter(ctx, policy); err != nil {
				t.Fatalf("비활성 처리 실패: %v", err)
			}
			var ds appsv1.DaemonSet
			err := r.Get(ctx, dcgmKey, &ds)
			if err == nil {
				t.Fatal("DS 가 남아있다 — 비활성 시 제거되어야 한다")
			}
			if !apierrors.IsNotFound(err) {
				t.Fatalf("예상치 못한 오류: %v", err)
			}
		})
	}
}

// 비활성 상태에서 DS 가 애초에 없어도 오류 없이 no-op 이어야 한다(멱등).
func TestDcgmExporter_DisabledNoopWhenAbsent(t *testing.T) {
	policy := dcgmPolicy(nil)
	r := newNPUReconciler(policy)
	if err := r.ensureDcgmExporter(context.Background(), policy); err != nil {
		t.Fatalf("no-op 이어야 하는데 오류: %v", err)
	}
}
