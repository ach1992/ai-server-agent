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

# A non-advancing cursor must also fail closed instead of looping or returning
# a partial discovery result.
curl(){
  printf '%s' '{"success":true,"result":[],"result_info":{"cursors":{"after":"same-cursor"}}}'
}
if cf_api GET '/zones/zone1/rulesets?per_page=100' >/dev/null 2>&1; then
  fail 'repeated pagination cursor was accepted'
fi

printf 'Cloudflare Rulesets pagination contract passed.\n'
