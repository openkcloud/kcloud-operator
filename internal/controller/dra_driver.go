// ============================================================
// dra_driver.go: 벤더 DRA 드라이버 배포·회수
// 상세: 벤더 chart 를 그대로 설치하지 않고 우리 라벨·이미지·네임스페이스로 옮겨
//       만든다. chart 의 affinity 가 NFD 와 벤더 탐지기 라벨 셋을 요구해 우리
//       클러스터에서는 어느 노드에도 스케줄되지 않기 때문이다
//       (docs/design/dra-driver-lifecycle.md §3.2).
//       사설 미러 경로는 여기서 조립하지 않는다 — 이 저장소는 레지스트리를 CR 이
//       아니라 helm 에서 조립하고, 완성된 경로가 spec 으로 내려온다.
// 생성일: 2026-08-07
// ============================================================

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

const (
	// draDSNameFuriosa 는 배포되는 DaemonSet 이름이다.
	draDSNameFuriosa = "kcloud-furiosa-dra-driver"
	// draDriverNameFuriosa 는 이 드라이버가 발행물에 쓰는 spec.driver 값이다.
	// 발행 관측이 이 값으로 ResourceSlice 를 고른다.
	draDriverNameFuriosa = "npu.furiosa.ai"
	// draDriverImageFuriosaDefault 는 spec 미지정 시 기본 이미지다.
	// 벤더 chart 2026.1.1 이 쓰는 것과 같다.
	draDriverImageFuriosaDefault = "docker.io/furiosaai/furiosa-dra-driver:2026.1.1"
	// draDriverCommandFuriosa 는 이미지의 CMD 다. 이 이미지에는 ENTRYPOINT 가 없어서
	// args 만 주면 CMD 가 통째로 밀려 바이너리가 사라진다(실측: exec 실패).
	draDriverCommandFuriosa = "./dra-kubelet-plugin"
	// draCDIRoot 는 드라이버가 CDI 스펙을 쓰는 경로다. init container 가 containerd 에
	// 등록하는 경로와 같아야 한다.
	draCDIRoot = "/var/run/cdi"
	// draNamespace 는 드라이버 오브젝트를 두는 네임스페이스다. 벤더 chart 는
	// kube-system 이지만 우리 operand 규약을 따른다.
	draNamespace = "kcloud"
)

// renderFuriosaDRADriver 는 Furiosa DRA 드라이버 오브젝트 다섯을 만든다.
// ServiceAccount · ClusterRole · ClusterRoleBinding · DaemonSet · DeviceClass.
func renderFuriosaDRADriver(policy *npuv1alpha1.NPUClusterPolicy) []client.Object {
	spec := policy.Spec.Furiosa.Rngd.DRA
	labels := map[string]string{
		"app.kubernetes.io/name":      draDSNameFuriosa,
		"app.kubernetes.io/component": "dra-driver",
	}

	sa := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: draDSNameFuriosa, Namespace: draNamespace, Labels: labels},
	}

	// 벤더 chart 실측 8줄을 그대로 옮긴다. resourceclaims/driver 의
	// associated-node:update 는 1.34 세대 권한이고, 빠지면 드라이버가 조용히 실패한다.
	cr := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: draDSNameFuriosa, Labels: labels},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceclaims"},
				Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceclaims/status"},
				Verbs: []string{"update", "patch"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceclaims/driver"},
				Verbs: []string{"associated-node:update", "associated-node:patch"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceclaimtemplates"},
				Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceclaimtemplates/status"},
				Verbs: []string{"update", "patch"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceslices"},
				Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceslices/status"},
				Verbs: []string{"update", "patch"}},
		},
	}

	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: draDSNameFuriosa, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: draDSNameFuriosa},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: draDSNameFuriosa, Namespace: draNamespace}},
	}

	// 벤더 chart 의 namespace Role 은 pods·deployments·jobs 전권(*)이다. 그대로 옮기지
	// 않는다 — 장치 발행자가 그 권한을 쓸 이유가 없고, 옮기려면 operator 자신이 먼저
	// 그 전권을 들고 있어야 한다(권한 상승 방지 규칙). 필요가 드러나면 그때 최소로 준다.

	dc := &resourcev1.DeviceClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "resource.k8s.io/v1", Kind: "DeviceClass"},
		ObjectMeta: metav1.ObjectMeta{Name: draDriverNameFuriosa, Labels: labels},
		Spec: resourcev1.DeviceClassSpec{
			Selectors: []resourcev1.DeviceSelector{{
				CEL: &resourcev1.CELDeviceSelector{
					Expression: "device.driver == '" + draDriverNameFuriosa + "'",
				},
			}},
		},
	}

	return []client.Object{sa, cr, crb, renderFuriosaDRADaemonSet(policy, spec, labels), dc}
}

