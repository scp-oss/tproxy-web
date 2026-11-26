# SOCKS5 с логином/паролем и fallback-туннелем через Cloudflare Worker, когда
# прямой путь к Telegram с этого сервера закрыт (DPI/провайдер). См. main.go.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/socks5 .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/socks5 /usr/local/bin/socks5
ENTRYPOINT ["/usr/local/bin/socks5"]
