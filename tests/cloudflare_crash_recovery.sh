#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; CONTROL_DIR="$CONFIG_DIR/control"; TLS_DIR="$CONFIG_DIR/tls"; CONFIG_FILE="$CONFIG_DIR/config.json"; MANAGED_STATE="$CONFIG_DIR/managed.json"; CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"; CF_TXN_BACKUP_DIR="$CONTROL_DIR/cloudflare-transaction-backup"; MANAGEMENT_LOCK="$CONTROL_DIR/management.lock"; REMOTE="$TMP/remote.json"; REMOTE_CERT="$TMP/remote-cert.json"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$CONTROL_DIR" "$TLS_DIR"
printf '%s\n' '{"listen_address":"127.0.0.1:3210","tls_cert_file":"","tls_key_file":""}' > "$CONFIG_FILE"
printf '%s\n' '{"active_provider":"local","port":3210}' > "$MANAGED_STATE"
child="$TMP/crash-child.sh"
cat > "$child" <<'CHILD'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""
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
# Production Cloudflare configuration creates the durable local rollback
# snapshot before any remote create intent is journaled. Exercise that real
# precondition so a crash-produced pending journal is valid production state.
prepare_cloudflare_local_backup
cf_reconcile_dns zone1 mcp.example.com 203.0.113.10 ""
CHILD
chmod +x "$child"

set +e
ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_MANAGEMENT_LOCK="$MANAGEMENT_LOCK" REMOTE="$REMOTE" timeout --signal=KILL 1s bash "$child" >/tmp/cloudflare-crash-child.out 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || { echo 'expected SIGKILL crash injection' >&2; exit 1; }
echo 'crash injection returned nonzero as expected'
test -s "$REMOTE" || { cat /tmp/cloudflare-crash-child.out >&2 || true; echo 'mock Cloudflare create did not reach remote-commit point' >&2; exit 1; }
echo 'remote create committed before crash'
test -s "$CF_TXN_STATE" || { echo 'durable transaction journal missing after crash' >&2; exit 1; }
test -d "$CF_TXN_BACKUP_DIR" || { echo 'durable local rollback snapshot missing after crash' >&2; exit 1; }
echo 'durable transaction journal and local rollback snapshot survived crash'
test "$(jq -r '.backup_ready' "$CF_TXN_STATE")" = true || { echo 'crash journal did not record durable backup readiness' >&2; cat "$CF_TXN_STATE" >&2; exit 1; }
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create || { echo 'pending kind missing after crash' >&2; cat "$CF_TXN_STATE" >&2; exit 1; }
marker="$(jq -r '.pending.marker' "$CF_TXN_STATE")"
test -n "$marker" || { echo 'pending ownership marker missing after crash' >&2; exit 1; }
test "$(jq -r '.comment' "$REMOTE")" = "Managed by AI Server Agent txn:$marker" || { echo 'remote marker does not match durable journal marker' >&2; exit 1; }
remote_fp="$(jq -cS '{type:(.type // ""),name:(.name // ""),content:(.content // ""),ttl:(.ttl // 0),proxied:(.proxied // false),comment:(.comment // "")}' "$REMOTE" | sha256sum | awk '{print $1}')"
test "$(jq -r '.pending.fingerprint' "$CF_TXN_STATE")" = "$remote_fp" || { echo 'remote representation does not match durable pending fingerprint' >&2; exit 1; }
echo 'remote ownership marker and representation match durable journal'

# New process: reload only the durable journal/backup and remote state, then
# recover through the production discovery + exact-representation delete path.
ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_MANAGEMENT_LOCK="$MANAGEMENT_LOCK" REMOTE="$REMOTE" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$ROOT/manage.sh"
  STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""
  AGENT_USER=root; CF_TOKEN=test
  cf_api(){
    local method="$1" path="$2"
    if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then
      if [ -s "$REMOTE" ]; then jq -c "{success:true,result:[.] }" "$REMOTE"; else printf "%s" "{\"success\":true,\"result\":[]}"; fi
      return 0
    fi
    return 2
  }
  cf_get_optional(){
    case "$1" in
      /zones/zone1/dns_records/dns-crashed)
        if [ -s "$REMOTE" ]; then jq -c "{success:true,result:.}" "$REMOTE"; return 0; fi
        return 3
        ;;
      *) return 2 ;;
    esac
  }
  cf_delete_owned(){ case "$1" in /zones/zone1/dns_records/dns-crashed) rm -f "$REMOTE"; return 0 ;; *) return 1 ;; esac; }
  recover_cloudflare_transaction
  test ! -e "$CF_TXN_STATE"
  test ! -e "$CF_TXN_BACKUP_DIR"
  test ! -e "$REMOTE"
