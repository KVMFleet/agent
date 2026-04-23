#!/bin/sh
# EuroKVM Fleet — Universal agent installer
#
# Usage:
#   curl -sSL https://install.eurokvm.io/agent | sh -s -- --token <enrollment-token>
#
# Or with env var:
#   EUROKVM_TOKEN=ekvent_xxx curl -sSL https://install.eurokvm.io/agent | sh
#
# Supports:
#   - JetKVM  (BusyBox Linux, armv7l, /etc/init.d/)
#   - PiKVM   (Arch Linux, aarch64, systemd)
#   - Generic Linux (systemd, x86_64/aarch64/armv7l)
#
# The script is idempotent — safe to re-run. It will not re-enroll if the
# agent is already registered (state file exists).
set -eu

PLATFORM_URL="${EUROKVM_API:-https://app.eurokvm.io}"
DOWNLOAD_BASE="${EUROKVM_DOWNLOAD_URL:-${PLATFORM_URL}/downloads}"
VERSION="${EUROKVM_AGENT_VERSION:-latest}"

# --- Parse args -----------------------------------------------------------

TOKEN="${EUROKVM_TOKEN:-}"
DEVICE_NAME="${EUROKVM_DEVICE_NAME:-}"
DEVICE_TAGS="${EUROKVM_DEVICE_TAGS:-}"

while [ $# -gt 0 ]; do
    case "$1" in
        --token)   TOKEN="$2";       shift 2 ;;
        --name)    DEVICE_NAME="$2"; shift 2 ;;
        --tags)    DEVICE_TAGS="$2"; shift 2 ;;
        --api)     PLATFORM_URL="$2"; shift 2 ;;
        --help|-h)
            echo "Usage: curl -sSL https://install.eurokvm.io/agent | sh -s -- --token <token>"
            echo ""
            echo "Options:"
            echo "  --token <token>   Enrollment token (required, or set EUROKVM_TOKEN)"
            echo "  --name <name>     Device name (optional)"
            echo "  --tags <t1,t2>    Comma-separated tags (optional)"
            echo "  --api <url>       Platform URL (default: https://app.eurokvm.io)"
            exit 0 ;;
        *) echo "unknown arg: $1"; exit 1 ;;
    esac
done

if [ -z "$TOKEN" ]; then
    echo "ERROR: enrollment token required."
    echo "  --token <token>  or  EUROKVM_TOKEN=<token>"
    exit 1
fi

# --- Detect environment ----------------------------------------------------

ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)   BINARY_ARCH="amd64" ;;
    aarch64|arm64)   BINARY_ARCH="arm64" ;;
    armv7l|armhf)    BINARY_ARCH="arm" ;;
    *)               echo "ERROR: unsupported architecture: $ARCH"; exit 1 ;;
esac

detect_init_system() {
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        echo "systemd"
    elif [ -d /etc/init.d ]; then
        echo "busybox"
    else
        echo "unknown"
    fi
}

detect_device_type() {
    if [ -f /userdata/kvm_config.json ]; then
        echo "jetkvm"
    elif command -v kvmd >/dev/null 2>&1 || [ -f /etc/kvmd/main.yaml ]; then
        echo "pikvm"
    else
        echo "generic"
    fi
}

INIT_SYSTEM=$(detect_init_system)
DEVICE_TYPE=$(detect_device_type)

echo "=== EuroKVM Agent Installer ==="
echo "  Platform:    $PLATFORM_URL"
echo "  Arch:        $ARCH → $BINARY_ARCH"
echo "  Init system: $INIT_SYSTEM"
echo "  Device type: $DEVICE_TYPE"
echo ""

# --- Paths (vary by device type) -------------------------------------------

