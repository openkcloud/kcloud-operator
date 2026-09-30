<!-- README.md: 가속기 확인용 파드 매니페스트 6종과 Dockerfile 2종 모음 | 생성일: 2026-09-21 -->

# accelerator-pods

kcloud-operator 가 노드에 광고한 가속기를 파드가 실제로 할당받아 `nvidia-smi`·
`furiosa-smi`·`tt-smi` 를 실행할 수 있는지 확인하는 매니페스트 6개와 이미지 빌드용
Dockerfile 2개다. 원래 워크스페이스
루트의 비추적 `pods/` 에 있었고(git 백업 없음), 이 디렉터리로 이관해 `kcloud-operator`
저장소 이력에 편입했다.

상세 절차(이미지 경로 선택 기준과 라이선스 조건, service k8s 의 Harbor CA 문제와 회피,
장치 광고 확인 명령, 이미지 업로드 방법, 확보 방식별 소요 시간)는
가속기 파드 시험 운영 문서
에 있다. 아래 표는 파일별 요약이다.

## 파일 목록

| 파일 | 대상 클러스터 | 장치 | 이미지 | 비고 |
|------|---------------|------|--------|------|
| `rngd-pod.yaml` | .91(사내 개발 클러스터) | `furiosa.ai/rngd` | `<레지스트리>:5100/kcloud/rngd-smi:2026.1.1` | `sleep infinity` 후 `exec` 로 확인 |
| `rngd-smi.Dockerfile` | (이미지 빌드용) | — | `rngd-smi` 이미지 정의 | 벤더 apt 로 `furiosa-smi` 만 설치, 드라이버는 호스트 커널 모듈 사용 |
| `tt-pod.yaml` | .91(사내 개발 클러스터) | `tenstorrent.com/blackhole` | `<레지스트리>:5100/kcloud/tt-smi:latest` | `sleep infinity` 후 `exec` 로 확인 |
| `tt-smi.Dockerfile` | (이미지 빌드용) | — | `tt-smi` 이미지 정의 | PyPI `tt-smi` 만 설치, `/dev/tenstorrent/N` 은 device-plugin 이 주입 |
| `under-gpu-pod.yaml` | under k8s | `nvidia.com/gpu` | `harbor.io:7443/library/cuda:12.9.1-base-ubuntu22.04` | 2026-09-17 `compute6` 에서 `NVIDIA L40S` 출력 확인 |
| `under-rngd-pod.yaml` | under k8s | `furiosa.ai/rngd` | `harbor.io:7443/library/furiosa-smi-demo:jammy` | 2026-09-17 기준 RNGD 광고 노드 없음, 이미지 받기만 확인 |
| `service-gpu-pod.yaml` | service k8s(Magnum) | `nvidia.com/gpu` | `harbor.internal/library/cuda:12.9.1-base-ubuntu22.04` | 2026-09-17 기준 GPU 노드 없음, 이미지 받기만 확인 |
| `service-rngd-pod.yaml` | service k8s(Magnum), `k8s-furiosa-rngd` | `furiosa.ai/rngd` | `harbor.internal/library/furiosa-smi-demo:jammy` | 2026-09-17 `k8s-furiosa-rngd` worker 에서 `npu0` alive 출력 확인 |

**클러스터 폐기 여부**: `under-*`·`service-*` 네 파일이 겨냥한 under k8s·service k8s 는
`accelerator_pod_test.md` 가 2026-09-17 기준 확인 대상으로 계속 다루는 클러스터이며, 문서
어디에도 폐기 기록이 없다. 따라서 "폐기된 클러스터용 참고" 표기 없이 그대로 이관했다.

## 실행·정리 명령

`.91` 대상 두 파일(`rngd-pod.yaml`, `tt-pod.yaml`)은 상주 파드에 `exec` 로 접속해 확인한다.

```bash
kubectl apply -f rngd-pod.yaml
kubectl exec -it rngd-pod -- furiosa-smi info
kubectl delete -f rngd-pod.yaml
```

```bash
kubectl apply -f tt-pod.yaml
kubectl exec -it tt-pod -- tt-smi -s
kubectl delete -f tt-pod.yaml
```

under k8s·service k8s 대상 네 파일은 명령을 실행하고 종료하는 1회성 파드다(`파일명`·
`파드 이름`만 바꿔 동일하게 적용, service k8s 는 사전에 해당 클러스터 `KUBECONFIG` 지정 필요
— 자세한 내용은 위 문서 2절·5절).

```bash
kubectl apply -f under-gpu-pod.yaml
kubectl get pod gpu-test          # STATUS 가 Completed 가 될 때까지 확인
kubectl logs gpu-test
kubectl delete pod gpu-test
```

두 Dockerfile(`rngd-smi.Dockerfile`, `tt-smi.Dockerfile`)은 내부 Harbor·registry 에 확인용
이미지가 없을 때 인터넷에 연결된 호스트에서 빌드해 올리는 용도다.

```bash
docker build -t <registry>/<repo>:<tag> -f rngd-smi.Dockerfile .
docker push <registry>/<repo>:<tag>
```
