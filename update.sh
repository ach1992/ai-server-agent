#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Run as root" >&2; exit 1; }
REF="${AI_SERVER_AGENT_REF:-main}"
CONFIG_FILE="/etc/ai-server-agent/config.json"
MODE="local"
PORT="3210"
if [ -r "$CONFIG_FILE" ]; then
  listen="$(sed -n 's/.*"listen_address"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG_FILE" | head -n1)"
  case "$listen" in
    0.0.0.0:*) MODE="public" ;;
    127.0.0.1:*) MODE="local" ;;
  esac
  parsed_port="${listen##*:}"
  case "$parsed_port" in ''|*[!0-9]*) ;; *) PORT="$parsed_port" ;; esac
fi
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
curl -fsSL "https://raw.githubusercontent.com/ach1992/ai-server-agent/$REF/install.sh" -o "$TMP/install.sh"
chmod +x "$TMP/install.sh"
AI_SERVER_AGENT_REF="$REF" AI_SERVER_AGENT_NONINTERACTIVE=1 AI_SERVER_AGENT_BIND_MODE="$MODE" AI_SERVER_AGENT_PORT="$PORT" bash "$TMP/install.sh"
systemctl restart ai-server-agent-executor.service ai-server-agent.service
sleep 1
systemctl is-active --quiet ai-server-agent-executor.service
systemctl is-active --quiet ai-server-agent.service
curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null
echo "AI Server Agent updated and restarted successfully."
