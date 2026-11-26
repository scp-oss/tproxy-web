// cover -- замена nginx-контейнера cover. Три задачи в одном бинарнике:
//
//  1. Форвардер 0.0.0.0:19443 -> 127.0.0.1:18080 (relay). network_mode:
//     "service:tgws" -- не наш выбор, а жёсткое требование самого relay
//     (tproxy-server): "listen", "public application" и КАЖДЫЙ
//     backend-адрес должны быть числовым loopback-адресом (официальный
//     README tproxy-server), значит relay и всё, что его вызывает, обязаны
//     быть в одном network namespace. caddy ТОЖЕ в этой группе (пробовали
//     вынести отдельным сервисом с DNS по имени владельца, tgws:19443 --
//     упёрлись в ограничение Docker: имя сервиса-владельца network_mode:
//     service:X группы не резолвится через embedded DNS других
//     контейнеров, даже с корректным IP на сети) -- достаёт сюда тем же
//     127.0.0.1:19443. На этом же порту ещё и путь мини-панели
//     (ADMIN_PATH), отличаем по URL.
//  2. Сайт-прикрытие на 127.0.0.1:13000 -- relay (тот же namespace)
//     обращается сюда как internal-клиент, обычный loopback.
//  3. Прокси /ADMIN_PATH/api/ -> panel-admin (в сети этого compose-проекта
//     по имени сервиса -- panel-admin НЕ в группе общего namespace, ему
//     loopback не требуется), с заголовком X-Panel-Token.
//
// ВАЖНО про X-Forwarded-For: relay принимает РОВНО ОДИН адрес и отвергает
// список (уже наступали на это с nginx -- $proxy_add_x_forwarded_for
// добавляет к уже существующему значению, а не заменяет его). ProxyRequest.
// SetXForwarded() в net/http/httputil ЗАМЕНЯЕТ, а не добавляет -- то, что
// нужно, но за этим стоит явно следить при любых будущих правках.
//
// ADMIN_PATH/PANEL_TOKEN: panel-admin сам генерирует их при первом старте
// (см. panel-admin/main.go bootstrap()) -- без init.sh, который раньше
// клал их в .env ДО того, как сервисы вообще стартуют. compose-интерполяция
// ${VAR} фиксируется при разборе compose-файла, до того как panel-admin
// успевает что-либо сгенерировать -- значит эти переменные тут могут
// прийти пустыми. envOrFile ждёт и читает тот же файл из смонтированного
// именованного volume config_data (туда же их пишет panel-admin).
package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
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

// envOrFile: если переменная окружения задана -- берём её; иначе ждём (с
// таймаутом) появления файла path и читаем значение оттуда. Файл пишет
// panel-admin при bootstrap() -- порядок гарантируется ещё и на уровне
// compose (depends_on: panel-admin: condition: service_healthy), это —
// защита на случай, если healthcheck почему-то не сработал.
func envOrFile(envKey, path string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	log.Fatalf("не удалось получить значение: ни %s, ни %s не доступны за 20с", envKey, path)
	return ""
}

// decoyHandler отдаёт статику с кастомной 404-страницей (аналог nginx
// try_files $uri $uri/ =404; error_page 404 /404.html;). Перехватываем
// попытку http.FileServer отдать свой стандартный "404 page not found" --
// ни заголовок, ни тело в реальный ResponseWriter не уходят, пока не
// убедимся, что статус действительно 404, и тогда подменяем целиком.
// readFileTrim: НЕ фатальная, в отличие от envOrFile -- вызывается на
// каждый запрос к странице панели, временная ошибка чтения не должна
// ронять весь процесс, просто отдаём пустую строку (шаблон корректно
// покажет пустую ссылку/без QR-картинки на этот конкретный запрос).
func readFileTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func decoyHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &statusCatcher{ResponseWriter: w}
		fs.ServeHTTP(rw, r)
		if rw.status == http.StatusNotFound {
			// http.ServeFile тут не подходит: он сам ставит 200 для найденного
			// файла, а статус ответа должен остаться 404 (как nginx
			// error_page 404 /404.html по умолчанию) -- просто другое тело.
			body, err := os.ReadFile(dir + "/404.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			if err == nil {
				_, _ = w.Write(body)
			}
		}
	})
}

type statusCatcher struct {
	http.ResponseWriter
	status int
}

func (s *statusCatcher) WriteHeader(code int) {
	s.status = code
	if code != http.StatusNotFound {
		s.ResponseWriter.WriteHeader(code)
	}
	// для 404 ничего не пишем в реальный ResponseWriter -- заголовок/тело
	// подменяются целиком снаружи, после того как fs.ServeHTTP вернёт управление
}

func (s *statusCatcher) Write(b []byte) (int, error) {
	if s.status == http.StatusNotFound {
		return len(b), nil // отбрасываем тело стандартного "404 page not found"
	}
	return s.ResponseWriter.Write(b)
}

