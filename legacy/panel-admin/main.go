// panel-admin -- маленький демон, который даёт мини-панели (site/admin)
// реально включать/выключать протоколы, менять порт/SNI и перевыпускать
// секреты -- с немедленным применением.
//
// Обычный сервис docker compose (не host-демон под systemd -- тот подход
// не переносится на Synology DSM и подобные NAS с собственным управлением
// сервисами). docker-outside-of-docker: у контейнера смонтирован только
// /var/run/docker.sock хоста (стандартный, одинаковый путь почти везде,
// включая Synology) -- и НИЧЕГО больше с хоста. Ни точного пути проекта,
// ни рабочей директории знать не нужно:
//
//   - сам compose-файл запечён В ОБРАЗ при сборке (см. Dockerfile, COPY
//     docker-compose.yml) и лежит по фиксированному внутреннему пути;
//   - имя compose-проекта тоже фиксировано явно (-p tproxy-web), не
//     зависит от того, как называется папка на хосте у Container Manager;
//   - все секреты/состояние (config/, свой .env) живут в ИМЕНОВАННОМ
//     docker volume (config_data), а не в host bind-mount -- тоже без
//     привязки к какому-либо конкретному пути на хосте.
//
// Раньше это требовало ${PROJECT_DIR} -- точного абсолютного пути проекта
// на хосте, который нужно было бы угадать/ввести в форме Container
// Manager (DooD иначе неправильно резолвит relative-пути volume'ов) --
// теперь этого параметра просто нет вообще.
//
// bootstrap(): весь первичный конфиг (секреты, admin_path, panel_token,
// profiles.json, config.json) генерируется ЗДЕСЬ, при старте контейнера,
// а не host-скриптом init.sh заранее. Причина -- Synology Container
// Manager (и подобные "только форма compose") не даёт выполнить
// произвольный .sh на хосте вообще, только собрать и запустить
// docker-compose.yml с переменными из формы. Значит единственное, что
// пользователь обязан предоставить -- это HOSTNAME_ARG/CF_WORKER (и, если
// нужен, CF_WORKER_AUTH уже готового воркера) -- всё остальное появляется
// само при первом docker compose up.
//
// Другие сервисы (tgws/cover), которым эти секреты нужны КАК переменные
// окружения (а не файлы), не могут получить их через compose-интерполяцию
// ${VAR} -- та фиксируется при разборе compose-файла, ДО того как этот
// контейнер успевает что-либо сгенерировать. Поэтому у них свой маленький
// fallback: если переменная окружения пуста -- читают (с ожиданием) файл
// из того же именованного volume (config_data), который сюда и пишет
// bootstrap(). См. tgws/Dockerfile (entrypoint) и cover/main.go (envOrFile).
//
// Сетевая топология: relay/cover/caddy ОБЯЗАНЫ делить network namespace с
// tgws (network_mode: "service:tgws"). Для relay/cover это не наш выбор,
// а жёсткое требование самого relay (tproxy-server): "listen", "public
// application" и КАЖДЫЙ backend-адрес должны быть числовым loopback-адресом
// (подтверждено официальным README tproxy-server). Для caddy это уже наш
// выбор, но вынужденный: пробовали вынести его отдельным сервисом,
// достающим до форвардера cover по DNS-имени владельца (tgws:19443) -- и
// упёрлись в отдельное ограничение самого Docker: имя сервиса-ВЛАДЕЛЬЦА
// network_mode: service:X группы не резолвится через embedded DNS других
// контейнеров, даже когда у него есть корректный IP на сети (проверено
// живьём: docker network connect даёт владельцу IP, виден в docker network
// inspect, но ни один сосед его по имени не находит). Пришлось вернуть
// caddy в общую группу тоже.
//
// Известный системный риск этой схемы: при пересборке/пересоздании
// ВЛАДЕЛЬЦА namespace (tgws) "гости" (relay, cover, caddy) остаются
// привязаны к СТАРОМУ, уже мёртвому namespace, хотя сами выглядят как "Up" -- молча
// перестают отвечать. Устранить полностью нельзя (relay не даёт слушать
// ни на чём, кроме loopback), но сведён к минимуму: TGWS_REF/TPROXY_REF
// закреплены на конкретный коммит (не "main"/"master") в docker-compose.yml,
// чтобы `--build` не пересобирал образ (и не пересоздавал контейнер)
// неожиданно просто из-за нового коммита в чужом репозитории -- тогда
// пересоздание tgws становится редким, осознанным действием, а не
// случайным побочным эффектом обычного деплоя. Если он всё же случится --
// сразу следом: `docker compose up -d --force-recreate relay cover caddy`.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	// Запечены в образ при сборке (Dockerfile), не зависят от хоста.
	composeFilePath     = "/app/docker-compose.yml"
	configTemplatePath  = "/app/config.json.template"
	configDir           = "/config" // точка монтирования именованного volume config_data
	fallbackProject     = "tproxy-web" // см. detectComposeProject()
)

