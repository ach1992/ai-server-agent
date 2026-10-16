#!/usr/bin/env bash
# Real installed runtime acceptance. ONLY on disposable, isolated GitHub Actions.
set -Eeuo pipefail
fail(){ printf 'INSTALLED RUNTIME ACCEPTANCE FAIL: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -ne 0 ] || fail 'Run as CI user, not root'
[ "$GITHUB_ACTIONS" = true ] && [ "$ASA_DISPOSABLE_RUNTIME_ACCEPTANCE" = 1 ] || fail 'Disposable CI opt-in required'
for x in curl jq sudo sha256sum systemctl; do command -v "$x" >/dev/null || fail "Missing $x"; done
sudo cmp -s /tmp/ai-server-agent /usr/local/bin/ai-server-agent || fail 'Not an installed exact candidate'
sudo systemctl is-active --quiet ai-server-agent.service || fail 'Agent inactive'
sudo systemctl is-active --quiet ai-server-agent-executor.service || fail 'Executor inactive'
IFS= read -r bearer
[[ "$bearer" =~ ^[0-9a-f]{64}$ ]] || fail 'Invalid private CI bearer'
scratch="$(mktemp -d)"
chmod 0700 "$scratch"
fixture="$(sudo mktemp -d /srv/ai-workspace/ci-runtime-acceptance.XXXXXXXX)"
case "$fixture" in /srv/ai-workspace/ci-runtime-acceptance.[A-Za-z0-9]*) ;; *) fail 'Unsafe fixture';; esac
sudo chown aiworker:aiworker "$fixture"
sudo chmod 0700 "$fixture"
job_ids=()
transfer_ids=()
mcp_request(){
  local payload="$1" output="$2" code
  code="$(printf 'header = "Authorization: Bearer %s"\n' "$bearer" |
    curl -fsS --max-time 28 --config - -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-11-25' \
      --data-binary "$payload" -o "$output" -w '%{http_code}' http://127.0.0.1:3210/mcp)" ||
    fail 'Authenticated MCP transport failed'
  [ "$code" = 200 ] || fail "Authenticated MCP HTTP $code"
  [ "$(stat -c '%s' "$output")" -le 1048576 ] || fail 'Unbounded MCP response'
  jq -e '.jsonrpc=="2.0" and (.error==null)' "$output" >/dev/null || fail 'MCP protocol error'
}
mcp_call(){
  local request
  request="$(jq -cn --arg name "$1" --argjson arguments "$2" \
    '{jsonrpc:"2.0",id:2,method:"tools/call",params:{name:$name,arguments:$arguments}}')"
  mcp_request "$request" "$scratch/response"
}
check_ok(){
  jq -e '.result.isError!=true and .result.structuredContent.ok==true' "$scratch/response" >/dev/null ||
    fail "MCP call failed: $1"
}
check_limit(){
  jq -e '.result.isError==true and .result.structuredContent.error_code=="resource_limit"' \
    "$scratch/response" >/dev/null || fail "Missing resource_limit denial: $1"
}
cleanup(){
  sudo touch "$fixture/go" 2>/dev/null || true
  for job_id in "${job_ids[@]}"; do
    if [[ "$job_id" =~ ^[0-9]+$ ]]; then
      body="$(jq -cn --arg job_id "$job_id" '{job_id:$job_id}')"
      mcp_call job_stop "$body" >/dev/null 2>&1 || true
    fi
  done
  for entry in "${transfer_ids[@]}"; do
    id="${entry%%:*}"
    path="${entry#*:}"
    if [[ "$id" =~ ^[A-Za-z0-9_-]+$ ]] && [[ "$path" =~ ^[a-z0-9_.-]+$ ]]; then
      body="$(jq -cn --arg workspace "$fixture" --arg path "$path" --arg transfer_id "$id" \
        '{workspace:$workspace,path:$path,operation:"abort",transfer_id:$transfer_id}')"
      mcp_call workspace_transfer "$body" >/dev/null 2>&1 || true
    fi
  done
  unset bearer
  rm -rf -- "$scratch"
  sudo rm -rf -- "$fixture"
}
trap cleanup EXIT
set_runtime(){
  sleep 11 # Preserve normal systemd start-rate windows.
  sudo ai-server-agent-manage runtime-set "$1" "$2" >"$scratch/manage" 2>&1 ||
    fail "runtime-set failed: $1"
  sudo jq -e --arg key "$1" --argjson n "$2" '.runtime[$key]==$n' \
    /etc/ai-server-agent/config.json >/dev/null || fail "Setting not persisted: $1"
  sudo systemctl is-active --quiet ai-server-agent.service || fail 'Agent unhealthy after set'
  sudo systemctl is-active --quiet ai-server-agent-executor.service || fail 'Executor unhealthy after set'
}
reset_runtime(){
  sleep 11
  sudo ai-server-agent-manage runtime-reset "$1" >"$scratch/manage" 2>&1 ||
    fail "runtime-reset failed: $1"
  sudo jq -e --arg key "$1" '.runtime[$key]==null' /etc/ai-server-agent/config.json >/dev/null ||
    fail "Reset not persisted: $1"
  sudo systemctl is-active --quiet ai-server-agent.service || fail 'Agent unhealthy after reset'
  sudo systemctl is-active --quiet ai-server-agent-executor.service || fail 'Executor unhealthy after reset'
}
capacity(){
  mcp_call runtime_list '{}'
  check_ok runtime_list
  jq -e --argjson limit "$1" --argjson tasks "$2" --argjson services "$3" '
    .result.structuredContent.runtime_capacity |
    .state_reliable==true and .task_limit==$limit and .service_limit==2 and
    .tasks_active==$tasks and .services_active==$services and .unclassified_active==0
  ' "$scratch/response" >/dev/null || fail "Capacity wrong: task-limit=$1 active=$2 services=$3"
}
start_job(){
  local request
  request="$(jq -cn --arg workspace "$fixture" --arg kind "$1" \
    --arg label "ci118_$1" --arg operation_id "ci118-$1-$2-stable" \
    '{workspace:$workspace,kind:$kind,command:"sleep 180",label:$label,operation_id:$operation_id}')"
  mcp_call start_job "$request"
}
begin_transfer(){
  local request
  request="$(jq -cn --arg workspace "$fixture" --arg path "$2" --arg key "$1" \
    --argjson size "$3" '{workspace:$workspace,path:$path,operation:"begin",transfer_key:$key,expected_size:$size,sha256:("0"*64),must_not_exist:true}')"
  mcp_call workspace_transfer "$request"
}
abort_transfer(){
  local request
  request="$(jq -cn --arg workspace "$fixture" --arg path "$2" --arg transfer_id "$1" \
    '{workspace:$workspace,path:$path,operation:"abort",transfer_id:$transfer_id}')"
  mcp_call workspace_transfer "$request"
  check_ok 'transfer abort'
}