// renderFuriosaDRADaemonSet 은 드라이버 DaemonSet 을 만든다. 벤더 chart 의
// 컨테이너·볼륨은 그대로 쓰고 스케줄 조건만 우리 것으로 바꾼다.
func renderFuriosaDRADaemonSet(policy *npuv1alpha1.NPUClusterPolicy, spec *npuv1alpha1.DRASpec,
	labels map[string]string) *appsv1.DaemonSet {
	image := draDriverImageFuriosaDefault
	if spec != nil && spec.Image != "" {
		image = spec.Image
	}
	args := []string{"--node-name=$(NODE_NAME)", "--cdi-root=$(CDI_ROOT)"}
	if spec != nil && len(spec.Args) > 0 {
		args = spec.Args
	}
	env := []corev1.EnvVar{
		{Name: "CDI_ROOT", Value: draCDIRoot},
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
	}
	if spec != nil {
		env = mergeEnv(env, spec.Env)
	}

	noEscalate := false
	hostPathDir := corev1.HostPathDirectoryOrCreate
	hostVol := func(name, path string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &hostPathDir},
		}}
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name: draDSNameFuriosa, Namespace: draNamespace, Labels: labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// 벤더 chart 의 affinity 는 NFD 와 벤더 탐지기 라벨 셋을 요구한다.
					// 우리는 둘 다 안 쓰므로 자립 라벨로 갈아 끼운다.
					NodeSelector:       map[string]string{"kcloud.ai/rngd.present": "true"},
					Affinity:           nil,
					ServiceAccountName: draDSNameFuriosa,
					PriorityClassName:  "system-node-critical",
					// nsenter -t 1 이 호스트 PID 네임스페이스를 요구한다.
					HostPID:        true,
					Tolerations:    []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					InitContainers: []corev1.Container{renderCDIInitContainer(policy)},
					Containers: []corev1.Container{{
						Name:    "plugin",
						Image:   image,
						Command: []string{draDriverCommandFuriosa},
						Args:    args,
						Env:     env,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &noEscalate,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "plugins-registry", MountPath: "/var/lib/kubelet/plugins_registry"},
							{Name: "plugins", MountPath: "/var/lib/kubelet/plugins"},
							{Name: "cdi", MountPath: draCDIRoot},
							{Name: "dev-fs", MountPath: "/dev"},
							{Name: "sys-fs", MountPath: "/sys"},
						},
					}},
					Volumes: []corev1.Volume{
						hostVol("plugins-registry", "/var/lib/kubelet/plugins_registry"),
						hostVol("plugins", "/var/lib/kubelet/plugins"),
						hostVol("cdi", draCDIRoot),
						hostVol("dev-fs", "/dev"),
						hostVol("sys-fs", "/sys"),
						hostVol("host-etc-containerd", "/etc/containerd"),
					},
				},
			},
		},
	}
}

// mergeEnv 는 base 에 extra 를 더한다. 같은 이름이면 extra 가 이긴다.
func mergeEnv(base, extra []corev1.EnvVar) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(base)+len(extra))
	override := map[string]corev1.EnvVar{}
	for _, e := range extra {
		override[e.Name] = e
	}
	for _, e := range base {
		if o, ok := override[e.Name]; ok {
			out = append(out, o)
			delete(override, e.Name)
			continue
		}
		out = append(out, e)
	}
	for _, e := range extra {
		if _, still := override[e.Name]; still {
			out = append(out, e)
			delete(override, e.Name)
		}
	}
	return out
}

