#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
# shellcheck source=../manage.sh
source "$ROOT/manage.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
CONFIG_DIR="$TMP"
CONFIG_FILE="$TMP/config.json"
CF_TXN_STATE="$TMP/cloudflare-transaction.json"
RUNTIME_BINARY="$TMP/ai-server-agent"
AGENT_USER="$(id -gn)"
fail(){ echo "FAIL: $*" >&2; exit 1; }

cat > "$CONFIG_FILE" <<'JSON'
{
  "listen_address":"127.0.0.1:3210",
  "mcp_path":"/mcp",
  "health_path":"/healthz",
  "auth_mode":"bearer",
  "bearer_token_file":"/tmp/nonexistent-test-token",
  "executor_socket":"/tmp/nonexistent-executor.sock",
  "executor_token_file":"/tmp/nonexistent-executor.token",
  "state_dir":"/tmp/no-state",
  "workspace_dir":"/tmp/no-workspace",
  "worker_user":"aiworker",
  "agent_user":"aiagent"
}
JSON
chmod 0640 "$CONFIG_FILE"
go build -o "$RUNTIME_BINARY" ./cmd/ai-server-agent

# Sandbox-only overrides: do not acquire a host root lock, restart host
# services or change real host ownership in unprivileged regression tests.
runtime_config_safe(){ [ -f "$CONFIG_FILE" ] && [ ! -L "$CONFIG_FILE" ]; }
runtime_binary_safe(){ [ -x "$RUNTIME_BINARY" ]; }
acquire_management_lock(){ :; }
chown(){ :; }
restart_and_verify_local(){
  echo restart >> "$TMP/restarts"
  [ ! -e "$TMP/force_all_restarts_fail" ] && [ "$(jq -r '.runtime.http_idle_timeout_seconds // 90' "$CONFIG_FILE")" != 999 ]
}
default="$(runtime_show)"
grep -Fq 'http_idle_timeout_seconds=90 seconds' <<<"$default" || fail 'legacy default is wrong'
grep -Fq 'text_fallback_bytes=32768 bytes' <<<"$default" || fail 'text fallback default is wrong'
runtime_apply set http_idle_timeout_seconds 300 >/dev/null
[ "$(jq -r '.runtime.http_idle_timeout_seconds' "$CONFIG_FILE")" = 300 ] || fail 'set was not persisted'
[ "$(jq -r '.bearer_token_file' "$CONFIG_FILE")" = /tmp/nonexistent-test-token ] || fail 'unrelated config was changed'
grep -Fq 'http_idle_timeout_seconds=300 seconds' <(runtime_show) || fail 'new effective config not inspectable'
before="$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)"
for bad in '-1' '0' '99999' '1.5' '00099'; do
  if (runtime_apply set command_timeout_seconds "$bad" >/dev/null 2>&1); then fail "invalid value $bad accepted"; fi
  [ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$before" ] || fail "invalid value $bad changed config"
done
if (runtime_apply set invisible_setting 10 >/dev/null 2>&1); then fail 'unknown key accepted'; fi
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$before" ] || fail 'unknown key changed config'
# A valid value that fails service health must roll back both config and
# service state. Caller gets nonzero instead of a false success.
if (runtime_apply set http_idle_timeout_seconds 999 >/dev/null 2>&1); then fail 'failed restart accepted'; fi
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$before" ] || fail 'failed restart left new config active'
[ "$(wc -l < "$TMP/restarts")" -eq 3 ] || fail 'restart/rollback attempts missing or duplicated'
runtime_apply reset http_idle_timeout_seconds >/dev/null
[ "$(jq -r '.runtime.http_idle_timeout_seconds // 90' "$CONFIG_FILE")" = 90 ] || fail 'reset did not restore default'
grep -Fq 'http_idle_timeout_seconds=90 seconds' <(runtime_show) || fail 'reset not effective'
# Abandoned prior rollback evidence blocks further edits until reconciled.
touch "$CONFIG_DIR/.runtime-old.abandoned"
if (runtime_apply set command_timeout_seconds 600 >/dev/null 2>&1); then
  fail 'abandoned prior rollback snapshot was ignored'
fi
rm -f -- "$CONFIG_DIR/.runtime-old.abandoned"
# R1: BOTH activation and rollback health fail. The old config must be
# restored on disk, but its backup must remain as durable unresolved state.
unresolved_before="$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)"
touch "$TMP/force_all_restarts_fail"
if (runtime_apply set command_timeout_seconds 600) >"$TMP/unresolved.out" 2>&1; then
  fail 'double failure falsely reported success'
fi
unresolved_backup="$(runtime_pending_backup)"
[ -n "$unresolved_backup" ] && [ -f "$unresolved_backup" ] || fail 'double failure consumed rollback snapshot'
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$unresolved_before" ] || fail 'rollback did not restore old config'
[ "$(sha256sum "$unresolved_backup" | cut -d' ' -f1)" = "$unresolved_before" ] || fail 'preserved snapshot differs from old config'
grep -Fq "$unresolved_backup" "$TMP/unresolved.out" || fail 'operator was not given real recovery path'
grep -Fq 'NOT verified' "$TMP/unresolved.out" || fail 'uncertain recovery was called successful'
restarts_at_failure="$(wc -l < "$TMP/restarts")"
if (runtime_apply set command_timeout_seconds 300) >"$TMP/blocked.out" 2>&1; then
  fail 'second set bypassed unverified recovery interlock'
fi
if (runtime_apply reset command_timeout_seconds) >>"$TMP/blocked.out" 2>&1; then
  fail 'second reset bypassed unverified recovery interlock'
fi
[ "$(wc -l < "$TMP/restarts")" = "$restarts_at_failure" ] || fail 'blocked operation restarted services'
grep -Fq 'unresolved rollback snapshot' "$TMP/blocked.out" || fail 'missing actionable blocked-state guidance'
rm -f -- "$TMP/force_all_restarts_fail"
# Only this isolated regression is authorized to discard its own fixture.
rm -f -- "$unresolved_backup"
test -z "$(find "$TMP" -maxdepth 1 -name '.runtime-*' -print)" || fail 'staged/backup files leaked on successful/verified failure paths'
echo 'Runtime settings management PASS'
