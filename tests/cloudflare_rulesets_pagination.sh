#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
# shellcheck source=../manage.sh
source "$ROOT/manage.sh"

CF_TOKEN=test
CF_API="https://api.cloudflare.com/client/v4"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
CALL_LOG="$TMP/calls.log"
fail(){ echo "FAIL: $*" >&2; exit 1; }
ORIG_CF_GET_OPTIONAL="$(declare -f cf_get_optional)"

# Historical filename retained for CI compatibility. Production discovery now
# uses exact phase entrypoints and must not depend on zone-wide list pagination.
if grep -Fq 'cf_list_zone_rulesets' "$ROOT/manage.sh" || grep -Fq '/rulesets?per_page=' "$ROOT/manage.sh"; then
  fail 'production code still depends on zone-wide Rulesets pagination'
fi

curl(){
  local url="${*: -1}"
  printf '%s\n' "$url" >> "$CALL_LOG"
  case "$url" in
    "$CF_API/zones/zone1/rulesets/phases/http_request_origin/entrypoint")
      printf '%s\n200' '{"success":true,"result":{"id":"origin-set","kind":"zone","phase":"http_request_origin","rules":[{"id":"r1","ref":"origin-ref"}]}}' ;;
    "$CF_API/zones/zone1/rulesets/phases/http_config_settings/entrypoint")
      printf '%s\n404' '{"success":false,"errors":[{"code":10000,"message":"entrypoint not found"}]}' ;;
    *) echo "unexpected curl URL: $url" >&2; return 22 ;;
  esac
}

origin="$(cf_get_phase_entrypoint zone1 http_request_origin)" || fail 'existing Origin Rules entrypoint was not discovered'
[ "$(jq -r '.id' <<<"$origin")" = origin-set ] || fail 'wrong Origin Rules entrypoint ID'
[ "$(jq -r '.phase' <<<"$origin")" = http_request_origin ] || fail 'wrong Origin Rules phase'
grep -Fxq "$CF_API/zones/zone1/rulesets/phases/http_request_origin/entrypoint" "$CALL_LOG" || fail 'exact Origin Rules phase endpoint was not used'

set +e
cf_get_phase_entrypoint zone1 http_config_settings >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "missing Configuration Rules entrypoint did not return absence status: $rc"

before="$(wc -l < "$CALL_LOG")"
set +e
cf_get_phase_entrypoint zone1 http_request_snippets >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail 'unsupported internal phase input was not rejected'
[ "$(wc -l < "$CALL_LOG")" -eq "$before" ] || fail 'unsupported phase input reached Cloudflare'

# Entrypoint identity and the Rules used for ownership/absence decisions must be
# structurally safe. A malformed Rule must never be silently treated as absent.
for bad in \
  '{"success":true,"result":{"id":"","kind":"zone","phase":"http_request_origin","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"root","phase":"http_request_origin","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_config_settings","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":{}}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":[null]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":[{"ref":"x"}]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":[{"id":""}]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":[{"id":123}]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":[{"id":"rule1","ref":""}]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":[{"id":"rule1","ref":123}]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":[{"id":"rule1","description":123}]}}'; do
  curl(){ printf '%s\n200' "$bad"; }
  if cf_get_phase_entrypoint zone1 http_request_origin >/dev/null 2>&1; then
    fail "malformed phase entrypoint was accepted: $bad"
  fi
done

# Null optional ref/description are tolerated by the transport schema; if
# present as strings, ref must be non-empty and description may be empty.
curl(){ printf '%s\n200' '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":[{"id":"rule1","ref":null,"description":null},{"id":"rule2","description":""}]}}'; }
cf_get_phase_entrypoint zone1 http_request_origin >/dev/null || fail 'valid optional rule fields were rejected'

