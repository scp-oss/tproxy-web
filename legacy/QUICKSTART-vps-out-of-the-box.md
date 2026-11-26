# Сценарий 4: голый VPS, без NPM и без своего веб-сервера

Самый короткий путь: домен указывает ПРЯМО на этот сервер (A-запись, без Cloudflare/CDN
перед ним), никакого другого веб-сервера на хосте нет. `docker-compose.remote-cf-caddy.yml`
поднимает Caddy, который сам получает настоящий сертификат Let's Encrypt — руками ничего
выпускать не нужно.

## 1. Установка

```bash
cd /opt && tar xzf tproxy-npm.tar.gz && cd tproxy-npm
rm -f site/html/README.txt
sudo ./install.sh
```

Меню → пункт **3** (тот же сценарий, что «Чужой NPM или без NPM» —
см. [QUICKSTART-no-npm.md](QUICKSTART-no-npm.md)), либо без меню:

```bash
sudo ./install.sh backend-only --hostname proxy.example.ru --cf-worker cdn.worker-domain.example
```

## 2. Включить автоматический сертификат

В `.env` рядом с `docker-compose.remote-cf-caddy.yml` добавить одну строку:

```
CADDY_AUTO_HTTPS=1
```

и поднять/пересоздать стек целиком (всегда вместе — см. README, «Грабли»):

```bash
docker compose -f docker-compose.remote-cf-caddy.yml up -d --force-recreate tgws relay cover caddy panel-admin
```

Caddy сам закажет сертификат через TLS-ALPN-01 (целиком по 443, отдельный порт 80 не
обязателен) при первом же реальном TLS-подключении с правильным SNI. Проверить:

```bash
curl -sv https://proxy.example.ru/ 2>&1 | grep -E 'subject:|issuer:'
# ожидается настоящий Let's Encrypt, не self-signed
```

Если DNS ещё не разошёлся или порт 443 у хостера не открыт сразу — первые попытки ACME
упадут (это видно в `docker logs <project>-caddy-1`), Caddy просто повторит позже
автоматически, ничего руками перезапускать не нужно.

## Когда НЕ подходит этот сценарий

- Домен проксируется через Cloudflare (оранжевое облако) или любой другой CDN/NPM — тогда
  ACME-валидация до вашего сервера не достучится. См. [QUICKSTART-no-npm.md](QUICKSTART-no-npm.md)
  (самоподписанный — вариант по умолчанию, достаточен за Cloudflare Full SSL) или
  [QUICKSTART-remote-npm.md](QUICKSTART-remote-npm.md) (NPM под вашим управлением, настоящий
  сертификат выпускает сам NPM).
- NPM чужой/внешний (хостинг-провайдер, не вы администрируете) — см.
  [QUICKSTART-no-npm.md](QUICKSTART-no-npm.md), раздел 4а, и готовый шаблон
  `npm/remote-mode-c-plain-forwarder.conf` (если NPM в той же локальной сети, что и сервер —
  самый простой и проверенный живьём вариант).
