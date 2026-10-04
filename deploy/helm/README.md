# kcloud-operator (Helm Chart)

NVIDIA GPU + Furiosa(Warboy/RNGD) + Tenstorrent Blackhole + Rebellions ATOM+ 를
단일 Operator 로 관리하는 Kubernetes NPU/GPU Operator 의 Helm 차트.

두 릴리스 라인이 있다. 이 디렉터리는 차트 0.8.x 로 Kubernetes 1.31~1.34 를 담당하고
(프리셋 `values-k8s1.34.yaml`), `Chart.yaml` 의 `kubeVersion: ">=1.31.0-0"` 이 그 범위 밖
클러스터의 설치를 거절한다. Kubernetes 1.26~1.30 은 차트 0.6.x 가 담당한다 — 그 차트와
프리셋 `values-k8s1.28.yaml` 은 `release/k8s-1.28` 브랜치에 있고 이 디렉터리에는 없다.
두 라인의 프리셋을 한 차트에 함께 두면 선언한 `kubeVersion` 과 실재가 어긋나고, 그 조합으로
설치하면 1.26~1.30 라인 operator 가 1.31+ CRD 를 덮어쓴다.

프리셋을 raw URL 로 줄 때 브랜치 주소는 최신 내용을 확인하는 용도다. Magnum 같은 운영
자동화는 `deploy/integration/release.yaml` 의 커밋 SHA 로 고정된 `valuesURL` 을 쓴다.

차트가 배포하는 것:
- **Operator**(controller-manager) Deployment + RBAC + (옵션) Leader election
- **NPUClusterPolicy** CR → operator 가 벤더별 **device-plugin DaemonSet** + **kcloud-node-manager** 를 reconcile
- **DriverInstallPolicy** CR(벤더별) → operator 가 **드라이버 설치 Job** 을 reconcile (기본 `mode: job`, 상시 DaemonSet 은 `mode: daemonset`)
- CRD(`npu.ai/*`) 13종, NVIDIA RuntimeClass, pre-upgrade hook(CRD apply / 구 DS cleanup),
  pre-delete 삭제 게이트와 post-delete CRD 정리 hook

> 드라이버는 호스트에 설치되며, 이미 일치하는 버전이 깔려 있으면 **idempotent skip(무재부팅)**.

## Prerequisites

- Kubernetes ≥ 1.24, Helm ≥ 3.8 (OCI 사용 시)
- 컨테이너 런타임: containerd. 사설/HTTP 레지스트리 사용 시 노드에 insecure-registry 설정
- 차트가 참조하는 **이미지들을 클러스터 노드가 pull 가능**해야 함 (가장 흔한 실패: ImagePullBackOff)
- **노드 라벨**은 부여하지 않아도 된다. `kcloud-node-manager` 가 PCI 조회 결과로 직접 붙이며
  NFD 설치 여부와 무관하다. device-plugin·드라이버 설치 Job·exporter 가 이 라벨을 nodeSelector 로 쓴다.

  | 가속기 | 라벨 |
  |--------|------|
  | NVIDIA GPU | `kcloud.ai/nvidia.present=true` |
  | Furiosa Warboy | `kcloud.ai/furiosa.present=true` |
  | Furiosa RNGD | `kcloud.ai/rngd.present=true` |
  | Tenstorrent Blackhole | `kcloud.ai/tenstorrent.present=true` |
  | Rebellions ATOM+ | `kcloud.ai/rebellions.present=true` |

## Installing

```bash
# 디렉토리에서
helm install kcloud-operator deploy/helm -n kcloud --create-namespace

# 또는 OCI 레지스트리에서 (HTTP 레지스트리는 --plain-http)
helm install kcloud-operator oci://<registry>/charts/kcloud-operator \
  --version <chart-version> -n kcloud --create-namespace --plain-http
```

kcloud 가 공개 발행하는 차트는 GHCR 에 있다. `v*` 태그를 push 하면 릴리스 워크플로가
`ghcr.io/openkcloud/charts/kcloud-operator` 로 차트를, `ghcr.io/openkcloud/kcloud-operator`
로 operator 이미지를 올린다. 차트 버전은 `v` 접두사가 없고 이미지 태그는 붙는다.

