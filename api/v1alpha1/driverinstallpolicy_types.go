// ============================================================
// driverinstallpolicy_types.go: DriverInstallPolicy CRD 타입
// 상세: 벤더 하나의 드라이버를 어떤 이미지·버전·방식으로 깔지 선언한다.
// 노드별 전이는 DriverUpgradeState 가 맡고 이 타입은 그 전이가 따를 정책이다.
// 생성일: 2026-04-10 | 수정일: 2026-10-09 (workloadProbe 검증 단계 opt-in 필드 추가)
// ============================================================
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

	// WorkloadProbe 는 검증 체인의 마지막 단계다. 켜면 드라이버 교체 검증 때 노드마다 장치 1개를 요청하는
	// 파드를 하나 띄워 명령을 실행하고 지운다(Succeeded 면 통과). 노드는 그 시점에 cordon 돼 있어 사용자
	// 워크로드를 밀어내지 않는다. 기본 꺼짐 — 이미지가 air-gap 미러에 있어야 한다.
	// +optional
	WorkloadProbe *WorkloadProbeSpec `json:"workloadProbe,omitempty"`
}

// KernelImageRef 는 kernelImages 맵의 값이다. image 와 같은 image reference 형식이며, 길이
// 상한은 kernelImages 값 CEL 검증의 비용을 API 서버 예산 안에 들이기 위한 것이다.
// +kubebuilder:validation:MaxLength=512
type KernelImageRef string

