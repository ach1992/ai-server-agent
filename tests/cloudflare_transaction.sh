#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
AGENT_USER=root
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; CONTROL_DIR="$CONFIG_DIR/control"; TLS_DIR="$CONFIG_DIR/tls"; CONFIG_FILE="$CONFIG_DIR/config.json"; MANAGED_STATE="$CONFIG_DIR/managed.json"; CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"; CF_TXN_BACKUP_DIR="$CONTROL_DIR/cloudflare-transaction-backup"; MANAGEMENT_LOCK="$CONTROL_DIR/management.lock"; MANAGEMENT_LOCK_FD=""; AUTH_HEADER_FILE="$CONFIG_DIR/mcp.authorization"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$CONTROL_DIR" "$TLS_DIR"
printf '{"listen_address":"127.0.0.1:3210","tls_cert_file":"","tls_key_file":""}\n' > "$CONFIG_FILE"
printf 'Bearer test\n' > "$AUTH_HEADER_FILE"
printf '{}\n' > "$MANAGED_STATE"
need_cmd(){ :; }
load_cf_token(){ CF_TOKEN=test; }
cf_find_zone(){ printf 'zone1|example.com\n'; }
cf_public_ipv4(){ printf '203.0.113.10\n'; }
restart_and_verify_local(){ return 0; }
verify_public(){ return 0; }
systemctl(){ return 0; }
confirm(){ return 1; }
expect_configure_failure(){
  local label="$1" rc
  set +e
  ( set -Eeuo pipefail; configure_cloudflare >/dev/null 2>&1 )
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || { echo "expected $label" >&2; exit 1; }
}
DELETE_LOG="$TMP/deletes.log"
ORIG_CF_DELETE_DNS_IF_EXPECTED="$(declare -f cf_delete_dns_if_expected)"
ORIG_CF_DELETE_RULE_IF_EXPECTED="$(declare -f cf_delete_rule_if_expected)"
ORIG_CF_GET_OPTIONAL="$(declare -f cf_get_optional)"
ORIG_CF_RECONCILE_SSL_CONFIG_RULE="$(declare -f cf_reconcile_ssl_config_rule)"
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
cf_delete_dns_if_expected(){ printf '/zones/%s/dns_records/%s\n' "$1" "$2" >> "$DELETE_LOG"; [ -n "$3" ]; }
cf_delete_rule_if_expected(){ printf '/zones/%s/rulesets/%s/rules/%s\n' "$1" "$2" "$3" >> "$DELETE_LOG"; [ -n "$4" ]; }
mock_cert(){ local stage="$3"; printf 'key\n' > "$stage/new.key"; printf 'csr\n' > "$stage/new.csr"; printf 'crt\n' > "$stage/new.crt"; CF_RESULT_CERT_ID=cert-new; }
mock_dns_created(){ CF_RESULT_DNS_ID=dns-new; CF_RESULT_DNS_OWNED=true; CF_RESULT_DNS_ACTION=created; CF_RESULT_DNS_OLD_CONTENT=""; CF_RESULT_DNS_OLD_PROXIED=""; CF_RESULT_DNS_OLD_TTL=""; CF_RESULT_DNS_FINGERPRINT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; }
mock_origin_fail(){ return 1; }
mock_origin_created(){ CF_RESULT_ORIGIN_RULESET_ID=origin-set; CF_RESULT_ORIGIN_RULE_ID=origin-rule; CF_RESULT_ORIGIN_ACTION=created; CF_RESULT_ORIGIN_FINGERPRINT=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; }
mock_ssl_created(){ CF_RESULT_SSL_RULESET_ID=ssl-set; CF_RESULT_SSL_RULE_ID=ssl-rule; CF_RESULT_SSL_ACTION=created; CF_RESULT_SSL_FINGERPRINT=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc; }
AI_SERVER_AGENT_HOSTNAME=mcp.example.com
AI_SERVER_AGENT_PORT=3210
cf_issue_origin_cert(){
  local stage="$3"
  printf 'key\n' > "$stage/new.key"; printf 'csr\n' > "$stage/new.csr"; printf 'crt\n' > "$stage/new.crt"
  CF_RESULT_CERT_ID=cert-midfail
  cert_id=cert-midfail
  cf_checkpoint_transaction
  return 1
}
expect_configure_failure 'certificate helper failure'
grep -qF '/certificates/cert-midfail' "$DELETE_LOG"
test ! -e "$CF_TXN_STATE"
: > "$DELETE_LOG"
cf_issue_origin_cert(){ mock_cert "$@"; }
cf_reconcile_dns(){ mock_dns_created; }
cf_reconcile_origin_rule(){ mock_origin_fail; }
cf_reconcile_ssl_config_rule(){ mock_ssl_created; }
expect_configure_failure 'origin-stage failure'
grep -qF '/zones/zone1/dns_records/dns-new' "$DELETE_LOG"
grep -qF '/certificates/cert-new' "$DELETE_LOG"
test ! -e "$CF_TXN_STATE"
: > "$DELETE_LOG"
cf_delete_dns_if_expected(){ printf '/zones/%s/dns_records/%s\n' "$1" "$2" >> "$DELETE_LOG"; return 1; }
expect_configure_failure 'rollback-delete failure'
test -s "$CF_TXN_STATE"
test "$(jq -r '.dns.id' "$CF_TXN_STATE")" = dns-new
test "$(jq -r '.dns.action' "$CF_TXN_STATE")" = created
cf_delete_dns_if_expected(){ printf '/zones/%s/dns_records/%s\n' "$1" "$2" >> "$DELETE_LOG"; return 0; }
recover_cloudflare_transaction
test ! -e "$CF_TXN_STATE"
: > "$DELETE_LOG"
printf '{}\n' > "$MANAGED_STATE"
cf_reconcile_origin_rule(){ mock_origin_created; }
cf_reconcile_ssl_config_rule(){ mock_ssl_created; }
save_cloudflare_state(){ return 1; }
expect_configure_failure 'state-write failure'
grep -qF '/zones/zone1/dns_records/dns-new' "$DELETE_LOG"
grep -qF '/zones/zone1/rulesets/origin-set/rules/origin-rule' "$DELETE_LOG"
grep -qF '/zones/zone1/rulesets/ssl-set/rules/ssl-rule' "$DELETE_LOG"
grep -qF '/certificates/cert-new' "$DELETE_LOG"
test ! -e "$CF_TXN_STATE"
printf '{"active_provider":"cloudflare","hostname":"mcp.example.com","cloudflare":{"zone_id":"zone1","origin_certificate_id":"cert-new"}}\n' > "$MANAGED_STATE"
save_previous_cloudflare_certificate mcp.example.com cert-old
test "$(jq -r '.cloudflare_previous_certificate.origin_certificate_id' "$MANAGED_STATE")" = cert-old
confirm(){ return 1; }
cleanup_old_cloudflare mcp.example.com zone1 '' false '' '' '' '' cert-old mcp.example.com cert-new zone1 >/dev/null
test "$(jq -r '.cloudflare_previous_certificate.origin_certificate_id' "$MANAGED_STATE")" = cert-old
confirm(){ return 0; }
cf_delete_owned(){ return 1; }
cleanup_old_cloudflare mcp.example.com zone1 '' false '' '' '' '' cert-old mcp.example.com cert-new zone1 >/dev/null
test "$(jq -r '.cloudflare_previous_certificate.origin_certificate_id' "$MANAGED_STATE")" = cert-old
cf_delete_owned(){ return 0; }
cleanup_old_cloudflare mcp.example.com zone1 '' false '' '' '' '' cert-old mcp.example.com cert-new zone1 >/dev/null
test "$(jq -r '.cloudflare_previous_certificate.origin_certificate_id // empty' "$MANAGED_STATE")" = ''
ORIG_CF_FIND_DNS_BY_MARKER="$(declare -f cf_find_dns_by_marker)"
ORIG_CF_FIND_RULE_BY_MARKER="$(declare -f cf_find_rule_by_marker)"
# Ambiguous POST recovery: semantic DNS signature is removed even when no response ID was captured.
: > "$DELETE_LOG"
CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""; CF_PENDING_MARKER=marker-good
cf_find_dns_by_marker(){ printf 'dns-ambiguous\n'; }
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''
grep -qF '/zones/zone1/dns_records/dns-ambiguous' "$DELETE_LOG"
test -z "$CF_PENDING_KIND"
test ! -e "$CF_TXN_STATE"

