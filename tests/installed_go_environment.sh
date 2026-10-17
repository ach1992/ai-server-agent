#!/usr/bin/env bash
# Genuine installed-Agent / root-executor / worker Go environment acceptance.
# Run ONLY on a disposable CI/test installation with an already authenticated
# direct MCP bearer supplied on stdin. Never invoke on occupied Agent hosts.
set -Eeuo pipefail
# Trace nested CI acceptance failures without printing bearer or responses.
phase=preflight
trap 'rc=$?; printf "INSTALLED_GO_PROBE_FAILED phase=%s line=%s rc=%s\n" "$phase" "$LINENO" "$rc" >&2' ERR

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

phase=trusted-go-check
# The disposable CI runner's /usr/bin/go may be a distro-managed
# symlink. Production rejects symlinked executables, and a lower-priority
# alias must not prevent provisioning a trusted higher-priority entry point.
# Preserve an occupied /usr/local/bin/go as-is: never replace an unknown
# binary or symlink. The CI setup-go toolchain is the only source when
# provisioning this otherwise vacant system path. Copying Go's binary can
# break its relative GOROOT; use a root-owned wrapper that executes the
# versioned setup-go toolchain from its original location.
if [ ! -e /usr/local/bin/go ] && [ ! -L /usr/local/bin/go ]; then
  ci_go="$(command -v go)"
  [ -x "$ci_go" ] && [ -f "$ci_go" ] || { echo 'CI Go executable missing' >&2; exit 1; }
  [ -n "${RUNNER_TOOL_CACHE:-}" ] &&
    [[ "$ci_go" == "$RUNNER_TOOL_CACHE"/go/*/bin/go ]] &&
    [[ "$ci_go" =~ ^[A-Za-z0-9_./-]+$ ]] || {
      echo 'CI Go must be an absolute setup-go toolcache executable' >&2
      exit 1
    }
  "$ci_go" version | grep -Eq '^go version go1[.]26[.]'
  staged_wrapper="$(mktemp)"
  printf '#!/bin/sh\nexec "%s" "$@"\n' "$ci_go" > "$staged_wrapper"
  sudo install -m 0755 -o root -g root "$staged_wrapper" /usr/local/bin/go
  cmp -s -- "$staged_wrapper" /usr/local/bin/go || {
    echo 'CI Go wrapper differs from the selected toolchain launcher' >&2
    exit 1
  }
  rm -f -- "$staged_wrapper"
fi
# The higher-priority production path is authoritative. Reject it if
# untrusted rather than silently falling back to /usr/bin/go.
candidate=/usr/local/bin/go
if [ ! -e "$candidate" ] && [ ! -L "$candidate" ]; then
  candidate=/usr/bin/go
fi
if [ -L "$candidate" ] || [ ! -f "$candidate" ]; then
  echo "installed Go probe refuses non-regular or symlinked system executable: $candidate" >&2
  exit 1
fi
if [ "$(stat -c '%u:%a' "$candidate")" != '0:755' ]; then
  echo "installed Go probe refuses unsafe owner/mode: $candidate" >&2
  exit 1
fi
"$candidate" version | grep -Eq '^go version go'


phase=fixture
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
# Historical installations also used a nested /var/tmp/e temp root. Cover
# both default and one-level nested temp roots without scanning worktrees.
find_probes(){
  # Enumerate as root: a private nested temp parent might not be traversable
  # by the runner user, and a partial listing must never be treated as PASS.
  sudo find /tmp /var/tmp -mindepth 1 -maxdepth 2 -type d \
    -name 'ai-server-agent-env-probe-*' -print | LC_ALL=C sort
}
phase=probe-baseline
find_probes > "$scratch/before"
# Exercise the same initialize handshake as a real MCP client; never use a
# fabricated executor-only shortcut to bypass bearer and transport checks.
phase=initialize
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
phase=repository-environment
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
phase=delayed-probe-check
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
phase=final-health
printf 'INSTALLED_GO_ENVIRONMENT_MCP_NO_PROBE_LEAK_PASS calls=6 delayed_seconds=4\n'
