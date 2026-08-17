#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run cloudflare_phase_recovery.sh as root" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; CONTROL_DIR="$CONFIG_DIR/control"; TLS_DIR="$CONFIG_DIR/tls"; CONFIG_FILE="$CONFIG_DIR/config.json"; MANAGED_STATE="$CONFIG_DIR/managed.json"; CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"; CF_TXN_BACKUP_DIR="$CONTROL_DIR/cloudflare-transaction-backup"; MANAGEMENT_LOCK="$CONTROL_DIR/management.lock"; MANAGEMENT_LOCK_FD=""; DELETE_LOG="$TMP/deletes.log"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$CONTROL_DIR" "$TLS_DIR"
old_config='{"listen_address":"127.0.0.1:3210","tls_cert_file":"","tls_key_file":""}'
old_managed='{"active_provider":"local","hostname":"old.example.com","port":3210,"cloudflare":{}}'
new_config='{"listen_address":"0.0.0.0:3210","tls_cert_file":"new","tls_key_file":"new"}'
new_managed='{"active_provider":"cloudflare","hostname":"mcp.example.com","port":3210,"cloudflare":{"zone_id":"zone1","zone_name":"example.com","dns_record_id":"dns-new","dns_record_owned":true,"dns_record_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","origin_ruleset_id":"origin-set","origin_rule_id":"origin-rule","origin_rule_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ssl_config_ruleset_id":"ssl-set","ssl_config_rule_id":"ssl-rule","ssl_config_rule_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","origin_certificate_id":"cert-new"}}'

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
    TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_MANAGEMENT_LOCK="$MANAGEMENT_LOCK" \
    TEST_NEW_CONFIG="$new_config" TEST_NEW_MANAGED="$new_managed" \
    bash -c '
      set -Eeuo pipefail
      export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
      source "$ROOT/manage.sh"
      AGENT_USER=root
      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""
      systemctl(){ return 0; }
      host=mcp.example.com; zone_id=zone1
      dns_id=dns-new; dns_action=created; dns_fingerprint=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      origin_ruleset_id=origin-set; origin_rule_id=origin-rule; origin_action=created; origin_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      ssl_ruleset_id=ssl-set; ssl_rule_id=ssl-rule; ssl_action=created; ssl_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc; cert_id=cert-new
      prepare_cloudflare_local_backup
      CF_TXN_PHASE=applying
      save_current_cloudflare_transaction_state
      printf "%s\n" "$TEST_NEW_CONFIG" > "$CONFIG_FILE"
      printf "new-key\n" > "$TLS_DIR/origin.key"; printf "new-crt\n" > "$TLS_DIR/origin.crt"
      case "$TEST_SCENARIO" in
        applying) ;;
        rolling_back)
          restore_cloudflare_local_backup
          cf_clear_commit_intent
          CF_TXN_PHASE=rolling_back
          cf_checkpoint_transaction
          ;;
        committing|committed)
          cf_set_commit_intent mcp.example.com 3210 zone1 example.com dns-new true origin-set origin-rule ssl-set ssl-rule cert-new aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
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
    TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_MANAGEMENT_LOCK="$MANAGEMENT_LOCK" TEST_DELETE_LOG="$DELETE_LOG" \
    bash -c '
      set -Eeuo pipefail
      export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
      source "$ROOT/manage.sh"
      AGENT_USER=root
      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""
      systemctl(){ return 0; }
      cf_delete_dns_if_expected(){ printf "/zones/%s/dns_records/%s\n" "$1" "$2" >> "$TEST_DELETE_LOG"; return 0; }
      cf_delete_rule_if_expected(){ printf "/zones/%s/rulesets/%s/rules/%s\n" "$1" "$2" "$3" >> "$TEST_DELETE_LOG"; return 0; }
      cf_delete_owned(){ printf "%s\n" "$1" >> "$TEST_DELETE_LOG"; return 0; }
      cf_recover_pending_write(){ cf_clear_pending_write; return 0; }
      recover_cloudflare_transaction
    '
}

write_prepared_pending(){
  local kind="$1"
  env \
    ROOT="$ROOT" TEST_KIND="$kind" \
    TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" \
    TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_MANAGEMENT_LOCK="$MANAGEMENT_LOCK" \
    bash -c '
      set -Eeuo pipefail
      export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
      source "$ROOT/manage.sh"
      AGENT_USER=root
      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""
      host=mcp.example.com; zone_id=zone1; dns_id=""; dns_action=""; dns_fingerprint=""; origin_ruleset_id=""; origin_rule_id=""; origin_action=""; origin_fingerprint=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""; ssl_fingerprint=""; cert_id=""
      prepare_cloudflare_local_backup
      CF_TXN_PHASE=prepared
      case "$TEST_KIND" in
        dns) cf_set_pending_write dns-create zone1 mcp.example.com 203.0.113.10 "" 0123456789abcdef0123456789abcdef aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
        origin) cf_set_pending_write origin-rule-create zone1 mcp.example.com ai_server_agent_test http_request_origin 0123456789abcdef0123456789abcdef bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
        *) exit 2 ;;
      esac
    '
}