// renderCDIInitContainer 는 containerd 에 CDI 를 켜는 init container 다.
// 이미 켜져 있으면 아무것도 하지 않는다 — 매번 재시작하면 노드가 흔들린다.
//
// 판정과 기록 위치가 둘 다 "유효 설정" 기준이다. 파일에 값이 있어도 같은 plugin
// 테이블을 다시 선언하는 drop-in 이 있으면 그쪽이 이긴다. 그래서 base 가 conf.d 를
// import 하면 거기에 쓰고, 마지막에 실제로 켜졌는지 다시 확인한 뒤 끝낸다.
//
// 이미지는 kcloud-host-exec 을 재사용한다(HOST_EXEC_IMAGE). 그 이미지에 python3 은
// 없고 awk·grep·nsenter 는 있다(2026-08-07 실측). systemctl 도 이미지에는 없으나
// nsenter 로 호스트 것을 부르므로 무관하다.
func renderCDIInitContainer(_ *npuv1alpha1.NPUClusterPolicy) corev1.Container {
	const script = `set -eu
DIR=/host/etc/containerd
CFG=$DIR/config.toml
SEC='[plugins."io.containerd.grpc.v1.cri"]'

# 유효 설정이 이미 켜져 있으면 아무것도 하지 않는다. 파일이 아니라 containerd 가
# 실제로 읽은 값을 본다 — 파일에 true 가 있어도 뒤 파일이 덮으면 꺼져 있다.
if nsenter -t 1 -m -u -i -n -p -- containerd config dump 2>/dev/null |
     grep -q 'enable_cdi[[:space:]]*=[[:space:]]*true'; then
  echo "CDI already enabled (effective) - no-op"
  exit 0
fi

# 이 plugin 테이블을 선언하는 파일 중 **마지막에 로드되는 것**을 고른다.
# containerd 의 import 병합은 같은 plugin 테이블을 재선언하면 앞 파일의 그 테이블을
# 통째로 대체한다. 새 drop-in 을 만들어 넣으면 거기 있던 다른 설정(예: nvidia 런타임
# 등록)이 사라진다 — 2026-08-10 k8s-worker1 에서 실제로 그렇게 만들어 device-plugin 이
# "no runtime for nvidia" 로 뜨지 못했다. 그래서 새 파일을 만들지 않는다.
TARGET=$CFG
if grep -qE '^[[:space:]]*imports[[:space:]]*=' "$CFG" && [ -d "$DIR/conf.d" ]; then
  for f in $(ls "$DIR"/conf.d/*.toml 2>/dev/null | sort); do
    if grep -qF "$SEC" "$f"; then TARGET=$f; fi
  done
fi
echo "CDI target: $TARGET"
cp "$TARGET" "$TARGET.bak.cdi"

if grep -qE '^[[:space:]]*enable_cdi[[:space:]]*=' "$TARGET"; then
  # 이미 키가 있으면 값만 바꾼다. 같은 테이블에 키가 두 번 있으면 TOML 파싱이
  # 실패해 containerd 가 아예 안 뜬다.
  sed -i -E 's/^([[:space:]]*)enable_cdi[[:space:]]*=.*/\1enable_cdi = true/' "$TARGET"
else
  # 공백을 걷어낸 줄이 대상 섹션과 정확히 같을 때만 그 뒤에 넣는다. 부분일치를
  # 쓰면 하위 섹션에 걸려 중복 섹션이 생기고 containerd 가 안 뜬다.
  awk -v sec="$SEC" '
{ line = $0; t = line; sub(/^[[:space:]]+/, "", t); sub(/[[:space:]]+$/, "", t); print line }
t == sec && !done {
  ind = line; sub(/[^[:space:]].*$/, "", ind)
  print ind "  enable_cdi = true"
  done = 1
}
' "$TARGET" > "$TARGET.new"
  if grep -q 'enable_cdi' "$TARGET.new"; then
    mv "$TARGET.new" "$TARGET"
  else
    rm -f "$TARGET.new"
    printf '\n%s\n  enable_cdi = true\n' "$SEC" >> "$TARGET"
  fi
fi

# spec 경로는 containerd 기본값이 이미 /etc/cdi 와 /var/run/cdi 다. 다시 쓰지 않는다 —
# 쓸수록 덮어쓰기 사고 면적만 넓어진다.

nsenter -t 1 -m -u -i -n -p -- systemctl restart containerd

# 적용을 확인한다. 실패하면 되돌리고 종료 코드로 드러낸다 — 조용한 미작동 금지.
if ! nsenter -t 1 -m -u -i -n -p -- containerd config dump 2>/dev/null |
       grep -q 'enable_cdi[[:space:]]*=[[:space:]]*true'; then
  echo "CDI 적용 실패 — 유효 설정이 여전히 꺼져 있다. 원복한다" >&2
  cp "$TARGET.bak.cdi" "$TARGET"
  nsenter -t 1 -m -u -i -n -p -- systemctl restart containerd
  exit 1
fi
echo "CDI enabled; containerd restarted"
`
	priv := true
	return corev1.Container{
		Name:            "enable-cdi",
		Image:           HostExecImage(),
		Command:         []string{"sh", "-c", script},
		SecurityContext: &corev1.SecurityContext{Privileged: &priv},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "host-etc-containerd", MountPath: "/host/etc/containerd"},
		},
	}
}

