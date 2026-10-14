#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
# shellcheck source=../manage.sh
source "$ROOT/manage.sh"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
CONFIG_DIR="$TMP"
CONFIG_FILE="$TMP/config.json"
CF_TXN_STATE="$TMP/cloudflare-transaction.json"
STATE_DIR="$TMP/state"
mkdir -p "$STATE_DIR"
AGENT_USER="$(id -gn)"
RUNTIME_BINARY="${ASA_PROFILE_TEST_BINARY:-$TMP/ai-server-agent}"
if [ ! -x "$RUNTIME_BINARY" ]; then
  go build -o "$RUNTIME_BINARY" ./cmd/ai-server-agent
fi
fail(){ echo "PROFILE TEST FAIL: $*" >&2; exit 1; }
cat > "$CONFIG_FILE" <<'JSON'
{"listen_address":"127.0.0.1:3210","mcp_path":"/mcp","health_path":"/healthz","auth_mode":"bearer","bearer_token_file":"/tmp/profile-test-secret-file","executor_socket":"/tmp/executor.sock","executor_token_file":"/tmp/executor.token","state_dir":"/tmp/state","workspace_dir":"/tmp/workspace","agent_user":"aiagent","worker_user":"aiworker","environment_profile":"development"}
JSON
chmod 0640 "$CONFIG_FILE"

# Isolated manager fixture: no real root lock, host process or connection.
runtime_config_safe(){ [ -f "$CONFIG_FILE" ] && [ ! -L "$CONFIG_FILE" ]; }
runtime_binary_safe(){ [ -x "$RUNTIME_BINARY" ]; }
acquire_management_lock(){ :; }
chown(){ :; }
restart_agent_and_verify_profile(){
  printf 'restart\n' >> "$TMP/restarts"
  [ ! -e "$TMP/fail_all" ] || return 1
  [ ! -e "$TMP/fail_validation" ] || [ "$(jq -r '.environment_profile' "$CONFIG_FILE")" != validation ]
}
profile_text="$(profile_show)"
grep -Fq 'Profile: development' <<<"$profile_text" || fail 'default role not visible'
profile_apply environment_profile validation >/dev/null
[ "$(jq -r '.environment_profile' "$CONFIG_FILE")" = validation ] || fail 'validation role not stored'
[ "$(jq -r '.bearer_token_file' "$CONFIG_FILE")" = /tmp/profile-test-secret-file ] || fail 'credential source altered'
profile_apply server_label 'QA آزمایش' >/dev/null
[ "$(jq -r '.server_label' "$CONFIG_FILE")" = 'QA آزمایش' ] || fail 'Unicode label not persisted'
before="$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)"
if (profile_apply environment_profile full >/dev/null 2>&1); then fail 'invalid role accepted'; fi
if (profile_apply server_label $'bad\nlabel' >/dev/null 2>&1); then fail 'control-character label accepted'; fi
if (profile_apply server_label "$(printf '%081d' 1)" >/dev/null 2>&1); then fail 'oversized label accepted'; fi
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$before" ] || fail 'invalid values changed config'
profile_apply server_label '' >/dev/null
[ "$(jq -r '.server_label // ""' "$CONFIG_FILE")" = '' ] || fail 'clearing label failed'
# Activation fails only for validation, then the old development config must
# be restored and verified without leaving a stale protected rollback.
profile_apply environment_profile development >/dev/null
before="$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)"
touch "$TMP/fail_validation"
if (profile_apply environment_profile validation) >"$TMP/failure.log" 2>&1; then fail 'failed activate returned success'; fi
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$before" ] || fail 'failed activate damaged old config'
[ -z "$(profile_pending_backup)" ] || fail 'verified rollback left unresolved backup'
rm -f "$TMP/fail_validation"
# Activation and recovery both fail: snapshot must remain and prevent more edits.
touch "$TMP/fail_all"
if (profile_apply server_label 'QA-2') >"$TMP/unresolved.log" 2>&1; then fail 'double failed restart accepted'; fi
snap="$(profile_pending_backup)"
[ -n "$snap" ] && [ -f "$snap" ] || fail 'unresolved rollback snapshot missing'
[ "$(sha256sum "$CONFIG_FILE" | cut -d' ' -f1)" = "$before" ] || fail 'unresolved edit corrupted old config'
grep -Fq 'NOT verified' "$TMP/unresolved.log" || fail 'unverified failure not clearly explained'
if (profile_apply environment_profile validation) >/dev/null 2>&1; then fail 'unresolved transaction did not block reuse'; fi
if (runtime_apply set command_timeout_seconds 300) >/dev/null 2>&1; then fail 'runtime edit bypassed unresolved profile guard'; fi
rm -f -- "$snap" "$TMP/fail_all"
test -z "$(profile_pending_backup)" || fail 'disposable unresolved fixture retained state'
echo 'Environment profile manager (isolated) PASS'