expect_journal_rejected(){
  local label="$1" before
  before="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"; : > "$DELETE_LOG"
  if recover_fresh; then echo "$label was accepted" >&2; exit 1; fi
  test "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$before"
  test ! -s "$DELETE_LOG"
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

# A persisted rolling_back phase means local restoration already completed; a
# fresh process resumes only remote rollback and finalization.
reset_old_local
kill_after_boundary rolling_back
: > "$DELETE_LOG"
recover_fresh
cmp -s <(printf '%s\n' "$old_config") "$CONFIG_FILE"
cmp -s <(printf '%s\n' "$old_managed") "$MANAGED_STATE"
grep -qF '/zones/zone1/dns_records/dns-new' "$DELETE_LOG"
grep -qF '/zones/zone1/rulesets/origin-set/rules/origin-rule' "$DELETE_LOG"
grep -qF '/zones/zone1/rulesets/ssl-set/rules/ssl-rule' "$DELETE_LOG"
grep -qF '/certificates/cert-new' "$DELETE_LOG"
test ! -e "$CF_TXN_STATE"; test ! -e "$CF_TXN_BACKUP_DIR"

# Real SIGKILL immediately after managed-state replacement while durable phase
# is committing. Exact committed identity finalizes without rollback.
reset_old_local
kill_after_boundary committing
: > "$DELETE_LOG"
recover_fresh
test ! -s "$DELETE_LOG"
cmp -s <(printf '%s\n' "$new_config") "$CONFIG_FILE"
cmp -s <(printf '%s\n' "$new_managed") "$MANAGED_STATE"
test ! -e "$CF_TXN_STATE"; test ! -e "$CF_TXN_BACKUP_DIR"

# Real SIGKILL after durable phase=committed but before journal deletion also
# finalizes only when managed state proves the exact commit.
reset_old_local
kill_after_boundary committed
: > "$DELETE_LOG"
recover_fresh
test ! -s "$DELETE_LOG"
cmp -s <(printf '%s\n' "$new_config") "$CONFIG_FILE"
cmp -s <(printf '%s\n' "$new_managed") "$MANAGED_STATE"
test ! -e "$CF_TXN_STATE"; test ! -e "$CF_TXN_BACKUP_DIR"

reset_old_local
kill_after_boundary committed
printf '%s\n' "$old_managed" > "$MANAGED_STATE"
chown root:root "$MANAGED_STATE"; chmod 0640 "$MANAGED_STATE"
expect_journal_rejected 'committed journal with mismatched managed state'
test -d "$CF_TXN_BACKUP_DIR"

reset_old_local
kill_after_boundary committed
rm -f "$MANAGED_STATE"
expect_journal_rejected 'committed journal with missing managed state'
test -d "$CF_TXN_BACKUP_DIR"

# Truncated/unknown-schema state fails closed and remains untouched.
reset_old_local
printf '{"version":1,"phase":"applying"' > "$CF_TXN_STATE"
chown root:root "$CF_TXN_STATE"; chmod 0600 "$CF_TXN_STATE"
expect_journal_rejected 'truncated journal'

# Pending identity is bound to the top-level transaction zone/hostname.
reset_old_local
write_prepared_pending dns
jtmp="$TMP/journal.tmp"
jq '.pending.zone_id="zone2"' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
expect_journal_rejected 'pending journal with mismatched zone identity'

reset_old_local
write_prepared_pending dns
jq '.pending.hostname="other.example.com"' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
expect_journal_rejected 'pending journal with mismatched hostname identity'

# Pending kind is semantically bound to the correct Cloudflare ruleset phase.
reset_old_local
write_prepared_pending origin
jq '.pending.phase="http_config_settings"' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
expect_journal_rejected 'origin pending journal with Configuration Rules phase'

# Pending create evidence cannot survive into local application/commit phases.
reset_old_local
write_prepared_pending dns
jq '.phase="applying"' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
expect_journal_rejected 'applying journal with pending create'

reset_old_local
kill_after_boundary committed
jq '.pending={kind:"dns-create",zone_id:"zone1",hostname:"mcp.example.com",value:"203.0.113.10",phase:"",marker:"0123456789abcdef",fingerprint:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
expect_journal_rejected 'committed journal with pending create'

# Commit identity is bound to the transaction and recorded remote resources.
reset_old_local
kill_after_boundary committed
jq '.commit.zone_id="zone2"' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
expect_journal_rejected 'committed journal with contradictory commit identity'

# rolled_back is terminal only with no pending/rollback-owned resources and no
# abandoned commit intent.
reset_old_local
kill_after_boundary committed
jq '.phase="rolled_back" | .pending={kind:"",zone_id:"",hostname:"",value:"",phase:"",marker:"",fingerprint:""} | .commit={hostname:"",port:"",zone_id:"",zone_name:"",dns_id:"",dns_owned:false,dns_fingerprint:"",origin_ruleset_id:"",origin_rule_id:"",origin_rule_fingerprint:"",ssl_ruleset_id:"",ssl_rule_id:"",ssl_rule_fingerprint:"",certificate_id:""} | .dns.action="created" | .dns.id="dns-new"' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
expect_journal_rejected 'rolled_back journal with live rollback resource'

reset_old_local
kill_after_boundary committed
jq '.phase="rolled_back" | .pending={kind:"",zone_id:"",hostname:"",value:"",phase:"",marker:"",fingerprint:""} | .dns={id:"",action:"",fingerprint:""} | .origin={ruleset_id:"",rule_id:"",action:"",fingerprint:""} | .ssl={ruleset_id:"",rule_id:"",action:"",fingerprint:""} | .certificate_id=""' "$CF_TXN_STATE" > "$jtmp"
install -o root -g root -m 0600 "$jtmp" "$CF_TXN_STATE"
expect_journal_rejected 'rolled_back journal with stale commit intent'

echo 'cloudflare durable phase-recovery SIGKILL tests passed'
