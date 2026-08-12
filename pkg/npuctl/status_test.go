// status_test.go: CollectStatus 단위 테스트(fake client, envtest 불필요)
// 상세: NCP/DIP(+rngd 서브벤더)/NDR/DUS/Node를 섞어 넣고 ClusterStatus 집약 결과를 검증한다.
// 생성일: 2026-07-16
package npuctl

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := npuv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("npuv1alpha1.AddToScheme: %v", err)
	}
	return s
}

func newTestClient(t *testing.T, objs ...client.Object) *Client {
	t.Helper()
	fc := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(
			&npuv1alpha1.NPUClusterPolicy{},
			&npuv1alpha1.NodeDeviceReport{},
			&npuv1alpha1.DriverUpgradeState{},
		).
		Build()
	return newClientFromRuntimeClient(fc)
}

func TestCollectStatus_Aggregation(t *testing.T) {
	ncp := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "npuclusterpolicy-sample", Namespace: "npu-operator"},
		Spec: npuv1alpha1.NPUClusterPolicySpec{
			Nvidia: npuv1alpha1.NvidiaSpec{Enabled: true, DevicePluginImage: "nvidia/k8s-device-plugin:v1"},
			Furiosa: npuv1alpha1.FuriosaSpec{
				Enabled:           true,
				DevicePluginImage: "furiosa/k8s-device-plugin:v1",
				Rngd:              npuv1alpha1.RngdSpec{Enabled: true, DevicePluginImage: "furiosa/rngd-plugin:v1"},
			},
		},
		Status: npuv1alpha1.NPUClusterPolicyStatus{
			Phase: "Ready",
			Conditions: []metav1.Condition{
				{
					Type:    "Ready",
					Status:  metav1.ConditionTrue,
					Reason:  "AllResourcesReady",
					Message: "All resources reconciled successfully",
				},
			},
		},
	}

	dipNvidia := &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-gpu-ds"},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor:           "nvidia",
			Model:            "generic",
			Driver:           npuv1alpha1.DriverSpec{Version: "580.159.03", Installer: "apt"},
			VerifiedVersions: []string{"580.126.09", "580.159.03"},
		},
	}
	dipWarboy := &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "furiosa-warboy-ds"},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "furiosa",
			Model:  "warboy",
			Driver: npuv1alpha1.DriverSpec{Version: "1.9.8-3"},
		},
	}
	dipRngd := &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "furiosa-rngd-ds"},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "furiosa",
			Model:  "rngd",
			Driver: npuv1alpha1.DriverSpec{Version: "2026.1.0"},
		},
	}

	ndrWorker1 := &npuv1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-worker1"},
		Spec:       npuv1alpha1.NodeDeviceReportSpec{NodeName: "k8s-worker1"},
		Status: npuv1alpha1.NodeDeviceReportStatus{
			Devices: []npuv1alpha1.DeviceEntry{
				{
					Vendor: "furiosa", Model: "warboy", Count: 1,
					DriverLoaded: true, DriverVersion: "1.9.8-3", DriverBinding: "furiosa",
				},
				{
					Vendor: "nvidia", Model: "generic", Count: 2,
					DriverLoaded: true, DriverVersion: "580.159.03", DriverBinding: "nvidia",
				},
			},
		},
	}

	dusNvidia := &npuv1alpha1.DriverUpgradeState{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-worker1-nvidia"},
		Spec:       npuv1alpha1.DriverUpgradeStateSpec{NodeName: "k8s-worker1", Vendor: "nvidia"},
		Status: npuv1alpha1.DriverUpgradeStateStatus{
			State:          npuv1alpha1.UpgradeStateIdle,
			CurrentVersion: "580.159.03",
			DesiredVersion: "580.159.03",
		},
	}

	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-worker1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				"nvidia.com/gpu":      resource.MustParse("2"),
				"beta.furiosa.ai/npu": resource.MustParse("1"),
			},
		},
	}

	c := newTestClient(t, ncp, dipNvidia, dipWarboy, dipRngd, ndrWorker1, dusNvidia, node1)

	status, err := c.CollectStatus(context.Background())
	if err != nil {
		t.Fatalf("CollectStatus: %v", err)
	}

	// --- Vendors ---
	vendorByName := map[string]VendorStatus{}
	for _, v := range status.Vendors {
		vendorByName[v.Name] = v
	}
	if len(vendorByName) != 5 {
		t.Fatalf("expected 5 vendor buckets (nvidia/furiosa/rngd/rebellions/tenstorrent), got %d: %+v",
			len(vendorByName), vendorByName)
	}
	nv := vendorByName["nvidia"]
	nvVersionOK := len(nv.DriverInstallPolicies) == 1 && nv.DriverInstallPolicies[0].DesiredVersion == "580.159.03"
	if !nv.Enabled || !nvVersionOK {
		t.Errorf("nvidia vendor status 불일치: %+v", nv)
	}
	if len(vendorByName["nvidia"].DriverInstallPolicies[0].VerifiedVersions) != 2 {
		t.Errorf("nvidia verifiedVersions 누락: %+v", vendorByName["nvidia"].DriverInstallPolicies[0])
	}
	fu := vendorByName["furiosa"]
	if !fu.Enabled || len(fu.DriverInstallPolicies) != 1 || fu.DriverInstallPolicies[0].Model != "warboy" {
		t.Errorf("furiosa(warboy) vendor status 불일치(=rngd는 별도 분리되어야 함): %+v", fu)
	}
	rngd := vendorByName["rngd"]
	rngdDIPOK := len(rngd.DriverInstallPolicies) == 1 && rngd.DriverInstallPolicies[0].Name == "furiosa-rngd-ds"
	if !rngd.Enabled || rngd.SubVendorOf != "furiosa" || !rngdDIPOK {
		t.Errorf("rngd 서브벤더 분리 실패: %+v", rngd)
	}
	if reb := vendorByName["rebellions"]; reb.Enabled {
		t.Errorf("rebellions는 spec 미설정으로 disabled 여야 함: %+v", reb)
	}

	// --- Nodes ---
	if len(status.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d: %+v", len(status.Nodes), status.Nodes)
	}
	n := status.Nodes[0]
	if n.Name != "k8s-worker1" || len(n.Devices) != 2 {
		t.Fatalf("node aggregation 불일치: %+v", n)
	}
	if n.Allocatable["nvidia.com/gpu"] != "2" {
		t.Errorf("allocatable nvidia.com/gpu 불일치: %+v", n.Allocatable)
	}
	if len(n.Upgrades) != 1 || n.Upgrades[0].State != npuv1alpha1.UpgradeStateIdle {
		t.Errorf("DUS 집약 불일치: %+v", n.Upgrades)
	}

	// --- ClusterPolicies ---
	if len(status.ClusterPolicies) != 1 || status.ClusterPolicies[0].Ready != "True" {
		t.Errorf("NPUClusterPolicy 상태 집약 불일치: %+v", status.ClusterPolicies)
	}
}

