package main

// Свой SOCKS5-сервер на внешнем порту модуля socks5.
//
// Почему не sing-box напрямую: для звонков (UDP ASSOCIATE) SOCKS-сервер
// говорит клиенту, куда слать UDP, а sing-box отвечает локальным адресом
// хоста и случайным портом. Телефон из мобильной сети по нему не достучится,
// а случайный порт не пробросить на роутере. Здесь UDP принимается на том же
// номере порта, что и TCP, а внешним клиентам называется внешний адрес
// (домен HOSTNAME_ARG или PUBLIC_IP).
//
// Выход -- через sing-box на 127.0.0.1 (без логина): он уже решает, вести
// поток через Shadowsocks-выход или Cloudflare Worker. UDP-пакеты клиента
// пересылаются в UDP-ассоциацию sing-box как есть: формат заголовка SOCKS5
// UDP у обеих сторон одинаковый.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

var (
	singboxAddr = envDefault("SINGBOX_ADDR", "127.0.0.1:41081")
	logConnects = envDefault("SOCKS_LOG", "1") == "1"
)

type socksFront struct {
	mu   sync.Mutex
	port string
	user string
	pass string
	tcp  net.Listener
	udp  *net.UDPConn

	sessMu   sync.Mutex
	sessions map[*udpSession]struct{}
	lastErr  string
}

type udpSession struct {
	clientIP   net.IP
	clientAddr *net.UDPAddr // узнаём по первому пакету
	upCtl      net.Conn     // TCP-управление ассоциацией у sing-box
	up         *net.UDPConn // UDP к sing-box
	upRelay    *net.UDPAddr
	lastSeen   time.Time
}

var front = &socksFront{sessions: map[*udpSession]struct{}{}}

func (s *socksFront) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcp != nil
}

func (s *socksFront) Err() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Start (пере)запускает сервер с текущими настройками.
func (s *socksFront) Start() {
	s.Stop()
	port, user, pass := setting("SOCKS5_PORT"), setting("SOCKS5_USER"), readTrim("socks5_pass.hex")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validPort(port) || user == "" || pass == "" {
		s.lastErr = "SOCKS5 не настроен (порт/логин/пароль)"
		return
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		s.lastErr = "TCP " + port + ": " + err.Error()
		return
	}
	pn, _ := strconv.Atoi(port)
	uc, err := net.ListenUDP("udp", &net.UDPAddr{Port: pn})
	if err != nil {
		ln.Close()
		s.lastErr = "UDP " + port + ": " + err.Error()
		return
	}
	s.port, s.user, s.pass, s.tcp, s.udp, s.lastErr = port, user, pass, ln, uc, ""
	go s.acceptLoop(ln)
	go s.udpLoop(uc)
	log.Printf("[socks5] слушаю TCP и UDP :%s", port)
}

func (s *socksFront) Stop() {
	s.mu.Lock()
	ln, uc := s.tcp, s.udp
	s.tcp, s.udp = nil, nil
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	if uc != nil {
		uc.Close()
	}
	s.sessMu.Lock()
	for ss := range s.sessions {
		ss.close()
		delete(s.sessions, ss)
	}
	s.sessMu.Unlock()
}

func (ss *udpSession) close() {
	if ss.upCtl != nil {
		ss.upCtl.Close()
	}
	if ss.up != nil {
		ss.up.Close()
	}
}

func (s *socksFront) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func readN(c net.Conn, n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(c, b)
	return b, err
}

// readAddr читает ATYP+адрес+порт (как в запросе SOCKS5) и возвращает их в сыром виде и как host:port.
func readAddr(c net.Conn) ([]byte, string, error) {
	t, err := readN(c, 1)
	if err != nil {
		return nil, "", err
	}
	var host string
	raw := []byte{t[0]}
	switch t[0] {
	case 1:
		b, err := readN(c, 4)
		if err != nil {
			return nil, "", err
		}
		raw, host = append(raw, b...), net.IP(b).String()
	case 4:
		b, err := readN(c, 16)
		if err != nil {
			return nil, "", err
		}
		raw, host = append(raw, b...), net.IP(b).String()
	case 3:
		l, err := readN(c, 1)
		if err != nil {
			return nil, "", err
		}
		b, err := readN(c, int(l[0]))
		if err != nil {
			return nil, "", err
		}
		raw, host = append(append(raw, l[0]), b...), string(b)
	default:
		return nil, "", errors.New("bad atyp")
	}
	p, err := readN(c, 2)
	if err != nil {
		return nil, "", err
	}
	raw = append(raw, p...)
	return raw, net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(p)))), nil
}

