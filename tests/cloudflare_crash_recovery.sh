#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; CONTROL_DIR="$CONFIG_DIR/control"; TLS_DIR="$CONFIG_DIR/tls"; CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"; REMOTE="$TMP/remote.json"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$CONTROL_DIR" "$TLS_DIR"
child="$TMP/crash-child.sh"
cat > "$child" <<'CHILD'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CONTROL_DIR/cloudflare-transaction-backup"
AGENT_USER=root; CF_TOKEN=test
cf_new_ownership_marker(){ printf '0123456789abcdef0123456789abcdef\n'; }
cf_api(){
  local method="$1" path="$2" body="${3:-}"
  if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then printf '%s' '{"success":true,"result":[]}'; return 0; fi
  if [ "$method" = POST ] && [ "$path" = /zones/zone1/dns_records ]; then
    printf '%s' "$body" | jq -c '. + {id:"dns-crashed"}' > "$REMOTE"
    while :; do sleep 1; done
  fi
  return 2
}
cf_reconcile_dns zone1 mcp.example.com 203.0.113.10 ""
CHILD
chmod +x "$child"

set +e
ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" timeout --signal=KILL 1s bash "$child" >/tmp/cloudflare-crash-child.out 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || { echo 'expected SIGKILL crash injection' >&2; exit 1; }
echo 'crash injection returned nonzero as expected'
test -s "$REMOTE" || { cat /tmp/cloudflare-crash-child.out >&2 || true; echo 'mock Cloudflare create did not reach remote-commit point' >&2; exit 1; }
echo 'remote create committed before crash'
test -s "$CF_TXN_STATE" || { echo 'durable transaction journal missing after crash' >&2; exit 1; }
echo 'durable transaction journal survived crash'
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create || { echo 'pending kind missing after crash' >&2; cat "$CF_TXN_STATE" >&2; exit 1; }
marker="$(jq -r '.pending.marker' "$CF_TXN_STATE")"
test -n "$marker" || { echo 'pending ownership marker missing after crash' >&2; exit 1; }
test "$(jq -r '.comment' "$REMOTE")" = "Managed by AI Server Agent txn:$marker" || { echo 'remote marker does not match durable journal marker' >&2; exit 1; }
echo 'remote ownership marker matches durable journal'

# New process: reload only the durable journal and remote state, then recover.
ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$ROOT/manage.sh"
  STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CONTROL_DIR/cloudflare-transaction-backup"
  AGENT_USER=root; CF_TOKEN=test
  cf_api(){
    method="$1"; path="$2"
    if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then
      if [ -s "$REMOTE" ]; then jq -c "{success:true,result:[.] }" "$REMOTE"; else printf "%s" "{\"success\":true,\"result\":[]}"; fi
      return 0
    fi
    return 2
  }
  cf_delete_owned(){ case "$1" in /zones/zone1/dns_records/dns-crashed) rm -f "$REMOTE"; return 0 ;; *) return 1 ;; esac; }
  recover_cloudflare_transaction
  test ! -e "$CF_TXN_STATE"
  test ! -e "$REMOTE"
' || { echo 'fresh-process crash recovery failed' >&2; test -e "$CF_TXN_STATE" && cat "$CF_TXN_STATE" >&2 || true; exit 1; }
echo 'fresh process recovered durable transaction journal'

echo 'cloudflare crash-recovery test passed'
