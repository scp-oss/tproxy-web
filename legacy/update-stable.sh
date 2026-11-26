#!/usr/bin/env bash
# Перевод уже работающей установки (docker-compose.remote-cf.yml) на настройки
# для максимальной стабильности на телефонах. Секрет, ключи, base path и ссылка
# для Telegram НЕ меняются.
#
#   sudo ./update-stable.sh
#
# Что делает:
#   1. транспорт relay -> https-lanes (переживает смену сети/обрывы, медиа не тормозит чаты);
#   2. reconnect_grace -> 10m (сессия переживает уход телефона в фон);
#   3. TGWS_ONLY_WORKER=0, если не задан (прямой путь быстрее Worker'а);
#   4. проверяет конфиг, пересобирает и перезапускает контейнеры.
set -euo pipefail
cd "$(dirname "$0")"
COMPOSE=(docker compose -f docker-compose.remote-cf.yml)

[[ -f config/profiles.json && -f config/config.json && -f .env ]] || {
	echo "нет config/profiles.json, config/config.json или .env — сначала запустите init.sh" >&2; exit 1; }

cp -p config/profiles.json config/profiles.json.bak
cp -p config/config.json config/config.json.bak

sed -i -E 's/"carrier_mode": *"[a-z-]+"/"carrier_mode": "https-lanes"/' config/profiles.json
sed -i -E 's/"reconnect_grace": *"[0-9a-z]+"/"reconnect_grace": "10m"/' config/config.json config/config.json.template
chown 65532:65532 config/profiles.json && chmod 600 config/profiles.json
chmod 644 config/config.json

grep -q '^TGWS_ONLY_WORKER=' .env || echo 'TGWS_ONLY_WORKER=0' >> .env

echo "Режим:   $(grep -o '"carrier_mode": *"[a-z-]*"' config/profiles.json)"
echo "Сессия:  $(grep -o '"reconnect_grace": *"[0-9a-z]*"' config/config.json)"
echo "Выход:   $(grep '^TGWS_ONLY_WORKER=' .env)"

"${COMPOSE[@]}" build relay tgws
if ! "${COMPOSE[@]}" run --rm --no-deps relay -config /config/config.json -check; then
	echo "конфиг не прошёл проверку — возвращаю прежний" >&2
	mv config/profiles.json.bak config/profiles.json
	mv config/config.json.bak config/config.json
	exit 1
fi
"${COMPOSE[@]}" up -d
rm -f config/profiles.json.bak config/config.json.bak

cat <<'EOF'

Готово. Ссылка и секрет для Telegram прежние.
Не забудьте задеплоить новый cloudflare/worker.js:
  Cloudflare → Workers & Pages → edge-sync → Edit code → Ctrl+A → вставить → Deploy
Затем:  docker compose -f docker-compose.remote-cf.yml restart tgws
EOF