// nvidiaDRABlockReason 은 NVIDIA DRA 드라이버를 배포하면 안 되는 사유다. 배포해도
// 되면 빈 문자열이다.
//
// 이 벤더에만 게이트가 둘 붙는 이유는 우리 설계가 아니라 벤더 chart 구조다
// (2026-08-07 실측, chart v0.5.0-dev):
//
//  1. gpus.enabled=false 로 깔면 DaemonSet 도 DeviceClass 도 나오지 않는다.
//     RBAC 껍데기뿐이다. 즉 "깔되 광고는 device-plugin 이" 라는 상태가 없다.
//     chart 자신도 device-plugin 과의 공존을 막는다.
//  2. gpus.enabled 는 DaemonSet 하나에 하나라 노드 단위로 나눌 수 없다.
//
// 둘 다 조용히 반쪽 상태를 만드는 대신 거절로 드러낸다.
func nvidiaDRABlockReason(policy *npuv1alpha1.NPUClusterPolicy) string {
	if !policy.Spec.Nvidia.DRA.IsEnabled() {
		return ""
	}
	if policy.Spec.Nvidia.AdvertisedBy() != npuv1alpha1.AdvertiseByDRA {
		return "NVIDIA DRA 드라이버는 advertiseBy: dra 일 때만 배포한다. " +
			"벤더 chart 가 device-plugin 과의 공존을 막고, 광고를 끈 채로 깔면 " +
			"발행물 없는 껍데기만 남는다. advertiseBy 를 dra 로 바꾼 뒤 다시 켤 것"
	}
	if policy.Spec.Nvidia.AdvertiseByNodeSelector != nil {
		return "NVIDIA 는 노드 일부만 DRA 로 넘길 수 없다. 벤더 chart 의 GPU 발행 스위치가 " +
			"DaemonSet 단위라 노드마다 다른 값을 줄 수 없다. " +
			"advertiseByNodeSelector 를 비워 전부 넘기거나 전부 device-plugin 으로 둘 것"
	}
	return ""
}

// draVendor 는 DRA 드라이버 배포 대상 벤더 하나다.
type draVendor struct {
	name string
	spec *npuv1alpha1.DRASpec
	// render 가 nil 이면 이 벤더는 아직 렌더러가 없다. 켜도 배포하지 않고 사유를 남긴다.
	render func(*npuv1alpha1.NPUClusterPolicy) []client.Object
	// driverName 은 발행물 관측에 쓰는 spec.driver 값이다. render 가 nil 이면 빈 값.
	driverName string
	// dsName 은 배포 여부 판정에 쓰는 DaemonSet 이름이다. render 가 nil 이면 빈 값.
	dsName string
}

