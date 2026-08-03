// ============================================================
// dra_driver_test.go: DRA 드라이버 렌더러와 배포 게이트 시험
// 상세: 벤더 chart 를 그대로 쓰지 않고 우리 라벨·이미지로 옮기는 것이 렌더러의
// 존재 이유다. 그 치환이 실제로 일어나는지를 고정한다.
// 생성일: 2026-08-07
// ============================================================
package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

func furiosaDRAPolicy() *npuv1alpha1.NPUClusterPolicy {
	p := &npuv1alpha1.NPUClusterPolicy{}
	p.Spec.Furiosa.Enabled = true
	p.Spec.Furiosa.Rngd.Enabled = true
	p.Spec.Furiosa.Rngd.DRA = &npuv1alpha1.DRASpec{Enabled: true}
	return p
}

func findDS(objs []client.Object) *appsv1.DaemonSet {
	for _, o := range objs {
		if ds, ok := o.(*appsv1.DaemonSet); ok {
			return ds
		}
	}
	return nil
}

// 벤더 chart 의 affinity 는 NFD 라벨과 furiosa-feature-discovery 라벨을 요구한다.
// 우리 클러스터에는 셋 다 없어서 그대로 쓰면 DaemonSet 이 어느 노드에도 안 뜬다.
// 렌더러는 그것을 우리 자립 라벨로 바꿔야 한다.
func TestRenderFuriosaDRADriver_UsesOurNodeLabel(t *testing.T) {
	objs := renderFuriosaDRADriver(furiosaDRAPolicy())
	ds := findDS(objs)
	if ds == nil {
		t.Fatal("DaemonSet 이 렌더되지 않았다")
	}
	sel := ds.Spec.Template.Spec.NodeSelector
	if sel["kcloud.ai/rngd.present"] != labelValueTrue {
		t.Errorf("nodeSelector=%v — 우리 자립 라벨이 없다", sel)
	}
	if ds.Spec.Template.Spec.Affinity != nil {
		t.Error("벤더 affinity 가 남아 있다 — NFD 라벨을 요구해 스케줄되지 않는다")
	}
}

// 이미지는 spec 이 주면 그것, 아니면 렌더러 기본값이다. 사설 미러 경로는 차트가
// spec 에 넣는다 — 이 저장소는 레지스트리를 CR 이 아니라 helm 에서 조립한다.
func TestRenderFuriosaDRADriver_DefaultImage(t *testing.T) {
	ds := findDS(renderFuriosaDRADriver(furiosaDRAPolicy()))
	img := ds.Spec.Template.Spec.Containers[0].Image
	if !strings.Contains(img, "furiosa-dra-driver:2026.1.1") {
		t.Errorf("image=%q — 기본 이미지가 아니다", img)
	}
}

// 이 이미지에는 ENTRYPOINT 가 없다. command 를 주지 않으면 args 가 CMD 를 밀어내
// 바이너리가 사라지고 컨테이너가 exec 실패로 죽는다(2026-08-07 실측).
func TestRenderFuriosaDRADriver_SetsCommand(t *testing.T) {
	ds := findDS(renderFuriosaDRADriver(furiosaDRAPolicy()))
	c := ds.Spec.Template.Spec.Containers[0]
	if len(c.Command) == 0 {
		t.Fatal("command 가 비어 있다 — 이미지에 ENTRYPOINT 가 없어 args 만으로는 못 뜬다")
	}
	if !strings.Contains(c.Command[0], "dra-kubelet-plugin") {
		t.Errorf("command=%v — 드라이버 바이너리가 아니다", c.Command)
	}
}

