package main

// Модуль «WireGuard для Telegram».
//
// Зачем: Telegram для Android (13.x) не пускает звонки через прокси вообще
// (в коде звонка прокси передаётся как null), а голосу нужен UDP. Профиль
// WireGuard на телефоне с AllowedIPs = только сети Telegram заворачивает в
// туннель весь трафик Telegram -- сообщения, звонки, видео; остальной интернет
// телефона идёт мимо. На сервере туннель принимает sing-box (endpoint
// wireguard, без модуля ядра) и выпускает трафик тем же выходом, что SOCKS5:
// Shadowsocks за границей, а без него TCP -- через Cloudflare Worker (тогда
// звонков не будет).

import (
	"crypto/rand"
	"crypto/subtle"
	"net/url"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/crypto/curve25519"
)

// Официальные сети Telegram (core.telegram.org/resources/cidr.txt), IPv4.
var telegramCIDRs = []string{
	"91.105.192.0/23", "91.108.4.0/22", "91.108.8.0/22", "91.108.12.0/22",
	"91.108.16.0/22", "91.108.20.0/22", "91.108.56.0/22", "95.161.64.0/20",
	"149.154.160.0/20", "185.76.151.0/24",
}

const wgNet = "10.77.0." // сервер .1, устройства .2–.254

type wgPeer struct {
	Name  string `json:"name"`
	Priv  string `json:"priv"`
	Pub   string `json:"pub"`
	IP    string `json:"ip"`
	Token string `json:"token,omitempty"` // адрес профиля sing-box для этого устройства (/p/<token>)
}

func wgKeypair() (priv, pub string) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		panic(err)
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	p, _ := curve25519.X25519(k[:], curve25519.Basepoint)
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(p)
}

var wgKeyMu sync.Mutex

// wgServerKeys: ключ сервера создаётся один раз (под мьютексом: его одновременно
// спрашивают панель и запуск sing-box, иначе у них оказались бы разные ключи).
func wgServerKeys() (priv, pub string) {
	wgKeyMu.Lock()
	defer wgKeyMu.Unlock()
	priv = readTrim("wg_server.key")
	if priv == "" {
		priv, _ = wgKeypair()
		_ = writeFile("wg_server.key", priv, 0o600)
	}
	kb, _ := base64.StdEncoding.DecodeString(priv)
	p, _ := curve25519.X25519(kb, curve25519.Basepoint)
	return priv, base64.StdEncoding.EncodeToString(p)
}

func wgPeers() []wgPeer {
	var ps []wgPeer
	_ = json.Unmarshal([]byte(readTrim("wg_peers.json")), &ps)
	return ps
}

func saveWgPeers(ps []wgPeer) error {
	b, _ := json.MarshalIndent(ps, "", "  ")
	return writeFile("wg_peers.json", string(b), 0o600)
}

// wgEndpoint -- адрес, который пишется в профиль: локальный IP (тумблер как у SOCKS) или домен.
func wgEndpoint() string {
	return net.JoinHostPort(linkHost(), setting("WG_PORT"))
}

