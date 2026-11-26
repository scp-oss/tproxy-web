// socks5 -- SOCKS5-сервер с логином/паролем (без TLS, см. docker-compose:
// у протокола нет собственного шифрования транспорта, и Telegram-клиенты
// не умеют SOCKS5-over-TLS). Заменяет сторонний serjs/go-socks5-proxy --
// тому пришлось бы всегда дозваниваться напрямую, а с этого сервера нет
// прямого доступа к Telegram (провайдер режет на уровне DPI/IP, подтверждено
// живым тестом: прямой TCP на DC 149.154.167.50:443 и api.telegram.org --
// таймаут). relay/mtproto (tg-ws-proxy, Flowseal) работают именно потому,
// что сами туннелируют через Cloudflare Worker, когда прямой путь недоступен
// -- тот же приём воспроизведён здесь для SOCKS5.
//
// Worker (cloudflare/worker.js) -- универсальный TCP-туннель по WebSocket:
// GET /apiws?auth=...&dst=<IP>, апгрейд на WebSocket, затем байты зеркалятся
// в TCP-соединение на dst:443 (порт захардкожен в самом Worker'е). Это НЕ
// MTProto-специфичный протокол на уровне байт -- значит, подходит для
// любого TCP-потока к Telegram, не только для MTProto-фреймов. Worker сам
// проверяет, что dst входит в диапазоны Telegram (встроенный список +
// официальный core.telegram.org/resources/cidr.txt) -- на посторонний IP
// ответит 404, так что эта логика безопасна и без дублирования проверки
// здесь.
package main

import (
	"context"
	"log"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/armon/go-socks5"
	"github.com/gorilla/websocket"
)

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("env %s не задан", k)
	}
	return v
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// wsConn оборачивает *websocket.Conn как net.Conn: каждое WS-сообщение --
// непрозрачный кусок байт потока, как и ожидает tg-ws-proxy/Worker с другой
// стороны (см. worker.js: server.send(value) на чтении из TCP, event.data
// на запись в TCP -- никакого собственного фрейминга сверху).
type wsConn struct {
	ws   *websocket.Conn
	rbuf []byte
}

func (c *wsConn) Read(b []byte) (int, error) {
	for len(c.rbuf) == 0 {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return 0, err
		}
		c.rbuf = data
	}
	n := copy(b, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *wsConn) Write(b []byte) (int, error) {
	if err := c.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *wsConn) Close() error                       { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr                { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// dialWorker открывает туннель через Cloudflare Worker на известный IP
// Telegram-датацентра. Порт запроса не передаётся -- Worker всегда сам
// дозванивается на dst:443 (см. worker.js), это согласовано с тем, как уже
// работает MTProto через тот же Worker.
func dialWorker(ctx context.Context, workerDomain, workerAuth, dstIP string) (net.Conn, error) {
	u := url.URL{Scheme: "wss", Host: workerDomain, Path: "/apiws"}
	q := u.Query()
	q.Set("auth", workerAuth)
	q.Set("dst", dstIP)
	u.RawQuery = q.Encode()

	// Явный таймаут здесь, а не только через Dialer.HandshakeTimeout -- не
	// полагаемся на то, что ctx от go-socks5 сам чем-то ограничен.
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	log.Printf("socks5: dialWorker start, dst=%s via %s", dstIP, workerDomain)
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, resp, err := dialer.DialContext(dctx, u.String(), nil)
	if err != nil {
		status := "n/a"
		if resp != nil {
			status = resp.Status
		}
		log.Printf("socks5: dialWorker FAILED dst=%s: %v (resp=%s)", dstIP, err, status)
		return nil, err
	}
	log.Printf("socks5: dialWorker OK dst=%s", dstIP)
	return &wsConn{ws: ws}, nil
}

// telegramNets -- те же встроенные подсети, что и в cloudflare/worker.js
// (запасной список там; тут используется только чтобы не тратить время на
// заведомо обречённую попытку прямого соединения с этого сервера).
var telegramNets = mustParseNets([]string{
	"149.154.160.0/20",
	"91.108.4.0/22",
	"91.108.8.0/22",
	"91.108.12.0/22",
	"91.108.16.0/22",
	"91.108.20.0/22",
	"91.108.56.0/22",
	"91.105.192.0/23",
	"95.161.64.0/20",
	"185.76.151.0/24",
})

func mustParseNets(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			log.Fatalf("плохой CIDR %q: %v", c, err)
		}
		nets = append(nets, n)
	}
	return nets
}

func looksLikeTelegram(ip net.IP) bool {
	for _, n := range telegramNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// makeDialer: сперва прямое соединение (короткий таймаут -- с этого сервера
// оно для Telegram всё равно не проходит, не стоит подвешивать клиента),
// при неудаче -- туннель через Worker. Для IP из известных диапазонов
// Telegram прямую попытку пропускаем вовсе: по живому тесту она гарантированно
// виснет на таймаут, а клиент (Telegram-приложение) в это время показывает
// "соединение" и в итоге сам отваливается по своему таймауту.
func makeDialer(workerDomain, workerAuth string, directTimeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		log.Printf("socks5: dial requested network=%s addr=%s", network, addr)
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			ips, rerr := net.DefaultResolver.LookupIP(ctx, "ip4", host)
			if rerr != nil || len(ips) == 0 {
				return nil, rerr
			}
			ip = ips[0]
		}

		if !looksLikeTelegram(ip) || workerDomain == "" {
			// Без CF_WORKER (сейчас это обычный режим -- SOCKS5 используется
			// как туннель общего назначения внутри локальной сети, не как
			// способ обхода блокировок Telegram, см. пометку в панели) --
			// fallback через Worker в принципе недоступен, пробуем только
			// прямое соединение, с обычным таймаутом подключения.
			dctx, cancel := context.WithTimeout(ctx, directTimeout)
			defer cancel()
			conn, derr := (&net.Dialer{}).DialContext(dctx, network, addr)
			if derr == nil {
				return conn, nil
			}
			if workerDomain == "" {
				return nil, derr
			}
			log.Printf("socks5: прямое соединение на %s не удалось (%v), пробую через Worker", addr, derr)
		}

		return dialWorker(ctx, workerDomain, workerAuth, ip.String())
	}
}

type staticCreds map[string]string

func (c staticCreds) Valid(user, password string) bool {
	want, ok := c[user]
	return ok && want == password
}

func main() {
	listenAddr := envDefault("LISTEN_ADDR", "0.0.0.0:1080")
	user := mustEnv("PROXY_USER")
	pass := mustEnv("PROXY_PASSWORD")
	workerDomain := envDefault("CF_WORKER", "")
	workerAuth := envDefault("CF_WORKER_AUTH", "")
	directTimeout := 3 * time.Second

	creds := staticCreds{user: pass}
	conf := &socks5.Config{
		AuthMethods: []socks5.Authenticator{socks5.UserPassAuthenticator{Credentials: creds}},
		Dial:        makeDialer(workerDomain, workerAuth, directTimeout),
		Logger:      log.New(os.Stdout, "socks5: ", log.LstdFlags),
	}
	server, err := socks5.New(conf)
	if err != nil {
		log.Fatalf("socks5.New: %v", err)
	}

	log.Printf("socks5: слушаю %s, fallback на Worker %s при недоступности прямого пути", listenAddr, workerDomain)
	log.Fatal(server.ListenAndServe("tcp", listenAddr))
}
