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

# Production callers must use the canonical bare Rulesets collection path.
# cf_api owns decision-complete cursor traversal and never sends per_page=100.
if grep -Fq 'rulesets?per_page=100' "$ROOT/manage.sh"; then
  fail 'legacy invalid Rulesets sentinel remains in production code'
fi

curl(){
  local url="${*: -1}"
  printf '%s\n' "$url" >> "$CALL_LOG"
  case "$url" in
    "$CF_API/zones/zone1/rulesets?per_page=50")
      printf '%s' '{"success":true,"result":[{"id":"first-page","kind":"zone","phase":"http_request_transform"}],"result_info":{"cursors":{"after":"cursor +/="}}}'
      ;;
    "$CF_API/zones/zone1/rulesets?per_page=50&cursor=cursor%20%2B%2F%3D")
      printf '%s' '{"success":true,"result":[{"id":"origin-page-two","kind":"zone","phase":"http_request_origin"}],"result_info":{"cursors":{}}}'
      ;;
    *)
      echo "unexpected curl URL: $url" >&2
      return 22
      ;;
  esac
}

out="$(cf_api GET '/zones/zone1/rulesets')"
[ "$(jq '.result | length' <<<"$out")" -eq 2 ] || fail 'ruleset pages were not merged'
[ "$(jq -r '.result[] | select(.phase=="http_request_origin") | .id' <<<"$out")" = origin-page-two ] || fail 'second-page phase was not discoverable'
grep -Fxq "$CF_API/zones/zone1/rulesets?per_page=50" "$CALL_LOG" || fail 'first page did not use per_page=50'
grep -Fxq "$CF_API/zones/zone1/rulesets?per_page=50&cursor=cursor%20%2B%2F%3D" "$CALL_LOG" || fail 'opaque cursor was not URL-encoded'
if grep -Fq 'per_page=100' "$CALL_LOG"; then fail 'invalid per_page=100 was sent to Cloudflare'; fi

# Cloudflare documents result_info and cursors as optional. Its own
# List Zone Rulesets example is a successful response without result_info.
curl(){ printf '%s' '{"success":true,"result":[{"id":"doc-shaped","kind":"zone","phase":"http_request_origin"}],"errors":[],"messages":[]}'; }
out="$(cf_api GET '/zones/zone1/rulesets')" || fail 'documented response without result_info was rejected'
[ "$(jq -r '.result[0].id' <<<"$out")" = doc-shaped ] || fail 'documented response without result_info was not preserved'

curl(){ printf '%s' '{"success":true,"result":[],"result_info":{}}'; }
cf_api GET '/zones/zone1/rulesets' >/dev/null || fail 'optional missing cursors object was rejected'
curl(){ printf '%s' '{"success":true,"result":[],"result_info":{"cursors":{}}}'; }
cf_api GET '/zones/zone1/rulesets' >/dev/null || fail 'empty cursor metadata was rejected'

# Optional metadata must still be structurally valid when present.
for bad_info in 'null' 'false' '123' '"bad"' '[]'; do
  curl(){ printf '{"success":true,"result":[],"result_info":%s}' "$bad_info"; }
  if cf_api GET '/zones/zone1/rulesets' >/dev/null 2>&1; then
    fail "invalid result_info was accepted: $bad_info"
  fi
done
for bad_cursors in 'null' 'false' '123' '"bad"' '[]'; do
  curl(){ printf '{"success":true,"result":[],"result_info":{"cursors":%s}}' "$bad_cursors"; }
  if cf_api GET '/zones/zone1/rulesets' >/dev/null 2>&1; then
    fail "invalid cursors metadata was accepted: $bad_cursors"
  fi
done

# If after is present, it must be a non-empty string.
for bad_after in 'false' 'null' '""' '123' '{}' '[]'; do
  curl(){ printf '{"success":true,"result":[],"result_info":{"cursors":{"after":%s}}}' "$bad_after"; }
  if cf_api GET '/zones/zone1/rulesets' >/dev/null 2>&1; then
    fail "invalid after cursor was accepted: $bad_after"
  fi
done

# Ruleset entries used for phase-absence decisions remain fail-closed.
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
  if cf_api GET '/zones/zone1/rulesets' >/dev/null 2>&1; then
    fail "malformed ruleset item was accepted: $bad_result"
  fi
done

for valid_kind in managed custom root zone; do
  curl(){ printf '{"success":true,"result":[{"id":"r1","kind":"%s","phase":"http_request_origin"}]}' "$valid_kind"; }
  cf_api GET '/zones/zone1/rulesets' >/dev/null || fail "documented ruleset kind was rejected: $valid_kind"
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
  cf_api GET '/zones/zone1/rulesets' >/dev/null || fail "documented ruleset phase was rejected: $valid_phase"
done

# A non-advancing cursor must fail closed rather than loop or return partial data.
curl(){ printf '%s' '{"success":true,"result":[],"result_info":{"cursors":{"after":"same-cursor"}}}'; }
if cf_api GET '/zones/zone1/rulesets' >/dev/null 2>&1; then
  fail 'repeated pagination cursor was accepted'
fi

printf 'Cloudflare Rulesets pagination contract passed.\n'
