# Контекст сборки -- корень репозитория (не ./cover), иначе не достать
# ../site/html и ../site/admin для COPY ниже. См. docker-compose.*.yml:
# build.context: ., dockerfile: cover/Dockerfile.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY cover/go.mod cover/main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/cover .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cover /usr/local/bin/cover
# Decoy-страница и шаблон мини-панели запечены прямо в образ (а не
# host bind-mount) -- на Synology иначе пользователю пришлось бы руками
# класть эти файлы в папку проекта через File Station до поднятия стека.
# Это публичный, не секретный контент (в отличие от ./certs -- те
# по-прежнему bind-mount, см. комментарий в docker-compose.*.yml).
COPY site/html /site/html
COPY site/admin /site/admin
ENTRYPOINT ["/usr/local/bin/cover"]