// 1.34 세대 권한이다. 빠지면 드라이버가 조용히 실패한다.
func TestRenderFuriosaDRADriver_HasAssociatedNodeVerb(t *testing.T) {
	var found bool
	for _, o := range renderFuriosaDRADriver(furiosaDRAPolicy()) {
		cr, ok := o.(*rbacv1.ClusterRole)
		if !ok {
			continue
		}
		for _, r := range cr.Rules {
			for _, res := range r.Resources {
				if res != "resourceclaims/driver" {
					continue
				}
				for _, v := range r.Verbs {
					if v == "associated-node:update" {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("resourceclaims/driver 의 associated-node:update 권한이 없다")
	}
}

// CDI 는 containerd 기본 비활성이다. init container 가 켜지 않으면 장치 주입이 안 된다.
func TestRenderFuriosaDRADriver_HasCDIInitContainer(t *testing.T) {
	ds := findDS(renderFuriosaDRADriver(furiosaDRAPolicy()))
	if len(ds.Spec.Template.Spec.InitContainers) == 0 {
		t.Fatal("init container 가 없다 — containerd CDI 를 켤 주체가 없다")
	}
	init := ds.Spec.Template.Spec.InitContainers[0]
	joined := strings.Join(init.Command, " ") + " " + strings.Join(init.Args, " ")
	if !strings.Contains(joined, "enable_cdi") {
		t.Errorf("init container 가 enable_cdi 를 다루지 않는다: %q", joined)
	}
	if !ds.Spec.Template.Spec.HostPID {
		t.Error("hostPID 가 없다 — nsenter -t 1 로 호스트 containerd 를 재시작할 수 없다")
	}
}

// 이미지·인자·환경변수만 덮을 수 있어야 한다.
func TestRenderFuriosaDRADriver_Overrides(t *testing.T) {
	p := furiosaDRAPolicy()
	p.Spec.Furiosa.Rngd.DRA.Image = "reg.example.com/kcloud/custom:9.9"
	p.Spec.Furiosa.Rngd.DRA.Args = []string{"--flag"}
	ds := findDS(renderFuriosaDRADriver(p))
	c := ds.Spec.Template.Spec.Containers[0]
	if c.Image != "reg.example.com/kcloud/custom:9.9" {
		t.Errorf("image=%q — 오버라이드가 무시됐다", c.Image)
	}
	if len(c.Args) != 1 || c.Args[0] != "--flag" {
		t.Errorf("args=%v — 오버라이드가 무시됐다", c.Args)
	}
}

// DeviceClass 가 없으면 사용자가 요청할 대상이 없다.
func TestRenderFuriosaDRADriver_HasDeviceClass(t *testing.T) {
	objs := renderFuriosaDRADriver(furiosaDRAPolicy())
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.GetObjectKind().GroupVersionKind().Kind+"/"+o.GetName())
	}
	joined := strings.Join(names, " ")
	if !strings.Contains(joined, "DeviceClass/npu.furiosa.ai") {
		t.Errorf("DeviceClass 가 없다: %v", names)
	}
}

// newDRAReconciler 는 resource.k8s.io 를 담은 scheme 으로 reconciler 를 만든다.
// newTestScheme 에는 그 그룹이 없어 DeviceClass·ResourceSlice 를 못 담는다.
func newDRAReconciler(objs ...client.Object) *NPUClusterPolicyReconciler {
	s := sharingScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &NPUClusterPolicyReconciler{Client: c, Scheme: s}
}

// 켜면 배포된다.
func TestEnsureDRADriver_CreatesWhenEnabled(t *testing.T) {
	p := furiosaDRAPolicy()
	r := newDRAReconciler(p)
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("ensureDRADriver: %v", err)
	}
	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(),
		types.NamespacedName{Name: draDSNameFuriosa, Namespace: draNamespace}, &ds); err != nil {
		t.Fatalf("DaemonSet 이 생성되지 않았다: %v", err)
	}
	var dc resourcev1.DeviceClass
	if err := r.Get(context.Background(),
		types.NamespacedName{Name: draDriverNameFuriosa}, &dc); err != nil {
		t.Fatalf("DeviceClass 가 생성되지 않았다: %v", err)
	}
}

// 끄면 회수된다. 켰다 끄는 왕복이 성립해야 정책이 되돌릴 수 있는 축이 된다.
func TestEnsureDRADriver_DeletesWhenDisabled(t *testing.T) {
	p := furiosaDRAPolicy()
	r := newDRAReconciler(p)
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("최초 배포 실패: %v", err)
	}
	p.Spec.Furiosa.Rngd.DRA.Enabled = false
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("회수 실패: %v", err)
	}
	var ds appsv1.DaemonSet
	err := r.Get(context.Background(),
		types.NamespacedName{Name: draDSNameFuriosa, Namespace: draNamespace}, &ds)
	if !apierrors.IsNotFound(err) {
		t.Errorf("DaemonSet 이 남아 있다(err=%v)", err)
	}
}