// draVendors 는 이 정책이 선언한 DRA 축을 벤더 목록으로 편다. 선언되지 않은
// 벤더는 목록에 넣지 않는다 — status 를 쓰지 않는 것과 Absent 를 쓰는 것은 다르다.
func draVendors(policy *npuv1alpha1.NPUClusterPolicy) []draVendor {
	out := make([]draVendor, 0, 3)
	if s := policy.Spec.Furiosa.Rngd.DRA; s != nil {
		out = append(out, draVendor{
			name: vendorFuriosa, spec: s,
			render: renderFuriosaDRADriver, driverName: draDriverNameFuriosa,
			dsName: draDSNameFuriosa,
		})
	}
	if s := policy.Spec.Nvidia.DRA; s != nil {
		out = append(out, draVendor{
			name: vendorNvidia, spec: s,
			render: renderNvidiaDRADriver, driverName: draDriverNameNvidia,
			dsName: draDSNameNvidia,
		})
	}
	if s := policy.Spec.Tenstorrent.DRA; s != nil {
		out = append(out, draVendor{name: vendorTenstorrent, spec: s})
	}
	return out
}

// ensureDRADriver 는 벤더별 dra.enabled 에 따라 드라이버를 배포/갱신하거나 회수하고,
// 발행물을 관측해 status 에 기록한다. 광고 주체는 건드리지 않는다 — 드라이버가
// 없어도 device-plugin 광고는 그대로 살아 있어야 한다.
func (r *NPUClusterPolicyReconciler) ensureDRADriver(
	ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy,
) error {
	vendors := draVendors(policy)
	out := make([]npuv1alpha1.DRADriverStatus, 0, len(vendors))
	for _, v := range vendors {
		st, err := r.reconcileOneDRADriver(ctx, policy, v)
		if err != nil {
			return err
		}
		out = append(out, st)
	}
	policy.Status.DRADrivers = out
	return nil
}

// reconcileOneDRADriver 는 벤더 하나를 맞춘다. 판정 순서가 곧 안전 순서다 —
// 끄기가 먼저이고, 렌더러 없는 벤더는 배포 시도조차 하지 않는다.
func (r *NPUClusterPolicyReconciler) reconcileOneDRADriver(
	ctx context.Context, policy *npuv1alpha1.NPUClusterPolicy, v draVendor,
) (npuv1alpha1.DRADriverStatus, error) {
	st := npuv1alpha1.DRADriverStatus{Vendor: v.name, Phase: npuv1alpha1.DRAPhaseAbsent}

	if !v.spec.IsEnabled() {
		// 배포한 적이 없으면 지울 것도 없다. DaemonSet 존재 여부를 먼저 보는 이유는
		// 매 reconcile 마다 삭제를 쏘면 API 소음이 되고, 한 번도 켠 적 없는 클러스터에서도
		// 삭제 권한을 요구하게 되기 때문이다.
		deployed, err := r.draDeployed(ctx, v)
		if err != nil || !deployed {
			return st, err
		}
		return st, r.deleteDRAObjects(ctx, v.render(policy))
	}

	if v.name == vendorNvidia {
		if reason := nvidiaDRABlockReason(policy); reason != "" {
			st.Phase = npuv1alpha1.DRAPhaseBlocked
			st.Message = reason
			return st, nil
		}
	}

	if v.render == nil {
		st.Phase = npuv1alpha1.DRAPhaseBlocked
		st.Message = v.name + " 는 아직 DRA 드라이버 렌더러가 없다 — 배포하지 않는다"
		return st, nil
	}

	objs := v.render(policy)
	for _, o := range objs {
		if ds, ok := o.(*appsv1.DaemonSet); ok {
			setOwnerAnnotation(&ds.ObjectMeta, policy)
			// control-plane 노드에는 올리지 않는다. 그 노드에도 벤더 라벨이 붙어 있을 수
			// 있고(2026-08-07 실측: k8s-master 에 nvidia.present), 장치가 없으니 벤더
			// prestart 가 init 에서 멈춘 채 남는다. 다른 operand 와 같은 규약이다.
			applyControlPlaneExclusion(&ds.Spec.Template.Spec)
			applyImagePullSecrets(&ds.Spec.Template.Spec, policy.Spec.ImagePullSecrets)
			if err := r.createOrUpdateDS(ctx, ds); err != nil {
				return st, err
			}
			continue
		}
		if err := r.applyDRAObject(ctx, o); err != nil {
			return st, err
		}
	}

	st.DriverName = v.driverName
	nodes, err := r.countPublishingNodes(ctx, v.driverName)
	if err != nil {
		return st, err
	}
	st.PublishedNodes = nodes
	// 발행물이 완료 기준이다. 파드가 Running 이어도 ResourceSlice 가 없으면
	// 사용자는 이 드라이버로 아무것도 요청할 수 없다.
	if nodes == 0 {
		st.Phase = npuv1alpha1.DRAPhaseInstalling
		st.Message = "드라이버는 배포됐으나 ResourceSlice 발행이 아직 관측되지 않음"
		return st, nil
	}
	st.Phase = npuv1alpha1.DRAPhaseReady
	return st, nil
}

