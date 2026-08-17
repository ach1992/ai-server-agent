from pathlib import Path

manage_path = Path("manage.sh")
text = manage_path.read_text()
old = '''    if [ "$rc" -ne 0 ] && [ "$transaction_committed" -ne 1 ]; then
      if [ "$local_mutation_started" -eq 1 ]; then'''
new = '''    if [ "$rc" -ne 0 ] && [ "$transaction_committed" -ne 1 ]; then
      [ -n "$cert_id" ] || cert_id="${CF_RESULT_CERT_ID:-}"
      [ -n "$dns_id" ] || dns_id="${CF_RESULT_DNS_ID:-}"
      [ -n "$dns_action" ] || dns_action="${CF_RESULT_DNS_ACTION:-}"
      [ -n "$dns_old_content" ] || dns_old_content="${CF_RESULT_DNS_OLD_CONTENT:-}"
      [ -n "$dns_old_proxied" ] || dns_old_proxied="${CF_RESULT_DNS_OLD_PROXIED:-}"
      [ -n "$dns_old_ttl" ] || dns_old_ttl="${CF_RESULT_DNS_OLD_TTL:-}"
      [ -n "$origin_ruleset_id" ] || origin_ruleset_id="${CF_RESULT_ORIGIN_RULESET_ID:-}"
      [ -n "$origin_rule_id" ] || origin_rule_id="${CF_RESULT_ORIGIN_RULE_ID:-}"
      [ -n "$origin_action" ] || origin_action="${CF_RESULT_ORIGIN_ACTION:-}"
      [ -n "$ssl_ruleset_id" ] || ssl_ruleset_id="${CF_RESULT_SSL_RULESET_ID:-}"
      [ -n "$ssl_rule_id" ] || ssl_rule_id="${CF_RESULT_SSL_RULE_ID:-}"
      [ -n "$ssl_action" ] || ssl_action="${CF_RESULT_SSL_ACTION:-}"
      if [ "$local_mutation_started" -eq 1 ]; then'''
if text.count(old) != 1:
    raise SystemExit(f"expected one transaction trap marker, got {text.count(old)}")
manage_path.write_text(text.replace(old, new, 1))

test_path = Path("tests/cloudflare_transaction.sh")
test = test_path.read_text()
marker = '''AI_SERVER_AGENT_HOSTNAME=mcp.example.com
AI_SERVER_AGENT_PORT=3210
cf_issue_origin_cert(){ mock_cert "$@"; }
cf_reconcile_dns(){ mock_dns_created; }'''
replacement = '''AI_SERVER_AGENT_HOSTNAME=mcp.example.com
AI_SERVER_AGENT_PORT=3210
cf_issue_origin_cert(){
  local stage="$2"
  printf 'key\\n' > "$stage/new.key"; printf 'csr\\n' > "$stage/new.csr"; printf 'crt\\n' > "$stage/new.crt"
  CF_RESULT_CERT_ID=cert-midfail
  return 1
}
if configure_cloudflare >/dev/null 2>&1; then echo 'expected certificate helper failure' >&2; exit 1; fi
grep -qF '/certificates/cert-midfail' "$DELETE_LOG"
test ! -e "$CF_TXN_STATE"
: > "$DELETE_LOG"
cf_issue_origin_cert(){ mock_cert "$@"; }
cf_reconcile_dns(){ mock_dns_created; }'''
if test.count(marker) != 1:
    raise SystemExit(f"expected one behavioral test marker, got {test.count(marker)}")
test_path.write_text(test.replace(marker, replacement, 1))
print("transaction trap fallback applied")