// 렌더러가 없는 벤더는 배포하지 않고 사유를 남긴다.
func TestEnsureDRADriver_RefusesUnsupportedVendor(t *testing.T) {
	p := &npuv1alpha1.NPUClusterPolicy{}
	p.Spec.Tenstorrent.Enabled = true
	p.Spec.Tenstorrent.DRA = &npuv1alpha1.DRASpec{Enabled: true}
	r := newDRAReconciler(p)
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("거절은 에러가 아니어야 한다: %v", err)
	}
	var found bool
	for _, s := range p.Status.DRADrivers {
		if s.Vendor != vendorTenstorrent {
			continue
		}
		found = true
		if s.Phase != npuv1alpha1.DRAPhaseBlocked {
			t.Errorf("phase=%q, want Blocked", s.Phase)
		}
		if s.Message == "" {
			t.Error("사유가 비었다 — 조용히 안 되는 것과 이유 있는 거절은 다르다")
		}
	}
	if !found {
		t.Error("tenstorrent 상태가 기록되지 않았다")
	}
}

// 발행물이 없으면 Ready 로 올리지 않는다. 파드가 떴다고 DRA 를 쓸 수 있는 것이 아니다.
func TestEnsureDRADriver_NotReadyWithoutSlices(t *testing.T) {
	p := furiosaDRAPolicy()
	r := newDRAReconciler(p)
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("ensureDRADriver: %v", err)
	}
	for _, s := range p.Status.DRADrivers {
		if s.Vendor == vendorFuriosa && s.Phase == npuv1alpha1.DRAPhaseReady {
			t.Error("ResourceSlice 가 없는데 Ready 다")
		}
	}
}

// 발행물이 있으면 그 수만큼 노드를 센다.
func TestEnsureDRADriver_ReadyCountsPublishingNodes(t *testing.T) {
	slice := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "rngd-1-npu"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   draDriverNameFuriosa,
			NodeName: ptr.To("rngd-1"),
			Pool:     resourcev1.ResourcePool{Name: "rngd-1", ResourceSliceCount: 1},
		},
	}
	p := furiosaDRAPolicy()
	r := newDRAReconciler(p, slice)
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("ensureDRADriver: %v", err)
	}
	for _, s := range p.Status.DRADrivers {
		if s.Vendor != vendorFuriosa {
			continue
		}
		if s.Phase != npuv1alpha1.DRAPhaseReady {
			t.Errorf("phase=%q, want Ready", s.Phase)
		}
		if s.PublishedNodes != 1 {
			t.Errorf("publishedNodes=%d, want 1", s.PublishedNodes)
		}
	}
}

// 벤더 chart 의 namespace Role 은 pods·deployments·jobs 전권이다. 그것을 옮기면
// operator 자신이 그 전권을 들고 있어야 하고(권한 상승 방지), 장치 발행자에게
// 줄 이유도 없다. 옮기지 않았다는 사실을 고정한다.
func TestRenderFuriosaDRADriver_NoWildcardNamespaceRole(t *testing.T) {
	for _, o := range renderFuriosaDRADriver(furiosaDRAPolicy()) {
		if role, ok := o.(*rbacv1.Role); ok {
			t.Fatalf("namespace Role 이 렌더됐다: %+v", role.Rules)
		}
	}
}

// NVIDIA 는 "깔되 광고 안 함" 상태가 없다. chart 가 gpus.enabled=false 면 DaemonSet 도
// DeviceClass 도 내지 않는다(2026-08-07 실측: RBAC 껍데기뿐). 그 상태로 깔면
// 사용자는 깔렸다고 믿는데 아무것도 발행되지 않는다.
func TestNvidiaDRABlockReason_RequiresDRAAdvertise(t *testing.T) {
	p := &npuv1alpha1.NPUClusterPolicy{}
	p.Spec.Nvidia.Enabled = true
	p.Spec.Nvidia.DRA = &npuv1alpha1.DRASpec{Enabled: true}
	p.Spec.Nvidia.AdvertiseBy = npuv1alpha1.AdvertiseByDevicePlugin

	reason := nvidiaDRABlockReason(p)
	if reason == "" {
		t.Fatal("device-plugin 광고 중인데 배포를 허용했다")
	}
	if !strings.Contains(reason, "advertiseBy") {
		t.Errorf("사유가 축을 지목하지 않는다: %q", reason)
	}
}

