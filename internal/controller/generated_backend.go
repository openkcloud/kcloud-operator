// ============================================================
// generated_backend.go: descriptor 를 device-plugin backend 배포물로 렌더
// 상세: 벤더 지식이 0인 일반 device-plugin 런타임에 줄 ConfigMap 과 DaemonSet 을
//       descriptor 하나로 조립한다. 벤더 하드코딩은 여기 없다 - vendor·product 는
//       전부 desc.Spec 에서 읽는다.
// 생성일: 2026-08-11 | 수정일: 2026-09-10 (1.28 라인: DeviceClass 를 타입 없이 렌더)
// ============================================================

package controller

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/descriptor"
)

// draCDIRoot 는 생성된 DRA 드라이버가 CDI 스펙을 쓰는 경로다. containerd 가 같은
// 경로를 읽어야 장치 주입이 성립한다.
const draCDIRoot = "/var/run/cdi"

// generatedDevicePluginNameFor 는 descriptor 하나가 만드는 device-plugin 배포물 이름이다.
// descriptor 이름을 넣어 여러 descriptor 의 배포물이 공존하게 한다.
func generatedDevicePluginNameFor(desc *npuv1alpha1.AcceleratorDescriptor) string {
	return "kcloud-generated-" + desc.Name + "-dp"
}

// renderGeneratedDevicePlugin 은 descriptor 하나를 device-plugin 배포물(ConfigMap +
// DaemonSet)로 렌더한다. cfgJSON 은 descriptor.CompileDevicePlugin 의 출력이다.
// 스케줄 조건은 기존 renderFuriosaDRADaemonSet 관례(자립 present 라벨, control-plane
// 미배제는 호출부 책임)를 따른다.
func renderGeneratedDevicePlugin(
	desc *npuv1alpha1.AcceleratorDescriptor, cfgJSON []byte, image, ns string,
) (*corev1.ConfigMap, *appsv1.DaemonSet) {
	name := generatedDevicePluginNameFor(desc)
	labels := map[string]string{
		"app.kubernetes.io/name":      name,
		"app.kubernetes.io/component": "generated-device-plugin",
	}

	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Data:       map[string]string{"backend.json": string(cfgJSON)},
	}

	priv := true
	// kubelet 소켓 디렉터리는 root 소유 0755 라 nonroot 로는 소켓을 열 수 없다.
	// privileged 만으로는 UID 가 바뀌지 않는다 - 2026-08-11 라이브에서 nonroot 이미지가
	// `bind: permission denied` 로 죽었다.
	var rootUID int64
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	hostVol := func(volName, path string) corev1.Volume {
		return corev1.Volume{Name: volName, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &dirOrCreate},
		}}
	}

	ds := &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector:       map[string]string{vendorPresentLabel(desc.Spec.Vendor): labelValueTrue},
					ServiceAccountName: "",
					PriorityClassName:  "system-node-critical",
					Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:            "plugin",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            []string{"--config=/etc/kcloud/backend.json", "--host-root=/host"},
						SecurityContext: &corev1.SecurityContext{Privileged: &priv, RunAsUser: &rootUID},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/etc/kcloud"},
							{Name: "dev", MountPath: "/host/dev"},
							{Name: "device-plugins", MountPath: "/var/lib/kubelet/device-plugins"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name}},
						}},
						hostVol("dev", "/dev"),
						hostVol("device-plugins", "/var/lib/kubelet/device-plugins"),
					},
				},
			},
		},
	}
	applyControlPlaneExclusion(&ds.Spec.Template.Spec)

	return cm, ds
}

// generatedDRADriverNameFor 는 descriptor 하나가 만드는 DRA 드라이버 배포물 이름이다.
func generatedDRADriverNameFor(desc *npuv1alpha1.AcceleratorDescriptor) string {
	return "kcloud-generated-" + desc.Name + "-dra"
}

