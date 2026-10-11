#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
ORIGINAL_CF_API="$(declare -f cf_api)"
fail(){ echo "FAIL: $*" >&2; exit 1; }
for area in dns origin ssl; do
  guidance="$(cf_conflict_guidance mcp.example.com "$area")"
  grep -Fq 'mcp.example.com' <<<"$guidance" || fail "$area hostname"
  grep -Fq 'another unused domain or subdomain' <<<"$guidance" || fail "$area alternative"
  grep -Fq 'will not delete or adopt' <<<"$guidance" || fail "$area safety"
done
payload='{"errors":[{"code":1000,"message":"Resource already exists"}]}'
out="$(cf_report_provider_conflict '/zones/zone1/dns_records' "$payload" dns 2>&1)"
grep -Fq 'DNS > Records' <<<"$out" || fail 'DNS API guidance'
out="$(cf_report_provider_conflict '/zones/zone1/rulesets/origin-set/rules' "$payload" origin 2>&1)"
grep -Fq 'Rules > Origin Rules' <<<"$out" || fail 'Origin API guidance'
! grep -Fq 'Configuration Rules' <<<"$out" || fail 'Origin misclassified as Configuration'
out="$(cf_report_provider_conflict '/zones/zone1/rulesets/config-set/rules' "$payload" ssl 2>&1)"
grep -Fq 'Rules > Configuration Rules' <<<"$out" || fail 'Configuration API guidance'
! grep -Fq 'Origin Rules' <<<"$out" || fail 'Configuration misclassified as Origin'
out="$(cf_report_provider_conflict '/zones/zone1/rulesets/config-set/rules' "$payload" 2>&1)"
test -z "$out" || fail 'ambiguous Rulesets URL guessed a Rule type'
out="$(cf_report_provider_conflict '/zones/zone1/dns_records' "$payload" origin 2>&1)"
test -z "$out" || fail 'mismatched context mislabeled a DNS path'
out="$(cf_report_provider_conflict '/zones/zone1/dns_records' '{"errors":[{"message":"Invalid API token"}]}' dns 2>&1)"
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

# Stale DNS record identity: refuse the external replacement before mutation.
if out="$(cf_reconcile_dns zone1 mcp.example.com 203.0.113.10 old-owned-id '' 2>&1)"; then
  fail 'stale DNS ownership accepted an external replacement'
fi
grep -Fq 'Refusing to adopt or modify the replacement record' <<<"$out" || fail 'stale DNS safety decision'
grep -Fq 'DNS > Records' <<<"$out" || fail 'stale DNS Dashboard guidance'
grep -Fq 'another unused domain or subdomain' <<<"$out" || fail 'stale DNS alternate hostname'

# Recorded Rule ID absent but another external Rule reuses the Agent ref.
# Neither a journal write nor a Cloudflare API mutation may occur here.
host=mcp.example.com
origin_ref="ai_server_agent_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
ssl_ref="ai_server_agent_ssl_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
stale_log="$(mktemp)"
trap 'rm -f "$stale_log"' EXIT
cf_get_phase_entrypoint(){
  case "$2" in
    http_request_origin)
      jq -nc --arg ref "$origin_ref" '{id:"origin-set",kind:"zone",phase:"http_request_origin",rules:[{id:"other-origin",ref:$ref,action:"route",action_parameters:{origin:{port:9999}},expression:"http.host eq \"mcp.example.com\"",enabled:true}]}' ;;
    http_config_settings)
      jq -nc --arg ref "$ssl_ref" '{id:"ssl-set",kind:"zone",phase:"http_config_settings",rules:[{id:"other-config",ref:$ref,action:"set_config",action_parameters:{ssl:"flexible"},expression:"http.host eq \"mcp.example.com\"",enabled:true}]}' ;;
    *) fail "unexpected phase: $2" ;;
  esac
}
cf_set_pending_write(){ printf 'unexpected journal: %s\n' "$*" >> "$stale_log"; return 99; }
cf_api(){ printf 'unexpected API: %s\n' "$*" >> "$stale_log"; return 99; }
if out="$(cf_reconcile_origin_rule zone1 "$host" 3210 origin-set missing-origin '' 2>&1)"; then
  fail 'stale Origin ref collision accepted'
fi
grep -Fq 'not the rule recorded as Agent-owned' <<<"$out" || fail 'stale Origin refusal missing'
grep -Fq 'Rules > Origin Rules' <<<"$out" || fail 'stale Origin Dashboard guidance'
! grep -Fq 'Configuration Rules' <<<"$out" || fail 'stale Origin ambiguous section'
grep -Fq 'another unused domain or subdomain' <<<"$out" || fail 'stale Origin alternative'
test ! -s "$stale_log" || fail 'stale Origin attempted API/journal mutation'
if out="$(cf_reconcile_ssl_config_rule zone1 "$host" ssl-set missing-config '' 2>&1)"; then
  fail 'stale Configuration ref collision accepted'
