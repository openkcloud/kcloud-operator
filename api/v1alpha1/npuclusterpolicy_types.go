/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// ============================================================
// npuclusterpolicy_types.go: NPUClusterPolicy CRD 타입 정의
// 상세: Detector/Nvidia/Furiosa/Rebellions/Tenstorrent vendor spec 포함
// 생성일: 2025-01-01 | 수정일: 2026-08-12
// ============================================================

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type DetectorSpec struct {
	Image string `json:"image,omitempty"`
}

type FuriosaSpec struct {
	// 광고 주체 축(devicePlugin|dra).
	AdvertiseSpec     `json:",inline"`
	Enabled           bool              `json:"enabled"`
	DevicePluginImage string            `json:"devicePluginImage"`
	ConfigMapName     string            `json:"configMapName,omitempty"`
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	// ExcludeNodeSelector 는 이 벤더의 배포 대상에서 뺄 노드를 고른다.
	// nodeSelector 는 양성 선택이라 노드 하나를 빼려면 나머지 전부에 라벨을 붙여야
	// 한다 — 노드가 늘수록 나빠진다. 이 축은 그 반대 방향이다.
	// control-plane 배제는 이 필드와 무관하게 항상 걸린다(정책으로 끌 수 없다).
	// +optional
	ExcludeNodeSelector *metav1.LabelSelector `json:"excludeNodeSelector,omitempty"`
	Rngd                RngdSpec              `json:"rngd,omitempty"`
	// DRA 는 이 벤더의 DRA 드라이버 배포 여부다. 광고 주체(AdvertiseBy)와 독립이다.
	// +optional
	DRA *DRASpec `json:"dra,omitempty"`
	// Unified: true 면 Warboy/RNGD 를 두 개의 개별 DaemonSet 대신 하나의 통합 DaemonSet 으로
	// 배포한다(A' 방안 — 동봉 바이너리 + PCI 감지 exec). 기본 false = 기존 2-DS 경로(fallback).
	// 통합 DS 는 자립 라벨(kcloud.ai/furiosa-family.present, node-manager 부여)로 양 노드에 스케줄되며,
	// 리소스명(beta.furiosa.ai/npu / furiosa.ai/rngd)은 각 바이너리가 광고하므로 불변.
	// +optional
	Unified bool `json:"unified,omitempty"`
	// UnifiedDevicePluginImage: 통합 DS 가 사용할 동봉 이미지. Unified=true 일 때만 사용.
	// +optional
	UnifiedDevicePluginImage string `json:"unifiedDevicePluginImage,omitempty"`
}

// RngdSpec defines the RNGD-specific (Furiosa RNGD NPU) device plugin configuration.
// Backward-compatible: all fields are omitempty; omit the whole block to keep legacy behavior.
type RngdSpec struct {
	// 광고 주체 축(devicePlugin|dra).
	AdvertiseSpec     `json:",inline"`
	Enabled           bool              `json:"enabled,omitempty"`
	DevicePluginImage string            `json:"devicePluginImage,omitempty"`
	ResourceName      string            `json:"resourceName,omitempty"` // default "furiosa.ai/rngd"
	ConfigMapName     string            `json:"configMapName,omitempty"`
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	// ExcludeNodeSelector 는 이 벤더의 배포 대상에서 뺄 노드를 고른다. FuriosaSpec.ExcludeNodeSelector 참조.
	// +optional
	ExcludeNodeSelector *metav1.LabelSelector `json:"excludeNodeSelector,omitempty"`
	// PartitionPolicy: "none" (default, 1 instance/card), "single-core" (8), "dual-core" (4), "quad-core" (2).
	// Furiosa libfuriosa-kubernetes PartitioningPolicy 와 1:1 매핑.
	// +kubebuilder:validation:Enum=none;single-core;dual-core;quad-core
	// +optional
	PartitionPolicy string `json:"partitionPolicy,omitempty"`
	// DRA 는 이 벤더의 DRA 드라이버 배포 여부다. 광고 주체(AdvertiseBy)와 독립이다.
	// +optional
	DRA *DRASpec `json:"dra,omitempty"`
}

type NvidiaSpec struct {
	// 광고 주체 축(devicePlugin|dra).
	AdvertiseSpec     `json:",inline"`
	Enabled           bool              `json:"enabled"`
	DevicePluginImage string            `json:"devicePluginImage"`
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	// ExcludeNodeSelector 는 이 벤더의 배포 대상에서 뺄 노드를 고른다. FuriosaSpec.ExcludeNodeSelector 참조.
	// +optional
	ExcludeNodeSelector *metav1.LabelSelector `json:"excludeNodeSelector,omitempty"`
	// DcgmExporter 는 NVIDIA GPU 텔레메트리(온도/사용률/메모리/전력) exporter 설정이다.
	// NPU 4벤더는 node-manager 가 hwmon 으로 수집하지만 NVIDIA 드라이버는 hwmon 을 등록하지
	// 않아 NVML 경로가 필요하므로, upstream dcgm-exporter 를 operand 로 배포한다.
	// 미지정(nil) 또는 Enabled=false 면 DS 를 만들지 않고 기존 것이 있으면 제거한다.
	// +optional
	DcgmExporter *DcgmExporterSpec `json:"dcgmExporter,omitempty"`
	// DRA 는 이 벤더의 DRA 드라이버 배포 여부다. 광고 주체(AdvertiseBy)와 독립이다.
	// +optional
	DRA *DRASpec `json:"dra,omitempty"`
}

