package main

// Подключение телефона без ручной возни: в панели «Подключить телефон» ->
// QR со ссылкой-приглашением (подписана token.key, живёт 15 минут, годится для
// нескольких устройств). Телефон открывает страницу, задаёт имя (по умолчанию
// модель из User-Agent), получает свой профиль WireGuard и клиент:
// Android -- APK прямо отсюда, iPhone -- ссылка в App Store.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const inviteTTL = 15 * time.Minute

func signInvite(exp int64) string {
	m := hmac.New(sha256.New, tokenKey)
	m.Write([]byte("wg-invite:" + strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 36) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:22]
}

func validInvite(tok string) bool {
	ts, _, ok := strings.Cut(tok, ".")
	exp, err := strconv.ParseInt(ts, 36, 64)
	if !ok || err != nil || time.Now().Unix() > exp {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(signInvite(exp)), []byte(tok)) == 1
}

// inviteBase -- адрес панели, который телефон в той же сети точно откроет.
func inviteBase(r *http.Request) string {
	if ip := localIP(); ip != "" {
		return "http://" + net.JoinHostPort(ip, portOf(panelAddr))
	}
	return "http://" + r.Host
}

func handleWgInvite(w http.ResponseWriter, r *http.Request) {
	if !flag("wg_enabled") {
		http.Error(w, "сначала включите модуль WireGuard", http.StatusBadRequest)
		return
	}
	exp := time.Now().Add(inviteTTL).Unix()
	u := inviteBase(r) + "/join/" + signInvite(exp)
	// крупный и редкий QR: его читают с экрана монитора, иногда издалека
	png, _ := qrcode.Encode(u, qrcode.Low, 512)
	writeJSON(w, map[string]any{"url": u, "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), "expires": exp})
}

var (
	uaAndroidModel = regexp.MustCompile(`Android [0-9.]+; (?:[a-z]{2}[-_][A-Za-z]{2}; )?([^;)]+?)(?: Build/[^;)]*)?[;)]`)
	nameClean      = regexp.MustCompile(`[^\p{L}\p{N} ._-]+`)
)

func deviceKind(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		return "ios"
	case strings.Contains(ua, "Android"):
		return "android"
	}
	return "other"
}

func defaultDeviceName(ua string) string {
	if m := uaAndroidModel.FindStringSubmatch(ua); m != nil && strings.TrimSpace(m[1]) != "K" {
		return strings.TrimSpace(nameClean.ReplaceAllString(m[1], ""))
	}
	switch deviceKind(ua) {
	case "ios":
		if strings.Contains(ua, "iPad") {
			return "iPad"
		}
		return "iPhone"
	case "android":
		return "Android"
	}
	return "Устройство"
}

// uniqueName: «S24», «S24 2», «S24 3», …
func uniqueName(base string, ps []wgPeer) string {
	base = strings.TrimSpace(nameClean.ReplaceAllString(base, ""))
	if len([]rune(base)) > 28 {
		base = string([]rune(base)[:28])
	}
	if base == "" {
		base = "Устройство"
	}
	used := map[string]bool{}
	for _, p := range ps {
		used[p.Name] = true
	}
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s %d", base, i); !used[n] {
			return n
		}
	}
}

func joinRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /join/{tok}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !validInvite(r.PathValue("tok")) || !flag("wg_enabled") {
			w.WriteHeader(http.StatusGone)
			fmt.Fprint(w, joinShell(`<h1>Ссылка устарела</h1><p>Попросите в панели новый QR-код («Подключить телефон»).</p>`))
			return
		}
		ua := r.UserAgent()
		fmt.Fprint(w, joinShell(fmt.Sprintf(joinBody, html.EscapeString(defaultDeviceName(ua)), deviceKind(ua))))
	})
	mux.HandleFunc("POST /join/{tok}", func(w http.ResponseWriter, r *http.Request) {
		if !validInvite(r.PathValue("tok")) || !flag("wg_enabled") {
			http.Error(w, "ссылка устарела — откройте в панели новый QR-код", http.StatusGone)
			return
		}
		var req struct{ Name string `json:"name"` }
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		mu.Lock()
		ps := wgPeers()
		if len(ps) >= 250 {
			mu.Unlock()
			http.Error(w, "нет свободных адресов", http.StatusConflict)
			return
		}
		name := uniqueName(req.Name, ps)
		p, err := addWgPeer(name)
		mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"name": p.Name, "conf": wgClientConf(p), "sfa": sfaImportURL(inviteBase(r), p)})
	})
	for _, f := range []string{"wireguard.apk", "sing-box.apk", "sing-box-armv7.apk"} {
		f := f
		mux.HandleFunc("GET /join/{tok}/"+f, func(w http.ResponseWriter, r *http.Request) {
			if !validInvite(r.PathValue("tok")) {
				http.Error(w, "ссылка устарела", http.StatusGone)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.android.package-archive")
			w.Header().Set("Content-Disposition", `attachment; filename="`+f+`"`)
			http.ServeFile(w, r, shareDir+"/"+f)
		})
	}
	// профиль sing-box: по секретному токену устройства, без входа (приложение обновляет его само)
	mux.HandleFunc("GET /p/{tok}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := peerByToken(r.PathValue("tok"))
		if !ok || !flag("wg_enabled") {
			http.NotFound(w, r)
			return
		}
		b, err := singboxProfile(p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
}

func joinShell(body string) string {
	return `<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>Подключение телефона</title><style>
body{font-family:system-ui,sans-serif;background:#0f1419;color:#d6e0ea;margin:0;padding:20px;line-height:1.45}
.wrap{max-width:460px;margin:0 auto}h1{font-size:20px;margin:6px 0 14px}p{color:#9aa8b6;font-size:14px}
.step{background:#161c24;border:1px solid #2a333f;border-radius:12px;padding:14px;margin:12px 0}
.step b{display:block;margin-bottom:8px}input{width:100%;box-sizing:border-box;padding:11px;border-radius:8px;border:1px solid #2a333f;background:#0a0d12;color:#eee;font-size:16px}
.btn{display:block;text-align:center;padding:12px;border-radius:8px;background:#3b9dff;color:#fff;font-weight:600;text-decoration:none;border:0;font-size:16px;width:100%;box-sizing:border-box;margin-top:10px;cursor:pointer}
.btn.ghost{background:transparent;border:1px solid #2a333f;color:#d6e0ea}.ok{color:#3ecf8e}.err{color:#e85d5d}.hide{display:none}small{color:#7c8a9a}
</style></head><body><div class="wrap">` + body + `</div></body></html>`
}

const joinBody = `<h1>Telegram через домашний сервер</h1>
<p>Звонки и сообщения Telegram пойдут через туннель WireGuard. Остальной интернет — как обычно.</p>
<div class="step" id="s1"><b>1. Имя этого устройства</b>
<input id="name" value="%s" maxlength="32">
<button class="btn" id="go">Получить профиль</button><div id="msg"></div></div>
<div class="step hide" id="sfa2"><b>2. Приложение sing-box</b>
<a class="btn ghost" id="sfaapk" href="">Скачать sing-box для Android</a>
<small>После скачивания откройте файл и разрешите установку из браузера. Старый телефон (32 бита)? <a id="sfav7" href="" style="color:#3b9dff">Эта версия</a>.</small></div>
<div class="step hide" id="sfa3"><b>3. Профиль <span class="pn"></span></b>
<a class="btn" id="sfaimp" href="">Добавить в sing-box</a>
<small>Откроется sing-box и добавит профиль. Включите его и разрешите VPN.</small>
<p style="margin-top:12px"><a href="#" id="usewg" style="color:#7c8a9a;font-size:13px">Нужен обычный WireGuard?</a></p></div>
<div class="step hide" id="s2"><b>2. Приложение WireGuard</b>
<a class="btn ghost hide" id="apk" href="">Скачать WireGuard для Android</a>
<a class="btn ghost hide" id="ios" href="https://apps.apple.com/app/wireguard/id1441195209">WireGuard в App Store</a>
<small id="apkhint" class="hide">После скачивания откройте файл и разрешите установку из браузера.</small></div>
<div class="step hide" id="s3"><b>3. Профиль <span id="pname"></span></b>
<a class="btn" id="conf" href="">Скачать профиль</a>
<small id="hint"></small></div>
<div class="step hide" id="s4"><b>4. В Telegram</b><small>Выключите прокси (Настройки → Данные и память → Прокси), если он был включён. Туннель WireGuard держите включённым.</small></div>
<script>
const kind='%s', base=location.pathname;
const $=s=>document.querySelector(s);
if(kind==='android'){$('#apk').href=base+'/wireguard.apk';$('#apk').classList.remove('hide');$('#apkhint').classList.remove('hide');}
else if(kind==='ios'){$('#ios').classList.remove('hide');}
else {$('#apk').href=base+'/wireguard.apk';$('#apk').classList.remove('hide');$('#ios').classList.remove('hide');}
$('#hint').textContent = kind==='ios'
 ? 'Нажмите «Скачать профиль», затем «Поделиться» → WireGuard. Или в WireGuard: «+» → «Создать из файла».'
 : 'Нажмите «Скачать профиль», затем в WireGuard: «+» → «Импорт из файла» → выберите скачанный файл и включите туннель.';
$('#go').onclick=()=>{
  $('#go').disabled=true;$('#msg').textContent='';
  fetch(base,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:$('#name').value.trim()})})
  .then(r=>r.ok?r.json():r.text().then(t=>{throw new Error(t)}))
  .then(d=>{
    $('#pname').textContent='«'+d.name+'»';document.querySelectorAll('.pn').forEach(e=>e.textContent='«'+d.name+'»');
    $('#sfaimp').href=d.sfa;$('#sfaapk').href=base+'/sing-box.apk';$('#sfav7').href=base+'/sing-box-armv7.apk';
    const fn=d.name.replace(/[^A-Za-z0-9._-]+/g,'_').slice(0,15)||'telegram';
    const blob=new Blob([d.conf],{type:'application/octet-stream'});
    $('#conf').href=URL.createObjectURL(blob);$('#conf').download=fn+'.conf';
    $('#s1').innerHTML='<b class="ok">✓ Устройство «'+d.name.replace(/</g,'&lt;')+'» добавлено</b>';
    const steps = kind==='android' ? ['#sfa2','#sfa3','#s4'] : ['#s2','#s3','#s4'];
    steps.forEach(s=>$(s).classList.remove('hide'));
  })
  .catch(e=>{$('#go').disabled=false;$('#msg').innerHTML='<p class="err">'+e.message.replace(/</g,'&lt;')+'</p>';});
};
$('#usewg').onclick=e=>{e.preventDefault();['#sfa2','#sfa3'].forEach(s=>$(s).classList.add('hide'));['#s2','#s3'].forEach(s=>$(s).classList.remove('hide'));};
</script>`
