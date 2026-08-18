#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


def replace_function(text: str, name: str, next_name: str, replacement: str) -> str:
    start = text.find(f"{name}(){{\n")
    end = text.find(f"\n\n{next_name}(){{", start)
    if start < 0 or end < 0:
        raise SystemExit(f"could not locate function boundary for {name}")
    return text[:start] + replacement.rstrip("\n") + text[end:]


manage_path = Path("manage.sh")
manage = manage_path.read_text()

manage = replace_once(
    manage,
    '''          (.rules|type)=="array" and\n          all(.rules[];\n''',
    '''          (.rules|type)=="array" and\n          (([.rules[].id] | length) == ([.rules[].id] | unique | length)) and\n          all(.rules[];\n''',
    "phase entrypoint rule-ID uniqueness",
)

manage = replace_function(
    manage,
    "cf_get_rule",
    "cf_get_origin_cert",
    r'''cf_get_rule(){
  local zone_id="$1" ruleset_id="$2" rule_id="$3" res rc count
  if res="$(cf_get_optional "/zones/$zone_id/rulesets/$ruleset_id")"; then
    :
  else
    rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
  fi
  if ! jq -e '
    (.result|type)=="object" and
    (.result.rules|type)=="array" and
    all(.result.rules[]; type=="object" and (.id|type)=="string" and (.id|length)>0) and
    (([.result.rules[].id] | length) == ([.result.rules[].id] | unique | length))
  ' >/dev/null 2>&1 <<<"$res"; then
    return 2
  fi
  count="$(jq --arg id "$rule_id" '[.result.rules[] | select(.id==$id)] | length' <<<"$res")"
  [ "$count" -eq 0 ] && return 3
  [ "$count" -eq 1 ] || return 2
  jq -c --arg id "$rule_id" '.result.rules[] | select(.id==$id)' <<<"$res"
}''',
)

manage = replace_function(
    manage,
    "cf_find_rule_by_marker",
    "cf_recover_pending_write",
    r'''cf_find_rule_by_marker(){
  local zone_id="$1" phase="$2" ref="$3" marker="$4" entry rc ruleset_id marker_ids ref_ids id marker_count=0 ref_count=0 found=""
  if entry="$(cf_get_phase_entrypoint "$zone_id" "$phase")"; then
    ruleset_id="$(jq -r '.id' <<<"$entry")"
  else
    rc=$?
    [ "$rc" -eq 3 ] && return 1
    return 2
  fi
  marker_ids="$(jq -r --arg marker "$marker" '.rules[] | select(((.description // "") | endswith(" txn:"+$marker))) | .id' <<<"$entry")"
  ref_ids="$(jq -r --arg ref "$ref" '.rules[] | select((.ref // "")==$ref) | .id' <<<"$entry")"
  while IFS= read -r id; do [ -n "$id" ] || continue; found="$ruleset_id|$id"; marker_count=$((marker_count + 1)); done <<<"$marker_ids"
  while IFS= read -r id; do [ -n "$id" ] || continue; ref_count=$((ref_count + 1)); done <<<"$ref_ids"
  [ "$marker_count" -eq 1 ] && { printf '%s\n' "$found"; return 0; }
  [ "$marker_count" -eq 0 ] || return 2
  [ "$ref_count" -eq 0 ] && return 1
  return 2
}''',
)

old_origin_local = '  local phase_entry rc ruleset_id rule_ref ref_match rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint\n'
new_origin_local = '  local phase_entry rc ruleset_id rule_ref ref_match id_count rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint\n'
# The same local declaration appears in Origin and Configuration reconciliation.
if manage.count(old_origin_local) != 2:
    raise SystemExit(f"reconcile locals: expected two matches, found {manage.count(old_origin_local)}")
manage = manage.replace(old_origin_local, new_origin_local, 2)

