#!/usr/bin/env bash
set -Eeuo pipefail

# Disposable GitHub Security runner only. Exercise the actual fresh installer
# -> installed manager FD3 boundary without contacting the Cloudflare API.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
BINARY="${ASA_FRESH_CLOUDFLARE_TEST_BINARY:-/tmp/ai-server-agent}"
test -x "$BINARY"
sudo test ! -e /etc/ai-server-agent
sudo test ! -e /usr/local/bin/ai-server-agent
sudo test ! -e /usr/local/sbin/ai-server-agent-manage

missing_cf_token=/run/ai-server-agent-ci-no-cloudflare-token
sudo test ! -e "$missing_cf_token"
output="$(mktemp)"
owns_fixture=0

cleanup(){
  local rc="$?"
  trap - EXIT
  if [ "$owns_fixture" -eq 1 ]; then
    if ! sudo env AI_SERVER_AGENT_YES=1 bash "$ROOT/uninstall.sh" --purge >/dev/null 2>&1; then
      echo "fresh Cloudflare handoff: could not purge the disposable CI installation" >&2
      rc=1
    fi
  fi
  rm -f -- "$output"
  exit "$rc"
}
trap cleanup EXIT

# Fresh install creates/verifies a named bearer, removes legacy plaintext, and
# passes that bearer to the actual installed manager over one-use FD3.
# Intentionally stop BEFORE any provider access via an absent token file.
owns_fixture=1
result=0
sudo env \
  -u AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN \
  -u AI_SERVER_AGENT_MCP_AUTH_FD \
  AI_SERVER_AGENT_BINARY="$BINARY" \
  AI_SERVER_AGENT_NONINTERACTIVE=1 \
  AI_SERVER_AGENT_SETUP_MODE=cloudflare \
  AI_SERVER_AGENT_HOSTNAME=mcp-ci-handoff.invalid \
  AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE="$missing_cf_token" \
  bash "$ROOT/install.sh" >"$output" 2>&1 || result=$?

[ "$result" -ne 0 ] || {
  echo "fresh Cloudflare handoff unexpectedly completed without a provider token" >&2
  exit 1
}
grep -qF 'Named MCP bearer verified against the local Agent; Cloudflare resources not yet modified.' "$output" || {
  echo "fresh installer -> installed manager did not complete authenticated MCP credential preflight" >&2
  exit 1
}
grep -qF 'Cloudflare token file is not readable.' "$output" || {
  echo "fresh Cloudflare setup did not stop at the intended pre-provider token boundary" >&2
  exit 1
}
if grep -qF 'Cloudflare zone:' "$output"; then
  echo "Cloudflare provider discovery began despite absent API credentials" >&2
  exit 1
fi
if grep -Eq '(Authorization: Bearer |Bearer )[0-9a-f]{64}' "$output"; then
  echo "sensitive MCP credential material leaked into installer diagnostics" >&2
  exit 1
fi

sudo cmp -s "$ROOT/manage.sh" /usr/local/lib/ai-server-agent/manage.sh
sudo test -x /usr/local/sbin/ai-server-agent-manage
sudo test ! -e /etc/ai-server-agent/mcp.authorization
sudo test ! -e /etc/ai-server-agent/mcp.token
sudo test ! -e /etc/ai-server-agent/control/cloudflare-transaction.json
sudo test ! -e /etc/ai-server-agent/tls/origin.crt
sudo jq -e '
  .version==1 and
  ([.credentials[] | select(.principal.id=="direct-default" and .enabled==true and
      .verifier_algorithm=="sha256-v1" and (.verifier | test("^[0-9a-f]{64}$")))] | length)==1
' /etc/ai-server-agent/mcp-credentials.json >/dev/null
sudo jq -e '.listen_address=="127.0.0.1:3210" and (.tls_cert_file//"")==""' /etc/ai-server-agent/config.json >/dev/null
sudo systemctl is-active --quiet ai-server-agent.service
sudo systemctl is-active --quiet ai-server-agent-executor.service
curl -fsS --max-time 5 http://127.0.0.1:3210/healthz >/dev/null

echo "Fresh-installed Cloudflare installer -> manager FD3 boundary: PASS (no provider token, no provider mutation, local services healthy)"