// DcgmExporterSpec 은 NVIDIA dcgm-exporter DaemonSet 배포 설정이다.
type DcgmExporterSpec struct {
	// Enabled=false(기본) 면 배포하지 않는다.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Image 미지정 시 nvcr.io upstream 기본값을 사용한다(air-gap 환경은 미러 경로를 지정).
	// +optional
	Image string `json:"image,omitempty"`
}

// RebellionsSpec defines the Rebellions ATOM+ device plugin configuration.
// Backward-compatible: all fields are omitempty; omit the whole block to keep legacy behavior.
type RebellionsSpec struct {
	Enabled           bool              `json:"enabled,omitempty"`
	DevicePluginImage string            `json:"devicePluginImage"`
	ResourceName      string            `json:"resourceName,omitempty"`   // default "ATOM"
	ResourcePrefix    string            `json:"resourcePrefix,omitempty"` // default "rebellions.ai"
	Namespace         string            `json:"namespace,omitempty"`      // default "rbln-system"
	ConfigMapName     string            `json:"configMapName,omitempty"`  // default "rbln-device-plugin-config"
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	// ExcludeNodeSelector 는 이 벤더의 배포 대상에서 뺄 노드를 고른다. FuriosaSpec.ExcludeNodeSelector 참조.
	// +optional
	ExcludeNodeSelector *metav1.LabelSelector `json:"excludeNodeSelector,omitempty"`
}

// TenstorrentSpec defines the Tenstorrent Blackhole NPU device plugin configuration.
// Backward-compatible: all fields are omitempty; omit the whole block to keep legacy behavior.
type TenstorrentSpec struct {
	// 광고 주체 축(devicePlugin|dra).
	AdvertiseSpec     `json:",inline"`
	Enabled           bool              `json:"enabled,omitempty"`
	DevicePluginImage string            `json:"devicePluginImage,omitempty"`
	ResourceName      string            `json:"resourceName,omitempty"` // default "tenstorrent.com/blackhole"
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	// ExcludeNodeSelector 는 이 벤더의 배포 대상에서 뺄 노드를 고른다. FuriosaSpec.ExcludeNodeSelector 참조.
	// +optional
	ExcludeNodeSelector *metav1.LabelSelector `json:"excludeNodeSelector,omitempty"`
	// DRA 는 이 벤더의 DRA 드라이버 배포 여부다. 광고 주체(AdvertiseBy)와 독립이다.
	// +optional
	DRA *DRASpec `json:"dra,omitempty"`
}

// NPUClusterPolicySpec defines the desired state of NPUClusterPolicy.
type NPUClusterPolicySpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Detector    *DetectorSpec   `json:"detector,omitempty"`
	Nvidia      NvidiaSpec      `json:"nvidia"`
	Furiosa     FuriosaSpec     `json:"furiosa"`
	Rebellions  RebellionsSpec  `json:"rebellions,omitempty"`
	Tenstorrent TenstorrentSpec `json:"tenstorrent,omitempty"`

	// ImagePullSecrets 는 operator 가 런타임 생성하는 모든 자식 DaemonSet
	// (detector/device-plugin) pod spec 및 생성 ServiceAccount 에 부착되는
	// pull secret 목록입니다. helm `imagePullSecrets` 값이 CR 을 통해 전파됩니다.
	// 미지정 시 빈 목록 — 노드레벨(containerd) 인증 경로를 유지하는 하위호환 동작입니다.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// NPUClusterPolicyStatus defines the observed state of NPUClusterPolicy.
type NPUClusterPolicyStatus struct {
	Phase      string             `json:"phase,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// AdvertiseSwitch 는 광고 주체 전환이 노드마다 어떻게 됐는가다. 거절된 노드가 왜
	// 거절됐는지 여기서 읽는다 — 조용히 안 되는 것과 이유 있는 거절은 다르다.
	// +optional
	AdvertiseSwitch []AdvertiseSwitchStatus `json:"advertiseSwitch,omitempty"`
	// DRADrivers 는 벤더별 DRA 드라이버 배포 상태다.
	// +optional
	DRADrivers []DRADriverStatus `json:"draDrivers,omitempty"`
}

// AdvertiseSwitchStatus 는 노드 하나에 대한 전환 판정이다.
type AdvertiseSwitchStatus struct {
	Vendor string `json:"vendor"`
	Node   string `json:"node"`
	// Applied 는 이 노드가 실제로 DRA 소유로 넘어갔는가다.
	Applied bool `json:"applied"`
	// Reason 은 넘어가지 못한 사유다(넘어갔으면 비어 있다).
	// +optional
	Reason string `json:"reason,omitempty"`
	// BlockingPods 는 장치를 쥐고 있어 전환을 막은 pod 들이다(namespace/name).
	// +optional
	BlockingPods []string `json:"blockingPods,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NPUClusterPolicy is the Schema for the npuclusterpolicies API.
type NPUClusterPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NPUClusterPolicySpec   `json:"spec,omitempty"`
	Status NPUClusterPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NPUClusterPolicyList contains a list of NPUClusterPolicy.
type NPUClusterPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NPUClusterPolicy `json:"items"`
}