case "$DEVICE_TYPE" in
    jetkvm)
        INSTALL_DIR="/userdata/eurokvm"
        AGENT_BIN="$INSTALL_DIR/agent"
        STATE_FILE="$INSTALL_DIR/state.json"
        TOKEN_FILE="$INSTALL_DIR/enrollment.token"
        ;;
    pikvm)
        INSTALL_DIR="/var/lib/eurokvm"
        AGENT_BIN="/usr/local/bin/eurokvm-agent"
        STATE_FILE="$INSTALL_DIR/state.json"
        TOKEN_FILE="$INSTALL_DIR/enrollment.token"
        ;;
    *)
        INSTALL_DIR="/var/lib/eurokvm"
        AGENT_BIN="/usr/local/bin/eurokvm-agent"
        STATE_FILE="$INSTALL_DIR/state.json"
        TOKEN_FILE="$INSTALL_DIR/enrollment.token"
        ;;
esac

# --- Check if already enrolled ---------------------------------------------

if [ -f "$STATE_FILE" ]; then
    echo "Agent already enrolled (state file exists at $STATE_FILE)."
    echo "To re-enroll, remove $STATE_FILE and run again."
    echo "Ensuring service is running..."
    case "$INIT_SYSTEM" in
        systemd)  systemctl start eurokvm-agent 2>/dev/null || true ;;
        busybox)  "$INSTALL_DIR/init.sh" start 2>/dev/null || true ;;
    esac
    exit 0
fi

# --- PiKVM: ensure filesystem is writable -----------------------------------

if [ "$DEVICE_TYPE" = "pikvm" ]; then
    if mount | grep "on / " | grep -q "ro,\|ro)"; then
        echo "PiKVM filesystem is read-only. Making writable..."
        mount -o remount,rw / 2>/dev/null || true
        PIKVM_REMOUNT_RO=1
    fi
fi

# --- Download agent binary --------------------------------------------------

mkdir -p "$INSTALL_DIR"

DOWNLOAD_URL="${DOWNLOAD_BASE}/eurokvm-agent.linux-${BINARY_ARCH}"
echo "Downloading agent from $DOWNLOAD_URL ..."

if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$DOWNLOAD_URL" -o "$AGENT_BIN" || {
        echo "ERROR: download failed. Check URL and network."
        exit 1
    }
elif command -v wget >/dev/null 2>&1; then
    wget -qO "$AGENT_BIN" "$DOWNLOAD_URL" || {
        echo "ERROR: download failed."
        exit 1
    }
else
    echo "ERROR: neither curl nor wget available."
    exit 1
fi

chmod +x "$AGENT_BIN"
echo "Agent binary installed at $AGENT_BIN"

# --- Write enrollment token -------------------------------------------------

echo "$TOKEN" > "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE"

# --- Detect kvmd URL --------------------------------------------------------

KVMD_URL=""
case "$DEVICE_TYPE" in
    jetkvm)  KVMD_URL="http://127.0.0.1:80" ;;
    pikvm)   KVMD_URL="https://127.0.0.1" ;;
esac

# --- Set up service ---------------------------------------------------------

case "$INIT_SYSTEM" in
    systemd)
        cat > /etc/systemd/system/eurokvm-agent.service <<UNIT
[Unit]
Description=EuroKVM Fleet Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$AGENT_BIN run
Restart=always
RestartSec=5
Environment=EUROKVM_API=$PLATFORM_URL
Environment=EUROKVM_TOKEN_FILE=$TOKEN_FILE
Environment=EUROKVM_STATE=$STATE_FILE
Environment=EUROKVM_DEVICE_NAME=$DEVICE_NAME
Environment=EUROKVM_DEVICE_TAGS=$DEVICE_TAGS
Environment=EUROKVM_KVMD_URL=$KVMD_URL
Environment=EUROKVM_CONSOLE_ADDR=off

[Install]
WantedBy=multi-user.target
UNIT
        systemctl daemon-reload
        systemctl enable eurokvm-agent
        systemctl start eurokvm-agent
        echo "Service installed: systemctl status eurokvm-agent"
        ;;

    busybox)
        INIT_SCRIPT="$INSTALL_DIR/init.sh"
        cat > "$INIT_SCRIPT" <<INITSCRIPT
