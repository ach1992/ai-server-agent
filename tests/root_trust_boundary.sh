#!/usr/bin/env bash
set -Eeuo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run root_trust_boundary.sh as root" >&2; exit 1; }
CONTROL_DIR=/etc/ai-server-agent/control
INSTALL_STATE="$CONTROL_DIR/install-state.json"
CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"
STATE_DIR=/var/lib/ai-server-agent
MANAGE=/usr/local/sbin/ai-server-agent-manage

assert_owner_mode(){
  local path="$1" owner="$2" group="$3" mode="$4" actual
  actual="$(stat -c '%U:%G:%a' "$path")"
  [ "$actual" = "$owner:$group:$mode" ] || { echo "$path ownership/mode = $actual, expected $owner:$group:$mode" >&2; exit 1; }
}
expect_denied(){
  local user="$1"; shift
  if runuser -u "$user" -- "$@" >/dev/null 2>&1; then
    echo "unexpected write capability for $user: $*" >&2
    exit 1
  fi
}

assert_owner_mode /etc/ai-server-agent root aiagent 750
assert_owner_mode "$CONTROL_DIR" root root 700
assert_owner_mode "$INSTALL_STATE" root root 600
assert_owner_mode "$STATE_DIR" root root 711
assert_owner_mode "$STATE_DIR/runtime" root root 711
assert_owner_mode "$STATE_DIR/jobs" root root 711
[ ! -L "$INSTALL_STATE" ]
jq -e 'type=="object" and (keys|sort)==["channel","ref","track_ref","version"]' "$INSTALL_STATE" >/dev/null

# The network-facing service has exactly one writable runtime output file,
# not a writable state-directory hierarchy.
readwrite_paths="$(systemctl show ai-server-agent.service -p ReadWritePaths --value)"
[ "$readwrite_paths" = "$STATE_DIR/AI_ENVIRONMENT.json" ] || {
  echo "unexpected aiagent ReadWritePaths: $readwrite_paths" >&2
  exit 1
}
assert_owner_mode "$STATE_DIR/AI_ENVIRONMENT.json" aiagent aiagent 640
expect_denied aiagent rm -f "$STATE_DIR/AI_ENVIRONMENT.json"
expect_denied aiagent mv "$STATE_DIR/AI_ENVIRONMENT.json" "$STATE_DIR/AI_ENVIRONMENT.replaced"
expect_denied aiagent ln -sfn /tmp/attacker "$STATE_DIR/AI_ENVIRONMENT.json"

state_hash="$(sha256sum "$INSTALL_STATE" | awk '{print $1}')"
for user in aiagent aiworker; do
  expect_denied "$user" rm -f "$INSTALL_STATE"
  expect_denied "$user" mv "$INSTALL_STATE" "$CONTROL_DIR/install-state.replaced"
  expect_denied "$user" ln -sfn /tmp/attacker "$INSTALL_STATE"
  expect_denied "$user" touch "$CONTROL_DIR/attacker"
  expect_denied "$user" sh -c "printf attacker > '$INSTALL_STATE'"
done
[ "$(sha256sum "$INSTALL_STATE" | awk '{print $1}')" = "$state_hash" ]

# Missing trusted identity must fail closed rather than drifting to source/main.
state_backup="$(mktemp)"
cp -- "$INSTALL_STATE" "$state_backup"
rm -f -- "$INSTALL_STATE"
if /usr/local/lib/ai-server-agent/update.sh --plan >/tmp/missing-install-state.out 2>&1; then
  echo "updater guessed a channel/ref without trusted install state" >&2
  exit 1
fi
grep -qF 'Trusted install state is missing' /tmp/missing-install-state.out
install -o root -g root -m 0600 "$state_backup" "$INSTALL_STATE"
rm -f "$state_backup"

# Neither unprivileged principal may replace root-consumed state containers.
for user in aiagent aiworker; do
  expect_denied "$user" mv "$STATE_DIR/jobs" "$STATE_DIR/jobs.replaced"
  expect_denied "$user" mv "$STATE_DIR/runtime" "$STATE_DIR/runtime.replaced"
  expect_denied "$user" touch "$STATE_DIR/attacker"
done

# Create the real Cloudflare transaction journal through production code.
AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1 bash -c '
  source /usr/local/sbin/ai-server-agent-manage
  CF_PENDING_KIND=dns-create
  CF_PENDING_ZONE=zone-test
  CF_PENDING_HOST=mcp.example.com
  CF_PENDING_VALUE=203.0.113.10
  CF_PENDING_MARKER=trust-boundary-marker
  save_cloudflare_transaction_state mcp.example.com zone-test "" "" "" "" "" "" "" "" 3210 "" "" "" ""
'
assert_owner_mode "$CF_TXN_STATE" root root 600
journal_hash="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"
for user in aiagent aiworker; do
  expect_denied "$user" rm -f "$CF_TXN_STATE"
  expect_denied "$user" mv "$CF_TXN_STATE" "$CONTROL_DIR/cloudflare-transaction.replaced"
  expect_denied "$user" ln -sfn /tmp/attacker "$CF_TXN_STATE"
  expect_denied "$user" sh -c "printf attacker > '$CF_TXN_STATE'"
done
[ "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$journal_hash" ]
rm -f "$CF_TXN_STATE"

# Old shell-style metadata and malformed custom state are never executed.
injection_marker=/tmp/ai-server-agent-state-injection-marker
malicious_state="$(mktemp)"
rm -f "$injection_marker"
cat > "$malicious_state" <<'MALICIOUS'
CHANNEL=stable
VERSION=$(touch /tmp/ai-server-agent-state-injection-marker)
REF=v9.9.9
TRACK_REF=v9.9.9
MALICIOUS
if AI_SERVER_AGENT_INSTALL_STATE="$malicious_state" /usr/local/lib/ai-server-agent/update.sh --plan >/tmp/invalid-install-state.out 2>&1; then
  echo "shell-style install state was accepted" >&2
  exit 1
fi
test ! -e "$injection_marker"
MALICIOUS_STATE="$malicious_state" MANAGE="$MANAGE" INJECTION_MARKER="$injection_marker" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$MANAGE"
  INSTALL_STATE="$MALICIOUS_STATE"
  installed_identity >/tmp/invalid-managed-identity.out
  test ! -e "$INJECTION_MARKER"
'
rm -f "$malicious_state" "$injection_marker"

echo "root trust-boundary tests passed"
