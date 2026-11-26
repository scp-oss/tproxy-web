#!/usr/bin/env bash
# Первичная настройка: секрет, ключ токенов, base path, конфиги, .env.
# Повторный запуск не меняет уже созданные секрет, ключ и base path.
#
#   sudo ./init.sh --hostname proxy.example.com --npm-container nginx-proxy-manager
#   опции: --public-ip 1.2.3.4   (иначе определяется автоматически)
#          --base-path none|<slug> (по умолчанию случайный slug)
#          --carrier https|https-lanes|websocket|websocket-lanes
#                    (по умолчанию https-lanes — лучше всего для телефонов;
#                     при повторном запуске сохраняется уже выбранный)
#
# Удалённый режим (NPM на ДРУГОМ сервере, здесь NPM нет):
#   sudo ./init.sh --remote --hostname mail.example.ru --npm-ip <IP главного NPM> [--gateway-port 18443]
#   затем: docker compose -f docker-compose.remote.yml up -d --build
#
# Удалённый режим с ВАШИМ MTProto-прокси вместо официального MTProxy
# (если прямой доступ к Telegram закрыт):
#   sudo ./init.sh --remote --hostname mail.example.ru --npm-ip <IP NPM> \
#        --backend-addr tg-proxy-backend:<порт> --backend-network <docker-сеть>
#   секрет вашего прокси спросит без эха (или --secret <hex>)
#   затем: docker compose -f docker-compose.remote-ext.yml up -d --build
#
# Удалённый режим с tg-ws-proxy и выходом в Telegram через Cloudflare Worker:
#   sudo ./init.sh --remote --hostname mail.example.ru --npm-ip <IP NPM> \
#        --cf-worker my-worker.username.workers.dev
#   затем: docker compose -f docker-compose.remote-cf.yml up -d --build
#
# Бэкенд за ЧУЖИМ/внешним NPM, или вообще без NPM (например за Cloudflare
# в режиме Full SSL): публичный TLS поднимает контейнеризованный Caddy
# (сервис caddy в самом compose-файле, самоподписанный серт) -- весь стек
# целиком через docker compose, без единого host-level сервиса (подходит
# для Synology DSM). Если нужен Proxy Host на их NPM -- см. шаблон ниже:
#   sudo ./init.sh --remote --backend-only --hostname mail.example.ru \
#        --cf-worker my-worker.username.workers.dev
#   затем: docker compose -f docker-compose.remote-cf-caddy.yml up -d --build
#
# Добавить классический MTProto-прокси (fake-TLS, для настоящих MTProto-клиентов,
# не WEB) рядом с любым из режимов выше, которые используют --cf-worker -- свой
# порт, та же устойчивость через Cloudflare Worker, что и у WEB-прокси:
#   ... --mtproto-port 8443 --fake-tls-domain www.google.com
# Мини-панель со ссылками генерируется всегда (config/admin_path) -- её адрес
# печатается в конце вывода.
set -euo pipefail
umask 077
cd "$(dirname "$0")"

HOSTNAME_ARG="" NPM_CONTAINER="" PUBLIC_IP="" BASE_PATH="" CARRIER=""
REMOTE=0 NPM_IP="" GATEWAY_PORT=18443
BACKEND_ADDR="" BACKEND_NETWORK="" SECRET_ARG="" CF_WORKER=""
BACKEND_ONLY=0
MTPROTO_PORT="" FAKE_TLS_DOMAIN=""
SOCKS5_PORT=""
while [[ $# -gt 0 ]]; do
	case "$1" in
		--hostname) HOSTNAME_ARG="$2"; shift 2 ;;
		--remote) REMOTE=1; shift ;;
		--npm-ip) NPM_IP="$2"; shift 2 ;;
		--gateway-port) GATEWAY_PORT="$2"; shift 2 ;;
		--npm-container) NPM_CONTAINER="$2"; shift 2 ;;
		--public-ip) PUBLIC_IP="$2"; shift 2 ;;
		--base-path) BASE_PATH="$2"; shift 2 ;;
		--carrier) CARRIER="$2"; shift 2 ;;
		--backend-addr) BACKEND_ADDR="$2"; shift 2 ;;
		--backend-network) BACKEND_NETWORK="$2"; shift 2 ;;
		--secret) SECRET_ARG="$2"; shift 2 ;;
		--cf-worker) CF_WORKER="$2"; shift 2 ;;
		--backend-only) BACKEND_ONLY=1; shift ;;
		--mtproto-port) MTPROTO_PORT="$2"; shift 2 ;;
		--fake-tls-domain) FAKE_TLS_DOMAIN="$2"; shift 2 ;;
		--socks5-port) SOCKS5_PORT="$2"; shift 2 ;;
		*) echo "неизвестный аргумент: $1" >&2; exit 2 ;;
	esac