// dra 로 넘긴 상태면 배포해도 된다.
func TestNvidiaDRABlockReason_AllowsWhenDRAAdvertise(t *testing.T) {
	p := &npuv1alpha1.NPUClusterPolicy{}
	p.Spec.Nvidia.Enabled = true
	p.Spec.Nvidia.DRA = &npuv1alpha1.DRASpec{Enabled: true}
	p.Spec.Nvidia.AdvertiseBy = npuv1alpha1.AdvertiseByDRA

	if reason := nvidiaDRABlockReason(p); reason != "" {
		t.Errorf("허용해야 하는데 거절했다: %q", reason)
	}
}

// chart 값은 DaemonSet 하나에 하나다. 노드 일부만 넘기는 구성은 표현할 수 없으므로
// 조용히 반쪽 상태가 되게 두지 않고 거절한다.
func TestNvidiaDRABlockReason_RefusesPartialSwitch(t *testing.T) {
	p := &npuv1alpha1.NPUClusterPolicy{}
	p.Spec.Nvidia.Enabled = true
	p.Spec.Nvidia.DRA = &npuv1alpha1.DRASpec{Enabled: true}
	p.Spec.Nvidia.AdvertiseBy = npuv1alpha1.AdvertiseByDRA
	p.Spec.Nvidia.AdvertiseByNodeSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"kubernetes.io/hostname": "k8s-worker1"},
	}

	reason := nvidiaDRABlockReason(p)
	if reason == "" {
		t.Fatal("부분 전환을 허용했다 — chart 값이 노드 단위가 아니라 표현할 수 없다")
	}
	if !strings.Contains(reason, "전부") {
		t.Errorf("사유가 전부-아니면-전무 제약을 설명하지 않는다: %q", reason)
	}
}

// Furiosa 는 이 제약이 없다. NVIDIA 게이트가 다른 벤더에 새면 안 된다.
func TestNvidiaDRABlockReason_IgnoresOtherVendors(t *testing.T) {
	p := furiosaDRAPolicy()
	p.Spec.Furiosa.Rngd.AdvertiseBy = npuv1alpha1.AdvertiseByDevicePlugin
	if reason := nvidiaDRABlockReason(p); reason != "" {
		t.Errorf("NVIDIA 축이 아닌데 사유를 냈다: %q", reason)
	}
}

func nvidiaDRAPolicy() *npuv1alpha1.NPUClusterPolicy {
	p := &npuv1alpha1.NPUClusterPolicy{}
	p.Spec.Nvidia.Enabled = true
	p.Spec.Nvidia.AdvertiseBy = npuv1alpha1.AdvertiseByDRA
	p.Spec.Nvidia.DRA = &npuv1alpha1.DRASpec{Enabled: true}
	return p
}

// chart affinity 는 NFD 4종과 외부 operator 라벨 1종의 OR 이다. 우리는 둘 다 안 쓴다.
func TestRenderNvidiaDRADriver_UsesOurNodeLabel(t *testing.T) {
	ds := findDS(renderNvidiaDRADriver(nvidiaDRAPolicy()))
	if ds == nil {
		t.Fatal("DaemonSet 이 렌더되지 않았다")
	}
	if ds.Spec.Template.Spec.NodeSelector["kcloud.ai/nvidia.present"] != labelValueTrue {
		t.Errorf("nodeSelector=%v — 우리 자립 라벨이 없다", ds.Spec.Template.Spec.NodeSelector)
	}
	if ds.Spec.Template.Spec.Affinity != nil {
		t.Error("벤더 affinity 가 남아 있다 — NFD·외부 operator 라벨을 요구해 스케줄되지 않는다")
	}
}

// CDI init container 가 벤더 prestart 보다 먼저 와야 한다. 순서가 뒤집히면
// prestart 가 CDI 없는 상태에서 돈다.
func TestRenderNvidiaDRADriver_CDIInitRunsFirst(t *testing.T) {
	ds := findDS(renderNvidiaDRADriver(nvidiaDRAPolicy()))
	inits := ds.Spec.Template.Spec.InitContainers
	if len(inits) < 2 {
		t.Fatalf("init container 가 %d 개 — CDI 와 벤더 prestart 둘이어야 한다", len(inits))
	}
	if inits[0].Name != "enable-cdi" {
		t.Errorf("첫 init container=%q — CDI 활성화가 먼저여야 한다", inits[0].Name)
	}
}

