# WebSocket Proxy

## Что это и зачем

Эта штука поднимает локальный SOCKS5-прокси и перенаправляет трафик Telegram через WebSocket-соединения. Для провайдера это выглядит как обычный HTTPS-трафик, а не как "подозрительный" MTProto. Никаких сторонних серверов — данные идут напрямую к серверам Telegram, просто по другому маршруту.

```
Telegram Desktop → SOCKS5 (127.0.0.1:1080) → WS Proxy → WSS → Telegram DC
```

> [!NOTE]
> **Сделано для себя** — положил в репозиторий чтобы не потерять.

Написано на Go — один бинарник, без зависимостей, работает на Windows, Linux, macOS и роутерах. За основу взят [Flowseal/tg-ws-proxy](https://github.com/Flowseal/tg-ws-proxy) на Python.

## Что под капотом

- **WebSocket-мост** — MTProto трафик оборачивается в WS-фреймы через kws*.web.telegram.org
- **Пул соединений** — 8 pre-warmed WS-коннектов на DC, 16 для медиа. Моментальное переключение без задержки на TLS handshake
- **DNS-over-HTTPS** — 6 провайдеров (Cloudflare, Google, Quad9, AdGuard, ControlD, DNS.SB) с параллельным racing — кто первый ответил, тот и победил
- **TLS-фрагментация** — разбивка ClientHello для обхода DPI (на случай если провайдер начнёт фильтровать по SNI)
- **TCP fallback** — DC1/DC3/DC5 не поддерживают WS через доступные IP → автоматический откат на прямое TCP
- **Буферы 512 KB + word-level XOR** — оптимизировано под потоковую загрузку медиа

## Как поставить

### Windows

1. Скачайте `tg-ws-proxy.exe` из [Releases](../../releases) (или соберите: `go build -o tg-ws-proxy.exe ./cmd/tg-ws-proxy/`)
2. Запустите:
```powershell
.\tg-ws-proxy.exe
```
3. В Telegram Desktop: **Настройки → Продвинутые → Тип подключения → Прокси**
   - Тип: **SOCKS5**
   - Сервер: **127.0.0.1**
   - Порт: **1080**
   - Логин/пароль: пусто

### Linux / macOS

```bash
./tg-ws-proxy
```

### Как служба

```bash
# Windows
.\tg-ws-proxy.exe --service install
.\tg-ws-proxy.exe --service start

# Linux / macOS
sudo ./tg-ws-proxy --service install
sudo ./tg-ws-proxy --service start
```

## Настройки

Всё работает из коробки, но если хочется покрутить:

```bash
# Другой порт
./tg-ws-proxy --port 9050

# Свои IP для DC
./tg-ws-proxy --dc-ip 2:149.154.167.220

# Подробные логи
./tg-ws-proxy -v

# Слушать на всех интерфейсах (для роутера/LAN)
./tg-ws-proxy --host 0.0.0.0

# Конкретный DoH провайдер вместо racing
./tg-ws-proxy --doh cloudflare

# Выключить TLS-фрагментацию
./tg-ws-proxy --tls-frag 0
```

Конфиг хранится в `config.json`:
- **Windows:** `%APPDATA%\TgWsProxy\config.json`
- **Linux:** `~/.config/TgWsProxy/config.json`
- **macOS:** `~/Library/Application Support/TgWsProxy/config.json`

## Сборка

Нужен Go 1.21+.

```bash
# Текущая платформа
make build

# Все платформы
make all-platforms

# Конкретная
make windows-amd64
make linux-amd64
make keenetic-mipsel
```

## Лицензия

MIT