# Exact Ruleset reads used by cleanup/recovery must distinguish malformed
# successful responses from a valid Ruleset in which the target Rule is absent.
for malformed in \
  '{"success":true,"result":null}' \
  '{"success":true,"result":{}}' \
  '{"success":true,"result":{"rules":null}}' \
  '{"success":true,"result":{"rules":{}}}' \
  '{"success":true,"result":{"rules":[null]}}' \
  '{"success":true,"result":{"rules":[{"id":null}]}}' \
  '{"success":true,"result":{"rules":[{"id":"target"},{"id":"target"}]}}'; do
  cf_get_optional(){ printf '%s' "$malformed"; }
  set +e
  cf_get_rule zone1 ruleset1 target >/dev/null 2>&1
  rc=$?
  set -e
  [ "$rc" -eq 2 ] || fail "malformed exact Ruleset response became absence: $malformed (rc=$rc)"
done
cf_get_optional(){ printf '%s' '{"success":true,"result":{"rules":[]}}'; }
set +e
cf_get_rule zone1 ruleset1 target >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "valid empty Ruleset did not report target absence: $rc"
cf_get_optional(){ printf '%s' '{"success":true,"result":{"rules":[{"id":"target","ref":null}]}}'; }
[ "$(jq -r '.id' <<<"$(cf_get_rule zone1 ruleset1 target)")" = target ] || fail 'valid exact Rule lookup failed'
eval "$ORIG_CF_GET_OPTIONAL"

curl(){ printf '%s\n403' '{"success":false,"errors":[{"code":9109,"message":"permission denied"}]}' ; }
set +e
err="$(cf_get_phase_entrypoint zone1 http_request_origin 2>&1 >/dev/null)"
rc=$?
set -e
[ "$rc" -eq 2 ] || fail 'non-404 phase lookup failure was treated as absence'
grep -Fq 'Cloudflare API error 9109: permission denied' <<<"$err" || fail 'structured Cloudflare phase lookup error was hidden'
if grep -Fq 'Bearer test' <<<"$err"; then fail 'Cloudflare token leaked into diagnostics'; fi

# Optional-read wrappers must preserve the first-class 404/absence status.
cf_get_optional(){ return 3; }
for helper in dns rule cert; do
  set +e
  case "$helper" in
    dns) cf_get_dns_record zone1 missing >/dev/null 2>&1 ;;
    rule) cf_get_rule zone1 ruleset1 missing >/dev/null 2>&1 ;;
    cert) cf_get_origin_cert missing >/dev/null 2>&1 ;;
  esac
  rc=$?
  set -e
  [ "$rc" -eq 3 ] || fail "$helper optional-read wrapper lost 404 absence status: $rc"
done


