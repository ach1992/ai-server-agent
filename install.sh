#!/usr/bin/env bash
set -Eeuo pipefail

REPO="ach1992/ai-server-agent"
REF="${AI_SERVER_AGENT_REF:-main}"
AGENT_VERSION="${AI_SERVER_AGENT_VERSION:-source}"
GO_VERSION="1.26.5"
INSTALL_BIN="/usr/local/bin/ai-server-agent"
CONFIG_DIR="/etc/ai-server-agent"
STATE_DIR="/var/lib/ai-server-agent"
LOG_DIR="/var/log/ai-server-agent"
WORKSPACE_DIR="/srv/ai-workspace"
CONFIG_FILE="$CONFIG_DIR/config.json"
MCP_AUTH_HEADER_FILE="$CONFIG_DIR/mcp.authorization"
AGENT_USER="aiagent"
WORKER_USER="aiworker"
PORT="${AI_SERVER_AGENT_PORT:-3210}"
MODE="${AI_SERVER_AGENT_BIND_MODE:-local}"
TLS_CERT_FILE="${AI_SERVER_AGENT_TLS_CERT_FILE:-}"
TLS_KEY_FILE="${AI_SERVER_AGENT_TLS_KEY_FILE:-}"
NONINTERACTIVE="${AI_SERVER_AGENT_NONINTERACTIVE:-0}"
CHECK_ONLY=0
RESOLVE_REF_ONLY=0

trap 'echo "[ERROR] Installation failed at line $LINENO. Review the output above; existing project files were not intentionally modified." >&2' ERR

log(){ printf '[ai-server-agent] %s\n' "$*"; }
die(){ printf '[ai-server-agent] ERROR: %s\n' "$*" >&2; exit 1; }
need_root(){ [ "$(id -u)" -eq 0 ] || die "Run this installer as root (for example: sudo bash install.sh)."; }
version_ge(){ dpkg --compare-versions "$1" ge "$2"; }

urlencode_ref(){
  local input="$1" output="" ch hex i
  LC_ALL=C
  for ((i=0; i<${#input}; i++)); do
    ch="${input:i:1}"
    case "$ch" in
      [a-zA-Z0-9.~_-]) output+="$ch" ;;
      *) printf -v hex '%%%02X' "'$ch"; output+="$hex" ;;
    esac
  done
  printf '%s' "$output"
}

resolve_source_ref(){
  local ref="$1" encoded json sha
  if [[ "$ref" =~ ^[0-9a-fA-F]{40}$ ]]; then
    printf '%s\n' "${ref,,}"
    return 0
  fi
  command -v curl >/dev/null || die "curl is required to resolve source ref '$ref'"
  encoded="$(urlencode_ref "$ref")"
  json="$(curl -fsSL \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "https://api.github.com/repos/$REPO/commits/$encoded")" || die "Could not resolve GitHub source ref '$ref'"
  sha="$(printf '%s' "$json" | grep -oE '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]{40}"' | head -n1 | grep -oE '[0-9a-f]{40}' || true)"
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || die "GitHub source ref '$ref' did not resolve to a commit SHA"
  printf '%s\n' "$sha"
}

case "${1:-}" in
  --check) CHECK_ONLY=1 ;;
  --resolve-ref) RESOLVE_REF_ONLY=1 ;;
esac
need_root
[ -r /etc/os-release ] || die "/etc/os-release not found"
# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-}" in
  ubuntu) version_ge "${VERSION_ID:-0}" "22.04" || die "Ubuntu 22.04 or newer is required" ;;
  debian) version_ge "${VERSION_ID:-0}" "11" || die "Debian 11 or newer is required" ;;
  *) die "Unsupported OS: ${ID:-unknown}. Supported: Ubuntu 22.04+ and Debian 11+." ;;
esac
command -v systemctl >/dev/null || die "systemd/systemctl is required"
case "$(uname -m)" in x86_64) ARCH=amd64; GOARCH=amd64 ;; aarch64|arm64) ARCH=arm64; GOARCH=arm64 ;; *) die "Unsupported architecture: $(uname -m)" ;; esac

