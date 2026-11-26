# panel-admin как обычный сервис compose (не host-демон под systemd -- тот
# подход не переносится на Synology DSM и подобные NAS с собственным
# управлением сервисами). docker-outside-of-docker: монтируется только
# /var/run/docker.sock хоста (см. volumes в docker-compose.yml) -- НИКАКОГО
# host-пути проекта знать не нужно: сам compose-файл и шаблон config.json
# запечены прямо в этот образ ниже (COPY), а секреты/состояние живут в
# именованном volume (config_data), не в host bind-mount. Раньше вместо
# этого монтировали ${PROJECT_DIR}:${PROJECT_DIR} по тому же абсолютному
# пути, что и на хосте -- пользователю приходилось вводить его точно в
# форме Container Manager, а при ошибке `docker compose` внутри контейнера
# неправильно резолвил relative-пути volume'ов. Теперь этого параметра нет.
#
# Финальный образ -- docker:27-cli: уже содержит клиент docker, нам нужен
# только он (сам демон не нужен, подключаемся к хостовому через сокет).
#
# Контекст сборки -- корень репозитория (см. docker-compose.yml: build.context: .,
# dockerfile: panel-admin/Dockerfile), не ./panel-admin -- иначе не достать
# docker-compose.yml и config.json.template для COPY ниже.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY panel-admin/go.mod panel-admin/go.sum panel-admin/main.go ./
RUN go mod download
RUN CGO_ENABLED=0 go build -trimpath -o /out/panel-admin .

FROM docker:27-cli
COPY --from=build /out/panel-admin /usr/local/bin/panel-admin
COPY docker-compose.remote-cf-caddy.yml /app/docker-compose.yml
COPY config/config.json.template /app/config.json.template
# Тот же шаблон, что использует cover (site/admin/index.html.template) --
# независимый вход (см. LISTEN_ADDR_PUBLIC в main.go) рендерит СТРАНИЦУ
# из того же файла, просто подставляя поля из currentState() напрямую в
# памяти, а не через файлы в config_data, как делает cover. Единый
# источник разметки -- не две разные копии UI, рискующие разойтись.
COPY site/admin /site/admin
ENTRYPOINT ["/usr/local/bin/panel-admin"]