// renderGeneratedDRADriver 는 descriptor 하나를 DRA 드라이버 배포물(ConfigMap + DaemonSet +
// DeviceClass)로 렌더한다. cfgJSON 은 descriptor.CompileDRA 의 출력이다. device-plugin 판과
// 같은 관례를 쓰되 볼륨은 kubelet plugin 등록·CDI 주입에 맞춘다 - 드라이버 자신이 kubelet 에
// 등록하고 ResourceSlice 를 발행하려면 registrar·plugins·cdi·dev 가 다 붙어야 한다.
func renderGeneratedDRADriver(
	desc *npuv1alpha1.AcceleratorDescriptor, cfgJSON []byte, image, ns string,
) (*corev1.ConfigMap, *appsv1.DaemonSet, *unstructured.Unstructured) {
	name := generatedDRADriverNameFor(desc)
	labels := map[string]string{
		"app.kubernetes.io/name":      name,
		"app.kubernetes.io/component": "generated-dra-driver",
	}

	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Data:       map[string]string{"backend.json": string(cfgJSON)},
	}

	priv := true
	// kubelet 소켓 디렉터리는 root 소유 0755 라 nonroot 로는 소켓을 열 수 없다.
	// privileged 만으로는 UID 가 바뀌지 않는다 - 2026-08-11 라이브에서 nonroot 이미지가
	// `bind: permission denied` 로 죽었다.
	var rootUID int64
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	hostVol := func(volName, path string) corev1.Volume {
		return corev1.Volume{Name: volName, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &dirOrCreate},
		}}
	}

	ds := &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{vendorPresentLabel(desc.Spec.Vendor): labelValueTrue},
					// DRA 런타임은 ResourceSlice 를 직접 발행하므로 API 권한이 필요하다.
					// device-plugin 판과 달리 전용 ServiceAccount 를 쓴다.
					ServiceAccountName: name,
					PriorityClassName:  "system-node-critical",
					Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:            "plugin",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env: []corev1.EnvVar{
							{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
							}},
							{Name: "CONFIG_PATH", Value: "/etc/kcloud/backend.json"},
							{Name: "CDI_ROOT", Value: draCDIRoot},
						},
						SecurityContext: &corev1.SecurityContext{Privileged: &priv, RunAsUser: &rootUID},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/etc/kcloud"},
							{Name: "dev", MountPath: "/host/dev"},
							{Name: "registrar", MountPath: "/var/lib/kubelet/plugins_registry"},
							{Name: "plugins", MountPath: "/var/lib/kubelet/plugins"},
							{Name: "cdi", MountPath: draCDIRoot},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name}},
						}},
						hostVol("dev", "/dev"),
						hostVol("registrar", "/var/lib/kubelet/plugins_registry"),
						hostVol("plugins", "/var/lib/kubelet/plugins"),
						hostVol("cdi", draCDIRoot),
					},
				},
			},
		},
	}
	applyControlPlaneExclusion(&ds.Spec.Template.Spec)

	driverName := descriptor.DriverNameFor(desc.Spec)
	// 이 라인(K8s 1.28)은 resource.k8s.io 를 링크하지 않으므로 DeviceClass 를 타입 없이
	// 조립한다. 필드 구성은 1.34 라인의 resourcev1.DeviceClass 와 같고, 직렬화 결과도 같다.
	dc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "resource.k8s.io/v1",
		"kind":       "DeviceClass",
		"metadata": map[string]any{
			"name":   driverName,
			"labels": toStringMap(labels),
		},
		"spec": map[string]any{
			"selectors": []any{map[string]any{
				"cel": map[string]any{
					"expression": "device.driver == '" + driverName + "'",
				},
			}},
		},
	}}

	return cm, ds, dc
}

// renderGeneratedDRARBAC 는 DRA 런타임이 ResourceSlice 를 발행하는 데 필요한 권한을 낸다.
// 배포물에 이것이 빠지면 드라이버가 뜨기는 하지만 발행에 실패하고 조용히 아무 장치도
// 나오지 않는다 - 2026-08-11 라이브에서 default ServiceAccount 로 떠서 실제로 그랬다.
// device-plugin 판은 API 를 보지 않으므로 대응물이 없다.
func renderGeneratedDRARBAC(desc *npuv1alpha1.AcceleratorDescriptor, ns string) (
	*corev1.ServiceAccount, *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding) {

	name := generatedDRADriverNameFor(desc)
	labels := map[string]string{
		"app.kubernetes.io/name":      name,
		"app.kubernetes.io/component": "generated-dra-driver",
	}

	sa := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
	}
	cr := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"resource.k8s.io"},
				Resources: []string{"resourceslices"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"resource.k8s.io"},
				Resources: []string{"resourceclaims"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"resource.k8s.io"},
				Resources: []string{"resourceclaims/status"},
				Verbs:     []string{"update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get"},
			},
		},
	}
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: ns}},
	}
	return sa, cr, crb
}

// RenderGeneratedBackends 는 descriptor 하나가 만드는 모든 배포물을 낸다.
// 컨트롤러가 아직 없으므로(2026-08-11 라이브 §6.3) 라이브 재현은 이 함수를 지나간다 -
// 손으로 쓴 YAML 을 쓰면 "descriptor 가 단일 출처" 라는 주장을 검증할 수 없다.
// 선언하지 않은 backend 의 배포물은 내지 않는다.
func RenderGeneratedBackends(
	desc *npuv1alpha1.AcceleratorDescriptor, deviceIDs []string,
	dpImage, draImage, ns string,
) ([]client.Object, error) {
	feasible, _ := descriptor.FeasibleBackends(desc.Spec)
	var out []client.Object

	for _, b := range feasible {
		switch b {
		case npuv1alpha1.BackendDevicePlugin:
			cfg, err := descriptor.CompileDevicePlugin(desc.Spec, deviceIDs)
			if err != nil {
				return nil, err
			}
			cm, ds := renderGeneratedDevicePlugin(desc, cfg, dpImage, ns)
			out = append(out, cm, ds)

		case npuv1alpha1.BackendDRA:
			cfg, err := descriptor.CompileDRA(desc.Spec, deviceIDs)
			if err != nil {
				return nil, err
			}
			sa, cr, crb := renderGeneratedDRARBAC(desc, ns)
			cm, ds, dc := renderGeneratedDRADriver(desc, cfg, draImage, ns)
			out = append(out, sa, cr, crb, cm, ds, dc)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("생성 가능한 backend 가 없다")
	}
	return out, nil
}

// toStringMap 은 라벨 맵을 unstructured 가 받는 any 맵으로 옮긴다.
func toStringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