instance_before="$(sudo jq -er '.instance_id' /etc/ai-server-agent/config.json)"
credential_before="$(sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | awk '{print $1}')"
sudo ai-server-agent-manage runtime-show >"$scratch/defaults"
grep -Fq 'command_timeout_seconds=300 seconds' "$scratch/defaults" || fail 'Default timeout not visible'
grep -Fq 'persistent_task_slots=4 tasks' "$scratch/defaults" || fail 'Default task capacity not visible'
mcp_request '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"ci-runtime-acceptance","version":"1"}}}' "$scratch/initialize"
jq -e '.result.serverInfo.name=="ai-server-agent"' "$scratch/initialize" >/dev/null || fail 'Initialize failed'
capacity 4 0 0

# Genuine eight-at-once worker command admission, not just configuration text.
set_runtime worker_command_slots 8
grep -Fq 'worker_command_slots=8 slots' <(sudo ai-server-agent-manage runtime-show) ||
  fail 'Worker effective override missing'
pids=()
for n in $(seq 1 8); do
  command="touch start-$n; while test ! -e go; do sleep 0.2; done; printf RELEASED"
  body="$(jq -cn --arg workspace "$fixture" --arg command "$command" '{workspace:$workspace,command:$command}')"
  payload="$(jq -cn --argjson args "$body" '{jsonrpc:"2.0",id:5,method:"tools/call",params:{name:"run_command",arguments:$args}}')"
  mcp_request "$payload" "$scratch/parallel-$n" &
  pids+=("$!")
