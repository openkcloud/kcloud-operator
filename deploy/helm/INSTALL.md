<!-- INSTALL.md: kcloud-operator Helm 설치와 확인 절차 | 생성일: 2026-09-14 -->

# kcloud-operator 설치 · 확인 절차

공개 GHCR 에서 차트를 받아 Service Kubernetes 에 설치하고, 노드가 GPU 와 NPU 를 자원으로 광고하는지까지 확인하는 절차입니다. Helm 상태가 `deployed` 로 나와도 노드의 allocatable 이 비어 있으면 워크로드는 스케줄되지 않으므로, 마지막 확인 단계까지 진행해야 설치가 끝난 것입니다.

순서는 사전 확인, 프로파일 선택, 사이트 값 작성, 설치, 검증 다섯 단계입니다.

## 1. Helm 과 Kubernetes 버전 확인

여기서 확인한 Kubernetes 마이너 버전이 다음 단계의 프로파일을 결정합니다.

```bash
helm version --short
kubectl version -o yaml | grep -A3 serverVersion
kubectl get nodes -o wide
```

```
v3.16.2+g13654a5
serverVersion:
  major: "1"
  minor: "34"
```

## 2. 릴리스 프로파일 선택

마이너 버전에 맞는 차트와 프리셋을 고릅니다. 설치에 쓰는 values 주소는 커밋으로 고정돼 있어 같은 주소가 늘 같은 내용을 돌려줍니다. 정본은 `deploy/integration/release.yaml` 입니다.

| Service K8s | 차트 | 프리셋 | 설치용 values 주소 |
|---|---|---|---|
| 1.31~1.34 | 0.7.30 | `values-k8s1.34.yaml` | `.../45fcbedc83008c81c6976774b6af4ef8b01cd44d/deploy/helm/values-k8s1.34.yaml` |
| 1.26~1.30 | 0.6.2 | `values-k8s1.28.yaml` | `.../9397ea30fe494120877d65cb27053a2a3eb09340/deploy/helm/values-k8s1.28.yaml` |

브랜치 주소(`release.yaml` 의 `valuesBrowseURL`)는 최신 내용을 사람이 확인할 때만 씁니다. 브랜치는 새 커밋이 들어오면 같은 주소가 다른 내용을 돌려주므로 설치에는 쓰지 않습니다.

```bash
export CHART_VERSION=0.7.30
export VALUES_SHA=45fcbedc83008c81c6976774b6af4ef8b01cd44d
export VALUES_URL="https://raw.githubusercontent.com/openkcloud/kcloud-operator/$VALUES_SHA/deploy/helm/values-k8s1.34.yaml"
```

1.26~1.30 클러스터라면 `CHART_VERSION=0.6.2`, `VALUES_SHA=9397ea30fe494120877d65cb27053a2a3eb09340`, 프리셋 파일 이름은 `values-k8s1.28.yaml` 입니다.

## 3. site-values.yaml 작성

저장소에 들어 있는 파일이 아니라, 설치 직전에 만들어 값을 덮어쓰는 로컬 파일입니다. 공개망이라 덮어쓸 값이 없어도 만들어야 합니다. helm 은 `-f` 로 지정한 파일이 없으면 실패합니다.

```bash
cat > site-values.yaml <<'EOF'
global:
  registry: ghcr.io/openkcloud
  vendorRegistry: ""
EOF
```

사설 미러를 쓰면 두 값을 미러 주소로 바꿉니다. `nvidia.devicePluginImage` 처럼 전체 경로를 그대로 받는 항목은 조립되지 않으므로 이 파일에 직접 적습니다. 본보기는 `../integration/examples/site-values.example.yaml` 에 있습니다.

## 4. 설치 전 렌더링 점검

리소스를 만들지 않고 렌더링 결과만 먼저 확인합니다.

```bash
helm template kcloud-operator oci://ghcr.io/openkcloud/charts/kcloud-operator \
    --version "$CHART_VERSION" -n kcloud \
    -f "$VALUES_URL" -f site-values.yaml \
  | grep -E "image:|kind:" | sort -u
```

operator 이미지가 `ghcr.io/openkcloud/kcloud-operator:v0.7.30` 으로 나오는지 봅니다. 목록에 DRA 드라이버 이미지 두 개가 함께 보이는데, 기본값이 꺼져 있어 실제로 배포되지는 않습니다. `kcloud-host-exec` 과 Tenstorrent device-plugin 은 차트가 아니라 operator 가 실행 중에 만들기 때문에 이 목록에 나오지 않습니다.

## 5. 설치

릴리스가 없으면 설치하고, 이미 있으면 업그레이드로 동작합니다.

```bash
helm upgrade --install kcloud-operator \
    oci://ghcr.io/openkcloud/charts/kcloud-operator \
    --version "$CHART_VERSION" \
    -n kcloud --create-namespace \
    --reset-values \
    -f "$VALUES_URL" -f site-values.yaml \
    --wait --wait-for-jobs --timeout 15m
```

`--reset-values` 는 이번 프리셋과 사이트 값을 기준으로 값을 다시 계산합니다. `--wait` 는 일반 리소스가 Ready 가 될 때까지, `--wait-for-jobs` 는 hook Job 이 끝날 때까지 기다립니다. hook 을 생략하면 CRD 가 갱신되지 않으므로 `--no-hooks` 는 쓰지 않습니다.

