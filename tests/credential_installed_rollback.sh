#!/usr/bin/env bash
set -Eeuo pipefail
# Destructive fixture ONLY on the GitHub-hosted disposable lifecycle runner.
[ "${ASA_DISPOSABLE_CREDENTIAL_ACCEPTANCE:-}" = 1 ] && [ "${GITHUB_ACTIONS:-}" = true ] || { echo 'DISPOSABLE_CREDENTIAL_ACCEPTANCE_NOT_AUTHORIZED' >&2; exit 1; }
[ "$(id -u)" -ne 0 ] || { echo 'Run as unprivileged CI user via sudo for specific operations' >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# Immutable v0.1.14 release: real pre-retirement named-credential schema.
BASE=6468d48b4ff5f1f82ad446531dff9951e86a0e3c
if ! git cat-file -e "$BASE^{commit}" 2>/dev/null; then
  git fetch -q --depth=1 origin refs/tags/v0.1.14
fi
git cat-file -e "$BASE^{commit}"
test -x /tmp/ai-server-agent
TRUSTED_SOURCE_BINARY=/run/asa-trusted-source-bin/ai-server-agent
TRUSTED_OLD_BINARY=/run/asa-trusted-source-bin/old-agent
sudo test -x "$TRUSTED_SOURCE_BINARY"
sudo test ! -e "$TRUSTED_OLD_BINARY"
sudo test ! -e /etc/ai-server-agent/config.json
SCRATCH="$(mktemp -d)"
trap 'sudo rm -f -- "$TRUSTED_OLD_BINARY"; rm -rf -- "$SCRATCH"; unset direct_token gateway_token secondary_token' EXIT
mkdir -p "$SCRATCH/old-source"
git archive "$BASE" | tar -x -C "$SCRATCH/old-source"
(cd "$SCRATCH/old-source" && CGO_ENABLED=0 go build -trimpath -o "$SCRATCH/old-agent" ./cmd/ai-server-agent)
sudo install -o root -g root -m 0755 "$SCRATCH/old-agent" "$TRUSTED_OLD_BINARY"
sudo cmp -s "$SCRATCH/old-agent" "$TRUSTED_OLD_BINARY"
# systemd restart returning does not prove that the old/new HTTP listener
# has rebound its port yet. Wait for a real healthy listener, but never treat
# an unresponsive or crashing service as successful authentication.
await_healthy_listener(){
  local attempt
  for attempt in {1..40}; do
    if sudo systemctl is-active --quiet ai-server-agent.service &&
       curl --silent --fail --max-time 2 http://127.0.0.1:3210/healthz >/dev/null; then
      return 0
    fi
    sleep 0.25
  done
  echo 'AGENT_LISTENER_NOT_HEALTHY_AFTER_RESTART' >&2
  sudo systemctl is-active ai-server-agent.service >&2 || true
  sudo journalctl -u ai-server-agent.service --no-pager -n 25 -o cat 2>&1 | sed -E 's/[0-9a-f]{64}/[redacted]/g' >&2 || true
  return 1
}
token_status(){
  local token="$1"
  printf 'header = "Authorization: Bearer %s"\n' "$token" | curl -sS --max-time 7 --config - -o /dev/null -w '%{http_code}' http://127.0.0.1:3210/agent-environment.json
}
verify_tokens(){
  local primary="$1" secondary="$2" expected_primary="$3" expected_secondary="$4"
  test "$(token_status "$primary")" = "$expected_primary"
  test "$(token_status "$secondary")" = "$expected_secondary"
}
binary_digest(){ sudo sha256sum /usr/local/bin/ai-server-agent | awk '{print $1}'; }
config_digest(){ sudo sha256sum /etc/ai-server-agent/config.json | awk '{print $1}'; }
store_digest(){ sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | awk '{print $1}'; }
identity_digest(){ sudo sha256sum /etc/ai-server-agent/control/install-state.json | awk '{print $1}'; }
assert_rejected_preserving_install(){
  local candidate="$1" reason="$2" before_binary before_config before_store
  before_binary="$(binary_digest)"; before_config="$(config_digest)"; before_store="$(store_digest)"
  if sudo env AI_SERVER_AGENT_BINARY="$candidate" AI_SERVER_AGENT_NONINTERACTIVE=1 bash install.sh > "$SCRATCH/reject.log" 2>&1; then
    echo 'INCOMPATIBLE_INSTALL_WAS_ACCEPTED' >&2; exit 1
  fi
  grep -Fq "$reason" "$SCRATCH/reject.log" || { echo 'MISSING_PREFLIGHT_DIAGNOSTIC' >&2; exit 1; }
  test "$(binary_digest)" = "$before_binary"
  test "$(config_digest)" = "$before_config"
  test "$(store_digest)" = "$before_store"
  sudo systemctl is-active --quiet ai-server-agent.service
  sudo systemctl is-active --quiet ai-server-agent-executor.service
}
# The exact documented Stable bootstrap (with a historical tag) must refuse
# *before* invoking any old installer on an occupied host. Check the real
# installed state and its live services, not just a static guard string.
assert_bootstrap_rejected_preserving_install(){
  local stage="$1" before_binary before_config before_store
  before_binary="$(binary_digest)"; before_config="$(config_digest)"; before_store="$(store_digest)"
  if AI_SERVER_AGENT_NONINTERACTIVE=1 bash "$ROOT/scripts/install-stable.sh" v0.1.14 > "$SCRATCH/bootstrap-$stage.log" 2>&1; then
    echo "UNSAFE_STABLE_BOOTSTRAP_ACCEPTED_$stage" >&2; exit 1
  fi
  grep -Fq 'Stable bootstrap is for fresh installations only' "$SCRATCH/bootstrap-$stage.log" || {
    echo "MISSING_STABLE_BOOTSTRAP_REFUSAL_$stage" >&2; exit 1;
  }
  test "$(binary_digest)" = "$before_binary"
  test "$(config_digest)" = "$before_config"
  test "$(store_digest)" = "$before_store"
  sudo systemctl is-active --quiet ai-server-agent.service
  sudo systemctl is-active --quiet ai-server-agent-executor.service
}
# The published v0.1.14 installer is authentic but predates the hardened
# helper/systemd trust contract. Refuse even when credentials are compatible,
# before running historical installer bytes or changing installed services.
assert_protected_update_refused(){
  local before_binary before_config before_store before_identity
  before_binary="$(binary_digest)"; before_config="$(config_digest)"; before_store="$(store_digest)"; before_identity="$(identity_digest)"
  if sudo env AI_SERVER_AGENT_UPDATE_CHANNEL=stable AI_SERVER_AGENT_VERSION=v0.1.14 \
      bash "$ROOT/update.sh" > "$SCRATCH/update-refused.log" 2>&1; then
    echo 'UNSAFE_PROTECTED_UPDATER_ACCEPTED' >&2; exit 1
  fi
  grep -Fq 'lacks required privileged lifecycle/startup path trust' "$SCRATCH/update-refused.log" || {
    echo 'MISSING_PROTECTED_UPDATER_DIAGNOSTIC' >&2; exit 1;
  }
  test "$(binary_digest)" = "$before_binary"
  test "$(config_digest)" = "$before_config"
  test "$(store_digest)" = "$before_store"
  test "$(identity_digest)" = "$before_identity"
  sudo systemctl is-active --quiet ai-server-agent.service
  sudo systemctl is-active --quiet ai-server-agent-executor.service
}
issue_secret(){
  local kind="$1" transcript="$SCRATCH/$1.transcript" token
  umask 077
  script -qefc "sudo ai-server-agent-manage credential-rotate $kind" "$transcript" >/dev/null 2>&1
  token="$(grep -oE 'Authorization: Bearer [0-9a-f]{64}' "$transcript" | tail -n1 | cut -d' ' -f3)"
  test "${#token}" -eq 64
  rm -f -- "$transcript"
  printf '%s' "$token"
}
echo 'STAGE: build historical binary and establish valid direct client'
sudo env AI_SERVER_AGENT_BINARY="$TRUSTED_SOURCE_BINARY" AI_SERVER_AGENT_NONINTERACTIVE=1 bash install.sh >/dev/null
direct_token="$(issue_secret direct-default)"
test "$(token_status "$direct_token")" = 200
sudo env AI_SERVER_AGENT_BINARY="$TRUSTED_OLD_BINARY" AI_SERVER_AGENT_NONINTERACTIVE=1 bash install.sh >/dev/null
test "$(token_status "$direct_token")" = 200
echo 'STAGE: valid historical store with only gateway active'
gateway_token="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
gateway_verifier="$(printf '%s' "$gateway_token" | sha256sum | awk '{print $1}')"
sudo jq --arg gv "$gateway_verifier" '(.credentials[] | select(.principal.id=="direct-default")) |= (.enabled=false | .revoked_at="2026-10-09T00:00:00Z") | .credentials += [{principal:{id:"mcp-gateway",class:"gateway",name:"mcp-gateway"},verifier_algorithm:"sha256-v1",verifier:$gv,created_at:"2026-10-08T00:00:00Z",enabled:true}]' /etc/ai-server-agent/mcp-credentials.json > "$SCRATCH/gateway-only.json"
sudo install -o root -g aiagent -m 0640 "$SCRATCH/gateway-only.json" /etc/ai-server-agent/mcp-credentials.json
sudo systemctl restart ai-server-agent.service
await_healthy_listener
verify_tokens "$direct_token" "$gateway_token" 401 200
assert_rejected_preserving_install "$TRUSTED_SOURCE_BINARY" 'Existing MCP credentials are incompatible with the target Agent binary'
verify_tokens "$direct_token" "$gateway_token" 401 200
echo 'STAGE: restore direct/default explicitly, upgrade safely, reject old bearer'
direct_token="$(issue_secret direct-default)"
verify_tokens "$direct_token" "$gateway_token" 200 200
sudo env AI_SERVER_AGENT_BINARY="$TRUSTED_SOURCE_BINARY" AI_SERVER_AGENT_NONINTERACTIVE=1 bash install.sh >/dev/null
verify_tokens "$direct_token" "$gateway_token" 200 401
assert_bootstrap_rejected_preserving_install gateway-retained
assert_protected_update_refused
echo 'STAGE: block rollback that would reactivate a retired bearer'
assert_rejected_preserving_install "$TRUSTED_OLD_BINARY" 'Unsafe rollback credential state'
verify_tokens "$direct_token" "$gateway_token" 200 401
echo 'STAGE: block rollback with newly issued AND revoked secondary'
secondary_token="$(issue_secret direct-secondary)"
verify_tokens "$direct_token" "$secondary_token" 200 200
assert_bootstrap_rejected_preserving_install secondary-active
assert_rejected_preserving_install "$TRUSTED_OLD_BINARY" 'Unsafe rollback credential state'
sudo ai-server-agent-manage credential-revoke direct-secondary >/dev/null
verify_tokens "$direct_token" "$secondary_token" 200 401
assert_bootstrap_rejected_preserving_install secondary-revoked
assert_rejected_preserving_install "$TRUSTED_OLD_BINARY" 'Unsafe rollback credential state'
echo 'STAGE: explicit rollback preparation cancellation and failed-restart recovery'
before_failure="$(store_digest)"
if sudo env AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1 ASA_RESTART_MARKER="$SCRATCH/forced-restart-failure" bash -c '
  source /usr/local/lib/ai-server-agent/manage.sh
  ui_has_interactive_tty(){ return 0; }
  confirm(){ return 0; }
  credential_restart_and_wait(){
    if [ ! -e "$ASA_RESTART_MARKER" ]; then touch "$ASA_RESTART_MARKER"; return 1; fi
    /usr/bin/systemctl is-active --quiet ai-server-agent.service
  }
  credential_prepare_rollback
' > "$SCRATCH/rollback-failure.log" 2>&1; then
  echo 'INJECTED_ROLLBACK_PREPARATION_FAILURE_UNEXPECTEDLY_SUCCEEDED' >&2; exit 1
fi
test -e "$SCRATCH/forced-restart-failure"
test "$(store_digest)" = "$before_failure"
verify_tokens "$direct_token" "$secondary_token" 200 401
before_cancel="$(store_digest)"
if printf 'n\n' | script -qefc 'sudo ai-server-agent-manage credential-prepare-rollback' "$SCRATCH/cancel.log" >/dev/null 2>&1; then
  echo 'CANCELLED_CREDENTIAL_REMOVAL_UNEXPECTEDLY_SUCCEEDED' >&2; exit 1
fi
test "$(store_digest)" = "$before_cancel"
direct_verifier="$(sudo jq -r '.credentials[] | select(.principal.id=="direct-default") | .verifier' /etc/ai-server-agent/mcp-credentials.json)"
printf 'y\n' | script -qefc 'sudo ai-server-agent-manage credential-prepare-rollback' "$SCRATCH/prepare.log" >/dev/null 2>&1 || { echo 'EXPLICIT_ROLLBACK_PREPARATION_FAILED' >&2; exit 1; }
test "$(sudo jq '.credentials | length' /etc/ai-server-agent/mcp-credentials.json)" -eq 1
test "$(sudo jq -r '.credentials[0].principal.id' /etc/ai-server-agent/mcp-credentials.json)" = direct-default
test "$(sudo jq -r '.credentials[0].verifier' /etc/ai-server-agent/mcp-credentials.json)" = "$direct_verifier"
verify_tokens "$direct_token" "$secondary_token" 200 401
assert_bootstrap_rejected_preserving_install direct-only
echo 'STAGE: published historical rollback remains refused after explicit credential cleanup'
assert_protected_update_refused
sudo jq -e '.channel=="source" and .version=="source"' /etc/ai-server-agent/control/install-state.json >/dev/null
verify_tokens "$direct_token" "$gateway_token" 200 401
verify_tokens "$direct_token" "$secondary_token" 200 401
echo 'STAGE: compatible source reinstall still preserves direct credential'
sudo env AI_SERVER_AGENT_BINARY="$TRUSTED_SOURCE_BINARY" AI_SERVER_AGENT_NONINTERACTIVE=1 bash install.sh >/dev/null
test "$(token_status "$direct_token")" = 200
sudo env AI_SERVER_AGENT_YES=1 bash uninstall.sh --purge >/dev/null
sudo test ! -e /etc/ai-server-agent
echo 'INSTALLED_CREDENTIAL_UPGRADE_ROLLBACK_SAFETY_PASS historical_immutable_downgrade=refused source_reinstall=verified'