if [ "$RESOLVE_REF_ONLY" -eq 1 ]; then
  resolve_source_ref "$REF"
  exit 0
fi

if [ "$CHECK_ONLY" -eq 1 ]; then
  log "Compatibility check passed: $PRETTY_NAME, $ARCH, systemd available."
  exit 0
fi

if [ "$NONINTERACTIVE" != "1" ] && [ -r /dev/tty ]; then
  echo
  echo "AI Server Agent installs a minimal control plane for a dedicated AI-operated test server."
  echo "It does NOT install or reserve nginx, Apache, PHP, MySQL, Docker, Node.js, Python, ports 80/443, or a hosting panel."
  echo
  read -r -p "Bind mode [local/public] (recommended: local for Secure MCP Tunnel): " ans </dev/tty
  [ -n "$ans" ] && MODE="$ans"
  read -r -p "MCP port [$PORT]: " ans </dev/tty
  [ -n "$ans" ] && PORT="$ans"
  if [ "$MODE" = "public" ] && { [ -z "$TLS_CERT_FILE" ] || [ -z "$TLS_KEY_FILE" ]; }; then
    read -r -p "TLS certificate file (required for public mode): " TLS_CERT_FILE </dev/tty
    read -r -p "TLS private key file (required for public mode): " TLS_KEY_FILE </dev/tty
  fi