설치가 끝나면 `STATUS: deployed` 와 안내 문구가 나옵니다. 여기서 끝내지 않고 아래 확인 단계를 이어서 진행합니다.

## 6. 확인 1 — 릴리스와 Operator Pod

```bash
helm list -n kcloud
kubectl -n kcloud get pods
```

릴리스가 `deployed` 이고 operator Pod 가 `1/1 Running` 이어야 합니다. `CrashLoopBackOff` 이면 `kubectl -n kcloud logs deploy/kcloud-operator --tail=50` 으로 원인을 봅니다.

## 7. 확인 2 — CRD 와 정책 리소스

```bash
kubectl get crd | grep npu.ai | wc -l
kubectl -n kcloud get npuclusterpolicy npuclusterpolicy-sample \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
kubectl get dip -o custom-columns='NAME:.metadata.name,VENDOR:.spec.vendor,MODE:.spec.driver.mode'
```

CRD 는 13종입니다. 정책 CR 이름은 `npuclusterpolicy-sample` 이고 네임스페이스는 `kcloud` 이며, `get` 출력에는 READY 열이 없으므로 조건 값을 직접 읽습니다. DriverInstallPolicy 는 클러스터 범위이고 기본 이름은 `nvidia-gpu-ds`, `furiosa-warboy-ds`, `furiosa-rngd-ds`, `tenstorrent-blackhole-ds` 입니다.

## 8. 확인 3 — 노드 라벨과 DaemonSet

`kcloud-node-manager` 가 PCI 장치를 읽어 라벨을 붙이고, device-plugin 과 드라이버 설치 Job 은 그 라벨이 있는 노드에만 배치됩니다.

```bash
kubectl get nodes -L kcloud.ai/nvidia.present,kcloud.ai/rngd.present,kcloud.ai/tenstorrent.present
kubectl get ds -A | grep -E "kcloud-|device-plugin"
```

구성 요소마다 네임스페이스가 다릅니다. `kcloud-node-manager`, `kcloud-furiosa-exporter`, Tenstorrent device-plugin 은 릴리스 네임스페이스인 `kcloud` 에 있고, 벤더 device-plugin 과 dcgm-exporter, Container Toolkit 은 `kube-system` 에 있습니다. 한쪽만 보면 빠지는 것이 생기므로 `-A` 로 봅니다.

## 9. 확인 4 — 드라이버 상태

```bash
kubectl get driverupgradestate
kubectl -n kcloud get jobs
```

이름은 `<노드>-<벤더>` 형식이고 열은 NODE, VENDOR, STATE, CURRENT, DESIRED 입니다. 설치 Job 이 끝나면 상태가 `Idle` 로 돌아옵니다. 드라이버 설치 Job 도 `kcloud` 네임스페이스에 생깁니다. `Installing` 이 오래 유지되면 그 Job 의 로그를 확인합니다.

## 10. 확인 5 — 노드 자원 광고

```bash
kubectl get nodes -o custom-columns='NODE:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,RNGD:.status.allocatable.furiosa\.ai/rngd'
kubectl get ndr
kubectl npu status
```

노드가 자원을 광고하면 워크로드를 스케줄할 수 있습니다. `NodeDeviceReport` 는 클러스터 범위라 이름과 나이만 나옵니다. 노드별 장치와 드라이버 버전을 한눈에 보려면 `kubectl npu status` 를 씁니다.

## 11. 설치가 실패할 때

이벤트를 먼저 보고, 정상이 아닌 Pod 를 찾아 해당 구성 요소의 로그를 읽습니다.

```bash
kubectl -n kcloud get events --sort-by=.lastTimestamp | tail -20
kubectl get pods -A | grep -vE "Running|Completed"
```

`ImagePullBackOff` 이면 레지스트리부터 봅니다. 공개망인지 사설망인지, pull secret 이 필요한지, `global.registry` 와 전체 경로를 직접 지정한 항목이 맞는지 순서로 확인합니다.

## 12. 되돌리기와 삭제

```bash
helm history kcloud-operator -n kcloud
helm rollback kcloud-operator 1 -n kcloud
helm uninstall kcloud-operator -n kcloud --wait
```

가속기를 쓰는 Pod 가 남아 있으면 pre-delete hook 이 삭제를 중단시키고 uninstall 이 실패합니다. 사유는 `kubectl -n kcloud logs job/kcloud-operator-uninstall-gate` 에 남습니다. 삭제 정책은 `README.md` 의 Uninstall 절에 있습니다.

## 13. 완료 판정

여섯 항목이 모두 통과하면 설치가 끝난 것입니다.

| 항목 | 기준 |
|---|---|
| Helm 릴리스 | STATUS 가 deployed |
| Operator Pod | 1/1 Running |
| CRD 와 정책 | CRD 13종, Ready 조건이 True |
| 노드와 DaemonSet | 벤더 라벨이 붙고 두 네임스페이스의 DaemonSet 이 Ready |
| 드라이버 | DriverUpgradeState 가 Idle |
| 자원 광고 | 노드 allocatable 에 GPU 나 NPU 가 보임 |

Magnum CAPI Add-on 은 이 순서를 그대로 자동화합니다. 활성화 여부 확인, Kubernetes 버전 판별, 프로파일 선택, 사이트 값 생성, Helm 실행 순서이며 상수는 `deploy/integration/release.yaml` 에 있습니다.
