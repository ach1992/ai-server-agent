#!/usr/bin/env python3
from pathlib import Path

manage = Path("manage.sh")
text = manage.read_text()

# Remove the broad list/pagination abstraction entirely. This product only
# needs the zone entry point for two known phases.
start = text.find("\ncf_list_zone_rulesets(){\n")
end = text.find("\n\ncf_delete_owned(){", start)
if start < 0 or end < 0:
    raise SystemExit("dedicated Rulesets list helper was not found")
text = text[:start] + text[end:]

# Make optional GETs preserve provider diagnostics, then add one narrow helper
# for the two phase entry points AI Server Agent manages.
start = text.find("cf_get_optional(){\n")
end = text.find("\n\ncf_dns_fingerprint(){", start)
if start < 0 or end < 0:
    raise SystemExit("cf_get_optional function boundary was not found")
replacement = r'''cf_get_optional(){
  local path="$1" cfg response status body
  cfg="$(mktemp)"; chmod 0600 "$cfg"
  printf 'header = "Authorization: Bearer %s"\n' "$CF_TOKEN" > "$cfg"
  response="$(curl -sS --retry 2 --request GET --config "$cfg" -H 'Content-Type: application/json' -w '\n%{http_code}' "$CF_API$path")" || { rm -f "$cfg"; return 2; }
  rm -f "$cfg"
  status="${response##*$'\n'}"; body="${response%$'\n'*}"
  [ "$status" = "404" ] && return 3
  if [[ ! "$status" =~ ^2[0-9][0-9]$ ]]; then
    jq -r '.errors[]? | if (.code // null) == null then (.message // empty) else ((.code|tostring) + ": " + (.message // "")) end' <<<"$body" >&2 2>/dev/null || true
    return 2
  fi
  if ! jq -e '.success == true' >/dev/null 2>&1 <<<"$body"; then
    jq -r '.errors[]? | if (.code // null) == null then (.message // empty) else ((.code|tostring) + ": " + (.message // "")) end' <<<"$body" >&2 2>/dev/null || true
    return 2
  fi
  printf '%s' "$body"
}

cf_get_zone_phase_ruleset(){
  local zone_id="$1" phase="$2" res rc
  case "$phase" in
    http_request_origin|http_config_settings) ;;
    *) warn "Unsupported Cloudflare phase lookup requested: $phase"; return 2 ;;
  esac

  if res="$(cf_get_optional "/zones/$zone_id/rulesets/phases/$phase/entrypoint")"; then
    if ! jq -e --arg phase "$phase" '
      .success == true and
      (.result|type)=="object" and
      (.result.id|type)=="string" and (.result.id|length)>0 and
      .result.kind=="zone" and
      .result.phase==$phase and
      (.result.rules|type)=="array"
    ' >/dev/null 2>&1 <<<"$res"; then
      warn "Cloudflare phase entrypoint response did not match the expected zone Ruleset contract."
      return 2
    fi
    printf '%s' "$res"
    return 0
  else
    rc=$?
    [ "$rc" -eq 3 ] && return 3
    return 2
  fi
}
'''
text = text[:start] + replacement + text[end:]

# Recovery discovery needs only the one entry point for the recorded phase.
start = text.find("cf_find_rule_by_marker(){\n")
end = text.find("\n\ncf_recover_pending_write(){", start)
if start < 0 or end < 0:
    raise SystemExit("cf_find_rule_by_marker function boundary was not found")
replacement = r'''cf_find_rule_by_marker(){
  local zone_id="$1" phase="$2" ref="$3" marker="$4" ruleset rc ruleset_id exact_ids ref_ids id exact_count=0 ref_count=0 found=""
  if ruleset="$(cf_get_zone_phase_ruleset "$zone_id" "$phase")"; then
    :
  else
    rc=$?
    [ "$rc" -eq 3 ] && return 1
    return 2
  fi
  ruleset_id="$(jq -r '.result.id' <<<"$ruleset")"
  exact_ids="$(jq -r --arg ref "$ref" --arg marker "$marker" '.result.rules[]? | select((.ref // "")==$ref and ((.description // "") | endswith(" txn:"+$marker))) | .id // empty' <<<"$ruleset")"
  ref_ids="$(jq -r --arg ref "$ref" '.result.rules[]? | select((.ref // "")==$ref) | .id // empty' <<<"$ruleset")"
  while IFS= read -r id; do [ -n "$id" ] || continue; found="$ruleset_id|$id"; exact_count=$((exact_count + 1)); done <<<"$exact_ids"
  while IFS= read -r id; do [ -n "$id" ] || continue; ref_count=$((ref_count + 1)); done <<<"$ref_ids"
  [ "$exact_count" -eq 1 ] && { printf '%s\n' "$found"; return 0; }
  [ "$exact_count" -eq 0 ] || return 2
  [ "$ref_count" -eq 0 ] && return 1
  return 2
}
'''
text = text[:start] + replacement + text[end:]

# Origin Rules: discover the exact phase entry point. 404 means it does not yet
# exist; every other lookup problem fails closed.
origin_start = text.find("cf_reconcile_origin_rule(){\n")
origin_end = text.find("\n\ncf_reconcile_ssl_config_rule(){", origin_start)
if origin_start < 0 or origin_end < 0:
    raise SystemExit("origin reconcile function boundary was not found")
origin = text[origin_start:origin_end]
old = '  local list ruleset_id rule_ref ref_match rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint\n'
new = '  local ruleset_id rule_ref ref_match rule_body create_body res ruleset rc marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint\n'
if origin.count(old) != 1:
    raise SystemExit("origin reconcile locals did not match")