```bash
helm install kcloud-operator oci://ghcr.io/openkcloud/charts/kcloud-operator \
  --version 0.8.0 -n kcloud --create-namespace
```

GHCR 은 HTTPS 이므로 `--plain-http` 를 붙이지 않는다. 공개 패키지는 인증 없이 받을 수 있고,
비공개 상태라면 `helm registry login ghcr.io` 로 먼저 로그인해야 한다.

단계별 절차와 확인 항목은 [INSTALL.md](INSTALL.md) 에 있다.

설치 직후 확인:
```bash
helm list -n kcloud
kubectl get pod -n kcloud                       # controller-manager 1/1 Running
# NPUClusterPolicy 는 get 출력에 READY 열이 없다. 조건 값을 직접 읽는다
kubectl -n kcloud get npuclusterpolicy npuclusterpolicy-sample -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
# node-manager·exporter 는 kcloud, 벤더 device-plugin 은 kube-system 에 있다
kubectl get ds -A | grep -E "kcloud-|device-plugin"
kubectl get nodes -o custom-columns='NODE:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,RNGD:.status.allocatable.furiosa\.ai/rngd'
kubectl get driverupgradestate                        # 각 노드 Idle 이어야 정상
```

## Upgrading

```bash
helm upgrade kcloud-operator deploy/helm -n kcloud --reset-then-reuse-values --set image.tag=<new>
```
- `crdUpgrade.enabled=true`(기본): helm upgrade 시 operator 의 `apply-crds` 서브명령 Job 이 CRD 를
  자동 적용(Helm 은 crds/ 를 install 전용으로 다루므로). 비활성화 시 수동 `kubectl apply -f crds/` 필요.
- **`--reuse-values` 금지**: Helm v4.0.1 에서 이 옵션은 직전 릴리스가 계산해 둔 값을 통째로
  재사용하므로, 사용자 값이 우선하지 않는 차트 기본값 변경이 반영되지 않는다. 2026-09-24 에 NVIDIA
  검증 목록 변경이 이 때문에 리비전 세 개 동안 조용히 빠졌다. 값을 바꿔 반영할 때는 위 명령처럼
  `--reset-then-reuse-values` 를 쓴다 — 차트 기본값을 다시 계산한 뒤 사용자 값을 다시 적용하므로
  둘 다 반영된다. `make helm-option-check` 가 저장소 안 `--reuse-values` 단독 사용을 검사한다.
- **반영 후 확인**: 리비전이 올라간 것만으로는 값이 반영됐다는 증거가 아니다. 바꾼 필드가 실제로
  라이브에 들어갔는지 `helm get values` 등으로 직접 확인한다.

## Uninstalling

삭제는 `helm uninstall` 한 번으로 끝납니다. 차트가 `pre-delete` hook Job 을 먼저 실행하고,
그 Job 이 정책 CR 과 operator 가 만든 DaemonSet 을 순서대로 정리합니다.

```bash
helm uninstall kcloud-operator -n kcloud
```

### 삭제 정책

operator 가 관리하는 가속기 장치를 사용 중인 Pod 가 하나라도 있으면 삭제를 보류합니다.
보류는 아무것도 지우지 않았다는 뜻입니다. hook Job 이 실패하면서 `helm uninstall` 이 멈추므로
operator, 정책 CR, CRD, DaemonSet 이 모두 그대로 남습니다.

사용 중 판정 대상은 다음 둘입니다.

- 컨테이너의 `requests`/`limits` 에 `nvidia.com/`, `furiosa.ai/`, `beta.furiosa.ai/`,
  `tenstorrent.com/`, `rebellions.ai/` 로 시작하는 자원이 있는 Pod
- `spec.resourceClaims` 로 ResourceClaim 을 참조하는 Pod (Dynamic Resource Allocation 경로)

`Succeeded`·`Failed` 상태의 Pod 는 장치를 잡고 있지 않으므로 세지 않습니다.

사용 중인 Pod 가 없으면 hook Job 이 다음 순서로 지웁니다. 순서는 operator 가 finalizer 로
DaemonSet 을 정리할 시간을 확보하기 위한 것입니다.