// draDeployed 는 이 벤더의 드라이버가 배포돼 있는지를 DaemonSet 존재로 판정한다.
func (r *NPUClusterPolicyReconciler) draDeployed(ctx context.Context, v draVendor) (bool, error) {
	if v.render == nil {
		return false, nil
	}
	var ds appsv1.DaemonSet
	err := r.Get(ctx, types.NamespacedName{Name: v.dsName, Namespace: draNamespace}, &ds)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// applyDRAObject 는 오브젝트 하나를 만들거나 갱신한다.
func (r *NPUClusterPolicyReconciler) applyDRAObject(ctx context.Context, desired client.Object) error {
	cur, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("오브젝트 복제 실패: %T", desired)
	}
	key := client.ObjectKeyFromObject(desired)
	if err := r.Get(ctx, key, cur); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}
	desired.SetResourceVersion(cur.GetResourceVersion())
	return r.Update(ctx, desired)
}

// deleteDRAObjects 는 렌더된 오브젝트를 지운다. 이미 없으면 조용히 넘어간다.
func (r *NPUClusterPolicyReconciler) deleteDRAObjects(ctx context.Context, objs []client.Object) error {
	for _, o := range objs {
		if err := r.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// countPublishingNodes 는 이 드라이버가 ResourceSlice 를 발행 중인 노드 수를 센다.
// 같은 노드가 여러 slice 를 내도 한 번만 센다.
func (r *NPUClusterPolicyReconciler) countPublishingNodes(ctx context.Context, driver string) (int32, error) {
	if driver == "" {
		return 0, nil
	}
	var slices resourcev1.ResourceSliceList
	if err := r.List(ctx, &slices); err != nil {
		// 클러스터에 이 API 가 없으면 발행물도 없다. 목록 실패로 reconcile 을 죽이지 않는다.
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return 0, nil
		}
		return 0, err
	}
	seen := map[string]bool{}
	for i := range slices.Items {
		s := &slices.Items[i]
		if s.Spec.Driver != driver || s.Spec.NodeName == nil {
			continue
		}
		seen[*s.Spec.NodeName] = true
	}
	return int32(len(seen)), nil //nolint:gosec // 노드 수가 int32 를 넘을 일은 없다
}

const (
	// draDSNameNvidia 는 배포되는 DaemonSet 이름이다.
	draDSNameNvidia = "kcloud-nvidia-dra-driver"
	// draDriverNameNvidia 는 발행물의 spec.driver 값이다. DeviceClass 셋이 모두
	// 이 driver 를 가리키므로 발행 관측은 이 하나로 센다.
	draDriverNameNvidia = "gpu.nvidia.com"
	// draDriverImageNvidiaDefault 는 spec 미지정 시 기본 이미지다.
	draDriverImageNvidiaDefault = "registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v0.4.0"
	// draKubeletPluginArgsNvidia 는 chart 가 주는 실행 스크립트다. MASK 처리 뒤
	// 플러그인을 띄운다. 그대로 옮긴다 — 우리가 줄이면 벤더 동작이 바뀐다.
	draKubeletPluginArgsNvidia = `if [ "${MASK_NVIDIA_DRIVER_PARAMS}" = "true" ]; then
  cp /proc/driver/nvidia/params /root/gpu-params
  sed -i 's/^ModifyDeviceFiles: 1$/ModifyDeviceFiles: 0/' /root/gpu-params
  mount --bind /root/gpu-params /proc/driver/nvidia/params
fi
gpu-kubelet-plugin -v $(LOG_VERBOSITY)`
)

// renderNvidiaDRADriver 는 NVIDIA DRA 드라이버 오브젝트를 만든다.
// ServiceAccount · ClusterRole · ClusterRoleBinding · DaemonSet · DeviceClass 3종.
//
// 벤더 chart(v0.5.0-dev)에서 다중 노드 NVLink 축은 통째로 뺀다 — GPU 노드가 하나뿐이라
// 쓸 데가 없고, chart 는 그 값을 꺼도 관련 오브젝트를 남긴다. namespace Role 도 그
// 축 전용(computedomaincliques)이라 함께 뺀다.
func renderNvidiaDRADriver(policy *npuv1alpha1.NPUClusterPolicy) []client.Object {
	spec := policy.Spec.Nvidia.DRA
	labels := map[string]string{
		"app.kubernetes.io/name":      draDSNameNvidia,
		"app.kubernetes.io/component": "dra-driver",
	}

	sa := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: draDSNameNvidia, Namespace: draNamespace, Labels: labels},
	}

	// chart 실측 5줄에서 resource.nvidia.com(다중 노드 NVLink) 하나를 뺀 넷이다.
	cr := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: draDSNameNvidia, Labels: labels},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceclaims"},
				Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"resource.k8s.io"}, Resources: []string{"resourceslices"},
				Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"nodes"},
				Verbs: []string{"get", "list", "watch", "update", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"pods"},
				Verbs: []string{"get", "list", "watch"}},
		},
	}

	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: draDSNameNvidia, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: draDSNameNvidia},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: draDSNameNvidia, Namespace: draNamespace}},
	}

	out := []client.Object{sa, cr, crb, renderNvidiaDRADaemonSet(policy, spec, labels)}
	for _, dc := range []struct{ name, kind string }{
		{"gpu.nvidia.com", "gpu"},
		{"mig.nvidia.com", "mig"},
		{"vfio.gpu.nvidia.com", "vfio"},
	} {
		out = append(out, &resourcev1.DeviceClass{
			TypeMeta:   metav1.TypeMeta{APIVersion: "resource.k8s.io/v1", Kind: "DeviceClass"},
			ObjectMeta: metav1.ObjectMeta{Name: dc.name, Labels: labels},
			Spec: resourcev1.DeviceClassSpec{
				Selectors: []resourcev1.DeviceSelector{{
					CEL: &resourcev1.CELDeviceSelector{
						Expression: "device.driver == '" + draDriverNameNvidia +
							"' && device.attributes['" + draDriverNameNvidia + "'].type == '" + dc.kind + "'",
					},
				}},
			},
		})
	}
	return out
}