var (
	listenAddr       = mustEnv("LISTEN_ADDR")
	listenAddrPublic = envDefault("LISTEN_ADDR_PUBLIC", "0.0.0.0:7655")
	envPath          = configDir + "/panel.env"
	composeProject   = fallbackProject // перезаписывается detectComposeProject() в main()

	// Заполняются bootstrap() в main(), до запуска HTTP-сервера -- не
	// mustEnv на старте, т.к. могут не быть заданы снаружи вовсе (тогда
	// генерируются сами и живут в config/ дальше).
	panelToken  string
	hostnameArg string

	// Логин+пароль независимого входа (см. листенер listenAddrPublic ниже)
	// -- генерируются bootstrap() при первом старте, живут в config_data,
	// печатаются в лог КАЖДЫЙ старт контейнера (не только первый), чтобы
	// их всегда можно было найти через `docker logs`.
	adminUser      string
	adminPassPlain string
	tokenKeyBytes  []byte // HMAC-ключ для подписи сессионных кук, тот же token.key, что и раньше

	mu sync.Mutex // сериализуем все мутации -- один оператор за раз, без гонок
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

// ---------- .env: единственный источник правды для ЭТОГО процесса, когда
// он сам перезапускает mtproto/socks5. Собственный файл в config_data,
// НЕ тот .env, что Container Manager использует при первом docker compose
// up -- ему достаточно значений из формы один раз; этот -- только для
// mtproto/socks5 (toggle-сервисы, которые НИКОГДА не поднимает сам
// Container Manager, только эта панель, см. profiles в docker-compose.yml) ----------

var envLineRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

func readEnv() (map[string]string, []string, error) {
	data, err := os.ReadFile(envPath)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	m := map[string]string{}
	for _, l := range lines {
		if mm := envLineRe.FindStringSubmatch(l); mm != nil {
			m[mm[1]] = mm[2]
		}
	}
	return m, lines, nil
}

// writeEnv обновляет только переданные ключи, остальные строки не трогает.
// Файл может ещё не существовать вовсе (первый запуск) -- тогда создаём с нуля.
func writeEnv(updates map[string]string) error {
	_, lines, err := readEnv()
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		lines = nil
	}
	seen := map[string]bool{}
	for i, l := range lines {
		if mm := envLineRe.FindStringSubmatch(l); mm != nil {
			if v, ok := updates[mm[1]]; ok {
				lines[i] = mm[1] + "=" + v
				seen[mm[1]] = true
			}
		}
	}
	for k, v := range updates {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	out := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(envPath, []byte(out), 0o600)
}

// ---------- docker compose ----------

func composeRun(args ...string) (string, error) {
	full := append([]string{"compose", "-f", composeFilePath, "-p", composeProject, "--env-file", envPath}, args...)
	cmd := exec.Command("docker", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// dockerLogin: panel-admin вызывает docker compose через СВОЙ собственный
// CLI внутри контейнера (DooD, /var/run/docker.sock смонтирован, но это
// только доступ к API демона -- credential store у client'а свой,
// отдельный от хоста). Значит логин, сделанный на самом Synology (через
// GUI Container Manager или ssh docker login), этому процессу не виден
// вообще -- первый docker compose up, сделанный вручную/через GUI,
// отработает, а последующий toggle из панели (handleToggle -- он сам
// вызывает composeRun("up"...)) упадёт с "pull access denied", если
// образ ещё не закэширован локально. DOCKERHUB_USER/DOCKERHUB_TOKEN --
// опциональные: если заданы, логинимся сами при старте, не завися от
// состояния хоста. Не fatal при ошибке -- деплой с публичными образами
// не должен требовать этого вовсе.
// detectComposeProject: Container Manager на Synology использует имя ПАПКИ
// проекта как имя compose-проекта (-p), игнорируя `name:` внутри самого
// YAML-файла -- поймано живьём (пользователь назвал проект "web-mtproto" в
// GUI, реальные контейнеры получили префикс web-mtproto-*, а не
// tproxy-web-*, как предполагал раньше жёстко зашитый composeProject).
// Нельзя предсказать это имя заранее -- оно зависит от того, как
// пользователь назовёт проект при создании. Вместо этого спрашиваем у
// самого Docker: собственный контейнер panel-admin несёт лейбл
// com.docker.compose.project, который ставит тот процесс, что его создал
// (GUI Container Manager или чистый docker compose CLI) -- он всегда
// верный, откуда бы контейнер ни был поднят. Без этого panel-admin при
// любом toggle (mtproto/socks5) создавал бы ОТДЕЛЬНЫЙ, не связанный с уже
// поднятым стеком, проект под именем fallbackProject.
func detectComposeProject() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return fallbackProject
	}
	out, err := exec.Command("docker", "inspect", hostname, "--format", `{{ index .Config.Labels "com.docker.compose.project" }}`).CombinedOutput()
	if err != nil {
		log.Printf("warn: не удалось определить имя compose-проекта по себе самому (%v), использую запасное значение %q", err, fallbackProject)
		return fallbackProject
	}
	project := strings.TrimSpace(string(out))
	if project == "" || project == "<no value>" {
		return fallbackProject
	}
	return project
}

func dockerLogin() {
	user := os.Getenv("DOCKERHUB_USER")
	token := os.Getenv("DOCKERHUB_TOKEN")
	if user == "" || token == "" {
		return
	}
	cmd := exec.Command("docker", "login", "-u", user, "--password-stdin")
	cmd.Stdin = strings.NewReader(token)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("warn: docker login (DOCKERHUB_USER=%s) не удался: %v: %s", user, err, strings.TrimSpace(string(out)))
	} else {
		log.Printf("docker login выполнен для %s", user)
	}
}

func serviceRunning(service string) bool {
	out, err := composeRun("ps", "--format", "{{.Service}} {{.State}}", service)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, service+" ") && strings.Contains(l, "running") {
			return true
		}
	}
	return false
}

func allRunning(services []string) bool {
	for _, s := range services {
		if !serviceRunning(s) {
			return false
		}
	}
	return true
}

// waitFor опрашивает состояние сервиса, пока оно не совпадёт с want или не истечёт таймаут.
func waitFor(service string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if serviceRunning(service) == want {
			return true
		}
		time.Sleep(700 * time.Millisecond)
	}
	return serviceRunning(service) == want
}

func waitForAll(services []string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if allRunning(services) == want {
			return true
		}
		time.Sleep(700 * time.Millisecond)
	}
	return allRunning(services) == want
}

// composeServices -- какие compose-сервисы отвечают за протокол. web -- это
// просто relay (tgws/cover -- общая база, не toggle-able); socks5/mtproto --
// отдельные сервисы с profiles: ["optional"] (см. docker-compose.yml) --
// простой `docker compose up` их не трогает, только явное имя сервиса (как
// здесь).
func composeServices(proto string) []string {
	switch proto {
	case "web":
		return []string{"relay"}
	case "mtproto":
		return []string{"mtproto"}
	case "socks5":
		return []string{"socks5"}
	}
	return nil
}

// ---------- секреты/ссылки ----------

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return b
}

func randHex(n int) string {
	return hex.EncodeToString(randBytes(n))
}

// randBase32Lower -- как init.sh: `head -c n /dev/urandom | base32 | tr A-Z a-z`,
// без паддинга (init.sh для admin_path отдельно делал tr -d '=', для
// base_path паддинг просто не возникал при n=10 -- тут то же самое для
// любого n за счёт NoPadding, оба случая покрыты одинаково).
func randBase32Lower(n int) string {
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randBytes(n))
	return strings.ToLower(s)
}

func writeSecretFile(name, value string) error {
	return os.WriteFile(configDir+"/"+name, []byte(value), 0o600)
}

// writeSecretAndEnv пишет секрет И в свой файл (используется для построения
// ссылки), И в свой .env под тем же значением (его читает docker compose
// при создании контейнера mtproto/socks5) -- рассинхронизация этих двух
// мест была реальным багом: после regen файл обновлялся, а .env -- нет, и
// контейнер продолжал принимать клиентов со СТАРЫМ секретом, хотя панель
// уже показывала новый.
func writeSecretAndEnv(fileName, envKey, value string) error {
	if err := writeSecretFile(fileName, value); err != nil {
		return err
	}
	return writeEnv(map[string]string{envKey: value})
}

func readFileTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ensureFileRaw читает существующий файл (если непустой) или генерирует
// новое значение через gen(), пишет его (с 0o600, с собственным секретом
// ничего более открытого быть не должно) и возвращает. Идемпотентно --
// повторный запуск контейнера не меняет уже сгенерированные значения.
func ensureFileRaw(path string, gen func() string) string {
	if v, err := readFileTrim(path); err == nil && v != "" {
		return v
	}
	v := gen()
	if err := os.WriteFile(path, []byte(v+"\n"), 0o600); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
	return v
}

// ensureFromEnvOrFile -- как ensureFileRaw, но сначала смотрит переменную
// окружения envKey: если пользователь её ЯВНО задал (например, чтобы
// переиспользовать уже настроенный Cloudflare Worker между несколькими
// серверами -- тот же Worker, тот же секрет AUTH), используем её и
// синхронизируем в файл, а не генерируем свою. Без этого bootstrap() на
// каждом НОВОМ сервере сам придумывал бы новый CF_WORKER_AUTH, который не
// совпадёт с уже настроенным в самом Worker'е -- найдено живьём именно так
// (SOCKS5/MTProto падали на "соединение", пока вручную не поправили файл).
func ensureFromEnvOrFile(envKey, path string, gen func() string) string {
	if v := os.Getenv(envKey); v != "" {
		if cur, err := readFileTrim(path); err != nil || cur != v {
			_ = os.WriteFile(path, []byte(v+"\n"), 0o600)
		}
		return v
	}
	return ensureFileRaw(path, gen)
}

