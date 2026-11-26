# Сценарий 3: NPM чужой/внешний, или NPM вообще нет

Используется `docker-compose.remote-cf-caddy.yml`. Стек поднимает только `tgws` + `relay` +
`cover` (форвардер и сайт-прикрытие) — без своего gateway-контейнера и без `--npm-ip`-фильтра.
Снаружи их публикует **какой-то локальный веб-сервер** — Caddy, nginx, Apache, да хоть снова
NPM — не важно какой, важно только, что он умеет: завершить TLS реальным сертификатом и
проксировать на `127.0.0.1:19443` с поддержкой WebSocket.

Три независимых вопроса, от ответов на которые зависит, что именно делать:

1. **Чем термируется TLS снаружи** — Caddy / nginx / Apache / NPM / что-то ещё? (шаг 3, пример
   блока под каждый вариант)
2. **Этот веб-сервер — ваш, или кто-то другой им управляет?** Если ваш — просто правите
   конфиг сами (шаг 3). Если нет — отдаёте его админу готовый блок и просите применить
   (шаг 4а, работает для любого ПО, не только NPM).
3. **Порт 443 — ваш, или его кто-то уже занял (например другой NPM, которым вы не управляете)?**
   Если ваш — этот же веб-сервер сам сидит на 443 (шаг 4б). Если нет — ваш веб-сервер сидит на
   внутреннем порту, а на 443 его проксирует ЕЩЁ один, внешний слой (шаг 4а).

Выход в Telegram — через `tg-ws-proxy`, сперва прямой WebSocket, при блокировке — через
Cloudflare Worker. Сначала настройте Worker: [QUICKSTART-cloudflare-worker.md](QUICKSTART-cloudflare-worker.md)
(секрет `AUTH` — после шага 1 ниже, когда появится `config/worker_auth`).

## 1-2. Инициализация и запуск

```bash
cd /opt && tar xzf tproxy-npm.tar.gz && cd tproxy-npm
rm -f site/html/README.txt

sudo ./install.sh
```

Меню → пункт **3**. Скрипт спросит домен и (если нужен) домен Cloudflare Worker'а, сам
запустит `init.sh --remote --backend-only` (отключает ненужные здесь `--npm-ip` и
самоподписанный gateway-сертификат — TLS снаружи держит ваш веб-сервер) и поднимет
`docker-compose.remote-cf-caddy.yml`. В конце выводит секрет, ссылку
`https://t.me/webproxy?...` и (если указали Worker) напоминание про `cat config/worker_auth`.

Без меню, одной строкой:

```bash
sudo ./install.sh backend-only --hostname proxy.example.ru --cf-worker cdn.worker-domain.example
```

Проверка, что стек реально поднялся:

```bash
docker compose -f docker-compose.remote-cf-caddy.yml ps   # все три сервиса должны быть Up
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:19443/   # ожидается 200 (сайт-прикрытие)
```

Если позже понадобится руками погонять `docker compose` в этом каталоге — **всегда указывайте
`-f docker-compose.remote-cf-caddy.yml` явно**: здесь лежат compose-файлы и для других режимов
тоже, и команда без `-f` подхватывает `docker-compose.yml` по умолчанию, тихо пересоздавая
`relay` под чужим конфигом (см. README, «Решённые проблемы»).

## 3. Локальный веб-сервер — блок под ваше ПО

Порт **8444** в примерах ниже — внутренний прыжок до этого веб-сервера (нужен только для
варианта 4а, где снаружи есть ещё один слой). Если идёте по варианту 4б (свой 443) — сразу
ставьте `443`/`proxy.example.ru` вместо `:8444`.

### Caddy

```caddyfile
proxy.example.ru:8444 {
    tls /path/to/cert/fullchain.pem /path/to/cert/privkey.pem
    reverse_proxy 127.0.0.1:19443
}
```
```bash
caddy validate --config /etc/caddy/Caddyfile && systemctl reload caddy
```

### nginx

