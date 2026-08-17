#!/usr/bin/env bash
set -Eeuo pipefail

REPO="ach1992/ai-server-agent"
AGENT_VERSION="${AI_SERVER_AGENT_VERSION:-source}"
if [ "$AGENT_VERSION" != "source" ] && [ -z "${AI_SERVER_AGENT_REF+x}" ]; then
  REF="$AGENT_VERSION"
else
  REF="${AI_SERVER_AGENT_REF:-main}"
fi
GO_VERSION="1.26.5"
INSTALL_BIN="/usr/local/bin/ai-server-agent"
MANAGE_BIN="/usr/local/sbin/ai-server-agent-manage"
LIB_DIR="/usr/local/lib/ai-server-agent"
CONFIG_DIR="/etc/ai-server-agent"
STATE_DIR="/var/lib/ai-server-agent"
LOG_DIR="/var/log/ai-server-agent"
WORKSPACE_DIR="/srv/ai-workspace"
CONFIG_FILE="$CONFIG_DIR/config.json"
CONTROL_DIR="$CONFIG_DIR/control"
INSTALL_STATE="$CONTROL_DIR/install-state.json"
MANAGED_STATE="$CONFIG_DIR/managed.json"
MCP_AUTH_HEADER_FILE="$CONFIG_DIR/mcp.authorization"
AGENT_USER="aiagent"
WORKER_USER="aiworker"
PORT="${AI_SERVER_AGENT_PORT:-3210}"
MODE="${AI_SERVER_AGENT_BIND_MODE:-local}"
TLS_CERT_FILE="${AI_SERVER_AGENT_TLS_CERT_FILE:-}"
TLS_KEY_FILE="${AI_SERVER_AGENT_TLS_KEY_FILE:-}"
HOSTNAME_VALUE="${AI_SERVER_AGENT_HOSTNAME:-}"
TRACK_REF="${AI_SERVER_AGENT_TRACK_REF:-$REF}"
NONINTERACTIVE="${AI_SERVER_AGENT_NONINTERACTIVE:-0}"
SETUP_MODE="${AI_SERVER_AGENT_SETUP_MODE:-}"
CHECK_ONLY=0
RESOLVE_REF_ONLY=0
FRESH_INSTALL=1
SETUP_INCOMPLETE=0
RESOLVED_SOURCE_REF=""
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-/dev/null}")" 2>/dev/null && pwd || true)"
LIFECYCLE_LOCK_DIR=/run/lock/ai-server-agent
LIFECYCLE_LOCK="$LIFECYCLE_LOCK_DIR/management.lock"

trap 'echo "[ERROR] Installation failed at line $LINENO. Review the output above; existing project files were not intentionally modified." >&2' ERR

log(){ printf '[ai-server-agent] %s\n' "$*"; }
warn(){ printf '[ai-server-agent] WARNING: %s\n' "$*" >&2; }
die(){ printf '[ai-server-agent] ERROR: %s\n' "$*" >&2; exit 1; }
need_root(){ [ "$(id -u)" -eq 0 ] || die "Run this installer as root (for example: sudo bash install.sh)."; }
version_ge(){ dpkg --compare-versions "$1" ge "$2"; }