done
started=0
for attempt in $(seq 1 90); do
  started="$(sudo find "$fixture" -maxdepth 1 -type f -name 'start-*' | wc -l)"
  [ "$started" -eq 8 ] && break
  sleep 0.2
done
sudo touch "$fixture/go"
for pid in "${pids[@]}"; do wait "$pid" || fail 'Parallel command failed'; done
[ "$started" -eq 8 ] || fail "Eight worker commands were not concurrent: $started"
for n in $(seq 1 8); do
  jq -e '.result.structuredContent.ok==true and (.result.structuredContent.output | contains("RELEASED"))' \
    "$scratch/parallel-$n" >/dev/null || fail "Parallel worker $n failed"
done

# Eight real systemd tasks survive an eight-to-one Executor restart.
set_runtime persistent_task_slots 8
capacity 8 0 0
for n in $(seq 1 8); do
  start_job task "$n"
  check_ok "task $n"
  job_id="$(jq -er '.result.structuredContent.job_id' "$scratch/response")"
  [[ "$job_id" =~ ^[0-9]+$ ]] || fail 'Invalid systemd task identity'
  job_ids+=("$job_id")
done
capacity 8 8 0
start_job task 9
check_limit 'Task 9 at 8 slots'
set_runtime persistent_task_slots 1
capacity 1 8 0
for job_id in "${job_ids[@]}"; do
  body="$(jq -cn --arg job_id "$job_id" '{job_id:$job_id}')"
  mcp_call job_status "$body"
  check_ok "Original task lost after restart: $job_id"
done
start_job task 10
check_limit 'Task after downscale'
for n in 1 2; do
  start_job service "$n"
  check_ok "Independent service $n"
  job_id="$(jq -er '.result.structuredContent.job_id' "$scratch/response")"
  [[ "$job_id" =~ ^[0-9]+$ ]] || fail 'Invalid service identity'
  job_ids+=("$job_id")
done
capacity 1 8 2
start_job service 3
check_limit 'Third independent service'
for job_id in "${job_ids[@]}"; do
  body="$(jq -cn --arg job_id "$job_id" '{job_id:$job_id}')"
  mcp_call job_stop "$body"
  check_ok "Stopping $job_id"
done
for attempt in $(seq 1 30); do
  mcp_call runtime_list '{}'
  if jq -e '.result.structuredContent.ok==true and .result.structuredContent.runtime_capacity.tasks_active==0 and .result.structuredContent.runtime_capacity.services_active==0' "$scratch/response" >/dev/null; then
    break
  fi
  sleep 1
done
capacity 1 0 0

# Real transfer quota and session admission; abort must restore capacity.
set_runtime transfer_staging_mib 16
set_runtime transfer_active_sessions 1
begin_transfer ci118-first-transfer-key-1 quota-one.bin 9437184
check_ok 'First nine-megabyte transfer reservation'
first_id="$(jq -er '.result.structuredContent.transfer_id' "$scratch/response")"
[[ "$first_id" =~ ^[A-Za-z0-9_-]+$ ]] || fail 'Bad transfer ID'
transfer_ids+=("$first_id:quota-one.bin")
begin_transfer ci118-second-transfer-key-2 quota-two.bin 9437184
jq -e '.result.isError==true and .result.structuredContent.error_code=="transfer_quota_exceeded"' "$scratch/response" >/dev/null ||
  fail 'Transfer sessions bypassed quota'
