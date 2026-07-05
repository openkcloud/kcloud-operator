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
// 생성일: 2025-01-01 | 수정일: 2026-07-15
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
	Enabled           bool              `json:"enabled"`
	DevicePluginImage string            `json:"devicePluginImage"`
	ConfigMapName     string            `json:"configMapName,omitempty"`
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	Rngd              RngdSpec          `json:"rngd,omitempty"`
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
	Enabled           bool              `json:"enabled,omitempty"`
	DevicePluginImage string            `json:"devicePluginImage,omitempty"`
	ResourceName      string            `json:"resourceName,omitempty"` // default "furiosa.ai/rngd"
	ConfigMapName     string            `json:"configMapName,omitempty"`
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	// PartitionPolicy: "none" (default, 1 instance/card), "single-core" (8), "dual-core" (4), "quad-core" (2).
	// Furiosa libfuriosa-kubernetes PartitioningPolicy 와 1:1 매핑.
	// +kubebuilder:validation:Enum=none;single-core;dual-core;quad-core
	// +optional
	PartitionPolicy string `json:"partitionPolicy,omitempty"`
}

type NvidiaSpec struct {
	Enabled           bool              `json:"enabled"`
	DevicePluginImage string            `json:"devicePluginImage"`
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	// DcgmExporter 는 NVIDIA GPU 텔레메트리(온도/사용률/메모리/전력) exporter 설정이다.
	// NPU 4벤더는 node-manager 가 hwmon 으로 수집하지만 NVIDIA 드라이버는 hwmon 을 등록하지
	// 않아 NVML 경로가 필요하므로, upstream dcgm-exporter 를 operand 로 배포한다.
	// 미지정(nil) 또는 Enabled=false 면 DS 를 만들지 않고 기존 것이 있으면 제거한다.
	// +optional
	DcgmExporter *DcgmExporterSpec `json:"dcgmExporter,omitempty"`
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
}

// TenstorrentSpec defines the Tenstorrent Blackhole NPU device plugin configuration.
// Backward-compatible: all fields are omitempty; omit the whole block to keep legacy behavior.
type TenstorrentSpec struct {
	Enabled           bool              `json:"enabled,omitempty"`
	DevicePluginImage string            `json:"devicePluginImage,omitempty"`
	ResourceName      string            `json:"resourceName,omitempty"` // default "tenstorrent.com/blackhole"
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
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
