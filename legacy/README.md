# WEB-прокси Telegram

Комплект для запуска [telegramdesktop/tproxy-server](https://github.com/telegramdesktop/tproxy-server)
(тип прокси **WEB** в Telegram Desktop 7.1.1+) на сервере, к которому трафик приходит
через главный Nginx Proxy Manager (NPM). Выход в Telegram идёт через
[Flowseal/tg-ws-proxy](https://github.com/Flowseal/tg-ws-proxy), а если прямой путь
закрыт — через свой Cloudflare Worker, защищённый ключом.

Снаружи хост выглядит как обычный сайт -- логин-форма вымышленного сервиса. Прокси открывается только
у того, кто знает адрес с base path и секрет.

---

## Быстрый старт

Сначала выберите сценарий -- он определяет, какой `docker-compose*.yml` и какие флаги
`init.sh` вам нужны. Все три сценария используют один и тот же код (`relay` + `tgws`),
разница только в том, кто перед ними стоит и завершает TLS.

| У вас есть... | Сценарий | Файл |
|---|---|---|
| NPM работает в Docker на ЭТОМ ЖЕ сервере | [NPM на этом сервере](QUICKSTART-local-npm.md) | `docker-compose.yml` |
| Главный NPM на ДРУГОМ сервере, но вы его администрируете (можете прописать Proxy Host и IP-фильтр) | [Удалённый NPM под вашим управлением](QUICKSTART-remote-npm.md) | `docker-compose.remote-cf.yml` |
| NPM чужой/внешний (хостинг-провайдер, другой админ), либо NPM вообще нет -- есть только IP:порт | [Чужой NPM или без NPM](QUICKSTART-no-npm.md) | `docker-compose.remote-cf-caddy.yml` |

Не уверены -- берите последний вариант: он работает всегда, просто у него на один внешний
прыжок больше (свой локальный веб-сервер -- Caddy, nginx, Apache, не важно какой). Если
администрируете NPM сами -- сразу идите по среднему варианту и сэкономьте этот прыжок.

На самом сервере весь выбор сводится к одной команде: распаковали проект, `sudo ./install.sh` --
меню само спросит то немногое, что нужно именно для вашего сценария (домен, при необходимости
IP NPM или домен Cloudflare Worker'а), и само поднимет нужный `docker-compose*.yml`.

Каждый QUICKSTART -- это короткая копируемая последовательность команд для деплоя на новом
сервере. Общее для всех трёх, прежде чем начинать:

- Протокол **WEB-прокси жёстко требует порт 443** на стороне клиента -- в ссылке
  `https://t.me/webproxy?server=...&secret=...` параметра порта нет. Чем бы вы ни пользовались,
  порт 443 в итоге должен привести именно к вашему `relay`.
- `install.sh`/`init.sh` идемпотентны: повторный запуск с теми же (или без) флагами не
  меняет уже созданные секрет, base path, ключ подписи токенов и выбранный `--carrier`.
- Секрет и ссылка клиента печатаются один раз при первом запуске `init.sh`. Чтобы увидеть их
  снова без смены секрета -- запустите `init.sh` ещё раз с теми же флагами режима.
- Remote-сценарии (2 и 3) поддерживают выход в Telegram через `tg-ws-proxy` (прямой WebSocket +
  запасной путь через ваш Cloudflare Worker) -- общие шаги настройки Worker'а в
  [QUICKSTART-cloudflare-worker.md](QUICKSTART-cloudflare-worker.md).

Полный справочник ниже (архитектура, все найденные грабли, обслуживание, все остальные режимы)
нужен не для первого деплоя, а когда что-то пошло не так или нужна тонкая настройка.

---

## Рабочая схема

```text
Telegram Desktop (тип WEB)
   │ https://proxy.example.ru/<base_path>/…
   ▼
главный NPM (192.168.1.1): домен, Let's Encrypt, HTTP/2
   │ https://192.168.1.10:18443  (самоподписанный сертификат, только с IP NPM)
   ▼
сервер proxy-host (192.168.1.10), стек docker-compose.remote-cf.yml:
   gateway (nginx) ──127.0.0.1:18080──▶ relay (tproxy-server)
        │                                  │ без секрета → сайт-прикрытие 127.0.0.1:13000
        │                                  │ с секретом  → 127.0.0.1:2398
        │                                  ▼
        │                              tgws (tg-ws-proxy)
        │                                  ├─ WebSocket напрямую к Telegram (DC2, DC4)
        │                                  └─ если закрыто → Worker cdn.worker-domain.example (DC1, DC203 …)
        └─ наружу открыт только порт 18443
```

### Три контейнера — это одна служба

| Контейнер | Что делает |
|---|---|
| `tproxy-npm-gateway-1` (nginx) | Принимает HTTPS от главного NPM на порту 18443, пускает только IP NPM, отдаёт сайт-прикрытие. Единственный опубликованный порт. |
| `tproxy-npm-relay-1` | Сам WEB-прокси: проверяет секрет, держит сессии WebView. Без секрета показывает сайт. |
| `tproxy-npm-tgws-1` | Выход в Telegram: WebSocket напрямую или через Cloudflare Worker с ключом. |

Все три работают в одном сетевом пространстве (`network_mode: service:gateway`) и общаются
через 127.0.0.1. Секрет один, ссылка одна. Relay и tgws снаружи недоступны.

---

## Состав комплекта

| Путь | Назначение |
|---|---|
| `init.sh` | Генерирует секрет, base path, ключи, `config/*.json`, `.env`, сертификат gateway и печатает ссылку |
| `docker-compose.remote-cf.yml` | **Рабочий режим**: gateway + relay + tg-ws-proxy с выходом через Worker |
| `cloudflare/worker.js` | Код Worker'а: только с ключом `AUTH` и только к адресам Telegram |
| `cloudflare/worker-original.js` | Тот же Worker с логикой соединения как в документации tg-ws-proxy |
| `update-stable.sh` | Перевод работающей установки на настройки для телефонов (без смены секрета) |
| `tgws/` | Сборка tg-ws-proxy с передачей ключа Worker'у (`patch_auth.py`) |
| `relay/Dockerfile` | Сборка tproxy-server из официального репозитория |
| `remote/nginx.conf.template` | Конфиг gateway: вход от NPM и сайт-прикрытие |
| `site/html/` | Сайт-прикрытие -- логин-форма вымышленного сервиса (пример, см. `index.html`/`404.html`) |
| `npm/remote-*.conf` | Блоки для вкладки Advanced в главном NPM |
| `docker-compose.remote.yml`, `docker-compose.remote-ext.yml`, `docker-compose.yml`, `mtproxy/` | Другие режимы, см. «Другие режимы» |
| `docker-compose.remote-cf-caddy.yml`, `remote/cover_nginx.conf` | Режим «за своим бэкендом, без отдельного gateway/NPM на этом сервере» -- см. раздел «Режим: бэкенд за чужим/внешним NPM» |
| `site/admin/index.html.template`, `config/admin_path` | Мини-панель со ссылками (WEB + MTProto, если включён) -- путь-секрет, без пароля/бэкенда, см. «Режим: бэкенд за чужим/внешним NPM» |
| `--mtproto-port`/`--fake-tls-domain` у `init.sh`, сервис `mtproto` в `docker-compose.remote-cf*.yml` | Опциональный классический MTProto (fake-TLS) рядом с WEB-прокси -- тот же `tg-ws-proxy`, другой порт |

Файлы, которые создаёт `init.sh` и которые **нельзя публиковать**:

| Файл | Что внутри |
|---|---|
| `config/secret.hex` | Секрет прокси |
| `config/worker_auth` | Ключ доступа к Worker'у (он же секрет `AUTH` в Cloudflare) |
| `config/token.key` | Ключ подписи сессий relay |
| `config/profiles.json`, `.env` | Секрет и ключ в составе конфигурации |
| `remote/certs/gateway.key` | Ключ сертификата gateway |

---

## Установка с нуля

### 1. DNS

A-запись `proxy.example.ru` → внешний IP, который приходит на главный NPM.
**Без** оранжевого облака Cloudflare: WEB-прокси нельзя ставить за CDN.

### 2. Cloudflare Worker

1. Cloudflare → **Workers & Pages** → **Create** → **Start with Hello World!** → имя (у нас `edge-sync`) → **Deploy**.
2. **Edit code** → удалить всё → вставить `cloudflare/worker.js` целиком → **Deploy**.
   Есть два варианта кода, ключ и фильтр адресов Telegram в обоих одинаковые:
   - `worker.js` — доработанный: к Telegram подключается при первых данных,
     аккуратно закрывает соединения, чистые логи Cloudflare;
   - `worker-original.js` — логика соединения один в один как в документации
     tg-ws-proxy; работает так же, но в логах бывают «code had hung» и исключения
     на завершении соединений.
3. **Settings → Domains & Routes → Add → Custom domain** → `cdn.worker-domain.example`.
   Зона `worker-domain.example` должна быть в Cloudflare в статусе Active.
4. Там же, в блоке **Worker URL**, выключить `*.workers.dev` (Production и Preview),
   чтобы Worker отвечал только на своём домене.
5. Секрет `AUTH` добавляется после шага 3 установки на сервере (ключ генерирует `init.sh`):
   **Settings → Variables and Secrets → Add** → тип **Secret**, имя `AUTH`,
   значение — содержимое `config/worker_auth` → **Deploy**.

Бот-прокси для `api.telegram.org`, если он есть, — это **другой** Worker. Его не трогать.

### 3. Сервер (192.168.1.10)

```bash
cd /opt && tar xzf tproxy-npm.tar.gz && cd tproxy-npm
rm -f site/html/README.txt

sudo ./init.sh --remote \
  --hostname proxy.example.ru \
  --npm-ip 192.168.1.1 \
  --cf-worker cdn.worker-domain.example

cat config/worker_auth          # → в секрет AUTH Worker'а (шаг 2.5)

docker compose -f docker-compose.remote-cf.yml up -d --build
docker compose -f docker-compose.remote-cf.yml ps
```

`init.sh` печатает сервер, секрет и ссылку `https://t.me/webproxy?...` для Telegram.
Повторный запуск не меняет секрет, base path, ключи и сертификат.

Параметры `init.sh`:

| Параметр | Значение |
|---|---|
| `--remote` | NPM на другом сервере |
| `--hostname` | Домен прокси |
| `--npm-ip` | LAN-IP машины с главным NPM: только с него gateway принимает соединения |
| `--cf-worker` | Домен Worker'а; включает режим tg-ws-proxy + Cloudflare |
| `--gateway-port` | Порт на хосте, по умолчанию 18443 (8443 занят UniFi) |
| `--base-path none\|<slug>` | Секретный префикс адреса, по умолчанию случайный |
| `--secret <hex>` | Задать свой секрет (32 hex или `dd`+32 hex) |
| `--carrier <режим>` | Транспорт relay: `https-lanes` (по умолчанию, лучший для телефонов), `https`, `websocket`, `websocket-lanes`. При повторном запуске сохраняется выбранный ранее |

### 4. Главный NPM (192.168.1.1)

**Hosts → Proxy Hosts → Add Proxy Host**:

| Вкладка | Настройка |
|---|---|
| Details | Домен `proxy.example.ru`, схема **https**, хост **192.168.1.10**, порт **18443** |
| Details | Кэшировать ресурсы — **выкл**, Блокировать эксплойты — **выкл**, WebSocket — **вкл** |
| SSL | Let's Encrypt, Всегда SSL — **вкл**, HTTP/2 — **вкл** |
| Advanced (шестерёнка) | Содержимое `npm/remote-mode-a-dedicated-host.conf` |

Блок Advanced нужен обязательно: стандартный location NPM передаёт `X-Forwarded-For`
списком, а relay принимает ровно один адрес. В блоке также заданы таймауты 90 с,
отключена буферизация и выключен access-лог (в адресе моста есть токен).

### 5. Telegram (Desktop 7.1.1+ или Android с поддержкой WEB)

Настройки → Продвинутые → Тип соединения → Прокси → Добавить → тип **WEB**:
сервер `proxy.example.ru/<base_path>` и секрет из `init.sh`, либо просто открыть ссылку.

---

## Стабильность на Android

Настройки комплекта подобраны под телефон:

| Настройка | Значение | Зачем |
|---|---|---|
| Транспорт relay (`carrier_mode`) | `https-lanes` | Каждое соединение Telegram — отдельная «полоса»: загрузка видео не тормозит чаты. Работает поверх HTTPS с повтором запросов, поэтому переживает короткие обрывы и смену Wi-Fi ↔ мобильная сеть. `websocket` при любом обрыве рвёт все соединения сразу — для телефона хуже |
| `reconnect_grace` | `10m` | Сессия ждёт телефон 10 минут: после возврата из фона Telegram продолжает прежнюю сессию, а не собирает новую. Разработчики tproxy-server прямо рекомендуют увеличивать для мобильных |
| `TGWS_ONLY_WORKER` | `0` | Прямой путь к DC2/DC4 быстрее Worker'а (Cloudflare обслуживает запросы в Гонконге/Портленде, +200–300 мс) |
| `cloudflare/worker.js` | доработанный | Подключается к Telegram только при данных, при закрытии сразу освобождает все ожидания — без «code had hung» |

Перевести уже работающую установку на эти настройки (секрет и ссылка не меняются):

```bash
cd /opt/tproxy-npm
sudo ./update-stable.sh
```

Что остаётся за пределами сервера:
- тип WEB на Android — экспериментальный: WebView работает, только пока Telegram на экране.
  В фоне Android может его закрыть — после возврата в приложение соединение собирается
  за несколько секунд. Это поведение клиента, с сервера не исправляется;
- в настройках Android для Telegram стоит отключить экономию батареи
  («Без ограничений»), тогда система реже закрывает его в фоне.

---

## Проверка

```bash
cd /opt/tproxy-npm
C=docker-compose.remote-cf.yml

# relay жив и видит tg-ws-proxy (ожидается 200)
docker compose -f $C exec gateway wget -qSO /dev/null http://127.0.0.1:18081/readyz 2>&1 | head -1

# Worker: без ключа 404, с ключом 101
H=(-H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==')
curl -s -o /dev/null -w 'без ключа %{http_code}\n' -m 5 --http1.1 "${H[@]}" 'https://cdn.worker-domain.example/apiws?dst=149.154.167.51'
curl -s -o /dev/null -w 'с ключом %{http_code}\n'  -m 5 --http1.1 "${H[@]}" "https://cdn.worker-domain.example/apiws?dst=149.154.167.51&auth=$(cat config/worker_auth)"

# работа tg-ws-proxy
docker compose -f $C logs --tail 50 tgws
```

В логе `tgws` нормальны строки `WS pool hit` (прямой путь) и
`CF worker pool hit via c**.red-d*****…` (через Worker). Отдельные `tcp_reset` —
обычные обрывы медиа-соединений, Telegram сам их переоткрывает.

Снаружи, например с телефона через мобильную сеть:
- `https://proxy.example.ru/` — страница-прикрытие (логин-форма вымышленного сервиса);
- `https://proxy.example.ru/?bridge=wrong` — та же страница.

---

## Обслуживание

### Логи и секреты

tg-ws-proxy при старте **печатает секрет** в лог (строки `Secret:` и `tg://proxy…`).
Логи не выкладывать. Если нужно показать — сначала вырезать эти строки:

```bash
docker compose -f docker-compose.remote-cf.yml logs tgws | grep -v -E 'Secret:|tg://proxy'
```

### Сменить секрет прокси (новая ссылка для всех)

```bash
rm config/secret.hex
sudo ./init.sh --remote --hostname proxy.example.ru --npm-ip 192.168.1.1 --cf-worker cdn.worker-domain.example
docker compose -f docker-compose.remote-cf.yml up -d
```

### Сменить ключ Worker'а

```bash
rm config/worker_auth
sudo ./init.sh --remote --hostname proxy.example.ru --npm-ip 192.168.1.1 --cf-worker cdn.worker-domain.example
cat config/worker_auth        # → обновить секрет AUTH в Cloudflare и нажать Deploy
docker compose -f docker-compose.remote-cf.yml up -d
```

Пока ключи в Cloudflare и на сервере расходятся, соединения через Worker отклоняются; прямой путь (DC2, DC4) продолжает работать.

### Обновить relay или tg-ws-proxy

Версии задаются в `.env`: `TPROXY_REF` (по умолчанию `master`) и `TGWS_REF` (по умолчанию `main`).

```bash
docker compose -f docker-compose.remote-cf.yml build --no-cache relay tgws
docker compose -f docker-compose.remote-cf.yml up -d
```

Если в новой версии tg-ws-proxy поменяется код запросов к Worker'у, сборка `tgws`
остановится с ошибкой `patch_auth.py`, а не соберёт образ без ключа.

### Весь Telegram только через свой Worker (вкл/выкл)

По умолчанию (`TGWS_ONLY_WORKER=0`) tg-ws-proxy идёт к Telegram напрямую по WebSocket
(`kws*.web.telegram.org`), если закрыто — через ваш Worker, затем через общие чужие
Cloudflare-домены из списка Flowseal на GitHub.

С `TGWS_ONLY_WORKER=1` прямой путь и общие домены отключаются (`--pool-size 0 --no-cfproxy`):
провайдер сервера видит только соединения с `cdn.worker-domain.example` (Cloudflare),
весь трафик расходует лимиты вашего Worker'а.

Включить:
```bash
cd /opt/tproxy-npm
sed -i 's/^TGWS_ONLY_WORKER=.*/TGWS_ONLY_WORKER=1/' .env
docker compose -f docker-compose.remote-cf.yml up -d tgws
docker compose -f docker-compose.remote-cf.yml logs --tail 20 tgws   # «mode: only Cloudflare Worker»
```

Выключить (вернуть как было):
```bash
cd /opt/tproxy-npm
sed -i 's/^TGWS_ONLY_WORKER=.*/TGWS_ONLY_WORKER=0/' .env
docker compose -f docker-compose.remote-cf.yml up -d tgws
```

В режиме `1` в логе не должно быть `WS pool hit` — только `CF worker … via c**.red-d*****…`.
Если Worker недоступен, tg-ws-proxy в последнюю очередь пробует прямое TCP-подключение
к Telegram; в сети, где Telegram закрыт, оно просто не пройдёт.

### Перезапуск и остановка

```bash
docker compose -f docker-compose.remote-cf.yml restart
docker compose -f docker-compose.remote-cf.yml down
```

После перезапуска relay клиенты переподключаются сами.

---

## Как устроена защита

- **Прокси.** Без секрета любой запрос получает сайт-прикрытие. Base path в адресе —
  второй неизвестный: без него сканеры не видят даже точку входа.
- **Gateway.** Порт 18443 отвечает только IP главного NPM, остальным соединение
  сбрасывается (`return 444`). Ограничение сделано в nginx, потому что опубликованные
  порты Docker обходят UFW.
- **Участок NPM → сервер** шифруется самоподписанным сертификатом, чтобы в сети не было
  открытого HTTP с `/api/v1/…` и токенами.
- **Worker** без правильного `auth` и к любым адресам, кроме сетей Telegram, отвечает
  одинаковым 404. Без секрета `AUTH` отклоняет всё. Чужие не могут тратить лимиты аккаунта.
- **Список сетей Telegram в Worker'е** обновляется сам: встроенный список плюс официальный
  `core.telegram.org/resources/cidr.txt`, который Worker перечитывает раз в сутки. Если файл
  недоступен, работает встроенный список. Сети шире `/12` из файла не принимаются.

---

## Режим: бэкенд за чужим/внешним NPM

Подходит, когда админ хостинга уже держит свой NPM на 443 (выпускает сертификаты сам) и
единственное, что можно получить, -- это Proxy Host на нужный внутренний порт сервера.
Свой NPM/nginx-gateway в докере тут не нужен: `docker-compose.remote-cf-caddy.yml` поднимает
только `tgws` + `relay` + `cover` (сайт-прикрытие и форвардер), а наружу их публикует локальный
Caddy (или другой веб-сервер, который уже есть на хосте).

```text
клиент ─https─▶ главный NPM (чужой, не наш, порт 443, свой сертификат)
                   │ proxy_pass на внутренний порт этого сервера
                   ▼
            локальный Caddy/веб-сервер (свой сертификат, см. npm/remote-mode-a-dedicated-host.conf)
                   │ reverse_proxy 127.0.0.1:PORT
                   ▼
            cover (nginx, форвардер 0.0.0.0:PORT ─▶ 127.0.0.1:18080) ─▶ relay
                                                                          │ 127.0.0.1:2398
                                                                          ▼
                                                tgws (tg-ws-proxy) ─WS─▶ Telegram / Worker
```

```bash
docker compose -f docker-compose.remote-cf-caddy.yml up -d --build
```

Пример блока для локального Caddy (порт и пути к сертификату -- свои):

```caddyfile
:PORT {
    tls /path/to/cert/fullchain.pem /path/to/cert/privkey.pem
    reverse_proxy 127.0.0.1:FORWARDER_PORT
}
```

Для чужого главного NPM -- тот же `npm/remote-mode-a-dedicated-host.conf`, что и в обычном
удалённом режиме, подставив реальный порт локального Caddy.

### Грабли №1: Docker не может NAT'ить host-порт на `127.0.0.1`-листенер внутри контейнера

`relay` слушает только `127.0.0.1:18080` (так требует `public_upstream` в `config/config.json` --
он принимает только числовой loopback-адрес, хостнейм типа `host.docker.internal` отклоняется
на старте с `configuration error: public_upstream: must use a numeric loopback address`).
Трафик, пришедший через опубликованный Docker-порт (`ports:`), попадает на `eth0` контейнера,
а не на его loopback -- слушателя там нет, соединение сразу рвётся (`Recv failure: Connection
reset by peer`), хотя сам relay живой и здоровый (проверяется изнутри общего network namespace:
`docker exec <tgws> ... 127.0.0.1:18081/readyz` → `200 ready`).

Решение -- сервис `cover` (обычный nginx, `network_mode: service:tgws`, то есть в том же
network namespace, что и `relay`): слушает `0.0.0.0:FORWARDER_PORT` и форвардит на
`127.0.0.1:18080` уже как «внутренний» клиент того же namespace, где loopback работает как
обычно. Только `FORWARDER_PORT` публикуется на хосте (`127.0.0.1:FORWARDER_PORT:FORWARDER_PORT`),
18080/18081 наружу не торчат. Тот же `cover`-контейнер отдаёт сайт-прикрытие на
`127.0.0.1:13000` -- туда и смотрит `public_upstream` в конфиге relay.

### Грабли №2: протокол WEB-прокси жёстко требует порт 443 -- сервер нельзя опубликовать на произвольном порту

Deep-ссылка `https://t.me/webproxy?server=...&secret=...` не несёт параметра порта: клиент
Telegram всегда идёт на 443 для указанного домена. Если нужный 443 занят чужим NPM (который мы
не контролируем) -- единственный рабочий вариант обхода этого ограничения и есть текущий режим:
просить админа NPM проксировать домен на внутренний порт этого сервера, где уже стоит свой
Caddy/бэкенд. Публиковать сервис сразу на нестандартном порту и давать такую ссылку клиентам
бессмысленно -- реальный клиент Telegram её просто не откроет на том порту.

### Грабли №3: `docker compose up/down` без `-f` подхватывает не тот compose-файл

В каталоге одновременно лежат несколько `docker-compose*.yml` для разных режимов (обычный NPM,
`--cf-worker`, этот режим и т. д.). Бездумный `docker compose up -d` / `docker compose down` без
явного `-f docker-compose.remote-cf-caddy.yml` подхватывает файл по умолчанию
(`docker-compose.yml`, локальный NPM-режим) -- у него другое определение сервиса `relay`
(и есть лишние `mtproxy`/`site`), поэтому Compose тихо пересоздаёт `relay` под чужим конфигом.
Контейнер при этом может остаться в состоянии `Created` (не стартовал) -- внешне выглядит как
«прокси отвалился», хотя дело в перепутанном compose-файле. Чинится так:

```bash
cd /opt/tproxy-npm
docker rm <лишние контейнеры не из своего режима>
docker compose -f docker-compose.remote-cf-caddy.yml up -d
```

Всегда указывайте `-f <нужный файл>` явно для любого `docker compose` в этом каталоге.

### Грабли №4: `profiles.json` -- секрет без MTProto-префикса, не как в клиентской ссылке

Поле `"secret"` в `config/profiles.json` должно быть **голым hex-секретом** (как в
`config/secret.hex`), без префикса `dd`/`ee` -- это маркер MTProto fake-TLS, используемый
только в клиентском секрете MTProto-прокси и не относящийся к серверному профилю WEB-прокси:

```json
"secret": "0123456789abcdef0123456789abcdef"
```

Если туда случайно попадает `dd`-префиксованное значение (`"secret": "dd4c5231..."`, например
скопированное из MTProto-подключения к тому же серверу), relay при сравнении декодированного
клиентского секрета (`0x70 || secret`, см. ниже) с профилем никогда не находит совпадения --
любой реальный запрос клиента тихо проваливается в `public_upstream` (сайт-прикрытие) с `404` на
путь базы, а relay-метрики (`total`/`active`) не растут вообще. Снаружи это выглядит как
«ссылка не работает»/«Telegram вечно переподключается», без единой ошибки в логах relay.

Клиентский секрет в самой ссылке (`?secret=...`) строится отдельно и по-другому: `0x70` + те же
16 байт голого секрета → unpadded base64url. Это НЕ тот же байтовый формат, что в `profiles.json`,
и префикс `0x70` там обязателен (иначе старые клиенты могли бы принять ссылку с base path как
обычный pathless-прокси) -- но в файле профиля он лишний.

### Грабли №5: `${var//pattern/$repl}` в bash портит значения со знаком `&`

При генерации мини-панели (`site/admin/index.html`) ссылки вида `.../webproxy?server=...&secret=...`
подставляются в HTML-шаблон через bash parameter expansion. Без кавычек вокруг переменной-замены
bash трактует `&` внутри неё как в `sed` -- "вставить совпадение целиком", и вместо ссылки получается
обрывок с буквальным текстом плейсхолдера посреди строки:

```bash
x="aZb"; y="1&2"
echo "${x//Z/$y}"     # a1Z2b  -- сломано, "Z" вернулся в вывод вместо замены
echo "${x//Z/"$y"}"   # a1&2b  -- верно, replacement в кавычках
```

Правило: **replacement в `${var//pattern/$repl}` всегда в кавычках** (`"$repl"`), если его
значение может содержать `&` -- а URL с query-параметрами почти всегда содержит. `init.sh` уже
следует этому правилу при рендере `site/admin/index.html.template`; если добавляете новую
подстановку туда же (или в любой другой bash-шаблон в этом проекте) -- не забывайте кавычки.

---

## Решённые проблемы

| Симптом | Причина и решение |
|---|---|
| `port is already allocated` на 8443 | UniFi держит 8443 в сети хоста → порт gateway 18443 |
| `curl: Failed to connect to core.telegram.org` в mtproxy | Прямой доступ к Telegram закрыт → режим `--cf-worker` вместо официального MTProxy |
| Мост отдаёт 404, хотя секрет верный | `X-Forwarded-For` пришёл списком → свой `location` в Advanced NPM |
| `required variable CF_WORKER is missing` | `.env` от старого режима → запустить `init.sh` с `--cf-worker` |
| Секрет `ee…` не принимается | Relay поддерживает только обычный или `dd`-секрет |
| `code had hung` / `Network connection lost` в логах Worker'а | Worker ждал запись/чтение на уже мёртвом соединении → новый `worker.js` отменяет все ожидания при закрытии |
| Пачка ошибок Worker'а в одну секунду | Перезапуск tg-ws-proxy (`restart tgws`) рвёт открытые соединения — не ошибка |
| `loadShed` в логах Worker'а | Cloudflare сам снял долгое соединение; tg-ws-proxy переоткрывает его, исправить кодом нельзя |

---

## Другие режимы

| Файл | Когда |
|---|---|
| `docker-compose.yml` | NPM на этом же сервере: контейнеры подключаются к сети контейнера NPM (`init.sh --npm-container <имя>`), конфиги `npm/mode-*.conf` |
| `docker-compose.remote.yml` | NPM на другом сервере, выход через официальный MTProxy (нужен прямой доступ к Telegram) |
| `docker-compose.remote-ext.yml` | NPM на другом сервере, выход через уже работающий MTProto-прокси (`--backend-addr`, `--backend-network`) |
| `docker-compose.remote-cf-caddy.yml` | Главный NPM не наш (управляется чужим админом) и не в докере на этом сервере: relay+tgws+сайт-прикрытие сами слушают один внутренний порт через форвардер в контейнере `cover`, снаружи их публикует локальный Caddy (или любой другой reverse proxy с реальным сертификатом). Главный NPM проксирует на этот порт как на обычный бэкенд. |

Режим B (`npm/remote-mode-b-existing-site.conf`) — прокси под секретным префиксом
внутри уже существующего сайта, например веб-почты. Прикрытием тогда служит сам сайт
(вариант 2 в `remote/nginx.conf.template`).
