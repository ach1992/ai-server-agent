#!/usr/bin/env python3
from pathlib import Path

manage = Path("manage.sh")
text = manage.read_text()

legacy_local = "  local ruleset_base cursor=\"\" encoded_cursor page_path page_out next_cursor pages=0 merged='[]'\n"
if text.count(legacy_local) != 1:
    raise SystemExit(f"expected one legacy cf_api Rulesets local line, found {text.count(legacy_local)}")
text = text.replace(legacy_local, "", 1)

marker = "  # The Rulesets list endpoint is cursor-paginated and currently caps per_page at 50.\n"
start = text.find(marker)
if start < 0:
    raise SystemExit("legacy Rulesets sentinel block not found")
end_marker = '  if [ -n "$body" ]; then\n'
end = text.find(end_marker, start)
if end < 0:
    raise SystemExit("generic cf_api request block not found")
text = text[:start] + text[end:]

old_body = '''  if [ -n "$body" ]; then
    out="$(curl -sS --fail-with-body "${retry_args[@]}" --request "$method" --config "$cfg" -H 'Content-Type: application/json' --data-binary "$body" "$CF_API$path")" || { rm -f "$cfg"; return 1; }
  else
    out="$(curl -sS --fail-with-body "${retry_args[@]}" --request "$method" --config "$cfg" -H 'Content-Type: application/json' "$CF_API$path")" || { rm -f "$cfg"; return 1; }
  fi
  rm -f "$cfg"
  if ! jq -e '.success == true' >/dev/null 2>&1 <<<"$out"; then
    jq -r '.errors[]?.message // empty' <<<"$out" >&2 || true
    return 1
  fi
  printf '%s' "$out"
}
'''
new_body = '''  if [ -n "$body" ]; then
    if ! out="$(curl -sS --fail-with-body "${retry_args[@]}" --request "$method" --config "$cfg" -H 'Content-Type: application/json' --data-binary "$body" "$CF_API$path")"; then
      jq -r '.errors[]? | if (.code // null) == null then (.message // empty) else ((.code|tostring) + ": " + (.message // "")) end' <<<"$out" >&2 2>/dev/null || true
      rm -f "$cfg"; return 1
    fi
  else
    if ! out="$(curl -sS --fail-with-body "${retry_args[@]}" --request "$method" --config "$cfg" -H 'Content-Type: application/json' "$CF_API$path")"; then
      jq -r '.errors[]? | if (.code // null) == null then (.message // empty) else ((.code|tostring) + ": " + (.message // "")) end' <<<"$out" >&2 2>/dev/null || true
      rm -f "$cfg"; return 1
    fi
  fi
  rm -f "$cfg"
  if ! jq -e '.success == true' >/dev/null 2>&1 <<<"$out"; then
    jq -r '.errors[]? | if (.code // null) == null then (.message // empty) else ((.code|tostring) + ": " + (.message // "")) end' <<<"$out" >&2 || true
    return 1
  fi
  printf '%s' "$out"
}
'''
if text.count(old_body) != 1:
    raise SystemExit("generic cf_api request body did not match expected v0.1.3 source")
text = text.replace(old_body, new_body, 1)

anchor = "\n\ncf_delete_owned(){\n"
pos = text.find(anchor, text.find("cf_api(){"))
if pos < 0:
    raise SystemExit("cf_api end anchor not found")

