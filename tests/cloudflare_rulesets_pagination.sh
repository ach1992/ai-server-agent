#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
# shellcheck source=../manage.sh
source "$ROOT/manage.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
CALL_LOG="$TMP/calls.log"
fail(){ echo "FAIL: $*" >&2; exit 1; }

MODE=origin
cf_get_optional(){
  printf '%s\n' "$1" >> "$CALL_LOG"
  case "$MODE" in
    origin)
      [ "$1" = '/zones/zone1/rulesets/phases/http_request_origin/entrypoint' ] || return 2
      printf '%s' '{"success":true,"result":{"id":"origin-set","kind":"zone","phase":"http_request_origin","rules":[]}}'
      ;;
    config)
      [ "$1" = '/zones/zone1/rulesets/phases/http_config_settings/entrypoint' ] || return 2
      printf '%s' '{"success":true,"result":{"id":"config-set","kind":"zone","phase":"http_config_settings","rules":[]}}'
      ;;
    absent) return 3 ;;
    provider_error) return 2 ;;
    malformed) printf '%s' "$MALFORMED_RESPONSE" ;;
    marked)
      printf '%s' '{"success":true,"result":{"id":"origin-set","kind":"zone","phase":"http_request_origin","rules":[{"id":"owned-rule","ref":"ai_server_agent_test","description":"AI Server Agent origin port txn:abcdef0123456789abcdef0123456789"}]}}'
      ;;
    ambiguous)
      printf '%s' '{"success":true,"result":{"id":"origin-set","kind":"zone","phase":"http_request_origin","rules":[{"id":"external-rule","ref":"ai_server_agent_test","description":"AI Server Agent origin port"}]}}'
      ;;
    *) return 2 ;;
  esac
}

# The exact phase endpoint returns the zone entry-point ruleset directly; broad
# Rulesets listing and pagination are not part of this discovery path.
out="$(cf_get_zone_phase_ruleset zone1 http_request_origin)"
[ "$(jq -r '.result.id' <<<"$out")" = origin-set ] || fail 'origin entrypoint was not returned'
grep -Fxq '/zones/zone1/rulesets/phases/http_request_origin/entrypoint' "$CALL_LOG" || fail 'origin phase entrypoint path was not used'

MODE=config
out="$(cf_get_zone_phase_ruleset zone1 http_config_settings)"
[ "$(jq -r '.result.id' <<<"$out")" = config-set ] || fail 'configuration entrypoint was not returned'
grep -Fxq '/zones/zone1/rulesets/phases/http_config_settings/entrypoint' "$CALL_LOG" || fail 'configuration phase entrypoint path was not used'

# Cloudflare defines 404 as absence for a phase entrypoint. Preserve that as a
# distinct result so callers may create only after authoritative absence.
MODE=absent
set +e
cf_get_zone_phase_ruleset zone1 http_request_origin >/dev/null
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "entrypoint absence returned $rc instead of 3"

# Provider/transport errors and unsupported phases are not absence.
MODE=provider_error
set +e
cf_get_zone_phase_ruleset zone1 http_request_origin >/dev/null
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "provider error returned $rc instead of 2"

before="$(wc -l < "$CALL_LOG")"
set +e
cf_get_zone_phase_ruleset zone1 http_request_transform >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "unsupported phase returned $rc instead of 2"
after="$(wc -l < "$CALL_LOG")"
[ "$after" -eq "$before" ] || fail 'unsupported phase reached Cloudflare'

# Existing entrypoints must be structurally consistent with the requested zone
# phase before their rules can drive ownership or absence decisions.
for malformed in \
  '{"success":true,"result":null}' \
  '{"success":true,"result":[]}' \
  '{"success":true,"result":{"kind":"zone","phase":"http_request_origin","rules":[]}}' \
  '{"success":true,"result":{"id":"","kind":"zone","phase":"http_request_origin","rules":[]}}' \
  '{"success":true,"result":{"id":123,"kind":"zone","phase":"http_request_origin","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"custom","phase":"http_request_origin","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_config_settings","rules":[]}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin"}}' \
  '{"success":true,"result":{"id":"r1","kind":"zone","phase":"http_request_origin","rules":{}}}'; do
  MODE=malformed MALFORMED_RESPONSE="$malformed"
  set +e
  cf_get_zone_phase_ruleset zone1 http_request_origin >/dev/null 2>&1
  rc=$?
  set -e
  [ "$rc" -eq 2 ] || fail "malformed entrypoint returned $rc: $malformed"
done

# Response-lost recovery still requires the exact transaction marker. The phase
# entrypoint gives one authoritative ruleset, so no list scan is needed.
MODE=marked
found="$(cf_find_rule_by_marker zone1 http_request_origin ai_server_agent_test abcdef0123456789abcdef0123456789)"
[ "$found" = 'origin-set|owned-rule' ] || fail 'marked rule was not recovered from phase entrypoint'

MODE=ambiguous
set +e
cf_find_rule_by_marker zone1 http_request_origin ai_server_agent_test abcdef0123456789abcdef0123456789 >/dev/null
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "same ref without ownership marker returned $rc instead of ambiguous"

# Guard the architecture: this flow must never regress to broad Rulesets list
# pagination, which caused the v0.1.2/v0.1.3 live failures.
if grep -Fq 'cf_list_zone_rulesets' "$ROOT/manage.sh"; then
  fail 'broad Rulesets list helper remains in manage.sh'
fi
if grep -Fq '/rulesets?per_page=' "$ROOT/manage.sh"; then
  fail 'broad Rulesets list request remains in manage.sh'
fi
grep -Fq '/rulesets/phases/$phase/entrypoint' "$ROOT/manage.sh" || fail 'phase entrypoint endpoint is missing'
[ "$(grep -Fc 'cf_get_zone_phase_ruleset "$zone_id"' "$ROOT/manage.sh")" -eq 3 ] || fail 'not all Rulesets discovery paths use the phase helper'

printf 'Cloudflare Rulesets phase-entrypoint discovery contract passed.\n'