manage = replace_once(
    manage,
    '''    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then\n      rule="$(jq -c --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref)' <<<"$ruleset" | head -n1)"\n''',
    '''    if [ -n "$owned_rule" ]; then\n      [ "$owned_ruleset" = "$ruleset_id" ] || die "The recorded Agent-owned Origin Rule ruleset no longer matches the current phase entrypoint. Refusing to create or adopt a replacement automatically."\n      id_count="$(jq --arg id "$owned_rule" '[.result.rules[] | select(.id==$id)] | length' <<<"$ruleset")"\n      [ "$id_count" -le 1 ] || die "Cloudflare returned duplicate entries for the recorded Agent-owned Origin Rule ID. Refusing ambiguous reconciliation."\n      rule="$(jq -c --arg id "$owned_rule" '.result.rules[] | select(.id==$id)' <<<"$ruleset")"\n''',
    "origin recorded-ID reconciliation",
)

manage = replace_once(
    manage,
    '''    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then\n      rule="$(jq -c --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref)' <<<"$ruleset" | head -n1)"\n''',
    '''    if [ -n "$owned_rule" ]; then\n      [ "$owned_ruleset" = "$ruleset_id" ] || die "The recorded Agent-owned Configuration Rule ruleset no longer matches the current phase entrypoint. Refusing to create or adopt a replacement automatically."\n      id_count="$(jq --arg id "$owned_rule" '[.result.rules[] | select(.id==$id)] | length' <<<"$ruleset")"\n      [ "$id_count" -le 1 ] || die "Cloudflare returned duplicate entries for the recorded Agent-owned Configuration Rule ID. Refusing ambiguous reconciliation."\n      rule="$(jq -c --arg id "$owned_rule" '.result.rules[] | select(.id==$id)' <<<"$ruleset")"\n''',
    "configuration recorded-ID reconciliation",
)

manage_path.write_text(manage)

rules_path = Path("tests/cloudflare_rulesets_pagination.sh")
rules = rules_path.read_text()
rules = replace_once(
    rules,
    'fail(){ echo "FAIL: $*" >&2; exit 1; }\n',
    'fail(){ echo "FAIL: $*" >&2; exit 1; }\nORIG_CF_GET_OPTIONAL="$(declare -f cf_get_optional)"\n',
    "save cf_get_optional in ruleset contract test",
)

rules_anchor = '''curl(){ printf '%s\\n403' '{"success":false,"errors":[{"code":9109,"message":"permission denied"}]}' ; }\n'''
rules_insert = r'''# Exact Ruleset reads used by cleanup/recovery must distinguish malformed
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

'''
rules = replace_once(rules, rules_anchor, rules_insert + rules_anchor, "exact Ruleset read regressions")
rules_path.write_text(rules)

txn_path = Path("tests/cloudflare_transaction.sh")
txn = txn_path.read_text()
txn = replace_once(
    txn,
    'ORIG_CF_FIND_RULE_BY_MARKER="$(declare -f cf_find_rule_by_marker)"\nORIG_CF_RECONCILE_SSL_CONFIG_RULE="$(declare -f cf_reconcile_ssl_config_rule)"\n',
    'ORIG_CF_FIND_RULE_BY_MARKER="$(declare -f cf_find_rule_by_marker)"\nORIG_CF_GET_PHASE_ENTRYPOINT="$(declare -f cf_get_phase_entrypoint)"\nORIG_CF_RECONCILE_ORIGIN_RULE="$(declare -f cf_reconcile_origin_rule)"\nORIG_CF_RECONCILE_SSL_CONFIG_RULE="$(declare -f cf_reconcile_ssl_config_rule)"\n',
    "save production rule helpers",
)