1. `AcceleratorWorkload`
2. `AcceleratorClass`, `AcceleratorPartitionPolicy` 를 비롯한 나머지 `npu.ai` CR
3. `NPUClusterPolicy`
4. `DriverInstallPolicy`
5. 이름과 `app.kubernetes.io/component` 라벨로 찾은 잔여 DaemonSet

`NPUClusterPolicy`, `DriverInstallPolicy`, `AcceleratorPartitionPolicy` 는 삭제 후 실제로
사라질 때까지 기다립니다. `uninstall.gate.timeout`(기본 `5m`)을 넘기면 finalizer 를 제거하고
경고를 남긴 뒤 진행합니다.

호스트에 설치된 드라이버 패키지와 커널 모듈은 지우지 않습니다. hook Job 은 Kubernetes
오브젝트만 다룹니다.

### 보류됐을 때 확인하는 방법

보류되면 `helm uninstall` 은 `resource not ready, name: kcloud-operator-uninstall-gate,
kind: Job, status: Failed` 로 끝납니다. helm 은 hook Job 이 실패해도 곧바로 멈추지 않고
`--timeout`(기본 `5m`)까지 기다린 뒤 이 오류를 냅니다. 판정 자체는 Job 이 시작하고 몇 초 안에
끝나므로, 결과를 빨리 보려면 아래 로그를 먼저 읽으면 됩니다.

hook Job 은 실패해도 남습니다. 어느 Pod 때문에 보류됐는지는 그 Job 의 로그에 표로 찍힙니다.

```bash
kubectl -n kcloud logs job/kcloud-operator-uninstall-gate
```

해당 Pod 를 정리한 뒤 `helm uninstall` 을 다시 실행하면 됩니다.

### 관련 값

| 값 | 기본값 | 설명 |
|----|--------|------|
| `uninstall.gate.enabled` | `true` | `pre-delete` 게이트 Job 실행 여부 |
| `uninstall.gate.skipUsageCheck` | `false` | 사용 중 검사를 건너뜁니다. 클러스터 자체를 삭제하는 경로에서만 켭니다 |
| `uninstall.gate.timeout` | `5m` | CR 이 finalizer 처리로 사라질 때까지 기다리는 상한 |
| `uninstall.purgeCRDs` | `true` | `post-delete` Job 이 `npu.ai` 그룹 CRD 를 삭제합니다 |

Helm 은 설계상 `crds/` 디렉터리의 CRD 를 삭제하지 않습니다. `uninstall.purgeCRDs` 가 참이면
`post-delete` Job 이 `npu.ai` 그룹 CRD 를 지웁니다. 같은 클러스터에 다시 설치할 예정이면
`--set uninstall.purgeCRDs=false` 로 CRD 를 남길 수 있습니다.

### 게이트를 끄고 수동으로 지우기

게이트 Job 에 필요한 권한을 줄 수 없는 환경에서는 `--set uninstall.gate.enabled=false` 로 끄고
`deploy/helm/uninstall.sh` 의 수동 순서를 씁니다. 그 경우 CR 을 operator 보다 먼저 지워야
합니다. operator 가 먼저 사라지면 finalizer 를 처리할 주체가 없어 CR 이 `Terminating` 에서
멈춥니다.

## Configuration

주요 파라미터(전체는 `values.yaml` 참조):

