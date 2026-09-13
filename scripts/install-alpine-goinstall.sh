#!/bin/sh
# Install pty-mcp on Alpine by `go install`-ing it straight from GitHub, then
# register it with OpenRC. RUN AS ROOT ON THE TARGET HOST:
#
#   doas sh install-alpine-goinstall.sh [version] [http-addr]
#
# Or fetch and run it directly. Fresh Alpine ships busybox wget (not curl),
# so the wget form is the one that works out of the box:
#
#   wget -qO- https://raw.githubusercontent.com/schaepher/pty-mcp/main/scripts/install-alpine-goinstall.sh | doas sh
#
# If curl is available (or installed first):
#
#   curl -fsSL <same-url> | doas sh
#
# Defaults: version=v1.0.0, http-addr=0.0.0.0:8765
#
# What it does:
#   1. apk add bash
#   2. installs the Go toolchain from the golang.google.cn tarball
#   3. CGO_ENABLED=0 GOBIN=/usr/local/bin go install <module>@<version>
#      (ai-tmux is a separate main package and installed too, best-effort)
#   4. writes /etc/init.d/pty-mcp + /etc/conf.d/pty-mcp with a bearer token
#   5. enables it at boot, starts it, health-checks /mcp, prints the token
#
# Env overrides:
#   GO_VERSION  (1.27.1)      GOPROXY  (goproxy.cn,goproxy.io,direct)
#   PTY_MCP_USER (mcp)        ROTATE_TOKEN (0)
#
# Note on GOPROXY: goproxy.cn may 404 on a freshly pushed tag; listing
# goproxy.io as a fallback makes `go install <module>@<new-tag>` resolve.
set -eu

VERSION="${1:-v1.0.0}"
HTTP_ADDR="${2:-0.0.0.0:8765}"
RUN_USER="${PTY_MCP_USER:-mcp}"
GO_VERSION="${GO_VERSION:-1.27.1}"
GOPROXY="${GOPROXY:-https://goproxy.cn,https://goproxy.io,direct}"
MODULE="github.com/schaepher/pty-mcp"

[ "$(id -u)" = 0 ] || { echo "error: must run as root (doas sh install-alpine-goinstall.sh)" >&2; exit 1; }

case "$(uname -m)" in
    x86_64|amd64)  GO_ARCH=amd64 ;;
    aarch64|arm64) GO_ARCH=arm64 ;;
    *) echo "error: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

echo "==> apk add bash"
apk add --no-cache bash

# --- Go toolchain ----------------------------------------------------------
echo "==> install Go ${GO_VERSION} (/usr/local/go)"
if [ "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')" = "go${GO_VERSION}" ]; then
    echo "    already present: $(/usr/local/go/bin/go version)"
else
    TARBALL="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
    wget -q -O "/tmp/${TARBALL}" "https://golang.google.cn/dl/${TARBALL}"
    if [ "$(head -c 2 "/tmp/${TARBALL}" | od -An -tx1 | tr -d ' ')" != "1f8b" ]; then
        echo "error: ${TARBALL} is not a gzip archive" >&2
        rm -f "/tmp/${TARBALL}"
        exit 1
    fi
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${TARBALL}"
    rm -f "/tmp/${TARBALL}"
fi
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
printf 'export PATH="$PATH:/usr/local/go/bin"\n' > /etc/profile.d/go.sh
chmod 644 /etc/profile.d/go.sh
/usr/local/go/bin/go version

# --- go install from GitHub ------------------------------------------------
echo "==> go install ${MODULE}@${VERSION}"
export GOPROXY
CGO_ENABLED=0 GOBIN=/usr/local/bin go install "${MODULE}@${VERSION}"
CGO_ENABLED=0 GOBIN=/usr/local/bin go install "${MODULE}/cmd/ai-tmux@${VERSION}" \
    || echo "    warning: ai-tmux install failed (optional)"
/usr/local/bin/pty-mcp --version

# --- bearer token (preserved across re-runs) -------------------------------
CONF=/etc/conf.d/pty-mcp
if [ -f "$CONF" ] && [ "${ROTATE_TOKEN:-0}" != "1" ] && grep -q '^PTY_MCP_HTTP_TOKEN=' "$CONF"; then
    TOKEN="$(sed -n 's/^PTY_MCP_HTTP_TOKEN="\(.*\)"$/\1/p' "$CONF")"
    echo "==> bearer token: preserving existing (ROTATE_TOKEN=1 to rotate)"
else
    TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    echo "==> bearer token: generated"
fi

# --- OpenRC service --------------------------------------------------------
echo "==> install OpenRC service"
cat > /etc/init.d/pty-mcp <<'INITD'
#!/sbin/openrc-run
name="pty-mcp"
description="pty-mcp MCP server (Streamable HTTP)"

: "${PTY_MCP_HTTP_ADDR:=0.0.0.0:8765}"
: "${PTY_MCP_HTTP_TOKEN:=}"
: "${PTY_MCP_USER:=mcp}"

command="/usr/local/bin/pty-mcp"
command_args="--http-addr ${PTY_MCP_HTTP_ADDR}"
command_user="${PTY_MCP_USER}"
command_background="yes"
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/${RC_SVCNAME}.log"
error_log="/var/log/${RC_SVCNAME}.log"

# Token is passed via the environment so it never shows up in `ps`.
export PTY_MCP_HTTP_TOKEN

depend() {
    need net
}

start_pre() {
    checkpath --file --owner "${PTY_MCP_USER}" --mode 0640 "${output_log}"
}
INITD
chmod 755 /etc/init.d/pty-mcp

umask 077
cat > "$CONF" <<EOF
PTY_MCP_HTTP_ADDR="$HTTP_ADDR"
PTY_MCP_HTTP_TOKEN="$TOKEN"
PTY_MCP_USER="$RUN_USER"
EOF
chmod 600 "$CONF"

echo "==> enable at boot + start"
rc-update add pty-mcp default >/dev/null
rc-service pty-mcp restart >/dev/null 2>&1 || rc-service pty-mcp start >/dev/null
sleep 1

# --- health check (must send the token) ------------------------------------
PORT="${HTTP_ADDR##*:}"
echo "==> health check"
if wget -qO- --header="Authorization: Bearer ${TOKEN}" \
        --header='Content-Type: application/json' \
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
