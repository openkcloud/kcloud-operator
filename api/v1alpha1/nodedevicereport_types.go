package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeDeviceReport Condition 타입 상수
const (
	// ConditionUpgradeInProgress는 노드에서 드라이버 업그레이드가 진행 중임을 나타냅니다.
	ConditionUpgradeInProgress = "UpgradeInProgress"
	// ConditionUpgradePending는 드라이버 업그레이드가 예약되었음을 나타냅니다.
	ConditionUpgradePending = "UpgradePending"
	// ConditionCordonedForUpgrade는 업그레이드를 위해 노드가 cordon 처리되었음을 나타냅니다.
	ConditionCordonedForUpgrade = "CordonedForUpgrade"
	// ConditionUpgradeSucceeded는 드라이버 업그레이드가 성공적으로 완료되었음을 나타냅니다.
	ConditionUpgradeSucceeded = "UpgradeSucceeded"
	// ConditionUpgradeFailed는 드라이버 업그레이드가 실패했음을 나타냅니다.
	ConditionUpgradeFailed = "UpgradeFailed"
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
	Vendor              string `json:"vendor,omitempty"` // "furiosa" | "nvidia" 등
	Model               string `json:"model,omitempty"`  // "warboy" 등
	Count               int32  `json:"count,omitempty"`
	DriverLoaded        bool   `json:"driverLoaded,omitempty"`
	DriverVersion       string `json:"driverVersion,omitempty"`
	DriverVersionDetail string `json:"driverVersionDetail,omitempty"` // 상세 버전 정보 (한 줄 요약)
	NeedsReboot         bool   `json:"needsReboot,omitempty"`
	// DriverBinding 은 해당 장치의 PCI 커널 드라이버 바인딩 상태입니다.
	// "nvidia"=드라이버 바인딩, "vfio-pci"=passthrough 바인딩, "none"=미바인딩(free). (a) passthrough 감지용.
	DriverBinding string `json:"driverBinding,omitempty"`
	// GFD(S3-3) 소스 필드 — node-agent 가 스캔 시 sysfs/PCI 에서 best-effort 추출합니다(미가용 시 공란).
	MemoryMiB         int64  `json:"memoryMiB,omitempty"`         // 디바이스 메모리(MiB)
	ComputeCapability string `json:"computeCapability,omitempty"` // NVIDIA sm_xx / 벤더 capability 문자열
	PCIeAddress       string `json:"pcieAddress,omitempty"`       // 대표 PCI 주소(라벨/디버깅용)
	// MigModeCurrent/Pending 은 NVIDIA MIG 모드 관측값이다(detector per-PCI, spec §14.1).
	// Disabled 만 apply 안전. Unknown(조회/파싱 실패) 또는 Enabled 는 apply 차단.
	MigModeCurrent string `json:"migModeCurrent,omitempty"`
	MigModePending string `json:"migModePending,omitempty"`
	// MigCurrentGeometry 는 현 GI 요약("1g.6gb x4"). ""/"disabled" = 없음. truthful Diff 진실 소스(§14.2).
	MigCurrentGeometry string `json:"migCurrentGeometry,omitempty"`
	// MigObservationError 는 관측 실패 사유(fail-closed 근거). 비어있지 않으면 apply 차단.
	MigObservationError string `json:"migObservationError,omitempty"`
	FirmwareVersion     string `json:"firmwareVersion,omitempty"` // 가용 시 펌웨어/BIOS 버전
	// MigLgipOutput 은 `nvidia-smi mig -lgip` 원문입니다(ACPP §4.2 MIG profile discovery 소스).
	// node-manager(detector) 가 채웁니다(Task 11b). NVIDIA 이외 벤더는 공란.
	MigLgipOutput string `json:"migLgipOutput,omitempty"`
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
	// PassthroughReserved 는 노드에 GPU 가 존재하며 전량 vfio-pci(passthrough)에 바인딩된 경우 true 입니다.
	// (a) 관리자가 passthrough 예약 노드를 식별하고 드라이버 설치 보류 판단을 보조합니다.
	PassthroughReserved bool `json:"passthroughReserved,omitempty"`
	// Validation 은 node-agent 가 상시/정기 실행하는 벤더별 검증 결과입니다(S2-3).
	Validation *ValidationStatus `json:"validation,omitempty"`
	// ObservedAt 은 node-agent 가 이 보고서를 마지막으로 갱신한 시각입니다. 비어 있으면 "관측 시각
	// 미상" 이고, health 판정은 그 상태를 Healthy 로 승격하지 않습니다(모르는 것을 아는 것처럼
	// 다루지 않는다 — R&D base v0.1 §10.3).
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// ValidationStatus 는 node-agent Validation 능력의 노드 검증 결과입니다(S2-3).
type ValidationStatus struct {
	// Passed 는 게이트 step 전부 통과 여부입니다(AC-1).
	Passed bool `json:"passed"`
	// LastRunTime 은 마지막 검증 시각입니다(node-agent 가 RFC3339 로 기록).
	LastRunTime metav1.Time `json:"lastRunTime,omitempty"`
	// Vendor 는 검증 대상 벤더입니다(멀티벤더 노드는 콤마 구분).
	Vendor string `json:"vendor,omitempty"`
	// Steps 는 step 별 결과입니다(AC-3: 실패 step 식별).
	Steps []ValidationStep `json:"steps,omitempty"`
}

// ValidationStep 은 S2-3 5단계 각각의 결과입니다.
type ValidationStep struct {
	// Name 은 step 이름입니다("driverModule"|"deviceNode"|"devicePlugin"|"runtime"|"sampleWorkload").
	// 멀티벤더 노드는 "<vendor>/<step>" 접두사가 붙습니다.
	Name string `json:"name"`
	// Passed 는 해당 step 통과 여부입니다.
	Passed bool `json:"passed"`
	// Message 는 실패/보조 사유입니다(운영자 디버깅).
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type NodeDeviceReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeDeviceReport `json:"items"`
}
