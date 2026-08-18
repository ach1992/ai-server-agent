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

# Cursor traversal must use per_page=50, URL-encode the opaque cursor, and
# merge all pages before a phase-absence decision. The final short page omits
# optional result_info to reproduce the live Cloudflare response shape.
curl(){
  local url="${*: -1}"
  printf '%s\n' "$url" >> "$CALL_LOG"
  case "$url" in
    "$CF_API/zones/zone1/rulesets?per_page=50")
      printf '%s' '{"success":true,"result":[{"id":"first-page","kind":"zone","phase":"http_request_transform"}],"result_info":{"cursors":{"after":"cursor +/="}}}'
      ;;
    "$CF_API/zones/zone1/rulesets?per_page=50&cursor=cursor%20%2B%2F%3D")
      printf '%s' '{"success":true,"result":[{"id":"origin-page-two","kind":"zone","phase":"http_request_origin"}]}'
      ;;
    *)
      echo "unexpected curl URL: $url" >&2
      return 22
      ;;
  esac
}

out="$(cf_list_zone_rulesets zone1)"
[ "$(jq '.result | length' <<<"$out")" -eq 2 ] || fail 'ruleset pages were not merged'
[ "$(jq -r '.result[] | select(.phase=="http_request_origin") | .id' <<<"$out")" = origin-page-two ] || fail 'second-page phase was not discoverable'
grep -Fxq "$CF_API/zones/zone1/rulesets?per_page=50" "$CALL_LOG" || fail 'first page did not use per_page=50'
grep -Fxq "$CF_API/zones/zone1/rulesets?per_page=50&cursor=cursor%20%2B%2F%3D" "$CALL_LOG" || fail 'opaque cursor was not URL-encoded'
if grep -Fq 'per_page=100' "$CALL_LOG"; then fail 'invalid per_page=100 was sent to Cloudflare'; fi

# Cloudflare documents result_info and cursors as optional. Short responses with
# no pagination metadata are valid terminal pages.
curl(){ printf '%s' '{"success":true,"result":[]}'; }
cf_list_zone_rulesets zone1 >/dev/null || fail 'short response without result_info was rejected'
curl(){ printf '%s' '{"success":true,"result":[],"result_info":{}}'; }
cf_list_zone_rulesets zone1 >/dev/null || fail 'short response without cursors was rejected'
curl(){ printf '%s' '{"success":true,"result":[],"result_info":{"cursors":{}}}'; }
cf_list_zone_rulesets zone1 >/dev/null || fail 'explicit terminal cursor object was rejected'

# A full page without pagination metadata is ambiguous. Fail closed rather than
# treating it as proof that a phase-specific ruleset is absent.
full_page="$(jq -cn '[range(0;50) | {id:("r"+tostring),kind:"zone",phase:"http_request_transform"}]')"
curl(){ printf '{"success":true,"result":%s}' "$full_page"; }
if cf_list_zone_rulesets zone1 >/dev/null 2>&1; then
  fail 'full page without pagination metadata was accepted as complete'
fi

# If an after cursor is present, it must be a non-empty string.
for bad_after in 'false' 'null' '""' '123' '{}' '[]'; do
  curl(){ printf '{"success":true,"result":[],"result_info":{"cursors":{"after":%s}}}' "$bad_after"; }
  if cf_list_zone_rulesets zone1 >/dev/null 2>&1; then
    fail "invalid after cursor was accepted: $bad_after"
  fi
done

# Entries used for a phase-absence decision must contain valid identity and
# documented kind/phase values.
for bad_result in \
  '[null]' \
  '[{"kind":"zone","phase":"http_request_origin"}]' \
  '[{"id":"r1","phase":"http_request_origin"}]' \
  '[{"id":"r1","kind":"zone"}]' \
  '[{"id":"","kind":"zone","phase":"http_request_origin"}]' \
  '[{"id":"r1","kind":"","phase":"http_request_origin"}]' \
  '[{"id":"r1","kind":"zone","phase":""}]' \
  '[{"id":1,"kind":"zone","phase":"http_request_origin"}]' \
  '[{"id":"r1","kind":1,"phase":"http_request_origin"}]' \
  '[{"id":"r1","kind":"zone","phase":1}]' \
  '[{"id":"r1","kind":"bogus","phase":"http_request_origin"}]' \
  '[{"id":"r1","kind":"zone","phase":"bogus"}]'; do
  curl(){ printf '{"success":true,"result":%s}' "$bad_result"; }
  if cf_list_zone_rulesets zone1 >/dev/null 2>&1; then
    fail "malformed ruleset item was accepted: $bad_result"
  fi
done

for valid_kind in managed custom root zone; do
  curl(){ printf '{"success":true,"result":[{"id":"r1","kind":"%s","phase":"http_request_origin"}]}' "$valid_kind"; }
  cf_list_zone_rulesets zone1 >/dev/null || fail "documented ruleset kind was rejected: $valid_kind"
done
for valid_phase in \
  ddos_l4 ddos_l7 http_config_settings http_custom_errors http_log_custom_fields \
  http_ratelimit http_request_cache_settings http_request_dynamic_redirect \
  http_request_firewall_custom http_request_firewall_managed http_request_late_transform \
  http_request_origin http_request_redirect http_request_sanitize http_request_sbfm \
  http_request_transform http_response_cache_settings http_response_compression \
  http_response_firewall_managed http_response_headers_transform magic_transit \
  magic_transit_ids_managed magic_transit_managed magic_transit_ratelimit; do
  curl(){ printf '{"success":true,"result":[{"id":"r1","kind":"zone","phase":"%s"}]}' "$valid_phase"; }
  cf_list_zone_rulesets zone1 >/dev/null || fail "documented ruleset phase was rejected: $valid_phase"
done

# A non-advancing cursor must fail rather than loop or return partial discovery.
curl(){ printf '%s' '{"success":true,"result":[],"result_info":{"cursors":{"after":"same-cursor"}}}'; }
if cf_list_zone_rulesets zone1 >/dev/null 2>&1; then
  fail 'repeated pagination cursor was accepted'
fi

# The production callers must all use the dedicated abstraction; no sentinel
# request may remain in manage.sh.
if grep -Fq 'rulesets?per_page=100' "$ROOT/manage.sh"; then
  fail 'legacy Rulesets per_page=100 sentinel remains in manage.sh'
fi
[ "$(grep -Fc 'cf_list_zone_rulesets "$zone_id"' "$ROOT/manage.sh")" -eq 3 ] || fail 'not all Rulesets discovery callers use the dedicated helper'

printf 'Cloudflare Rulesets discovery contract passed.\n'
