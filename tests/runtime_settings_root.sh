#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Root-only isolated regression" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
# shellcheck source=../manage.sh
source "$ROOT/manage.sh"
# Capture the production helper before the ordinary root fixture overrides it.
production_restart_helper="$(declare -f restart_and_verify_local)"
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
  [ ! -e "$TEMP/force_all_restarts_fail" ] && [ "$(jq -r '.runtime.command_timeout_seconds // 300' "$CONFIG_FILE")" != 800 ]
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
# R1 root: both service health checks fail; leave a REAL root-owned
# recovery snapshot, and refuse subsequent set/reset while unverified.
touch "$TEMP/force_all_restarts_fail"
if (runtime_apply set command_timeout_seconds 650) >"$TEMP/unresolved.out" 2>&1; then
  echo 'root: double health failure was accepted' >&2; exit 1
fi
snapshot="$(runtime_pending_backup)"
[ -n "$snapshot" ] && [ -f "$snapshot" ] || { echo 'root: missing durable backup' >&2; exit 1; }
[ "$(sha256sum "$snapshot" | cut -d' ' -f1)" = "$success" ] || { echo 'root: backup contents drifted' >&2; exit 1; }
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$success" ] || { echo 'root: old config not restored' >&2; exit 1; }
[ "$(stat -c '%u:%g:%a' "$snapshot")" = '0:0:640' ] || { echo 'root: backup protection lost' >&2; exit 1; }
[ "$(stat -c '%u:%g:%a' "$CONFIG_FILE")" = '0:0:640' ] || { echo 'root: restored file protection drifted' >&2; exit 1; }
grep -Fq "$snapshot" "$TEMP/unresolved.out" || { echo 'root: recovery path not reported' >&2; exit 1; }
restart_count="$(wc -l < "$TEMP/restarts")"
if (runtime_apply set command_timeout_seconds 650 >/dev/null 2>&1); then
  echo 'root: unverified recovery allowed second set' >&2; exit 1
fi
if (runtime_apply reset command_timeout_seconds >/dev/null 2>&1); then
  echo 'root: unverified recovery allowed reset' >&2; exit 1
fi
[ "$(wc -l < "$TEMP/restarts")" = "$restart_count" ] || { echo 'root: refused edit tried restart' >&2; exit 1; }
rm -f -- "$TEMP/force_all_restarts_fail" "$snapshot"
test -z "$(runtime_pending_backup)" || { echo 'root: fixture backup cleanup failed' >&2; exit 1; }
[ "$original" != "$success" ] || { echo 'positive setting not applied' >&2; exit 1; }
# R4: exercise the original PRODUCTION restart helper, not the above
# replacement health fixture. Fake only external systemctl/curl so no real
# root-host services, sockets or health endpoints can be mutated/contacted.
eval "$production_restart_helper"
systemctl(){
  case "${1:-}" in
    restart) printf 'restart\n' >> "$TEMP/r4.restart_calls"; return 42 ;;
    is-active) printf 'is-active\n' >> "$TEMP/r4.health_probes"; return 0 ;;
    *) echo "root: unexpected fixture systemctl: $*" >&2; return 2 ;;
  esac
}
curl(){ printf 'health\n' >> "$TEMP/r4.health_probes"; return 0; }
sleep(){ printf 'sleep\n' >> "$TEMP/r4.sleep_calls"; return 0; }
systemctl is-active --quiet ai-server-agent.service || { echo 'root: old-service health fixture failed' >&2; exit 1; }
curl -fsS http://127.0.0.1:3210/healthz || { echo 'root: old HTTP-health fixture failed' >&2; exit 1; }
: > "$TEMP/r4.health_probes"
if restart_and_verify_local >/dev/null 2>&1; then
  echo 'root: restart helper reported success despite failed systemctl restart' >&2; exit 1
fi
[ "$(wc -l < "$TEMP/r4.restart_calls")" -eq 1 ] || { echo 'root: wrong direct restart count' >&2; exit 1; }
[ ! -s "$TEMP/r4.health_probes" ] || { echo 'root: read old service health after restart error' >&2; exit 1; }
[ ! -e "$TEMP/r4.sleep_calls" ] || { echo 'root: failed restart was not rejected immediately' >&2; exit 1; }

r4_before="$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)"
if (runtime_apply set command_timeout_seconds 700) >"$TEMP/r4.apply.out" 2>&1; then
  echo 'root: command-level failed restart falsely accepted' >&2; exit 1
fi
r4_snapshot="$(runtime_pending_backup)"
[ -n "$r4_snapshot" ] && [ -f "$r4_snapshot" ] || { echo 'root: failed systemctl restart lost rollback snapshot' >&2; exit 1; }
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$r4_before" ] || { echo 'root: failed restart corrupted old config' >&2; exit 1; }
[ "$(sha256sum "$r4_snapshot" | cut -d' ' -f1)" = "$r4_before" ] || { echo 'root: failed restart damaged snapshot' >&2; exit 1; }
[ "$(stat -c '%u:%g:%a' "$CONFIG_FILE")" = '0:0:640' ] || { echo 'root: restored config trust lost' >&2; exit 1; }
[ "$(stat -c '%u:%g:%a' "$r4_snapshot")" = '0:0:640' ] || { echo 'root: failed restart backup trust lost' >&2; exit 1; }
[ "$(wc -l < "$TEMP/r4.restart_calls")" -eq 3 ] || { echo 'root: missing initial/rollback restart attempts' >&2; exit 1; }
[ ! -s "$TEMP/r4.health_probes" ] || { echo 'root: old health disguised failed restart' >&2; exit 1; }
grep -Fq 'NOT verified' "$TEMP/r4.apply.out" || { echo 'root: missing uncertain restart diagnostic' >&2; exit 1; }
grep -Fq "$r4_snapshot" "$TEMP/r4.apply.out" || { echo 'root: missing true recovery snapshot path' >&2; exit 1; }
if (runtime_apply reset command_timeout_seconds >/dev/null 2>&1); then
  echo 'root: unresolved restart allowed further setting change' >&2; exit 1
fi
[ "$(wc -l < "$TEMP/r4.restart_calls")" -eq 3 ] || { echo 'root: blocked change started a new restart' >&2; exit 1; }
# Clean only the isolated harness's own snapshot, never installed state.
rm -f -- "$r4_snapshot"
test -z "$(runtime_pending_backup)" || { echo 'root: R4 test leftover rollback state' >&2; exit 1; }

echo 'Runtime settings root safety PASS'