fi
case "$MODE" in local|public) ;; *) die "Bind mode must be local or public" ;; esac
[[ "$PORT" =~ ^[0-9]+$ ]] && [ "$PORT" -ge 1024 ] && [ "$PORT" -le 65535 ] || die "Port must be an integer between 1024 and 65535"
if [ -n "$TLS_CERT_FILE" ] || [ -n "$TLS_KEY_FILE" ]; then
  [ -n "$TLS_CERT_FILE" ] && [ -n "$TLS_KEY_FILE" ] || die "TLS certificate and private key must be configured together"
  [[ "$TLS_CERT_FILE" = /* ]] && [[ "$TLS_KEY_FILE" = /* ]] || die "TLS certificate and private key paths must be absolute"
  [ -r "$TLS_CERT_FILE" ] || die "TLS certificate is not readable: $TLS_CERT_FILE"
  [ -r "$TLS_KEY_FILE" ] || die "TLS private key is not readable: $TLS_KEY_FILE"
fi
if [ "$MODE" = "public" ] && { [ -z "$TLS_CERT_FILE" ] || [ -z "$TLS_KEY_FILE" ]; }; then
  die "Public mode requires native TLS. Set AI_SERVER_AGENT_TLS_CERT_FILE and AI_SERVER_AGENT_TLS_KEY_FILE."
fi

export DEBIAN_FRONTEND=noninteractive
log "Installing minimal installation utilities (ca-certificates, curl, tar, xz-utils)..."
apt-get update -qq
apt-get install -y -qq ca-certificates curl tar xz-utils >/dev/null

if ! getent group "$AGENT_USER" >/dev/null 2>&1; then groupadd --system "$AGENT_USER"; fi
if ! getent group "$WORKER_USER" >/dev/null 2>&1; then groupadd --system "$WORKER_USER"; fi
if ! id "$AGENT_USER" >/dev/null 2>&1; then useradd --system --gid "$AGENT_USER" --home "$STATE_DIR" --shell /usr/sbin/nologin "$AGENT_USER"; fi
if ! id "$WORKER_USER" >/dev/null 2>&1; then useradd --system --gid "$WORKER_USER" --create-home --home-dir "$WORKSPACE_DIR" --shell /bin/bash "$WORKER_USER"; fi

install -d -m 0750 -o root -g "$AGENT_USER" "$CONFIG_DIR"
install -d -m 0711 -o "$AGENT_USER" -g "$AGENT_USER" "$STATE_DIR"
install -d -m 2750 -o root -g "$AGENT_USER" "$LOG_DIR"
install -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$WORKSPACE_DIR"
install -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$STATE_DIR/runtime"
install -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$STATE_DIR/jobs"

random_hex(){ od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }
if [ ! -s "$CONFIG_DIR/executor.token" ]; then random_hex > "$CONFIG_DIR/executor.token"; fi
if [ ! -s "$CONFIG_DIR/mcp.token" ]; then random_hex > "$CONFIG_DIR/mcp.token"; fi
chown root:"$AGENT_USER" "$CONFIG_DIR"/*.token
chmod 0640 "$CONFIG_DIR"/*.token
printf 'Bearer %s\n' "$(cat "$CONFIG_DIR/mcp.token")" > "$MCP_AUTH_HEADER_FILE"
chown root:"$AGENT_USER" "$MCP_AUTH_HEADER_FILE"
chmod 0640 "$MCP_AUTH_HEADER_FILE"

build_from_source(){ (
  set -Eeuo pipefail
  local tmp go_url go_sha resolved_ref
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  resolved_ref="$(resolve_source_ref "$REF")"
  log "Resolved source ref '$REF' to commit '$resolved_ref'."
  log "Building a self-contained binary from immutable commit '$resolved_ref' using a temporary Go toolchain..."
  curl -fsSL \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "https://api.github.com/repos/$REPO/tarball/$resolved_ref" \
    -o "$tmp/src.tgz"
  mkdir "$tmp/src"; tar -xzf "$tmp/src.tgz" -C "$tmp/src" --strip-components=1
  case "$GOARCH" in
    amd64) go_sha="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053" ;;
    arm64) go_sha="fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49" ;;
  esac
  go_url="https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz"
  curl -fsSL "$go_url" -o "$tmp/go.tgz"
  printf '%s  %s\n' "$go_sha" "$tmp/go.tgz" | sha256sum -c - >/dev/null
  mkdir "$tmp/go"; tar -xzf "$tmp/go.tgz" -C "$tmp/go" --strip-components=1
  (cd "$tmp/src" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" "$tmp/go/bin/go" test ./... && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" "$tmp/go/bin/go" build -trimpath -ldflags='-s -w' -o "$tmp/ai-server-agent" ./cmd/ai-server-agent)
  install -m 0755 "$tmp/ai-server-agent" "$INSTALL_BIN"
); }

download_release(){ (
  set -Eeuo pipefail
  local tmp url asset bin
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  asset="ai-server-agent_${AGENT_VERSION#v}_linux_${ARCH}.tar.gz"
  url="https://github.com/$REPO/releases/download/$AGENT_VERSION"
  log "Downloading release $AGENT_VERSION ($ARCH)..."
  curl -fsSL "$url/$asset" -o "$tmp/$asset"
  curl -fsSL "$url/SHA256SUMS" -o "$tmp/SHA256SUMS"
  (cd "$tmp" && grep "  $asset$" SHA256SUMS | sha256sum -c -)
  tar -xzf "$tmp/$asset" -C "$tmp"
  bin="$(find "$tmp" -type f -name ai-server-agent -print -quit)"
  [ -n "$bin" ] || die "release archive does not contain ai-server-agent"
  install -m 0755 "$bin" "$INSTALL_BIN"
); }

if [ -n "${AI_SERVER_AGENT_BINARY:-}" ]; then
  [ -x "$AI_SERVER_AGENT_BINARY" ] || die "AI_SERVER_AGENT_BINARY is not executable"
  install -m 0755 "$AI_SERVER_AGENT_BINARY" "$INSTALL_BIN"
elif [ "$AGENT_VERSION" = "source" ]; then build_from_source; else download_release; fi

BIND="127.0.0.1:$PORT"; AUTH_MODE="bearer"; SCHEME="http"
if [ "$MODE" = "public" ]; then BIND="0.0.0.0:$PORT"; fi
if [ -n "$TLS_CERT_FILE" ]; then SCHEME="https"; fi
cat > "$CONFIG_FILE" <<JSON
{
  "listen_address": "$BIND",
  "mcp_path": "/mcp",
  "health_path": "/healthz",
  "auth_mode": "$AUTH_MODE",
  "bearer_token_file": "$CONFIG_DIR/mcp.token",
  "tls_cert_file": "$TLS_CERT_FILE",
  "tls_key_file": "$TLS_KEY_FILE",
  "executor_socket": "/run/ai-server-agent/executor.sock",
  "executor_token_file": "$CONFIG_DIR/executor.token",
  "state_dir": "$STATE_DIR",
  "log_dir": "$LOG_DIR",
  "workspace_dir": "$WORKSPACE_DIR",
  "worker_user": "$WORKER_USER",
  "agent_user": "$AGENT_USER"
}
JSON
chown root:"$AGENT_USER" "$CONFIG_FILE"; chmod 0640 "$CONFIG_FILE"

cat > /etc/systemd/system/ai-server-agent-executor.service <<EOF_UNIT
[Unit]
Description=AI Server Agent privileged executor
After=local-fs.target

[Service]
Type=simple
Group=$AGENT_USER
ExecStart=$INSTALL_BIN -config $CONFIG_FILE executor
Restart=always
RestartSec=2
RuntimeDirectory=ai-server-agent
RuntimeDirectoryMode=0750
UMask=0027

[Install]
WantedBy=multi-user.target
EOF_UNIT

cat > /etc/systemd/system/ai-server-agent.service <<EOF_UNIT
[Unit]
Description=AI Server Agent MCP control plane
After=network-online.target ai-server-agent-executor.service
Wants=network-online.target
Requires=ai-server-agent-executor.service

[Service]
Type=simple
User=$AGENT_USER
Group=$AGENT_USER
ExecStart=$INSTALL_BIN -config $CONFIG_FILE serve
Restart=always
RestartSec=2
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$STATE_DIR
UMask=0027

[Install]
WantedBy=multi-user.target
EOF_UNIT

systemctl daemon-reload
systemctl enable ai-server-agent-executor.service ai-server-agent.service
systemctl restart ai-server-agent-executor.service ai-server-agent.service
sleep 1
systemctl is-active --quiet ai-server-agent-executor.service || die "privileged executor did not start"
systemctl is-active --quiet ai-server-agent.service || die "MCP service did not start"
if [ "$SCHEME" = "https" ]; then
  curl -kfsS "https://127.0.0.1:$PORT/healthz" >/dev/null || die "TLS health check failed"
else
  curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null || die "health check failed"
fi

cat <<EOF_SUMMARY

AI Server Agent installed successfully.

Control plane:
  Service:       ai-server-agent.service
  Executor:      ai-server-agent-executor.service
  MCP endpoint:  $SCHEME://$BIND/mcp
  Health:        $SCHEME://$BIND/healthz
  Environment:   $STATE_DIR/AI_ENVIRONMENT.json
  Workspace:     $WORKSPACE_DIR
  Bind mode:     $MODE
  Auth mode:     $AUTH_MODE
  Native TLS:    $([ "$SCHEME" = "https" ] && printf 'enabled' || printf 'disabled')

The agent does not use ports 80/443 and does not install a web server, database,
Docker, language runtime, or hosting panel. Optional browser dependencies are
installed only when ChatGPT calls browser_setup.
EOF_SUMMARY
if [ "$MODE" = "local" ]; then
  cat <<EOF_SUMMARY

Recommended ChatGPT Business connection:
  Keep this endpoint loopback-only and use OpenAI Secure MCP Tunnel.
  MCP requests require the generated bearer token even on loopback so unprivileged
  local project/browser code cannot call root-capable MCP tools directly.
  For tunnel-client, configure the MCP Authorization header from this protected file:
    $MCP_AUTH_HEADER_FILE
EOF_SUMMARY
else
  cat <<EOF_SUMMARY

PUBLIC MODE:
  Native TLS and bearer authentication are enabled on the Agent's dedicated port.
  Keep ports 80/443 free for project web stacks. If an edge proxy maps standard HTTPS
  to this origin port, keep encryption enabled from the edge to this Agent.
  The bearer token remains protected at $CONFIG_DIR/mcp.token and is not printed.
EOF_SUMMARY
fi
