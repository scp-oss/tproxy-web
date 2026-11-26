// Cloudflare Worker для tg-ws-proxy — ОРИГИНАЛЬНАЯ логика соединения
// из Flowseal/tg-ws-proxy (docs/RU/CfWorker.md) без изменений.
// Добавлены только две защиты:
//  1. ключ: ?auth=… должен совпасть с секретом AUTH воркера
//     (Settings → Variables and Secrets → тип Secret, имя AUTH);
//  2. соединения только с сетями Telegram.
// Неподходящий запрос — одинаковый 404.
//
// Отличие от worker.js: соединение с Telegram открывается сразу, и закрытия
// обрабатываются как в оригинале. Работает так же, но в логах Cloudflare
// возможны ошибки «code had hung», «Network connection lost» и исключения
// на завершении соединений — как и у оригинального кода.

import { connect } from "cloudflare:sockets";

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

const NETS = TELEGRAM_NETS.map((cidr) => {
	const [base, bits] = cidr.split("/");
	const size = 2 ** (32 - Number(bits));
	const start = Math.floor(ipv4ToInt(base) / size) * size;
	return [start, start + size];
});

function isTelegram(ip) {
	const value = ipv4ToInt(ip);
	if (value === null) return false;
	return NETS.some(([start, end]) => value >= start && value < end);
}

function safeEqual(a, b) {
	const x = new TextEncoder().encode(a);
	const y = new TextEncoder().encode(b);
	let diff = x.length ^ y.length;
	for (let i = 0; i < Math.max(x.length, y.length); i++) {
		diff |= (x[i] || 0) ^ (y[i] || 0);
	}
	return diff === 0;
}

function toBytes(data) {
	if (data instanceof ArrayBuffer) {
		return new Uint8Array(data);
	}
	if (typeof data === "string") {
		return new TextEncoder().encode(data);
	}
	if (data && typeof data.arrayBuffer === "function") {
		return data.arrayBuffer().then((ab) => new Uint8Array(ab));
	}
	return new Uint8Array();
}

export default {
	async fetch(request, env) {
		const url = new URL(request.url);
		const expected = (env && env.AUTH) || "";
		if (expected.length < 16 ||
			!safeEqual(url.searchParams.get("auth") || "", expected) ||
			(request.headers.get("Upgrade") || "").toLowerCase() !== "websocket" ||
			url.pathname !== "/apiws") {
			return new Response("Not found", { status: 404 });
		}

		const dst = url.searchParams.get("dst");
		if (!dst || !isTelegram(dst)) {
			return new Response("Not found", { status: 404 });
		}

		// ---- дальше без изменений, как в оригинале ----
		const pair = new WebSocketPair();
		const client = pair[0];
		const server = pair[1];
		server.accept();

		const socket = connect({ hostname: dst, port: 443 });
		const tcpReader = socket.readable.getReader();
		const tcpWriter = socket.writable.getWriter();

		server.addEventListener("message", async (event) => {
			try {
				await tcpWriter.write(await toBytes(event.data));
			} catch {
				try {
					server.close(1011, "tcp write failed");
				} catch {}
			}
		});

		server.addEventListener("close", async () => {
			try {
				await tcpWriter.close();
			} catch {}
			try {
				socket.close();
			} catch {}
		});

		(async () => {
			try {
				while (true) {
					const { value, done } = await tcpReader.read();
					if (done) {
						break;
					}
					if (value) {
						server.send(value);
					}
				}
			} catch {
			} finally {
				try {
					server.close();
				} catch {}
				try {
					tcpReader.releaseLock();
				} catch {}
				try {
					socket.close();
				} catch {}
			}
		})();

		return new Response(null, { status: 101, webSocket: client });
	},
};