# If semantic recovery itself cannot read Cloudflare, pending intent is preserved for a later cleanup retry.
CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""; CF_PENDING_MARKER=abcdefabcdefabcdefabcdefabcdefab
cf_find_dns_by_marker(){ return 2; }
if rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''; then echo 'expected pending recovery journal' >&2; exit 1; fi
test -s "$CF_TXN_STATE"
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create
test "$(jq -r '.pending.hostname' "$CF_TXN_STATE")" = mcp.example.com
cf_find_dns_by_marker(){ printf 'dns-later\n'; }
cf_delete_dns_if_expected(){ printf '/zones/%s/dns_records/%s\n' "$1" "$2" >> "$DELETE_LOG"; return 0; }
recover_cloudflare_transaction
test ! -e "$CF_TXN_STATE"
test -z "$CF_PENDING_KIND"

# Deterministic rule refs also recover ambiguous rule creates without deleting an entire shared ruleset.
: > "$DELETE_LOG"
CF_PENDING_KIND=origin-rule-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=ai_server_agent_test; CF_PENDING_PHASE=http_request_origin; CF_PENDING_MARKER=rule-marker
cf_find_rule_by_marker(){ printf 'shared-set|ambiguous-rule\n'; }
rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''
grep -qF '/zones/zone1/rulesets/shared-set/rules/ambiguous-rule' "$DELETE_LOG"
if grep -Fxq '/zones/zone1/rulesets/shared-set' "$DELETE_LOG"; then echo 'rollback deleted a whole ruleset' >&2; exit 1; fi


