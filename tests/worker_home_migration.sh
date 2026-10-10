#!/usr/bin/env bash
set -Eeuo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
# Extract the exact production function without sourcing/executing the
# privileged installer or modifying real passwd/services.
body="$(awk '/^migrate_existing_worker_login_home\(\) \{/ { within=1 } within {print} within && /^}/ {exit}' "$root/install.sh")"
[[ "$body" == migrate_existing_worker_login_home* && "$body" == *'usermod --home'* ]] || { echo 'production worker HOME guard not found'; exit 1; }
eval "$body"

LEGACY=/srv/ai-workspace
NEW=/var/lib/ai-server-agent/worker-home

test_case() {
  local name="$1" mode="$2" after="$3" original="$4" expect_rc="$5" expect_calls="$6" expect_warnings="$7"
  local calls=0 warnings=0 rc=0
  usermod(){ calls=$((calls+1)); return "$mode"; }
  getent(){ [[ "$1" == passwd && "$2" == aiworker ]] || return 2; printf 'aiworker:x:987:988::%s:/bin/bash\n' "$after"; }
  warn(){ warnings=$((warnings+1)); }
  die(){ return 86; }
  if migrate_existing_worker_login_home aiworker "$LEGACY" "$NEW" "$original"; then rc=0; else rc=$?; fi
  if [[ "$rc" != "$expect_rc" || "$calls" != "$expect_calls" || "$warnings" != "$expect_warnings" ]]; then
    echo "FAIL $name: rc=$rc calls=$calls warnings=$warnings expected_rc=$expect_rc expected_calls=$expect_calls expected_warnings=$expect_warnings"
    exit 1
  fi
  echo "PASS $name"
}
test_case idle_success           0 "$NEW"    "$LEGACY" 0 1 0
test_case active_worker_busy     8 "$LEGACY" "$LEGACY" 0 1 1
test_case busy_but_home_changed  8 "$NEW"    "$LEGACY" 86 1 0
test_case other_failure          1 "$LEGACY" "$LEGACY" 86 1 0
test_case already_migrated       8 "$NEW"    "$NEW"    0 0 0
test_case customized_home        8 /home/custom /home/custom 0 0 0
