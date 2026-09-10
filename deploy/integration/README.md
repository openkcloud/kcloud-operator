<!-- README.md: Magnum CAPI Add-on 연동 자료(kcloud-operator 측 제공물) 안내 | 생성일: 2026-09-10 -->

# Magnum CAPI 연동 자료

`kcloud-magnum-capi-helm` 이 Service K8s 클러스터에 kcloud-operator 를 자동 설치할 때 필요한 값을 모아 둔 디렉터리다. 릴리스 태그마다 같이 갱신된다.

| 파일 | 내용 |
|---|---|
| `release.yaml` | 차트 주소·버전·Release 이름·namespace·K8s 버전별 프리셋·입력·사이트 값·helm 호출 예 |
| `image-manifest.yaml` | 이 릴리스가 쓰는 이미지 전체(조건 포함), 완성 경로 필드, 드라이버 apt 저장소 |
| `examples/values-service-k8s.yaml` | 프리셋 뒤에 붙일 사이트 값 예시 |

## 설치 형태

```bash
helm upgrade --install kcloud-operator oci://ghcr.io/openkcloud/charts/kcloud-operator \
  --version 0.7.30 -n kcloud --create-namespace \
  --reset-values \
  -f https://raw.githubusercontent.com/openkcloud/kcloud-operator/main/deploy/helm/values-k8s1.34.yaml \
  -f examples/values-service-k8s.yaml \
  --wait --wait-for-jobs --timeout 15m
```

`values-k8s1.34.yaml` 은 차트 안의 프리셋이다. helm 의 `-f` 는 URL 을 받으므로 차트를 풀지 않고 저장소 태그의 raw URL 을 그대로 준다(`release.yaml` 의 `valuesURL`). Add-on CR 에 값을 인라인으로 넣는 방식이면 그 파일 내용을 옮긴다. Service K8s 가 1.26~1.30 이면 차트 `0.6.2` 와 `values-k8s1.28.yaml`(태그 `v0.6.2-k8s1.28`)을 쓴다. 업그레이드도 같은 명령으로 값을 전부 다시 넘긴다. hook 을 생략하면 CRD 가 갱신되지 않으므로 `--no-hooks` 는 쓰지 않는다.

## 입력

Magnum label 은 `kcloud_operator_enabled` 하나다. 벤더 선택은 받지 않는다. node-manager 가 노드의 PCI 장치를 읽어 `kcloud.ai/<vendor>.present` 라벨을 붙이고, device-plugin·드라이버 설치 Job·exporter 는 그 라벨이 있는 노드에만 배치된다. 장치가 없는 벤더는 Pod 가 생기지 않는다. kcloud-operator 를 켜면 NVIDIA GPU Operator 는 꺼야 한다. 드라이버·Container Toolkit·device-plugin·dcgm-exporter 를 kcloud-operator 가 직접 관리하기 때문이다.

## 삭제

```bash
helm uninstall kcloud-operator -n kcloud --wait
```

`pre-delete` hook 이 가속기 자원(`nvidia.com/*`, `furiosa.ai/*`, `beta.furiosa.ai/*`, `tenstorrent.com/*`, `rebellions.ai/*`, DRA claim)을 쓰는 Pod 를 확인한다. 하나라도 있으면 uninstall 이 실패하고 아무것도 지워지지 않는다. Pod 목록은 `kubectl -n kcloud logs job/kcloud-operator-uninstall-gate` 에 있다. 없으면 정책 CR, operator 가 만든 DaemonSet·Job, CRD 까지 전부 삭제된다. 호스트의 드라이버 패키지는 지우지 않는다.

## 이미지 접근

기본값은 kcloud 이미지를 `ghcr.io/openkcloud` 에서, 벤더 이미지를 각 벤더의 공개 레지스트리에서 받는다. 사내 미러를 쓰려면 `global.registry`·`global.vendorRegistry` 와 `image-manifest.yaml` 의 `fullPathValues` 다섯 필드를 미러 경로로 지정한다. 드라이버는 이미지가 아니라 노드가 apt 로 받으므로 `aptRepositories` 의 저장소 도달성(또는 apt 미러)과 Warboy 인증 Secret 이 별도로 필요하다.