# Existing-ruleset POST /rules returns the updated Ruleset. Select and
# fingerprint the exact marked Rule, never the Ruleset container ID or a
# pre-existing/distractor Rule from the returned Ruleset.
CF_PENDING_MARKER=""
cf_set_pending_write(){ CF_PENDING_MARKER="$6"; }
cf_new_ownership_marker(){ printf '%s\n' 11111111111111111111111111111111; }
origin_ref="ai_server_agent_$(printf '%s' mcp.example.com | sha256sum | cut -c1-16)"
ssl_ref="ai_server_agent_ssl_$(printf '%s' mcp.example.com | sha256sum | cut -c1-16)"
expected_origin="$(jq -nc --arg ref "$origin_ref" '{id:"origin-new",ref:$ref,description:"AI Server Agent origin port txn:11111111111111111111111111111111",expression:"http.host eq \"mcp.example.com\"",action:"route",action_parameters:{origin:{port:3210}},enabled:true}')"
expected_ssl="$(jq -nc --arg ref "$ssl_ref" '{id:"ssl-new",ref:$ref,description:"AI Server Agent strict SSL txn:11111111111111111111111111111111",expression:"http.host eq \"mcp.example.com\"",action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
expected_origin_fp="$(cf_rule_fingerprint <<<"$expected_origin")"
expected_ssl_fp="$(cf_rule_fingerprint <<<"$expected_ssl")"
origin_before="$(jq -nc '{id:"origin-before",ref:"manual-before",description:"existing origin before",expression:"http.host eq \"before.example.com\"",action:"route",action_parameters:{origin:{port:443}},enabled:true}')"
origin_after="$(jq -nc '{id:"origin-after",ref:"manual-after",description:"existing origin after",expression:"http.host eq \"after.example.com\"",action:"route",action_parameters:{origin:{port:8443}},enabled:true}')"
ssl_before="$(jq -nc '{id:"ssl-before",ref:"manual-ssl-before",description:"existing ssl before",expression:"http.host eq \"before.example.com\"",action:"set_config",action_parameters:{ssl:"flexible"},enabled:true}')"
ssl_after="$(jq -nc '{id:"ssl-after",ref:"manual-ssl-after",description:"existing ssl after",expression:"http.host eq \"after.example.com\"",action:"set_config",action_parameters:{ssl:"full"},enabled:true}')"
cf_get_phase_entrypoint(){
  case "$2:$TEST_MODE" in
    http_request_origin:create|http_request_origin:ambiguous-origin) jq -nc '{id:"origin-set",kind:"zone",phase:"http_request_origin",rules:[]}' ;;
    http_config_settings:create|http_config_settings:ambiguous-ssl) jq -nc '{id:"ssl-set",kind:"zone",phase:"http_config_settings",rules:[]}' ;;
    *) return 2 ;;
  esac
}
cf_api(){
  local method="$1" path="$2" body="${3:-}" rule duplicate
  case "$TEST_MODE:$method:$path" in
    create:POST:/zones/zone1/rulesets/origin-set/rules)
      rule="$(jq -nc --argjson body "$body" '$body+{id:"origin-new"}')"
      jq -nc --argjson before "$origin_before" --argjson rule "$rule" --argjson after "$origin_after" '{success:true,result:{id:"origin-set",rules:[$before,$rule,$after]}}'
      ;;
    create:GET:/zones/zone1/rulesets/origin-set)
      jq -nc --argjson before "$origin_before" --argjson rule "$expected_origin" --argjson after "$origin_after" '{success:true,result:{id:"origin-set",rules:[$before,$rule,$after]}}'
      ;;
    create:POST:/zones/zone1/rulesets/ssl-set/rules)
      rule="$(jq -nc --argjson body "$body" '$body+{id:"ssl-new"}')"
      jq -nc --argjson before "$ssl_before" --argjson rule "$rule" --argjson after "$ssl_after" '{success:true,result:{id:"ssl-set",rules:[$before,$rule,$after]}}'
      ;;
    create:GET:/zones/zone1/rulesets/ssl-set)
      jq -nc --argjson before "$ssl_before" --argjson rule "$expected_ssl" --argjson after "$ssl_after" '{success:true,result:{id:"ssl-set",rules:[$before,$rule,$after]}}'
      ;;
    ambiguous-origin:POST:/zones/zone1/rulesets/origin-set/rules)
      rule="$(jq -nc --argjson body "$body" '$body+{id:"origin-a"}')"
      duplicate="$(jq -nc --argjson body "$body" '$body+{id:"origin-b"}')"
      jq -nc --argjson a "$rule" --argjson b "$duplicate" '{success:true,result:{id:"origin-set",rules:[$a,$b]}}'
      ;;
    ambiguous-ssl:POST:/zones/zone1/rulesets/ssl-set/rules)
      rule="$(jq -nc --argjson body "$body" '$body+{id:"ssl-a"}')"
      duplicate="$(jq -nc --argjson body "$body" '$body+{id:"ssl-b"}')"
      jq -nc --argjson a "$rule" --argjson b "$duplicate" '{success:true,result:{id:"ssl-set",rules:[$a,$b]}}'
      ;;
    *) fail "unexpected create-response API call: $TEST_MODE $method $path" ;;
  esac
}
TEST_MODE=create
cf_reconcile_origin_rule zone1 mcp.example.com 3210 '' '' '' >/dev/null
[ "$CF_RESULT_ORIGIN_RULESET_ID" = origin-set ] && [ "$CF_RESULT_ORIGIN_RULE_ID" = origin-new ] && [ "$CF_RESULT_ORIGIN_ACTION" = created ] && [ "$CF_RESULT_ORIGIN_FINGERPRINT" = "$expected_origin_fp" ] || fail 'Origin updated-Ruleset response did not select/fingerprint the exact created Rule'
CF_PENDING_MARKER=""
cf_reconcile_ssl_config_rule zone1 mcp.example.com '' '' '' >/dev/null
[ "$CF_RESULT_SSL_RULESET_ID" = ssl-set ] && [ "$CF_RESULT_SSL_RULE_ID" = ssl-new ] && [ "$CF_RESULT_SSL_ACTION" = created ] && [ "$CF_RESULT_SSL_FINGERPRINT" = "$expected_ssl_fp" ] || fail 'Configuration updated-Ruleset response did not select/fingerprint the exact created Rule'

