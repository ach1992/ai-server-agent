#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Run as root" >&2; exit 1; }
REPO="ach1992/ai-server-agent"
REF="${AI_SERVER_AGENT_REF:-main}"
CONFIG_FILE="/etc/ai-server-agent/config.json"
MODE="local"
PORT="3210"
TLS_CERT_FILE=""
TLS_KEY_FILE=""

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
  encoded="$(urlencode_ref "$ref")"
  json="$(curl -fsSL \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "https://api.github.com/repos/$REPO/commits/$encoded")" || {
      echo "Could not resolve GitHub source ref '$ref'" >&2
      exit 1
    }
  sha="$(printf '%s' "$json" | grep -oE '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]{40}"' | head -n1 | grep -oE '[0-9a-f]{40}' || true)"
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || {
    echo "GitHub source ref '$ref' did not resolve to a commit SHA" >&2
    exit 1
  }
  printf '%s\n' "$sha"
}

json_string_value(){
  local key="$1"
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$CONFIG_FILE" | head -n1
}

if [ -r "$CONFIG_FILE" ]; then
  listen="$(json_string_value listen_address)"
  case "$listen" in
    0.0.0.0:*) MODE="public" ;;
    127.0.0.1:*) MODE="local" ;;
  esac
  parsed_port="${listen##*:}"
  case "$parsed_port" in ''|*[!0-9]*) ;; *) PORT="$parsed_port" ;; esac
  TLS_CERT_FILE="$(json_string_value tls_cert_file)"
  TLS_KEY_FILE="$(json_string_value tls_key_file)"
fi

RESOLVED_REF="$(resolve_source_ref "$REF")"
echo "[ai-server-agent] Resolved update ref '$REF' to immutable commit '$RESOLVED_REF'."

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
curl -fsSL \
  -H 'Accept: application/vnd.github.raw+json' \
  -H 'X-GitHub-Api-Version: 2022-11-28' \
  "https://api.github.com/repos/$REPO/contents/install.sh?ref=$RESOLVED_REF" \
  -o "$TMP/install.sh"
chmod +x "$TMP/install.sh"
AI_SERVER_AGENT_REF="$RESOLVED_REF" \
AI_SERVER_AGENT_NONINTERACTIVE=1 \
AI_SERVER_AGENT_BIND_MODE="$MODE" \
AI_SERVER_AGENT_PORT="$PORT" \
AI_SERVER_AGENT_TLS_CERT_FILE="$TLS_CERT_FILE" \
AI_SERVER_AGENT_TLS_KEY_FILE="$TLS_KEY_FILE" \
  bash "$TMP/install.sh"
sleep 1
systemctl is-active --quiet ai-server-agent-executor.service
systemctl is-active --quiet ai-server-agent.service
if [ -n "$TLS_CERT_FILE" ]; then
  curl -kfsS "https://127.0.0.1:$PORT/healthz" >/dev/null
else
  curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null
fi
echo "AI Server Agent updated to $RESOLVED_REF and restarted successfully."