func domainHex(domain string) string {
	return hex.EncodeToString([]byte(domain))
}

func mtprotoLink(secret, domain, port string) string {
	full := "ee" + secret + domainHex(domain)
	return fmt.Sprintf("tg://proxy?server=%s&port=%s&secret=%s", hostnameArg, port, full)
}

// socks5LocalIPPrefPath -- выбор пользователя (тумблер в панели), какой
// адрес класть в ссылку/QR SOCKS: локальный IP или публичный домен. Имеет
// смысл, только если локальный IP вообще известен (см. effectiveLocalIP());
// хранится отдельно от самого IP, чтобы можно было переключаться туда-обратно
// без правки .env и рестарта.
const socks5LocalIPPrefFile = "socks5_use_local_ip"

var (
	autoLocalIPMu     sync.Mutex
	autoLocalIPCached string
	autoLocalIPTried  bool
)

// autoDetectLocalIP -- LAN IP хоста, определённый без ручного LOCAL_IP в
// .env. Сам panel-admin не на network_mode: host (как и никто в этом
// стеке), поэтому собственные интерфейсы контейнера -- это Docker bridge,
// не адрес хоста в локальной сети. Но docker.sock уже смонтирован (нужен
// для тумблеров/compose), и через него можно на секунду поднять
// вспомогательный контейнер С network_mode: host -- он уже видит реальные
// интерфейсы хоста. `ip route get` к любому внешнему адресу возвращает
// src-IP того интерфейса, с которого хост обычно уходит в сеть -- это и
// есть LAN IP (работает и за NAT домашнего роутера, т.к. это именно
// исходящий локальный адрес, а не адрес назначения). Результат кэшируется
// на всё время жизни процесса -- LAN IP у сервера не меняется на лету, а
// повторный docker run на каждый запрос состояния был бы лишней задержкой.
func autoDetectLocalIP() string {
	autoLocalIPMu.Lock()
	defer autoLocalIPMu.Unlock()
	if autoLocalIPTried {
		return autoLocalIPCached
	}
	autoLocalIPTried = true
	out, err := exec.Command("docker", "run", "--rm", "--network", "host", "busybox",
		"sh", "-c", "ip -4 route get 1.1.1.1 2>/dev/null").CombinedOutput()
	if err != nil {
		log.Printf("panel-admin: auto-detect local IP failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "src" && i+1 < len(fields) && isPrivateIPv4(fields[i+1]) {
			autoLocalIPCached = fields[i+1]
			break
		}
	}
	return autoLocalIPCached
}