```nginx
server {
    listen 8444 ssl;
    server_name proxy.example.ru;
    ssl_certificate     /path/to/cert/fullchain.pem;
    ssl_certificate_key /path/to/cert/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:19443;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 90s;
        proxy_buffering off;
        access_log off;
    }
}
```
```bash
nginx -t && systemctl reload nginx
```

**Домен уже занят другими сервисами (443 общий, десятки других `server_name` на этом же
хосте)** — частый случай для домашней сети/NAS: вместо отдельного `server` блока добавьте
`location` с префиксом внутрь УЖЕ существующего `server { listen 443 ssl; ... }` этого
домена (тот же приём, что и в `npm/mode-b-existing-site.conf`, только напрямую в nginx, без
NPM). Замените `phcf2vfe7zgbrslg` на ваш `config/base_path`:

```nginx
# внутри уже существующего server { listen 443 ssl; server_name home.example.net; ... }
location ^~ /phcf2vfe7zgbrslg/ {
    proxy_pass http://127.0.0.1:19443;   # без URI-части: префикс уходит как есть
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 90s;
    proxy_buffering off;
    access_log off;
}
```

`^~` не даёт другим `location`-блокам (regex, статика и т. д.) перехватить префикс раньше.
После смены `ADMIN_PATH`/`base_path` (регенерируются при переустановке `config_data`) этот
блок нужно обновить синхронно — иначе путь будет вести в никуда.

### Apache (2.4+, нужны mod_proxy, mod_proxy_http, mod_proxy_wstunnel, mod_ssl)

```apache
<VirtualHost *:8444>
    ServerName proxy.example.ru
    SSLEngine on
    SSLCertificateFile      /path/to/cert/fullchain.pem
    SSLCertificateKeyFile   /path/to/cert/privkey.pem

    RewriteEngine On
    RewriteCond %{HTTP:Upgrade} =websocket [NC]
    RewriteRule /(.*) ws://127.0.0.1:19443/$1 [P,L]

    ProxyPreserveHost On
    ProxyPass        / http://127.0.0.1:19443/
    ProxyPassReverse / http://127.0.0.1:19443/
</VirtualHost>
```
```bash
a2enmod proxy proxy_http proxy_wstunnel rewrite ssl
apachectl configtest && systemctl reload apache2
```

Если у вас ещё нет сертификата на этот домен — выпустите его отдельно (Let's Encrypt на любом
сервисе, которому реально отвечает домен, или попросите сертификат у админа внешнего слоя, если
у него уже есть) и подставьте пути выше.

### Проверка на пустом месте: порт реально открыт у провайдера?

Прежде чем звать админа или переходить к шагу 4, проверьте порт **с другого внешнего хоста**
(не localhost), с паузами:

```bash
curl -s -o /dev/null -w 'HTTP_CODE=%{http_code}\n' --max-time 15 https://proxy.example.ru:8444/
sleep 10
curl -s -o /dev/null -w 'HTTP_CODE=%{http_code}\n' --max-time 15 https://proxy.example.ru:8444/
```

Один успешный ответ, затем тишина на все последующие — классический признак того, что порт не
входит в список открытых у хостинг-провайдера (не баг в конфиге). Узнайте у провайдера
актуальный список открытых портов и перевесьте блок на один из них.

## 4а. Внешний слой (NPM или любой другой веб-сервер) не ваш

Не важно, что это за ПО у админа — NPM, голый nginx, Apache, что угодно. Логика одна: он
должен проксировать домен на ваш локальный веб-сервер из шага 3, с поддержкой WebSocket и
ровно одним IP в `X-Forwarded-For`.

**Если у админа NPM** — текст для него:

