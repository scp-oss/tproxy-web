# Cloudflare Worker — запасной путь в Telegram

Нужен для режимов [«Удалённый NPM под вашим управлением»](QUICKSTART-remote-npm.md) и
[«Чужой NPM или без NPM»](QUICKSTART-no-npm.md) — оба запускают `tgws` с `--cf-worker`.
Прямой путь к Telegram (`kws*.web.telegram.org`) работает без Worker'а; Worker — это то,
через что `tgws` пойдёт, если прямой путь закрыт у вашего хостинг-провайдера.

1. Cloudflare → **Workers & Pages** → **Create** → **Start with Hello World!** → дайте имя
   (например `edge-sync`) → **Deploy**.
2. **Edit code** → удалить всё → вставить `cloudflare/worker.js` из этого репозитория целиком
   → **Deploy**.
   Есть два варианта кода, ключ и фильтр адресов Telegram в обоих одинаковые:
   - `cloudflare/worker.js` — доработанный: к Telegram подключается при первых данных,
     аккуратно закрывает соединения, чистые логи Cloudflare;
   - `cloudflare/worker-original.js` — логика соединения как в документации tg-ws-proxy;
     работает так же, но в логах бывают «code had hung» и исключения на завершении соединений.
3. **Settings → Domains & Routes → Add → Custom domain** → ваш домен Worker'а
   (например `cdn.worker-domain.example`). Зона должна быть в Cloudflare в статусе Active.
4. Там же, в блоке **Worker URL**, выключить `*.workers.dev` (Production и Preview), чтобы
   Worker отвечал только на своём домене.
5. Секрет `AUTH` добавляется ПОСЛЕ запуска `init.sh` на сервере (ключ генерирует сам скрипт,
   см. шаг в выбранном вами QUICKSTART): **Settings → Variables and Secrets → Add** → тип
   **Secret**, имя `AUTH`, значение — содержимое `config/worker_auth` → **Deploy**.

Если на этом аккаунте уже есть Worker-прокси для `api.telegram.org` (например для бота) —
это **другой** Worker, не трогайте его.

## Проверка

```bash
H=(-H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==')
curl -s -o /dev/null -w 'без ключа %{http_code}\n' -m 5 --http1.1 "${H[@]}" 'https://cdn.worker-domain.example/apiws?dst=149.154.167.51'
curl -s -o /dev/null -w 'с ключом %{http_code}\n'  -m 5 --http1.1 "${H[@]}" "https://cdn.worker-domain.example/apiws?dst=149.154.167.51&auth=$(cat config/worker_auth)"
```

Без ключа — `404`, с ключом — `101`. Если оба `404` — ключ `AUTH` в Cloudflare не совпадает с
`config/worker_auth` на сервере (перепроверьте Deploy на шаге 5).