// isPrivateIPv4 -- защита от того, чтобы случайно не подставить в ссылку
// что-то кроме обычного адреса локальной сети (на случай неожиданного
// вывода `ip route get` в нетипичной сетевой конфигурации).
func isPrivateIPv4(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return false
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// effectiveLocalIP -- LOCAL_IP из .env, если задан явно (приоритет --
// пользователь мог прописать его осознанно, например если авто-определение
// однажды ошибётся в нетипичной сети), иначе результат autoDetectLocalIP().
func effectiveLocalIP() string {
	if v := os.Getenv("LOCAL_IP"); v != "" {
		return v
	}
	return autoDetectLocalIP()
}

// socks5LocalIPAvailable -- известен ли вообще локальный IP (вручную или
// автоматически) -- без него тумблеру нечего переключать, см. socks5Server().
func socks5LocalIPAvailable() bool {
	return effectiveLocalIP() != ""
}

// socks5LocalIPActive -- текущее состояние тумблера. По умолчанию (файла
// предпочтения ещё нет) включено, если локальный IP известен -- сохраняет
// прежнее поведение (раньше локальный IP использовался безусловно, если
// задан) для тех, кто уже это настроил через LOCAL_IP.
func socks5LocalIPActive() bool {
	if !socks5LocalIPAvailable() {
		return false
	}
	v, err := readFileTrim(configDir + "/" + socks5LocalIPPrefFile)
	if err != nil {
		return true // файла нет -- дефолт "включено"
	}
	return v == "1"
}

// socks5Server -- хост в SOCKS5-ссылке. SOCKS5 у нас теперь туннель для
// локальной сети (см. пометку в панели), а не способ обхода блокировок
// Telegram -- значит в ссылке зачастую полезнее локальный IP, а не
// публичный домен, но это не всегда так (например, если подключаются не
// из той же сети) -- поэтому выбор вынесен тумблером в панель, а не жёстко
// зашит. Адрес -- явный LOCAL_IP из .env либо автоопределённый (см.
// effectiveLocalIP()); без того и другого используем hostnameArg, тумблер
// в этом случае скрыт в панели.
func socks5Server() string {
	if socks5LocalIPActive() {
		return effectiveLocalIP()
	}
	return hostnameArg
}

// Формат https://t.me/socks?server=...&port=...&user=...&pass=... -- тот
// же вид ссылки, что Telegram сам показывает/принимает для SOCKS5 (не
// socks5://, который клиенты Telegram не распознают как ссылку для
// автонастройки прокси). url.Values сам экранирует спецсимволы в
// пароле/логине, если они там окажутся (обычно это randHex, без них, но
// пользователь может задать свой логин/пароль руками через панель).
func socks5Link(user, pass, port string) string {
	q := url.Values{}
	q.Set("server", socks5Server())
	q.Set("port", port)
	q.Set("user", user)
	q.Set("pass", pass)
	return "https://t.me/socks?" + q.Encode()
}

// socks5LinkApp: tg://-вариант для кнопки "Открыть в Telegram", тем же
// принципом, что webLinkApp() выше -- t.me/socks устоявшийся, давно
// существующий путь (в отличие от нового webproxy), так что
// https://t.me/socks вероятно и так открывает приложение корректно, но
// tg:// надёжнее в любом случае (не зависит от того, как именно t.me его
// сейчас маршрутизирует) и ничего не стоит сделать единообразно.
func socks5LinkApp(user, pass, port string) string {
	link := socks5Link(user, pass, port)
	if link == "" {
		return ""
	}
	return strings.Replace(link, "https://t.me/socks?", "tg://socks?", 1)
}

// bootstrap генерирует весь первичный конфиг в config_data (именованный
// volume, общий с tgws/relay/cover), если его там ещё нет -- см. комментарий
// в начале файла. Повторяет логику init.sh (секрет, base_path, admin_path,
// panel_token, token.key, profiles.json, config.json), но исполняется
// внутри контейнера при каждом старте, а не однажды host-скриптом.
// Возвращает admin_path и panel_token -- их, в отличие от остальных файлов,
// читает сам этот процесс (handleState и auth()), остальное читают/ждут
// другие сервисы напрямую из config_data.
func bootstrap() (adminPath, panelTok string) {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", configDir, err)
	}

	cfWorker := os.Getenv("CF_WORKER")

	// web-секрет: общий для relay-профиля (без dd) и для клиентской
	// WEB-ссылки (webLink() ниже сам берёт секрет как есть). При CF_WORKER
	// используем dd-префикс -- маркер "это секрет relay-профиля", как и в
	// init.sh. tgws читает производный tgws_secret (bare, без dd) из файла
	// сам -- см. его entrypoint в tgws/Dockerfile.
	secret := ensureFileRaw(configDir+"/secret.hex", func() string {
		s := randHex(16)
		if cfWorker != "" {
			s = "dd" + s
		}
		return s
	})
	bareSecret := strings.TrimPrefix(secret, "dd")
	// 0644, не 0600 -- этот файл читает ДРУГОЙ контейнер (tgws, свой UID,
	// см. entrypoint в tgws/Dockerfile), а не только этот процесс.
	if err := os.WriteFile(configDir+"/tgws_secret", []byte(bareSecret+"\n"), 0o644); err != nil {
		log.Printf("warn: write tgws_secret: %v", err)
	}

	if cfWorker != "" {
		workerAuth := ensureFromEnvOrFile("CF_WORKER_AUTH", configDir+"/worker_auth", func() string {
			return base64URLNoPad(randBytes(24))
		})
		_ = os.Chmod(configDir+"/worker_auth", 0o644) // тоже читает tgws, см. выше
		// mtproto/socks5 читают CF_WORKER_AUTH из СВОЕГО .env напрямую (у них
		// нет file-wait, как у tgws/cover) -- значит, к моменту включения
		// любого из них через панель, этот .env должен его уже иметь.
		if err := writeEnv(map[string]string{"CF_WORKER_AUTH": workerAuth}); err != nil {
			log.Printf("warn: writeEnv CF_WORKER_AUTH: %v", err)
		}
		log.Printf("CF_WORKER_AUTH (вставьте тем же значением в секрет AUTH вашего Cloudflare Worker, если ещё не совпадает): %s", workerAuth)
	}

	basePath := ensureFileRaw(configDir+"/base_path", func() string {
		return randBase32Lower(10)
	})

	// web_link.txt/web_qr.txt: cover (см. cover/main.go) читает их ПРЯМО,
	// чтобы вшить настоящую ссылку/QR в HTML уже на сервере, не полагаясь
	// на JS-запрос к /api/state после загрузки страницы -- без этого
	// кнопка "Открыть в Telegram" первое время (а при сбое fetch -- вообще
	// всегда) оставалась с href="", т.к. __WEB_LINK__ в шаблоне раньше
	// подставлялся пустым намеренно. "web" (в отличие от mtproto/socks5)
	// никогда не выключен -- его можно и нужно знать уже на этапе рендера.
	webLinkForCover := webLink(secret, basePath)
	if err := os.WriteFile(configDir+"/web_link.txt", []byte(webLinkForCover+"\n"), 0o644); err != nil {
		log.Printf("warn: write web_link.txt: %v", err)
	}
	if err := os.WriteFile(configDir+"/web_open.txt", []byte(webLinkApp(secret, basePath)+"\n"), 0o644); err != nil {
		log.Printf("warn: write web_open.txt: %v", err)
	}
	if err := os.WriteFile(configDir+"/web_qr.txt", []byte(qrDataURI(webLinkForCover)+"\n"), 0o644); err != nil {
		log.Printf("warn: write web_qr.txt: %v", err)
	}

	// ADMIN_PATH можно задать явно в .env (например, запомнить короткий
	// путь вместо случайного) -- ensureFromEnvOrFile уважает заданное
	// значение и синхронизирует его в файл, та же логика, что у
	// CF_WORKER_AUTH/workerAuth выше.
	adminPath = ensureFromEnvOrFile("ADMIN_PATH", configDir+"/admin_path", func() string {
		return "panel-" + randBase32Lower(32)
	})
	panelTok = ensureFileRaw(configDir+"/panel_token", func() string {
		return randHex(32)
	})
	// cover (gcr.io/distroless/static-debian12:nonroot, UID 65532 -- тот же,
	// что у relay) читает эти два файла напрямую, см. cover/main.go envOrFile.
	_ = os.Chown(configDir+"/admin_path", 65532, 65532)
	_ = os.Chown(configDir+"/panel_token", 65532, 65532)

	tokenKeyPath := configDir + "/token.key"
	if _, err := os.Stat(tokenKeyPath); os.IsNotExist(err) {
		if err := os.WriteFile(tokenKeyPath, randBytes(32), 0o600); err != nil {
			log.Fatalf("write token.key: %v", err)
		}
	}

	// profiles.json -- то, что реально матчит relay на клиентский секрет.
	// backend строго "127.0.0.1:2398" -- relay требует числовой loopback
	// для КАЖДОГО backend-адреса (см. комментарий в начале файла), tgws
	// слушает на этом порту в ТОМ ЖЕ network namespace, что и relay.
	// carrier_mode не настраивается через переменные среды (как и в
	// init.sh по умолчанию) -- https-lanes лучше всего для телефонов.
	profiles := fmt.Sprintf(`{
  "profiles": [
    {
      "name": "default",
      "secret": "%s",
      "backend": "127.0.0.1:2398",
      "carrier_mode": "https-lanes"
    }
  ]
}
`, bareSecret)
	if err := os.WriteFile(configDir+"/profiles.json", []byte(profiles), 0o600); err != nil {
		log.Fatalf("write profiles.json: %v", err)
	}

	if tmplB, err := os.ReadFile(configTemplatePath); err == nil {
		cfgJSON := strings.ReplaceAll(string(tmplB), "__HOSTNAME__", hostnameArg)
		cfgJSON = strings.ReplaceAll(cfgJSON, "__BASE_PATH__", basePath)
		if err := os.WriteFile(configDir+"/config.json", []byte(cfgJSON), 0o644); err != nil {
			log.Fatalf("write config.json: %v", err)
		}
	} else {
		log.Printf("warn: %s не найден (%v) -- config.json не обновлён", configTemplatePath, err)
	}

	// relay работает как uid 65532 (см. relay/Dockerfile) и не стартует
	// при чужих/широких правах на свои файлы.
	_ = os.Chown(configDir+"/profiles.json", 65532, 65532)
	_ = os.Chown(tokenKeyPath, 65532, 65532)

	// Логин/пароль независимого входа (см. комментарий в начале файла про
	// независимость панели от состояния протоколов) -- генерируются один
	// раз, живут в config_data (0600, только этот процесс их читает), при
	// повторных стартах подхватываются, не перегенерируются. ensureFromEnvOrFile
	// уважает ADMIN_USER/ADMIN_PASSWORD, если заданы явно в .env -- тот же
	// принцип, что у ADMIN_PATH/CF_WORKER_AUTH выше.
	adminUser = ensureFromEnvOrFile("ADMIN_USER", configDir+"/admin_user", func() string {
		return "admin" + randHex(2)
	})
	adminPassPlain = ensureFromEnvOrFile("ADMIN_PASSWORD", configDir+"/admin_password", func() string {
		return randBase32Lower(10)
	})
	log.Printf("============================================================")
	log.Printf("  Независимый вход в панель: http://<IP-этого-сервера>:%s/", strings.TrimPrefix(listenAddrPublic, "0.0.0.0:"))
	log.Printf("  Логин:  %s", adminUser)
	log.Printf("  Пароль: %s", adminPassPlain)
	log.Printf("  (эти креды печатаются при КАЖДОМ старте контейнера)")
	log.Printf("============================================================")

	log.Printf("bootstrap: config готов (admin_path=%s)", adminPath)
	currentState() // первичный снэпшот mtproto/socks5 для cover -- см. persistProtoFiles()
	return adminPath, panelTok
}