acquire_lifecycle_lock(){
  command -v flock >/dev/null 2>&1 || die "flock is required for privileged lifecycle serialization."
  [ ! -L "$LIFECYCLE_LOCK_DIR" ] || die "Refusing symlinked lifecycle lock directory: $LIFECYCLE_LOCK_DIR"
  install -d -o root -g root -m 0700 "$LIFECYCLE_LOCK_DIR"
  chown root:root "$LIFECYCLE_LOCK_DIR"; chmod 0700 "$LIFECYCLE_LOCK_DIR"
  [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK_DIR" 2>/dev/null)" = "0:0:700" ] || die "Lifecycle lock directory ownership/mode is unsafe."
  [ ! -L "$LIFECYCLE_LOCK" ] || die "Refusing symlinked lifecycle lock: $LIFECYCLE_LOCK"
  ( umask 077; : >> "$LIFECYCLE_LOCK" ) || die "Could not create lifecycle lock."
  [ -f "$LIFECYCLE_LOCK" ] && [ ! -L "$LIFECYCLE_LOCK" ] || die "Lifecycle lock is not a regular file."
  chown root:root "$LIFECYCLE_LOCK"; chmod 0600 "$LIFECYCLE_LOCK"
  [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK" 2>/dev/null)" = "0:0:600" ] || die "Lifecycle lock ownership/mode is unsafe."
  if [ "$(readlink /proc/$$/fd/9 2>/dev/null || true)" = "$LIFECYCLE_LOCK" ]; then
    flock -n 9 || die "Another AI Server Agent management operation is already active. Retry after it finishes."
    return 0
  fi
  exec 9>>"$LIFECYCLE_LOCK" || die "Could not open lifecycle lock."
  flock -n 9 || die "Another AI Server Agent management operation is already active. Retry after it finishes."
}

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
  if [[ "$ref" =~ ^[0-9a-fA-F]{40}$ ]]; then printf '%s\n' "${ref,,}"; return 0; fi
  command -v curl >/dev/null || die "curl is required to resolve source ref '$ref'"
  encoded="$(urlencode_ref "$ref")"
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "https://api.github.com/repos/$REPO/commits/$encoded")" || die "Could not resolve GitHub source ref '$ref'"
  sha="$(printf '%s' "$json" | grep -oE '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]{40}"' | head -n1 | grep -oE '[0-9a-f]{40}' || true)"
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || die "GitHub source ref '$ref' did not resolve to a commit SHA"
  printf '%s\n' "$sha"
}

json_string_value(){
  local key="$1"
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$CONFIG_FILE" | head -n1
}

case "${1:-}" in
  --check) CHECK_ONLY=1 ;;
  --resolve-ref) RESOLVE_REF_ONLY=1 ;;
esac
need_root
[ -r /etc/os-release ] || die "/etc/os-release not found"
# shellcheck disable=SC1091
. /etc/os-release
command -v systemctl >/dev/null || die "systemd/systemctl is required"
case "$(uname -m)" in x86_64) ARCH=amd64; GOARCH=amd64 ;; aarch64|arm64) ARCH=arm64; GOARCH=arm64 ;; *) die "Unsupported architecture: $(uname -m)" ;; esac

if [ "$AGENT_VERSION" = "source" ]; then
  case "${ID:-}" in
    ubuntu) version_ge "${VERSION_ID:-0}" "22.04" || die "Ubuntu 22.04 or newer is required" ;;
    debian) version_ge "${VERSION_ID:-0}" "11" || die "Debian 11 or newer is required" ;;
    *) die "Unsupported source-install OS: ${ID:-unknown}." ;;
  esac
else
  [ "${ID:-}" = "ubuntu" ] && [ "${VERSION_ID:-}" = "22.04" ] || die "Stable v0.1 supports Ubuntu 22.04 LTS only. Detected: ${PRETTY_NAME:-unknown}."
  [ "$ARCH" = "amd64" ] || die "Stable v0.1 supports amd64/x86_64 only."
fi

if [ "$AGENT_VERSION" != "source" ] && [ -n "${AI_SERVER_AGENT_BINARY:-}" ]; then
  die "AI_SERVER_AGENT_BINARY is disabled for stable releases. Stable installs must use the release archive verified by SHA256SUMS."
fi
if [ "$RESOLVE_REF_ONLY" -eq 1 ]; then resolve_source_ref "$REF"; exit 0; fi
if [ "$CHECK_ONLY" -eq 1 ]; then log "Compatibility check passed: $PRETTY_NAME, $ARCH, systemd available."; exit 0; fi

acquire_lifecycle_lock
if [ -s "$CONFIG_FILE" ]; then FRESH_INSTALL=0; fi

export DEBIAN_FRONTEND=noninteractive
log "Installing minimal setup utilities (ca-certificates, curl, jq, openssl, tar, xz-utils)..."
apt-get update -qq
apt-get install -y -qq ca-certificates curl jq openssl tar xz-utils >/dev/null