| 파라미터 | 기본값 | 설명 |
|----------|--------|------|
| `global.registry` | `ghcr.io/openkcloud` | **kcloud 가 빌드하는 이미지**의 레지스트리 |
| `global.vendorRegistry` | `""` (빈 값) | **벤더(3rd-party) 이미지**의 미러. 비우면 각 항목의 upstream 에서 직접 pull |
| `image.repository` / `image.tag` | `kcloud-operator` / 릴리스 번호 | operator 이미지 (`global.registry` 조립) |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | 사설 레지스트리 인증 시 |
| `deployClusterPolicy` | `true` | NPUClusterPolicy CR 자동 생성 |
| `detector.image` | `""` | 지정하면 `detector.repository`/`detector.tag` 조립을 무시하고 전체 경로 그대로 사용(노드 디바이스 감지기) |
| `nvidia.enabled` / `nvidia.devicePluginImage` | `true` / `nvcr.io/nvidia/k8s-device-plugin:v0.17.1` | NVIDIA device-plugin |
| `furiosa.enabled` / `furiosa.devicePluginImage` | `true` / `ghcr.io/furiosa-ai/k8s-device-plugin:0.10.1` | Warboy device-plugin |
| `furiosa.rngd.enabled` | `true` | RNGD device-plugin |
| `furiosa.rngd.devicePluginUpstream` / `devicePluginRepository` / `devicePluginTag` | `docker.io` / `furiosaai/furiosa-device-plugin` / `2026.1.1` | 벤더 공개 RNGD device-plugin(`global.vendorRegistry` 로 조립). `--policy` 미지원 |
| `furiosa.rngd.partitionPolicy` / `debugMode` | `none` / `false` | `none`(1)/`quad-core`(2)/`dual-core`(4)/`single-core`(8) instance/card. `none` 외 값은 파티션 지원 내부 이미지(`kcloud/furiosa-device-plugin-mi`) 필요 |
| `rebellions.enabled` | `true` | ATOM+ device-plugin |
| `rebellions.devicePluginUpstream` / `devicePluginRepository` / `devicePluginTag` | `docker.io` / `rebellions/k8s-device-plugin` / `v0.3.6` | 벤더 공개 ATOM+ device-plugin(`global.vendorRegistry` 로 조립) |
| `driverInstallPolicies.<vendor>.enabled` | `true` | 벤더별 driver 설치 정책 CR 생성 |
| `driverInstallPolicies.<vendor>.driver.version` / `.image` | 벤더별 | 설치할 드라이버 버전·이미지 |
| `driverInstallPolicies.<vendor>.driver.mode` | `daemonset` | (job 모드는 legacy) |
| `driverInstallPolicies.<vendor>.driver.kernelImageBuildRevision` | `""` (빈 값) | 커널별 사전 컴파일 이미지 태그의 빌드 개정(`…-vN`). 값을 주면 노드 커널로 태그를 조립해 당긴다(`mode=job` 전용). 비우면 조립하지 않고 `driver.image` 를 쓴다 |
| `driverInstallPolicies.<vendor>.driver.kernelImageVariant` | `warboy`(furiosa) / `rngd` / 그 외 빈 값 | 조립할 태그의 variant 토막(버전과 커널 토막 사이). `kernelImageBuildRevision` 이 비면 효과 없음 |
| `driverInstallPolicies.<vendor>.upgradePolicy.autoUpgrade` | `true` | 버전 불일치 시 자동 업그레이드 |
| `driverInstallPolicies.<vendor>.verifiedVersions` | 벤더별 | 검증된 버전 허용 목록(비면 검증 skip) |
| `crdUpgrade.enabled` | `true` | upgrade 시 CRD 자동 적용 Job |
| `crdUpgrade.kubectlUpstream` / `kubectlRepository` / `kubectlTag` | `docker.io` / `bitnamilegacy/kubectl` / `1.28` | pre-upgrade cleanup Job 의 kubectl 이미지(`global.vendorRegistry` 로 조립) |
| `crdUpgrade.kubectlImage` | `""` | 지정하면 위 조립을 무시하고 전체 경로 그대로 사용 |
| `leaderElection.enabled` | `true` | controller-runtime Lease |
| `resources` | 100m/128Mi ~ 500m/256Mi | operator 리소스 |

벤더 `<vendor>` = `nvidia` / `furiosa`(Warboy) / `rngd`.

## 레지스트리 구성 (Registry Configuration)

### 개요

차트가 참조하는 이미지는 세 부류로 나뉘고, 부류마다 레지스트리를 정하는 방법이 다릅니다.

| 부류 | 조립 방법 | 기본 동작 |
|------|-----------|-----------|
| kcloud 가 빌드하는 이미지 | `<global.registry>/<repository>:<tag>` | `ghcr.io/openkcloud` 에서 pull |
| 벤더(3rd-party) 이미지 | `<global.vendorRegistry>/<repository>:<tag>`, 비어 있으면 `<upstream>/<repository>:<tag>` | 각 벤더의 공개 레지스트리에서 직접 pull |
| 전체 경로를 그대로 적는 이미지 | 조립하지 않고 값 그대로 | 값에 적힌 공개 경로에서 pull |