eval "$ORIG_CF_FIND_DNS_BY_MARKER"
eval "$ORIG_CF_FIND_RULE_BY_MARKER"

# Durable pre-POST journal includes the unpredictable marker before any create request.
rm -f "$CF_TXN_STATE"
host=mcp.example.com; zone_id=zone1; old_port=3210; dns_id=""; dns_action=""; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""; origin_ruleset_id=""; origin_rule_id=""; origin_action=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""; cert_id=""
cf_set_pending_write dns-create zone1 mcp.example.com 203.0.113.10 "" nonce-before-post
test -s "$CF_TXN_STATE"
test "$(jq -r '.pending.marker' "$CF_TXN_STATE")" = nonce-before-post
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create
clear_cloudflare_transaction_state; cf_clear_pending_write

# A same-shape concurrent DNS record without our nonce is ambiguous and must never be deleted.
cf_api(){ printf '%s' '{"success":true,"result":[{"id":"external-dns","type":"A","name":"mcp.example.com","content":"203.0.113.10","proxied":true,"comment":"Managed by AI Server Agent"}]}' ; }
if cf_find_dns_by_marker zone1 mcp.example.com 203.0.113.10 our-secret-marker >/dev/null; then echo 'concurrent DNS was treated as owned' >&2; exit 1; else test "$?" -eq 2; fi