abort_transfer "$first_id" quota-one.bin
begin_transfer ci118-second-transfer-key-2 quota-two.bin 9437184
check_ok 'Abort did not free quota'
second_id="$(jq -er '.result.structuredContent.transfer_id' "$scratch/response")"
transfer_ids+=("$second_id:quota-two.bin")
abort_transfer "$second_id" quota-two.bin
sudo test ! -e "$fixture/quota-one.bin" && sudo test ! -e "$fixture/quota-two.bin" ||
  fail 'Aborted data was published'

# Real timing verifies configured-versus-effective command timeout.
set_runtime command_timeout_seconds 10
grep -Fq 'command_timeout_seconds=10 seconds' <(sudo ai-server-agent-manage runtime-show) ||
  fail 'Operator timeout not shown'
mcp_call run_command '{"command":"sleep 12"}'
jq -e '.result.structuredContent.timed_out==true' "$scratch/response" >/dev/null ||
  fail '10-second operator timeout was not effective'
reset_runtime command_timeout_seconds
mcp_call run_command '{"command":"sleep 11; printf RESTORED"}'
check_ok 'Command failed after timeout reset'
jq -e '.result.structuredContent.output | contains("RESTORED")' "$scratch/response" >/dev/null ||
  fail 'Reset timeout remained effective'

# One-shot real restart failure and verified exact rollback, not a mocked manager.
sleep 11
before="$(sudo sha256sum /etc/ai-server-agent/config.json | awk '{print $1}')"
mkdir "$scratch/bin"
cat > "$scratch/bin/systemctl" <<'SYSTEMCTL'
#!/usr/bin/env bash
if [ "$1" = restart ] && [ ! -e "$RUNTIME_CI_FAIL_ONCE" ]; then
  touch "$RUNTIME_CI_FAIL_ONCE"
  exit 93
fi
exec /usr/bin/systemctl "$@"
SYSTEMCTL
chmod 0755 "$scratch/bin/systemctl"
if sudo env AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1 \
  RUNTIME_CI_FAIL_ONCE="$scratch/restart-failed" PATH="$scratch/bin:/usr/bin:/bin" \
  bash -c 'source /usr/local/lib/ai-server-agent/manage.sh; runtime_apply set command_timeout_seconds 15' \
  >"$scratch/injected" 2>&1; then
  fail 'Failed restart falsely applied new settings'
fi
test -f "$scratch/restart-failed" || fail 'Restart fault injector did not run'
grep -Fq 'old config and local service health restored' "$scratch/injected" ||
  fail 'Rollback was not fully health verified'
[ "$(sudo sha256sum /etc/ai-server-agent/config.json | awk '{print $1}')" = "$before" ] ||
  fail 'Failed restart changed original configuration'
test -z "$(sudo find /etc/ai-server-agent -maxdepth 1 -name '.runtime-old.*' -print)" ||
  fail 'Unresolved snapshot remained after healthy rollback'
sudo systemctl is-active --quiet ai-server-agent.service || fail 'Agent unhealthy after rollback'
sudo systemctl is-active --quiet ai-server-agent-executor.service || fail 'Executor unhealthy after rollback'
[ "$(sudo jq -er '.instance_id' /etc/ai-server-agent/config.json)" = "$instance_before" ] ||
  fail 'Instance identity changed'
[ "$(sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | awk '{print $1}')" = "$credential_before" ] ||
  fail 'Credentials mutated'
mcp_call runtime_list '{}'
check_ok 'Authenticated client unavailable after rollback'
sudo cmp -s /tmp/ai-server-agent /usr/local/bin/ai-server-agent || fail 'Installed binary changed'
printf 'INSTALLED_RUNTIME_CONFIG_ACCEPTANCE_PASS worker_slots=8 task_slots=8_to_1 old_tasks=preserved services=independent transfer_quota=bounded abort=recovered timeout=set_reset rollback=verified authenticated_client=yes\n'
