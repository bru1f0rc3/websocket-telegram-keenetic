#!/bin/sh
### Установка tg-ws-proxy на Keenetic с Entware
### Запускать через SSH на роутере

set -e

ARCH=""
case "$(uname -m)" in
    mips)     ARCH="keenetic_mips" ;;
    mipsel)   ARCH="keenetic_mipsel" ;;
    aarch64)  ARCH="keenetic_aarch64" ;;
    armv7l)   ARCH="entware_armv7" ;;
    *)        echo "Unsupported architecture: $(uname -m)"; exit 1 ;;
esac

BINARY="tg-ws-proxy_${ARCH}"
INSTALL_DIR="/opt/bin"
CONFIG_DIR="/opt/etc/tg-ws-proxy"
INITD_DIR="/opt/etc/init.d"

echo "=== TG WS Proxy installer for Keenetic/Entware ==="
echo "Architecture: $(uname -m) -> ${ARCH}"
echo ""

# Check if binary exists in current directory
if [ ! -f "./${BINARY}" ]; then
    echo "Error: Binary ./${BINARY} not found!"
    echo "Please place the correct binary in current directory."
    echo ""
    echo "Available architectures:"
    echo "  tg-ws-proxy_keenetic_mipsel   - Viva, Omni, Extra, Giga, Ultra, Giant, Hero 4G, Hopper (KN-3810)"
    echo "  tg-ws-proxy_keenetic_mips     - Ultra SE, Giga SE, DSL, Skipper DSL, Duo, Hopper DSL"
    echo "  tg-ws-proxy_keenetic_aarch64  - Peak, Ultra (KN-1811), Giga (KN-1012), Hopper (KN-3811/3812)"
    echo "  tg-ws-proxy_entware_armv7     - Прочие роутеры с Entware"
    exit 1
fi

# Install binary
echo "Installing binary to ${INSTALL_DIR}..."
cp "./${BINARY}" "${INSTALL_DIR}/tg-ws-proxy"
chmod +x "${INSTALL_DIR}/tg-ws-proxy"

# Create config
if [ ! -f "${CONFIG_DIR}/config.json" ]; then
    echo "Creating config directory ${CONFIG_DIR}..."
    mkdir -p "${CONFIG_DIR}"
    cat > "${CONFIG_DIR}/config.json" << 'EOF'
{
  "host": "0.0.0.0",
  "port": 1080,
  "dc_ip": [
    "2:149.154.167.220",
    "4:149.154.167.220"
  ],
  "verbose": false,
  "pool_size": 2,
  "buf_kb": 128,
  "log_file": "/opt/var/log/tg-ws-proxy.log"
}
EOF
    echo "Config created at ${CONFIG_DIR}/config.json"
else
    echo "Config already exists, skipping."
fi

# Install init.d script
echo "Installing init.d script..."
cat > "${INITD_DIR}/S99tgwsproxy" << 'EOF'
#!/bin/sh

ENABLED=yes
PROCS=tg-ws-proxy
ARGS="--config /opt/etc/tg-ws-proxy/config.json"
PREARGS=""
DESC="Telegram WebSocket Proxy"
PATH=/opt/sbin:/opt/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

. /opt/etc/init.d/rc.func
EOF
chmod +x "${INITD_DIR}/S99tgwsproxy"

echo ""
echo "=== Installation complete ==="
echo ""
echo "Configuration: ${CONFIG_DIR}/config.json"
echo "  Edit host to 0.0.0.0 to allow LAN access"
echo ""
echo "Start service:"
echo "  ${INITD_DIR}/S99tgwsproxy start"
echo ""
echo "Stop service:"
echo "  ${INITD_DIR}/S99tgwsproxy stop"
echo ""
echo "In Telegram Desktop set SOCKS5 proxy:"
echo "  Server: <router_ip>"
echo "  Port:   1080"
echo "  Login/Password: empty"
echo ""