done
[[ -n "$HOSTNAME_ARG" ]] || { echo "нужен --hostname" >&2; exit 2; }
if [[ "$REMOTE" == 1 ]]; then
	if [[ "$BACKEND_ONLY" == 1 ]]; then
		[[ -z "$NPM_IP" ]] || echo "--backend-only: --npm-ip не используется (gateway-контейнера нет), игнорирую" >&2
	else
		[[ -n "$NPM_IP" ]] || { echo "в --remote нужен --npm-ip (IP сервера с главным NPM)" >&2; exit 2; }
	fi
	NPM_CONTAINER="-"
	if [[ -n "$CF_WORKER" && -n "$BACKEND_ADDR" ]]; then
		echo "--cf-worker и --backend-addr — разные режимы, выберите один" >&2; exit 2
	fi
	if [[ -n "$CF_WORKER" ]]; then
		CF_WORKER="$(printf %s "$CF_WORKER" | sed -e 's|^https\?://||' -e 's|/.*$||' | tr 'A-Z' 'a-z')"
		[[ "$CF_WORKER" == *.* ]] || { echo "--cf-worker: нужен домен воркера, например name.user.workers.dev" >&2; exit 2; }
	fi
	if [[ -n "$BACKEND_ADDR" ]]; then
		[[ -n "$BACKEND_NETWORK" ]] || { echo "с --backend-addr нужен --backend-network (docker-сеть вашего прокси)" >&2; exit 2; }
	fi
else
	[[ -n "$NPM_CONTAINER" ]] || { echo "нужен --npm-container (имя контейнера NPM: docker ps)" >&2; exit 2; }
fi
HOSTNAME_ARG="$(printf %s "$HOSTNAME_ARG" | tr 'A-Z' 'a-z')"
if [[ -z "$CARRIER" ]]; then
	# повторный запуск: оставить режим, выбранный ранее
	CARRIER="$(grep -s -o '"carrier_mode": *"[a-z-]*"' config/profiles.json | head -1 | sed 's/.*"\([a-z-]*\)"$/\1/' || true)"
	[[ -n "$CARRIER" ]] || CARRIER="https-lanes"
fi
case "$CARRIER" in https|https-lanes|websocket|websocket-lanes) ;; *) echo "плохой --carrier" >&2; exit 2 ;; esac
if [[ -n "$MTPROTO_PORT" || -n "$FAKE_TLS_DOMAIN" ]]; then
	[[ -n "$MTPROTO_PORT" && -n "$FAKE_TLS_DOMAIN" ]] || { echo "--mtproto-port и --fake-tls-domain нужны вместе" >&2; exit 2; }
	[[ "$MTPROTO_PORT" =~ ^[0-9]+$ ]] || { echo "--mtproto-port должен быть числом" >&2; exit 2; }
fi
if [[ -n "$SOCKS5_PORT" ]]; then
	[[ "$SOCKS5_PORT" =~ ^[0-9]+$ ]] || { echo "--socks5-port должен быть числом" >&2; exit 2; }
fi