// newReverseProxy строит ReverseProxy через ПОЛЕ Rewrite, а не через
// NewSingleHostReverseProxy (та задаёт legacy-поле Director) -- Go не
// позволяет одновременно Director и Rewrite, падает в рантайме на каждый
// запрос с "must have exactly one of Director or Rewrite set" (поймано
// реальным тестом, не из документации).
//
// ВАЖНО про Host: ProxyRequest.SetURL переписывает исходящий Host-заголовок
// на host ЦЕЛИ (target, т.е. "127.0.0.1:18080" для relay), а не оставляет
// исходный Host клиента ("proxy.example.ru") -- задокументированное
// поведение SetURL, не баг Go. relay же сверяет bridge-capability (WEB-прокси
// handshake) именно по Host == public_hostname из config.json -- с
// переписанным Host релей НИКОГДА не узнаёт настоящий bridge-запрос клиента
// и втихую откатывается на сайт-прикрытие (счётчики в /metrics на admin_listen
// остаются нулевыми даже для настоящих попыток подключения -- так и было
// найдено: реальный клиент висел на "соединение", а relay не видел вообще
// ничего, даже отказа). Восстанавливаем исходный Host ПОСЛЕ SetURL.
func newReverseProxy(target string) *httputil.ReverseProxy {
	u := &url.URL{Scheme: "http", Host: target}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			originalHost := pr.In.Host
			pr.SetURL(u)
			pr.Out.Host = originalHost
			pr.SetXForwarded() // ЗАМЕНЯЕТ X-Forwarded-For на один актуальный IP, не добавляет к списку
		},
	}
}

// protoFields читает снэпшот состояния mtproto/socks5, который пишет
// panel-admin (currentState() -> persistProtoFiles(), см.
// panel-admin/main.go) -- тем же приёмом, что уже используется для web в
// хендлере ниже: читаем заново на каждый запрос, чтобы странице не нужно
// было узнавать правду про включён/выключен протокол уже после отдачи
// HTML через /api/state.
func protoFields(configDir, proto string) map[string]string {
	enabled, _ := readFileTrim(configDir + "/" + proto + "_enabled")
	link, _ := readFileTrim(configDir + "/" + proto + "_link.txt")
	open, _ := readFileTrim(configDir + "/" + proto + "_open.txt")
	qr, _ := readFileTrim(configDir + "/" + proto + "_qr.txt")
	sni, _ := readFileTrim(configDir + "/" + proto + "_sni.txt")
	// Только socks5 (файлов для mtproto просто не будет -- readFileTrim
	// тихо вернёт "" при отсутствии файла, не ошибку).
	localAvail, _ := readFileTrim(configDir + "/" + proto + "_localip_available")
	localActive, _ := readFileTrim(configDir + "/" + proto + "_localip_active")

	f := map[string]string{
		"SWITCH_ON":         "",
		"STATUS_CLASS":      "off",
		"STATUS_TEXT":       "выключен",
		"LINK":              "",
		"OPEN":              "#",
		"QR_SRC":            "",
		"QR_CLASS":          "",
		"ACTIONS_STYLE":     "none",
		"LOCALIP_ROW_STYLE": "none",
		"LOCALIP_SWITCH_ON": "",
	}
	if localAvail == "1" {
		f["LOCALIP_ROW_STYLE"] = "flex"
		if localActive == "1" {
			f["LOCALIP_SWITCH_ON"] = "on"
		}
	}
	if enabled == "1" && link != "" {
		f["SWITCH_ON"] = "on"
		f["STATUS_CLASS"] = "ok"
		statusText := "\u25cf активен"
		if proto == "mtproto" && sni != "" {
			statusText += ", fake-TLS под " + sni
		}
		f["STATUS_TEXT"] = statusText
		f["LINK"] = link
		if open != "" {
			f["OPEN"] = open
		}
		if qr != "" {
			f["QR_SRC"] = qr
			f["QR_CLASS"] = "show"
		}
		f["ACTIONS_STYLE"] = "flex"
	}
	return f
}

