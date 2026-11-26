# syntax=docker/dockerfile:1.7
# tg-ws-proxy (Flowseal) + ключ доступа к Cloudflare Worker.
# Повторяет официальный Dockerfile, добавлен только шаг patch_auth.py.

ARG TGWS_REF=main

FROM python:3.12-slim AS builder
ENV PYTHONDONTWRITEBYTECODE=1 PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1 VIRTUAL_ENV=/opt/venv
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential cargo libffi-dev libssl-dev \
    && python -m venv "$VIRTUAL_ENV" \
    && "$VIRTUAL_ENV/bin/pip" install --upgrade pip setuptools wheel \
    && rm -rf /var/lib/apt/lists/*
RUN "$VIRTUAL_ENV/bin/pip" install cryptography==46.0.5 certifi "httpx[http2]==0.28.1"

FROM python:3.12-slim AS source
ARG TGWS_REF
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && git clone https://github.com/Flowseal/tg-ws-proxy.git /src \
    && git -C /src checkout "$TGWS_REF"
COPY patch_auth.py /patch_auth.py
RUN python /patch_auth.py /src

FROM python:3.12-slim AS runtime
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PATH=/opt/venv/bin:$PATH
RUN apt-get update \
    && apt-get install -y --no-install-recommends tini ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system app \
    && useradd --system --gid app --create-home --home-dir /home/app app
WORKDIR /app
COPY --from=builder /opt/venv /opt/venv
COPY --from=source /src/proxy ./proxy
COPY --from=source /src/utils ./utils
USER app

# Параметры — из окружения (см. docker-compose.remote-cf.yml).
# Ключ воркера читается патчем из TG_WS_PROXY_CF_WORKER_AUTH и в командную строку не попадает.
# TG_WS_PROXY_ONLY_WORKER=1 — весь трафик только через свой Worker:
#   --pool-size 0  без прямого WebSocket к Telegram
#   --no-cfproxy   без общих чужих Cloudflare-доменов
#
# TG_WS_PROXY_SECRET/TG_WS_PROXY_CF_WORKER_AUTH могут прийти пустыми: их
# генерирует panel-admin при своём старте (bootstrap(), см.
# panel-admin/main.go), а не host-скрипт ЗАРАНЕЕ -- значит в момент разбора
# compose-файла (когда фиксируются ${VAR}) их ещё может не быть. Если
# переменная пуста -- ждём (до 20с) файл в смонтированном ./config, куда
# их пишет panel-admin, и берём значение оттуда.
ENTRYPOINT ["/usr/bin/tini", "--", "/bin/sh", "-c", "set -eu; secret=\"$TG_WS_PROXY_SECRET\"; if [ -z \"$secret\" ]; then i=0; while [ ! -s /config/tgws_secret ] && [ $i -lt 100 ]; do sleep 0.2; i=$((i+1)); done; secret=\"$(cat /config/tgws_secret 2>/dev/null)\"; fi; auth=\"$TG_WS_PROXY_CF_WORKER_AUTH\"; if [ -z \"$auth\" ] && [ -s /config/worker_auth ]; then auth=\"$(cat /config/worker_auth)\"; fi; export TG_WS_PROXY_CF_WORKER_AUTH=\"$auth\"; extra=''; if [ \"${TG_WS_PROXY_ONLY_WORKER:-0}\" = 1 ]; then extra='--pool-size 0 --no-cfproxy'; echo 'mode: only Cloudflare Worker'; fi; exec /opt/venv/bin/python -u proxy/tg_ws_proxy.py --host \"$TG_WS_PROXY_HOST\" --port \"$TG_WS_PROXY_PORT\" --secret \"$secret\" --cfproxy-worker-domain \"$TG_WS_PROXY_CF_WORKER\" $extra \"$@\"", "--"]
CMD []
