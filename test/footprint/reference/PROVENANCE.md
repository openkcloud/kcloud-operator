<!-- PROVENANCE.md: footprint 측정에 쓰는 벤더 차트의 출처·버전·해시 고정 기록 | 생성일: 2026-09-21 -->

# 벤더 차트 출처 고정

`measure.sh` 의 정적·라이브 측정이 참조하는 두 벤더 Helm 차트의 출처를 여기 고정한다.
둘 다 이 문서에 적은 URL·버전·sha256 과 다른 파일이 들어오면 측정이 실패해야 한다.

## nvidia-gpu-operator

| 항목 | 값 |
|---|---|
| 출처 | https://github.com/NVIDIA/gpu-operator (Helm 저장소: `https://helm.ngc.nvidia.com/nvidia`) |
| 받는 법 | `helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm pull nvidia/gpu-operator --version v25.10.1` |
| 차트 버전 | v25.10.1 |
| 파일 | `gpu-operator-v25.10.1.tgz` |
| sha256 | `e13359580e9e0bfbf560fb38e39911b76a17bdbb5b0110fdd911f22467c8d339` |
| 크기 | 43,942 bytes |
| 라이선스 | Apache License 2.0(저장소 `LICENSE`, 재배포 허용) |
| 저장소 내 위치 | `test/footprint/reference/gpu-operator-v25.10.1.tgz`(커밋됨, 수정 없이 그대로) |
| chart_source() 기본값 | 이 파일. `NVIDIA_TGZ` 환경변수로 다른 경로 override 가능(단 내용은 위 sha256 과 같아야 한다 — sha256 은 코드 상수라 값이 다른 파일은 실행 시 exit 1) |

무수정 재배포이므로 Apache-2.0 4조(파생물이 아닌 원본 배포)를 그대로 만족한다.

## rbln-npu-operator

| 항목 | 값 |
|---|---|
| 출처 | https://github.com/RBLN-SW/rbln-npu-operator |
| 릴리스 | v0.5.0(2026-08-28 게시) |
| 자산 | `rbln-npu-operator-chart-0.5.0.tgz` |
| 다운로드 URL | `https://github.com/RBLN-SW/rbln-npu-operator/releases/download/v0.5.0/rbln-npu-operator-chart-0.5.0.tgz` |
| sha256 | `a33880c79f73b54f837706970ab22fbf901030c1fd11f56a90ed672f5fd6de3c` |
| 크기 | 74,995 bytes |
| 라이선스 | Rebellions "Software User License Agreement"(사유 라이선스). 소스코드 배포 동의 없이 복사·재배포를 금지하는 조항이 있어 재배포 허용 여부가 불명확하다 |
| 저장소 내 위치 | 커밋하지 않음. `test/footprint/reference/rbln-npu-operator-chart-0.5.0.tgz` 는 `.gitignore` 대상이며, `measure.sh` 의 `ensure_rbln_chart()` 가 실행 시 위 URL 에서 받아 이 자리에 캐시하고 sha256 을 대조한다. 이미 있으면 sha256 만 대조하고 재다운로드하지 않는다. 대조 불일치는 손상/변조로 보고 즉시 실패한다. 네트워크가 없어 받지 못하면(오프라인) 그 회차의 rbln 정적 값을 `null` 로 남기고 사유를 limits 에 적는다 |
| chart_source() 기본값 | 위 캐시 경로. `RBLN_TGZ` 환경변수로 override 가능(단 내용은 위 sha256 과 같아야 한다 — sha256 은 코드 상수라 값이 다른 파일은 실행 시 exit 1) |

### 이전 로컬 참조 사본과의 관계

이 고정 이전에는 `measure.sh` 가 저장소 밖의 `$OPERATOR_ROOT/reference/rbln-npu-operator/deployments/rbln-npu-operator`
(이 저장소의 형제 디렉터리, `kcloud-operator` 저장소만 체크아웃하면 존재하지 않는 경로)를 가리켰다. 그 경로는
`https://github.com/RBLN-SW/rbln-npu-operator` 를 shallow clone 한 것으로 커밋 `04447fd`(2026-07-29,
"hotfix: url fix (#89)") 상태이며 `Chart.yaml` 의 `version` 이 `0.0.1` 이다. `v0.5.0` 릴리스(2026-08-28)보다
약 한 달 앞선 `main` 스냅샷이라 태그된 릴리스와 매치되지 않는다. `Chart.yaml`·`values.yaml`·RBAC 템플릿
(`role-secret-reader.yaml`·`rolebinding-secret-reader.yaml` 이 v0.5.0 에만 있음) 모두 내용이 달라 단순
재라벨링이 아닌 실제 버전 차이다. 이번 고정으로 `chart_source()` 는 이 로컬 사본을 더 이상 쓰지 않고 위
sha256 고정 릴리스로 교체했다 — 재현 가능하고(저장소 밖 형제 디렉터리에 의존하지 않음) 버전이 확인된
출처이기 때문이다. 이 교체로 `rbln-npu-operator` 의 정적 측정값(리소스 개수)이 이전 측정 대비 달라질 수
있으며, 그 차이는 벤더 차트가 바뀌었기 때문이지 kcloud 측정 로직 변경 때문이 아니다.
(`test/footprint/baseline.json` 은 `targets.kcloud.static` 만 담아 이 교체의 영향을 받지 않는다.)