// compute-domain 은 다중 노드 NVLink 용이다. GPU 노드가 하나뿐이라 뺀다.
// chart 는 값을 꺼도 관련 오브젝트를 남기므로 렌더러가 걸러야 한다.
func TestRenderNvidiaDRADriver_ExcludesComputeDomain(t *testing.T) {
	for _, o := range renderNvidiaDRADriver(nvidiaDRAPolicy()) {
		if strings.Contains(o.GetName(), "compute-domain") {
			t.Errorf("compute-domain 오브젝트가 남았다: %s", o.GetName())
		}
		cr, ok := o.(*rbacv1.ClusterRole)
		if !ok {
			continue
		}
		for _, r := range cr.Rules {
			for _, g := range r.APIGroups {
				if g == "resource.nvidia.com" {
					t.Errorf("다중 노드 NVLink 전용 API 그룹 권한이 남았다: %+v", r)
				}
			}
		}
	}
}

// 발행 관측이 이 driver 이름으로 ResourceSlice 를 센다. DeviceClass 셋이 다 같은
// driver 를 가리키므로 이름이 틀리면 영원히 배포됨 상태에 머문다.
func TestRenderNvidiaDRADriver_DeviceClassesPresent(t *testing.T) {
	var got []string
	for _, o := range renderNvidiaDRADriver(nvidiaDRAPolicy()) {
		if o.GetObjectKind().GroupVersionKind().Kind == "DeviceClass" {
			got = append(got, o.GetName())
		}
	}
	for _, want := range []string{"gpu.nvidia.com", "mig.nvidia.com", "vfio.gpu.nvidia.com"} {
		var found bool
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DeviceClass %q 가 없다 (있는 것: %v)", want, got)
		}
	}
}

// 이미지는 spec 이 주면 그것, 아니면 렌더러 기본값이다. 사설 미러 경로는 차트가
// spec 에 넣는다 — 이 저장소는 레지스트리를 CR 이 아니라 helm 에서 조립한다.
func TestRenderNvidiaDRADriver_DefaultImage(t *testing.T) {
	ds := findDS(renderNvidiaDRADriver(nvidiaDRAPolicy()))
	img := ds.Spec.Template.Spec.Containers[0].Image
	if !strings.Contains(img, "dra-driver-nvidia-gpu:") {
		t.Errorf("image=%q — 기본 이미지가 아니다", img)
	}
	if ds.Spec.Template.Spec.InitContainers[1].Image != img {
		t.Errorf("prestart 이미지가 본체와 다르다: %q", ds.Spec.Template.Spec.InitContainers[1].Image)
	}
}

// 이 드라이버는 호스트 드라이버 루트를 읽고 kubelet 플러그인 디렉터리에 쓴다.
// 볼륨 하나가 빠지면 컨테이너는 뜨지만 발행이 안 된다.
func TestRenderNvidiaDRADriver_HasVendorVolumes(t *testing.T) {
	ds := findDS(renderNvidiaDRADriver(nvidiaDRAPolicy()))
	have := map[string]bool{}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		have[v.Name] = true
	}
	for _, want := range []string{"plugins-registry", "plugins", "cdi", "driver-root", "driver-root-parent"} {
		if !have[want] {
			t.Errorf("볼륨 %q 가 없다 (있는 것: %v)", want, have)
		}
	}
}

// 벤더 게이트가 배포 자체를 막는지 본다. 사유만 남기고 오브젝트를 만들면 안 된다.
func TestEnsureDRADriver_NvidiaBlockedWithoutDRAAdvertise(t *testing.T) {
	p := nvidiaDRAPolicy()
	p.Spec.Nvidia.AdvertiseBy = npuv1alpha1.AdvertiseByDevicePlugin
	r := newDRAReconciler(p)
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("거절은 에러가 아니어야 한다: %v", err)
	}
	var ds appsv1.DaemonSet
	err := r.Get(context.Background(),
		types.NamespacedName{Name: draDSNameNvidia, Namespace: draNamespace}, &ds)
	if !apierrors.IsNotFound(err) {
		t.Errorf("거절했는데 DaemonSet 이 생겼다(err=%v)", err)
	}
	for _, s := range p.Status.DRADrivers {
		if s.Vendor != vendorNvidia {
			continue
		}
		if s.Phase != npuv1alpha1.DRAPhaseBlocked {
			t.Errorf("phase=%q, want Blocked", s.Phase)
		}
	}
}

