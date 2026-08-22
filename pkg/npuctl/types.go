// types.go: pkg/npuctl 관리 API 1단계 집약 상태 스키마
// 상세: kubectl-npu status --json 및 향후 internal/apiserver(2단계)가 그대로 재사용할
//
//	안정 필드명의 JSON 계약. 필드는 모두 omitempty로 두어 벤더/리소스가 없을 때 생략된다.
//
// 생성일: 2026-07-16 | 수정일: 2026-08-24
package npuctl

// 벤더 식별자 상수. NPUClusterPolicy.spec의 최상위 키와 1:1 대응하며(rngd는 furiosa의 서브벤더),
// status.go/control.go에서 반복 사용되는 리터럴을 한 곳에서 관리한다.
const (
	VendorNvidia      = "nvidia"
	VendorFuriosa     = "furiosa"
	VendorRngd        = "rngd"
	VendorRebellions  = "rebellions"
	VendorTenstorrent = "tenstorrent"
)

// ClusterStatus는 한 번의 조회로 얻는 노드×벤더 집약 상태입니다.
// (roadmap S5-3-①, .omc/plans/management-api.md §6 수용 기준)
type ClusterStatus struct {
	// Vendors는 NPUClusterPolicy.spec 기준 벤더별 enabled 여부 + 해당 벤더의 DriverInstallPolicy 목록입니다.
	Vendors []VendorStatus `json:"vendors,omitempty"`
	// Nodes는 노드별 디바이스(NDR)/allocatable/업그레이드 상태(DUS)입니다.
	Nodes []NodeStatus `json:"nodes,omitempty"`
	// ClusterPolicies는 조회된 NPUClusterPolicy 원본 목록(namespace/name/phase)입니다.
	ClusterPolicies []ClusterPolicyStatus `json:"clusterPolicies,omitempty"`
}

// VendorStatus는 벤더 하나의 활성화 여부와 소속 DriverInstallPolicy 목록입니다.
// Vendor 값은 NPUClusterPolicy.spec의 최상위 키(nvidia/furiosa/rebellions/tenstorrent)이며,
// furiosa의 하위 rngd는 별도 Name="rngd"(SubVendorOf="furiosa")로 표현합니다.
type VendorStatus struct {
	// Name은 벤더 식별자입니다: nvidia | furiosa | rngd | rebellions | tenstorrent.
	Name string `json:"name"`
	// SubVendorOf는 name이 하위 벤더(rngd 등)일 때 상위 벤더명입니다. 최상위 벤더는 비워둡니다.
	SubVendorOf string `json:"subVendorOf,omitempty"`
	// Enabled는 NPUClusterPolicy.spec.<vendor>.enabled 값입니다.
	Enabled bool `json:"enabled"`
	// DevicePluginImage는 NPUClusterPolicy.spec.<vendor>.devicePluginImage 입니다.
	DevicePluginImage string `json:"devicePluginImage,omitempty"`
	// DriverInstallPolicies는 spec.vendor(또는 rngd의 spec.model=="rngd")가 이 벤더와 일치하는 DIP 목록입니다.
	DriverInstallPolicies []DIPStatus `json:"driverInstallPolicies,omitempty"`
}

// DIPStatus는 DriverInstallPolicy의 관리툴 관심 필드입니다(목표 버전/검증 버전 화이트리스트 등).
type DIPStatus struct {
	Name             string   `json:"name"`
	Vendor           string   `json:"vendor"`
	Model            string   `json:"model,omitempty"`
	DesiredVersion   string   `json:"desiredVersion,omitempty"`
	Installer        string   `json:"installer,omitempty"`
	VerifiedVersions []string `json:"verifiedVersions,omitempty"`
	AllowDowngrade   bool     `json:"allowDowngrade,omitempty"`
	VersionSource    string   `json:"versionSource,omitempty"`
	AutoUpgrade      bool     `json:"autoUpgrade,omitempty"`
	DrainEnabled     bool     `json:"drainEnabled,omitempty"`
}

// NodeStatus는 노드 하나의 디바이스/allocatable/업그레이드 상태 요약입니다.
type NodeStatus struct {
	Name string `json:"name"`
	// Excluded 는 이 노드가 배포 대상에서 제외됐는지다. operator 가 붙인
	// kcloud.ai/excluded 라벨을 그대로 옮긴다 — 여기서 다시 판정하지 않는다.
	Excluded bool `json:"excluded,omitempty"`
	// ExcludedReason 은 고정 어휘다: control-plane | policy
	ExcludedReason string `json:"excludedReason,omitempty"`
	// Devices는 NodeDeviceReport.status.devices 입니다.
	Devices []DeviceStatus `json:"devices,omitempty"`
	// PassthroughReserved는 NodeDeviceReport.status.passthroughReserved 입니다(전량 vfio-pci 바인딩 시 true).
	PassthroughReserved bool `json:"passthroughReserved,omitempty"`
	// Allocatable은 node.status.allocatable 전체(quantity 문자열)입니다. 벤더별 리소스명이
	// (furiosa.ai/rngd, nvidia.com/gpu, tenstorrent.com/blackhole, rebellions.ai/ATOM 등) 설정에 따라
	// 달라지므로 필터링하지 않고 그대로 노출합니다.
	Allocatable map[string]string `json:"allocatable,omitempty"`
	// Upgrades는 이 노드에 속한 DriverUpgradeState 목록(벤더별 1개)입니다.
	Upgrades []UpgradeStatus `json:"upgrades,omitempty"`
}

