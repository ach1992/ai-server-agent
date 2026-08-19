#!/usr/bin/env python3
from pathlib import Path
import hashlib
p=Path('tests/cloudflare_rulesets_pagination.sh'); data=p.read_bytes(); sha=hashlib.sha1(f'blob {len(data)}\0'.encode()+data).hexdigest()
if sha!='e1a6fb7e40125af5a07c01890786ef0d89f3f0cc': raise SystemExit(f'unexpected rulesets test blob: {sha}')
s=data.decode(); end="printf 'Cloudflare Rulesets phase-entrypoint discovery contract passed.\\n'\n"
if s.count(end)!=1: raise SystemExit('test completion drift')
block=r'''
# Existing-ruleset POST /rules returns the updated Ruleset. Select and
# fingerprint the exact marked Rule, never the Ruleset container ID.
CF_PENDING_MARKER=""
cf_set_pending_write(){ CF_PENDING_MARKER="$6"; }
cf_new_ownership_marker(){ printf '%s\n' 11111111111111111111111111111111; }
origin_ref="ai_server_agent_$(printf '%s' mcp.example.com | sha256sum | cut -c1-16)"
ssl_ref="ai_server_agent_ssl_$(printf '%s' mcp.example.com | sha256sum | cut -c1-16)"
cf_get_phase_entrypoint(){ case "$2:$TEST_MODE" in http_request_origin:create) jq -nc '{id:"origin-set",kind:"zone",phase:"http_request_origin",rules:[]}' ;; http_config_settings:create) jq -nc '{id:"ssl-set",kind:"zone",phase:"http_config_settings",rules:[]}' ;; *) return 2 ;; esac; }
cf_api(){
  local method="$1" path="$2" body="${3:-}" rule
  case "$TEST_MODE:$method:$path" in
    create:POST:/zones/zone1/rulesets/origin-set/rules) rule="$(jq -nc --argjson body "$body" '$body+{id:"origin-new"}')"; jq -nc --argjson rule "$rule" '{success:true,result:{id:"origin-set",rules:[$rule]}}' ;;
    create:GET:/zones/zone1/rulesets/origin-set) rule="$(jq -nc --arg ref "$origin_ref" '{id:"origin-new",ref:$ref,description:"AI Server Agent origin port txn:11111111111111111111111111111111",expression:"http.host eq \"mcp.example.com\"",action:"route",action_parameters:{origin:{port:3210}},enabled:true}')"; jq -nc --argjson rule "$rule" '{success:true,result:{id:"origin-set",rules:[$rule]}}' ;;
    create:POST:/zones/zone1/rulesets/ssl-set/rules) rule="$(jq -nc --argjson body "$body" '$body+{id:"ssl-new"}')"; jq -nc --argjson rule "$rule" '{success:true,result:{id:"ssl-set",rules:[$rule]}}' ;;
    create:GET:/zones/zone1/rulesets/ssl-set) rule="$(jq -nc --arg ref "$ssl_ref" '{id:"ssl-new",ref:$ref,description:"AI Server Agent strict SSL txn:11111111111111111111111111111111",expression:"http.host eq \"mcp.example.com\"",action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"; jq -nc --argjson rule "$rule" '{success:true,result:{id:"ssl-set",rules:[$rule]}}' ;;
    *) fail "unexpected create-response API call: $TEST_MODE $method $path" ;;
  esac
}
TEST_MODE=create
cf_reconcile_origin_rule zone1 mcp.example.com 3210 '' '' '' >/dev/null
[ "$CF_RESULT_ORIGIN_RULESET_ID" = origin-set ] && [ "$CF_RESULT_ORIGIN_RULE_ID" = origin-new ] && [ "$CF_RESULT_ORIGIN_ACTION" = created ] && [ -n "$CF_RESULT_ORIGIN_FINGERPRINT" ] || fail 'Origin updated-Ruleset response was parsed as a Rule'
CF_PENDING_MARKER=""
cf_reconcile_ssl_config_rule zone1 mcp.example.com '' '' '' >/dev/null
[ "$CF_RESULT_SSL_RULESET_ID" = ssl-set ] && [ "$CF_RESULT_SSL_RULE_ID" = ssl-new ] && [ "$CF_RESULT_SSL_ACTION" = created ] && [ -n "$CF_RESULT_SSL_FINGERPRINT" ] || fail 'Configuration updated-Ruleset response was parsed as a Rule'

# External semantic equivalents are preserved and block duplicate creation.
# A recorded Agent-owned Rule remains authoritative on rerun.
manual_origin="$(jq -nc '{id:"manual-origin",ref:"manual-origin",description:"manual origin",expression:"(http.host eq \"mcp.example.com\")",action:"route",action_parameters:{origin:{port:3210}},enabled:true}')"
owned_origin="$(jq -nc --arg ref "$origin_ref" '{id:"owned-origin",ref:$ref,description:"AI Server Agent origin port txn:22222222222222222222222222222222",expression:"http.host eq \"mcp.example.com\"",action:"route",action_parameters:{origin:{port:3210}},enabled:true}')"
manual_ssl="$(jq -nc '{id:"manual-ssl",ref:"manual-ssl",description:"manual ssl",expression:"(http.host eq \"mcp.example.com\")",action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
owned_ssl="$(jq -nc --arg ref "$ssl_ref" '{id:"owned-ssl",ref:$ref,description:"AI Server Agent strict SSL txn:33333333333333333333333333333333",expression:"http.host eq \"mcp.example.com\"",action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
MUTATION_LOG="$TMP/rule-contract-mutations.log"
cf_get_phase_entrypoint(){ case "$2:$TEST_MODE" in http_request_origin:external) jq -nc --argjson r "$manual_origin" '{id:"origin-set",kind:"zone",phase:"http_request_origin",rules:[$r]}' ;; http_request_origin:owned) jq -nc --argjson a "$manual_origin" --argjson b "$owned_origin" '{id:"origin-set",kind:"zone",phase:"http_request_origin",rules:[$a,$b]}' ;; http_config_settings:external) jq -nc --argjson r "$manual_ssl" '{id:"ssl-set",kind:"zone",phase:"http_config_settings",rules:[$r]}' ;; http_config_settings:owned) jq -nc --argjson a "$manual_ssl" --argjson b "$owned_ssl" '{id:"ssl-set",kind:"zone",phase:"http_config_settings",rules:[$a,$b]}' ;; *) return 2 ;; esac; }
cf_set_pending_write(){ printf 'pending %s\n' "$*" >> "$MUTATION_LOG"; return 99; }
cf_api(){ local method="$1" path="$2"; if [ "$TEST_MODE" = owned ] && [ "$method" = GET ]; then case "$path" in /zones/zone1/rulesets/origin-set) jq -nc --argjson a "$manual_origin" --argjson b "$owned_origin" '{success:true,result:{rules:[$a,$b]}}'; return ;; /zones/zone1/rulesets/ssl-set) jq -nc --argjson a "$manual_ssl" --argjson b "$owned_ssl" '{success:true,result:{rules:[$a,$b]}}'; return ;; esac; fi; printf '%s %s\n' "$method" "$path" >> "$MUTATION_LOG"; return 99; }
TEST_MODE=external; : > "$MUTATION_LOG"; CF_PENDING_MARKER=""; set +e; out="$(cf_reconcile_origin_rule zone1 mcp.example.com 3210 '' '' '' 2>&1)"; rc=$?; set -e; [ "$rc" -ne 0 ] && grep -Fq 'equivalent external Origin Rule(s)' <<<"$out" && test ! -s "$MUTATION_LOG" || fail 'external Origin equivalent did not fail closed before mutation'
TEST_MODE=external; : > "$MUTATION_LOG"; CF_PENDING_MARKER=""; set +e; out="$(cf_reconcile_ssl_config_rule zone1 mcp.example.com '' '' '' 2>&1)"; rc=$?; set -e; [ "$rc" -ne 0 ] && grep -Fq 'equivalent external strict SSL Configuration Rule(s)' <<<"$out" && test ! -s "$MUTATION_LOG" || fail 'external Configuration equivalent did not fail closed before mutation'
TEST_MODE=owned; : > "$MUTATION_LOG"; fp="$(cf_rule_fingerprint <<<"$owned_origin")"; out="$(cf_reconcile_origin_rule zone1 mcp.example.com 3210 origin-set owned-origin "$fp" 2>&1)"; grep -Fq 'found 2 equivalent Origin Rules' <<<"$out" && test ! -s "$MUTATION_LOG" || fail 'owned Origin rerun mutated or hid external equivalent'
TEST_MODE=owned; : > "$MUTATION_LOG"; fp="$(cf_rule_fingerprint <<<"$owned_ssl")"; out="$(cf_reconcile_ssl_config_rule zone1 mcp.example.com ssl-set owned-ssl "$fp" 2>&1)"; grep -Fq 'found 2 equivalent strict SSL Configuration Rules' <<<"$out" && test ! -s "$MUTATION_LOG" || fail 'owned Configuration rerun mutated or hid external equivalent'
'''
p.write_text(s.replace(end,block+'\n'+end))
ci=Path('.github/workflows/ci.yml'); c=ci.read_text(); old='  RELEASE_VERSION: v0.1.4\n'
if c.count(old)!=1: raise SystemExit('CI release target drift')
ci.write_text(c.replace(old,'  RELEASE_VERSION: v0.1.5\n',1))