// dra 로 넘긴 상태면 실제로 배포된다.
func TestEnsureDRADriver_NvidiaDeploysWhenAllowed(t *testing.T) {
	p := nvidiaDRAPolicy()
	r := newDRAReconciler(p)
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("ensureDRADriver: %v", err)
	}
	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(),
		types.NamespacedName{Name: draDSNameNvidia, Namespace: draNamespace}, &ds); err != nil {
		t.Fatalf("DaemonSet 이 생성되지 않았다: %v", err)
	}
}

// control-plane 노드에도 벤더 라벨이 붙어 있을 수 있다(실측: k8s-master 에
// nvidia.present). 장치가 없는 노드에 올리면 벤더 prestart 가 init 에서 멈춘다.
func TestEnsureDRADriver_ExcludesControlPlane(t *testing.T) {
	p := nvidiaDRAPolicy()
	r := newDRAReconciler(p)
	if err := r.ensureDRADriver(context.Background(), p); err != nil {
		t.Fatalf("ensureDRADriver: %v", err)
	}
	var ds appsv1.DaemonSet
	if err := r.Get(context.Background(),
		types.NamespacedName{Name: draDSNameNvidia, Namespace: draNamespace}, &ds); err != nil {
		t.Fatalf("DaemonSet 조회 실패: %v", err)
	}
	aff := ds.Spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil {
		t.Fatal("control-plane 배제 affinity 가 없다")
	}
	var found bool
	for _, term := range aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, req := range term.MatchExpressions {
			if req.Operator == corev1.NodeSelectorOpDoesNotExist {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("control-plane 라벨 부재 조건이 없다: %+v", aff.NodeAffinity)
	}
}

// CDI 활성화는 "파일에 썼다" 가 아니라 "containerd 가 실제로 켰다" 로 판정해야 한다.
// 같은 plugin 테이블을 다시 선언하는 drop-in 이 있으면 base 에 쓴 값이 덮인다
// (2026-08-10 k8s-worker1 실측: conf.d/99-nvidia.toml 때문에 enable_cdi 가 false).
func TestRenderCDIInitContainer_UsesEffectiveConfigAndDropIn(t *testing.T) {
	c := renderCDIInitContainer(furiosaDRAPolicy())
	script := strings.Join(c.Command, " ")

	for _, want := range []string{
		"containerd config dump", // 파일이 아니라 유효 설정으로 판정
		"CDI target",             // 마지막에 선언한 파일을 골라 거기에 쓴다
		"imports",                // drop-in 을 쓰는지 base 를 고치는지 가르는 조건
	} {
		if !strings.Contains(script, want) {
			t.Errorf("스크립트에 %q 가 없다", want)
		}
	}
	// 적용 확인 없이 성공으로 끝나면 장치 주입이 안 되는 채로 드라이버만 뜬다.
	if !strings.Contains(script, "CDI 적용 실패") {
		t.Error("적용 확인 실패 시 종료하는 경로가 없다")
	}
	// 같은 테이블에 키가 두 번 들어가면 containerd 가 파싱에 실패한다.
	if !strings.Contains(script, "enable_cdi[[:space:]]*=") {
		t.Error("기존 키 존재 여부를 보지 않는다 — 중복 키를 만들 수 있다")
	}
	// 새 drop-in 을 만들면 그 plugin 테이블의 다른 설정(nvidia 런타임 등록 등)이
	// 통째로 사라진다. 파일을 만들지 않고 기존 선언 파일을 고쳐야 한다.
	if strings.Contains(script, "99-zz-cdi.toml") || strings.Contains(script, "cat > $DROPIN") {
		t.Error("새 drop-in 을 만든다 — 같은 테이블의 다른 설정을 지운다")
	}
	// 실패하면 되돌려야 한다. 반쯤 고친 채로 두면 노드가 망가진 상태로 남는다.
	if !strings.Contains(script, "원복한다") {
		t.Error("적용 실패 시 원복 경로가 없다")
	}
}