TEST_MODE=ambiguous-origin; CF_PENDING_MARKER=""; set +e
out="$(cf_reconcile_origin_rule zone1 mcp.example.com 3210 '' '' '' 2>&1)"; rc=$?
set -e
[ "$rc" -ne 0 ] && grep -Fq 'invalid or ambiguous created Origin Rule' <<<"$out" || fail 'ambiguous Origin create response was accepted'

TEST_MODE=ambiguous-ssl; CF_PENDING_MARKER=""; set +e
out="$(cf_reconcile_ssl_config_rule zone1 mcp.example.com '' '' '' 2>&1)"; rc=$?
set -e
[ "$rc" -ne 0 ] && grep -Fq 'invalid or ambiguous created Configuration Rule' <<<"$out" || fail 'ambiguous Configuration create response was accepted'

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



# Stale recorded ownership plus an external equivalent must fail before
# any create/journal mutation instead of recreating a duplicate.
TEST_MODE=external; : > "$MUTATION_LOG"; CF_PENDING_MARKER=""
set +e; out="$(cf_reconcile_origin_rule zone1 mcp.example.com 3210 origin-set missing-origin '' 2>&1)"; rc=$?; set -e
[ "$rc" -ne 0 ] && grep -Fq 'recorded Agent-owned Origin Rule is absent' <<<"$out" && test ! -s "$MUTATION_LOG" || fail 'stale Origin ownership recreated around an external equivalent'
TEST_MODE=external; : > "$MUTATION_LOG"; CF_PENDING_MARKER=""
set +e; out="$(cf_reconcile_ssl_config_rule zone1 mcp.example.com ssl-set missing-ssl '' 2>&1)"; rc=$?; set -e
[ "$rc" -ne 0 ] && grep -Fq 'recorded Agent-owned Configuration Rule is absent' <<<"$out" && test ! -s "$MUTATION_LOG" || fail 'stale Configuration ownership recreated around an external equivalent'

# A create response with the right marker/ref but mutated semantics is
# not confirmation of the durable pre-POST intent.
cf_get_phase_entrypoint(){ case "$2" in http_request_origin) jq -nc '{id:"origin-set",kind:"zone",phase:"http_request_origin",rules:[]}' ;; *) return 2 ;; esac; }
cf_set_pending_write(){ CF_PENDING_MARKER="$6"; }
cf_new_ownership_marker(){ printf '%s\n' 44444444444444444444444444444444; }
cf_api(){
  local method="$1" path="$2" body="${3:-}" rule
  case "$method:$path" in
    POST:/zones/zone1/rulesets/origin-set/rules)
      rule="$(jq -nc --argjson body "$body" '$body + {id:"drifted-origin"} | .action_parameters.origin.port=9999')"
      jq -nc --argjson rule "$rule" '{success:true,result:{id:"origin-set",rules:[$rule]}}'
      ;;
    *) fail "unexpected intent-drift API call: $method $path" ;;
  esac
}
CF_PENDING_MARKER=""
set +e; out="$(cf_reconcile_origin_rule zone1 mcp.example.com 3210 '' '' '' 2>&1)"; rc=$?; set -e
[ "$rc" -ne 0 ] && grep -Fq 'does not match the requested transaction intent' <<<"$out" || fail 'semantic drift in a create response was accepted as confirmation'

printf 'Cloudflare Rulesets phase-entrypoint discovery contract passed.\n'
