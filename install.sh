#!/bin/sh
# KVM Fleet — Universal agent installer
#
# Usage:
#   curl -sSL https://install.kvmfleet.io/agent | sh -s -- --token <enrollment-token>
#
# Or with env var:
#   KVMFLEET_TOKEN=ekvent_xxx curl -sSL https://install.kvmfleet.io/agent | sh
#
# Supports:
#   - JetKVM  (BusyBox Linux, armv7l, /etc/init.d/)
#   - PiKVM   (Arch Linux, aarch64, systemd)
#   - Generic Linux (systemd, x86_64/aarch64/armv7l)
#
# The script is idempotent — safe to re-run. It will not re-enroll if the
# agent is already registered (state file exists).
set -eu

PLATFORM_URL="${KVMFLEET_API:-https://app.kvmfleet.io}"
DOWNLOAD_BASE="${KVMFLEET_DOWNLOAD_URL:-${PLATFORM_URL}/downloads}"
VERSION="${KVMFLEET_AGENT_VERSION:-latest}"

# --- Parse args -----------------------------------------------------------

TOKEN="${KVMFLEET_TOKEN:-}"
DEVICE_NAME="${KVMFLEET_DEVICE_NAME:-}"
DEVICE_TAGS="${KVMFLEET_DEVICE_TAGS:-}"
KVMD_USER="${KVMFLEET_KVMD_USER:-admin}"
KVMD_PASS="${KVMFLEET_KVMD_PASS:-}"

while [ $# -gt 0 ]; do
    case "$1" in
        --token)     TOKEN="$2";       shift 2 ;;
        --name)      DEVICE_NAME="$2"; shift 2 ;;
        --tags)      DEVICE_TAGS="$2"; shift 2 ;;
        --api)       PLATFORM_URL="$2"; shift 2 ;;
        --kvmd-user) KVMD_USER="$2";   shift 2 ;;
        --kvmd-pass) KVMD_PASS="$2";   shift 2 ;;
        --help|-h)
            echo "Usage: curl -sSL https://app.kvmfleet.io/install | sh -s -- --token <token> --kvmd-pass <pass>"
            echo ""
            echo "Options:"
            echo "  --token <token>      Enrollment token (required, or set KVMFLEET_TOKEN)"
            echo "  --name <name>        Device name (optional)"
            echo "  --tags <t1,t2>       Comma-separated tags (optional)"
            echo "  --api <url>          Platform URL (default: https://app.kvmfleet.io)"
            echo "  --kvmd-user <user>   Local kvmd username (default: admin)"
            echo "  --kvmd-pass <pass>   Local kvmd password (required for PiKVM, or set KVMFLEET_KVMD_PASS)"
            exit 0 ;;
        *) echo "unknown arg: $1"; exit 1 ;;
    esac
done

if [ -z "$TOKEN" ]; then
    echo "ERROR: enrollment token required."
    echo "  --token <token>  or  KVMFLEET_TOKEN=<token>"
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

echo "=== KVM Fleet Agent Installer ==="
echo "  Platform:    $PLATFORM_URL"
echo "  Arch:        $ARCH → $BINARY_ARCH"
echo "  Init system: $INIT_SYSTEM"
echo "  Device type: $DEVICE_TYPE"
echo ""

# --- Paths (vary by device type) -------------------------------------------

case "$DEVICE_TYPE" in
    jetkvm)
        INSTALL_DIR="/userdata/kvmfleet"
        AGENT_BIN="$INSTALL_DIR/agent"
        STATE_FILE="$INSTALL_DIR/state.json"
        TOKEN_FILE="$INSTALL_DIR/enrollment.token"
        ;;
    pikvm)
        INSTALL_DIR="/var/lib/kvmfleet"
        AGENT_BIN="/usr/local/bin/kvmfleet-agent"
        STATE_FILE="$INSTALL_DIR/state.json"
        TOKEN_FILE="$INSTALL_DIR/enrollment.token"
        ;;
    *)
        INSTALL_DIR="/var/lib/kvmfleet"
        AGENT_BIN="/usr/local/bin/kvmfleet-agent"
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
        systemd)  systemctl start kvmfleet-agent 2>/dev/null || true ;;
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

DOWNLOAD_URL="${DOWNLOAD_BASE}/kvmfleet-agent.linux-${BINARY_ARCH}"
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