기본값 그대로 설치하면 모든 이미지가 공개 레지스트리에서 내려옵니다. 사설 미러를
쓰는 경우에만 아래 두 값을 바꿉니다.

```bash
--set global.registry=<미러>/<프로젝트>        # kcloud 이미지
--set global.vendorRegistry=<미러>/<프로젝트>  # 벤더 이미지
```

두 값은 서로 독립입니다. kcloud 이미지만 사내에 두고 벤더 이미지는 인터넷에서 받는
구성이면 `global.vendorRegistry` 를 비워 두면 됩니다.

#### 부류별 이미지 목록

**kcloud 가 빌드하는 이미지** — `global.registry` 로 조립됩니다.

| values 키 | repository | 공개 기본 경로 |
|-----------|-----------|----------------|
| `image` | `kcloud-operator` | `ghcr.io/openkcloud/kcloud-operator` |
| `detector` | `kcloud-node-manager` | `ghcr.io/openkcloud/kcloud-node-manager` |
| `hostExec` | `kcloud-host-exec` | `ghcr.io/openkcloud/kcloud-host-exec` |
| `furiosa.exporter` | `furiosa-exporter` | `ghcr.io/openkcloud/furiosa-exporter` |
| `furiosa.unified.devicePluginRepository` | `kcloud/furiosa-unified-device-plugin` | `ghcr.io/openkcloud/kcloud/furiosa-unified-device-plugin` |
| `tenstorrent.devicePluginRepository` | `kcloud-tt-device-plugin` | `ghcr.io/openkcloud/kcloud-tt-device-plugin` |
| `driverInstallPolicies.nvidia.driver` | `nvidia-driver-ds` | `ghcr.io/openkcloud/nvidia-driver-ds` |
| `driverInstallPolicies.furiosa.driver` / `.rngd.driver` | `furiosa-driver-ds` | `ghcr.io/openkcloud/furiosa-driver-ds` |
| `driverInstallPolicies.tenstorrent.driver` | `tenstorrent-driver-ds` | `ghcr.io/openkcloud/tenstorrent-driver-ds` |

`furiosa.unified` 의 `kcloud/` 접두는 오타가 아닙니다. 사내 Harbor 에 그 경로로만
올라가 있어 지우면 즉시 pull 이 깨지고, `chart_image_path_test.go` 가 그 사실을
고정하고 있습니다.

**벤더(3rd-party) 이미지** — `global.vendorRegistry` 로 조립됩니다. 비어 있으면
`upstream` 값이 가리키는 공개 레지스트리에서 직접 받습니다.

| values 키 | upstream | repository | 언제 필요한가 |
|-----------|----------|-----------|----------------|
| `acpp.mpsControlDaemon` | `nvcr.io` | `nvidia/k8s-device-plugin` | ACPP 공유 모드가 `mps` 일 때 |
| `crdUpgrade.kubectl*` | `docker.io` | `bitnamilegacy/kubectl` | helm upgrade 마다(pre-upgrade Job) |
| `nvidia.dra` | `registry.k8s.io` | `dra-driver-nvidia/dra-driver-nvidia-gpu` | NVIDIA DRA 를 켤 때 |
| `furiosa.rngd.devicePlugin*` | `docker.io` | `furiosaai/furiosa-device-plugin` | RNGD device-plugin 배포 시(기본 켜짐) |
| `furiosa.rngd.dra` | `docker.io` | `furiosaai/furiosa-dra-driver` | Furiosa DRA 를 켤 때 |
| `rebellions.devicePlugin*` | `docker.io` | `rebellions/k8s-device-plugin` | ATOM+ device-plugin 배포 시(기본 켜짐) |
| `rebellions.dra` | `docker.io` | `rebellions/k8s-dra-driver-npu` | Rebellions DRA 를 켤 때 |
| `tenstorrent.dra` | `ghcr.io` | `tenstorrent/tt-dra-driver` | Tenstorrent DRA 를 켤 때 |
| `tenstorrent.dra.fabricManager` | `ghcr.io` | `tenstorrent/tt-fabric-manager` | Tenstorrent DRA 를 켤 때(드라이버가 이 데몬 없이 기동하지 못함) |

