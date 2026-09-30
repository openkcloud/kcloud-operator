# Dockerfile: install/rollback Job 스텁 이미지 — DRIVER_VERSION 과 무관하게 baked exit code 로 즉시 종료
# 상세: fault-injection 시나리오 02 에서 실제 드라이버 설치를 대신하는 최소 이미지.
#       RenderInstallJob/RenderRollbackJob(internal/driverjob/job.go)은 컨테이너 command 를
#       오버라이드하지 않으므로 이 이미지의 CMD 가 유일한 동작이다. STUB_EXIT_CODE 를 빌드
#       인자로 받아 이미지에 굽는다 — 같은 (vendor,model,node) install Job 이름을 재사용하는
#       상태기계 특성상, 태그(버전)별로 다른 이미지를 만들어야 성공/실패 분기를 구분할 수 있다.
#       0=최초 업그레이드 install Job(성공 흉내, 검증은 NDR.driverLoaded=false 로 별도 실패).
#       1=rollback install Job(실패 흉내 — RollbackAttempts 소진을 만든다).
#       test/live/tt-upgrade-transition/stub/Dockerfile(같은 저장소, 기존 작업)과 같은 기법이다.
# 생성일: 2026-09-20
FROM busybox:1.36
ARG STUB_EXIT_CODE=0
ENV STUB_EXIT_CODE=${STUB_EXIT_CODE}
CMD ["sh", "-c", "echo \"fault02-upgrade-stub: exiting ${STUB_EXIT_CODE} (DRIVER_VERSION=${DRIVER_VERSION} VENDOR=${VENDOR} RUN_MODE=${RUN_MODE})\"; exit ${STUB_EXIT_CODE}"]