// DriverSpec은 드라이버 버전/이미지 및 설치 방법을 정의합니다.
// +kubebuilder:validation:XValidation:rule="!has(self.kernelImages) || size(self.kernelImages) == 0 || self.mode == 'job'",message="driver.kernelImages requires driver.mode=job"
// +kubebuilder:validation:XValidation:rule="!has(self.kernelImages) || self.kernelImages.all(k, self.kernelImages[k].matches('^[a-zA-Z0-9._:/-]+:[a-zA-Z0-9._+-]+$'))",message="driver.kernelImages values must be image references"
// +kubebuilder:validation:XValidation:rule="!has(self.kernelImageBuildRevision) || size(self.kernelImageBuildRevision) == 0 || self.mode == 'job'",message="driver.kernelImageBuildRevision requires driver.mode=job"
// +kubebuilder:validation:XValidation:rule="!has(self.kernelImageBuildRevision) || size(self.kernelImageBuildRevision) == 0 || (has(self.version) && size(self.version) > 0)",message="driver.kernelImageBuildRevision requires a non-empty driver.version"
// +kubebuilder:validation:XValidation:rule="!has(self.kernelImageVariant) || size(self.kernelImageVariant) == 0 || self.mode == 'job'",message="driver.kernelImageVariant requires driver.mode=job"
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

	// KernelImages 는 노드 커널 버전(node.status.nodeInfo.kernelVersion, 예 "5.15.0-190-generic")마다
	// 쓸 사전 컴파일 설치 이미지다. 상태기계가 preflight 에서 노드 커널로 이 맵을 찾고, 없으면
	// image(DKMS 컴파일 이미지)를 쓴다. 값은 image 와 같은 형식이며 검증 빌드 태그(…-vN)여야 한다.
	// mode=job 에서만 유효하다(DaemonSet 하나는 커널별 이미지를 가질 수 없다). 자동 롤백 대상은 전이
	// 착수 시 캡처한 이전 버전의 DKMS 태그다(resolvedImage 에서 커널 토큰을 떼고 …-vN 만 남긴 재구성).
	// 따라서 사전 컴파일 설치의 롤백은 노드 DKMS 빌드로 되돌아가며 헤더·컴파일러가 필요하다.
	// 맵 크기와 값 길이 상한은 kernelImages CEL 검증의 비용을 API 서버 예산 안에 들이기 위한 것이다.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	KernelImages map[string]KernelImageRef `json:"kernelImages,omitempty"`

	// KernelImageBuildRevision 은 사전 컴파일 커널 이미지 태그의 빌드 개정(…-vN)이다. 선언하면
	// kernelImages 에 노드 커널 항목이 없어도 태그 규약
	// "<version>[-<kernelImageVariant>]-k<커널>-<빌드 개정>" 으로 이미지 참조를 조립한다 —
	// 커널 하나하나를 맵에 적지 않아도 사전 컴파일 경로가 동작한다.
	// 비우면(기본) 조립하지 않고 image(DKMS 컴파일)를 쓴다. version 이 비면 태그의 버전 토막을
	// 정할 수 없어 조립 자체가 불가능하므로 CEL 이 그 조합을 거절한다.
	//
	// 불리언 토글이 아니라 이 값이 조립을 켜는 이유는 빌드 개정을 유도할 수 없기 때문이다. 같은
	// 드라이버 버전의 커널 이미지를 다시 구우면 개정만 올라가고, 그 숫자는 발행하는 쪽만 안다
	// (라이브 예: 커널 이미지 …:580.126.09-k5.15.0-171-generic-v4 와 같은 정책의 일반 설치
	// 이미지 …:v3 은 서로 다른 개정이다).
	//
	// kernelImages 에 적힌 항목은 조립보다 우선한다 — 규약을 벗어난 이미지의 예외 지정 수단이다.
	// 조립한 참조도 kernelImages 값과 같은 검증(검증 빌드 태그·버전 토큰 대조)을 통과해야 쓰인다.
	// 조립한 태그가 레지스트리에 없으면 install Job 이 그 이미지를 당기지 못하므로, 발행된 커널
	// 집합을 벗어난 노드는 kernelImages 로 따로 지정하거나 이 값을 비워 둔다.
	// mode=job 에서만 유효하다(kernelImages 와 같은 이유 — DaemonSet 하나는 커널별 이미지를 가질 수 없다).
	// +optional
	// +kubebuilder:validation:Pattern=`^v[0-9]+(\.[0-9]+)?$`
	KernelImageBuildRevision string `json:"kernelImageBuildRevision,omitempty"`

	// KernelImageVariant 는 조립할 태그의 variant 토막이다(버전과 커널 토막 사이).
	// 벤더 발행 규약에 그 토막이 있을 때만 채운다 — Furiosa 계열은 필수이고
	// (…:<pkgVersion>-rngd-k<커널>-vN), NVIDIA·Tenstorrent 규약에는 그 토막이 없다.
	// 벤더 차이를 코드가 아니라 정책으로 받는 이유는 컨트롤러에 벤더 이름 분기를 만들지
	// 않기 위해서다. 비우면(기본) "<version>-k<커널>-<빌드 개정>" 을 조립한다.
	// spec.model 을 쓰지 않는 이유는 모든 벤더에 model 이 있고 그 값이 태그에 들어가서는
	// 안 되는 벤더가 있기 때문이다.
	// kernelImageBuildRevision 이 비면 아무 효과가 없다. mode=job 에서만 유효하다.
	// +optional
	// 첫 문자에서 숫자를 빼는 이유는 버전 토큰 대조다. 조립한 태그에서 버전 토막을 떼는 정규식
	// (internal/upgrade 의 kernelImageVersionRe)은 숫자·점으로 이어진 앞머리를 버전으로 읽으므로,
	// "2x" 같은 값이면 버전 토막이 variant 첫 자까지 삼켜 spec.driver.version 과 달라진다.
	// 그러면 조립한 이미지가 거절되고 DKMS 이미지로 조용히 떨어진다.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._]*$`
	KernelImageVariant string `json:"kernelImageVariant,omitempty"`

	// 설치 방식
	// apt  : APT 기반(내/외부 리포지터리 사용)
	// ngc  : NVIDIA NGC 드라이버 컨테이너(커널 매칭 이미지) 사용
	// script : 컨테이너 내 스크립트로 설치(자유도 높음)
	// +kubebuilder:validation:Enum=apt;ngc;script
	Installer string `json:"installer,omitempty"`

	// ImageVersionBaked 는 이미지에 드라이버 버전이 구워져 있어 태그가 곧 설치 버전인 경우다
	// (예: tenstorrent-driver-ds:2.8.0-v1 — entrypoint 가 baked 버전을 권위로 삼고 DRIVER_VERSION 을
	// 무시한다). 참이면 apt/script installer 라도 롤백 대상 이미지를 이전 버전 태그로 재구성한다.
	// 거짓(기본)이면 같은 이미지 + DRIVER_VERSION 으로 되돌린다.
	// +optional
	ImageVersionBaked bool `json:"imageVersionBaked,omitempty"`

	// Mode는 드라이버 설치 방식을 결정합니다.
	// daemonset: DaemonSet으로 드라이버를 상시 실행 (컨테이너화 드라이버, 검증된 21초 롤링 자산 — 기본값).
	// job:       순간 install Job 으로 설치 후 즉시 종료 (상주 pod 없음, WP-C-1). 100% opt-in.
	//            DUS 상태머신이 install Job 의 단일 소유자이며 daemonset 경로 본문은 무변경으로 재사용된다.
	// +kubebuilder:validation:Enum=daemonset;job
	// +kubebuilder:default=daemonset
	Mode string `json:"mode,omitempty"`

	// (선택) 인스톨러 환경변수
	Env []KV `json:"env,omitempty"`

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

	// (선택) 전용 인스톨러 이미지 (미지정 시 Driver.Image 재사용 가능)
	Image string `json:"image,omitempty"`

	// (선택) 환경변수
	Env []KV `json:"env,omitempty"`
}

// WorkloadProbeSpec 은 샘플 워크로드 검증 설정이다.
type WorkloadProbeSpec struct {
	Enabled bool `json:"enabled"`
	// Image 는 벤더 CLI 가 든 이미지다(예: acpp.probeImages 와 같은 것).
	Image string `json:"image,omitempty"`
	// Command 는 종료 코드 0 이면 통과로 보는 명령이다. 비면 이미지 ENTRYPOINT.
	Command []string `json:"command,omitempty"`
	// ResourceName 은 요청할 확장 자원(예: nvidia.com/gpu). 비면 벤더 기본 리소스명.
	ResourceName string `json:"resourceName,omitempty"`
	// TimeoutSeconds 는 파드 완료 대기 상한(기본 120).
	// +kubebuilder:validation:Minimum=10
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
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

// +kubebuilder:object:root=true
type DriverInstallPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DriverInstallPolicy `json:"items"`
}