// ---------- HTTP ----------

// ---------- независимая сессионная авторизация (логин+пароль) ----------
//
// Токен X-Panel-Token (см. authAny ниже) остаётся как было -- его шлёт
// cover для своего внутреннего прокси /ADMIN_PATH/api/. Сессионная кука --
// ВТОРОЙ, независимый способ пройти ту же проверку: для входа напрямую на
// listenAddrPublic (см. main()), который не зависит от того, живы ли
// caddy/cover/relay/tgws вообще. Кука подписана HMAC-SHA256 на token.key
// (уже существующий секрет, который сам процесс и так хранит и больше
// никому не отдаёт) -- без отдельного хранилища сессий на диске/в памяти:
// валидность проверяется пересчётом подписи, а не поиском по списку.

const sessionCookieName = "panel_session"

func signSession(expiry int64) string {
	msg := strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, tokenKeyBytes)
	mac.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifySession(cookieVal string) bool {
	parts := strings.SplitN(cookieVal, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	expected := signSession(expiry)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(cookieVal)) == 1
}

func hasValidSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return verifySession(c.Value)
}

// sessionTTL -- 15 минут по явной просьбе пользователя (2026-11-03): после
// входа сессия сама сбрасывается через этот промежуток, а не живёт неделями
// -- случайно забытая открытая вкладка не держит панель залогиненной вечно.
const sessionTTL = 15 * time.Minute

func issueSession(w http.ResponseWriter) {
	expiry := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: signSession(expiry), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
}

// authAny -- либо валидный X-Panel-Token (путь cover, как раньше), либо
// валидная сессионная кука (путь независимого входа). Заменяет старый
// auth() везде -- оба листенера (internal :7654 и public :7655, см.
// main()) используют одни и те же HTTP-хендлеры, разница только в том,
// каким из двух способов запрос доказывает право на доступ.
func authAny(w http.ResponseWriter, r *http.Request) bool {
	if got := r.Header.Get("X-Panel-Token"); got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(panelToken)) == 1 {
		return true
	}
	if hasValidSession(r) {
		return true
	}
	http.Error(w, "forbidden", http.StatusForbidden)
	return false
}

// loginPageHTML -- НАМЕРЕННО без упоминания сути/названия проекта (см.
// заголовок файла) -- эта страница теперь служит decoy'ем, который видит
// любой посторонний посетитель публичного домена (см. cover/main.go,
// decoyAddr теперь проксирует сюда вместо статики из site/html). Бренд
// "NodeSentry" -- тот же вымышленный бренд, что уже используется для
// сайта-прикрытия на Rostelecom (см. site/html).
const loginPageHTML = `<!doctype html><html><head><meta charset="utf-8"><title>NodeSentry — Sign in</title>
<style>
body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
form{background:#1c1c1c;padding:32px;border-radius:12px;width:280px}
input{width:100%%;padding:10px;margin:8px 0;border-radius:6px;border:1px solid #333;background:#0d0d0d;color:#eee;box-sizing:border-box;font-size:15px}
button{width:100%%;padding:10px;margin-top:8px;border-radius:6px;border:0;background:#2a7fff;color:#fff;font-weight:600;cursor:pointer;font-size:15px}
.err{color:#ff6767;font-size:14px;margin-top:8px}
h1{font-size:18px;margin:0 0 16px}
</style></head><body>
<form method="post" action="/login">
<h1>NodeSentry</h1>
<input name="user" placeholder="Username" autofocus required>
<input name="pass" type="password" placeholder="Password" required>
<button type="submit">Sign in</button>
%s
</form></body></html>`

func handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if hasValidSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, loginPageHTML, "")
}

func handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u, p := r.FormValue("user"), r.FormValue("pass")
	okUser := subtle.ConstantTimeCompare([]byte(u), []byte(adminUser)) == 1
	okPass := subtle.ConstantTimeCompare([]byte(p), []byte(adminPassPlain)) == 1
	if !okUser || !okPass {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, loginPageHTML, `<div class="err">Неверный логин или пароль</div>`)
		return
	}
	issueSession(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSession(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// adminTemplateRaw -- ТОТ ЖЕ файл, что использует cover для /ADMIN_PATH/
// (site/admin/index.html.template, см. Dockerfile COPY) -- единый источник
// разметки для обоих входов (старого, через cover+X-Panel-Token, и нового
// независимого через сессионную куку), чтобы интерфейс не расходился на
// две версии. Отличие только в ИСТОЧНИКЕ подставляемых значений: cover
// читает их из файлов в config_data (пишет persistProtoFiles(), см. выше),
// а здесь -- напрямую из currentState() в памяти этого же процесса, без
// лишнего файлового round-trip.
var adminTemplateRaw string

// protoFieldsFromState -- то же самое, что protoFields() в cover/main.go,
// переписанное на protoState вместо чтения файлов (см. комментарий выше у
// adminTemplateRaw) -- ДОЛЖНО давать идентичный набор плейсхолдеров, иначе
// страница будет рендериться по-разному в зависимости от того, через какой
// вход открыта.
func protoFieldsFromState(proto string, s protoState) map[string]string {
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
	if s.LocalIPAvailable {
		f["LOCALIP_ROW_STYLE"] = "flex"
		if s.LocalIPActive {
			f["LOCALIP_SWITCH_ON"] = "on"
		}
	}
	if s.Enabled && s.Link != "" {
		f["SWITCH_ON"] = "on"
		f["STATUS_CLASS"] = "ok"
		statusText := "● активен"
		if proto == "mtproto" && s.SNI != "" {
			statusText += ", fake-TLS под " + s.SNI
		}
		f["STATUS_TEXT"] = statusText
		f["LINK"] = s.Link
		if s.Open != "" {
			f["OPEN"] = s.Open
		}
		if s.QR != "" {
			f["QR_SRC"] = s.QR
			f["QR_CLASS"] = "show"
		}
		f["ACTIONS_STYLE"] = "flex"
	}
	return f
}

func buildDashboardHTML() string {
	st := currentState()
	page := strings.ReplaceAll(adminTemplateRaw, "__HOSTNAME__", hostnameArg)
	page = strings.ReplaceAll(page, "__WEB_LINK__", st["web"].Link)
	page = strings.ReplaceAll(page, "__WEB_OPEN__", st["web"].Open)
	page = strings.ReplaceAll(page, "__WEB_QR__", st["web"].QR)
	for _, proto := range []string{"mtproto", "socks5"} {
		prefix := "__" + strings.ToUpper(proto) + "_"
		for key, val := range protoFieldsFromState(proto, st[proto]) {
			page = strings.ReplaceAll(page, prefix+key+"__", val)
		}
	}
	return page
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !hasValidSession(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(buildDashboardHTML()))
}

// ---------- диагностика: CF Worker + Telegram DC ----------
//
// Не читает внутреннее состояние tgws (у него нет HTTP API для этого) --
// panel-admin сам, независимо, проверяет те же две вещи, от которых
// зависит реальная работа WEB-прокси: доступен ли Cloudflare Worker
// (TCP:443) и доступны ли дата-центры Telegram НАПРЯМУЮ (см. комментарий
// у tgDCProbes). Результат кэшируется в памяти, обновляется раз в 30с
// фоновым циклом -- запрос /status никогда не ждёт сетевого таймаута.

type statusInfo struct {
	CFWorkerOK   bool      `json:"cfWorkerOk"`
	CFWorkerAt   time.Time `json:"cfWorkerCheckedAt"`
	CFWorkerHost string    `json:"cfWorkerHost,omitempty"`
	TGDCOK       bool      `json:"tgDcOk"`
	TGDCAt       time.Time `json:"tgDcCheckedAt"`
}

var (
	statusMu   sync.RWMutex
	lastStatus statusInfo
)

func checkTCP(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func checkCFWorker() bool {
	host := os.Getenv("CF_WORKER")
	if host == "" {
		return false
	}
	return checkTCP(host+":443", 5*time.Second)
}

// checkTGDC -- "доступен ли путь к DC Telegram через воркер". Везде, где
// реально развёрнут этот стек (MTS/NETH1/Rostelecom), TGWS_ONLY_WORKER=1 --
// прямые соединения к IP телеграмных дата-центров у как минимум одного
// провайдера (MTS) заблокированы (подтверждено живьём 2026-11-02:
// таймаут на 149.154.167.51:443/149.154.167.91:443 при полностью рабочем
// остальном интернете с того же хоста), и tgws обходит это ЧЕРЕЗ
// Cloudflare Worker -- значит реальный, актуальный вопрос для диагностики
// не "доступны ли DC напрямую" (это почти всегда "нет" и выглядит как
// ложная тревога), а "жив ли путь, которым tgws их реально достаёт" --
// тот же CF_WORKER, что и у checkCFWorker() выше.
func checkTGDC() bool {
	return checkCFWorker()
}

func statusLoop() {
	update := func() {
		cf, dc := checkCFWorker(), checkTGDC()
		statusMu.Lock()
		lastStatus = statusInfo{
			CFWorkerOK: cf, CFWorkerAt: time.Now(), CFWorkerHost: os.Getenv("CF_WORKER"),
			TGDCOK: dc, TGDCAt: time.Now(),
		}
		statusMu.Unlock()
	}
	update()
	for range time.Tick(30 * time.Second) {
		update()
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if !authAny(w, r) {
		return
	}
	statusMu.RLock()
	s := lastStatus
	statusMu.RUnlock()
	writeJSON(w, s)
}

type protoState struct {
	Enabled bool   `json:"enabled"`
	Port    string `json:"port,omitempty"`
	SNI     string `json:"sni,omitempty"`
	User    string `json:"user,omitempty"`
	Link    string `json:"link,omitempty"`
	Open    string `json:"open,omitempty"` // tg://... для кнопки "Открыть в Telegram" -- см. webLinkApp()/socks5LinkApp()
	QR      string `json:"qr,omitempty"` // data:image/png;base64,... -- см. qrDataURI()
	Note    string `json:"note,omitempty"` // информационная строка (например "нет сертификата")
	// Только для socks5 -- тумблер "локальный IP вместо домена" в ссылке/QR,
	// см. socks5Server()/socks5LocalIPActive().
	LocalIPAvailable bool `json:"localIpAvailable,omitempty"`
	LocalIPActive    bool `json:"localIpActive,omitempty"`
}

// qrDataURI: QR-код генерируется на сервере (Go, github.com/skip2/go-qrcode),
// не в браузере клиента -- страница панели должна открываться и по чисто
// локальному IP без интернета вообще (см. комментарий в docker-compose.yml
// про FORWARDER_PORT), а любая JS-библиотека с CDN в таком случае просто не
// загрузится. Возвращает пустую строку для пустой ссылки (протокол выключен).
func qrDataURI(link string) string {
	if link == "" {
		return ""
	}
	png, err := qrcode.Encode(link, qrcode.Medium, 220)
	if err != nil {
		log.Printf("warn: qrcode.Encode(%q): %v", link, err)
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// persistProtoFiles пишет небольшой снэпшот состояния mtproto/socks5 в
// config_data -- тем же приёмом, каким web_link.txt/web_open.txt/web_qr.txt
// уже пишутся выше (bootstrap()/handleRegen()). cover читает эти файлы на
// каждый HTTP-запрос и сразу подставляет реальное состояние в HTML --
// без этого браузеру приходилось узнавать включён/выключен протокол уже
// ПОСЛЕ загрузки страницы через /api/state (см. index.html.template),
// то есть страница на мгновение показывала неправду. web в этот список не
// входит -- у него свои файлы, см. выше.
func persistProtoFiles(st map[string]protoState) {
	for _, proto := range []string{"mtproto", "socks5"} {
		s := st[proto]
		enabled := ""
		if s.Enabled {
			enabled = "1"
		}
		files := map[string]string{
			proto + "_enabled":  enabled,
			proto + "_link.txt": s.Link,
			proto + "_open.txt": s.Open,
			proto + "_qr.txt":   s.QR,
			proto + "_sni.txt":  s.SNI,
		}
		if proto == "socks5" {
			localAvail, localActive := "", ""
			if s.LocalIPAvailable {
				localAvail = "1"
			}
			if s.LocalIPActive {
				localActive = "1"
			}
			files["socks5_localip_available"] = localAvail
			files["socks5_localip_active"] = localActive
		}
		for name, val := range files {
			path := configDir + "/" + name
			if err := os.WriteFile(path, []byte(val+"\n"), 0o644); err != nil {
				log.Printf("warn: write %s: %v", name, err)
				continue
			}
			_ = os.Chown(path, 65532, 65532) // то же, что у web_*.txt -- читает cover
		}
	}
}

func currentState() map[string]protoState {
	env, _, _ := readEnv()
	out := map[string]protoState{}

	webSecret, _ := readFileTrim(configDir + "/secret.hex")
	webBasePath, _ := readFileTrim(configDir + "/base_path")
	webLinkVal := ""
	if webSecret != "" {
		webLinkVal = webLink(webSecret, webBasePath)
	}
	out["web"] = protoState{Enabled: serviceRunning("relay"), Link: webLinkVal, Open: webLinkApp(webSecret, webBasePath), QR: qrDataURI(webLinkVal)}

	mtPort := env["MTPROTO_PORT"]
	mtSNI := env["MTPROTO_FAKE_TLS_DOMAIN"]
	mtSecret, _ := readFileTrim(configDir + "/mtproto_secret.hex")
	mtEnabled := serviceRunning("mtproto")
	mtLink := ""
	if mtPort != "" && mtSNI != "" && mtSecret != "" {
		mtLink = mtprotoLink(mtSecret, mtSNI, mtPort)
	}
	out["mtproto"] = protoState{Enabled: mtEnabled, Port: mtPort, SNI: mtSNI, Link: mtLink, Open: mtLink, QR: qrDataURI(mtLink)}

	s5Port := env["SOCKS5_PORT"]
	s5User := env["SOCKS5_USER"]
	s5Pass, _ := readFileTrim(configDir + "/socks5_pass.hex")
	s5Enabled := allRunning(composeServices("socks5"))
	s5Link := ""
	if s5Port != "" && s5User != "" && s5Pass != "" {
		s5Link = socks5Link(s5User, s5Pass, s5Port)
	}
	s5Open := ""
	if s5Link != "" {
		s5Open = socks5LinkApp(s5User, s5Pass, s5Port)
	}
	out["socks5"] = protoState{
		Enabled: s5Enabled, Port: s5Port, User: s5User, Link: s5Link, Open: s5Open, QR: qrDataURI(s5Link),
		LocalIPAvailable: socks5LocalIPAvailable(), LocalIPActive: socks5LocalIPActive(),
	}

	persistProtoFiles(out)
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func handleState(w http.ResponseWriter, r *http.Request) {
	if !authAny(w, r) {
		return
	}
	writeJSON(w, currentState())
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Без авторизации -- только "bootstrap выполнен, сервер поднят", без
	// каких-либо секретов в ответе. Используется compose healthcheck'ом,
	// чтобы tgws/relay/cover дождались готового config_data перед своим
	// стартом (см. depends_on: condition: service_healthy в docker-compose.yml).
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

type toggleReq struct {
	Proto string `json:"proto"`
	On    bool   `json:"on"`
	Port  string `json:"port"`
	SNI   string `json:"sni"`
	User  string `json:"user"`
	Pass  string `json:"pass"`
}

func validProto(p string) bool { return p == "web" || p == "mtproto" || p == "socks5" }

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 65536
}

func handleToggle(w http.ResponseWriter, r *http.Request) {
	if !authAny(w, r) {
		return
	}
	var req toggleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validProto(req.Proto) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()

	services := composeServices(req.Proto)

	// web -- просто relay, без порта/секретов (база уже поднята tgws/cover)
	if req.Proto == "web" {
		if req.On {
			if out, err := composeRun("up", "-d", services[0]); err != nil {
				http.Error(w, "docker compose up: "+err.Error()+"\n"+out, http.StatusInternalServerError)
				return
			}
		} else {
			if out, err := composeRun("stop", services[0]); err != nil {
				http.Error(w, "docker compose stop: "+err.Error()+"\n"+out, http.StatusInternalServerError)
				return
			}
		}
		waitForAll(services, req.On, 20*time.Second)
		writeJSON(w, currentState()["web"])
		return
	}

	if req.On {
		env, _, _ := readEnv()
		portKey := strings.ToUpper(req.Proto) + "_PORT"
		port := req.Port
		if port == "" {
			port = env[portKey]
		}
		if !validPort(port) {
			http.Error(w, "нужен корректный порт", http.StatusBadRequest)
			return
		}
		updates := map[string]string{portKey: port}

		if req.Proto == "mtproto" {
			sni := req.SNI
			if sni == "" {
				sni = env["MTPROTO_FAKE_TLS_DOMAIN"]
			}
			if sni == "" {
				sni = "www.google.com"
			}
			updates["MTPROTO_FAKE_TLS_DOMAIN"] = sni
			mtSecret, err := readFileTrim(configDir + "/mtproto_secret.hex")
			if err != nil || mtSecret == "" {
				mtSecret = randHex(16)
				_ = writeSecretFile("mtproto_secret.hex", mtSecret)
			}
			updates["MTPROTO_SECRET"] = mtSecret // всегда синхронизируем файл -> .env, не только при первом создании
		} else { // socks5: голый SOCKS5 с логином/паролем, без TLS -- у протокола
			// нет своего шифрования транспорта, и Telegram-клиенты не умеют
			// SOCKS5-over-TLS (проверено против документации/поведения клиентов)
			user := req.User
			if user == "" {
				user = env["SOCKS5_USER"]
			}
			if user == "" {
				user = "user" + randHex(2)
			}
			updates["SOCKS5_USER"] = user
			s5pass := req.Pass
			if s5pass == "" {
				if existing, err := readFileTrim(configDir + "/socks5_pass.hex"); err == nil && existing != "" {
					s5pass = existing
				} else {
					s5pass = randHex(12)
				}
			}
			_ = writeSecretFile("socks5_pass.hex", s5pass)
			updates["SOCKS5_PASS"] = s5pass // всегда синхронизируем файл -> .env
		}
		if err := writeEnv(updates); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		args := append([]string{"up", "-d", "--force-recreate"}, services...)
		if out, err := composeRun(args...); err != nil {
			http.Error(w, "docker compose up: "+err.Error()+"\n"+out, http.StatusInternalServerError)
			return
		}
		waitForAll(services, true, 20*time.Second)
	} else {
		args := append([]string{"stop"}, services...)
		if out, err := composeRun(args...); err != nil {
			http.Error(w, "docker compose stop: "+err.Error()+"\n"+out, http.StatusInternalServerError)
			return
		}
		waitForAll(services, false, 15*time.Second)
	}

	writeJSON(w, currentState()[req.Proto])
}

type regenReq struct {
	Proto string `json:"proto"`
}

func handleRegen(w http.ResponseWriter, r *http.Request) {
	if !authAny(w, r) {
		return
	}
	var req regenReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()

	switch req.Proto {
	case "web":
		secret := randHex(16)
		if err := writeSecretFile("secret.hex", secret); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		profiles := fmt.Sprintf(`{
  "profiles": [
    {
      "name": "default",
      "secret": "%s",
      "backend": "127.0.0.1:2398",
      "carrier_mode": "https-lanes"
    }
  ]
}
`, secret)
		if err := os.WriteFile(configDir+"/profiles.json", []byte(profiles), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = composeRun("restart", "relay")
		basePath, _ := readFileTrim(configDir + "/base_path")
		link := webLink(secret, basePath)
		openLink := webLinkApp(secret, basePath)
		// Те же файлы, что читает cover для серверного рендера -- см.
		// комментарий в bootstrap() про web_link.txt/web_open.txt/web_qr.txt.
		// Без этого после перевыпуска секрета страница отдавала бы старую
		// ссылку при прямом открытии /ADMIN_PATH/.
		if err := os.WriteFile(configDir+"/web_link.txt", []byte(link+"\n"), 0o644); err != nil {
			log.Printf("warn: write web_link.txt: %v", err)
		}
		if err := os.WriteFile(configDir+"/web_open.txt", []byte(openLink+"\n"), 0o644); err != nil {
			log.Printf("warn: write web_open.txt: %v", err)
		}
		if err := os.WriteFile(configDir+"/web_qr.txt", []byte(qrDataURI(link)+"\n"), 0o644); err != nil {
			log.Printf("warn: write web_qr.txt: %v", err)
		}
		writeJSON(w, map[string]string{"link": link, "open": openLink, "qr": qrDataURI(link)})
	case "mtproto", "socks5":
		services := composeServices(req.Proto)
		if req.Proto == "mtproto" {
			_ = writeSecretAndEnv("mtproto_secret.hex", "MTPROTO_SECRET", randHex(16))
		} else {
			_ = writeSecretAndEnv("socks5_pass.hex", "SOCKS5_PASS", randHex(12))
		}
		if allRunning(services) {
			args := append([]string{"up", "-d", "--force-recreate"}, services...)
			_, _ = composeRun(args...)
			waitForAll(services, true, 20*time.Second)
		}
		writeJSON(w, currentState()[req.Proto])
	default:
		http.Error(w, "bad proto", http.StatusBadRequest)
	}
}

type socks5LocalIPReq struct {
	On bool `json:"on"`
}

// handleSocks5LocalIP -- тумблер в панели "ссылка/QR SOCKS с локальным IP
// вместо домена", см. socks5Server()/socks5LocalIPActive(). Только меняет
// сохранённое предпочтение и перегенерированный currentState() сам пересчитает
// ссылку -- сервис socks5 не трогаем (адрес в ссылке -- не то же самое, что
// сам прокси-сервер, его слушающий порт/учётка не меняются).
func handleSocks5LocalIP(w http.ResponseWriter, r *http.Request) {
	if !authAny(w, r) {
		return
	}
	var req socks5LocalIPReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()

	if !socks5LocalIPAvailable() {
		http.Error(w, "локальный IP не задан и не удалось определить автоматически -- переключать нечего", http.StatusBadRequest)
		return
	}
	val := "0"
	if req.On {
		val = "1"
	}
	if err := os.WriteFile(configDir+"/"+socks5LocalIPPrefFile, []byte(val+"\n"), 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, currentState()["socks5"])
}

// webLink повторяет логику init.sh: 0x70 || голый секрет -> base64url без паддинга.
func webLink(secretHex, basePath string) string {
	raw, err := hex.DecodeString(strings.TrimPrefix(secretHex, "dd"))
	if err != nil {
		return ""
	}
	payload := append([]byte{0x70}, raw...)
	enc := base64URLNoPad(payload)
	server := hostnameArg
	if basePath != "" {
		server = hostnameArg + "%2F" + basePath
	}
	return fmt.Sprintf("https://t.me/webproxy?server=%s&secret=%s", server, enc)
}

// webLinkApp: та же ссылка, но tg:// вместо https://t.me/ -- официальный
// README tproxy-server (telegramdesktop) прямо говорит: "Clients may also
// accept the equivalent tg://webproxy form. The public t.me frontend does
// not yet register this route" -- т.е. https://t.me/webproxy сейчас НЕ
// триггерит открытие приложения (t.me сам не знает про этот путь, просто
// отдаёт обычный сайт), а tg:// -- системная схема, которую Telegram
// Desktop/мобильные клиенты регистрируют у ОС напрямую, в обход t.me
// вообще (тот же принцип, что у tg://proxy для MTProto, который уже и так
// открывает приложение корректно). Используется ТОЛЬКО для кнопки
// "Открыть в Telegram" -- копируемая/видимая ссылка и QR остаются
// https://t.me/... (это "shareable"-формат по самой спецификации).
func webLinkApp(secretHex, basePath string) string {
	link := webLink(secretHex, basePath)
	if link == "" {
		return ""
	}
	return strings.Replace(link, "https://t.me/webproxy?", "tg://webproxy?", 1)
}

func base64URLNoPad(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		chunk := b[i:minInt(i+3, len(b))]
		n := len(chunk)
		var v uint32
		for j := 0; j < 3; j++ {
			v <<= 8
			if j < n {
				v |= uint32(chunk[j])
			}
		}
		outLen := map[int]int{1: 2, 2: 3, 3: 4}[n]
		for j := 0; j < outLen; j++ {
			shift := uint(18 - 6*j)
			sb.WriteByte(alphabet[(v>>shift)&0x3f])
		}
	}
	return sb.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	hostnameArg = mustEnv("HOSTNAME_ARG")
	composeProject = detectComposeProject()
	log.Printf("определён compose-проект: %s", composeProject)
	dockerLogin()
	adminPath, panelTok := bootstrap()
	panelToken = envDefault("PANEL_TOKEN", panelTok)
	_ = adminPath // печатается внутри bootstrap(); отдельно наружу не нужен -- cover сам читает тот же файл

	if b, err := os.ReadFile(configDir + "/token.key"); err != nil {
		log.Fatalf("не удалось прочитать token.key: %v", err)
	} else {
		tokenKeyBytes = b
	}

	adminHTMLDir := envDefault("ADMIN_HTML_DIR", "/site/admin")
	if b, err := os.ReadFile(adminHTMLDir + "/index.html.template"); err != nil {
		log.Fatalf("не удалось прочитать %s/index.html.template: %v", adminHTMLDir, err)
	} else {
		adminTemplateRaw = string(b)
	}

	go statusLoop()

	// Внутренний API, как раньше -- cover ходит сюда с X-Panel-Token (см.
	// panelProxy в cover/main.go). Поведение не меняется.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", handleState)
	mux.HandleFunc("POST /toggle", handleToggle)
	mux.HandleFunc("POST /regen", handleRegen)
	mux.HandleFunc("POST /socks5-local-ip", handleSocks5LocalIP)
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /status", handleStatus)

	// Публичный независимый вход -- своя сессионная авторизация
	// логин+пароль (см. bootstrap(), печатаются в лог), НЕ зависит от
	// состояния relay/cover/caddy/tgws вообще: свой листенер на отдельном
	// порту, напрямую из compose (ports: в docker-compose.*.yml), минуя
	// caddy/cover полностью. Те же хендлеры /state,/toggle,/regen, что и
	// у внутреннего API -- authAny() принимает здесь сессионную куку вместо
	// X-Panel-Token.
	pmux := http.NewServeMux()
	pmux.HandleFunc("GET /login", handleLoginPage)
	pmux.HandleFunc("POST /login", handleLoginSubmit)
	pmux.HandleFunc("GET /logout", handleLogout)
	pmux.HandleFunc("GET /", handleDashboard)
	pmux.HandleFunc("GET /state", handleState)
	pmux.HandleFunc("POST /toggle", handleToggle)
	pmux.HandleFunc("POST /regen", handleRegen)
	pmux.HandleFunc("POST /socks5-local-ip", handleSocks5LocalIP)
	pmux.HandleFunc("GET /status", handleStatus)
	pmux.HandleFunc("GET /healthz", handleHealthz)
	// /api/* -- алиасы тех же хендлеров под тем же префиксом, что ждёт JS
	// шаблона (site/admin/index.html.template: fetch('api/'+path), путь
	// относительный -- на странице, открытой с "/", резолвится в "/api/...").
	// Сам шаблон -- общий с cover (см. adminTemplateRaw выше), который
	// проксирует api/ под /ADMIN_PATH/api/ -- здесь то же самое, просто без
	// ADMIN_PATH-префикса (независимый вход и так требует сессионную куку).
	pmux.HandleFunc("GET /api/state", handleState)
	pmux.HandleFunc("POST /api/toggle", handleToggle)
	pmux.HandleFunc("POST /api/regen", handleRegen)
	pmux.HandleFunc("POST /api/socks5-local-ip", handleSocks5LocalIP)
	pmux.HandleFunc("GET /api/status", handleStatus)

	go func() {
		log.Printf("panel-admin: независимый вход слушает %s (логин=%s)", listenAddrPublic, adminUser)
		log.Fatal(http.ListenAndServe(listenAddrPublic, pmux))
	}()

	log.Printf("panel-admin слушает %s, проект %s (compose %s)", listenAddr, composeProject, composeFilePath)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