`bitnamilegacy` 는 오타가 아닙니다. Bitnami 가 2025-08 에 공개 카탈로그를 정리하면서
`docker.io/bitnami/kubectl` 의 태그를 모두 내리고 `bitnamilegacy` 로 옮겼습니다.
사설 미러를 옛 `<registry>/bitnami/kubectl` 경로에 두고 있다면 `crdUpgrade.kubectlImage`
로 그 경로를 직접 지정하세요.

**전체 경로를 그대로 적는 이미지** — 두 레지스트리 어느 쪽도 적용되지 않습니다.
사용자가 미러 경로를 직접 적는 자리이기 때문입니다.

| values 키 | 기본값 |
|-----------|--------|
| `nvidia.devicePluginImage` | `nvcr.io/nvidia/k8s-device-plugin:v0.17.1` |
| `nvidia.dcgmExporter.image` | `nvcr.io/nvidia/k8s/dcgm-exporter:4.5.2-4.8.1-ubuntu22.04` |
| `furiosa.devicePluginImage` | `ghcr.io/furiosa-ai/k8s-device-plugin:0.10.1` |
| `acpp.probeImage` | `docker.io/library/nginx:1.25.2-alpine` |
| `acpp.probeImages.nvidia` | `""` (비면 레거시 pause; 예 `<미러>/kcloud/nvidia/cuda:12.9.1-base-ubuntu22.04`) |
| `acpp.probeImages.tenstorrent` | `""` (비면 레거시 pause; 예 `<미러>/kcloud/probe-tenstorrent:v1`) |
| `acpp.probeImages.furiosa` | `""` (공식 이미지에 CLI 없음 — NEEDS VENDOR RESEARCH, 비워 둠) |
| `acpp.probeImages.rebellions` | `""` (privileged+hostPID — 비면 레거시 pause; 예 `<미러>/kcloud/probe-rebellions:v1`) |
| `driverInstallPolicies.nvidia.toolkit.image` | 미지정 시 코드 기본값 `nvcr.io/nvidia/k8s/container-toolkit:v1.17.8-ubuntu20.04` |
| `crdUpgrade.kubectlImage` | `""` (지정 시 `crdUpgrade.kubectl*` 조립을 무시) |

### air-gap 미러 절차

1. 위 세 표의 이미지를 인터넷이 되는 곳에서 받습니다.
2. 미러에 올릴 때 경로 규약을 지킵니다. kcloud 이미지는 `<미러>/<repository>`,
   벤더 이미지는 `<미러>/<upstream 의 org>/<image>` 입니다. 예를 들어
   `nvcr.io/nvidia/k8s-device-plugin:v0.19.3` 은 `<미러>/nvidia/k8s-device-plugin:v0.19.3`
   으로 올립니다. upstream 호스트 이름은 경로에 넣지 않습니다.
3. 전체 경로 값 열 개는 values 에서 미러 경로로 직접 덮어씁니다.
   `values-airgap.example.yaml` 에 그 형태가 그대로 들어 있습니다.
4. 설치 시 `--set global.registry=<미러>/<프로젝트> --set global.vendorRegistry=<미러>/<프로젝트>`
   를 함께 줍니다.

미러 경로에 upstream 호스트가 섞이면(`<미러>/nvcr.io/nvidia/...`) 조립 결과가 달라져
`ImagePullBackOff` 가 됩니다. helm 은 이것을 오류로 보지 않으므로 렌더 결과를
`helm template` 으로 먼저 확인하는 편이 빠릅니다.

### 빠른 시작 (Quick Install)

#### 옵션 A: 공개 레지스트리에서 그대로 설치

```bash
helm install kcloud-operator deploy/helm -n kcloud --create-namespace
```

기본값이 공개 경로이므로 추가 설정이 필요 없습니다. 사설 미러를 쓰면 두 줄을 줍니다.

