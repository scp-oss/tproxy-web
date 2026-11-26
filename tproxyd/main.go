// tproxyd -- весь tproxy-web в одном процессе (и в одном контейнере).
//
// Раньше: 7 контейнеров (caddy, cover, relay, tgws, panel-admin, mtproto,
// socks5), общий network namespace, панель с docker.sock, которая включала
// модули через `docker compose up/stop` по своей копии compose-файла.
//
// Теперь tproxyd сам:
//   - панель (логин/пароль), работает всегда, от модулей не зависит;
//   - вход для WEB-прокси (HTTP на FRONT_ADDR, по желанию ещё и HTTPS на
//     FRONT_TLS_ADDR): путь панели -> панель, остальное -> relay, а при
//     выключенном WEB -> страница входа (сайт-прикрытие);
//   - модули -- дочерние процессы, которые он запускает/останавливает и
//     перезапускает при падении:
//       web     = tproxy-server (relay) + tg-ws-proxy за ним (127.0.0.1:2398);
//       mtproto = tg-ws-proxy с fake-TLS на своём порту;
//       socks5  = sing-box (SOCKS5 с логином; TCP и UDP). TCP идёт через
//                 «выход» (Shadowsocks-сервер, если задан) или через
//                 Cloudflare Worker; UDP (звонки) -- только через выход:
//                 Worker умеет лишь TCP.
//
// Все настройки и секреты -- в /config (тот же volume, что у старой версии:
// секреты и ссылки клиентов при переходе не меняются).
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/armon/go-socks5"
	"github.com/gorilla/websocket"
	qrcode "github.com/skip2/go-qrcode"
)

// ---------------------------------------------------------------- пути и env

var (
	configDir    = envDefault("CONFIG_DIR", "/config")
	shareDir     = envDefault("SHARE_DIR", "/usr/share/tproxyd")
	certDir      = envDefault("CERT_DIR", "/certs")
	panelAddr    = envDefault("PANEL_ADDR", "0.0.0.0:7655")
	frontAddr    = envDefault("FRONT_ADDR", "127.0.0.1:19443") // начальное значение; дальше -- из панели (WEB_HTTP_ADDR)
	frontTLSAddr = envDefault("FRONT_TLS_ADDR", "")               // начальное значение; дальше -- из панели (WEB_TLS_PORT)

	relayBin   = envDefault("RELAY_BIN", "/usr/local/bin/tproxy-server")
	tgwsPython = envDefault("TGWS_PYTHON", "/opt/venv/bin/python")
	tgwsDir    = envDefault("TGWS_DIR", "/app")
	singboxBin = envDefault("SINGBOX_BIN", "/usr/local/bin/sing-box")

	// локальный SOCKS (без логина) -> Cloudflare Worker, только для sing-box
	workerDialer = envDefault("WORKER_DIALER_ADDR", "127.0.0.1:41080")
)

const (
	relayAddr     = "127.0.0.1:18080" // совпадает с config.json (listen)
	decoyAddr     = "127.0.0.1:13000" // config.json public_upstream
	tgwsWebAddr   = "127.0.0.1:2398"  // backend в profiles.json
	sessionCookie = "panel_session"
	sessionTTL    = 15 * time.Minute
)

func envDefault(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------- случайности

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("rand: %v", err)
	}
	return b
}
func randHex(n int) string { return hex.EncodeToString(randBytes(n)) }
func randBase32Lower(n int) string {
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randBytes(n)))[:n]
}

// ---------------------------------------------------------------- файлы в /config

func cfgPath(name string) string { return configDir + "/" + name }