func main() {
	configDir := envDefault("CONFIG_DIR", "/config")
	adminPath := envOrFile("ADMIN_PATH", configDir+"/admin_path")
	panelToken := envOrFile("PANEL_TOKEN", configDir+"/panel_token")
	hostnameArg := mustEnv("HOSTNAME_ARG")
	// relay и cover ОБЯЗАНЫ делить network namespace (см. комментарий в
	// начале файла) -- loopback и для вызова relay, и для его собственного
	// обращения сюда за decoy. panelAddr -- исключение: panel-admin не в
	// этой группе, ему loopback не требуется, достаём по имени сервиса.
	relayAddr := envDefault("RELAY_ADDR", "127.0.0.1:18080")
	panelAddr := envDefault("PANEL_ADDR", "panel-admin:7654")
	adminDir := envDefault("ADMIN_HTML_DIR", "/site/admin")
	decoyDir := envDefault("DECOY_HTML_DIR", "/site/html")
	forwarderAddr := envDefault("FORWARDER_ADDR", "0.0.0.0:19443")
	decoyAddr := envDefault("DECOY_ADDR", "127.0.0.1:13000")
	// panelPublicAddr -- независимый HTTP-вход panel-admin (логин+пароль,
	// см. panel-admin/main.go), НЕ тот же, что panelAddr выше (:7654,
	// внутренний API с X-Panel-Token). Теперь это и есть содержимое
	// decoy'я: relay (тот же namespace) обращается на decoyAddr как
	// internal-клиент для любого запроса, не прошедшего настоящий
	// WEB-прокси handshake -- посторонний посетитель публичного домена
	// видит страницу входа NodeSentry вместо прежней статической
	// заглушки, а не что-то, прямо выдающее назначение сервера.
	panelPublicAddr := envDefault("PANEL_PUBLIC_ADDR", "panel-admin:7655")

	relayProxy := newReverseProxy(relayAddr)
	decoyProxy := newReverseProxy(panelPublicAddr)
	panelProxy := newReverseProxy(panelAddr)
	origPanelRewrite := panelProxy.Rewrite
	panelProxy.Rewrite = func(pr *httputil.ProxyRequest) {
		origPanelRewrite(pr)
		pr.Out.Header.Set("X-Panel-Token", panelToken)
	}

	// Шаблон отдаём напрямую, без отдельного шага генерации index.html
	// (раньше это делал init.sh -- bash-подстановкой __HOSTNAME__/__WEB_LINK__
	// перед первым деплоем; на Synology такого шага не будет вообще).
	// __HOSTNAME__ подставляем ОДИН раз -- не меняется за время жизни
	// процесса. __WEB_LINK__/__WEB_QR__ -- КАЖДЫЙ запрос заново (см. handler
	// ниже), не здесь: "web" никогда не выключен, но секрет можно
	// перевыпустить через панель (handleRegen в panel-admin/main.go) без
	// перезапуска cover -- кэшировать раз и навсегда означало бы показывать
	// старую ссылку после перевыпуска до следующего рестарта контейнера.
	// Раньше __WEB_LINK__ подставлялся пустым намеренно, страница тянула
	// ссылку сама через /api/state после загрузки -- при сбое этого fetch
	// (или просто до его завершения) кнопка "Открыть в Telegram" оставалась
	// с href="" и ничего не делала. Теперь сервер подставляет настоящее
	// значение сразу, JS-обновление остаётся как было -- просто больше не
	// единственный источник правды.
	adminTemplate, err := os.ReadFile(adminDir + "/index.html.template")
	if err != nil {
		log.Fatalf("не удалось прочитать %s/index.html.template: %v", adminDir, err)
	}
	adminHTMLBase := strings.ReplaceAll(string(adminTemplate), "__HOSTNAME__", hostnameArg)

	// ServeMux требует паттерны с ведущим "/" -- без него не матчит вообще
	// ничего (поймано реальным тестом: без "/" все запросы проваливались
	// в relay-обработчик по умолчанию, "/").
	adminPrefix := "/" + adminPath
	mux := http.NewServeMux()
	mux.Handle(adminPrefix+"/api/", http.StripPrefix(adminPrefix+"/api", panelProxy))
	mux.HandleFunc(adminPrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		webLink, _ := readFileTrim(configDir + "/web_link.txt")
		webOpen, _ := readFileTrim(configDir + "/web_open.txt")
		webQR, _ := readFileTrim(configDir + "/web_qr.txt")
		page := strings.ReplaceAll(adminHTMLBase, "__WEB_LINK__", webLink)
		page = strings.ReplaceAll(page, "__WEB_OPEN__", webOpen)
		page = strings.ReplaceAll(page, "__WEB_QR__", webQR)
		for _, proto := range []string{"mtproto", "socks5"} {
			prefix := "__" + strings.ToUpper(proto) + "_"
			for key, val := range protoFields(configDir, proto) {
				page = strings.ReplaceAll(page, prefix+key+"__", val)
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	mux.Handle("/", relayProxy)

	go func() {
		log.Printf("cover: форвардер+панель слушает %s -> relay %s, panel-admin %s", forwarderAddr, relayAddr, panelAddr)
		log.Fatal(http.ListenAndServe(forwarderAddr, mux))
	}()

	// Сайт-прикрытие теперь -- независимый вход panel-admin (см. комментарий
	// у panelPublicAddr выше), не статика из decoyDir. decoyDir/decoyHandler
	// оставлены в файле НЕ используемыми -- на случай отката, без удаления
	// самой функции (мёртвый код в Go не мешает сборке).
	_ = decoyDir
	log.Printf("cover: сайт-прикрытие (decoy) слушает %s -> panel-admin %s", decoyAddr, panelPublicAddr)
	log.Fatal(http.ListenAndServe(decoyAddr, decoyProxy))
}
