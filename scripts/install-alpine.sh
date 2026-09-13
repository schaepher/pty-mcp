#!/bin/sh
# One-shot pty-mcp + OpenRC setup for Alpine. RUN AS ROOT ON THE TARGET HOST.
#
# Usage:
#   doas sh install-alpine.sh <pty-mcp-bin> [<ai-tmux-bin>|-] [<http-addr>]
#
# Example:
#   doas sh install-alpine.sh /home/mcp/pty-mcp.bin /home/mcp/ai-tmux.bin 0.0.0.0:8765
#
# Expects the OpenRC init script next to this file (scripts/openrc/pty-mcp.initd),
# or at $INITD. Installs bash via apk, unpacks Go into /usr/local/go, configures
# the service with a random bearer token, enables it at boot, starts it, and
# health-checks the endpoint.
#
# Environment overrides:
#   GO_VERSION  (default 1.27.1)      GO_URL  (default golang.google.cn mirror)
#   GO_SHA256   (optional; verified with sha256sum when set)
#   PTY_MCP_USER (default mcp)
set -eu

BIN="${1:?usage: install-alpine.sh <pty-mcp-bin> [<ai-tmux-bin>|-] [<http-addr>]}"
AITMUX="${2:--}"
HTTP_ADDR="${3:-0.0.0.0:8765}"
RUN_USER="${PTY_MCP_USER:-mcp}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INITD="${INITD:-$SCRIPT_DIR/openrc/pty-mcp.initd}"

[ "$(id -u)" = "0" ] || { echo "error: must run as root (doas sh $0 ...)" >&2; exit 1; }
[ -f "$BIN" ] || { echo "error: pty-mcp binary not found: $BIN" >&2; exit 1; }
[ -f "$INITD" ] || { echo "error: init script not found: $INITD" >&2; exit 1; }

echo "==> apk add bash"
apk add --no-cache bash

# --- Go: official tarball into /usr/local/go -------------------------------
GO_VERSION="${GO_VERSION:-1.27.1}"
case "$(uname -m)" in
    x86_64|amd64)  GO_ARCH=amd64 ;;
    aarch64|arm64) GO_ARCH=arm64 ;;
    *) echo "error: unsupported architecture for Go: $(uname -m)" >&2; exit 1 ;;
esac
GO_TARBALL="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
GO_URL="${GO_URL:-https://golang.google.cn/dl/${GO_TARBALL}}"

echo "==> install Go ${GO_VERSION} (/usr/local/go)"
if [ "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')" = "go${GO_VERSION}" ]; then
    echo "    already present: $(/usr/local/go/bin/go version)"
else
    echo "    downloading ${GO_URL}"
    wget -q -O "/tmp/${GO_TARBALL}" "${GO_URL}"
    if [ -n "${GO_SHA256:-}" ]; then
        echo "    verifying sha256"
        echo "${GO_SHA256}  /tmp/${GO_TARBALL}" | sha256sum -c - || {
            echo "error: sha256 mismatch for ${GO_TARBALL}" >&2; rm -f "/tmp/${GO_TARBALL}"; exit 1
        }
    fi
    # validate gzip magic before clobbering an existing install
    if [ "$(head -c 2 "/tmp/${GO_TARBALL}" | od -An -tx1 | tr -d ' ')" != "1f8b" ]; then
        echo "error: ${GO_TARBALL} is not a gzip archive" >&2; rm -f "/tmp/${GO_TARBALL}"; exit 1
    fi
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
    rm -f "/tmp/${GO_TARBALL}"
fi
# Expose go/gofmt on the default PATH. Symlinks keep working when /usr/local/go
# is later replaced by a new tarball.
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
printf 'export PATH="$PATH:/usr/local/go/bin"\n' > /etc/profile.d/go.sh
chmod 644 /etc/profile.d/go.sh
/usr/local/go/bin/go version

echo "==> install pty-mcp"
install -m 0755 "$BIN" /usr/local/bin/pty-mcp
if [ "$AITMUX" != "-" ] && [ -f "$AITMUX" ]; then
    install -m 0755 "$AITMUX" /usr/local/bin/ai-tmux
fi

echo "==> bearer token"
CONF=/etc/conf.d/pty-mcp
if [ -f "$CONF" ] && [ "${ROTATE_TOKEN:-0}" != "1" ] && grep -q '^PTY_MCP_HTTP_TOKEN=' "$CONF"; then
    TOKEN="$(sed -n 's/^PTY_MCP_HTTP_TOKEN="\(.*\)"$/\1/p' "$CONF")"
    echo "    preserving existing token (set ROTATE_TOKEN=1 to rotate)"
else
    TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    echo "    generated new token"
fi

echo "==> install OpenRC service"
install -m 0755 "$INITD" /etc/init.d/pty-mcp
umask 077
cat > /etc/conf.d/pty-mcp <<EOF
PTY_MCP_HTTP_ADDR="$HTTP_ADDR"
PTY_MCP_HTTP_TOKEN="$TOKEN"
PTY_MCP_USER="$RUN_USER"
EOF
chmod 600 /etc/conf.d/pty-mcp

echo "==> enable at boot + start"
rc-update add pty-mcp default >/dev/null
rc-service pty-mcp restart >/dev/null 2>&1 || rc-service pty-mcp start >/dev/null

sleep 1
PORT="${HTTP_ADDR##*:}"
echo "==> health check"
if wget -qO- --header='Content-Type: application/json' \
        --post-data='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
        "http://127.0.0.1:${PORT}/mcp" 2>/dev/null | grep -q '"serverInfo"'; then
    echo "    OK: /mcp answered initialize"
else
    echo "    WARNING: no valid response; check: rc-service pty-mcp status; tail /var/log/pty-mcp.log" >&2
fi

echo
echo "pty-mcp MCP endpoint : http://<this-host>:${PORT}/mcp"
echo "Bearer token         : ${TOKEN}"
echo "Service              : rc-service pty-mcp {status,restart}"
