# Официальный MTProxy, тот же закреплённый коммит, что и в deploy/install-mtproxy.sh
FROM debian:12-slim AS build
ARG MTPROXY_COMMIT=f36d8af769ffaeac36978d38c2c0f6d1104c2137
ARG MTPROXY_SHA256=919795c416b870670841a21d1930ad97a24c7b84b9eb8c6f9e3de32f2fdf4655
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl build-essential libssl-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /build
RUN curl -fsSL -o mtproxy.tgz "https://github.com/TelegramMessenger/MTProxy/archive/${MTPROXY_COMMIT}.tar.gz" \
 && echo "${MTPROXY_SHA256}  mtproxy.tgz" | sha256sum -c - \
 && mkdir src && tar -C src --strip-components=1 -xzf mtproxy.tgz \
 && make -C src -j"$(nproc)"

FROM debian:12-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl iproute2 libssl3 zlib1g \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --no-create-home --shell /usr/sbin/nologin mtproxy \
 && install -d -o root -g mtproxy -m 0750 /var/lib/mtproxy
COPY --from=build /build/src/objs/bin/mtproto-proxy /usr/local/bin/mtproto-proxy
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