#!/bin/sh
# EuroKVM agent init script for BusyBox
PIDFILE="/var/run/eurokvm-agent.pid"
AGENT="$AGENT_BIN"

export EUROKVM_API="$PLATFORM_URL"
export EUROKVM_TOKEN_FILE="$TOKEN_FILE"
export EUROKVM_STATE="$STATE_FILE"
export EUROKVM_DEVICE_NAME="$DEVICE_NAME"
export EUROKVM_DEVICE_TAGS="$DEVICE_TAGS"
export EUROKVM_KVMD_URL="$KVMD_URL"
export EUROKVM_CONSOLE_ADDR="off"

case "\$1" in
    start)
        echo "Starting eurokvm-agent..."
        start-stop-daemon -S -b -m -p "\$PIDFILE" -x "\$AGENT" -- run
        ;;
    stop)
        echo "Stopping eurokvm-agent..."
        start-stop-daemon -K -p "\$PIDFILE" 2>/dev/null
        rm -f "\$PIDFILE"
        ;;
    restart)
        \$0 stop
        sleep 1
        \$0 start
        ;;
    *)
        echo "Usage: \$0 {start|stop|restart}"
        exit 1
        ;;
esac
INITSCRIPT
        chmod +x "$INIT_SCRIPT"

        # Link into /etc/init.d/ for auto-start on boot
        ln -sf "$INIT_SCRIPT" /etc/init.d/S99eurokvm 2>/dev/null || true

        "$INIT_SCRIPT" start
        echo "Service installed: $INIT_SCRIPT {start|stop|restart}"
        ;;

    *)
        echo "WARNING: unknown init system. Starting agent in foreground."
        echo "You may want to set up a service manually."
        EUROKVM_API="$PLATFORM_URL" \
        EUROKVM_TOKEN_FILE="$TOKEN_FILE" \
        EUROKVM_STATE="$STATE_FILE" \
        EUROKVM_DEVICE_NAME="$DEVICE_NAME" \
        EUROKVM_DEVICE_TAGS="$DEVICE_TAGS" \
        EUROKVM_KVMD_URL="$KVMD_URL" \
        EUROKVM_CONSOLE_ADDR="off" \
            "$AGENT_BIN" run &
        ;;
esac

# --- PiKVM: remount read-only ----------------------------------------------

if [ "${PIKVM_REMOUNT_RO:-0}" = "1" ]; then
    mount -o remount,ro / 2>/dev/null || true
    echo "PiKVM filesystem remounted read-only."
fi

# --- Wait for enrollment ---------------------------------------------------

echo ""
echo "Waiting for agent to enroll..."
for i in 1 2 3 4 5 6 7 8 9 10; do
    if [ -f "$STATE_FILE" ]; then
        DEVICE_ID=$(cat "$STATE_FILE" | grep -o '"device_id":"[^"]*"' | head -1 | cut -d'"' -f4)
        echo ""
        echo "=== Enrolled successfully ==="
        echo "  Device ID:  $DEVICE_ID"
        echo "  Dashboard:  $PLATFORM_URL"
        echo "  State file: $STATE_FILE"
        echo ""
        echo "Your device should appear in the fleet dashboard within seconds."
        exit 0
    fi
    sleep 1
done

echo ""
echo "Agent started but enrollment not confirmed yet."
echo "Check logs:"
case "$INIT_SYSTEM" in
    systemd) echo "  journalctl -u eurokvm-agent -f" ;;
    busybox) echo "  cat /var/log/messages | grep eurokvm" ;;
esac
echo ""
echo "If enrollment fails, verify:"
echo "  1. The token hasn't expired (30 min TTL)"
echo "  2. The device can reach $PLATFORM_URL"
echo "  3. DNS resolves correctly"
