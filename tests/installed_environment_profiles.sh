#!/usr/bin/env bash
# Real installed, authenticated direct-MCP environment-profile acceptance.
# ONLY for a disposable Ubuntu CI installation. Never run against occupied hosts.
set -Eeuo pipefail

[ "$(id -u)" -ne 0 ] || { echo 'Run as the disposable CI runner, not root' >&2; exit 1; }
for tool in curl jq sudo script; do command -v "$tool" >/dev/null || exit 1; done
test -x /usr/local/bin/ai-server-agent
test -x /usr/local/sbin/ai-server-agent-manage
test -x /tmp/ai-server-agent
sudo cmp -s /tmp/ai-server-agent /usr/local/bin/ai-server-agent
sudo systemctl is-active --quiet ai-server-agent.service
sudo systemctl is-active --quiet ai-server-agent-executor.service

IFS= read -r bearer
[[ "$bearer" =~ ^[0-9a-f]{64}$ ]] || { echo 'Invalid disposable-client credential' >&2; exit 1; }
scratch="$(mktemp -d)"
chmod 0700 "$scratch"
trap 'unset bearer; rm -rf -- "$scratch"' EXIT

fail(){ echo "INSTALLED PROFILE ACCEPTANCE FAIL: $*" >&2; exit 1; }
mcp_request(){
  local payload="$1" output="$2" status
  status="$(printf 'header = "Authorization: Bearer %s"\n' "$bearer" |
    curl --fail-with-body -sS --max-time 20 --config - \
      -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' \
      -H 'MCP-Protocol-Version: 2025-11-25' \
      --data-binary "$payload" -o "$output" -w '%{http_code}' \
      http://127.0.0.1:3210/mcp)" || fail 'Direct MCP transport request failed'
  [ "$status" = 200 ] || fail "Direct MCP HTTP status=$status"
  [ "$(stat -c '%s' "$output")" -le 1048576 ] || fail 'Direct MCP response exceeded 1 MiB'
  jq -e '.jsonrpc=="2.0" and (.error==null)' "$output" >/dev/null ||
    fail 'Direct MCP returned a protocol error'
}
expected_instance="$(sudo jq -er '.instance_id' /etc/ai-server-agent/config.json)"
[[ "$expected_instance" =~ ^asa_[0-9a-f]{32}$ ]] || fail 'Invalid installed instance identity'
credential_digest="$(sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | awk '{print $1}')"
executor_pid="$(sudo systemctl show --property=MainPID --value ai-server-agent-executor.service)"
[[ "$executor_pid" =~ ^[1-9][0-9]*$ ]] || fail 'Executor has no live PID'

