"""Добавляет ключ доступа к запросам tg-ws-proxy в Cloudflare Worker.

tg-ws-proxy обращается к воркеру как wss://<домен>/apiws?dst=<IP>&dc=<N>.
Патч добавляет в эту строку параметр auth из переменной окружения
TG_WS_PROXY_CF_WORKER_AUTH. Воркер без правильного auth отвечает 404.

Если в новой версии tg-ws-proxy код поменяется и место вставки не найдётся,
сборка образа упадёт — тихо собрать образ без ключа не получится.
"""
import pathlib
import sys

ANCHOR = "'dc': str(dc),"
INSERT = "'auth': __import__('os').environ.get('TG_WS_PROXY_CF_WORKER_AUTH', ''),"
FILES = ["proxy/bridge.py", "proxy/pool.py"]

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".")
for name in FILES:
    path = root / name
    text = path.read_text(encoding="utf-8")
    if INSERT in text:
        print(f"{name}: уже пропатчен")
        continue
    count = text.count(ANCHOR)
    if count != 1:
        sys.exit(f"{name}: ожидалось 1 место вставки, найдено {count} — проверьте версию tg-ws-proxy")
    lines = text.split("\n")
    for index, line in enumerate(lines):
        if ANCHOR in line:
            indent = line[: len(line) - len(line.lstrip())]
            lines.insert(index + 1, indent + INSERT)
            break
    path.write_text("\n".join(lines), encoding="utf-8")
    print(f"{name}: добавлен auth")
