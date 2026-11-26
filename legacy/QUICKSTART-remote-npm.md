# Сценарий 2: главный NPM на другом сервере, но вы его администрируете

Используется `docker-compose.remote-cf.yml`. Этот сервер поднимает СВОЙ gateway-контейнер
(nginx) с самоподписанным сертификатом на внутреннем порту — главный NPM проксирует на него.
Gateway сам фильтрует входящие соединения по IP главного NPM (`--npm-ip`), поэтому снаружи
кроме NPM к нему никто не достучится, даже зная порт.

Выход в Telegram — через `tg-ws-proxy`: сперва прямой WebSocket, при блокировке — через ваш
Cloudflare Worker (скрипт сам спросит, нужен ли он). Про сам Worker —
[QUICKSTART-cloudflare-worker.md](QUICKSTART-cloudflare-worker.md) (шаг с секретом `AUTH` — уже
после шага 2 ниже, когда появится `config/worker_auth`).

## 1. DNS

A-запись `proxy.example.ru` → внешний IP, на который приходит главный NPM. **Без** оранжевого
облака Cloudflare — WEB-прокси нельзя ставить за CDN (см. «Важно» в конце).

## 2. Этот сервер (бэкенд, 192.168.1.10 в примере)

```bash
cd /opt && tar xzf tproxy-npm.tar.gz && cd tproxy-npm
rm -f site/html/README.txt

sudo ./install.sh
```

Меню → пункт **2**. Скрипт спросит домен, IP главного NPM и (если нужен) домен Cloudflare
Worker'а, сам запустит `init.sh --remote` и поднимет `docker-compose.remote-cf.yml`. В конце
печатает секрет и готовую ссылку `https://t.me/webproxy?...`; `cat config/worker_auth` —
если указали Worker, ключ `AUTH` для Cloudflare.

Без меню, одной строкой (плюс любые доп. флаги `init.sh`, например `--gateway-port`, если
18443 занят):

```bash
sudo ./install.sh remote --hostname proxy.example.ru --npm-ip 192.168.1.1 --cf-worker cdn.worker-domain.example
```

Повторный запуск (`install.sh`/`init.sh`) не меняет уже созданные секрет/base path/ключи.

## 3. Главный NPM (192.168.1.1)

**Hosts → Proxy Hosts → Add Proxy Host**:

| Вкладка | Настройка |
|---|---|
| Details | Домен `proxy.example.ru`, схема **https**, хост **192.168.1.10**, порт **18443** (или ваш `--gateway-port`) |
| Details | Кэшировать ресурсы — выкл, Блокировать эксплойты — выкл, WebSocket — вкл |
| SSL | Let's Encrypt, Всегда SSL — вкл, HTTP/2 — вкл |
| Advanced | содержимое `npm/remote-mode-a-dedicated-host.conf` (отдельный домен) или `npm/remote-mode-b-existing-site.conf` (прокси под префиксом внутри существующего сайта) — подставьте реальный IP этого сервера |

## 4. Telegram

Откройте ссылку из вывода `install.sh`, либо вручную: тип **WEB**, сервер
`proxy.example.ru/<base_path>`, секрет из вывода. Ссылка также дублируется на мини-панели
(адрес -- длинный случайный путь из вывода `install.sh`, больше нигде не публикуется).

Меню `install.sh` в этом сценарии дополнительно предлагает классический MTProto-прокси
(`tg://proxy?...`, fake-TLS, для клиентов без типа WEB) -- свой порт, нужно заранее
проверить у провайдера, что он открыт; подробности и флаговый вызов без меню --
[QUICKSTART-no-npm.md](QUICKSTART-no-npm.md#классический-mtproto-не-только-web-прокси)
(шаги идентичны, меняется только compose-файл).

## Проверка

```bash
cd /opt/tproxy-npm
C=docker-compose.remote-cf.yml
docker compose -f $C exec gateway wget -qSO /dev/null http://127.0.0.1:18081/readyz 2>&1 | head -1   # ожидается 200
docker compose -f $C logs --tail 50 tgws   # WS pool hit = прямой путь, CF worker pool hit = через Worker
```

## Важно: порт 443 на клиенте жёсткий

Ссылка `https://t.me/webproxy?server=...` не несёт параметра порта — клиент Telegram всегда
идёт на 443 для указанного домена. Поэтому главный NPM (шаг 3) должен сам слушать 443 для
этого домена — внутренний `--gateway-port` на бэкенде может быть любым, это не проблема.

Полный список граблей (почему именно так, а не иначе) — в README.md, раздел
«Решённые проблемы».
