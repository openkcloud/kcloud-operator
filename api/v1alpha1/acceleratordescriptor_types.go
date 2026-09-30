// ============================================================
// acceleratordescriptor_types.go: AcceleratorDescriptor CRD 타입
// 상세: 장치 하나를 backend 중립으로 기술한다. device-plugin 과 DRA 가 같은 장치를
//
//	다르게 모델링한 실측(Furiosa DP 4유닛 vs DRA 카드 1개)이 이 타입의 존재
//	이유다. 할당 단위와 식별자를 여기서 못 박아 두 backend 가 갈리지 않게 한다.
//
// 생성일: 2026-08-10 | 수정일: 2026-09-19
// ============================================================
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 할당 단위. 카드 1장이 단위인지, 하드웨어 파티션 1개가 단위인지.
const (
	AllocationUnitDevice    = "device"
	AllocationUnitPartition = "partition"
)

// 장치를 무엇으로 식별하는가.
const (
	IdentitySourcePCIAddress = "pciAddress"
	IdentitySourceUUID       = "uuid"
	IdentitySourceSerial     = "serial"
)

// 생성 대상 backend.
const (
	BackendDevicePlugin = "devicePlugin"
	BackendDRA          = "dra"
)

// DescriptorIdentity 는 장치를 되짚는 근거다. 재부팅·드라이버 교체를 건너 같은 장치를
// 같은 것으로 부를 수 있어야 backend 가 안정적으로 발행된다.
type DescriptorIdentity struct {
	// Source 는 식별자의 출처다.
	// +kubebuilder:validation:Enum=pciAddress;uuid;serial
	Source string `json:"source"`
	// StableAcrossReboot 이 true 면 재부팅 후에도 같은 값이어야 한다.
	// pciAddress 를 쓰면서 true 로 두는 것은 허용하되, 슬롯 변경 시 계약 위반이며
	// 그 책임은 descriptor 작성자가 진다.
	StableAcrossReboot bool `json:"stableAcrossReboot"`
}

// DescriptorCleanup 은 claim 반납 후 노드가 만족해야 할 조건이다.
type DescriptorCleanup struct {
	// RequiresDeviceReset 이 true 면 반납 후 장치 리셋이 필요하다.
	// 리셋 수단이 선언되지 않으면 backend 생성이 거절된다.
	// +optional
	RequiresDeviceReset bool `json:"requiresDeviceReset,omitempty"`
	// ResetCommand 는 리셋 수단이다. RequiresDeviceReset 이 true 면 필수.
	// +optional
	ResetCommand []string `json:"resetCommand,omitempty"`
}

// DescriptorSharing 은 SW 공유 선언이다. 현재 timeSliced 만 지원한다 — device-plugin 이
// 장치 1개를 replicas 개로 광고한다(격리 없음). 벤더 런타임이 동시 접근을 견딜 때만 안전하며,
// 그 판정은 internal/vendorcap 레지스트리가 한다(컨트롤러 게이트).
type DescriptorSharing struct {
	// +kubebuilder:validation:Enum=timeSliced
	Mode string `json:"mode"`
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=16
	Replicas int32 `json:"replicas,omitempty"`
}

// AcceleratorDescriptorSpec 은 장치 하나의 backend 중립 기술이다.
type AcceleratorDescriptorSpec struct {
	// Vendor 는 벤더 키다("nvidia"·"rngd"·"tenstorrent" 등, detector 의 vendorKey 와 같은 어휘).
	Vendor string `json:"vendor"`
	// Product 는 제품 키다("a30"·"blackhole-p150" 등).
	Product string `json:"product"`
	// AllocationUnit 은 할당의 최소 단위다.
	// +kubebuilder:validation:Enum=device;partition
	AllocationUnit string `json:"allocationUnit"`
	// Identity 는 장치 식별 근거다.
	Identity DescriptorIdentity `json:"identity"`
	// DeviceNodes 는 컨테이너에 주입할 /dev 경로 glob 목록이다. 비면 주입할 것이 없다는 뜻이라
	// 어느 backend 로도 갈 수 없다.
	// +kubebuilder:validation:MinItems=1
	DeviceNodes []string `json:"deviceNodes"`
	// Backends 는 이 descriptor 로 만들 수 있다고 주장하는 backend 목록이다.
	// 주장일 뿐이며 실제 가능 여부는 정적 검증기가 판정한다.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`
	// Cleanup 은 반납 후 조건이다.
	// +optional
	Cleanup DescriptorCleanup `json:"cleanup,omitempty"`
	// Sharing 은 SW 공유 선언이다(현재 timeSliced). 선언 시 devicePlugin backend 가 필수다.
	// +optional
	Sharing *DescriptorSharing `json:"sharing,omitempty"`
}

// AcceleratorDescriptorStatus 는 정적 검증 결과다.
type AcceleratorDescriptorStatus struct {
	// FeasibleBackends 는 검증기가 생성 가능하다고 판정한 backend 목록이다.
	// +optional
	FeasibleBackends []string `json:"feasibleBackends,omitempty"`
	// Refusals 는 거절된 backend 와 그 사유다. 거절을 조용히 삼키지 않는다.
	// +optional
	Refusals []DescriptorRefusal `json:"refusals,omitempty"`
	// +optional
	Conditions []Condition `json:"conditions,omitempty"`
	// ResolvedDevices 는 이 descriptor 의 vendor/product 에 맞는 NDR 카드들의 노드별 안정 ID 다.
	// +optional
	ResolvedDevices []NodeDeviceIDs `json:"resolvedDevices,omitempty"`
}

// NodeDeviceIDs 는 한 노드에서 이 descriptor 에 맞는 장치들의 안정 식별자 목록이다.
type NodeDeviceIDs struct {
	Node string   `json:"node"`
	IDs  []string `json:"ids,omitempty"`
}

// DescriptorRefusal 은 backend 하나가 왜 거절됐는지다.
type DescriptorRefusal struct {
	Backend string `json:"backend"`
	Reason  string `json:"reason"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=adesc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="VENDOR",type=string,JSONPath=".spec.vendor"
// +kubebuilder:printcolumn:name="UNIT",type=string,JSONPath=".spec.allocationUnit"
// +kubebuilder:printcolumn:name="FEASIBLE",type=string,JSONPath=".status.feasibleBackends"
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=".metadata.creationTimestamp"
type AcceleratorDescriptor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AcceleratorDescriptorSpec   `json:"spec,omitempty"`
	Status            AcceleratorDescriptorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AcceleratorDescriptorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AcceleratorDescriptor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AcceleratorDescriptor{}, &AcceleratorDescriptorList{})
}
