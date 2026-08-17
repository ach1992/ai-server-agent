#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
AGENT_USER=root
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; TLS_DIR="$CONFIG_DIR/tls"; CONFIG_FILE="$CONFIG_DIR/config.json"; MANAGED_STATE="$CONFIG_DIR/managed.json"; CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"; AUTH_HEADER_FILE="$CONFIG_DIR/mcp.authorization"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$TLS_DIR"
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
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
mock_cert(){ local stage="$3"; printf 'key\n' > "$stage/new.key"; printf 'csr\n' > "$stage/new.csr"; printf 'crt\n' > "$stage/new.crt"; CF_RESULT_CERT_ID=cert-new; }
mock_dns_created(){ CF_RESULT_DNS_ID=dns-new; CF_RESULT_DNS_OWNED=true; CF_RESULT_DNS_ACTION=created; CF_RESULT_DNS_OLD_CONTENT=""; CF_RESULT_DNS_OLD_PROXIED=""; CF_RESULT_DNS_OLD_TTL=""; }
mock_origin_fail(){ return 1; }
mock_origin_created(){ CF_RESULT_ORIGIN_RULESET_ID=origin-set; CF_RESULT_ORIGIN_RULE_ID=origin-rule; CF_RESULT_ORIGIN_ACTION=created; }
mock_ssl_created(){ CF_RESULT_SSL_RULESET_ID=ssl-set; CF_RESULT_SSL_RULE_ID=ssl-rule; CF_RESULT_SSL_ACTION=created; }
AI_SERVER_AGENT_HOSTNAME=mcp.example.com
AI_SERVER_AGENT_PORT=3210
cf_issue_origin_cert(){
  local stage="$3"
  printf 'key\n' > "$stage/new.key"; printf 'csr\n' > "$stage/new.csr"; printf 'crt\n' > "$stage/new.crt"
  CF_RESULT_CERT_ID=cert-midfail
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
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; case "$1" in */dns_records/dns-new) return 1 ;; *) return 0 ;; esac; }
expect_configure_failure 'rollback-delete failure'
test -s "$CF_TXN_STATE"
test "$(jq -r '.dns.id' "$CF_TXN_STATE")" = dns-new
test "$(jq -r '.dns.action' "$CF_TXN_STATE")" = created
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
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
# Ambiguous POST recovery: semantic DNS signature is removed even when no response ID was captured.
: > "$DELETE_LOG"
CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""
cf_find_dns_by_signature(){ printf 'dns-ambiguous\n'; }
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''
grep -qF '/zones/zone1/dns_records/dns-ambiguous' "$DELETE_LOG"
test -z "$CF_PENDING_KIND"
test ! -e "$CF_TXN_STATE"

# If semantic recovery itself cannot read Cloudflare, pending intent is preserved for a later cleanup retry.
CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""
cf_find_dns_by_signature(){ return 2; }
if rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''; then echo 'expected pending recovery journal' >&2; exit 1; fi
test -s "$CF_TXN_STATE"
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create
test "$(jq -r '.pending.hostname' "$CF_TXN_STATE")" = mcp.example.com
cf_find_dns_by_signature(){ printf 'dns-later\n'; }
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
recover_cloudflare_transaction
test ! -e "$CF_TXN_STATE"
test -z "$CF_PENDING_KIND"

# Deterministic rule refs also recover ambiguous rule creates without deleting an entire shared ruleset.
: > "$DELETE_LOG"
CF_PENDING_KIND=origin-rule-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=ai_server_agent_test; CF_PENDING_PHASE=http_request_origin
cf_find_rule_by_ref(){ printf 'shared-set|ambiguous-rule\n'; }
rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''
grep -qF '/zones/zone1/rulesets/shared-set/rules/ambiguous-rule' "$DELETE_LOG"
if grep -Fxq '/zones/zone1/rulesets/shared-set' "$DELETE_LOG"; then echo 'rollback deleted a whole ruleset' >&2; exit 1; fi

echo 'cloudflare transaction tests passed'