' || { echo 'fresh-process crash recovery failed' >&2; test -e "$CF_TXN_STATE" && cat "$CF_TXN_STATE" >&2 || true; exit 1; }
echo 'fresh process recovered durable DNS transaction journal'

# Origin CA response-lost recovery must survive the same SIGKILL boundary. The
# generated CSR is the unpredictable discovery value, while the full create
# representation fingerprint is the destructive recovery proof.
cert_child="$TMP/cert-crash-child.sh"
cat > "$cert_child" <<'CERT_CHILD'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""
AGENT_USER=root; CF_TOKEN=test
stage="$(mktemp -d)"; trap 'rm -rf "$stage"' EXIT
cf_api(){
  local method="$1" path="$2" body="${3:-}"
  if [ "$method" = POST ] && [ "$path" = /certificates ]; then
    printf '%s' "$body" | jq -c '. + {id:"cert-crashed"}' > "$REMOTE_CERT"
    while :; do sleep 1; done
  fi
  return 2
}
prepare_cloudflare_local_backup
cf_issue_origin_cert zone1 mcp.example.com "$stage"
CERT_CHILD
chmod +x "$cert_child"

set +e
ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_MANAGEMENT_LOCK="$MANAGEMENT_LOCK" REMOTE_CERT="$REMOTE_CERT" timeout --signal=KILL 2s bash "$cert_child" >/tmp/cloudflare-cert-crash-child.out 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || { echo 'expected Origin CA SIGKILL crash injection' >&2; exit 1; }
test -s "$REMOTE_CERT" || { cat /tmp/cloudflare-cert-crash-child.out >&2 || true; echo 'mock Origin CA create did not reach remote-commit point' >&2; exit 1; }
test -s "$CF_TXN_STATE" || { echo 'Origin CA durable transaction journal missing after crash' >&2; exit 1; }
test -d "$CF_TXN_BACKUP_DIR" || { echo 'Origin CA durable local rollback snapshot missing after crash' >&2; exit 1; }
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = origin-cert-create || { echo 'Origin CA pending kind missing after crash' >&2; cat "$CF_TXN_STATE" >&2; exit 1; }
test "$(jq -r '.pending.value' "$CF_TXN_STATE")" = "$(jq -r '.csr' "$REMOTE_CERT")" || { echo 'Origin CA durable CSR does not match remote create intent' >&2; exit 1; }
cert_remote_fp="$(jq -cS '{csr:(.csr // ""),hostnames:((.hostnames // []) | sort),request_type:(.request_type // ""),requested_validity:(.requested_validity // 0)}' "$REMOTE_CERT" | sha256sum | awk '{print $1}')"
test "$(jq -r '.pending.fingerprint' "$CF_TXN_STATE")" = "$cert_remote_fp" || { echo 'Origin CA remote representation does not match durable pending fingerprint' >&2; exit 1; }
validate_owner="$(stat -c '%u:%g:%a' "$CF_TXN_STATE")"
test "$validate_owner" = '0:0:600' || { echo 'Origin CA crash journal ownership/mode changed' >&2; exit 1; }
echo 'Origin CA remote representation matches durable crash journal'

ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_MANAGEMENT_LOCK="$MANAGEMENT_LOCK" REMOTE_CERT="$REMOTE_CERT" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$ROOT/manage.sh"
  STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""
  AGENT_USER=root; CF_TOKEN=test
  cf_api(){
    local method="$1" path="$2"
    if [ "$method" = GET ] && [[ "$path" == /certificates\?zone_id=zone1* ]]; then
      if [ -s "$REMOTE_CERT" ]; then jq -c "{success:true,result:[.],result_info:{total_pages:1}}" "$REMOTE_CERT"; else printf "%s" "{\"success\":true,\"result\":[],\"result_info\":{\"total_pages\":1}}"; fi
      return 0
    fi
    return 2
  }
  cf_get_optional(){
    case "$1" in
      /certificates/cert-crashed)
        if [ -s "$REMOTE_CERT" ]; then jq -c "{success:true,result:.}" "$REMOTE_CERT"; return 0; fi
        return 3
        ;;
      *) return 2 ;;
    esac
  }
  cf_delete_owned(){ case "$1" in /certificates/cert-crashed) rm -f "$REMOTE_CERT"; return 0 ;; *) return 1 ;; esac; }
  recover_cloudflare_transaction
  test ! -e "$CF_TXN_STATE"
  test ! -e "$CF_TXN_BACKUP_DIR"
  test ! -e "$REMOTE_CERT"
' || { echo 'fresh-process Origin CA crash recovery failed' >&2; test -e "$CF_TXN_STATE" && cat "$CF_TXN_STATE" >&2 || true; exit 1; }
echo 'fresh process recovered durable Origin CA transaction journal'

echo 'cloudflare crash-recovery test passed'
