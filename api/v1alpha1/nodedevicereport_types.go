package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ndr
// +kubebuilder:subresource:status
type NodeDeviceReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeDeviceReportSpec   `json:"spec,omitempty"`
	Status NodeDeviceReportStatus `json:"status,omitempty"`
}

type NodeDeviceReportSpec struct {
	// 이 리포트가 속하는 노드 이름 (immutable 권장)
	NodeName string `json:"nodeName"`
}

type DeviceEntry struct {
	Vendor        string `json:"vendor,omitempty"` // "furiosa" | "nvidia" 등
	Model         string `json:"model,omitempty"`  // "warboy" 등
	Count         int32  `json:"count,omitempty"`
	DriverLoaded  bool   `json:"driverLoaded,omitempty"`
	DriverVersion string `json:"driverVersion,omitempty"`
	NeedsReboot   bool   `json:"needsReboot,omitempty"`
}

type Condition struct {
	Type    string `json:"type,omitempty"`
	Status  string `json:"status,omitempty"` // "True"|"False"|"Unknown"
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type NodeDeviceReportStatus struct {
	Devices    []DeviceEntry `json:"devices,omitempty"`
	Conditions []Condition   `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type NodeDeviceReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeDeviceReport `json:"items"`
}
