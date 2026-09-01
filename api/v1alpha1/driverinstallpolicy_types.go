package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 드라이버 설치 방식. ngc 만 이미지 자체가 드라이버 버전을 담는다 — 나머지는 이미지가
// 버전과 무관하고 DRIVER_VERSION 환경 변수로 설치 버전이 정해진다.
const (
	DriverInstallerAPT    = "apt"
	DriverInstallerNGC    = "ngc"
	DriverInstallerScript = "script"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=dip
// DriverInstallPolicy는 노드의 장치 드라이버/툴킷 설치 정책을 정의합니다.
// 예) Vendor: "furiosa"/"nvidia", Model: "warboy"/"generic"(또는 GPU 세대명)
type DriverInstallPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DriverInstallPolicySpec `json:"spec,omitempty"`
}

// UpgradePolicy defines the upgrade behavior for driver installation
type UpgradePolicy struct {
	// AutoUpgrade enables automatic upgrade when version mismatch is detected
	AutoUpgrade bool `json:"autoUpgrade,omitempty"`
	// DrainEnabled enables cordon+drain before driver upgrade on nodes with active GPU workloads
	DrainEnabled bool `json:"drainEnabled,omitempty"`
	// ForceUpgrade allows drain --force when pods cannot be evicted gracefully
	ForceUpgrade bool `json:"forceUpgrade,omitempty"`
	// MaxUnavailable is the max number of nodes being upgraded simultaneously
	// +kubebuilder:default=1
	MaxUnavailable int32 `json:"maxUnavailable,omitempty"`
	// MaxParallelUpgrades is the maximum number of nodes being upgraded in parallel
	// +kubebuilder:validation:Minimum=1
	MaxParallelUpgrades int32 `json:"maxParallelUpgrades,omitempty"`
	// DrainTimeout is the maximum duration to wait for node drain to complete (e.g. "5m", "10m")
	DrainTimeout string `json:"drainTimeout,omitempty"`
	// ValidationTimeout is the maximum duration to wait for driver validation after upgrade (e.g. "2m")
	ValidationTimeout string `json:"validationTimeout,omitempty"`
	// RollbackOnFailure triggers automatic rollback to the previous driver version on upgrade failure
	RollbackOnFailure bool `json:"rollbackOnFailure,omitempty"`
	// MaxRollbackAttempts는 롤백이 반복 실패할 때 시도할 최대 횟수입니다.
	// 이 값을 초과하면 "Failed" 상태로 전이하여 무한 루프를 방지합니다. 기본값 3.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	MaxRollbackAttempts int32 `json:"maxRollbackAttempts,omitempty"`

	// RollbackTarget 은 롤백 시 어떤 image 로 되돌릴지 결정합니다.
	// previousValidated : 직전 사이클에서 검증된 PreviousImage 가 있을 때만 롤백 (없으면 Failed).
	//                     plain tag 치환으로 인한 broken image 회귀 (architectural plan §3.4) 차단.
	// spec              : 현재 동작 — PreviousImage 가 없으면 태그만 치환하여 복구 (legacy, unsafe).
	// 비워두면 spec 과 동일하게 처리되어 backward compat 유지. 신규 클러스터는 previousValidated 권장.
	// +kubebuilder:validation:Enum=previousValidated;spec
	RollbackTarget string `json:"rollbackTarget,omitempty"`

	// IdleCooldownSeconds 는 Idle 진입 후 다음 upgrade 트리거를 받지 않을 최소 기간(초).
	// 미지정(nil) 시 default 10s 적용. 0 또는 음수는 cooldown 비활성화 (legacy 동작).
	// rolling-update 테스트에서 연속 trigger 가 mid-state 로 오인되거나 관측되지 않은 채
	// 신규 사이클이 시작되는 것을 차단하는 안전장치 (followup plan §F3).
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	IdleCooldownSeconds *int32 `json:"idleCooldownSeconds,omitempty"`

	// MaxReboots 는 cross-major 드라이버 교체 시 노드 재부팅을 트리거할 최대 횟수입니다(S2-5).
	// 초과 시 Failed 로 전이(무한 재부팅 방지). cross-major 는 통상 1회면 충분. 기본 1.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	MaxReboots int32 `json:"maxReboots,omitempty"`
	// RebootTimeout 은 Rebooting 상태에서 노드 Ready 복귀를 대기할 최대 시간(e.g. "15m"). 미지정 시 15m.
	RebootTimeout string `json:"rebootTimeout,omitempty"`
}

