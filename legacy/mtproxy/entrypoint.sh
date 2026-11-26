#!/bin/sh
# Запуск официального MTProxy внутри сетевого пространства NPM.
# Слушает 2398 (клиенты, только для relay) и 8888 (статистика).
set -eu

: "${MTPROXY_SECRET:?MTPROXY_SECRET не задан (32 hex-символа, тот же, что в profiles.json)}"
: "${PUBLIC_IP:?PUBLIC_IP не задан (внешний IPv4 сервера)}"
WORKERS="${MTPROXY_WORKERS:-1}"
MAXCONN="${MTPROXY_MAX_CONNECTIONS:-4096}"
DIR=/var/lib/mtproxy

fetch() {
	curl -fsS --proto '=https' --tlsv1.2 -o "$DIR/proxy-secret.new" https://core.telegram.org/getProxySecret
	curl -fsS --proto '=https' --tlsv1.2 -o "$DIR/proxy-multi.conf.new" https://core.telegram.org/getProxyConfig
	[ "$(wc -c < "$DIR/proxy-secret.new")" -eq 128 ]
	grep -q '^proxy_for ' "$DIR/proxy-multi.conf.new"
	chgrp mtproxy "$DIR"/*.new && chmod 0640 "$DIR"/*.new
}

fetch
mv "$DIR/proxy-secret.new" "$DIR/proxy-secret"
mv "$DIR/proxy-multi.conf.new" "$DIR/proxy-multi.conf"

# Контейнер NPM стоит за docker-NAT. Без --nat-info MTProxy молча не работает:
# middle-end обрывает соединения сразу после рукопожатия (см. README апстрима).
LOCAL_IP="$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }}')"
NAT_ARGS=""
if [ -n "$LOCAL_IP" ] && [ "$LOCAL_IP" != "$PUBLIC_IP" ]; then
	NAT_ARGS="--nat-info ${LOCAL_IP}:${PUBLIC_IP}"
fi
echo "mtproxy: local ${LOCAL_IP:-?} public ${PUBLIC_IP} workers ${WORKERS}"

# Раз в сутки обновляем маршрутную конфигурацию Telegram; если она изменилась —
# завершаем процесс, и docker (restart: unless-stopped) поднимает его заново.
(
	while sleep 86400; do
		if fetch && ! cmp -s "$DIR/proxy-multi.conf.new" "$DIR/proxy-multi.conf"; then
			mv "$DIR/proxy-secret.new" "$DIR/proxy-secret"
			mv "$DIR/proxy-multi.conf.new" "$DIR/proxy-multi.conf"
			echo "mtproxy: config changed, restarting"
			kill -TERM 1
		fi
	done
) &

# shellcheck disable=SC2086
exec /usr/local/bin/mtproto-proxy -u mtproxy -p 8888 -H 2398 -S "$MTPROXY_SECRET" $NAT_ARGS \
	--aes-pwd "$DIR/proxy-secret" "$DIR/proxy-multi.conf" -M "$WORKERS" -C "$MAXCONN"
