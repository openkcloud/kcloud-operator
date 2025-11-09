package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=dip
type DriverInstallPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DriverInstallPolicySpec `json:"spec,omitempty"`
}

type DriverInstallPolicySpec struct {
	// 예: "furiosa"
	Vendor string `json:"vendor"`
	// 예: "warboy"
	Model string `json:"model"`

	// 설치할 드라이버 사양
	Driver DriverSpec `json:"driver"`

	// 허용 커널 버전 패턴 (예: ["5.15.*","6.8.*"])
	KernelAllowlist []string `json:"kernelAllowlist,omitempty"`

	// 최소 containerd 버전 (semver)
	ContainerdMinVersion string `json:"containerdMinVersion,omitempty"`

	// 재부팅 전략
	// +kubebuilder:validation:Enum=Require;IfNeeded;Never
	RebootStrategy string `json:"rebootStrategy,omitempty"`
}

// DriverSpec은 드라이버 버전/이미지를 정의합니다.
type DriverSpec struct {
	// 예: "2.1.x"
	Version string `json:"version"`
	// 예: "REGISTRY/path/furiosa-driver-installer:2.1.x"
	Image string `json:"image"`
}

// +kubebuilder:object:root=true
type DriverInstallPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DriverInstallPolicy `json:"items"`
}
