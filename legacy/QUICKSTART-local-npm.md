# Сценарий 1: NPM уже работает в Docker на этом же сервере

Используется `docker-compose.yml`. Весь стек (`relay`, `mtproxy`, сайт-прикрытие) подключается
к сетевому пространству контейнера NPM — отдельный gateway/TLS не нужен, NPM видит ваш `relay`
прямо на `127.0.0.1:18080` как если бы это был процесс на том же хосте.

Выход в Telegram в этом режиме — через официальный MTProxy (`mtproxy/`), поэтому нужен прямой
доступ к серверам Telegram. Если он закрыт у вашего провайдера — вам нужен не этот, а
[сценарий 2 или 3](QUICKSTART.md) (там используется `tg-ws-proxy` + Cloudflare Worker).

## Шаги

```bash
cd /opt && tar xzf tproxy-npm.tar.gz && cd tproxy-npm   # или git clone, если из этого репозитория
rm -f site/html/README.txt   # если остался от старой версии

sudo ./install.sh
```

`install.sh` без аргументов — меню: выберите пункт **1**, скрипт сам спросит домен и имя
контейнера NPM (подскажет список `docker ps`), запустит `init.sh` и поднимет стек. В конце
печатает домен, секрет и готовую ссылку `https://t.me/webproxy?...` — сохраните их.

Без меню, одной строкой (для скриптов/повторных деплоев):

```bash
sudo ./install.sh local --hostname proxy.example.ru --npm-container nginx-proxy-manager-app-1
```

## Настройка в NPM

**Hosts → Proxy Hosts → Add Proxy Host**:

| Вкладка | Настройка |
|---|---|
| Details | Домен `proxy.example.ru`, схема **http**, хост **127.0.0.1**, порт **18080** |
| Details | Кэшировать ресурсы — выкл, Блокировать эксплойты — выкл, WebSocket — вкл |
| SSL | Let's Encrypt, Всегда SSL — вкл, HTTP/2 — вкл |
| Advanced | содержимое `npm/mode-a-dedicated-host.conf` (отдельный домен только под прокси) или `npm/mode-b-existing-site.conf` (прокси под префиксом внутри уже существующего сайта) |

Блок Advanced обязателен в обоих режимах: стандартный `location` в NPM передаёт
`X-Forwarded-For` списком, а relay принимает ровно один адрес и отклоняет список.

## Проверка

```bash
cd /opt/tproxy-npm
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18081/readyz   # ожидается 200 ready
```

Снаружи, с телефона через мобильную сеть: откройте ссылку `https://t.me/webproxy?...` из
вывода `install.sh`, либо `https://proxy.example.ru/` — должна открыться страница-прикрытие
(без секрета в адресе), а не ошибка.

## Повторный запуск / смена секрета

```bash
cd /opt/tproxy-npm
rm config/secret.hex
sudo ./install.sh local --hostname proxy.example.ru --npm-container nginx-proxy-manager-app-1
```