func readTrim(name string) string {
	b, err := os.ReadFile(cfgPath(name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeFile(name, val string, mode os.FileMode) error {
	tmp := cfgPath(name) + ".tmp"
	if err := os.WriteFile(tmp, []byte(val+"\n"), mode); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath(name))
}

// ensure: значение из env (если задано) или из файла, иначе сгенерировать; сохраняет в файл.
func ensure(envKey, name string, gen func() string) string {
	v := ""
	if envKey != "" {
		v = strings.TrimSpace(os.Getenv(envKey))
	}
	if v == "" {
		v = readTrim(name)
	}
	if v == "" {
		v = gen()
	}
	if readTrim(name) != v {
		if err := writeFile(name, v, 0o600); err != nil {
			log.Fatalf("write %s: %v", name, err)
		}
	}
	return v
}

func flag(name string) bool        { return readTrim(name) == "1" }
func setFlag(name string, on bool) { v := ""; if on { v = "1" }; _ = writeFile(name, v, 0o600) }

// ---------------------------------------------------------------- настройки (panel.env)
//
// panel.env -- KEY=VALUE, тот же файл, что у прежней панели. Переменные
// окружения контейнера важнее файла (их задаёт тот, кто разворачивает).

var settingsMu sync.Mutex

func readSettings() map[string]string {
	out := map[string]string{}
	f, err := os.Open(cfgPath("panel.env"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func writeSettings(upd map[string]string) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	cur := readSettings()
	for k, v := range upd {
		if v == "" {
			delete(cur, k)
		} else {
			cur[k] = v
		}
	}
	keys := make([]string, 0, len(cur))
	for k := range cur {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%s\n", k, cur[k])
	}
	return writeFile("panel.env", strings.TrimSuffix(sb.String(), "\n"), 0o600)
}

func setting(k string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return readSettings()[k]
}

// ---------------------------------------------------------------- состояние, общее для всех

var (
	hostname     string
	adminPath    string
	adminUser    string
	adminPass    string
	tokenKey     []byte
	workerDomain string
	workerAuth   string
	onlyWorker   bool
	mu           sync.Mutex // сериализует изменения из панели
)

// bootstrap -- первичные секреты и конфиги relay (как прежний panel-admin).
func bootstrap() {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		log.Fatalf("mkdir %s: %v", configDir, err)
	}
	hostname = setting("HOSTNAME_ARG")
	if hostname == "" {
		log.Fatal("не задан HOSTNAME_ARG (домен, по которому клиенты приходят на WEB-прокси)")
	}
	workerDomain = setting("CF_WORKER")
	onlyWorker = setting("TGWS_ONLY_WORKER") == "1"
	if workerDomain != "" {
		workerAuth = ensure("CF_WORKER_AUTH", "worker_auth", func() string {
			return base64.RawURLEncoding.EncodeToString(randBytes(24))
		})
	}

	secret := ensure("", "secret.hex", func() string {
		s := randHex(16)
		if workerDomain != "" {
			s = "dd" + s
		}
		return s
	})
	basePath := ensure("", "base_path", func() string { return randBase32Lower(10) })
	adminPath = ensure("ADMIN_PATH", "admin_path", func() string { return "panel-" + randBase32Lower(32) })
	adminUser = ensure("ADMIN_USER", "admin_user", func() string { return "admin" + randHex(2) })
	adminPass = ensure("ADMIN_PASSWORD", "admin_password", func() string { return randBase32Lower(10) })

	if _, err := os.Stat(cfgPath("token.key")); os.IsNotExist(err) {
		if err := os.WriteFile(cfgPath("token.key"), randBytes(32), 0o600); err != nil {
			log.Fatalf("token.key: %v", err)
		}
	}
	var err error
	if tokenKey, err = os.ReadFile(cfgPath("token.key")); err != nil {
		log.Fatalf("token.key: %v", err)
	}
	writeRelayConfig(secret, basePath)

	// WEB по умолчанию включён (так было и раньше: relay стартовал всегда).
	if _, err := os.Stat(cfgPath("web_enabled")); os.IsNotExist(err) {
		setFlag("web_enabled", true)
	}

	log.Printf("============================================================")
	log.Printf("  Панель: http://<IP-сервера>:%s/  и  https://%s/%s/", portOf(panelAddr), hostname, adminPath)
	log.Printf("  Логин:  %s", adminUser)
	log.Printf("  Пароль: %s", adminPass)
	log.Printf("============================================================")
}

func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return p
}

func writeRelayConfig(secret, basePath string) {
	bare := strings.TrimPrefix(secret, "dd")
	profiles := map[string]any{"profiles": []map[string]string{{
		"name": "default", "secret": bare, "backend": tgwsWebAddr, "carrier_mode": "https-lanes",
	}}}
	pb, _ := json.MarshalIndent(profiles, "", "  ")
	if err := writeFile("profiles.json", string(pb), 0o600); err != nil {
		log.Fatalf("profiles.json: %v", err)
	}
	tmpl, err := os.ReadFile(shareDir + "/config.json.template")
	if err != nil {
		log.Fatalf("config.json.template: %v", err)
	}
	c := strings.ReplaceAll(string(tmpl), "__HOSTNAME__", hostname)
	c = strings.ReplaceAll(c, "__BASE_PATH__", basePath)
	c = strings.ReplaceAll(c, "/config/", configDir+"/")
	if err := writeFile("config.json", strings.TrimSpace(c), 0o600); err != nil {
		log.Fatalf("config.json: %v", err)
	}
}

// ---------------------------------------------------------------- дочерние процессы

type proc struct {
	name  string
	build func() (*exec.Cmd, error)

	mu       sync.Mutex
	want     bool
	cmd      *exec.Cmd
	done     chan struct{}
	lastErr  string
	restarts int
	started  time.Time
	tail     []string
}

func (p *proc) logLine(s string) {
	log.Printf("[%s] %s", p.name, s)
	p.mu.Lock()
	p.tail = append(p.tail, s)
	if len(p.tail) > 40 {
		p.tail = p.tail[len(p.tail)-40:]
	}
	p.mu.Unlock()
}

type lineWriter struct {
	p   *proc
	buf []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.p.logLine(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 8192 {
		w.p.logLine(string(w.buf))
		w.buf = nil
	}
	return len(b), nil
}

func (p *proc) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.want = true
	if p.done != nil {
		return // уже работает (или перезапускается)
	}
	p.done = make(chan struct{})
	go p.loop(p.done)
}

func (p *proc) loop(done chan struct{}) {
	defer close(done)
	backoff := time.Second
	for {
		p.mu.Lock()
		if !p.want {
			p.done = nil
			p.mu.Unlock()
			return
		}
		cmd, err := p.build()
		if err == nil {
			cmd.Stdout = &lineWriter{p: p}
			cmd.Stderr = cmd.Stdout
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			err = cmd.Start()
		}
		if err != nil {
			p.lastErr = err.Error()
			p.mu.Unlock()
			log.Printf("[%s] не запустился: %v", p.name, err)
		} else {
			p.cmd = cmd
			p.started = time.Now()
			p.mu.Unlock()
			log.Printf("[%s] запущен (pid %d)", p.name, cmd.Process.Pid)
			werr := cmd.Wait()
			p.mu.Lock()
			p.cmd = nil
			ran := time.Since(p.started)
			if p.want {
				p.restarts++
				p.lastErr = fmt.Sprintf("процесс завершился: %v", werr)
				if ran > time.Minute {
					backoff = time.Second
				}
			}
			p.mu.Unlock()
			if werr != nil {
				log.Printf("[%s] завершился: %v", p.name, werr)
			}
		}
		// пауза перед перезапуском; Stop() может прервать её через want=false
		for t := time.Duration(0); t < backoff; t += 200 * time.Millisecond {
			time.Sleep(200 * time.Millisecond)
			p.mu.Lock()
			w := p.want
			p.mu.Unlock()
			if !w {
				break
			}
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (p *proc) Stop() {
	p.mu.Lock()
	p.want = false
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		if cmd != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
	}
	p.mu.Lock()
	p.lastErr, p.restarts = "", 0
	p.mu.Unlock()
}

func (p *proc) Restart() { p.Stop(); p.Start() }

type procStatus struct {
	Running  bool   `json:"running"`
	Uptime   int64  `json:"uptime,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
	LastErr  string `json:"lastError,omitempty"`
}

func (p *proc) Status() procStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := procStatus{Running: p.cmd != nil, Restarts: p.restarts, LastErr: p.lastErr}
	if s.Running {
		s.Uptime = int64(time.Since(p.started).Seconds())
		if s.Uptime > 60 {
			s.LastErr = ""
		}
	}
	return s
}

// ---------------------------------------------------------------- модули

func tgwsCmd(args ...string) (*exec.Cmd, error) {
	all := append([]string{"-u", "proxy/tg_ws_proxy.py"}, args...)
	if workerDomain != "" {
		all = append(all, "--cfproxy-worker-domain", workerDomain)
	}
	if onlyWorker {
		all = append(all, "--pool-size", "0", "--no-cfproxy")
	}
	cmd := exec.Command(tgwsPython, all...)
	cmd.Dir = tgwsDir
	cmd.Env = append(os.Environ(), "TG_WS_PROXY_CF_WORKER_AUTH="+workerAuth, "PYTHONUNBUFFERED=1")
	return cmd, nil
}

var (
	relayProc = &proc{name: "relay", build: func() (*exec.Cmd, error) {
		return exec.Command(relayBin, "-config", cfgPath("config.json")), nil
	}}
	tgwsWebProc = &proc{name: "web-tgws", build: func() (*exec.Cmd, error) {
		host, port, _ := net.SplitHostPort(tgwsWebAddr)
		return tgwsCmd("--host", host, "--port", port, "--secret", strings.TrimPrefix(readTrim("secret.hex"), "dd"))
	}}
	mtprotoProc = &proc{name: "mtproto", build: func() (*exec.Cmd, error) {
		port, sni, sec := setting("MTPROTO_PORT"), setting("MTPROTO_FAKE_TLS_DOMAIN"), readTrim("mtproto_secret.hex")
		if !validPort(port) || sni == "" || sec == "" {
			return nil, errors.New("MTProto не настроен (порт/SNI/секрет)")
		}
		return tgwsCmd("--host", "0.0.0.0", "--port", port, "--secret", sec, "--fake-tls-domain", sni)
	}}
	socksProc = &proc{name: "sing-box", build: func() (*exec.Cmd, error) {
		if err := writeSingbox(); err != nil {
			return nil, err
		}
		return exec.Command(singboxBin, "run", "-c", cfgPath("singbox.json")), nil
	}}
)

func applyModules() {
	if flag("web_enabled") {
		tgwsWebProc.Start()
		relayProc.Start()
	} else {
		relayProc.Stop()
		tgwsWebProc.Stop()
	}
	if flag("mtproto_enabled") {
		mtprotoProc.Start()
	} else {
		mtprotoProc.Stop()
	}
	if egressWanted() {
		socksProc.Start()
	} else {
		socksProc.Stop()
	}
	if flag("socks5_enabled") {
		if !front.Running() {
			front.Start()
		}
	} else {
		front.Stop()
	}
}

// ---------------------------------------------------------------- выход (Shadowsocks) для SOCKS

type ssUpstream struct {
	Method, Password, Host string
	Port                   int
}

// parseSS понимает ss://BASE64(method:pass)@host:port и ss://method:pass@host:port (SIP002).
func parseSS(s string) (*ssUpstream, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "ss" || u.User == nil {
		return nil, errors.New("нужна ссылка вида ss://метод:пароль@хост:порт")
	}
	var method, pass string
	if p, ok := u.User.Password(); ok {
		method, pass = u.User.Username(), p
	} else {
		raw := u.User.Username()
		var dec []byte
		for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
			if dec, err = enc.DecodeString(raw); err == nil {
				break
			}
		}
		if err != nil {
			return nil, errors.New("не разобрать метод и пароль в ss://-ссылке")
		}
		m, p, ok := strings.Cut(string(dec), ":")
		if !ok {
			return nil, errors.New("не разобрать метод и пароль в ss://-ссылке")
		}
		method, pass = m, p
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || u.Hostname() == "" || port <= 0 || port > 65535 {
		return nil, errors.New("в ss://-ссылке нет хоста или порта")
	}
	return &ssUpstream{Method: method, Password: pass, Host: u.Hostname(), Port: port}, nil
}

// egressWanted: sing-box (выход) нужен SOCKS5 и WireGuard.
func egressWanted() bool {
	return flag("socks5_enabled") || wgSingboxEndpoint() != nil
}

func writeSingbox() error {
	up, err := parseSS(setting("UPSTREAM_SS"))
	if err != nil {
		return err
	}
	sbHost, sbp, _ := net.SplitHostPort(singboxAddr)
	sbPort, _ := strconv.Atoi(sbp)
	outbounds := []map[string]any{}
	rules := []map[string]any{}
	final := ""
	if up != nil {
		outbounds = append(outbounds, map[string]any{
			"type": "shadowsocks", "tag": "upstream", "server": up.Host, "server_port": up.Port,
			"method": up.Method, "password": up.Password,
		})
		final = "upstream"
	} else {
		rules = append(rules, map[string]any{"network": []string{"udp"}, "action": "reject"})
	}
	if workerDomain != "" {
		host, wp, _ := net.SplitHostPort(workerDialer)
		wpn, _ := strconv.Atoi(wp)
		outbounds = append(outbounds, map[string]any{"type": "socks", "tag": "worker", "server": host, "server_port": wpn, "version": "5"})
		if final == "" {
			final = "worker"
		}
	}
	if final == "" {
		outbounds = append(outbounds, map[string]any{"type": "direct", "tag": "direct"})
		final = "direct"
	}
	lvl := setting("SINGBOX_LOG")
	if lvl == "" {
		lvl = "warn"
	}
	conf := map[string]any{
		"log": map[string]any{"level": lvl, "timestamp": false},
		// наружу смотрит socksFront (socks.go), sing-box -- только выход, на loopback без логина
		"inbounds": []map[string]any{{
			"type": "socks", "tag": "in", "listen": sbHost, "listen_port": sbPort,
		}},
		"outbounds": outbounds,
		"route":     map[string]any{"rules": rules, "final": final},
	}
	if ep := wgSingboxEndpoint(); ep != nil {
		conf["endpoints"] = []map[string]any{ep}
		// DNS устройств (запросы на 10.77.0.1:53 из туннеля) -- через DoH: при выходе
		// за границу -- через него (Cloudflare), без выхода -- напрямую к Google DoH.
		doh := map[string]any{"type": "https", "tag": "doh", "server": "8.8.8.8"}
		if up != nil {
			doh = map[string]any{"type": "https", "tag": "doh", "server": "1.1.1.1", "detour": "upstream"}
		}
		conf["dns"] = map[string]any{"servers": []map[string]any{doh}, "final": "doh"}
		rules = append([]map[string]any{
			{"inbound": []string{"wg-in"}, "action": "sniff"},
			{"inbound": []string{"wg-in"}, "protocol": "dns", "action": "hijack-dns"},
		}, rules...)
		conf["route"] = map[string]any{"rules": rules, "final": final}
	}
	b, _ := json.MarshalIndent(conf, "", "  ")
	return writeFile("singbox.json", string(b), 0o600)
}

// ---------------------------------------------------------------- локальный SOCKS -> Cloudflare Worker

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
func (c *wsConn) Close() error         { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// Worker сам пускает только к адресам Telegram и всегда на порт 443.
func dialWorker(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve %s: %v", host, err)
		}
		ip = ips[0]
	}
	u := url.URL{Scheme: "wss", Host: workerDomain, Path: "/apiws"}
	q := u.Query()
	q.Set("auth", workerAuth)
	q.Set("dst", ip.String())
	u.RawQuery = q.Encode()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(dctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	return &wsConn{ws: ws}, nil
}

func runWorkerDialer() {
	if workerDomain == "" {
		return
	}
	srv, err := socks5.New(&socks5.Config{Dial: dialWorker, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		log.Fatalf("worker dialer: %v", err)
	}
	go func() {
		// не Fatal: без него не работает только TCP через Worker у SOCKS, панель и остальное живут
		log.Printf("локальный SOCKS -> Worker (%s) остановлен: %v", workerDialer, srv.ListenAndServe("tcp", workerDialer))
	}()
}

// ---------------------------------------------------------------- ссылки

func webLink(secretHex, basePath string) string {
	raw, err := hex.DecodeString(strings.TrimPrefix(secretHex, "dd"))
	if err != nil || secretHex == "" {
		return ""
	}
	enc := base64.RawURLEncoding.EncodeToString(append([]byte{0x70}, raw...))
	server := hostname
	if basePath != "" {
		server = hostname + "%2F" + basePath
	}
	return fmt.Sprintf("https://t.me/webproxy?server=%s&secret=%s", server, enc)
}

func mtprotoLink(secret, sni, port string) string {
	return fmt.Sprintf("tg://proxy?server=%s&port=%s&secret=ee%s%s", linkHost(), port, secret, hex.EncodeToString([]byte(sni)))
}

func localIP() string {
	if v := setting("LOCAL_IP"); v != "" {
		return v
	}
	c, err := net.Dial("udp", "1.1.1.1:53") // UDP connect не шлёт пакетов, только выбирает маршрут
	if err != nil {
		return ""
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP
	if ip.IsPrivate() {
		return ip.String()
	}
	return ""
}

// useLocalIP -- общий переключатель панели: в ссылках/QR MTProto, SOCKS5 и
// WireGuard локальный IP вместо домена. WEB-прокси всегда по домену (нужен 443 и TLS).
func useLocalIP() bool {
	if localIP() == "" {
		return false
	}
	v := readTrim("use_local_ip")
	if v == "" {
		v = readTrim("socks5_use_local_ip") // прежняя настройка блока SOCKS5
	}
	return v == "" || v == "1"
}

func linkHost() string {
	if useLocalIP() {
		return localIP()
	}
	return hostname
}

func socksLocalActive() bool { return useLocalIP() }

func socksLink(user, pass, port string) string {
	server := hostname
	if socksLocalActive() {
		server = localIP()
	}
	q := url.Values{}
	q.Set("server", server)
	q.Set("port", port)
	q.Set("user", user)
	q.Set("pass", pass)
	return "https://t.me/socks?" + q.Encode()
}

func qrDataURI(link string) string {
	if link == "" {
		return ""
	}
	png, err := qrcode.Encode(link, qrcode.Medium, 220)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 65536
}

// ---------------------------------------------------------------- состояние для панели

type protoState struct {
	Enabled bool   `json:"enabled"`
	Port    string `json:"port,omitempty"`
	SNI     string `json:"sni,omitempty"`
	User    string `json:"user,omitempty"`
	Link    string `json:"link,omitempty"`
	Open    string `json:"open,omitempty"`
	QR      string `json:"qr,omitempty"`
	Note    string `json:"note,omitempty"`
	Problem string `json:"problem,omitempty"` // включён, но процесс не работает
	Share   string `json:"share,omitempty"`   // https://t.me/... -- для пересылки тем, у кого t.me открывается

	LocalIPAvailable bool   `json:"localIpAvailable,omitempty"`
	LocalIPActive    bool   `json:"localIpActive,omitempty"`
	Upstream         string `json:"upstream,omitempty"` // host:port выхода (без пароля)
	HTTPAddr         string `json:"http_addr,omitempty"` // WEB: HTTP-вход для обратного прокси
	TLSPort          string `json:"tls_port,omitempty"`  // WEB: прямой HTTPS-вход
	Calls            bool   `json:"calls,omitempty"`    // UDP есть куда выпускать
}

func problemOf(ps ...*proc) string {
	for _, p := range ps {
		s := p.Status()
		if !s.Running {
			if s.LastErr != "" {
				return p.name + ": " + s.LastErr
			}
			return p.name + ": запускается…"
		}
	}
	return ""
}

func currentState() map[string]protoState {
	out := map[string]protoState{}

	sec, base := readTrim("secret.hex"), readTrim("base_path")
	wl := webLink(sec, base)
	wtg := strings.Replace(wl, "https://t.me/", "tg://", 1)
	web := protoState{Enabled: flag("web_enabled"), Link: wtg, Open: wtg, QR: qrDataURI(wtg), Share: wl,
		HTTPAddr: webHTTPAddr(), TLSPort: webTLSPort()}
	if web.Enabled {
		web.Problem = problemOf(tgwsWebProc, relayProc)
		if e := webListenErr(); web.Problem == "" && e != "" {
			web.Problem = e
		}
	}
	out["web"] = web

	mp, msni, msec := setting("MTPROTO_PORT"), setting("MTPROTO_FAKE_TLS_DOMAIN"), readTrim("mtproto_secret.hex")
	mt := protoState{Enabled: flag("mtproto_enabled"), Port: mp, SNI: msni}
	if validPort(mp) && msni != "" && msec != "" {
		mt.Link = mtprotoLink(msec, msni, mp)
		mt.Open, mt.QR = mt.Link, qrDataURI(mt.Link)
	}
	if mt.Enabled {
		mt.Problem = problemOf(mtprotoProc)
	}
	out["mtproto"] = mt

	sp, su, spass := setting("SOCKS5_PORT"), setting("SOCKS5_USER"), readTrim("socks5_pass.hex")
	s5 := protoState{Enabled: flag("socks5_enabled"), Port: sp, User: su, LocalIPAvailable: localIP() != "", LocalIPActive: socksLocalActive()}
	if validPort(sp) && su != "" && spass != "" {
		s5.Share = socksLink(su, spass, sp)
		s5.Link = strings.Replace(s5.Share, "https://t.me/", "tg://", 1)
		s5.Open, s5.QR = s5.Link, qrDataURI(s5.Link)
	}
	if up, _ := parseSS(setting("UPSTREAM_SS")); up != nil {
		s5.Upstream = net.JoinHostPort(up.Host, strconv.Itoa(up.Port))
		s5.Calls = true
	}
	if s5.Enabled {
		s5.Problem = problemOf(socksProc)
		if e := front.Err(); s5.Problem == "" && e != "" {
			s5.Problem = e
		}
	}
	out["socks5"] = s5
	return out
}

// ---------------------------------------------------------------- диагностика

type statusInfo struct {
	CFWorkerOK   bool                  `json:"cfWorkerOk"`
	CFWorkerHost string                `json:"cfWorkerHost,omitempty"`
	TGDCOK       bool                  `json:"tgDcOk"`
	UpstreamOK   *bool                 `json:"upstreamOk,omitempty"`
	Procs        map[string]procStatus `json:"procs"`
}

var (
	statusMu   sync.RWMutex
	lastStatus statusInfo
)

func checkTLS(host string) bool {
	d := &net.Dialer{Timeout: 6 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(host, "443"), &tls.Config{ServerName: host})
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func statusLoop() {
	for {
		s := statusInfo{CFWorkerHost: workerDomain}
		if workerDomain != "" {
			s.CFWorkerOK = checkTLS(workerDomain)
		}
		s.TGDCOK = s.CFWorkerOK
		if up, _ := parseSS(setting("UPSTREAM_SS")); up != nil {
			c, err := net.DialTimeout("tcp", net.JoinHostPort(up.Host, strconv.Itoa(up.Port)), 6*time.Second)
			ok := err == nil
			if ok {
				_ = c.Close()
			}
			s.UpstreamOK = &ok
		}
		statusMu.Lock()
		lastStatus = s
		statusMu.Unlock()
		time.Sleep(30 * time.Second)
	}
}

// ---------------------------------------------------------------- сессии

func signSession(exp int64) string {
	m := hmac.New(sha256.New, tokenKey)
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	ts, _, ok := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(ts, 10, 64)
	if !ok || err != nil || time.Now().Unix() > exp {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(signSession(exp)), []byte(c.Value)) == 1
}

// ---------------------------------------------------------------- HTTP: панель

const loginPage = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>NodeSentry — Sign in</title>
<style>body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
form{background:#1c1c1c;padding:32px;border-radius:12px;width:280px}input{width:100%%;padding:10px;margin:8px 0;border-radius:6px;border:1px solid #333;background:#0d0d0d;color:#eee;box-sizing:border-box;font-size:15px}
button{width:100%%;padding:10px;margin-top:8px;border-radius:6px;border:0;background:#2a7fff;color:#fff;font-weight:600;cursor:pointer;font-size:15px}.err{color:#ff6767;font-size:14px;margin-top:8px}h1{font-size:18px;margin:0 0 16px}</style></head><body>
<form method="post" action="%s"><h1>NodeSentry</h1><input name="user" placeholder="Username" autofocus required><input name="pass" type="password" placeholder="Password" required><button type="submit">Sign in</button>%s</form></body></html>`

var adminTemplate string

type panel struct {
	prefix   string // "" для отдельного порта панели, "/<admin_path>" на общем домене
	needAuth bool   // на общем домене пускает знание секретного пути (как и раньше)
}

func (pn panel) authed(r *http.Request) bool { return !pn.needAuth || validSession(r) }

func (pn panel) routes(mux *http.ServeMux) {
	p := pn.prefix
	mux.HandleFunc("GET "+p+"/login", func(w http.ResponseWriter, r *http.Request) {
		if validSession(r) {
			http.Redirect(w, r, p+"/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, loginPage, p+"/login", "")
	})
	mux.HandleFunc("POST "+p+"/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		okU := subtle.ConstantTimeCompare([]byte(r.FormValue("user")), []byte(adminUser)) == 1
		okP := subtle.ConstantTimeCompare([]byte(r.FormValue("pass")), []byte(adminPass)) == 1
		if !okU || !okP {
			time.Sleep(700 * time.Millisecond)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, loginPage, p+"/login", `<div class="err">Неверный логин или пароль</div>`)
			return
		}
		exp := time.Now().Add(sessionTTL).Unix()
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: signSession(exp), Path: "/", HttpOnly: true,
			SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()), Secure: r.TLS != nil})
		http.Redirect(w, r, p+"/", http.StatusFound)
	})
	mux.HandleFunc("GET "+p+"/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, p+"/login", http.StatusFound)
	})
	mux.HandleFunc("GET "+p+"/{$}", func(w http.ResponseWriter, r *http.Request) {
		if !pn.authed(r) {
			http.Redirect(w, r, p+"/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(renderDashboard(pn.needAuth)))
	})
	api := func(method, path string, h http.HandlerFunc) {
		mux.HandleFunc(method+" "+p+"/api/"+path, func(w http.ResponseWriter, r *http.Request) {
			if !pn.authed(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			h(w, r)
		})
	}
	api("GET", "state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, fullState()) })
	api("POST", "wg-peer", handleWgPeer)
	api("POST", "wg-local-ip", handleWgLocalIP)
	api("POST", "wg-invite", handleWgInvite)
	api("GET", "status", handleStatus)
	api("POST", "toggle", handleToggle)
	api("POST", "web-listen", handleWebListen)
	api("POST", "regen", handleRegen)
	api("POST", "socks5-local-ip", handleLocalIP)
	api("POST", "local-ip", handleLocalIP)
	api("POST", "upstream", handleUpstream)
	api("GET", "logs", handleLogs)
	mux.HandleFunc("GET "+p+"/dl/wireguard.apk", func(w http.ResponseWriter, r *http.Request) {
		if !pn.authed(r) {
			http.Redirect(w, r, p+"/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
		w.Header().Set("Content-Disposition", `attachment; filename="wireguard.apk"`)
		http.ServeFile(w, r, shareDir+"/wireguard.apk")
	})
	mux.HandleFunc("GET "+p+"/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
}

func renderDashboard(withLogout bool) string {
	st := currentState()
	page := strings.ReplaceAll(adminTemplate, "__HOSTNAME__", hostname)
	logout := ""
	if withLogout {
		logout = `<a href="logout" class="logout">Выйти</a>`
	}
	page = strings.ReplaceAll(page, "__LOGOUT__", logout)
	_ = st
	b, _ := json.Marshal(fullState())
	return strings.ReplaceAll(page, "__STATE_JSON__", string(b))
}

func fullState() map[string]any {
	out := map[string]any{}
	for k, v := range currentState() {
		out[k] = v
	}
	out["wg"] = currentWg()
	out["localIp"] = map[string]any{"available": localIP() != "", "active": useLocalIP(), "ip": localIP()}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	statusMu.RLock()
	s := lastStatus
	statusMu.RUnlock()
	s.Procs = map[string]procStatus{}
	for _, p := range []*proc{relayProc, tgwsWebProc, mtprotoProc, socksProc} {
		s.Procs[p.name] = p.Status()
	}
	writeJSON(w, s)
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	out := map[string][]string{}
	for _, p := range []*proc{relayProc, tgwsWebProc, mtprotoProc, socksProc} {
		p.mu.Lock()
		out[p.name] = append([]string(nil), p.tail...)
		p.mu.Unlock()
	}
	writeJSON(w, out)
}

type toggleReq struct {
	Proto string `json:"proto"`
	On    bool   `json:"on"`
	Port  string `json:"port"`
	SNI   string `json:"sni"`
	User  string `json:"user"`
	Pass  string `json:"pass"`
}

// portBusy: порт уже кем-то занят (не нашим же модулем).
func portBusy(port string, own *proc) bool {
	if own.Status().Running && setting(strings.ToUpper(own.name)+"_PORT") == port {
		return false
	}
	if own == socksProc && front.Running() && setting("SOCKS5_PORT") == port {
		return false
	}
	l, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

func handleToggle(w http.ResponseWriter, r *http.Request) {
	var req toggleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	switch req.Proto {
	case "wg":
		if err := toggleWg(req.On, req.Port); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		socksProc.Stop() // перечитать конфиг sing-box
		applyModules()
		time.Sleep(1500 * time.Millisecond)
		writeJSON(w, currentWg())
		return
	case "web":
		setFlag("web_enabled", req.On)
	case "mtproto", "socks5":
		if req.On {
			key := strings.ToUpper(req.Proto) + "_PORT"
			port := req.Port
			if port == "" {
				port = setting(key)
			}
			if !validPort(port) {
				http.Error(w, "нужен корректный порт", http.StatusBadRequest)
				return
			}
			own := mtprotoProc
			if req.Proto == "socks5" {
				own = socksProc
			}
			if portBusy(port, own) {
				http.Error(w, "порт "+port+" уже занят другой программой", http.StatusConflict)
				return
			}
			upd := map[string]string{key: port}
			if req.Proto == "mtproto" {
				sni := req.SNI
				if sni == "" {
					sni = setting("MTPROTO_FAKE_TLS_DOMAIN")
				}
				if sni == "" {
					sni = "www.google.com"
				}
				upd["MTPROTO_FAKE_TLS_DOMAIN"] = sni
				ensure("", "mtproto_secret.hex", func() string { return randHex(16) })
			} else {
				user := req.User
				if user == "" {
					user = setting("SOCKS5_USER")
				}
				if user == "" {
					user = "user" + randHex(2)
				}
				upd["SOCKS5_USER"] = user
				if req.Pass != "" {
					_ = writeFile("socks5_pass.hex", req.Pass, 0o600)
				} else {
					ensure("", "socks5_pass.hex", func() string { return randHex(12) })
				}
			}
			if err := writeSettings(upd); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		setFlag(req.Proto+"_enabled", req.On)
		if req.On { // новые порт/учётка -- перезапуск
			if req.Proto == "mtproto" {
				mtprotoProc.Stop()
			} else {
				front.Stop()
			}
		}
	default:
		http.Error(w, "bad proto", http.StatusBadRequest)
		return
	}
	applyModules()
	if req.Proto == "socks5" && socksProc.Status().Running {
		socksProc.Restart()
	}
	time.Sleep(1500 * time.Millisecond) // дать процессу подняться, чтобы сразу показать ошибку, если она есть
	writeJSON(w, currentState()[req.Proto])
}

func handleRegen(w http.ResponseWriter, r *http.Request) {
	var req struct{ Proto string `json:"proto"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	switch req.Proto {
	case "web":
		s := randHex(16)
		if workerDomain != "" {
			s = "dd" + s
		}
		_ = writeFile("secret.hex", s, 0o600)
		writeRelayConfig(s, readTrim("base_path"))
		if flag("web_enabled") {
			tgwsWebProc.Restart()
			relayProc.Restart()
		}
	case "mtproto":
		_ = writeFile("mtproto_secret.hex", randHex(16), 0o600)
		if flag("mtproto_enabled") {
			mtprotoProc.Restart()
		}
	case "socks5":
		_ = writeFile("socks5_pass.hex", randHex(12), 0o600)
		if flag("socks5_enabled") {
			front.Start()
		}
	default:
		http.Error(w, "bad proto", http.StatusBadRequest)
		return
	}
	writeJSON(w, currentState()[req.Proto])
}

func handleLocalIP(w http.ResponseWriter, r *http.Request) {
	var req struct{ On bool `json:"on"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	v := "0"
	if req.On {
		v = "1"
	}
	_ = writeFile("use_local_ip", v, 0o600)
	writeJSON(w, fullState())
}

func handleUpstream(w http.ResponseWriter, r *http.Request) {
	var req struct{ Link string `json:"link"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if _, err := parseSS(req.Link); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if err := writeSettings(map[string]string{"UPSTREAM_SS": strings.TrimSpace(req.Link)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if egressWanted() {
		socksProc.Restart()
		time.Sleep(1500 * time.Millisecond)
	}
	writeJSON(w, currentState()["socks5"])
}

// ---------------------------------------------------------------- HTTP: вход WEB-прокси

func reverseProxy(target string) *httputil.ReverseProxy {
	u := &url.URL{Scheme: "http", Host: target}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			host := pr.In.Host
			pr.SetURL(u)
			pr.Out.Host = host
			// relay принимает в X-Forwarded-For ровно один адрес: SetXForwarded заменяет, не дописывает.
			pr.SetXForwarded()
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			decoyHandler.ServeHTTP(w, r)
		},
	}
}

var decoyHandler http.Handler

// ---------------------------------------------------------------- вход WEB-прокси: адреса задаются в панели
// WEB_HTTP_ADDR -- HTTP для обратного прокси (NPM/Caddy) на этом сервере; WEB_TLS_PORT -- прямой HTTPS
// ("off" -- не открывать). Начальные значения -- FRONT_ADDR / FRONT_TLS_ADDR; смена -- без пересоздания контейнера.
// Ссылка для клиентов от порта не зависит: Telegram ходит на домен по 443, внешний 443 сопоставляет прокси или роутер.

type webListener struct {
	mu   sync.Mutex
	srv  *http.Server
	addr string
	err  string
}

var (
	webHTTP, webTLS webListener
	webHandler      http.Handler
)

func webHTTPAddr() string {
	if v := readSettings()["WEB_HTTP_ADDR"]; v != "" {
		return v
	}
	return frontAddr
}

func webTLSPort() string {
	switch v := readSettings()["WEB_TLS_PORT"]; v {
	case "off":
		return ""
	case "":
		if frontTLSAddr != "" {
			if _, p, err := net.SplitHostPort(frontTLSAddr); err == nil {
				return p
			}
		}
		return ""
	default:
		return v
	}
}

// listen открывает новый адрес и только потом закрывает старый: если порт занят, прежний вход продолжает работать.
// Старый сервер закрывается в фоне -- запрос, который меняет адрес, мог прийти через него же.
func (wl *webListener) listen(addr string, tlsOn bool) error {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	if addr == wl.addr && (wl.srv != nil || addr == "") {
		return nil
	}
	old := wl.srv
	if addr == "" {
		wl.srv, wl.addr, wl.err = nil, "", ""
	} else {
		ln, err := net.Listen("tcp", addr)
		if err != nil && old != nil && portOf(addr) == portOf(wl.addr) { // тот же порт, другой IP: порт держит старый
			_ = old.Close()
			old = nil
			ln, err = net.Listen("tcp", addr)
		}
		if err != nil {
			wl.err = err.Error()
			return err
		}
		srv := &http.Server{Handler: webHandler, ReadHeaderTimeout: 15 * time.Second}
		kind := "HTTP"
		if tlsOn {
			kind = "HTTPS"
			cl := &certLoader{}
			srv.TLSConfig = &tls.Config{GetCertificate: cl.get, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
		}
		go func() {
			var e error
			if tlsOn {
				e = srv.ServeTLS(ln, "", "")
			} else {
				e = srv.Serve(ln)
			}
			if e != nil && e != http.ErrServerClosed {
				log.Printf("вход WEB-прокси %s %s: %v", kind, addr, e)
			}
		}()
		wl.srv, wl.addr, wl.err = srv, addr, ""
		log.Printf("вход WEB-прокси (%s): %s", kind, addr)
	}
	if old != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = old.Shutdown(ctx)
		}()
	}
	return nil
}

func tlsListenAddr(port string) string {
	if port == "" {
		return ""
	}
	return ":" + port
}

func applyWebListeners() {
	if err := webHTTP.listen(webHTTPAddr(), false); err != nil {
		log.Printf("вход WEB-прокси HTTP %s: %v", webHTTPAddr(), err)
	}
	if err := webTLS.listen(tlsListenAddr(webTLSPort()), true); err != nil {
		log.Printf("вход WEB-прокси HTTPS :%s: %v", webTLSPort(), err)
	}
}

func webListenErr() string {
	var p []string
	for _, l := range []*webListener{&webHTTP, &webTLS} {
		l.mu.Lock()
		if l.err != "" {
			p = append(p, "вход WEB-прокси не открыт: "+l.err)
		}
		l.mu.Unlock()
	}
	return strings.Join(p, "; ")
}

// handleWebListen -- адреса входа WEB-прокси из панели. http_addr: «IP:порт» или только порт (тогда 127.0.0.1);
// tls_port: порт прямого HTTPS или пусто (не открывать).
func handleWebListen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HTTPAddr string `json:"http_addr"`
		TLSPort  string `json:"tls_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ha, tp := strings.TrimSpace(req.HTTPAddr), strings.TrimSpace(req.TLSPort)
	if ha == "" {
		ha = "127.0.0.1:19443"
	}
	if !strings.Contains(ha, ":") {
		ha = "127.0.0.1:" + ha
	}
	h, p, err := net.SplitHostPort(ha)
	if err != nil || !validPort(p) || (h != "" && net.ParseIP(h) == nil) {
		http.Error(w, "HTTP-вход: IP:порт, например 127.0.0.1:19443", http.StatusBadRequest)
		return
	}
	if tp != "" && !validPort(tp) {
		http.Error(w, "порт HTTPS: число 1–65535 или пусто", http.StatusBadRequest)
		return
	}
	if tp != "" && tp == p && (h == "" || h == "0.0.0.0" || h == "::") {
		http.Error(w, "HTTP и HTTPS не могут слушать один порт", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if err := webHTTP.listen(ha, false); err != nil {
		http.Error(w, "HTTP-вход "+ha+" не открыт: "+err.Error(), http.StatusConflict)
		return
	}
	_ = writeSettings(map[string]string{"WEB_HTTP_ADDR": ha})
	if err := webTLS.listen(tlsListenAddr(tp), true); err != nil {
		http.Error(w, "HTTPS-вход :"+tp+" не открыт: "+err.Error(), http.StatusConflict)
		return
	}
	tv := tp
	if tv == "" {
		tv = "off"
	}
	_ = writeSettings(map[string]string{"WEB_TLS_PORT": tv})
	writeJSON(w, currentState()["web"])
}

func frontHandler() http.Handler {
	mux := http.NewServeMux()
	panel{prefix: "/" + adminPath, needAuth: false}.routes(mux)
	relay := reverseProxy(relayAddr)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if flag("web_enabled") && relayProc.Status().Running {
			relay.ServeHTTP(w, r)
			return
		}
		decoyHandler.ServeHTTP(w, r)
	})
	return mux
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

// Сертификат перечитывается с диска раз в час: продление снаружи подхватится без перезапуска.
type certLoader struct {
	mu   sync.Mutex
	cert *tls.Certificate
	at   time.Time
}

func (c *certLoader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && time.Since(c.at) < time.Hour {
		return c.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(certDir+"/fullchain.pem", certDir+"/privkey.pem")
	if err != nil {
		if c.cert == nil {
			log.Printf("TLS: нет %s/fullchain.pem и privkey.pem -- самоподписанный сертификат", certDir)
			if cert, err = selfSignedCert(); err != nil {
				return nil, err
			}
		} else {
			return c.cert, nil
		}
	}
	c.cert, c.at = &cert, time.Now()
	return c.cert, nil
}

func serve(addr string, h http.Handler, tlsOn bool) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 15 * time.Second}
	if tlsOn {
		cl := &certLoader{}
		srv.TLSConfig = &tls.Config{GetCertificate: cl.get, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
		log.Printf("HTTPS-вход: %s", addr)
		log.Fatal(srv.ListenAndServeTLS("", ""))
	}
	log.Fatal(srv.ListenAndServe())
}

// ---------------------------------------------------------------- main

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	bootstrap()
	tpl, err := os.ReadFile(shareDir + "/admin.html")
	if err != nil {
		log.Fatalf("admin.html: %v", err)
	}
	adminTemplate = string(tpl)

	// отдельный порт панели: логин+пароль
	pmux := http.NewServeMux()
	panel{prefix: "", needAuth: true}.routes(pmux)
	joinRoutes(pmux) // приглашения для телефонов: без входа, по подписанной ссылке
	ensurePeerTokens()
	pmux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) })
	decoyHandler = pmux // сайт-прикрытие = страница входа в панель

	go statusLoop()
	runWorkerDialer()
	go serve(panelAddr, pmux, false)
	go serve(decoyAddr, pmux, false)
	webHandler = frontHandler()
	applyWebListeners()
	log.Printf("панель: %s, вход WEB-прокси: %s (адреса входа меняются в панели)", panelAddr, webHTTPAddr())
	applyModules()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	log.Printf("остановка…")
	front.Stop()
	var wg sync.WaitGroup
	for _, p := range []*proc{relayProc, tgwsWebProc, mtprotoProc, socksProc} {
		wg.Add(1)
		go func(p *proc) { defer wg.Done(); p.Stop() }(p)
	}
	wg.Wait()
}
