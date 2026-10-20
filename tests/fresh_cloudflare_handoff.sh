#!/usr/bin/env bash
set -Eeuo pipefail

# Disposable GitHub Security runner only. Exercise the actual fresh installer
# -> installed manager FD3 boundary without contacting the Cloudflare API.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
BINARY="${ASA_FRESH_CLOUDFLARE_TEST_BINARY:-/run/asa-trusted-source-bin/ai-server-agent}"
test -x "$BINARY"
sudo test ! -e /etc/ai-server-agent
sudo test ! -e /usr/local/bin/ai-server-agent
sudo test ! -e /usr/local/sbin/ai-server-agent-manage

missing_cf_token=/run/ai-server-agent-ci-no-cloudflare-token
sudo test ! -e "$missing_cf_token"
output="$(mktemp)"
owns_fixture=0
stage='pre-install prerequisites'
# Emit only a non-secret stage name and script line on failure. Never dump
# installer output, which can contain one-use bearer material on a regression.
trap 'printf "fresh Cloudflare handoff: unhandled failure at line %s (stage: %s; exit %s)\n" "$LINENO" "$stage" "$?" >&2' ERR

cleanup(){
  local rc="$?"
  trap - EXIT
  if [ "$rc" -ne 0 ]; then
    printf 'fresh Cloudflare handoff: failed during %s (exit %s; installer output withheld for credential safety)\n' "$stage" "$rc" >&2
  fi
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
stage='fresh noninteractive Cloudflare installation and intentional absent-token refusal'
owns_fixture=1
result=0
sudo env \
  -u AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN \
  -u AI_SERVER_AGENT_MCP_AUTH_FD \
  AI_SERVER_AGENT_BINARY="$BINARY" \
  AI_SERVER_AGENT_NONINTERACTIVE=1 \
  AI_SERVER_AGENT_ENVIRONMENT_PROFILE=validation \
  AI_SERVER_AGENT_SERVER_LABEL='QA Agent Host' \
  AI_SERVER_AGENT_SETUP_MODE=cloudflare \
  AI_SERVER_AGENT_HOSTNAME=mcp-ci-handoff.invalid \
  AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE="$missing_cf_token" \
  bash "$ROOT/install.sh" >"$output" 2>&1 || result=$?

stage='verify named bearer preflight and no provider access'
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

stage='verify installed manager binary and fresh protected state'
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
sudo jq -e '.listen_address=="127.0.0.1:3210" and (.tls_cert_file//"")=="" and .environment_profile=="validation" and .server_label=="QA Agent Host"' /etc/ai-server-agent/config.json >/dev/null
sudo jq -e '.environment_profile=="validation" and .server_label=="QA Agent Host"' /var/lib/ai-server-agent/AI_ENVIRONMENT.json >/dev/null
sudo systemctl is-active --quiet ai-server-agent.service
sudo systemctl is-active --quiet ai-server-agent-executor.service
curl -fsS --max-time 5 http://127.0.0.1:3210/healthz >/dev/null

# New-baseline live management: only the Agent restarts for a purpose change.
# The authorization store, immutable instance identity and Executor service
# are independent of profile choice.
stage='profile setting and executor identity preservation'
executor_pid="$(sudo systemctl show -p MainPID --value ai-server-agent-executor.service)"
instance_id="$(sudo jq -r .instance_id /etc/ai-server-agent/config.json)"
credential_digest="$(sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | cut -d' ' -f1)"
sudo ai-server-agent-manage profile-set development >/dev/null
sudo jq -e '.environment_profile=="development" and .server_label=="QA Agent Host"' /var/lib/ai-server-agent/AI_ENVIRONMENT.json >/dev/null
sudo ai-server-agent-manage profile-set validation >/dev/null
sudo ai-server-agent-manage profile-label 'QA Agent Host 2' >/dev/null
sudo jq -e '.environment_profile=="validation" and .server_label=="QA Agent Host 2"' /var/lib/ai-server-agent/AI_ENVIRONMENT.json >/dev/null
sudo ai-server-agent-manage profile-label 'QA Agent Host' >/dev/null
test "$(sudo systemctl show -p MainPID --value ai-server-agent-executor.service)" = "$executor_pid"
test "$(sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | cut -d' ' -f1)" = "$credential_digest"

# Install the SAME new-baseline candidate again in the disposable fixture.
# The purpose and label survive the installer regeneration, without relying
# on compatibility machinery for earlier Agent generations.
stage='second noninteractive new-baseline installer'
reinstall_rc=0
sudo env AI_SERVER_AGENT_BINARY="$BINARY" AI_SERVER_AGENT_NONINTERACTIVE=1 \
  AI_SERVER_AGENT_SETUP_MODE=local bash "$ROOT/install.sh" >"$output" 2>&1 || reinstall_rc=$?
if [ "$reinstall_rc" -ne 0 ]; then
  # A faulty installer may include credentials in its private output. Print
  # only a bounded, redacted error category; never dump the raw file.
  printf 'fresh Cloudflare handoff: local reinstall failed (exit %s)\n' "$reinstall_rc" >&2
  while IFS= read -r diagnostic; do
    case "$diagnostic" in
      *'[ai-server-agent] ERROR:'*|*'[ai-server-agent] WARNING:'*|sudo:\ *|systemctl:\ *)
        case "${diagnostic,,}" in
          *bearer*|*authorization*|*token*|*secret*|*password*|*private*key*)
            printf 'fresh Cloudflare handoff: sensitive error details withheld\n' >&2 ;;
          *)
            printf '%s\n' "$diagnostic" | LC_ALL=C tr -d '\000-\010\013-\037\177' | sed -E 's/[[:xdigit:]]{64}/[redacted-digest]/g' >&2 ;;
        esac ;;
    esac
  done < <(tail -n 35 "$output")
  exit "$reinstall_rc"
fi
stage='post-second-install manifest/credential/service verification'
sudo jq -e '.environment_profile=="validation" and .server_label=="QA Agent Host"' /etc/ai-server-agent/config.json >/dev/null
sudo jq -e '.environment_profile=="validation" and .server_label=="QA Agent Host"' /var/lib/ai-server-agent/AI_ENVIRONMENT.json >/dev/null
test "$(sudo jq -r .instance_id /etc/ai-server-agent/config.json)" = "$instance_id"
test "$(sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | cut -d' ' -f1)" = "$credential_digest"
sudo systemctl is-active --quiet ai-server-agent.service
sudo systemctl is-active --quiet ai-server-agent-executor.service

echo "Fresh-installed Cloudflare handoff + purpose profile management + new-baseline reinstall: PASS (no provider token, no provider mutation)"