# A matching deterministic ref without our nonce is ambiguous in both rule phases.
cf_api(){
  case "$2" in
    '/zones/zone1/rulesets?per_page=100') printf '%s' '{"success":true,"result":[{"id":"origin-set-race","kind":"zone","phase":"http_request_origin"}]}' ;;
    '/zones/zone1/rulesets/origin-set-race') printf '%s' '{"success":true,"result":{"rules":[{"id":"external-origin","ref":"ai_server_agent_test","description":"AI Server Agent origin port"}]}}' ;;
    *) return 2 ;;
  esac
}
if cf_find_rule_by_marker zone1 http_request_origin ai_server_agent_test our-origin-marker >/dev/null; then echo 'concurrent Origin Rule was treated as owned' >&2; exit 1; else test "$?" -eq 2; fi
cf_api(){
  case "$2" in
    '/zones/zone1/rulesets?per_page=100') printf '%s' '{"success":true,"result":[{"id":"ssl-set-race","kind":"zone","phase":"http_config_settings"}]}' ;;
    '/zones/zone1/rulesets/ssl-set-race') printf '%s' '{"success":true,"result":{"rules":[{"id":"external-ssl","ref":"ai_server_agent_ssl_test","description":"AI Server Agent strict SSL"}]}}' ;;
    *) return 2 ;;
  esac
}
if cf_find_rule_by_marker zone1 http_config_settings ai_server_agent_ssl_test our-ssl-marker >/dev/null; then echo 'concurrent Configuration Rule was treated as owned' >&2; exit 1; else test "$?" -eq 2; fi

echo 'cloudflare transaction tests passed'

# A response-lost ruleset create may recover its exact nonce-marked rule, but
# must never delete the ruleset container: another writer can add a rule after
# our inspection and before a whole-ruleset DELETE.
: > "$DELETE_LOG"
CF_PENDING_KIND=origin-ruleset-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=ai_server_agent_ruleset_test; CF_PENDING_PHASE=http_request_origin; CF_PENDING_MARKER=abcdef0123456789abcdef0123456789
cf_find_ruleset_by_marker(){ printf 'marked-set\n'; }
cf_api(){
  case "$1:$2" in
    GET:/zones/zone1/rulesets/marked-set)
      printf '%s' '{"success":true,"result":{"rules":[{"id":"agent-rule","ref":"ai_server_agent_ruleset_test","description":"AI Server Agent origin port txn:abcdef0123456789abcdef0123456789"}]}}'
      ;;
    *) return 2 ;;
  esac
}
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
cf_recover_pending_write
grep -Fxq '/zones/zone1/rulesets/marked-set/rules/agent-rule' "$DELETE_LOG"
if grep -Fxq '/zones/zone1/rulesets/marked-set' "$DELETE_LOG"; then echo 'pending recovery deleted a whole ruleset' >&2; exit 1; fi


# Global management lock excludes a second root management process and is reusable after release.
rm -f "$MANAGEMENT_LOCK" "$TMP/lock-ready"
(
  MANAGEMENT_LOCK_FD=""
  acquire_management_lock
  printf 'ready\n' > "$TMP/lock-ready"
  sleep 2
) &
lock_pid=$!
for _ in $(seq 1 50); do [ -s "$TMP/lock-ready" ] && break; sleep 0.05; done
[ -s "$TMP/lock-ready" ] || { echo 'management lock holder did not start' >&2; exit 1; }
if ( MANAGEMENT_LOCK_FD=""; acquire_management_lock ); then echo 'second management process acquired singleton lock' >&2; kill "$lock_pid" 2>/dev/null || true; exit 1; fi
wait "$lock_pid"
( MANAGEMENT_LOCK_FD=""; acquire_management_lock )

# Restore real conditional deletion helpers for representation-drift tests.
eval "$ORIG_CF_DELETE_DNS_IF_EXPECTED"
eval "$ORIG_CF_DELETE_RULE_IF_EXPECTED"
eval "$ORIG_CF_GET_OPTIONAL"
eval "$ORIG_CF_RECONCILE_SSL_CONFIG_RULE"

