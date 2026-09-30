# ============================================================
# Dockerfile: tt-metalium 파생 이미지 — ttnn import 용 torch 추가(베이스 태그 인자화)
# 상세: 베이스 이미지의 ttnn 은 operations/complex_binary_backward.py 가 torch 를 무조건
#       import 하므로 torch 없이는 `python3 -c 'import ttnn'` 가 ModuleNotFoundError 로 실패한다.
#       CPU 전용 torch 2.2.1 을 ttnn 이 설치된 venv(/opt/venv)의 site-packages 에 넣어 import 를
#       통과시킨다(장치 연산은 ttnn 이 tt-metal 로 수행, torch 는 import 의존성만 충족).
#       venv 에 pip 모듈이 없는 베이스(예: v0.79.0)에서는 bare `pip` 이 시스템 pip(/usr/bin/pip)로
#       빠져 dist-packages 에 설치되고 venv python3 이 그걸 못 본다. 그래서 venv python3 로 시스템
#       pip 모듈을 PYTHONPATH 로 빌려 venv site-packages 에 설치한다.
#       BASE_IMAGE 인자로 베이스 tt-metalium 태그를 교체해 카드 펌웨어와 맞는 런타임으로 재빌드.
# 생성일: 2026-09-22 | 수정일: 2026-09-23
# ============================================================
ARG BASE_IMAGE=<레지스트리>:5100/kcloud/tenstorrent/tt-metalium:v0.58.0-rc4
FROM ${BASE_IMAGE}
RUN PYTHONPATH=/usr/lib/python3/dist-packages python3 -m pip install --no-cache-dir torch==2.2.1 --index-url https://download.pytorch.org/whl/cpu