check_profile(){
  local role="$1" label="$2" manifest
  sudo systemctl is-active --quiet ai-server-agent.service || fail 'Agent service inactive'
  sudo systemctl is-active --quiet ai-server-agent-executor.service || fail 'Executor service inactive'
  sudo jq -e --arg role "$role" --arg server_label "$label" --arg id "$expected_instance" \
    '.environment_profile==$role and (.server_label // "")==$server_label and .instance_id==$id' \
    /etc/ai-server-agent/config.json >/dev/null || fail 'Installed config role/label/identity changed'
  sudo jq -e --arg role "$role" --arg server_label "$label" --arg id "$expected_instance" \
    '.environment_profile==$role and (.server_label // "")==$server_label and .instance_id==$id' \
    /var/lib/ai-server-agent/AI_ENVIRONMENT.json >/dev/null || fail 'Effective manifest stale'
  sudo ai-server-agent-manage profile-show > "$scratch/profile-show"
  grep -Fq "Profile: $role" "$scratch/profile-show" || fail 'Installed profile-show disagrees'
  if [ -n "$label" ]; then
    grep -Fq "Label:   $label" "$scratch/profile-show" || fail 'Installed profile label hidden'
  fi

  mcp_request '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"ci-installed-purpose-client","version":"1"}}}' "$scratch/initialize"
  jq -e --arg role "$role" --arg server_label "$label" '
    .result.serverInfo.name=="ai-server-agent" and
    (.result.instructions | contains("Environment purpose: " + $role)) and
    (.result.instructions | contains("guidance; it is not an authorization")) and
    (if $server_label=="" then true else (.result.instructions | contains("Server label: " + $server_label)) end)
  ' "$scratch/initialize" >/dev/null || fail 'Real MCP initialization contradicts effective role'
  if [ "$role" = production ]; then
    jq -e '.result.instructions |
      contains("Before each change") and contains("never reuse prior approval") and
      contains("Agent cannot verify consent")' "$scratch/initialize" >/dev/null ||
      fail 'Production MCP instructions lack fresh-consent warning'
  fi

  mcp_request '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"agent_environment","arguments":{}}}' "$scratch/environment"
  manifest="$(jq -er '.result.content[0].text' "$scratch/environment")" ||
    fail 'Direct MCP environment text was missing'
  jq -e --arg role "$role" --arg server_label "$label" --arg id "$expected_instance" '
    .environment_profile==$role and (.server_label // "")==$server_label and
    .instance_id==$id and .workspace_dir=="/srv/ai-workspace"
  ' <<< "$manifest" >/dev/null || fail 'Direct MCP environment did not reflect installed state'
  if [ "$role" = production ]; then
    jq -e '.purpose |
      contains("passive read-only inspection") and
      contains("fresh, action-specific permission") and
      contains("does not verify such consent")' <<< "$manifest" >/dev/null ||
      fail 'Production manifest missing honest advisory guidance'
  fi

  mcp_request '{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}' "$scratch/tools"
  jq -ce '[.result.tools[] | {name,description}] | sort_by(.name)' "$scratch/tools" > "$scratch/current-tools" ||
    fail 'Direct MCP tool inventory missing'
  jq -e '(.result.tools | length) >= 40 and
    ([.result.tools[].name] | index("run_command") != null and
    index("run_root_command") != null and index("agent_environment") != null)' \
    "$scratch/tools" >/dev/null || fail 'Profile hides baseline worker/root tools'
  if [ -e "$scratch/baseline-tools" ]; then
    cmp -s "$scratch/current-tools" "$scratch/baseline-tools" ||
      fail 'Profile switch changed direct MCP tool metadata'
  else
    cp "$scratch/current-tools" "$scratch/baseline-tools"
  fi

  mcp_request '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"run_command","arguments":{"command":"id -un"}}}' "$scratch/worker"
  jq -e '.result.isError!=true and .result.structuredContent.ok==true and
    (.result.structuredContent.output | test("^aiworker\\s*$"))' "$scratch/worker" >/dev/null ||
    fail 'Worker execution changed under purpose profile'
  mcp_request '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"run_root_command","arguments":{"command":"id -un","approval":true}}}' "$scratch/root"
  jq -e '.result.isError!=true and .result.structuredContent.ok==true and
    (.result.structuredContent.output | test("^root\\s*$"))' "$scratch/root" >/dev/null ||
    fail 'Authorized root capability changed under purpose profile'

  [ "$(sudo systemctl show --property=MainPID --value ai-server-agent-executor.service)" = "$executor_pid" ] ||
    fail 'Purpose-only profile change restarted Executor'
  [ "$(sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | awk '{print $1}')" = "$credential_digest" ] ||
    fail 'Purpose-only profile change modified credentials'
}

check_profile validation ''
sudo ai-server-agent-manage profile-set development >/dev/null
check_profile development ''
sudo ai-server-agent-manage profile-label 'Disposable CI' >/dev/null
check_profile development 'Disposable CI'
sudo ai-server-agent-manage profile-set production >/dev/null
check_profile production 'Disposable CI'
# Preserve the normal systemd start-rate budget before another profile switch.
sleep 11
sudo ai-server-agent-manage profile-set validation >/dev/null
check_profile validation 'Disposable CI'

# Fresh install, credential rotation and several rapid profile switches can
# consume systemd's normal five-starts-per-ten-seconds budget. Allow that
# protection window to expire before fault injection, rather than resetting
# service counters or weakening the installed unit's start limit.
sleep 11

# Fail exactly the first Agent restart: the real manager must restore the
# previous config, re-activate its service, and clear the verified rollback.
before="$(sudo sha256sum /etc/ai-server-agent/config.json | awk '{print $1}')"
mkdir "$scratch/bin"
cat > "$scratch/bin/systemctl" <<'EOF_SYSTEMCTL'
#!/usr/bin/env bash
if [ "$1" = restart ] && [ "$2" = ai-server-agent.service ] &&
   [ ! -e "$PROFILE_CI_FAIL_ONCE_MARKER" ]; then
  touch "$PROFILE_CI_FAIL_ONCE_MARKER"
  exit 93
fi
exec /usr/bin/systemctl "$@"
EOF_SYSTEMCTL
chmod 0755 "$scratch/bin/systemctl"
if sudo env AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1 \
    PROFILE_CI_FAIL_ONCE_MARKER="$scratch/restart-failed" \
    PATH="$scratch/bin:/usr/bin:/bin" \
    bash -c 'source /usr/local/lib/ai-server-agent/manage.sh; profile_apply environment_profile development' \
    >"$scratch/injected-failure" 2>&1; then
  fail 'Injected service restart failure falsely succeeded'
fi
test -f "$scratch/restart-failed" || fail 'Failure injector never exercised real restart'
if ! grep -Fq 'previous configuration and Agent health were restored' "$scratch/injected-failure"; then
  # Only emit bounded, static management status lines, never the private
  # credential or the complete process environment.
  grep -F -e 'Purpose update failed' -e 'Environment update failed' -e 'Another lifecycle operation' -e 'ERROR:' -e 'WARNING:' "$scratch/injected-failure" | head -n 10 >&2 || true
  if [ "$(sudo sha256sum /etc/ai-server-agent/config.json | awk '{print $1}')" = "$before" ]; then
    echo 'PROFILE_ROLLBACK_DIAGNOSTIC config_restored=yes' >&2
  else
    echo 'PROFILE_ROLLBACK_DIAGNOSTIC config_restored=no' >&2
  fi
  if sudo systemctl is-active --quiet ai-server-agent.service; then
    echo 'PROFILE_ROLLBACK_DIAGNOSTIC agent_active=yes' >&2
  else
    echo 'PROFILE_ROLLBACK_DIAGNOSTIC agent_active=no' >&2
  fi
  fail 'Manager rollback was not verified'
fi
[ "$(sudo sha256sum /etc/ai-server-agent/config.json | awk '{print $1}')" = "$before" ] ||
  fail 'Failed activation changed old configuration'
test -z "$(sudo find /etc/ai-server-agent -maxdepth 1 -name '.profile-old.*' -print)" ||
  fail 'Verified rollback left a protected pending snapshot'
check_profile validation 'Disposable CI'

# Invalid input must fail before any config mutation/restart.
if sudo ai-server-agent-manage profile-set invalid >"$scratch/invalid" 2>&1; then
  fail 'Invalid role was accepted'
fi
if sudo ai-server-agent-manage profile-label $'bad\nlabel' >"$scratch/invalid" 2>&1; then
  fail 'Invalid label was accepted'
fi
[ "$(sudo sha256sum /etc/ai-server-agent/config.json | awk '{print $1}')" = "$before" ] ||
  fail 'Invalid input mutated installed configuration'
check_profile validation 'Disposable CI'

# Re-invoke the actual source installer on the disposable runner. Inputs for
# an EXISTING install must not replace the persisted profile or label.
sudo env AI_SERVER_AGENT_BINARY=/tmp/ai-server-agent \
  AI_SERVER_AGENT_NONINTERACTIVE=1 \
  AI_SERVER_AGENT_ENVIRONMENT_PROFILE=development \
  AI_SERVER_AGENT_SERVER_LABEL='Ignored on reinstall' \
  bash install.sh >"$scratch/reinstall" 2>&1 || fail 'State-preserving reinstall failed'
sudo cmp -s /tmp/ai-server-agent /usr/local/bin/ai-server-agent ||
  fail 'Reinstall binary diverged from the tested source'
executor_pid="$(sudo systemctl show --property=MainPID --value ai-server-agent-executor.service)"
check_profile validation 'Disposable CI'
sudo ai-server-agent-manage profile-label-clear >/dev/null
check_profile validation ''
printf 'INSTALLED_ENVIRONMENT_PROFILE_MCP_ACCEPTANCE_PASS roles=2 switches=2 rollback=verified reinstall=preserved authenticated_client=yes\n'