# Same-host canonical owned SSL rule is a no-op, never an unsupported journal action=updated.
ssl_ref="ai_server_agent_ssl_$(printf '%s' mcp.example.com | sha256sum | cut -c1-16)"
ssl_rule_json="$(jq -nc --arg ref "$ssl_ref" '{id:"ssl-rule-owned",ref:$ref,description:"AI Server Agent strict SSL",expression:"http.host eq \"mcp.example.com\"",action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
ssl_fp="$(cf_rule_fingerprint <<<"$ssl_rule_json")"
cf_api(){
  case "$2" in
    '/zones/zone1/rulesets?per_page=100') printf '%s' '{"success":true,"result":[{"id":"ssl-set-owned","kind":"zone","phase":"http_config_settings"}]}' ;;
    '/zones/zone1/rulesets/ssl-set-owned') jq -nc --argjson rule "$ssl_rule_json" '{success:true,result:{rules:[$rule]}}' ;;
    *) return 2 ;;
  esac
}
cf_reconcile_ssl_config_rule zone1 mcp.example.com ssl-set-owned ssl-rule-owned "$ssl_fp"
test "$CF_RESULT_SSL_ACTION" = ''
test "$CF_RESULT_SSL_FINGERPRINT" = "$ssl_fp"

# A recorded mutable resource is deleted only if its current representation still matches the durable fingerprint.
expected_dns='{"id":"dns-owned","type":"A","name":"mcp.example.com","content":"203.0.113.10","ttl":1,"proxied":true,"comment":"Managed by AI Server Agent txn:0123456789abcdef"}'
expected_dns_fp="$(cf_dns_fingerprint <<<"$expected_dns")"
cf_get_optional(){ jq -nc --argjson result "$expected_dns" '{success:true,result:$result}'; }
: > "$DELETE_LOG"
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
cf_delete_dns_if_expected zone1 dns-owned "$expected_dns_fp"
grep -Fxq '/zones/zone1/dns_records/dns-owned' "$DELETE_LOG"
mutated_dns='{"id":"dns-owned","type":"A","name":"mcp.example.com","content":"198.51.100.77","ttl":1,"proxied":true,"comment":"external concurrent change"}'
cf_get_optional(){ jq -nc --argjson result "$mutated_dns" '{success:true,result:$result}'; }
: > "$DELETE_LOG"
if cf_delete_dns_if_expected zone1 dns-owned "$expected_dns_fp"; then echo 'drifted DNS was deleted' >&2; exit 1; fi
test ! -s "$DELETE_LOG"

expected_rule='{"id":"rule-owned","ref":"ai_server_agent_test","description":"AI Server Agent origin port txn:0123456789abcdef","expression":"http.host eq \"mcp.example.com\"","action":"route","action_parameters":{"origin":{"port":3210}},"enabled":true}'
expected_rule_fp="$(cf_rule_fingerprint <<<"$expected_rule")"
cf_get_optional(){ jq -nc --argjson rule "$expected_rule" '{success:true,result:{rules:[$rule]}}'; }
: > "$DELETE_LOG"
cf_delete_rule_if_expected zone1 ruleset-owned rule-owned "$expected_rule_fp"
grep -Fxq '/zones/zone1/rulesets/ruleset-owned/rules/rule-owned' "$DELETE_LOG"
mutated_rule='{"id":"rule-owned","ref":"ai_server_agent_test","description":"external concurrent change","expression":"http.host eq \"mcp.example.com\"","action":"route","action_parameters":{"origin":{"port":9999}},"enabled":true}'
cf_get_optional(){ jq -nc --argjson rule "$mutated_rule" '{success:true,result:{rules:[$rule]}}'; }
: > "$DELETE_LOG"
if cf_delete_rule_if_expected zone1 ruleset-owned rule-owned "$expected_rule_fp"; then echo 'drifted rule was deleted' >&2; exit 1; fi
test ! -s "$DELETE_LOG"

# Cleanup with missing legacy fingerprint fails closed rather than deleting a recorded mutable ID blindly.
if delete_recorded_cloudflare_resources zone1 dns-owned true '' '' '' '' '' '' '' ''; then echo 'cleanup accepted missing DNS fingerprint' >&2; exit 1; fi

echo 'pending ruleset recovery never deletes the ruleset container'