func reply(c net.Conn, code byte, ip net.IP, port int) {
	b := []byte{5, code, 0}
	if v4 := ip.To4(); v4 != nil {
		b = append(append(b, 1), v4...)
	} else if ip != nil {
		b = append(append(b, 4), ip.To16()...)
	} else {
		b = append(b, 1, 0, 0, 0, 0)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(port))
	_, _ = c.Write(b)
}

// upstreamHandshake: SOCKS5 без логина к sing-box, отправка запроса cmd на адрес raw; возвращает BND из ответа.
func upstreamHandshake(cmd byte, raw []byte) (net.Conn, *net.UDPAddr, error) {
	u, err := net.DialTimeout("tcp", singboxAddr, 5*time.Second)
	if err != nil {
		return nil, nil, err
	}
	_ = u.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err = u.Write([]byte{5, 1, 0}); err == nil {
		var m []byte
		if m, err = readN(u, 2); err == nil && m[1] != 0 {
			err = errors.New("sing-box: метод не принят")
		}
	}
	if err == nil {
		_, err = u.Write(append([]byte{5, cmd, 0}, raw...))
	}
	var bnd *net.UDPAddr
	if err == nil {
		var h []byte
		if h, err = readN(u, 3); err == nil {
			if h[1] != 0 {
				err = fmt.Errorf("sing-box: код %d", h[1])
			} else {
				var a string
				if _, a, err = readAddr(u); err == nil {
					bnd, _ = net.ResolveUDPAddr("udp", a)
				}
			}
		}
	}
	if err != nil {
		u.Close()
		return nil, nil, err
	}
	_ = u.SetDeadline(time.Time{})
	return u, bnd, nil
}

func (s *socksFront) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	h, err := readN(c, 2)
	if err != nil || h[0] != 5 {
		return
	}
	methods, err := readN(c, int(h[1]))
	if err != nil || !bytes.Contains(methods, []byte{2}) {
		_, _ = c.Write([]byte{5, 0xff})
		return
	}
	_, _ = c.Write([]byte{5, 2})
	a, err := readN(c, 2)
	if err != nil || a[0] != 1 {
		return
	}
	un, _ := readN(c, int(a[1]))
	pl, _ := readN(c, 1)
	if len(pl) == 0 {
		return
	}
	pw, _ := readN(c, int(pl[0]))
	s.mu.Lock()
	ok := string(un) == s.user && string(pw) == s.pass
	s.mu.Unlock()
	if !ok {
		time.Sleep(500 * time.Millisecond)
		_, _ = c.Write([]byte{1, 1})
		return
	}
	_, _ = c.Write([]byte{1, 0})

	req, err := readN(c, 3)
	if err != nil || req[0] != 5 {
		return
	}
	raw, dst, err := readAddr(c)
	if err != nil {
		return
	}
	cl := c.RemoteAddr().String()
	switch req[1] {
	case 1: // CONNECT
		u, _, err := upstreamHandshake(1, raw)
		if err != nil {
			reply(c, 5, nil, 0)
			return
		}
		defer u.Close()
		reply(c, 0, nil, 0)
		_ = c.SetDeadline(time.Time{})
		t0 := time.Now()
		var upB int64
		done := make(chan struct{})
		go func() { upB, _ = io.Copy(u, c); u.Close(); close(done) }()
		downB, _ := io.Copy(c, u)
		<-done
		if logConnects {
			log.Printf("[socks5] TCP %s -> %s: %s, ↑%d ↓%d байт", cl, dst, time.Since(t0).Round(time.Second), upB, downB)
		}
	case 3: // UDP ASSOCIATE
		s.associate(c)
	default:
		reply(c, 7, nil, 0)
	}
}

