// Cloudflare Worker для tg-ws-proxy (протокол из Flowseal/tg-ws-proxy, docs/RU/CfWorker.md).
// Отличия от оригинала:
//  1. Нужен ключ: параметр ?auth=… должен совпасть с секретом AUTH воркера
//     (Settings → Variables and Secrets → Add → тип Secret, имя AUTH).
//     Без секрета AUTH воркер отклоняет всё.
//  2. Соединяет ТОЛЬКО с адресами Telegram. Список сетей = встроенный (ниже)
//     + официальный список Telegram (core.telegram.org/resources/cidr.txt),
//     который воркер сам подгружает и обновляет раз в сутки. Если файл
//     недоступен, работает встроенный список.
// На любой неподходящий запрос — одинаковый 404, по ответу нельзя понять, что именно не так.

import { connect } from "cloudflare:sockets";

// Встроенные сети Telegram (IPv4) — запасной список, работает всегда.
// tg-ws-proxy передаёт в dst IP дата-центра.
const TELEGRAM_NETS = [
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
];

function ipv4ToInt(ip) {
	const parts = ip.split(".");
	if (parts.length !== 4) return null;
	let value = 0;
	for (const part of parts) {
		if (!/^\d{1,3}$/.test(part)) return null;
		const n = Number(part);
		if (n > 255) return null;
		value = value * 256 + n;
	}
	return value;
}

// "a.b.c.d/n" → [начало, конец). Сети шире /12 не принимаем: даже испорченный
// файл со списком не сможет открыть воркер «на весь интернет».
function parseCidr(cidr) {
	const match = /^(\d{1,3}(?:\.\d{1,3}){3})\/(\d{1,2})$/.exec(cidr.trim());
	if (!match) return null;
	const base = ipv4ToInt(match[1]);
	const bits = Number(match[2]);
	if (base === null || bits < 12 || bits > 32) return null;
	const size = 2 ** (32 - bits);
	const start = Math.floor(base / size) * size;
	return [start, start + size];
}

const BUILTIN_NETS = TELEGRAM_NETS.map(parseCidr).filter(Boolean);

// --- официальный список Telegram, обновляется раз в сутки ---
const CIDR_URL = "https://core.telegram.org/resources/cidr.txt";
const REFRESH_MS = 24 * 60 * 60 * 1000;
let officialNets = [];
let officialAt = 0;
let lastAttempt = 0;
const RETRY_MS = 5 * 60 * 1000;

// Загрузка выполняется внутри того запроса, который её начал. Промисы между
// запросами не передаются: в Workers это может «подвесить» чужой запрос.
async function loadOfficialNets() {
	lastAttempt = Date.now();
	try {
		const response = await fetch(CIDR_URL, { cf: { cacheTtl: 3600, cacheEverything: true } });
		if (!response.ok) return;
		const nets = (await response.text()).split(/\s+/).map(parseCidr).filter(Boolean);
		if (nets.length >= 3) {
			officialNets = nets;
			officialAt = Date.now();
		}
	} catch {
		// недоступен — остаёмся на встроенном и прошлом загруженном списке
	}
}

function inNets(value, nets) {
	return nets.some(([start, end]) => value >= start && value < end);
}

// Проверка адреса. Известный адрес пропускается сразу; устаревший список
// обновляется в фоне. Незнакомый адрес — один раз пробуем свежий список.
async function isTelegram(ip, ctx) {
	const value = ipv4ToInt(ip);
	if (value === null) return false;
	const now = Date.now();
	const stale = now - officialAt > REFRESH_MS;
	const mayRetry = now - lastAttempt > RETRY_MS;
	if (inNets(value, BUILTIN_NETS) || inNets(value, officialNets)) {
		if (stale && mayRetry && ctx && ctx.waitUntil) ctx.waitUntil(loadOfficialNets());
		return true;
	}
	if (stale && mayRetry) {
		await loadOfficialNets();
		return inNets(value, officialNets);
	}
	return false;
}