// renderNvidiaDRADaemonSet 은 kubelet-plugin DaemonSet 을 만든다. 컨테이너·볼륨·환경변수는
// chart 원본을 그대로 쓰고 스케줄 조건과 init container 순서만 우리 것으로 바꾼다.
func renderNvidiaDRADaemonSet(policy *npuv1alpha1.NPUClusterPolicy, spec *npuv1alpha1.DRASpec,
	labels map[string]string) *appsv1.DaemonSet {
	image := draDriverImageNvidiaDefault
	if spec != nil && spec.Image != "" {
		image = spec.Image
	}
	args := []string{draKubeletPluginArgsNvidia}
	if spec != nil && len(spec.Args) > 0 {
		args = spec.Args
	}
	env := []corev1.EnvVar{
		{Name: "HTTP_ENDPOINT", Value: ":8080"},
		{Name: "METRICS_PATH", Value: "/metrics"},
		{Name: "LOG_VERBOSITY", Value: "4"},
		{Name: "MASK_NVIDIA_DRIVER_PARAMS", Value: ""},
		{Name: "NVIDIA_DRIVER_ROOT", Value: "/"},
		{Name: "NVIDIA_VISIBLE_DEVICES", Value: "void"},
		{Name: "CDI_ROOT", Value: draCDIRoot},
		{Name: "NVIDIA_MIG_CONFIG_DEVICES", Value: "all"},
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
		{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		}},
		{Name: "IMAGE_NAME", Value: image},
		{Name: "IMAGE_PULL_POLICY", Value: string(corev1.PullIfNotPresent)},
		{Name: "KUBELET_REGISTRAR_DIRECTORY_PATH", Value: "/var/lib/kubelet/plugins_registry"},
		{Name: "KUBELET_PLUGINS_DIRECTORY_PATH", Value: "/var/lib/kubelet/plugins"},
		{Name: "HEALTHCHECK_PORT", Value: "51516"},
	}
	if spec != nil {
		env = mergeEnv(env, spec.Env)
	}

	priv := true
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	bidirectional := corev1.MountPropagationBidirectional
	hostToContainer := corev1.MountPropagationHostToContainer
	hostVol := func(name, path string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &dirOrCreate},
		}}
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name: draDSNameNvidia, Namespace: draNamespace, Labels: labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// chart affinity 는 NFD 4종과 외부 operator 라벨 1종의 OR 이다.
					// 우리는 둘 다 안 쓰므로 자립 라벨로 갈아 끼운다.
					NodeSelector:       map[string]string{"kcloud.ai/nvidia.present": labelValueTrue},
					Affinity:           nil,
					ServiceAccountName: draDSNameNvidia,
					PriorityClassName:  "system-node-critical",
					// CDI init container 가 nsenter -t 1 로 호스트 containerd 를 재시작한다.
					HostPID:     true,
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					InitContainers: []corev1.Container{
						// CDI 활성화가 반드시 먼저다. 벤더 prestart 가 CDI 없는 상태에서 돌면 안 된다.
						renderCDIInitContainer(policy),
						{
							Name:            "vendor-prestart",
							Image:           image,
							Command:         []string{"bash", "/usr/bin/kubelet-plugin-prestart.sh"},
							SecurityContext: &corev1.SecurityContext{Privileged: &priv},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "driver-root-parent", MountPath: "/driver-root-parent"},
							},
						},
					},
					Containers: []corev1.Container{{
						Name:            "gpus",
						Image:           image,
						Command:         []string{"bash", "-c"},
						Args:            args,
						Env:             env,
						SecurityContext: &corev1.SecurityContext{Privileged: &priv},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "plugins-registry", MountPath: "/var/lib/kubelet/plugins_registry"},
							{Name: "plugins", MountPath: "/var/lib/kubelet/plugins", MountPropagation: &bidirectional},
							{Name: "cdi", MountPath: draCDIRoot},
							{Name: "driver-root", MountPath: "/driver-root", MountPropagation: &hostToContainer},
						},
					}},
					Volumes: []corev1.Volume{
						hostVol("plugins-registry", "/var/lib/kubelet/plugins_registry"),
						hostVol("plugins", "/var/lib/kubelet/plugins"),
						hostVol("cdi", draCDIRoot),
						hostVol("driver-root-parent", "/"),
						hostVol("driver-root", "/"),
						hostVol("host-etc-containerd", "/etc/containerd"),
					},
				},
			},
		},
	}
}