restore_anchor = '''eval "$ORIG_CF_GET_ORIGIN_CERT"\n\n# Durable pre-POST journal contains the exact semantic fingerprint before POST.\n'''
restore_insert = r'''eval "$ORIG_CF_GET_ORIGIN_CERT"

# Marker discovery is independent of mutable Rule ref. If the exact marked Rule
# survives but its ref drifts, recovery must find it, fail the full intent
# fingerprint check, and preserve the pending journal without deleting anything.
marker_drift_rule="$(jq -nc '{id:"marker-drift-rule",ref:"external-ref",description:"AI Server Agent origin port txn:abcdef0123456789abcdef0123456789",expression:"http.host eq \\"mcp.example.com\\"",action:"route",action_parameters:{origin:{port:3210}},enabled:true}')"
CF_PENDING_KIND=origin-rule-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=ai_server_agent_test; CF_PENDING_PHASE=http_request_origin; CF_PENDING_MARKER=abcdef0123456789abcdef0123456789; CF_PENDING_FINGERPRINT="$pending_rule_fp"
cf_get_phase_entrypoint(){ jq -nc --argjson rule "$marker_drift_rule" '{id:"shared-set",kind:"zone",phase:"http_request_origin",rules:[$rule]}' ; }
cf_get_optional(){
  case "$1" in
    '/zones/zone1/rulesets/shared-set') jq -nc --argjson rule "$marker_drift_rule" '{success:true,result:{rules:[$rule]}}' ;;
    *) return 2 ;;
  esac
}
: > "$DELETE_LOG"
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
if cf_recover_pending_write; then echo 'marker-only drift was treated as pending-rule absence' >&2; exit 1; fi
test ! -s "$DELETE_LOG"
test "$CF_PENDING_KIND" = origin-rule-create
cf_clear_pending_write
eval "$ORIG_CF_GET_OPTIONAL"
eval "$ORIG_CF_GET_PHASE_ENTRYPOINT"

# Durable pre-POST journal contains the exact semantic fingerprint before POST.
'''
txn = replace_once(txn, restore_anchor, restore_insert, "pending marker-drift regression")

reconcile_anchor = '''eval "$ORIG_CF_GET_OPTIONAL"\neval "$ORIG_CF_RECONCILE_SSL_CONFIG_RULE"\n\n# Same-host canonical owned SSL rule is a no-op.\n'''
reconcile_insert = r'''eval "$ORIG_CF_GET_OPTIONAL"
eval "$ORIG_CF_RECONCILE_ORIGIN_RULE"
eval "$ORIG_CF_RECONCILE_SSL_CONFIG_RULE"

# A recorded Rule ID is ownership evidence independent of its mutable ref. If
# that ID still exists with a changed/missing ref, reconciliation must fail
# before any POST rather than manufacturing a replacement Rule.
MUTATION_LOG="$TMP/rule-mutations.log"
origin_ref="ai_server_agent_$(printf '%s' mcp.example.com | sha256sum | cut -c1-16)"
origin_drift_rule="$(jq -nc '{id:"origin-rule-owned",ref:"external-ref",description:"AI Server Agent origin port",expression:"http.host eq \\"mcp.example.com\\"",action:"route",action_parameters:{origin:{port:3210}},enabled:true}')"
cf_get_phase_entrypoint(){ jq -nc --argjson rule "$origin_drift_rule" '{id:"origin-set-owned",kind:"zone",phase:"http_request_origin",rules:[$rule]}' ; }
cf_api(){ printf '%s %s\n' "$1" "$2" >> "$MUTATION_LOG"; return 2; }
: > "$MUTATION_LOG"
if ( cf_reconcile_origin_rule zone1 mcp.example.com 3210 origin-set-owned origin-rule-owned '' ) >/dev/null 2>&1; then echo 'Origin Rule ref drift was accepted' >&2; exit 1; fi
test ! -s "$MUTATION_LOG" || { echo 'Origin Rule ref drift attempted a remote mutation' >&2; exit 1; }

ssl_ref="ai_server_agent_ssl_$(printf '%s' mcp.example.com | sha256sum | cut -c1-16)"
ssl_drift_rule="$(jq -nc '{id:"ssl-rule-owned",ref:null,description:"AI Server Agent strict SSL",expression:"http.host eq \\"mcp.example.com\\"",action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
cf_get_phase_entrypoint(){ jq -nc --argjson rule "$ssl_drift_rule" '{id:"ssl-set-owned",kind:"zone",phase:"http_config_settings",rules:[$rule]}' ; }
: > "$MUTATION_LOG"
if ( cf_reconcile_ssl_config_rule zone1 mcp.example.com ssl-set-owned ssl-rule-owned '' ) >/dev/null 2>&1; then echo 'Configuration Rule ref drift was accepted' >&2; exit 1; fi
test ! -s "$MUTATION_LOG" || { echo 'Configuration Rule ref drift attempted a remote mutation' >&2; exit 1; }

# Same-host canonical owned SSL rule is a no-op.
'''
txn = replace_once(txn, reconcile_anchor, reconcile_insert, "recorded-ID ref-drift regressions")

txn_path.write_text(txn)
