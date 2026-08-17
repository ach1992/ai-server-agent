#!/usr/bin/env bash
set -Eeuo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run root_trust_boundary.sh as root" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTROL_DIR=/etc/ai-server-agent/control
INSTALL_STATE="$CONTROL_DIR/install-state.json"
CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"
CF_TXN_BACKUP_DIR="$CONTROL_DIR/cloudflare-transaction-backup"
STATE_DIR=/var/lib/ai-server-agent
MANAGE_CMD=/usr/local/sbin/ai-server-agent-manage
MANAGE_IMPL=/usr/local/lib/ai-server-agent/manage.sh
LIFECYCLE_LOCK_DIR=/run/lock/ai-server-agent
LIFECYCLE_LOCK="$LIFECYCLE_LOCK_DIR/management.lock"

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
assert_owner_mode "$LIFECYCLE_LOCK_DIR" root root 700
assert_owner_mode "$LIFECYCLE_LOCK" root root 600
[ ! -L "$INSTALL_STATE" ]
[ ! -L "$LIFECYCLE_LOCK" ]
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

# One purge-safe root lifecycle lock serializes every privileged mutating entry
# point. Hold the actual lock and prove production commands fail before mutation.
lifecycle_tmp="$(mktemp -d)"
holder_pid=""
cleanup_lifecycle_test(){
  if [ -n "$holder_pid" ]; then kill "$holder_pid" 2>/dev/null || true; wait "$holder_pid" 2>/dev/null || true; fi
  rm -rf "$lifecycle_tmp"
}
trap cleanup_lifecycle_test EXIT
(
  exec 9>>"$LIFECYCLE_LOCK"
  flock -n 9
  : > "$lifecycle_tmp/ready"
  sleep 60 9>&-
) &
holder_pid=$!
for _ in $(seq 1 50); do [ -e "$lifecycle_tmp/ready" ] && break; sleep 0.1; done
[ -e "$lifecycle_tmp/ready" ] || { echo "could not hold lifecycle lock for overlap test" >&2; exit 1; }

config_hash="$(sha256sum /etc/ai-server-agent/config.json | awk '{print $1}')"
managed_hash="$(sha256sum /etc/ai-server-agent/managed.json | awk '{print $1}')"
installed_ref="$(jq -r '.ref' "$INSTALL_STATE")"

if AI_SERVER_AGENT_PORT=3210 "$MANAGE_CMD" configure-local >"$lifecycle_tmp/manage.out" 2>&1; then
  echo "configure-local bypassed the global lifecycle lock" >&2; exit 1
fi
grep -qF 'Another AI Server Agent management operation is already active' "$lifecycle_tmp/manage.out"

if AI_SERVER_AGENT_REF="$installed_ref" /usr/local/lib/ai-server-agent/update.sh >"$lifecycle_tmp/update.out" 2>&1; then
  echo "update bypassed the global lifecycle lock" >&2; exit 1
fi
grep -qF 'Another AI Server Agent management operation is already active' "$lifecycle_tmp/update.out"

if AI_SERVER_AGENT_BINARY=/usr/local/bin/ai-server-agent AI_SERVER_AGENT_REF="$installed_ref" AI_SERVER_AGENT_NONINTERACTIVE=1 AI_SERVER_AGENT_SETUP_MODE=keep bash "$ROOT/install.sh" >"$lifecycle_tmp/install.out" 2>&1; then
  echo "installer bypassed the global lifecycle lock" >&2; exit 1
fi
grep -qF 'Another AI Server Agent management operation is already active' "$lifecycle_tmp/install.out"

if AI_SERVER_AGENT_YES=1 /usr/local/lib/ai-server-agent/uninstall.sh --purge >"$lifecycle_tmp/purge.out" 2>&1; then
  echo "purge bypassed the global lifecycle lock" >&2; exit 1
fi
grep -qF 'Another AI Server Agent management operation is already active' "$lifecycle_tmp/purge.out"

[ "$(sha256sum /etc/ai-server-agent/config.json | awk '{print $1}')" = "$config_hash" ]
[ "$(sha256sum /etc/ai-server-agent/managed.json | awk '{print $1}')" = "$managed_hash" ]
[ "$(sha256sum "$INSTALL_STATE" | awk '{print $1}')" = "$state_hash" ]
systemctl is-active --quiet ai-server-agent-executor.service
systemctl is-active --quiet ai-server-agent.service
kill "$holder_pid"; wait "$holder_pid" 2>/dev/null || true; holder_pid=""
rm -rf "$lifecycle_tmp"
trap - EXIT

# Create a valid production-shaped Cloudflare transaction journal through the
# installed management implementation. The schema-v3 journal is meaningful only
# after the durable local rollback snapshot exists.
AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1 bash -c '
  set -Eeuo pipefail
  source /usr/local/lib/ai-server-agent/manage.sh
  host=mcp.example.com
  zone_id=zone-test
  dns_id=""; dns_action=""; dns_fingerprint=""
  origin_ruleset_id=""; origin_rule_id=""; origin_action=""; origin_fingerprint=""
  ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""; ssl_fingerprint=""; cert_id=""
  prepare_cloudflare_local_backup
  CF_TXN_PHASE=prepared
  marker=0123456789abcdef0123456789abcdef
  body="$(jq -n --arg name "$host" --arg ip 203.0.113.10 --arg comment "Managed by AI Server Agent txn:$marker" "{type:\"A\",name:\$name,content:\$ip,ttl:1,proxied:true,comment:\$comment}")"
  fingerprint="$(cf_dns_intent_fingerprint <<<"$body")"
  cf_set_pending_write dns-create "$zone_id" "$host" 203.0.113.10 "" "$marker" "$fingerprint"
  validate_cloudflare_transaction_state "$CF_TXN_STATE"
'
assert_owner_mode "$CF_TXN_STATE" root root 600
assert_owner_mode "$CF_TXN_BACKUP_DIR" root root 700
journal_hash="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"
for user in aiagent aiworker; do
  expect_denied "$user" rm -f "$CF_TXN_STATE"
  expect_denied "$user" mv "$CF_TXN_STATE" "$CONTROL_DIR/cloudflare-transaction.replaced"
  expect_denied "$user" ln -sfn /tmp/attacker "$CF_TXN_STATE"
  expect_denied "$user" sh -c "printf attacker > '$CF_TXN_STATE'"
done
[ "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$journal_hash" ]
AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1 bash -c '
  set -Eeuo pipefail
  source /usr/local/lib/ai-server-agent/manage.sh
  finalize_cloudflare_transaction_state
'
test ! -e "$CF_TXN_STATE"
test ! -e "$CF_TXN_BACKUP_DIR"

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
MALICIOUS_STATE="$malicious_state" MANAGE_IMPL="$MANAGE_IMPL" INJECTION_MARKER="$injection_marker" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$MANAGE_IMPL"
  INSTALL_STATE="$MALICIOUS_STATE"
  installed_identity >/tmp/invalid-managed-identity.out
  test ! -e "$INJECTION_MARKER"
'
rm -f "$malicious_state" "$injection_marker"

echo "root trust-boundary and lifecycle serialization tests passed"
