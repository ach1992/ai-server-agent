from pathlib import Path

manage = Path('manage.sh')
text = manage.read_text()
old = '''    (if (.phase=="applying" or .phase=="committing") then .backup_ready==true else true end) and
    (if (.phase=="committing" or .phase=="committed") then ((.commit.hostname|length)>0 and (.commit.port|test("^[0-9]+$")) and (.commit.zone_id|length)>0 and (.commit.zone_name|length)>0 and (.commit.dns_id|length)>0 and (.commit.origin_ruleset_id|length)>0 and (.commit.origin_rule_id|length)>0 and (.commit.ssl_ruleset_id|length)>0 and (.commit.ssl_rule_id|length)>0 and (.commit.certificate_id|length)>0) else true end)
'''
new = '''    (if (.phase=="applying" or .phase=="committing" or .phase=="committed") then .backup_ready==true else true end) and
    (if (.phase=="committing" or .phase=="committed" or .phase=="rolled_back") then .pending.kind=="" else true end) and
    (if .phase=="rolled_back" then (.dns.action=="" and .origin.action=="" and .ssl.action=="" and .certificate_id=="") else true end) and
    (if (.phase=="committing" or .phase=="committed") then ((.commit.hostname|length)>0 and (.commit.port|test("^[0-9]+$")) and (.commit.zone_id|length)>0 and (.commit.zone_name|length)>0 and (.commit.dns_id|length)>0 and (.commit.origin_ruleset_id|length)>0 and (.commit.origin_rule_id|length)>0 and (.commit.ssl_ruleset_id|length)>0 and (.commit.ssl_rule_id|length)>0 and (.commit.certificate_id|length)>0) else true end)
'''
if text.count(old) != 1:
    raise SystemExit('validator anchor mismatch')
manage.write_text(text.replace(old, new))