# .env ниже перезаписывается целиком при каждом запуске -- если флаг не
# передали СЕЙЧАС, подтягиваем то, что уже настроили раньше (панелью или
# прошлым запуском init.sh), а не молча обнуляем. Явно переданный флаг
# всегда главнее того, что уже есть в .env.
_env_get() { grep -s "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- || true; }
[[ -n "$MTPROTO_PORT" ]]    || MTPROTO_PORT="$(_env_get MTPROTO_PORT)"
[[ -n "$FAKE_TLS_DOMAIN" ]] || FAKE_TLS_DOMAIN="$(_env_get MTPROTO_FAKE_TLS_DOMAIN)"
[[ -n "$SOCKS5_PORT" ]]     || SOCKS5_PORT="$(_env_get SOCKS5_PORT)"

if [[ -z "$PUBLIC_IP" ]]; then
	PUBLIC_IP="$(curl -4 -fsS --max-time 10 https://api.ipify.org || true)"
	if [[ -z "$PUBLIC_IP" ]]; then
		if [[ -n "$BACKEND_ADDR" || -n "$CF_WORKER" ]]; then
			PUBLIC_IP="-"   # без официального MTProxy не нужен
		else
			echo "не удалось определить внешний IP, задайте --public-ip" >&2; exit 1
		fi
	fi
fi

mkdir -p config
# --- секрет (общий для relay-профиля и MTProxy) ---
if [[ -n "$BACKEND_ADDR" && -z "$SECRET_ARG" ]]; then
	read -rsp "Секрет вашего MTProto-прокси (32 hex или dd + 32 hex): " SECRET_ARG; echo
fi
if [[ -n "$SECRET_ARG" ]]; then
	SECRET_ARG="$(printf %s "$SECRET_ARG" | tr -d '[:space:]' | tr 'A-F' 'a-f')"
	if [[ "$SECRET_ARG" == ee* ]]; then
		echo "секрет ee… (fake-TLS) relay не поддерживает; нужен обычный или dd-секрет того же прокси" >&2; exit 1
	fi
	[[ "$SECRET_ARG" =~ ^(dd)?[0-9a-f]{32}$ ]] || { echo "секрет должен быть 32 hex или dd + 32 hex" >&2; exit 1; }
	printf '%s\n' "$SECRET_ARG" > config/secret.hex
fi
if [[ -f config/secret.hex ]]; then
	SECRET="$(cat config/secret.hex)"
else
	SECRET="$(openssl rand -hex 16)"
	printf '%s\n' "$SECRET" > config/secret.hex
fi
TGWS_SECRET="" CF_WORKER_AUTH=""
if [[ -n "$CF_WORKER" ]]; then
	TGWS_SECRET="${SECRET#dd}"
	SECRET="dd$TGWS_SECRET"
	# ключ доступа к воркеру: создаётся один раз, хранится в config/worker_auth
	if [[ -f config/worker_auth ]]; then
		CF_WORKER_AUTH="$(cat config/worker_auth)"
	else
		CF_WORKER_AUTH="$(head -c 24 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')"
		printf '%s\n' "$CF_WORKER_AUTH" > config/worker_auth
	fi
fi
# --- base path ---
if [[ -f config/base_path ]]; then
	BASE_PATH="$(cat config/base_path)"
else
	if [[ -z "$BASE_PATH" ]]; then
		BASE_PATH="$(head -c 10 /dev/urandom | base32 | tr 'A-Z' 'a-z')"
	elif [[ "$BASE_PATH" == "none" ]]; then
		BASE_PATH=""
	fi
	printf '%s' "$BASE_PATH" > config/base_path
fi
# --- ключ подписи токенов: ровно 32 байта, права 0600 ---
[[ -f config/token.key ]] || head -c 32 /dev/urandom > config/token.key

# --- отдельный секрет для MTProto (НЕ путать с WEB-прокси секретом выше --
# разные протоколы, разные секреты, компрометация одного не задевает другой) ---
if [[ -f config/mtproto_secret.hex ]]; then
	MTPROTO_SECRET="$(cat config/mtproto_secret.hex)"
else
	MTPROTO_SECRET="$(openssl rand -hex 16)"
	printf '%s\n' "$MTPROTO_SECRET" > config/mtproto_secret.hex
fi

# --- логин/пароль для SOCKS5 -- генерируются ВСЕГДА, не только когда вы
# явно включаете SOCKS5 флагами/панелью. Так compose-файл всегда может
# поднять сервис с реальными правами (serjs/go-socks5-proxy без них
# отказывается стартовать вовсе), даже если его подняли вручную через SSH
# в обход panel-admin -- та же логика, что уже была у MTPROTO_SECRET выше,
# просто теперь применена последовательно для всех протоколов ---
if [[ -f config/socks5_pass.hex ]]; then
	SOCKS5_PASS="$(cat config/socks5_pass.hex)"
else
	SOCKS5_PASS="$(openssl rand -hex 12)"
	printf '%s\n' "$SOCKS5_PASS" > config/socks5_pass.hex
fi
if [[ -f config/socks5_user ]]; then
	SOCKS5_USER="$(cat config/socks5_user)"
else
	SOCKS5_USER="user$(openssl rand -hex 3)"
	printf '%s' "$SOCKS5_USER" > config/socks5_user
fi

# --- путь до мини-панели со ссылками: тот же принцип, что у base_path --
# длинный случайный путь и есть ключ, на сервере ничего не проверяется ---
if [[ -f config/admin_path ]]; then
	ADMIN_PATH="$(cat config/admin_path)"
else
	ADMIN_PATH="panel-$(head -c 32 /dev/urandom | base32 | tr 'A-Z' 'a-z' | tr -d '=')"
	printf '%s' "$ADMIN_PATH" > config/admin_path
fi

# --- токен для panel-admin (обычный сервис compose, см. ниже) -- второй
# слой поверх длинного пути: даже если что-то другое в сети этого
# compose-проекта достучится до него в обход nginx, без токена он откажет ---
if [[ -f config/panel_token ]]; then
	PANEL_TOKEN="$(cat config/panel_token)"
else
	PANEL_TOKEN="$(openssl rand -hex 32)"
	printf '%s' "$PANEL_TOKEN" > config/panel_token
fi
# panel-admin -- обычный сервис compose (см. ниже), ему нужно знать
# абсолютный путь проекта (для docker-outside-of-docker volume-монтирования
# по тому же пути) и имя compose-файла, которым сам себя же и перезапускает.
PROJECT_DIR_ABS="$(pwd)"
PANEL_COMPOSE_FILE="docker-compose.remote-cf-caddy.yml"
[[ "$BACKEND_ONLY" == 1 ]] || PANEL_COMPOSE_FILE="docker-compose.remote-cf.yml"

sed -e "s|__HOSTNAME__|$HOSTNAME_ARG|" -e "s|__BASE_PATH__|$BASE_PATH|" \
	config/config.json.template > config/config.json
# cover теперь свой Go-бинарник (см. cover/main.go) -- ADMIN_PATH/PANEL_TOKEN
# читает напрямую из окружения compose (.env ниже), шаблон/sed не нужен.
# profiles.json хочет ГОЛЫЙ hex без dd/ee -- это маркер исключительно
# клиентского MTProto fake-TLS секрета, в профиле relay ему не место
# (иначе relay никогда не матчит реальный секрет клиента и молча
# откатывается на сайт-прикрытие -- без единой ошибки в логах).
BARE_SECRET="${SECRET#dd}"
cat > config/profiles.json <<EOF
{
  "profiles": [
    {
      "name": "default",
      "secret": "$BARE_SECRET",
      "backend": "127.0.0.1:2398",
      "carrier_mode": "$CARRIER"
    }
  ]
}
EOF
# переключатель режима переживает повторный запуск init.sh
TGWS_ONLY_WORKER="$(grep -s '^TGWS_ONLY_WORKER=' .env | tail -1 | cut -d= -f2- || true)"
cat > .env <<EOF
NPM_CONTAINER=$NPM_CONTAINER
PUBLIC_IP=$PUBLIC_IP
MTPROXY_SECRET=$SECRET
MTPROXY_WORKERS=1
MTPROXY_MAX_CONNECTIONS=4096
TPROXY_REF=master
NPM_IP=$NPM_IP
GATEWAY_PORT=$GATEWAY_PORT
BACKEND_ADDR=$BACKEND_ADDR
BACKEND_NETWORK=$BACKEND_NETWORK
CF_WORKER=$CF_WORKER
MTPROTO_SECRET=$MTPROTO_SECRET
MTPROTO_FAKE_TLS_DOMAIN=$FAKE_TLS_DOMAIN
MTPROTO_PORT=$MTPROTO_PORT
SOCKS5_USER=$SOCKS5_USER
SOCKS5_PASS=$SOCKS5_PASS
SOCKS5_PORT=$SOCKS5_PORT
TGWS_SECRET=$TGWS_SECRET
CF_WORKER_AUTH=$CF_WORKER_AUTH
TGWS_ONLY_WORKER=${TGWS_ONLY_WORKER:-0}
TGWS_REF=main
PANEL_TOKEN=$PANEL_TOKEN
ADMIN_PATH=$ADMIN_PATH
HOSTNAME_ARG=$HOSTNAME_ARG
PROJECT_DIR=$PROJECT_DIR_ABS
PANEL_COMPOSE_FILE=$PANEL_COMPOSE_FILE
EOF

# удалённый режим: самоподписанный сертификат для участка NPM → этот сервер
# (не нужен в --backend-only: там нет своего gateway-контейнера с TLS)
if [[ "$REMOTE" == 1 && "$BACKEND_ONLY" == 0 ]]; then
	mkdir -p remote/certs
	if [[ ! -f remote/certs/gateway.crt ]]; then
		openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=$HOSTNAME_ARG" \
			-keyout remote/certs/gateway.key -out remote/certs/gateway.crt 2>/dev/null
	fi
	chmod 0755 remote/certs
	chmod 0644 remote/certs/gateway.crt
	chmod 0600 remote/certs/gateway.key
fi

# relay работает как uid 65532 и отказывается стартовать при широких правах
chmod 0755 config
chmod 0644 config/config.json config/config.json.template
chmod 0600 config/profiles.json config/token.key config/secret.hex config/base_path \
	config/mtproto_secret.hex config/admin_path config/panel_token \
	config/socks5_pass.hex config/socks5_user .env
[[ -f config/worker_auth ]] && chmod 0600 config/worker_auth
chown 65532:65532 config/profiles.json config/token.key

# panel-admin -- обычный сервис compose теперь (не host-демон под systemd,
# см. docker-compose.*.yml и panel-admin/Dockerfile) -- install.sh поднимает
# его вместе с tgws/relay/cover как часть базового набора, отдельно
# собирать/ставить тут ничего не нужно.

# --- данные для клиента ---
if [[ -z "$BASE_PATH" ]]; then
	SERVER="$HOSTNAME_ARG"
	CLIENT_SECRET="$SECRET"
	LINK_SERVER="$SERVER"
else
	SERVER="$HOSTNAME_ARG/$BASE_PATH"
	# клиентский secret= для base path = 0x70 || ГОЛЫЙ секрет -> base64url
	# без паддинга (см. BARE_SECRET выше); то же значение, не hex, идёт и
	# в поле "Секрет" при ручном вводе в Telegram.
	CLIENT_SECRET="$({ printf '\x70'; printf "$(printf %s "$BARE_SECRET" | sed 's/../\\x&/g')"; } \
		| base64 | tr '+/' '-_' | tr -d '=\n')"
	LINK_SERVER="$HOSTNAME_ARG%2F$BASE_PATH"
fi

WEB_LINK="https://t.me/webproxy?server=$LINK_SERVER&secret=$CLIENT_SECRET"

# --- MTProto (если включён --mtproto-port/--fake-tls-domain) -- печатается
# в вывод ниже; живое состояние (вкл/выкл) мини-панель теперь берёт сама
# через panel-admin (JS, см. site/admin/index.html.template), а не из
# статической подстановки при генерации страницы ---
MTPROTO_LINK=""
if [[ -n "$MTPROTO_PORT" ]]; then
	DOMAIN_HEX="$(printf %s "$FAKE_TLS_DOMAIN" | od -An -tx1 | tr -d ' \n')"
	MTPROTO_FULL_SECRET="ee${MTPROTO_SECRET}${DOMAIN_HEX}"
	MTPROTO_LINK="tg://proxy?server=$HOSTNAME_ARG&port=$MTPROTO_PORT&secret=$MTPROTO_FULL_SECRET"
fi

# --- мини-панель: статическая обвязка (заголовок + всегда-активная
# WEB-ссылка) рендерится сейчас, а переключатели MTProto/SOCKS5 сами тянут
# живое состояние через panel-admin при открытии страницы -- чистая
# bash-подстановка, без sed (в ссылках есть &, которые sed трактует особо)
# и без новых зависимостей вроде python3 ---
ADMIN_HTML="$(cat site/admin/index.html.template)"
# ВАЖНО: $-переменная в правой части ${var//pat/repl} ОБЯЗАНА быть в кавычках --
# без них bash трактует & внутри неё как в sed (== "вставить совпадение") и
# портит любое значение со знаком & (а оно почти всегда есть в наших ссылках).
ADMIN_HTML="${ADMIN_HTML//__WEB_LINK__/"$WEB_LINK"}"
ADMIN_HTML="${ADMIN_HTML//__HOSTNAME__/"$HOSTNAME_ARG"}"
mkdir -p site/admin
printf '%s\n' "$ADMIN_HTML" > site/admin/index.html
# umask 077 (см. начало скрипта) иначе даёт 0600 на СВЕЖЕМ файле -- он же
# отдаётся nginx в ДРУГОМ контейнере под другим uid через read-only volume,
# читать не сможет (ровно так и было -- 403/Permission denied на первом же
# реальном деплое, пока не нашли и не поправили). На уже существующем файле
# umask ни при чём (> не трогает права при truncate) -- баг именно на первом
# запуске с нуля, поэтому легко было не заметить при повторных тестах.
chmod 0644 site/admin/index.html

cat <<EOF

Готово.
  Внешний IP:        $PUBLIC_IP
  Контейнер NPM:     $NPM_CONTAINER
  Base path:         ${BASE_PATH:-(корень)}

В Telegram (тип прокси WEB):
  Сервер:            $SERVER
  Секрет (вручную):  $CLIENT_SECRET
  Ссылка:            $WEB_LINK

EOF
if [[ -n "$MTPROTO_PORT" ]]; then
	cat <<EOF
В Telegram (тип прокси MTProto, обычный, не WEB):
  Сервер:            $HOSTNAME_ARG
  Порт:              $MTPROTO_PORT
  Секрет:            $MTPROTO_FULL_SECRET
  Ссылка:            $MTPROTO_LINK

EOF
fi
if [[ -n "$SOCKS5_PORT" ]]; then
	cat <<EOF
SOCKS5 (логин/пароль, без TLS -- протокол его не поддерживает):
  Сервер:            $HOSTNAME_ARG
  Порт:              $SOCKS5_PORT
  Логин:             $SOCKS5_USER
  Пароль:            $SOCKS5_PASS
  Ссылка:            socks5://$SOCKS5_USER:$SOCKS5_PASS@$HOSTNAME_ARG:$SOCKS5_PORT
  Дальше:            docker compose -f $PANEL_COMPOSE_FILE up -d socks5

EOF
fi
cat <<EOF
Мини-панель со ссылками (длинный случайный путь -- он и есть ключ, никому
кроме вас его не давайте): https://$HOSTNAME_ARG/$ADMIN_PATH/

EOF
if [[ "$REMOTE" == 1 ]]; then
	if [[ "$BACKEND_ONLY" == 0 ]]; then
		LAN_IP="$(ip -4 route get "$NPM_IP" 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "src") { print $(i + 1); exit }}' || true)"
		cat <<EOF
Удалённый режим: главный NPM ($NPM_IP) → https://${LAN_IP:-<IP этого сервера>}:$GATEWAY_PORT
Конфиг для главного NPM: npm/remote-*.conf (подставьте base path и IP этого сервера).
EOF
	fi
	if [[ "$BACKEND_ONLY" == 1 ]]; then
		echo "Режим --backend-only: своего gateway-контейнера и NPM_IP-фильтра нет,"
		echo "но публичный TLS уже поднимается сам -- сервис caddy в самом compose-файле,"
		echo "слушает порт \${WEB_PUBLIC_PORT:-8444} (переопределяется в .env) и проксирует"
		echo "на форвардер cover. Сертификат самоподписанный (Caddy 'tls internal') --"
		echo "этого достаточно, если перед сервером Cloudflare в режиме Full SSL (не Full"
		echo "strict), он не проверяет цепочку/имя сертификата источника. Если нужен"
		echo "сервер без Cloudflare перед ним (чужой NPM видит сертификат напрямую) --"
		echo "шаблон Advanced-блока: npm/remote-mode-a-dedicated-host.conf."
		echo "Ключ воркера (секрет AUTH в Cloudflare): cat config/worker_auth"
		echo "Дальше: docker compose -f docker-compose.remote-cf-caddy.yml up -d --build"
	elif [[ -n "$CF_WORKER" ]]; then
		echo "Выход в Telegram: tg-ws-proxy, запасной путь через Worker $CF_WORKER"
		echo "Ключ воркера (секрет AUTH в Cloudflare): cat config/worker_auth"
		echo "Дальше: docker compose -f docker-compose.remote-cf.yml up -d --build"
	elif [[ -n "$BACKEND_ADDR" ]]; then
		echo "Бэкенд: $BACKEND_ADDR (сеть $BACKEND_NETWORK)"
		echo "Дальше: docker compose -f docker-compose.remote-ext.yml up -d --build"
	else
		echo "Дальше: docker compose -f docker-compose.remote.yml up -d --build"
	fi
else
	cat <<EOF
Для NPM подставьте base path в npm/*.conf (строка location).
Дальше: docker compose up -d --build
EOF
fi