// DeviceStatus는 NodeDeviceReport.status.devices의 한 항목입니다.
type DeviceStatus struct {
	Vendor        string `json:"vendor,omitempty"`
	Model         string `json:"model,omitempty"`
	Count         int32  `json:"count,omitempty"`
	DriverLoaded  bool   `json:"driverLoaded,omitempty"`
	DriverVersion string `json:"driverVersion,omitempty"`
	// DriverBinding: "nvidia" 등 드라이버 바인딩 | "vfio-pci"(passthrough) | "none"(미바인딩).
	DriverBinding string `json:"driverBinding,omitempty"`
	NeedsReboot   bool   `json:"needsReboot,omitempty"`
	// PCIeAddress 는 NodeDeviceReport 의 대표 PCI 주소다. 같은 모델 여러 장을
	// 사람이 구분하는 유일한 축이므로 화면·CLI 가 이것으로 행을 가른다.
	PCIeAddress string `json:"pcieAddress,omitempty"`
	// Product 는 "<vendor>/<model>" 표시명이다(예: nvidia/a30). model 이
	// 비면 "<vendor>/generic" 이 되어 모른다는 사실이 화면에 남는다.
	Product string `json:"product,omitempty"`
}

// UpgradeStatus는 DriverUpgradeState 한 건입니다.
type UpgradeStatus struct {
	Name           string `json:"name"`
	Vendor         string `json:"vendor"`
	Model          string `json:"model,omitempty"`
	State          string `json:"state,omitempty"`
	CurrentVersion string `json:"currentVersion,omitempty"`
	DesiredVersion string `json:"desiredVersion,omitempty"`
	Message        string `json:"message,omitempty"`
	Retries        int32  `json:"retries,omitempty"`
}

// ClusterPolicyStatus는 NPUClusterPolicy 하나의 조회 결과(관측 상태)입니다.
type ClusterPolicyStatus struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Phase/Ready는 status.phase 및 status.conditions[type=Ready] 요약입니다.
	Phase        string `json:"phase,omitempty"`
	Ready        string `json:"ready,omitempty"` // "True"|"False"|"Unknown"|"" (조건 없음)
	ReadyReason  string `json:"readyReason,omitempty"`
	ReadyMessage string `json:"readyMessage,omitempty"`
}

// NodeDetail 은 노드 하나의 상세다. 여기 실리는 값은 전부 다른 컨트롤러가 이미 판정해
// CRD 에 적어 둔 것이다 — 이 패키지는 옮기기만 하고 다시 판정하지 않는다.
type NodeDetail struct {
	Name           string            `json:"name"`
	Excluded       bool              `json:"excluded,omitempty"`
	ExcludedReason string            `json:"excludedReason,omitempty"`
	Devices        []DeviceStatus    `json:"devices,omitempty"`
	Allocatable    map[string]string `json:"allocatable,omitempty"`
	Health         *HealthView       `json:"health,omitempty"`
	Evidence       []EvidenceView    `json:"evidence,omitempty"`
	Policies       []PolicyView      `json:"policies,omitempty"`
}

// HealthView 는 AcceleratorHealth.status 의 표시용 사본이다.
type HealthView struct {
	State             string             `json:"state,omitempty"`
	Reason            string             `json:"reason,omitempty"`
	AllocationAllowed bool               `json:"allocationAllowed,omitempty"`
	Devices           []DeviceHealthView `json:"devices,omitempty"`
}

// DeviceHealthView 는 AcceleratorHealth.status.devices 의 한 항목이다.
type DeviceHealthView struct {
	PCIAddress string `json:"pciAddress,omitempty"`
	State      string `json:"state,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// EvidenceView 는 AcceleratorEvidence 의 표시용 사본이다. 벤더별로 여러 건일 수 있다.
type EvidenceView struct {
	Vendor    string `json:"vendor,omitempty"`
	Level     string `json:"level,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// PolicyView 는 이 노드를 대상으로 삼은 AcceleratorPartitionPolicy 다.
type PolicyView struct {
	Name   string `json:"name,omitempty"`
	Vendor string `json:"vendor,omitempty"`
	Phase  string `json:"phase,omitempty"`
}
