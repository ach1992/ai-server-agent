#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Root-only isolated regression" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
# shellcheck source=../manage.sh
source "$ROOT/manage.sh"
TEMP="$(mktemp -d)"
trap 'rm -rf -- "$TEMP"' EXIT
CONFIG_DIR="$TEMP"
CONFIG_FILE="$TEMP/config.json"
CF_TXN_STATE="$TEMP/cloudflare-transaction.json"
RUNTIME_BINARY="$TEMP/agent"
AGENT_USER=root
# Nothing below touches host services or the real lifecycle lock.
acquire_management_lock(){ :; }
restart_and_verify_local(){
  printf 'restart\n' >> "$TEMP/restarts"
  [ "$(jq -r '.runtime.command_timeout_seconds // 300' "$CONFIG_FILE")" != 800 ]
}
install -o root -g root -m 0755 "${ASA_RUNTIME_TEST_BINARY:?required pinned local source binary}" "$RUNTIME_BINARY"
cat > "$CONFIG_FILE" <<'JSON'
{"listen_address":"127.0.0.1:3210","mcp_path":"/mcp","health_path":"/healthz","auth_mode":"bearer","bearer_token_file":"/tmp/no-credential-data","executor_socket":"/tmp/exec.sock","executor_token_file":"/tmp/exec.token","state_dir":"/tmp/state","workspace_dir":"/tmp/workspace","agent_user":"aiagent","worker_user":"aiworker"}
JSON
chown root:root "$CONFIG_FILE"
chmod 0640 "$CONFIG_FILE"
original="$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)"
runtime_apply set command_timeout_seconds 600 >/dev/null
[ "$(jq -r '.runtime.command_timeout_seconds' "$CONFIG_FILE")" = 600 ] || { echo 'real root setting not applied' >&2; exit 1; }
[ "$(stat -c '%u:%g:%a' "$CONFIG_FILE")" = '0:0:640' ] || { echo 'config owner/mode drifted' >&2; exit 1; }
success="$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)"
if (runtime_apply set command_timeout_seconds 800 >/dev/null 2>&1); then echo 'failed restart accepted' >&2; exit 1; fi
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$success" ] || { echo 'rollback not exact' >&2; exit 1; }
if (runtime_apply set command_timeout_seconds 1801 >/dev/null 2>&1); then echo 'unsafe timeout accepted' >&2; exit 1; fi
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$success" ] || { echo 'invalid value mutated file' >&2; exit 1; }
mv "$CONFIG_FILE" "$TEMP/real-config"
ln -s "$TEMP/real-config" "$CONFIG_FILE"
if (runtime_apply set command_timeout_seconds 600 >/dev/null 2>&1); then echo 'symlinked config accepted' >&2; exit 1; fi
rm -- "$CONFIG_FILE"
mv "$TEMP/real-config" "$CONFIG_FILE"
chmod 0660 "$CONFIG_FILE"
if (runtime_apply set command_timeout_seconds 600 >/dev/null 2>&1); then echo 'group-writable config accepted' >&2; exit 1; fi
chmod 0640 "$CONFIG_FILE"
touch "$CF_TXN_STATE"
if (runtime_apply set command_timeout_seconds 600 >/dev/null 2>&1); then echo 'active cloudflare journal bypassed' >&2; exit 1; fi
rm -- "$CF_TXN_STATE"
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$success" ] || { echo 'negative root tests mutated config' >&2; exit 1; }
touch "$CONFIG_DIR/.runtime-old.abandoned"
if (runtime_apply set command_timeout_seconds 600 >/dev/null 2>&1); then echo 'abandoned rollback accepted' >&2; exit 1; fi
rm -f -- "$CONFIG_DIR/.runtime-old.abandoned"
[ "$original" != "$success" ] || { echo 'positive setting not applied' >&2; exit 1; }
echo 'Runtime settings root safety PASS'