- **Domain**: `proxy.example.ru`
- **Scheme**: `https`
- **Forward Hostname/IP**: `<публичный IP этого сервера>`
- **Forward Port**: `8444` (ваш порт из шага 3)
- **Websockets Support**: включить — обязательно
- **Cache Assets** / **Block Common Exploits**: выключить
- **SSL**: Request a new SSL Certificate / Let's Encrypt, Force SSL — вкл, HTTP/2 — вкл
- **Advanced** — содержимое `npm/remote-mode-a-dedicated-host.conf`, заменив
  `proxy_pass https://192.168.1.10:18443` на `proxy_pass https://<IP этого сервера>:8444` и
  добавив `proxy_ssl_verify off;` (на бэкенде у вас свой, не их, сертификат).

**Если у админа голый nginx/Apache (не NPM)** — отдайте ему блок из шага 3 («nginx» или
«Apache» выше), подставив в `proxy_pass`/`ProxyPass` `https://<IP этого сервера>:8444` вместо
`127.0.0.1:19443`, плюс попросите `proxy_ssl_verify off` (nginx) или
`SSLProxyCheckPeerCN off`/`SSLProxyVerify none` (Apache), раз сертификат на бэкенде не их.

После того как админ применит блок, порт 443 у клиента станет настоящим — ссылка заработает.

## 4б. Порт 443 свободен — веб-сервер из шага 3 сидит на нём сам

Если 443 у вас свободен и ничего его не держит — просто поставьте в шаге 3 `443` вместо
`:8444` (для Caddy — без явного порта вообще, см. пример) и пропустите шаг 4а целиком.

```caddyfile
proxy.example.ru {
    tls /path/to/cert/fullchain.pem /path/to/cert/privkey.pem
    reverse_proxy 127.0.0.1:19443
}
```

## Проверка целиком

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://proxy.example.ru/   # 200, страница-прикрытие
```

Откройте ссылку из вывода `install.sh`/`init.sh` (шаг 1) в Telegram, либо мини-панель
(адрес тоже в выводе шага 1 -- длинный случайный путь, больше нигде не публикуется).

## Классический MTProto, не только WEB-прокси

Меню `install.sh` (пункт для этого сценария) спросит отдельно, нужен ли ещё и обычный
MTProto-прокси (`tg://proxy?...`, fake-TLS) -- для клиентов без поддержки типа WEB. Это
тот же `tg-ws-proxy`, что и для WEB-прокси, просто запущенный напрямую на своём порту, с
той же устойчивостью через Cloudflare Worker. Нужен отдельный открытый порт (как и всюду в
этом проекте -- проверяйте у провайдера заранее, см. раздел «Грабли №2» в README) и домен
для fake-TLS маскировки (любой реальный HTTPS-сайт, например `www.google.com`).

Без меню:
```bash
sudo ./install.sh backend-only --hostname proxy.example.ru --cf-worker cdn.worker-domain.example \
  --mtproto-port 8443 --fake-tls-domain www.google.com
```

Ссылка печатается в выводе `install.sh`/`init.sh` и дублируется на мини-панели.

## Если реальный клиент не подключается, а curl отвечает 200

`curl` на корень (`/`) без секрета всегда покажет страницу-прикрытие — это ожидаемо и ничего
не проверяет про сам прокси. Если именно НАСТОЯЩИЙ клиент Telegram виснет на «подключение» —
смотрите логи:

```bash
docker logs tproxy-npm-relay-1 --tail 40
docker logs tproxy-npm-cover-1 --tail 40   # access-лог покажет реальные запросы от клиента
```

Если в access-логе `cover` видны запросы с реальным User-Agent (не curl) на
`/<base_path>/?bridge=...`, но всегда `404` — проверьте `config/profiles.json`:

```bash
cat config/profiles.json
cat config/secret.hex
```

Поле `"secret"` должно БУКВАЛЬНО совпадать с содержимым `config/secret.hex` — без префикса
`dd`. Это уже исправлено в `init.sh` (см. README, «Грабли №4»), но если `config/profiles.json`
остался от старой версии скрипта — удалите его и перезапустите `init.sh`:

```bash
rm config/profiles.json
sudo ./install.sh backend-only --hostname proxy.example.ru --cf-worker cdn.worker-domain.example
docker compose -f docker-compose.remote-cf-caddy.yml restart relay
```