fi
grep -Fq 'not the rule recorded as Agent-owned' <<<"$out" || fail 'stale Configuration refusal missing'
grep -Fq 'Rules > Configuration Rules' <<<"$out" || fail 'stale Configuration Dashboard guidance'
! grep -Fq 'Origin Rules' <<<"$out" || fail 'stale Configuration ambiguous section'
grep -Fq 'another unused domain or subdomain' <<<"$out" || fail 'stale Configuration alternative'
test ! -s "$stale_log" || fail 'stale Configuration attempted API/journal mutation'

# Exercise reconciliation -> real cf_api -> duplicate HTTP failure for BOTH
# Rulesets URLs, not just the diagnostic helper. curl is always mocked here.
check_rule_provider_duplicate() (
  local area="$1" mode="$2" wanted other out log_tmp url expected_url arg
  log_tmp="$(mktemp -d)"
  trap 'rm -rf "$log_tmp"' EXIT
  eval "$ORIGINAL_CF_API"
  CF_TOKEN="fixture-only"
  CF_API="https://cloudflare.invalid/client/v4"
  CF_PENDING_MARKER=""
  wanted='Rules > Origin Rules'
  other='Configuration Rules'
  if [ "$area" = ssl ]; then
    wanted='Rules > Configuration Rules'
    other='Origin Rules'
  fi
  cf_get_phase_entrypoint(){
    case "$mode:$2" in
      new:http_request_origin|new:http_config_settings) return 3 ;;
      existing:http_request_origin) jq -nc '{id:"origin-set",kind:"zone",phase:"http_request_origin",rules:[]}' ;;
      existing:http_config_settings) jq -nc '{id:"config-set",kind:"zone",phase:"http_config_settings",rules:[]}' ;;
      *) fail "unexpected phase in provider duplicate: $mode $2" ;;
    esac
  }
  cf_set_pending_write(){ printf 'PENDING %s\n' "$1" >> "$log_tmp/actions"; }
  curl(){
    local url="" arg
    for arg in "$@"; do url="$arg"; done
    printf 'POST %s\n' "$url" >> "$log_tmp/actions"
    case " $* " in *' --request POST '*) ;; *) fail 'non-POST provider operation' ;; esac
    case "$url" in
      "$CF_API/zones/zone1/rulesets"|"$CF_API/zones/zone1/rulesets/origin-set/rules"|"$CF_API/zones/zone1/rulesets/config-set/rules") ;;
      *) fail "unexpected mocked Cloudflare API URL: $url" ;;
    esac
    printf '%s' '{"success":false,"errors":[{"code":1000,"message":"Resource already exists"}]}'
    return 22
  }
  if [ "$area" = origin ]; then
    if out="$(cf_reconcile_origin_rule zone1 mcp.example.com 3210 '' '' '' 2>&1)"; then
      fail "Origin $mode duplicate was accepted"
    fi
  else
    if out="$(cf_reconcile_ssl_config_rule zone1 mcp.example.com '' '' '' 2>&1)"; then
      fail "Configuration $mode duplicate was accepted"
    fi
  fi
  grep -Fq "$wanted" <<<"$out" || fail "$area $mode missing specific Dashboard section"
  ! grep -Fq "$other" <<<"$out" || fail "$area $mode offered wrong Dashboard section"
  grep -Fq 'another unused domain/subdomain' <<<"$out" || fail "$area $mode missing hostname alternative"
  [ "$(grep -c '^POST ' "$log_tmp/actions")" -eq 1 ] || fail "$area $mode attempted extra API calls"
  [ "$(grep -c '^PENDING ' "$log_tmp/actions")" -eq 1 ] || fail "$area $mode lost pre-write transaction state"
  if [ "$mode" = new ]; then
    expected_url="$CF_API/zones/zone1/rulesets"
  elif [ "$area" = origin ]; then
    expected_url="$CF_API/zones/zone1/rulesets/origin-set/rules"
  else
    expected_url="$CF_API/zones/zone1/rulesets/config-set/rules"
  fi
  grep -Fxq "POST $expected_url" "$log_tmp/actions" || fail "$area $mode used wrong provider endpoint"
)
for area in origin ssl; do
  for mode in new existing; do
    check_rule_provider_duplicate "$area" "$mode"
  done
done

# DNS REST error also uses explicit DNS context, not a Rulesets URL heuristic.
check_dns_provider_duplicate() (
  eval "$ORIGINAL_CF_API"
  CF_TOKEN="fixture-only"
  CF_API="https://cloudflare.invalid/client/v4"
  curl(){
    local url="" arg
    for arg in "$@"; do url="$arg"; done
    [ "$url" = "$CF_API/zones/zone1/dns_records" ] || fail 'unexpected mocked DNS provider URL'
    printf '%s' '{"success":false,"errors":[{"code":1000,"message":"Resource already exists"}]}'
    return 22
  }
  if out="$(cf_api POST '/zones/zone1/dns_records' '{}' dns 2>&1)"; then
    fail 'DNS duplicate API failure accepted'
  fi
  grep -Fq 'DNS > Records' <<<"$out" || fail 'DNS provider context missing'
  ! grep -Fq 'Origin Rules' <<<"$out" || fail 'DNS provider Rule misclassification'
)
check_dns_provider_duplicate

echo 'Cloudflare conflict guidance PASS'