async function toBytes(data) {
	if (data instanceof ArrayBuffer) return new Uint8Array(data);
	if (typeof data === "string") return new TextEncoder().encode(data);
	if (data && typeof data.arrayBuffer === "function") {
		return new Uint8Array(await data.arrayBuffer());
	}
	return new Uint8Array();
}

// Сравнение без утечки по времени.
function safeEqual(a, b) {
	const x = new TextEncoder().encode(a);
	const y = new TextEncoder().encode(b);
	let diff = x.length ^ y.length;
	for (let i = 0; i < Math.max(x.length, y.length); i++) {
		diff |= (x[i] || 0) ^ (y[i] || 0);
	}
	return diff === 0;
}

function notFound() {
	return new Response("Not found", { status: 404 });
}

export default {
	async fetch(request, env, ctx) {
		const url = new URL(request.url);
		const expected = (env && env.AUTH) || "";
		if (expected.length < 16 ||
			!safeEqual(url.searchParams.get("auth") || "", expected) ||
			(request.headers.get("Upgrade") || "").toLowerCase() !== "websocket" ||
			url.pathname !== "/apiws") {
			return notFound();
		}

		const dst = url.searchParams.get("dst") || "";
		if (!(await isTelegram(dst, ctx))) {
			return notFound();
		}

		const pair = new WebSocketPair();
		const client = pair[0];
		const server = pair[1];
		server.accept();

		// Соединение с Telegram открываем ЛЕНИВО — при первых данных от tg-ws-proxy.
		// tg-ws-proxy держит «запасные» WebSocket'ы (пул) без дела до 100 с.
		// Если сразу соединяться с Telegram, он закрывает простаивающее соединение,
		// закрытие уходит в запасной WebSocket, который никто не читает, и Workers
		// записывает это как «code had hung». Без данных — без соединения.
		let socket = null;
		let tcpReader = null;
		let tcpWriter = null;
		let finished = false;

		// Закрыть всё и РАЗБЛОКИРОВАТЬ все ожидания: незавершённое чтение
		// отменяется (cancel), незавершённая запись прерывается (abort).
		// Если оставить их висеть на мёртвом сокете, Workers пишет «code had hung».
		const closeAll = (code) => {
			if (finished) return;
			finished = true;
			try { server.close(code); } catch {}
			if (tcpReader) { try { tcpReader.cancel().catch(() => {}); } catch {} }
			if (tcpWriter) { try { tcpWriter.abort().catch(() => {}); } catch {} }
			try { if (socket) socket.close(); } catch {}
		};

		const openTelegram = () => {
			socket = connect({ hostname: dst, port: 443 });
			if (socket.opened) socket.opened.catch(() => closeAll(1011));
			if (socket.closed) socket.closed.catch(() => {});

			tcpReader = socket.readable.getReader();
			tcpWriter = socket.writable.getWriter();
			// При обрыве сети эти служебные промисы отклоняются; обрыв
			// обрабатывается в цикле чтения ниже.
			if (tcpReader.closed) tcpReader.closed.catch(() => {});
			if (tcpWriter.closed) tcpWriter.closed.catch(() => {});
			if (tcpWriter.ready) tcpWriter.ready.catch(() => {});

			(async () => {
				let code = 1000;
				try {
					while (!finished) {
						const { value, done } = await tcpReader.read();
						if (done) break;
						if (value) server.send(value);
					}
				} catch {
					code = 1011;   // Telegram оборвал соединение
				} finally {
					closeAll(code);
				}
			})();
		};

		// Сохраняем порядок: каждое сообщение пишется после предыдущего.
		let chain = Promise.resolve();
		server.addEventListener("message", (event) => {
			chain = chain.then(async () => {
				if (finished) return;
				try {
					if (!socket) openTelegram();
					await tcpWriter.write(await toBytes(event.data));
				} catch {
					closeAll(1011);
				}
			});
		});

		// tg-ws-proxy закрыл WebSocket (или связь с ним пропала): сразу
		// закрываем всё, ничего не дожидаясь.
		server.addEventListener("close", () => closeAll(1000));
		server.addEventListener("error", () => closeAll(1011));

		return new Response(null, { status: 101, webSocket: client });
	},
};