# Preserve an existing connection unless explicit environment variables override it.
if [ "$FRESH_INSTALL" -eq 0 ]; then
  existing_listen="$(json_string_value listen_address)"
  if [ -z "${AI_SERVER_AGENT_BIND_MODE+x}" ]; then case "$existing_listen" in 0.0.0.0:*) MODE=public ;; *) MODE=local ;; esac; fi
  if [ -z "${AI_SERVER_AGENT_PORT+x}" ]; then existing_port="${existing_listen##*:}"; [[ "$existing_port" =~ ^[0-9]+$ ]] && PORT="$existing_port"; fi
  [ -n "${AI_SERVER_AGENT_TLS_CERT_FILE+x}" ] || TLS_CERT_FILE="$(json_string_value tls_cert_file)"
  [ -n "${AI_SERVER_AGENT_TLS_KEY_FILE+x}" ] || TLS_KEY_FILE="$(json_string_value tls_key_file)"
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
  # Fresh interactive installs intentionally bootstrap local first; the wizard configures public TLS afterward.
  if [ "$FRESH_INSTALL" -eq 1 ] && [ "$NONINTERACTIVE" != "1" ] && [ -z "${AI_SERVER_AGENT_BIND_MODE+x}" ]; then
    MODE=local; TLS_CERT_FILE=""; TLS_KEY_FILE=""
  else
    die "Public mode requires native TLS. Use the guided Cloudflare setup or set both TLS file paths."
  fi
fi

if ! getent group "$AGENT_USER" >/dev/null 2>&1; then groupadd --system "$AGENT_USER"; fi
if ! getent group "$WORKER_USER" >/dev/null 2>&1; then groupadd --system "$WORKER_USER"; fi
if ! id "$AGENT_USER" >/dev/null 2>&1; then useradd --system --gid "$AGENT_USER" --home "$STATE_DIR" --shell /usr/sbin/nologin "$AGENT_USER"; fi
if ! id "$WORKER_USER" >/dev/null 2>&1; then useradd --system --gid "$WORKER_USER" --create-home --home-dir "$WORKSPACE_DIR" --shell /bin/bash "$WORKER_USER"; fi

install -d -m 0750 -o root -g "$AGENT_USER" "$CONFIG_DIR"
install -d -m 0700 -o root -g root "$CONTROL_DIR"
[ ! -L "$STATE_DIR" ] || die "Refusing a symlinked state directory: $STATE_DIR"
install -d -m 0711 -o root -g root "$STATE_DIR"
install -d -m 2750 -o root -g "$AGENT_USER" "$LOG_DIR"
install -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$WORKSPACE_DIR"
secure_state_container(){
  local path="$1"
  if [ -L "$path" ]; then
    warn "Removing unsafe legacy state-container symlink without following it: $path"
    rm -f -- "$path"
  fi
  if [ -e "$path" ] && [ ! -d "$path" ]; then
    die "State container is not a real directory: $path"
  fi
  install -d -m 0711 -o root -g root "$path"
}
secure_state_container "$STATE_DIR/runtime"
secure_state_container "$STATE_DIR/jobs"
if [ -L "$STATE_DIR/runtime/browser" ]; then
  warn "Removing unsafe legacy browser-data symlink without following it: $STATE_DIR/runtime/browser"
  rm -f -- "$STATE_DIR/runtime/browser"
fi
if [ -e "$STATE_DIR/runtime/browser" ] && [ ! -d "$STATE_DIR/runtime/browser" ]; then
  die "Browser runtime-data path is not a real directory: $STATE_DIR/runtime/browser"
fi
# Existing job outputs from the older worker-owned container are never trusted
# as symlinks after migration.
find "$STATE_DIR/jobs" -mindepth 1 -maxdepth 1 -type l -delete
# AI_ENVIRONMENT.json is informational output written by aiagent. Its parent is
# root-controlled, so the service can update the file but cannot replace the
# directory entry with a symlink or another inode.
rm -f -- "$STATE_DIR/AI_ENVIRONMENT.json"
install -o "$AGENT_USER" -g "$AGENT_USER" -m 0640 /dev/null "$STATE_DIR/AI_ENVIRONMENT.json"
install -d -m 0755 -o root -g root "$LIB_DIR"

