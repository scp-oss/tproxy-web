#!/usr/bin/env bash
# Один вызов: sudo ./install.sh -- меню, подменю с вопросами под выбранный
# сценарий, затем init.sh + docker compose up -d --build сами.
#
# Для скриптов/повторных запусков можно и без меню, как раньше:
#   sudo ./install.sh local        --hostname proxy.example.ru --npm-container <имя>
#   sudo ./install.sh remote       --hostname proxy.example.ru --npm-ip <IP> --cf-worker <домен>
#   sudo ./install.sh backend-only --hostname proxy.example.ru --cf-worker <домен>
# Добавить MTProto (remote/backend-only): ... --mtproto-port 8443 --fake-tls-domain www.google.com
#
# Подробности по каждому сценарию -- QUICKSTART-*.md в этом репозитории.
set -euo pipefail
cd "$(dirname "$0")"

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
	sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
	exit 0
fi

ask() {
	# ask "вопрос" "значение_по_умолчанию" -> эхо в stdout
	local prompt="$1" default="${2:-}" reply
	if [[ -n "$default" ]]; then
		read -rp "$prompt [$default]: " reply
		echo "${reply:-$default}"
	else
		read -rp "$prompt: " reply
		echo "$reply"
	fi
}

ask_yn() {
	# ask_yn "вопрос" -> "y" или "n"
	local prompt="$1" reply
	while true; do
		read -rp "$prompt [y/n]: " reply
		case "$reply" in
			y|Y|д|Д) echo y; return ;;
			n|N|н|Н) echo n; return ;;
		esac
	done
}

# ask_mtproto <имя-массива-args>
# Добавляет --mtproto-port/--fake-tls-domain в указанный массив аргументов,
# если пользователь согласился; выставляет MTPROTO_ENABLED=y|n в вызывающем
# окружении (сам массив services там же дополняется отдельно).
ask_mtproto() {
	local -n _args_ref="$1"
	local use_mt port domain
	echo
	use_mt="$(ask_yn 'Добавить классический MTProto (fake-TLS, для обычных MTProto-клиентов, не WEB)')"
	MTPROTO_ENABLED="$use_mt"
	if [[ "$use_mt" == y ]]; then
		echo "ВАЖНО: нужен прямой доступ к Telegram с этого сервера хотя бы как запасной путь"
		echo "(та же устойчивость через Cloudflare Worker, что у WEB-прокси, но без него --"
		echo "совсем без доступа к Telegram -- не заработает ни напрямую, ни через Worker)."
		port="$(ask 'Порт для MTProto (должен быть открыт у провайдера)')"
		[[ -n "$port" ]] || { echo "порт обязателен" >&2; exit 1; }
		domain="$(ask 'Домен для fake-TLS маскировки (любой реальный HTTPS-сайт)' 'www.google.com')"
		_args_ref+=(--mtproto-port "$port" --fake-tls-domain "$domain")
	fi
}

run_init_and_compose() {
	# $1 = compose-файл, $2 = список сервисов для up (пусто = все из файла),
	# остальное -- флаги init.sh
	local compose_file="$1" services="$2"; shift 2
	echo
	echo "== ./init.sh $* =="
	./init.sh "$@"
	echo
	echo "== docker compose -f $compose_file up -d --build ${services:-(все сервисы)} =="
	# shellcheck disable=SC2086
	docker compose -f "$compose_file" up -d --build $services
	docker compose -f "$compose_file" ps
}

next_step_hint() {
	case "$1" in
		local)
			echo "Дальше: добавить Proxy Host в NPM -- см. QUICKSTART-local-npm.md." ;;
		remote)
			echo "Дальше: добавить Proxy Host в главном NPM -- см. QUICKSTART-remote-npm.md." ;;
		backend-only)
			echo "Дальше: настроить локальный веб-сервер (Caddy/nginx/Apache) и/или попросить"
			echo "админа внешнего NPM добавить Proxy Host -- см. QUICKSTART-no-npm.md." ;;
	esac
}

scenario_local() {
	echo
	echo "--- NPM на этом сервере (в Docker) ---"
	local hostname npm_container
	hostname="$(ask 'Домен прокси (например proxy.example.ru)')"
	[[ -n "$hostname" ]] || { echo "домен обязателен" >&2; exit 1; }
	echo "Контейнеры NPM на этом хосте:"
	docker ps --format '  %s' '{{.Names}}' 2>/dev/null || true
	npm_container="$(ask 'Имя контейнера NPM (docker ps)')"
	[[ -n "$npm_container" ]] || { echo "имя контейнера обязательно" >&2; exit 1; }
	run_init_and_compose docker-compose.yml --hostname "$hostname" --npm-container "$npm_container"
	next_step_hint local
}