// 광고 주체 — 같은 하드웨어를 device-plugin 과 DRA 가 동시에 광고하면 스케줄러가 장치 하나를
// 독립 자원 둘로 본다. 어느 쪽이 광고할지를 벤더(또는 벤더×노드) 단위로 선언한다.
const (
	AdvertiseByDevicePlugin = "devicePlugin"
	AdvertiseByDRA          = "dra"
)

// AdvertiseSpec 은 이 벤더 장치를 누가 광고하는가다. 벤더 블록에 inline 으로 들어간다.
type AdvertiseSpec struct {
	// AdvertiseBy 는 devicePlugin(기본) 또는 dra 다. dra 면 operator 가 대상 노드에
	// DRAOwnedNodeLabel 을 붙이고, 그 노드에서 이 벤더의 device-plugin DaemonSet 이 물러난다.
	// +kubebuilder:validation:Enum=devicePlugin;dra
	// +optional
	AdvertiseBy string `json:"advertiseBy,omitempty"`
	// AdvertiseByNodeSelector 는 dra 로 넘길 노드를 좁힌다. 미지정이면 그 벤더 장치가 있는
	// 모든 노드가 대상이다. DRA 드라이버가 일부 노드에만 깔린 과도기를 위한 축이다.
	// +optional
	AdvertiseByNodeSelector *metav1.LabelSelector `json:"advertiseByNodeSelector,omitempty"`
}

// AdvertisedBy 는 미지정을 devicePlugin 으로 해석한다. 이 필드를 모르는 기존 정책이
// 지금까지와 똑같이 동작해야 한다.
func (a AdvertiseSpec) AdvertisedBy() string {
	if a.AdvertiseBy == "" {
		return AdvertiseByDevicePlugin
	}
	return a.AdvertiseBy
}

// DRASpec 은 이 벤더의 DRA 드라이버를 operator 가 배포할 것인가다. 벤더 블록에
// 포인터로 들어간다 — nil 이 기본이어야 이 필드를 모르는 기존 정책이 지금까지와
// 똑같이 동작한다.
type DRASpec struct {
	// Enabled 는 이 벤더의 DRA 드라이버를 배포할 것인가다. 기본 false.
	// 켜도 광고 주체는 바뀌지 않는다 — 그건 AdvertiseBy 의 몫이다.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Image 는 드라이버 이미지를 덮는다. 비우면 렌더러 기본값.
	// +optional
	Image string `json:"image,omitempty"`
	// Args 는 드라이버 컨테이너 인자를 덮는다. 비우면 렌더러 기본값.
	// +optional
	Args []string `json:"args,omitempty"`
	// Env 는 드라이버 컨테이너 환경변수에 더한다. 같은 이름이면 이쪽이 이긴다.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
}

// IsEnabled 는 nil 수신자를 꺼짐으로 읽는다. 호출부마다 nil 검사를 반복하지 않기 위함이다.
func (d *DRASpec) IsEnabled() bool {
	return d != nil && d.Enabled
}

// DRA 드라이버 배포 상태. Ready 는 "파드가 떴다" 가 아니라 "발행물이 관측된다" 다 —
// 파드가 Running 이어도 ResourceSlice 가 없으면 DRA 는 쓸 수 없다.
const (
	DRAPhaseAbsent     = "Absent"
	DRAPhaseBlocked    = "Blocked"
	DRAPhaseInstalling = "Installing"
	DRAPhaseReady      = "Ready"
)

// DRADriverStatus 는 벤더 하나의 DRA 드라이버 배포 상태다.
type DRADriverStatus struct {
	Vendor string `json:"vendor"`
	// Phase 는 Absent | Blocked | Installing | Ready 다.
	Phase string `json:"phase"`
	// DriverName 은 발행물의 spec.driver 값이다(예: npu.furiosa.ai). 미확인이면 빈 값.
	// +optional
	DriverName string `json:"driverName,omitempty"`
	// PublishedNodes 는 이 드라이버가 ResourceSlice 를 발행 중인 노드 수다.
	// +optional
	PublishedNodes int32 `json:"publishedNodes,omitempty"`
	// Message 는 Blocked·Installing 에서 무엇이 없는지다.
	// +optional
	Message string `json:"message,omitempty"`
}

// DRAOwnedNodeLabel 은 "이 노드의 이 벤더 장치는 DRA 가 광고한다" 를 나타내는 노드 라벨이다.
// operator 가 spec 에서 파생해 붙인다 — 사용자가 손으로 붙이는 라벨이 아니므로 spec 을
// 되돌리면 라벨도 사라진다.
func DRAOwnedNodeLabel(vendor string) string {
	return "kcloud.ai/" + vendor + ".dra-owned"
}
