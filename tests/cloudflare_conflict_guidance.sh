#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
fail(){ echo "FAIL: $*" >&2; exit 1; }
for area in dns origin ssl; do
  guidance="$(cf_conflict_guidance mcp.example.com "$area")"
  grep -Fq 'mcp.example.com' <<<"$guidance" || fail "$area hostname"
  grep -Fq 'another unused domain or subdomain' <<<"$guidance" || fail "$area alternative"
  grep -Fq 'will not delete or adopt' <<<"$guidance" || fail "$area safety"
done
payload='{"errors":[{"code":1000,"message":"Resource already exists"}]}'
out="$(cf_report_provider_conflict '/zones/zone1/dns_records' "$payload" 2>&1)"
grep -Fq 'DNS > Records' <<<"$out" || fail 'DNS API guidance'
out="$(cf_report_provider_conflict '/zones/zone1/rulesets/origin-set/rules' "$payload" 2>&1)"
grep -Fq 'Origin Rules / Configuration Rules' <<<"$out" || fail 'Rule API guidance'
out="$(cf_report_provider_conflict '/zones/zone1/dns_records' '{"errors":[{"message":"Invalid API token"}]}' 2>&1)"
test -z "$out" || fail 'unrelated auth error misclassified'
cf_api(){
  case "$1:$2" in
    GET:/zones/zone1/dns_records?name=mcp.example.com\&per_page=100)
      jq -nc '{success:true,result:[{id:"external",type:"A",name:"mcp.example.com",content:"198.51.100.7",proxied:true}]}' ;;
    *) fail "unexpected mutation: $1 $2" ;;
  esac
}
if out="$(cf_reconcile_dns zone1 mcp.example.com 203.0.113.10 '' '' 2>&1)"; then fail 'unowned DNS conflict accepted'; fi
grep -Fq 'DNS > Records' <<<"$out" || fail 'DNS reconciliation guidance'
grep -Fq 'another unused domain or subdomain' <<<"$out" || fail 'DNS alternate hostname'
echo 'Cloudflare conflict guidance PASS'