func wgClientConf(p wgPeer) string {
	_, spub := wgServerKeys()
	return fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s/32
MTU = 1280

[Peer]
PublicKey = %s
AllowedIPs = %s
Endpoint = %s
PersistentKeepalive = 25
`, p.Priv, p.IP, spub, strings.Join(telegramCIDRs, ", "), wgEndpoint())
}

// wgSingboxEndpoint -- кусок конфига sing-box; nil, если модуль выключен или нет устройств.
func wgSingboxEndpoint() map[string]any {
	if !flag("wg_enabled") || !validPort(setting("WG_PORT")) {
		return nil
	}
	ps := wgPeers()
	if len(ps) == 0 {
		return nil
	}
	priv, _ := wgServerKeys()
	peers := []map[string]any{}
	for _, p := range ps {
		peers = append(peers, map[string]any{"public_key": p.Pub, "allowed_ips": []string{p.IP + "/32"}})
	}
	port := 0
	fmt.Sscan(setting("WG_PORT"), &port)
	return map[string]any{
		"type": "wireguard", "tag": "wg-in", "system": false, "mtu": 1280,
		"address": []string{wgNet + "1/24"}, "private_key": priv, "listen_port": port, "peers": peers,
	}
}

type wgState struct {
	Enabled          bool        `json:"enabled"`
	Port             string      `json:"port,omitempty"`
	Endpoint         string      `json:"endpoint,omitempty"`
	Peers            []wgPeerOut `json:"peers"`
	Problem          string      `json:"problem,omitempty"`
	Calls            bool        `json:"calls,omitempty"`
	LocalIPAvailable bool        `json:"localIpAvailable,omitempty"`
	LocalIPActive    bool        `json:"localIpActive,omitempty"`
}

type wgPeerOut struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	Conf string `json:"conf"`
	QR   string `json:"qr"`
}

func currentWg() wgState {
	st := wgState{Enabled: flag("wg_enabled"), Port: setting("WG_PORT"), Peers: []wgPeerOut{},
		LocalIPAvailable: localIP() != "", LocalIPActive: localIP() != "" && readTrim("wg_use_local_ip") != "0"}
	if up, _ := parseSS(setting("UPSTREAM_SS")); up != nil {
		st.Calls = true
	}
	if validPort(st.Port) {
		st.Endpoint = wgEndpoint()
	}
	for _, p := range wgPeers() {
		c := wgClientConf(p)
		st.Peers = append(st.Peers, wgPeerOut{Name: p.Name, IP: p.IP, Conf: c, QR: qrDataURI(c)})
	}
	if st.Enabled {
		if len(st.Peers) == 0 {
			st.Problem = "добавьте устройство — без него туннель не поднимается"
		} else {
			st.Problem = problemOf(socksProc)
		}
	}
	return st
}

var peerNameRx = regexp.MustCompile(`^[\p{L}\p{N} ._-]{1,32}$`)

func handleWgPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Delete bool   `json:"delete"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	mu.Lock()
	defer mu.Unlock()
	ps := wgPeers()
	if req.Delete {
		out := ps[:0]
		for _, p := range ps {
			if p.Name != req.Name {
				out = append(out, p)
			}
		}
		ps = out
	} else {
		if !peerNameRx.MatchString(req.Name) {
			http.Error(w, "имя устройства: 1–32 символа, буквы, цифры, пробел . _ -", http.StatusBadRequest)
			return
		}
		for _, p := range ps {
			if p.Name == req.Name {
				http.Error(w, "такое устройство уже есть", http.StatusConflict)
				return
			}
		}
		if _, err := addWgPeer(req.Name); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, currentWg())
		return
	}
	if err := saveWgPeers(ps); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	applyModules()
	if egressWanted() {
		socksProc.Restart()
	}
	writeJSON(w, currentWg())
}

func handleWgLocalIP(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, currentWg())
}

func toggleWg(on bool, port string) error {
	if on {
		if port == "" {
			port = setting("WG_PORT")
		}
		if port == "" {
			port = "51888"
		}
		if !validPort(port) {
			return errors.New("нужен корректный порт")
		}
		if setting("WG_PORT") != port {
			if l, err := net.ListenPacket("udp", ":"+port); err != nil {
				return errors.New("UDP-порт " + port + " уже занят")
			} else {
				l.Close()
			}
		}
		if err := writeSettings(map[string]string{"WG_PORT": port}); err != nil {
			return err
		}
	}
	setFlag("wg_enabled", on)
	return nil
}