phase_test = r'''#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run cloudflare_phase_recovery.sh as root" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; CONTROL_DIR="$CONFIG_DIR/control"; TLS_DIR="$CONFIG_DIR/tls"; CONFIG_FILE="$CONFIG_DIR/config.json"; MANAGED_STATE="$CONFIG_DIR/managed.json"; CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"; CF_TXN_BACKUP_DIR="$CONTROL_DIR/cloudflare-transaction-backup"; DELETE_LOG="$TMP/deletes.log"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$CONTROL_DIR" "$TLS_DIR"
old_config='{"listen_address":"127.0.0.1:3210","tls_cert_file":"","tls_key_file":""}'
old_managed='{"active_provider":"local","hostname":"old.example.com","port":3210,"cloudflare":{}}'
new_config='{"listen_address":"0.0.0.0:3210","tls_cert_file":"new","tls_key_file":"new"}'
new_managed='{"active_provider":"cloudflare","hostname":"mcp.example.com","port":3210,"cloudflare":{"zone_id":"zone1","zone_name":"example.com","dns_record_id":"dns-new","dns_record_owned":true,"origin_ruleset_id":"origin-set","origin_rule_id":"origin-rule","ssl_config_ruleset_id":"ssl-set","ssl_config_rule_id":"ssl-rule","origin_certificate_id":"cert-new"}}'

reset_old_local(){
  rm -rf "$CF_TXN_BACKUP_DIR"; rm -f "$CF_TXN_STATE"; : > "$DELETE_LOG"
  printf '%s\n' "$old_config" > "$CONFIG_FILE"
  printf '%s\n' "$old_managed" > "$MANAGED_STATE"
  printf 'old-key\n' > "$TLS_DIR/origin.key"; printf 'old-crt\n' > "$TLS_DIR/origin.crt"; rm -f "$TLS_DIR/origin.csr"
  chown root:root "$CONFIG_FILE" "$MANAGED_STATE" "$TLS_DIR/origin.key" "$TLS_DIR/origin.crt"
  chmod 0640 "$CONFIG_FILE" "$MANAGED_STATE" "$TLS_DIR/origin.key"; chmod 0644 "$TLS_DIR/origin.crt"
}

kill_after_boundary(){
  local scenario="$1" rc
  set +e
  env \
    ROOT="$ROOT" TEST_SCENARIO="$scenario" \
    TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" \
    TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" \
    TEST_NEW_CONFIG="$new_config" TEST_NEW_MANAGED="$new_managed" \
    bash -c '
      set -Eeuo pipefail
      export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
      source "$ROOT/manage.sh"
      AGENT_USER=root
      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"
      host=mcp.example.com; zone_id=zone1; old_port=3210
      dns_id=dns-new; dns_action=created; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""
      origin_ruleset_id=origin-set; origin_rule_id=origin-rule; origin_action=created
      ssl_ruleset_id=ssl-set; ssl_rule_id=ssl-rule; ssl_action=created; cert_id=cert-new
      prepare_cloudflare_local_backup
      CF_TXN_PHASE=applying
      save_current_cloudflare_transaction_state
      printf "%s\n" "$TEST_NEW_CONFIG" > "$CONFIG_FILE"
      printf "new-key\n" > "$TLS_DIR/origin.key"; printf "new-crt\n" > "$TLS_DIR/origin.crt"
      case "$TEST_SCENARIO" in
        applying) ;;
        committing|committed)
          cf_set_commit_intent mcp.example.com 3210 zone1 example.com dns-new true origin-set origin-rule ssl-set ssl-rule cert-new
          printf "%s\n" "$TEST_NEW_MANAGED" > "$MANAGED_STATE"
          if [ "$TEST_SCENARIO" = committed ]; then
            CF_TXN_PHASE=committed
            cf_checkpoint_transaction
          fi
          ;;
        *) exit 2 ;;
      esac
      kill -KILL $$
    ' >/dev/null 2>&1
  rc=$?
  set -e
  [ "$rc" -eq 137 ] || { echo "expected SIGKILL (137) at $scenario boundary, got $rc" >&2; exit 1; }
  test -s "$CF_TXN_STATE" || { echo "transaction journal missing after $scenario SIGKILL" >&2; exit 1; }
}

recover_fresh(){
  env \
    ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" \
    TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_DELETE_LOG="$DELETE_LOG" \
    bash -c '
      set -Eeuo pipefail
      export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
      source "$ROOT/manage.sh"
      AGENT_USER=root
      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"
      systemctl(){ return 0; }
      cf_delete_owned(){ printf "%s\n" "$1" >> "$TEST_DELETE_LOG"; return 0; }
      cf_recover_pending_write(){ cf_clear_pending_write; return 0; }
      recover_cloudflare_transaction
    '
}

# Real SIGKILL immediately after local TLS/config mutation. A fresh process must
# restore the durable local snapshot and roll back only recorded remote state.
reset_old_local
kill_after_boundary applying
recover_fresh
cmp -s <(printf '%s\n' "$old_config") "$CONFIG_FILE"
cmp -s <(printf '%s\n' "$old_managed") "$MANAGED_STATE"
grep -qF '/zones/zone1/dns_records/dns-new' "$DELETE_LOG"
grep -qF '/zones/zone1/rulesets/origin-set/rules/origin-rule' "$DELETE_LOG"
grep -qF '/zones/zone1/rulesets/ssl-set/rules/ssl-rule' "$DELETE_LOG"
grep -qF '/certificates/cert-new' "$DELETE_LOG"
test ! -e "$CF_TXN_STATE"; test ! -e "$CF_TXN_BACKUP_DIR"

# Real SIGKILL immediately after managed-state replacement while durable phase
# is committing. A fresh process must detect the exact committed identity and
# finalize without deleting active Cloudflare resources or restoring old local state.
reset_old_local
kill_after_boundary committing
: > "$DELETE_LOG"
recover_fresh
test ! -s "$DELETE_LOG"
cmp -s <(printf '%s\n' "$new_config") "$CONFIG_FILE"
cmp -s <(printf '%s\n' "$new_managed") "$MANAGED_STATE"
test ! -e "$CF_TXN_STATE"; test ! -e "$CF_TXN_BACKUP_DIR"

# Real SIGKILL after durable phase=committed but before journal deletion. A
# fresh process must finalize only; no rollback is allowed.
reset_old_local
kill_after_boundary committed
: > "$DELETE_LOG"
recover_fresh
test ! -s "$DELETE_LOG"
cmp -s <(printf '%s\n' "$new_config") "$CONFIG_FILE"
cmp -s <(printf '%s\n' "$new_managed") "$MANAGED_STATE"
test ! -e "$CF_TXN_STATE"; test ! -e "$CF_TXN_BACKUP_DIR"

# Truncated journal fails closed and remains byte-for-byte untouched.
reset_old_local
printf '{"version":1,"phase":"applying"' > "$CF_TXN_STATE"
chown root:root "$CF_TXN_STATE"; chmod 0600 "$CF_TXN_STATE"
before="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"; : > "$DELETE_LOG"
if recover_fresh; then echo 'truncated journal was accepted' >&2; exit 1; fi
test "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$before"; test ! -s "$DELETE_LOG"

# A syntactically valid but contradictory committed journal with live pending
# create intent must also fail closed instead of finalizing away ownership evidence.
reset_old_local
kill_after_boundary committed
jtmp="$TMP/journal.tmp"
jq '.pending={kind:"dns-create",zone_id:"zone1",hostname:"mcp.example.com",value:"203.0.113.10",phase:"",marker:"0123456789abcdef"}' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
before="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"; : > "$DELETE_LOG"
if recover_fresh; then echo 'committed journal with pending create was accepted' >&2; exit 1; fi
test "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$before"; test ! -s "$DELETE_LOG"

# rolled_back is terminal only when no rollback-owned resources remain.
reset_old_local
kill_after_boundary committed
jq '.phase="rolled_back" | .pending={kind:"",zone_id:"",hostname:"",value:"",phase:"",marker:""} | .dns.action="created" | .dns.id="dns-new"' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
before="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"; : > "$DELETE_LOG"
if recover_fresh; then echo 'rolled_back journal with live rollback resources was accepted' >&2; exit 1; fi
test "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$before"; test ! -s "$DELETE_LOG"

echo 'cloudflare durable phase-recovery SIGKILL tests passed'
'''
Path('tests/cloudflare_phase_recovery.sh').write_text(phase_test)