// publicIP -- адрес, который называем клиентам не из локальной сети.
var (
	pubMu sync.Mutex
	pubIP net.IP
	pubAt time.Time
)

func publicIP() net.IP {
	pubMu.Lock()
	defer pubMu.Unlock()
	if v := setting("PUBLIC_IP"); v != "" {
		return net.ParseIP(v)
	}
	if pubIP != nil && time.Since(pubAt) < 10*time.Minute {
		return pubIP
	}
	if ips, err := net.LookupIP(hostname); err == nil {
		for _, ip := range ips {
			if ip.To4() != nil {
				pubIP, pubAt = ip, time.Now()
				break
			}
		}
	}
	return pubIP
}

func (s *socksFront) associate(c net.Conn) {
	cip := c.RemoteAddr().(*net.TCPAddr).IP
	upCtl, bnd, err := upstreamHandshake(3, []byte{1, 0, 0, 0, 0, 0, 0})
	if err != nil {
		reply(c, 1, nil, 0)
		return
	}
	if bnd.IP.IsUnspecified() {
		bnd.IP = net.IPv4(127, 0, 0, 1)
	}
	up, err := net.DialUDP("udp", nil, bnd)
	if err != nil {
		upCtl.Close()
		reply(c, 1, nil, 0)
		return
	}
	ss := &udpSession{clientIP: cip, upCtl: upCtl, up: up, upRelay: bnd, lastSeen: time.Now()}
	s.sessMu.Lock()
	s.sessions[ss] = struct{}{}
	s.sessMu.Unlock()

	// куда слать UDP: локальным -- адрес, на который они подключились; остальным -- внешний
	ip := c.LocalAddr().(*net.TCPAddr).IP
	if v4 := cip.To4(); v4 != nil && !(cip.IsPrivate() || cip.IsLoopback()) {
		if p := publicIP(); p != nil {
			ip = p
		}
	}
	pn, _ := strconv.Atoi(s.port)
	log.Printf("[socks5] звонок/UDP: ассоциация для %s, UDP на %s:%d", cip, ip, pn)
	reply(c, 0, ip, pn)
	_ = c.SetDeadline(time.Time{})

	// ответы sing-box -> клиенту
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := up.Read(buf)
			if err != nil {
				return
			}
			s.sessMu.Lock()
			dst := ss.clientAddr
			s.sessMu.Unlock()
			s.mu.Lock()
			uc := s.udp
			s.mu.Unlock()
			if dst != nil && uc != nil {
				_, _ = uc.WriteToUDP(buf[:n], dst)
			}
		}
	}()
	// ассоциация живёт, пока открыто TCP-соединение клиента
	_, _ = io.Copy(io.Discard, c)
	s.sessMu.Lock()
	delete(s.sessions, ss)
	s.sessMu.Unlock()
	ss.close()
}

// udpLoop: пакеты клиентов -> их ассоциация у sing-box.
func (s *socksFront) udpLoop(uc *net.UDPConn) {
	buf := make([]byte, 65535)
	for {
		n, from, err := uc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 4 || buf[2] != 0 { // фрагменты не поддерживаем
			continue
		}
		s.sessMu.Lock()
		var match, free *udpSession
		for ss := range s.sessions {
			if !ss.clientIP.Equal(from.IP) {
				continue
			}
			if ss.clientAddr != nil && ss.clientAddr.Port == from.Port {
				match = ss
				break
			}
			if ss.clientAddr == nil && (free == nil || ss.lastSeen.After(free.lastSeen)) {
				free = ss
			}
		}
		if match == nil && free != nil {
			free.clientAddr = &net.UDPAddr{IP: from.IP, Port: from.Port}
			match = free
			log.Printf("[socks5] UDP пошёл от %s", from)
		}
		if match == nil {
			log.Printf("[socks5] UDP от %s без ассоциации -- отброшен", from)
		}
		if match != nil {
			match.lastSeen = time.Now()
		}
		s.sessMu.Unlock()
		if match != nil {
			_, _ = match.up.Write(buf[:n])
		}
	}
}