// addWgPeer создаёт устройство и перезапускает sing-box. Вызывать под mu.
func addWgPeer(name string) (wgPeer, error) {
	ps := wgPeers()
	used := map[string]bool{}
	for _, p := range ps {
		used[p.IP] = true
	}
	ip := ""
	for i := 2; i < 255; i++ {
		if c := fmt.Sprintf("%s%d", wgNet, i); !used[c] {
			ip = c
			break
		}
	}
	if ip == "" {
		return wgPeer{}, errors.New("нет свободных адресов")
	}
	priv, pub := wgKeypair()
	p := wgPeer{Name: name, Priv: priv, Pub: pub, IP: ip, Token: randHex(16)}
	if err := saveWgPeers(append(ps, p)); err != nil {
		return wgPeer{}, err
	}
	applyModules()
	if egressWanted() {
		socksProc.Restart()
	}
	return p, nil
}

// singboxProfile -- профиль для приложения sing-box (SFA): TUN только на сети
// Telegram (route_address), внутри -- тот же WireGuard до панели. Приложение
// берёт его по ссылке /p/<token> и само обновляет.
func singboxProfile(p wgPeer) ([]byte, error) {
	_, spub := wgServerKeys()
	host, port, err := net.SplitHostPort(wgEndpoint())
	if err != nil {
		return nil, err
	}
	pn := 0
	fmt.Sscan(port, &pn)
	conf := map[string]any{
		"log": map[string]any{"level": "warn"},
		"inbounds": []map[string]any{{
			"type": "tun", "tag": "tun-in", "address": []string{"172.19.0.1/30"}, "mtu": 1280,
			"auto_route": true, "strict_route": false, "route_address": telegramCIDRs,
		}},
		"endpoints": []map[string]any{{
			"type": "wireguard", "tag": "wg", "system": false, "mtu": 1280,
			"address": []string{p.IP + "/32"}, "private_key": p.Priv,
			"peers": []map[string]any{{
				"address": host, "port": pn, "public_key": spub,
				"allowed_ips": []string{"0.0.0.0/0"}, "persistent_keepalive_interval": 25,
			}},
		}},
		// Android шлёт все DNS-запросы телефона на адрес внутри TUN: отвечаем на них
		// здесь же, через обычную сеть телефона, иначе DNS уйдёт в туннель и пропадёт.
		// DNS телефона -- на панель внутри туннеля (10.77.0.1), панель отвечает через
		// зашифрованный DoH: провайдер запросы не видит и подменить не может.
		"dns": map[string]any{
			"servers": []map[string]any{{"type": "udp", "tag": "panel", "server": wgNet + "1", "detour": "wg"}},
			"final":   "panel",
		},
		"route": map[string]any{
			"rules": []map[string]any{
				{"action": "sniff"},
				{"protocol": "dns", "action": "hijack-dns"},
				{"ip_cidr": telegramCIDRs, "outbound": "wg"},
			},
			"final":                   "direct",
			"auto_detect_interface":   true,
			"default_domain_resolver": "panel",
		},
		"outbounds": []map[string]any{{"type": "direct", "tag": "direct"}},
	}
	return json.MarshalIndent(conf, "", "  ")
}

// peerByToken находит устройство по токену профиля.
func peerByToken(tok string) (wgPeer, bool) {
	if len(tok) < 16 {
		return wgPeer{}, false
	}
	for _, p := range wgPeers() {
		if p.Token != "" && subtle.ConstantTimeCompare([]byte(p.Token), []byte(tok)) == 1 {
			return p, true
		}
	}
	return wgPeer{}, false
}

// ensurePeerTokens: у устройств, созданных до появления профилей sing-box, токена нет.
func ensurePeerTokens() {
	ps := wgPeers()
	changed := false
	for i := range ps {
		if ps[i].Token == "" {
			ps[i].Token = randHex(16)
			changed = true
		}
	}
	if changed {
		_ = saveWgPeers(ps)
	}
}

func sfaImportURL(base string, p wgPeer) string {
	return "sing-box://import-remote-profile?url=" + url.QueryEscape(base+"/p/"+p.Token) + "#" + url.PathEscape("Telegram "+p.Name)
}