// DriverInstallPolicySpec 은 벤더/모델별 드라이버 설치 정책을 담습니다.
type DriverInstallPolicySpec struct {
	// 예: "furiosa", "nvidia"
	// 컨트롤러에서 NodeDeviceReport.status.devices[].vendor 와 매칭
	Vendor string `json:"vendor"`

	// 예: "warboy", "generic" (또는 "a100","a30" 등 필요시 사용)
	// 빈 값 허용(벤더만 매칭)하려면 Controller에서 로직으로 지원
	Model string `json:"model,omitempty"`

	// 설치할 드라이버 사양
	Driver DriverSpec `json:"driver"`

	// (선택) NVIDIA Container Toolkit 등 런타임 툴킷 설치 사양
	Toolkit *ToolkitSpec `json:"toolkit,omitempty"`

	// 허용 커널 버전 패턴 (예: ["5.15.*","6.8.*"])
	KernelAllowlist []string `json:"kernelAllowlist,omitempty"`

	// 최소 containerd 버전 (semver)
	ContainerdMinVersion string `json:"containerdMinVersion,omitempty"`

	// 재부팅 전략
	// +kubebuilder:validation:Enum=Require;IfNeeded;Never
	RebootStrategy string `json:"rebootStrategy,omitempty"`

	// RebootExecutor 는 재부팅 실행 방식입니다(S2-5). Job(기본): operator 가 privileged reboot Job 생성.
	// kured: 노드에 kured sentinel annotation 만 부착(kured 운영 클러스터용, cordon/drain 중복 억제 설정 권장).
	// +kubebuilder:validation:Enum=Job;kured
	// +kubebuilder:default=Job
	RebootExecutor string `json:"rebootExecutor,omitempty"`

	// (선택) 잡 템플릿 오버라이드(서비스어카운트, TTL, Backoff 등)
	JobOverrides *JobOverrides `json:"jobOverrides,omitempty"`

	// (선택) 잡/파드에 적용할 노드 셀렉터(특정 노드군에만 설치)
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Upgrade policy for driver version changes
	UpgradePolicy *UpgradePolicy `json:"upgradePolicy,omitempty"`

	// VerifiedVersions 검증된 driver version 화이트리스트.
	// non-empty 이면 spec.driver.version 이 이 목록에 없을 경우 업그레이드를 거부하고
	// DUS 를 "UnverifiedVersion" 상태로 전이합니다 (NVIDIA GPU Operator validateDriver 동등).
	// 비어있으면 버전 검증을 skip 하여 기존 동작을 유지합니다 (backward compat).
	// +optional
	VerifiedVersions []string `json:"verifiedVersions,omitempty"`

	// ImagePullSecrets 는 operator 가 생성하는 driver DaemonSet pod spec 에 부착되는
	// pull secret 목록입니다. helm `imagePullSecrets` 값이 CR 을 통해 전파됩니다.
	// 미지정 시 빈 목록 — 노드레벨 인증 경로를 유지하는 하위호환 동작입니다.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// DriverSpec은 드라이버 버전/이미지 및 설치 방법을 정의합니다.
type DriverSpec struct {
	// 예: "1.7.8" (Furiosa) 또는 "575.64.03"(NVIDIA)
	// 비워두면 인스톨러 스크립트가 "권장 버전"으로 자동 선택하도록 구현 가능
	Version string `json:"version,omitempty"`

	// 인스톨러 컨테이너 이미지
	// 예: "registry.example.com:5000/furiosa-driver-installer:1.7.8" 또는 "ghcr.io/you/nvidia-apt-installer:latest"
	//
	// CRD 는 docker reference 의 syntax 만 검증한다 (tag 부에 invalid char 차단). 의미론적
	// 검증 — broken plain tag 가 mode=daemonset 환경에서 entrypoint 누락된 broken image 로
	// 작동하는 결함을 차단 (architectural plan §3.4 defense-in-depth) — 은 state_machine 의
	// isVerifiedBuildTag 가드 가 담당한다 (followup plan §F4).
	//
	// 허용 (registry/host:port/repo 부 + ":" + tag 부):
	//   - registry 부: alphanumerics, `.`, `_`, `:`, `/`, `-` (port 표기 호환)
	//   - tag 부:      alphanumerics, `.`, `_`, `+`, `-` (sub-version 예: "580.142-v17.1")
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._:/-]+:[a-zA-Z0-9._+-]+$`
	Image string `json:"image"`

	// 설치 방식
	// apt  : APT 기반(내/외부 리포지터리 사용)
	// ngc  : NVIDIA NGC 드라이버 컨테이너(커널 매칭 이미지) 사용
	// script : 컨테이너 내 스크립트로 설치(자유도 높음)
	// +kubebuilder:validation:Enum=apt;ngc;script
	Installer string `json:"installer,omitempty"`

	// Mode는 드라이버 설치 방식을 결정합니다.
	// daemonset: DaemonSet으로 드라이버를 상시 실행 (컨테이너화 드라이버, 검증된 21초 롤링 자산 — 기본값).
	// job:       순간 install Job 으로 설치 후 즉시 종료 (상주 pod 없음, WP-C-1). 100% opt-in.
	//            DUS 상태머신이 install Job 의 단일 소유자이며 daemonset 경로 본문은 무변경으로 재사용된다.
	// +kubebuilder:validation:Enum=daemonset;job
	// +kubebuilder:default=daemonset
	Mode string `json:"mode,omitempty"`

	// (선택) 인스톨러 환경변수
	Env []KV `json:"env,omitempty"`

	// (선택) 시크릿 마운트(예: Furiosa APT 인증)
	// 기본 MountPath는 "/secrets"로 두고 필요시 개별 지정
	Secrets []SecretMount `json:"secrets,omitempty"`

	// (선택) 추가 호스트 마운트(기본 /lib/modules, /usr/src, /etc, /var/lib/kcloud-operator 외)
	ExtraHostMounts []HostPathMount `json:"extraHostMounts,omitempty"`

	// AllowDowngrade (b) 는 호스트에 이미 설치된 드라이버가 desired 보다 상위 버전일 때
	// 하위 버전으로의 다운그레이드 설치를 허용할지 결정합니다.
	// 기본 false = 다운그레이드 금지(기존 상위 버전 유지, 설치 보류). true 여야 하위 버전 설치 허용.
	// +kubebuilder:default=false
	AllowDowngrade bool `json:"allowDowngrade,omitempty"`

	// VersionSource (c) 는 effective desired 버전을 어디서 채택할지 결정합니다.
	// Policy : 기존 동작 — spec.driver.version 을 desired 로 사용.
	// Host   : 호스트에 설치된 버전을 desired 로 채택(기존 존중). 호스트 무드라이버면 Policy 버전으로 fallback.
	// +kubebuilder:validation:Enum=Policy;Host
	// +kubebuilder:default=Policy
	VersionSource string `json:"versionSource,omitempty"`

	// SkipOnPassthrough (a) 는 노드의 GPU 가 전량 vfio-pci(passthrough)에 바인딩된 경우
	// 드라이버 설치를 보류(skip)할지 결정합니다.
	// 기본 true = 전량 vfio 노드에서 설치 보류(passthrough 보호). false 면 기존 동작(설치 진행).
	// +kubebuilder:default=true
	SkipOnPassthrough bool `json:"skipOnPassthrough,omitempty"`

	// TrackOnly 는 true 면 DUS 추적(버전 관측 + verifiedVersions 게이트)만 수행하고,
	// 설치 경로(driver DaemonSet / install Job)를 생성하지 않습니다. installer 이미지가
	// 아직 없는 벤더를 버전 관리 체계에 임시 편입할 때 사용합니다(예: Tenstorrent Blackhole —
	// 실제 설치는 tt-kmd DKMS 로 2차). 기본 false = 기존 동작(설치 경로 활성).
	// +kubebuilder:default=false
	TrackOnly bool `json:"trackOnly,omitempty"`
}

// ToolkitSpec 은 NVIDIA Container Toolkit 등 런타임 툴킷 설치를 정의합니다.
type ToolkitSpec struct {
	// 설치 여부
	Enabled bool `json:"enabled"`

	// 설치 방법
	// apt : NVIDIA 공식 APT 저장소/패키지 사용
	// ctk : nvidia-ctk 기반 구성만 수행(패키지는 사전 설치 가정)
	// +kubebuilder:validation:Enum=apt;ctk
	Method string `json:"method,omitempty"`

	// 원하는 툴킷 버전(예: "1.17.8-1"). 비우면 최신(또는 스크립트 내 권장)으로
	Version string `json:"version,omitempty"`

	// (선택) 전용 인스톨러 이미지 (미지정 시 Driver.Image 재사용 가능)
	Image string `json:"image,omitempty"`

	// (선택) 환경변수/시크릿/호스트마운트
	Env             []KV            `json:"env,omitempty"`
	Secrets         []SecretMount   `json:"secrets,omitempty"`
	ExtraHostMounts []HostPathMount `json:"extraHostMounts,omitempty"`
}

// JobOverrides 는 잡 스펙의 일부를 정책에서 제어하기 위한 필드입니다.
type JobOverrides struct {
	// 서비스어카운트 이름(미지정 시 운영자 기본 SA 사용 또는 빈값)
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// 완료 후 자동 삭제 TTL(초)
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// 실패 재시도 횟수
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`

	// (선택) 파드 우선순위 클래스
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// KV는 단순 키-값 환경변수 표현입니다.
type KV struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// SecretMount는 시크릿을 파드에 마운트하기 위한 간단한 스펙입니다.
type SecretMount struct {
	// 시크릿 이름
	Name string `json:"name"`

	// 마운트 경로 (기본: "/secrets")
	MountPath string `json:"mountPath,omitempty"`

	// 선택적 여부
	Optional *bool `json:"optional,omitempty"`
}

// HostPathMount는 호스트 디렉터리를 파드에 마운트하기 위한 간단한 스펙입니다.
type HostPathMount struct {
	// /etc, /lib/modules 등의 호스트 경로
	HostPath string `json:"hostPath"`

	// 파드 내 마운트 경로
	MountPath string `json:"mountPath"`

	// 읽기전용 여부
	ReadOnly bool `json:"readOnly,omitempty"`
}

// +kubebuilder:object:root=true
type DriverInstallPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DriverInstallPolicy `json:"items"`
}