helper = r'''

cf_list_zone_rulesets(){
  local zone_id="$1" cursor="" encoded_cursor path page next_cursor count pages=0 merged='[]'
  while :; do
    pages=$((pages + 1))
    [ "$pages" -le 1000 ] || { warn "Cloudflare Rulesets pagination exceeded the safety bound."; return 1; }
    if [ -n "$cursor" ]; then
      encoded_cursor="$(jq -rn --arg cursor "$cursor" '$cursor|@uri')"
      path="/zones/$zone_id/rulesets?per_page=50&cursor=$encoded_cursor"
    else
      path="/zones/$zone_id/rulesets?per_page=50"
    fi

    page="$(cf_api GET "$path")" || return 1
    if ! jq -e '
      def valid_kind: .=="managed" or .=="custom" or .=="root" or .=="zone";
      def valid_phase:
        .=="ddos_l4" or .=="ddos_l7" or .=="http_config_settings" or
        .=="http_custom_errors" or .=="http_log_custom_fields" or .=="http_ratelimit" or
        .=="http_request_cache_settings" or .=="http_request_dynamic_redirect" or
        .=="http_request_firewall_custom" or .=="http_request_firewall_managed" or
        .=="http_request_late_transform" or .=="http_request_origin" or
        .=="http_request_redirect" or .=="http_request_sanitize" or .=="http_request_sbfm" or
        .=="http_request_transform" or .=="http_response_cache_settings" or
        .=="http_response_compression" or .=="http_response_firewall_managed" or
        .=="http_response_headers_transform" or .=="magic_transit" or
        .=="magic_transit_ids_managed" or .=="magic_transit_managed" or
        .=="magic_transit_ratelimit";
      .success == true and
      (.result|type)=="array" and
      all(.result[];
        type=="object" and
        (.id|type)=="string" and (.id|length)>0 and
        (.kind|type)=="string" and (.kind|valid_kind) and
        (.phase|type)=="string" and (.phase|valid_phase)
      ) and
      (
        (has("result_info")|not) or
        (
          (.result_info|type)=="object" and
          (
            (.result_info|has("cursors")|not) or
            (
              (.result_info.cursors|type)=="object" and
              (
                (.result_info.cursors|has("after")|not) or
                ((.result_info.cursors.after|type)=="string" and (.result_info.cursors.after|length)>0)
              )
            )
          )
        )
      )
    ' >/dev/null 2>&1 <<<"$page"; then
      warn "Cloudflare Rulesets list response did not match the documented response contract."
      return 1
    fi

    merged="$(jq -cn --argjson acc "$merged" --argjson response "$page" '$acc + $response.result')"
    count="$(jq '.result | length' <<<"$page")"

    if jq -e '(.result_info? | type)=="object" and (.result_info.cursors? | type)=="object" and (.result_info.cursors | has("after"))' >/dev/null 2>&1 <<<"$page"; then
      next_cursor="$(jq -r '.result_info.cursors.after' <<<"$page")"
      [ "$next_cursor" != "$cursor" ] || { warn "Cloudflare Rulesets pagination returned a non-advancing cursor."; return 1; }
      cursor="$next_cursor"
      continue
    fi

    if jq -e '(.result_info? | type)=="object" and (.result_info.cursors? | type)=="object"' >/dev/null 2>&1 <<<"$page"; then
      break
    fi

    # Cloudflare documents result_info/cursors as optional. With no cursor
    # metadata, a short page is complete at the requested 50-item page size.
    # A full page is ambiguous and must not authorize an absence decision.
    [ "$count" -lt 50 ] || { warn "Cloudflare Rulesets returned a full page without pagination metadata; refusing an incomplete absence decision."; return 1; }
    break
  done

  jq -cn --argjson result "$merged" '{success:true,result:$result}'
}
'''
text = text[:pos] + helper + text[pos:]

legacy_call = 'cf_api GET "/zones/$zone_id/rulesets?per_page=100"'
if text.count(legacy_call) != 3:
    raise SystemExit(f"expected three legacy Rulesets call sites, found {text.count(legacy_call)}")
text = text.replace(legacy_call, 'cf_list_zone_rulesets "$zone_id"')
if "rulesets?per_page=100" in text:
    raise SystemExit("legacy Rulesets per_page=100 sentinel remains")
if text.count('cf_list_zone_rulesets "$zone_id"') != 3:
    raise SystemExit("not all Rulesets callers use the dedicated helper")
manage.write_text(text)

ci = Path(".github/workflows/ci.yml")
ci_text = ci.read_text()
if ci_text.count("RELEASE_VERSION: v0.1.3") != 1:
    raise SystemExit("expected exactly one v0.1.3 release target in CI")
ci.write_text(ci_text.replace("RELEASE_VERSION: v0.1.3", "RELEASE_VERSION: v0.1.4", 1))
