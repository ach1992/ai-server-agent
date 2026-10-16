#!/usr/bin/env bash
# Genuine installed-Agent / root-executor / worker Go environment acceptance.
# Run ONLY on a disposable CI/test installation with an already authenticated
# direct MCP bearer supplied on stdin. Never invoke on occupied Agent hosts.
set -Eeuo pipefail

[ "$(id -u)" -ne 0 ] || { echo "run this test as the disposable runner, not root" >&2; exit 1; }
command -v curl >/dev/null && command -v jq >/dev/null && command -v sudo >/dev/null
test -d /srv/ai-workspace
test -x /usr/local/bin/ai-server-agent
test -x /usr/local/sbin/ai-server-agent-manage
sudo systemctl is-active --quiet ai-server-agent.service
sudo systemctl is-active --quiet ai-server-agent-executor.service

# The installed Agent stores only the verifier. Borrow the ephemeral CI bearer
# from an already validated migration test; never echo, export or log it.
IFS= read -r bearer
[[ "$bearer" =~ ^[0-9a-f]{64}$ ]] || { echo 'invalid CI-only MCP bearer' >&2; exit 1; }

# GitHub's setup-go binary normally lives outside the Agent's trusted system
# search paths. Only on this disposable runner, provision the actual Go binary
# into a root-owned, production-discoverable path if neither is occupied.
if [ ! -e /usr/local/bin/go ] && [ ! -L /usr/local/bin/go ] &&
   [ ! -e /usr/bin/go ] && [ ! -L /usr/bin/go ]; then
  ci_go="$(command -v go)"
  [ -x "$ci_go" ] || { echo 'CI Go executable missing' >&2; exit 1; }
  sudo install -m 0755 -o root -g root "$ci_go" /usr/local/bin/go
fi
go_available=false
for candidate in /usr/local/bin/go /usr/bin/go; do
  if [ -e "$candidate" ] || [ -L "$candidate" ]; then
    test -f "$candidate" && test ! -L "$candidate"
    test "$(stat -c '%u:%a' "$candidate")" = '0:755'
    "$candidate" version | grep -Eq '^go version go'
    go_available=true
    break
  fi
done
[ "$go_available" = true ] || { echo 'no trusted Go executable available for installed probe' >&2; exit 1; }

fixture="$(sudo mktemp -d /srv/ai-workspace/ci-installed-go-probe.XXXXXXXX)"
scratch="$(mktemp -d)"
chmod 0700 "$scratch"
cleanup(){
  unset bearer
  rm -rf -- "$scratch"
  # Never delete a workspace path unless it matches this one-use test prefix.
  case "$fixture" in
    /srv/ai-workspace/ci-installed-go-probe.[A-Za-z0-9]*) sudo rm -rf -- "$fixture" ;;
    *) echo 'refusing unexpected test fixture cleanup path' >&2; return 1 ;;
  esac
}
trap cleanup EXIT
sudo chown aiworker:aiworker "$fixture"
sudo chmod 0700 "$fixture"
sudo -u aiworker git -C "$fixture" init -q
printf 'module example.invalid/installed-go-probe\n\ngo 1.26.0\n' |
  sudo -u aiworker tee "$fixture/go.mod" >/dev/null
sudo -u aiworker git -C "$fixture" add go.mod
sudo -u aiworker git -C "$fixture" -c user.name='CI Acceptance' \
  -c user.email='ci-acceptance@example.invalid' commit -qm 'create Go probe fixture'

# Count existing probe dirs without touching them; an installed Agent must not
# recreate a deleted Go telemetry HOME after returning its MCP response.
find_probes(){
  find /tmp /var/tmp -mindepth 1 -maxdepth 1 -type d \
    -name 'ai-server-agent-env-probe-*' -print 2>/dev/null | LC_ALL=C sort
}
find_probes > "$scratch/before"
# Exercise the same initialize handshake as a real MCP client; never use a
# fabricated executor-only shortcut to bypass bearer and transport checks.
initialize='{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"ci-installed-go-probe","version":"1"}}}'
init_status="$(printf 'header = "Authorization: Bearer %s"\n' "$bearer" |
  curl --fail-with-body -sS --max-time 20 --config - \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -H 'MCP-Protocol-Version: 2025-11-25' \
    --data-binary "$initialize" \
    -o "$scratch/initialize" -w '%{http_code}' http://127.0.0.1:3210/mcp)"
[ "$init_status" = 200 ] &&
  jq -e '.result.serverInfo.name == "ai-server-agent"' "$scratch/initialize" >/dev/null || {
    echo 'installed direct MCP initialize failed' >&2
    exit 1
  }
for attempt in 1 2 3 4 5 6; do
  body="$(jq -cn --argjson id "$attempt" --arg path "$fixture" \
    '{jsonrpc:"2.0",id:$id,method:"tools/call",params:{name:"repository_environment",arguments:{path:$path}}}')"
  response="$scratch/response"
  http_status="$(printf 'header = "Authorization: Bearer %s"\n' "$bearer" |
    curl --fail-with-body -sS --max-time 20 --config - \
      -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' \
      -H 'MCP-Protocol-Version: 2025-11-25' \
      --data-binary "$body" \
      -o "$response" -w '%{http_code}' http://127.0.0.1:3210/mcp)"
  [ "$http_status" = 200 ] || { echo "installed MCP probe failed HTTP $http_status" >&2; exit 1; }
  [ "$(stat -c '%s' "$response")" -le 131072 ] || { echo 'oversized MCP discovery response' >&2; exit 1; }
  jq -e '
    .jsonrpc == "2.0" and
    (.error == null) and
    (.result.isError != true) and
    ([.result.structuredContent.tools[]? |
      select(.executable == "go" and
        .source == "system_path" and
        ((.version // "") | startswith("go version go")))] | length) > 0
  ' "$response" >/dev/null || {
    echo 'installed MCP repository_environment did not return a genuine Go version' >&2
    exit 1
  }
  find_probes > "$scratch/after"
  cmp -s "$scratch/before" "$scratch/after" || {
    echo 'Go discovery leaked a temporary telemetry HOME' >&2
    exit 1
  }
done
for delay in 1 3; do
  sleep "$delay"
  find_probes > "$scratch/after"
  cmp -s "$scratch/before" "$scratch/after" || {
    echo 'Go telemetry HOME reappeared after delayed installed MCP probe' >&2
    exit 1
  }
done
sudo systemctl is-active --quiet ai-server-agent.service
sudo systemctl is-active --quiet ai-server-agent-executor.service
printf 'INSTALLED_GO_ENVIRONMENT_MCP_NO_PROBE_LEAK_PASS calls=6 delayed_seconds=4\n'