random_hex(){ od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; printf '\n'; }
if [ ! -s "$CONFIG_DIR/executor.token" ]; then random_hex > "$CONFIG_DIR/executor.token"; fi
if [ ! -s "$CONFIG_DIR/mcp.token" ]; then random_hex > "$CONFIG_DIR/mcp.token"; fi
chown root:"$AGENT_USER" "$CONFIG_DIR"/*.token
chmod 0640 "$CONFIG_DIR"/*.token
printf 'Bearer %s\n' "$(cat "$CONFIG_DIR/mcp.token")" > "$MCP_AUTH_HEADER_FILE"
chown root:"$AGENT_USER" "$MCP_AUTH_HEADER_FILE"; chmod 0640 "$MCP_AUTH_HEADER_FILE"

install_helpers(){
  local root="$1"
  [ -f "$root/manage.sh" ] && [ -f "$root/update.sh" ] && [ -f "$root/uninstall.sh" ] || die "release/source payload is missing management helpers"
  install -o root -g root -m 0700 "$root/manage.sh" "$LIB_DIR/manage.sh"
  install -o root -g root -m 0755 "$root/update.sh" "$LIB_DIR/update.sh"
  install -o root -g root -m 0755 "$root/uninstall.sh" "$LIB_DIR/uninstall.sh"
  cat > "$MANAGE_BIN" <<'EOF_MANAGE_WRAPPER'
#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Run as root" >&2; exit 1; }
LOCK_DIR=/run/lock/ai-server-agent
LOCK_FILE="$LOCK_DIR/management.lock"
MANAGE_IMPL=/usr/local/lib/ai-server-agent/manage.sh
command -v flock >/dev/null 2>&1 || { echo "flock is required for privileged lifecycle serialization" >&2; exit 1; }
[ ! -L "$LOCK_DIR" ] || { echo "Refusing symlinked lifecycle lock directory: $LOCK_DIR" >&2; exit 1; }
install -d -o root -g root -m 0700 "$LOCK_DIR"
chown root:root "$LOCK_DIR"; chmod 0700 "$LOCK_DIR"
[ "$(stat -c '%u:%g:%a' "$LOCK_DIR" 2>/dev/null)" = "0:0:700" ] || { echo "Lifecycle lock directory ownership/mode is unsafe" >&2; exit 1; }
[ ! -L "$LOCK_FILE" ] || { echo "Refusing symlinked lifecycle lock: $LOCK_FILE" >&2; exit 1; }
( umask 077; : >> "$LOCK_FILE" )
[ -f "$LOCK_FILE" ] && [ ! -L "$LOCK_FILE" ] || { echo "Lifecycle lock is not a regular file" >&2; exit 1; }
chown root:root "$LOCK_FILE"; chmod 0600 "$LOCK_FILE"
[ "$(stat -c '%u:%g:%a' "$LOCK_FILE" 2>/dev/null)" = "0:0:600" ] || { echo "Lifecycle lock ownership/mode is unsafe" >&2; exit 1; }
if [ "$(readlink /proc/$$/fd/9 2>/dev/null || true)" = "$LOCK_FILE" ]; then
  flock -n 9 || { echo "Another AI Server Agent management operation is already active. Retry after it finishes." >&2; exit 1; }
else
  exec 9>>"$LOCK_FILE"
  flock -n 9 || { echo "Another AI Server Agent management operation is already active. Retry after it finishes." >&2; exit 1; }
fi
[ -x "$MANAGE_IMPL" ] || { echo "Management implementation is missing: $MANAGE_IMPL" >&2; exit 1; }
exec "$MANAGE_IMPL" "$@"
EOF_MANAGE_WRAPPER
  chown root:root "$MANAGE_BIN"; chmod 0755 "$MANAGE_BIN"
}

build_from_source(){ (
  set -Eeuo pipefail
  local tmp go_url go_sha
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  log "Building a self-contained binary from immutable commit '$RESOLVED_SOURCE_REF' using a temporary Go toolchain..."
  curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "https://api.github.com/repos/$REPO/tarball/$RESOLVED_SOURCE_REF" -o "$tmp/src.tgz"
  mkdir "$tmp/src"; tar -xzf "$tmp/src.tgz" -C "$tmp/src" --strip-components=1
  case "$GOARCH" in amd64) go_sha="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053" ;; arm64) go_sha="fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49" ;; esac
  go_url="https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz"
  curl -fsSL "$go_url" -o "$tmp/go.tgz"
  printf '%s  %s\n' "$go_sha" "$tmp/go.tgz" | sha256sum -c - >/dev/null
  mkdir "$tmp/go"; tar -xzf "$tmp/go.tgz" -C "$tmp/go" --strip-components=1
  (cd "$tmp/src" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" "$tmp/go/bin/go" test ./... && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" "$tmp/go/bin/go" build -trimpath -ldflags='-s -w' -o "$tmp/ai-server-agent" ./cmd/ai-server-agent)
  install -m 0755 "$tmp/ai-server-agent" "$INSTALL_BIN"
  install_helpers "$tmp/src"
); }

download_release(){ (
  set -Eeuo pipefail
  local tmp url asset bin payload_root
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
  payload_root="$(dirname "$bin")"
  install -m 0755 "$bin" "$INSTALL_BIN"
  install_helpers "$payload_root"
); }

if [ -n "${AI_SERVER_AGENT_BINARY:-}" ]; then
  [ -x "$AI_SERVER_AGENT_BINARY" ] || die "AI_SERVER_AGENT_BINARY is not executable"
  install -m 0755 "$AI_SERVER_AGENT_BINARY" "$INSTALL_BIN"
  if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/manage.sh" ]; then install_helpers "$SCRIPT_DIR"; else die "AI_SERVER_AGENT_BINARY installs require manage.sh/update.sh/uninstall.sh beside install.sh"; fi
  RESOLVED_SOURCE_REF="${AI_SERVER_AGENT_REF:-binary}"
elif [ "$AGENT_VERSION" = "source" ]; then
  RESOLVED_SOURCE_REF="$(resolve_source_ref "$REF")"
  log "Resolved source ref '$REF' to commit '$RESOLVED_SOURCE_REF'."
  build_from_source
else
  download_release
fi

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

if [ "$AGENT_VERSION" = "source" ]; then
  STATE_CHANNEL=source; STATE_VERSION=source; STATE_REF="${RESOLVED_SOURCE_REF:-$REF}"
else
  STATE_CHANNEL=stable; STATE_VERSION="$AGENT_VERSION"; STATE_REF="$REF"; TRACK_REF="$AGENT_VERSION"
fi
case "$STATE_CHANNEL" in stable|source) ;; *) die "invalid install channel" ;; esac
[[ "$STATE_VERSION" =~ ^(source|v[0-9]+\.[0-9]+\.[0-9]+)$ ]] || die "invalid install version metadata"
[[ "$STATE_REF" =~ ^([0-9a-f]{40}|v[0-9]+\.[0-9]+\.[0-9]+|binary)$ ]] || die "invalid install ref metadata: $STATE_REF"
[[ "$TRACK_REF" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]] || die "invalid install tracking ref metadata: $TRACK_REF"
if [ "$STATE_CHANNEL" = stable ]; then
  [ "$STATE_REF" = "$STATE_VERSION" ] && [ "$TRACK_REF" = "$STATE_VERSION" ] || die "Stable install metadata must pin version/ref/track_ref to the same tag."
else
  [ "$STATE_VERSION" = source ] || die "Source install metadata must use version=source."
  [[ "$STATE_REF" =~ ^([0-9a-f]{40}|binary)$ ]] || die "Source install metadata must use an immutable commit SHA or binary marker."
fi
state_tmp="$(mktemp "$CONTROL_DIR/.install-state.XXXXXX")"
jq -n \
  --arg channel "$STATE_CHANNEL" \
  --arg version "$STATE_VERSION" \
  --arg ref "$STATE_REF" \
  --arg track_ref "$TRACK_REF" \
  '{channel:$channel,version:$version,ref:$ref,track_ref:$track_ref}' > "$state_tmp"
chown root:root "$state_tmp"; chmod 0600 "$state_tmp"
mv -f "$state_tmp" "$INSTALL_STATE"
# v0.1.1 and earlier stored executable shell metadata in the runtime state
# directory. Never read it during migration; remove only the legacy directory
# entry after the new root-only JSON state has been committed.
rm -f "$STATE_DIR/install-state.env"

if [ ! -s "$MANAGED_STATE" ]; then
  jq -n --arg provider "$([ "$MODE" = "public" ] && printf manual || printf local)" --arg hostname "$HOSTNAME_VALUE" --argjson port "$PORT" '{active_provider:$provider,hostname:$hostname,port:$port,cloudflare:{}}' > "$MANAGED_STATE"
  chown root:"$AGENT_USER" "$MANAGED_STATE"; chmod 0640 "$MANAGED_STATE"
fi

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
ReadWritePaths=$STATE_DIR/AI_ENVIRONMENT.json
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
if [ "$SCHEME" = "https" ]; then curl -kfsS "https://127.0.0.1:$PORT/healthz" >/dev/null || die "TLS health check failed"
else curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null || die "health check failed"; fi

log "Core installation is healthy."

if [ "$NONINTERACTIVE" = "1" ]; then
  case "$SETUP_MODE" in
    ""|keep) ;;
    local) "$MANAGE_BIN" configure-local ;;
    cloudflare) "$MANAGE_BIN" configure-cloudflare ;;
    manual) "$MANAGE_BIN" configure-manual-tls ;;
    *) die "AI_SERVER_AGENT_SETUP_MODE must be local, cloudflare, manual, keep, or empty" ;;
  esac
elif [ "$FRESH_INSTALL" -eq 1 ] && [ -r /dev/tty ] && [ -z "${AI_SERVER_AGENT_BIND_MODE+x}" ]; then
  if ! "$MANAGE_BIN" first-run; then
    SETUP_INCOMPLETE=1
    warn "Core installation succeeded, but connection setup was not completed. The Agent remains installed and healthy in its last verified mode."
    warn "Resume setup with: sudo ai-server-agent-manage"
  fi
fi

if [ "$SETUP_INCOMPLETE" -eq 1 ]; then
  cat <<EOF_SUMMARY

AI Server Agent core installed successfully; connection setup is incomplete.

  Service:       ai-server-agent.service
  Executor:      ai-server-agent-executor.service
  Environment:   $STATE_DIR/AI_ENVIRONMENT.json
  Workspace:     $WORKSPACE_DIR
  Management:    sudo ai-server-agent-manage
  Auth:          bearer (protected; not printed)

The Agent is installed and healthy. Resume connection setup with:
  sudo ai-server-agent-manage
EOF_SUMMARY
else
  cat <<EOF_SUMMARY

AI Server Agent installed successfully.

  Service:       ai-server-agent.service
  Executor:      ai-server-agent-executor.service
  Environment:   $STATE_DIR/AI_ENVIRONMENT.json
  Workspace:     $WORKSPACE_DIR
  Management:    sudo ai-server-agent-manage
  Auth:          bearer (protected; not printed)

Use "sudo ai-server-agent-manage" for status, ChatGPT setup, domain/TLS changes,
updates, repair, Cloudflare cleanup, safe uninstall, or purge.
EOF_SUMMARY
fi

if [ "$NONINTERACTIVE" != "1" ] && [ -r /dev/tty ]; then
  echo
  "$MANAGE_BIN" status
  echo
  if [ "$SETUP_INCOMPLETE" -eq 1 ]; then
    echo "Next: run 'sudo ai-server-agent-manage' and choose Configure or change domain / connection."
  else
    echo "Next: run 'sudo ai-server-agent-manage' any time. Choose ChatGPT Setup when you are ready to connect."
  fi
fi
