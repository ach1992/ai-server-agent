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

# Rulesets discovery must use Cloudflare's supported maximum page size, follow
# the opaque cursor, URL-encode it, and merge every page before callers decide
# that a phase-specific ruleset is absent.
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

out="$(cf_api GET '/zones/zone1/rulesets?per_page=100')"
[ "$(jq '.result | length' <<<"$out")" -eq 2 ] || fail 'ruleset pages were not merged'
[ "$(jq -r '.result[] | select(.phase=="http_request_origin") | .id' <<<"$out")" = origin-page-two ] || fail 'second-page phase was not discoverable'
grep -Fxq "$CF_API/zones/zone1/rulesets?per_page=50" "$CALL_LOG" || fail 'first page did not use per_page=50'
grep -Fxq "$CF_API/zones/zone1/rulesets?per_page=50&cursor=cursor%20%2B%2F%3D" "$CALL_LOG" || fail 'opaque cursor was not URL-encoded'
if grep -Fq 'per_page=100' "$CALL_LOG"; then fail 'invalid per_page=100 was sent to Cloudflare'; fi

# If Cloudflare omits pagination metadata, completeness is unknown. Fail closed
# rather than treating a potentially truncated page as proof of absence.
curl(){ printf '%s' '{"success":true,"result":[]}'; }
if cf_api GET '/zones/zone1/rulesets?per_page=100' >/dev/null 2>&1; then
  fail 'missing result_info was accepted as complete discovery'
fi
curl(){ printf '%s' '{"success":true,"result":[],"result_info":{}}'; }
if cf_api GET '/zones/zone1/rulesets?per_page=100' >/dev/null 2>&1; then
  fail 'missing cursor metadata was accepted as complete discovery'
fi

# An after cursor is optional only by absence. If present, it must be a
# non-empty string; malformed values must not be mistaken for completion.
for bad_after in 'false' 'null' '""' '123' '{}' '[]'; do
  curl(){ printf '{"success":true,"result":[],"result_info":{"cursors":{"after":%s}}}' "$bad_after"; }
  if cf_api GET '/zones/zone1/rulesets?per_page=100' >/dev/null 2>&1; then
    fail "invalid after cursor was accepted: $bad_after"
  fi
done

# Ruleset entries used to decide phase absence must contain valid identity and
# classification fields. Missing, mistyped, empty, or unknown enum values fail
# closed instead of being treated as proof that a phase-specific ruleset is absent.
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
  curl(){ printf '{"success":true,"result":%s,"result_info":{"cursors":{}}}' "$bad_result"; }
  if cf_api GET '/zones/zone1/rulesets?per_page=100' >/dev/null 2>&1; then
    fail "malformed ruleset item was accepted: $bad_result"
  fi
done

# Keep the validator aligned with Cloudflare's documented RulesetKind and
# RulesetPhase enums so tightening malformed-response handling does not reject
# documented values.
for valid_kind in managed custom root zone; do
  curl(){ printf '{"success":true,"result":[{"id":"r1","kind":"%s","phase":"http_request_origin"}],"result_info":{"cursors":{}}}' "$valid_kind"; }
  cf_api GET '/zones/zone1/rulesets?per_page=100' >/dev/null || fail "documented ruleset kind was rejected: $valid_kind"
done
for valid_phase in \
  ddos_l4 ddos_l7 http_config_settings http_custom_errors http_log_custom_fields \
  http_ratelimit http_request_cache_settings http_request_dynamic_redirect \
  http_request_firewall_custom http_request_firewall_managed http_request_late_transform \
  http_request_origin http_request_redirect http_request_sanitize http_request_sbfm \
  http_request_transform http_response_cache_settings http_response_compression \
  http_response_firewall_managed http_response_headers_transform magic_transit \
  magic_transit_ids_managed magic_transit_managed magic_transit_ratelimit; do
  curl(){ printf '{"success":true,"result":[{"id":"r1","kind":"zone","phase":"%s"}],"result_info":{"cursors":{}}}' "$valid_phase"; }
  cf_api GET '/zones/zone1/rulesets?per_page=100' >/dev/null || fail "documented ruleset phase was rejected: $valid_phase"
done

# A non-advancing cursor must also fail closed instead of looping or returning
# a partial discovery result.
curl(){
  printf '%s' '{"success":true,"result":[],"result_info":{"cursors":{"after":"same-cursor"}}}'
}
if cf_api GET '/zones/zone1/rulesets?per_page=100' >/dev/null 2>&1; then
  fail 'repeated pagination cursor was accepted'
fi

printf 'Cloudflare Rulesets pagination contract passed.\n'