// TestBuildNodeStatusesCarriesPCIAndProduct 는 같은 노드에 붙은 서로 다른 모델 두 장이
// 화면에서 구분 가능한지 본다. 구분 축은 PCI 주소이고, 표시명은 벤더/모델 합성이다.
func TestBuildNodeStatusesCarriesPCIAndProduct(t *testing.T) {
	ndr := npuv1alpha1.NodeDeviceReport{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-worker1"},
		Spec:       npuv1alpha1.NodeDeviceReportSpec{NodeName: "k8s-worker1"},
		Status: npuv1alpha1.NodeDeviceReportStatus{
			Devices: []npuv1alpha1.DeviceEntry{
				{
					Vendor: "nvidia", Model: "a30", Count: 1,
					DriverLoaded: true, DriverVersion: "580.173.02", DriverBinding: "nvidia",
					PCIeAddress: "0000:18:00.0",
				},
				{
					Vendor: "nvidia", Model: "a2", Count: 1,
					DriverLoaded: true, DriverVersion: "580.173.02", DriverBinding: "nvidia",
					PCIeAddress: "0000:86:00.0",
				},
				{
					// 모델 미판정 장치. "모른다"는 사실이 표시명에 남아야 한다.
					Vendor: "nvidia", Model: "", Count: 1,
					DriverLoaded: true, DriverVersion: "580.173.02", DriverBinding: "nvidia",
					PCIeAddress: "0000:af:00.0",
				},
			},
		},
	}

	out := buildNodeStatuses([]npuv1alpha1.NodeDeviceReport{ndr}, nil, nil)
	if len(out) != 1 || len(out[0].Devices) != 3 {
		t.Fatalf("노드 1개·장치 3개가 나와야 함: %+v", out)
	}

	want := []DeviceStatus{
		{Product: "nvidia/a30", PCIeAddress: "0000:18:00.0"},
		{Product: "nvidia/a2", PCIeAddress: "0000:86:00.0"},
		{Product: "nvidia/generic", PCIeAddress: "0000:af:00.0"},
	}
	for i, w := range want {
		got := out[0].Devices[i]
		if got.Product != w.Product {
			t.Errorf("devices[%d].Product = %q, want %q", i, got.Product, w.Product)
		}
		if got.PCIeAddress != w.PCIeAddress {
			t.Errorf("devices[%d].PCIeAddress = %q, want %q", i, got.PCIeAddress, w.PCIeAddress)
		}
	}
}

func TestCollectStatus_EmptyCluster(t *testing.T) {
	c := newTestClient(t)
	status, err := c.CollectStatus(context.Background())
	if err != nil {
		t.Fatalf("빈 클러스터에서도 에러 없이 빈 결과를 반환해야 함: %v", err)
	}
	if len(status.Vendors) != 5 {
		t.Fatalf("빈 클러스터에서도 5개 벤더 버킷(모두 disabled)이 있어야 함: %+v", status.Vendors)
	}
	for _, v := range status.Vendors {
		if v.Enabled {
			t.Errorf("NCP 없는 클러스터에서 %s 가 enabled 로 나옴", v.Name)
		}
	}
	if len(status.Nodes) != 0 {
		t.Errorf("빈 클러스터에서 노드가 없어야 함: %+v", status.Nodes)
	}
}
