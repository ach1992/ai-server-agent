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

for bad in \
  '{"success":true,"result":{"id":"","kind":"zone","phase":"http_request_origin","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"root","phase":"http_request_origin","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_config_settings","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":{}}}'; do
  curl(){ printf '%s\n200' "$bad"; }
  if cf_get_phase_entrypoint zone1 http_request_origin >/dev/null 2>&1; then
    fail "malformed phase entrypoint was accepted: $bad"
  fi
done

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

printf 'Cloudflare Rulesets phase-entrypoint discovery contract passed.\n'
