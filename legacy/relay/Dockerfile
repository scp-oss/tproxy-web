# Relay tproxy-server (Go). Собирается из исходников официального репозитория.
FROM golang:1.24-bookworm AS build
ARG TPROXY_REF=master
WORKDIR /src
RUN git clone https://github.com/telegramdesktop/tproxy-server.git . \
 && git checkout "$TPROXY_REF"
# Тесты репозитория; тесты, которым нужны node/caddy, сами пропускаются.
RUN go test ./... \
 && CGO_ENABLED=0 go build -trimpath -o /out/tproxy-server ./cmd/tproxy-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tproxy-server /usr/local/bin/tproxy-server
# uid 65532 — владелец config/token.key и config/profiles.json (их ставит init.sh)
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/tproxy-server"]
CMD ["-config", "/config/config.json"]