```bash
helm install kcloud-operator deploy/helm -n kcloud --create-namespace \
  --set global.registry=<your-registry>/kcloud \
  --set global.vendorRegistry=<your-registry>/kcloud
```

#### 옵션 B: deploy.env + install.sh (권장 — 반복 배포)

1. `deploy.env` 파일 준비:
```bash
cp deploy/helm/deploy.env.example deploy.env
vi deploy.env    # REGISTRY=<host:port> 설정 필수
```

2. `install.sh` 실행:
```bash
bash deploy/helm/install.sh            # 실제 설치
bash deploy/helm/install.sh --dry-run  # 미리보기
```

`install.sh` 는 `deploy.env` 의 `REGISTRY` 를 `--set global.registry=...` 로 주입합니다.
`VENDOR_REGISTRY` 도 함께 주입하며, 지정하지 않으면 `REGISTRY` 와 같은 값으로 봅니다
(사설 미러 하나에 모든 이미지를 올려 두는 기존 방식이 그대로 동작합니다).

#### 옵션 C: airgap/사설 미러 (모든 이미지 단일 미러)

`values-airgap.example.yaml` 사용:

```bash
cp deploy/helm/values-airgap.example.yaml values-airgap.yaml
# <your-registry> 를 실제 미러 주소(예: registry.internal:5000)로 치환
vi values-airgap.yaml

helm install kcloud-operator deploy/helm -n kcloud --create-namespace \
  -f values-airgap.yaml
```

> 또는 `deploy.env` 에서 EXTRA_ARGS 로 override:
> ```bash
> REGISTRY=registry.internal:5000
> EXTRA_ARGS="-f values-airgap.yaml"
> bash deploy/helm/install.sh
> ```

### 업그레이드 시 레지스트리 변경

```bash
# 방법 1: --set
helm upgrade kcloud-operator deploy/helm -n kcloud --reset-then-reuse-values \
  --set global.registry=new-registry.internal:5000 \
  --set global.vendorRegistry=new-registry.internal:5000

# 방법 2: deploy.env (권장)
# 1. deploy.env 의 REGISTRY 변경
# 2. bash deploy/helm/install.sh --reset-then-reuse-values
```

## Examples

```bash
# 특정 벤더만 끄기 (예: Rebellions 비활성)
helm install kcloud-operator deploy/helm -n kcloud --create-namespace \
  --set rebellions.enabled=false

# RNGD 파티션 변경 (1 instance/card)
helm upgrade kcloud-operator deploy/helm -n kcloud --reset-then-reuse-values \
  --set furiosa.rngd.partitionPolicy=none

# 외부(다른) 레지스트리로 배포 — 레지스트리 경로 override (-f values 파일 권장)
helm install kcloud-operator oci://<reg>/charts/kcloud-operator --version <v> \
  -n kcloud --create-namespace --plain-http -f values-new-registry.yaml
```

## Notes / Gotchas

- **기존 릴리스에서 올릴 때**: `global.vendorRegistry` 는 새로 생긴 값이라 기존 사용자 값에 없다.
  사설 미러를 쓰는 클러스터는 upgrade 시 이 값을 함께 주지 않으면 벤더 이미지 아홉 개가
  upstream 을 직접 당기려 하고, air-gap 이면 `ImagePullBackOff` 로 멈춘다.
- **RNGD partition**: 기본값은 공개 벤더 이미지 + `partitionPolicy: none` + `debugMode: false` 다(2026-09-09).
  `partitionPolicy != none` 은 operator 가 device-plugin 에 `--policy` 를 전달하므로 파티션 지원 내부 이미지
  (`kcloud/furiosa-device-plugin-mi`)로 함께 바꿔야 한다. 공개 이미지는 `--policy` 를 거부해 CrashLoop.
- **driver DS 수명주기**(v0.5.22+): driver DaemonSet 은 DriverInstallPolicy 에 ownerReference 가 걸려
  DIP 삭제 시 K8s GC 가 cascade 삭제.
