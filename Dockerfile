# syntax=docker/dockerfile:1.7
# tproxy-web: один образ, один контейнер. Внутри tproxyd (панель + вход + управление
# модулями) и то, что он запускает: tproxy-server, tg-ws-proxy, sing-box.

# Закреплённые версии чужого кода: обновлять осознанно, с проверкой.
ARG TPROXY_REF=c8adb8b7c6b7fc46c12ae3acb68be9070c26a8e8
ARG TGWS_REF=9f71904e1a4a282fd9e5044bbb71d59f4d30576b
ARG SINGBOX_VERSION=1.12.12
# Официальный клиент WireGuard для Android (подпись WireGuard LLC), раздаётся из панели:
# у провайдера может быть заблокирован магазин/сайт, а панель в своей сети доступна всегда.
ARG WG_APK=com.wireguard.android-1.0.20260315.apk
ARG WG_APK_SHA256=f92971bc804f4c448e8845ae97e426095eab06ec09a2473a3fa2cfe7288e3298
# Официальный клиент sing-box для Android (SFA): умеет добавлять профиль по ссылке в одно нажатие.
ARG SFA_ARM64_SHA256=63d532380625ab441c5cb5bcbba11fbe8341fb31c62f3be7587b627cd3d79da2
ARG SFA_ARMV7_SHA256=6644254753b8b2471806f798cabc411fe442657f07f79441773d9cb2e3ffceff

FROM golang:1.24-bookworm AS relay
ARG TPROXY_REF
WORKDIR /src
RUN git clone https://github.com/telegramdesktop/tproxy-server.git . && git checkout "$TPROXY_REF" \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/tproxy-server ./cmd/tproxy-server

FROM golang:1.26-bookworm AS tproxyd
WORKDIR /src
COPY tproxyd/go.mod tproxyd/go.sum* ./
COPY tproxyd/*.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/tproxyd .

FROM python:3.12-slim AS tgws
ARG TGWS_REF
ENV PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
 && python -m venv /opt/venv \
 && /opt/venv/bin/pip install cryptography==46.0.5 certifi "httpx[http2]==0.28.1" \
 && git clone https://github.com/Flowseal/tg-ws-proxy.git /src && git -C /src checkout "$TGWS_REF"
COPY tgws/patch_auth.py /patch_auth.py
RUN python /patch_auth.py /src

FROM ghcr.io/sagernet/sing-box:v${SINGBOX_VERSION} AS singbox

FROM alpine:3.20 AS wgapk
ARG WG_APK
ARG WG_APK_SHA256
ARG SINGBOX_VERSION
ARG SFA_ARM64_SHA256
ARG SFA_ARMV7_SHA256
RUN wget -q -O /wireguard.apk "https://download.wireguard.com/android-client/${WG_APK}" \
 && echo "${WG_APK_SHA256}  /wireguard.apk" | sha256sum -c - \
 && wget -q -O /sing-box.apk "https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/SFA-${SINGBOX_VERSION}-arm64-v8a.apk" \
 && echo "${SFA_ARM64_SHA256}  /sing-box.apk" | sha256sum -c - \
 && wget -q -O /sing-box-armv7.apk "https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/SFA-${SINGBOX_VERSION}-armeabi-v7a.apk" \
 && echo "${SFA_ARMV7_SHA256}  /sing-box-armv7.apk" | sha256sum -c -

FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
RUN apt-get update && apt-get install -y --no-install-recommends tini ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=tgws /opt/venv /opt/venv
COPY --from=tgws /src/proxy /app/proxy
COPY --from=tgws /src/utils /app/utils
COPY --from=relay /out/tproxy-server /usr/local/bin/tproxy-server
COPY --from=singbox /usr/local/bin/sing-box /usr/local/bin/sing-box
COPY --from=tproxyd /out/tproxyd /usr/local/bin/tproxyd
COPY --from=wgapk /wireguard.apk /sing-box.apk /sing-box-armv7.apk /usr/share/tproxyd/
COPY tproxyd/admin.html tproxyd/config.json.template /usr/share/tproxyd/
VOLUME /config
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/tproxyd"]