# kvmd password is required on devices that proxy through a local kvmd
# (PiKVM, JetKVM). Generic Linux installs without kvmd skip this.
case "$DEVICE_TYPE" in
    pikvm|jetkvm)
        if [ -z "$KVMD_PASS" ]; then
            echo "ERROR: this device proxies through a local kvmd that requires a password."
            echo "Either:"
            echo "  1. Pass it on the command line:"
            echo "       curl -sSL https://app.kvmfleet.io/install | sh -s -- \\"
            echo "         --token $TOKEN --kvmd-pass <your-kvmd-password>"
            echo "  2. Or set the env var before piping:"
            echo "       KVMFLEET_TOKEN=... KVMFLEET_KVMD_PASS=<pass> \\"
            echo "         curl -sSL https://app.kvmfleet.io/install | sh"
            echo ""
            echo "If you haven't set a kvmd password yet, run on the device:"
            echo "       kvmd-htpasswd set admin"
            echo "(the default 'admin' password is rejected by the agent for safety)."
            exit 1
        fi
        ;;
esac

# Persist kvmd credentials to a 0600-mode env file separate from the unit
# file so they aren't world-readable in /etc/systemd/system/.
KVMD_ENV_DIR="/etc/kvmfleet"
KVMD_ENV_FILE="$KVMD_ENV_DIR/agent.env"
if [ -n "$KVMD_PASS" ]; then
    mkdir -p "$KVMD_ENV_DIR"
    cat > "$KVMD_ENV_FILE" <<KVMDENV
KVMFLEET_KVMD_USER=$KVMD_USER
KVMFLEET_KVMD_PASS=$KVMD_PASS
KVMDENV
    chmod 600 "$KVMD_ENV_FILE"
fi

# --- Set up service ---------------------------------------------------------

case "$INIT_SYSTEM" in
    systemd)
        cat > /etc/systemd/system/kvmfleet-agent.service <<UNIT
[Unit]
Description=KVM Fleet Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$AGENT_BIN run
Restart=always
RestartSec=5
Environment=KVMFLEET_API=$PLATFORM_URL
Environment=KVMFLEET_TOKEN_FILE=$TOKEN_FILE
Environment=KVMFLEET_STATE=$STATE_FILE
Environment=KVMFLEET_DEVICE_NAME=$DEVICE_NAME
Environment=KVMFLEET_DEVICE_TAGS=$DEVICE_TAGS
Environment=KVMFLEET_KVMD_URL=$KVMD_URL
Environment=KVMFLEET_CONSOLE_ADDR=off
EnvironmentFile=-$KVMD_ENV_FILE

[Install]
WantedBy=multi-user.target
UNIT
        systemctl daemon-reload
        systemctl enable kvmfleet-agent
        systemctl start kvmfleet-agent
        echo "Service installed: systemctl status kvmfleet-agent"
        ;;

    busybox)
        INIT_SCRIPT="$INSTALL_DIR/init.sh"
        cat > "$INIT_SCRIPT" <<INITSCRIPT
#!/bin/sh
# KVM Fleet agent init script for BusyBox
PIDFILE="/var/run/kvmfleet-agent.pid"
AGENT="$AGENT_BIN"

export KVMFLEET_API="$PLATFORM_URL"
export KVMFLEET_TOKEN_FILE="$TOKEN_FILE"
export KVMFLEET_STATE="$STATE_FILE"
export KVMFLEET_DEVICE_NAME="$DEVICE_NAME"
export KVMFLEET_DEVICE_TAGS="$DEVICE_TAGS"
export KVMFLEET_KVMD_URL="$KVMD_URL"
export KVMFLEET_CONSOLE_ADDR="off"
[ -f "$KVMD_ENV_FILE" ] && . "$KVMD_ENV_FILE" && export KVMFLEET_KVMD_USER KVMFLEET_KVMD_PASS

case "\$1" in
    start)
        echo "Starting kvmfleet-agent..."
        start-stop-daemon -S -b -m -p "\$PIDFILE" -x "\$AGENT" -- run
        ;;
    stop)
        echo "Stopping kvmfleet-agent..."
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
        ln -sf "$INIT_SCRIPT" /etc/init.d/S99kvmfleet 2>/dev/null || true

        "$INIT_SCRIPT" start
        echo "Service installed: $INIT_SCRIPT {start|stop|restart}"
        ;;

    *)
        echo "WARNING: unknown init system. Starting agent in foreground."
        echo "You may want to set up a service manually."
        KVMFLEET_API="$PLATFORM_URL" \
        KVMFLEET_TOKEN_FILE="$TOKEN_FILE" \
        KVMFLEET_STATE="$STATE_FILE" \
        KVMFLEET_DEVICE_NAME="$DEVICE_NAME" \
        KVMFLEET_DEVICE_TAGS="$DEVICE_TAGS" \
        KVMFLEET_KVMD_URL="$KVMD_URL" \
        KVMFLEET_CONSOLE_ADDR="off" \
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
    systemd) echo "  journalctl -u kvmfleet-agent -f" ;;
    busybox) echo "  cat /var/log/messages | grep kvmfleet" ;;
esac
echo ""
echo "If enrollment fails, verify:"
echo "  1. The token hasn't expired (30 min TTL)"
echo "  2. The device can reach $PLATFORM_URL"
echo "  3. DNS resolves correctly"