origin = origin.replace(old, new, 1)
old = '''  list="$(cf_list_zone_rulesets "$zone_id")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_request_origin") | .id' <<<"$list" | head -n1)"
'''
new = '''  if ruleset="$(cf_get_zone_phase_ruleset "$zone_id" http_request_origin)"; then
    ruleset_id="$(jq -r '.result.id' <<<"$ruleset")"
  else
    rc=$?
    [ "$rc" -eq 3 ] || die "Cloudflare Origin Rules entrypoint lookup failed."
    ruleset_id=""
  fi
'''
if origin.count(old) != 1:
    raise SystemExit("origin list discovery did not match")
origin = origin.replace(old, new, 1)
old = '    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Origin Rules ruleset."\n'
if origin.count(old) != 1:
    raise SystemExit("origin redundant ruleset reread did not match")
origin = origin.replace(old, '', 1)
text = text[:origin_start] + origin + text[origin_end:]

# Configuration Rules: same exact phase-entrypoint strategy.
ssl_start = text.find("cf_reconcile_ssl_config_rule(){\n")
ssl_end = text.find("\n\nverify_public(){", ssl_start)
if ssl_start < 0 or ssl_end < 0:
    raise SystemExit("SSL reconcile function boundary was not found")
ssl = text[ssl_start:ssl_end]
old = '  local list ruleset_id rule_ref ref_match rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint\n'
new = '  local ruleset_id rule_ref ref_match rule_body create_body res ruleset rc marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint\n'
if ssl.count(old) != 1:
    raise SystemExit("SSL reconcile locals did not match")
ssl = ssl.replace(old, new, 1)
old = '''  list="$(cf_list_zone_rulesets "$zone_id")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_config_settings") | .id' <<<"$list" | head -n1)"
'''
new = '''  if ruleset="$(cf_get_zone_phase_ruleset "$zone_id" http_config_settings)"; then
    ruleset_id="$(jq -r '.result.id' <<<"$ruleset")"
  else
    rc=$?
    [ "$rc" -eq 3 ] || die "Cloudflare Configuration Rules entrypoint lookup failed."
    ruleset_id=""
  fi
'''
if ssl.count(old) != 1:
    raise SystemExit("SSL list discovery did not match")
ssl = ssl.replace(old, new, 1)
old = '    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Configuration Rules ruleset."\n'
if ssl.count(old) != 1:
    raise SystemExit("SSL redundant ruleset reread did not match")
ssl = ssl.replace(old, '', 1)
text = text[:ssl_start] + ssl + text[ssl_end:]

if "cf_list_zone_rulesets" in text:
    raise SystemExit("broad Rulesets list helper remains")
if "/rulesets?per_page=" in text:
    raise SystemExit("broad Rulesets list request remains")
if text.count('cf_get_zone_phase_ruleset "$zone_id"') != 3:
    raise SystemExit("expected recovery, Origin Rules, and Configuration Rules to use phase entrypoints")
manage.write_text(text)

# Align the two transaction fixtures that exercise rule discovery/reconciliation.
txn = Path("tests/cloudflare_transaction.sh")
t = txn.read_text()
old = '''# A deterministic rule ref without our nonce is likewise ambiguous.
cf_api(){
  case "$2" in
    '/zones/zone1/rulesets?per_page=50') printf '%s' '{"success":true,"result":[{"id":"origin-set-race","kind":"zone","phase":"http_request_origin"}]}' ;;
    '/zones/zone1/rulesets/origin-set-race') printf '%s' '{"success":true,"result":{"rules":[{"id":"external-origin","ref":"ai_server_agent_test","description":"AI Server Agent origin port"}]}}' ;;
    *) return 2 ;;
  esac
}
'''
new = '''# A deterministic rule ref without our nonce is likewise ambiguous.
cf_get_optional(){
  case "$1" in
    '/zones/zone1/rulesets/phases/http_request_origin/entrypoint')
      printf '%s' '{"success":true,"result":{"id":"origin-set-race","kind":"zone","phase":"http_request_origin","rules":[{"id":"external-origin","ref":"ai_server_agent_test","description":"AI Server Agent origin port"}]}}'
      ;;
    *) return 2 ;;
  esac
}
'''
if t.count(old) != 1:
    raise SystemExit("origin transaction fixture did not match")
t = t.replace(old, new, 1)
old = '''cf_api(){
  case "$2" in
    '/zones/zone1/rulesets?per_page=50') printf '%s' '{"success":true,"result":[{"id":"ssl-set-owned","kind":"zone","phase":"http_config_settings"}]}' ;;
    '/zones/zone1/rulesets/ssl-set-owned') jq -nc --argjson rule "$ssl_rule_json" '{success:true,result:{rules:[$rule]}}' ;;
    *) return 2 ;;
  esac
}
'''
new = '''cf_get_optional(){
  case "$1" in
    '/zones/zone1/rulesets/phases/http_config_settings/entrypoint')
      jq -nc --argjson rule "$ssl_rule_json" '{success:true,result:{id:"ssl-set-owned",kind:"zone",phase:"http_config_settings",rules:[$rule]}}'
      ;;
    *) return 2 ;;
  esac
}
cf_api(){
  case "$2" in
    '/zones/zone1/rulesets/ssl-set-owned') jq -nc --argjson rule "$ssl_rule_json" '{success:true,result:{id:"ssl-set-owned",kind:"zone",phase:"http_config_settings",rules:[$rule]}}' ;;
    *) return 2 ;;
  esac
}
'''
if t.count(old) != 1:
    raise SystemExit("SSL transaction fixture did not match")
t = t.replace(old, new, 1)
if "/rulesets?per_page=" in t:
    raise SystemExit("legacy Rulesets list fixture remains in transaction tests")
txn.write_text(t)