- **설치 정책은 릴리스 자원**(2026-09-24): 벤더별 `DriverInstallPolicy` 네 개는 helm hook 이
  아니라 릴리스 자원으로 관리된다. hook 으로 두면 업그레이드마다 삭제·재생성되어 uid 가 바뀌고,
  정책을 소유자로 참조하는 toolkit DaemonSet 이 연쇄로 다시 만들어졌다. 정책에 직접 넣은 값도
  다음 업그레이드에서 사라졌다. 이 변경 이전에 설치한 클러스터는 정책에 helm 소유 표시가 없으므로
  `helm upgrade` 전에 `hack/adopt-helm-resources.sh --apply` 를 한 번 실행한다. 릴리스 자원이 되면서 `helm uninstall` 의 삭제 대상에 들어가지만, 실제 삭제 동작은 달라지지 않는다. `uninstall.gate.enabled` 가 기본으로 켜져 있어 pre-delete 게이트가 `npu.ai` 사용자 정의 자원을 먼저 순서대로 지우기 때문이다. 실행하지 않고
  업그레이드하면 helm 이 "already exists" 로 거절한다.
- **레지스트리 도달성**: 다른 클러스터 배포 시 노드가 이미지 레지스트리에 도달 가능해야 함. 상세 절차는
  운영 노트(`operator/tester.md` §5) 참조.
- **CRD**: helm 자체는 `crds/` 의 CRD 를 삭제하지 않는다. 차트의 `post-delete` Job 이
  대신 지운다(`uninstall.purgeCRDs`, 기본 참). 남기려면 그 값을 거짓으로 둔다.
- **containerdMinVersion 제거**(2026-10-04): `DriverInstallPolicy` 에서 `containerdMinVersion`
  필드를 없앴다. kubectl 1.25+ 는 서버 측 필드 검증이 기본 strict 라, 이 필드를 든 손으로 쓴
  DIP 매니페스트는 조용히 무시되지 않고 거절된다. 오류 메시지가 필드 이름을 알려주므로 그
  한 줄만 지우면 된다. **이 차트 자신의 values·템플릿·샘플에서는 이미 지웠다.** 그러나 더
  흔한 경로는 이쪽이다 — 그 전에 설치한 **사이트**가 자기 `-f` values 파일에
  `driverInstallPolicies.<vendor>.driver.containerdMinVersion` 을 그대로 들고 있으면,
  `values.schema.json`(아래 항목) 이 kubectl 과 무관하게 `helm upgrade`/`template` 자체를
  거절한다(`additional properties 'containerdMinVersion' not allowed`). 그 값 파일에서
  그 줄을 지우고 다시 올린다.
- **detector.namespace 제거**(2026-10-04): 소비하는 템플릿·컨트롤러 코드가 없던 죽은 값이라
  지웠다. detector(node-manager)의 실제 네임스페이스는 operator 가 설치된 release
  namespace 로 고정이며, 이 값으로 바꿀 수 있던 적이 없다. 위와 같은 이유로, 그 전에 자기
  values 파일에 `detector.namespace` 를 들고 있던 사이트는 `helm upgrade` 가 거절된다 —
  그 줄을 지운다.
- **values.schema.json**(2026-10-04 추가): 이 차트는 선언되지 않은 값 키를 `helm
  template`/`install`/`upgrade` 단계에서 거절한다. 이전에는 helm 이 모르는 키를 조용히
  버렸다 — 오타 키나 죽은 키가 몇 달째 아무 효과 없이 values 파일에 쌓이는 사고가 반복돼
  막은 것이다. 거절되면 오류가 `at '/<부모경로>': additional properties '<키>' not allowed`
  형태로 **키 경로를 그대로** 알려주므로, 그 경로의 그 줄만 지우면 된다. 벤더가 읽지
  않는 키(예: furiosa 의 `toolkit`, nvidia 의 `driver.trackOnly`)는 문구가 다르다 —
  `at '/driverInstallPolicies/furiosa': 'allOf' failed` 다음 줄에 `at
  '.../toolkit': false schema` 로 나온다. 문구는 달라도 경로는 똑같이 알려준다.
  `affinity`, `resources.limits`/`resources.requests` 는 쿠버네티스 쪽 키라 임의 값을
  그대로 열어 뒀다 — 그 외 블록은 모르는 키를 전부 막는다.