scenario_remote() {
	echo
	echo "--- Удалённый NPM под вашим управлением ---"
	echo "(этот сценарий всегда идёт через tg-ws-proxy -- Cloudflare Worker обязателен,"
	echo "см. QUICKSTART-cloudflare-worker.md. Нужен голый официальный MTProxy без Worker'а"
	echo "-- это другой режим, docker-compose.remote.yml, им install.sh пока не управляет.)"
	local hostname npm_ip cf_worker services args
	hostname="$(ask 'Домен прокси (например proxy.example.ru)')"
	[[ -n "$hostname" ]] || { echo "домен обязателен" >&2; exit 1; }
	npm_ip="$(ask 'IP сервера с главным NPM')"
	[[ -n "$npm_ip" ]] || { echo "IP главного NPM обязателен" >&2; exit 1; }
	cf_worker="$(ask 'Домен Cloudflare Worker (например cdn.worker-domain.example)')"
	[[ -n "$cf_worker" ]] || { echo "домен Worker'а обязателен" >&2; exit 1; }
	args=(--hostname "$hostname" --npm-ip "$npm_ip" --cf-worker "$cf_worker")
	services="gateway tgws relay"
	ask_mtproto args
	[[ "$MTPROTO_ENABLED" == y ]] && services="$services mtproto"
	run_init_and_compose docker-compose.remote-cf.yml "$services" "${args[@]}"
	next_step_hint remote
}

scenario_backend_only() {
	echo
	echo "--- Чужой/внешний NPM, или вообще без NPM ---"
	echo "(этот сценарий всегда идёт через tg-ws-proxy -- Cloudflare Worker обязателен,"
	echo "см. QUICKSTART-cloudflare-worker.md.)"
	local hostname cf_worker services args
	hostname="$(ask 'Домен прокси (например proxy.example.ru)')"
	[[ -n "$hostname" ]] || { echo "домен обязателен" >&2; exit 1; }
	cf_worker="$(ask 'Домен Cloudflare Worker (например cdn.worker-domain.example)')"
	[[ -n "$cf_worker" ]] || { echo "домен Worker'а обязателен" >&2; exit 1; }
	args=(--hostname "$hostname" --cf-worker "$cf_worker")
	services="tgws relay panel-admin cover caddy"
	ask_mtproto args
	[[ "$MTPROTO_ENABLED" == y ]] && services="$services mtproto"
	run_init_and_compose docker-compose.remote-cf-caddy.yml "$services" --remote --backend-only "${args[@]}"
	next_step_hint backend-only
}

# --- без меню: сценарий и флаги init.sh переданы аргументами ---
if [[ $# -ge 1 && "$1" != "" ]]; then
	case "$1" in
		local|remote|backend-only) ;;
		*) echo "неизвестный сценарий: $1 (нужен local | remote | backend-only)" >&2; exit 2 ;;
	esac
	SCENARIO="$1"; shift
	# --mtproto-port среди "$@" значит добавить mtproto в список сервисов up
	MT_SVC=""
	for a in "$@"; do [[ "$a" == "--mtproto-port" ]] && MT_SVC=" mtproto"; done
	case "$SCENARIO" in
		local)        run_init_and_compose docker-compose.yml "" "$@" ;;
		remote)       run_init_and_compose docker-compose.remote-cf.yml "gateway tgws relay$MT_SVC" --remote "$@" ;;
		backend-only) run_init_and_compose docker-compose.remote-cf-caddy.yml "tgws relay panel-admin cover caddy$MT_SVC" --remote --backend-only "$@" ;;
	esac
	next_step_hint "$SCENARIO"
	exit 0
fi

# --- меню ---
echo "Какой у вас сценарий?"
echo
echo "  1) NPM работает в Docker на этом же сервере"
echo "  2) Главный NPM на другом сервере, но вы его администрируете"
echo "  3) NPM чужой/внешний, или вообще без NPM"
echo "  0) Выход"
echo
choice="$(ask 'Номер варианта' '3')"
case "$choice" in
	1) scenario_local ;;
	2) scenario_remote ;;
	3) scenario_backend_only ;;
	0) exit 0 ;;
	*) echo "непонятный выбор: $choice" >&2; exit 2 ;;
esac
