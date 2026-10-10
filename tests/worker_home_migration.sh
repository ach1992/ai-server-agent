#!/usr/bin/env bash
set -Eeuo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
# Test the exact production migration function, without sourcing/running the
# privileged installer or changing passwd, credentials, services or real HOME.
body="$(awk '/^migrate_existing_worker_login_home\(\) \{/ { within=1 } within {print} within && /^}/ {exit}' "$root/install.sh")"
[[ "$body" == migrate_existing_worker_login_home* && "$body" == *'usermod --home'* && "$body" == *'stat -c'* ]] || {
  echo 'Production worker HOME and identity guard missing' >&2; exit 1;
}
eval "$body"

LEGACY=/srv/ai-workspace
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
NEW="$tmp/worker-home"
mkdir -m 0700 "$NEW"

account() { printf 'aiworker:x:%s:%s::%s:%s' "$1" "$2" "$3" "${4:-/bin/bash}"; }

test_case() {
  local name="$1" mode="$2" mock_before_record="$3" mock_after_record="$4" private_stat="$5"
  local expected_rc="$6" expected_calls="$7" expected_warnings="$8"
  local calls=0 warnings=0 rc=0 stage=before
  usermod() {
    [[ "$1" == --home && "$2" == "$NEW" && "$3" == aiworker ]] || return 2
    calls=$((calls+1))
    stage=after
    return "$mode"
  }
  getent() {
    [[ "$1" == passwd && "$2" == aiworker ]] || return 2
    local record="$mock_before_record"
    [[ "$stage" == after ]] && record="$mock_after_record"
    [[ "$record" != __missing__ ]] || return 2
    printf '%s\n' "$record"
  }
  stat() {
    [[ "$#" -eq 4 && "$1" == -c && "$2" == '%u:%g:%a' && "$3" == -- && "$4" == "$NEW" ]] || return 2
    [[ "$private_stat" != __missing__ ]] || return 2
    printf '%s\n' "$private_stat"
  }
  warn() { warnings=$((warnings+1)); }
  die() { return 86; }
  if migrate_existing_worker_login_home aiworker "$LEGACY" "$NEW"; then rc=0; else rc=$?; fi
  if [[ "$rc" -ne "$expected_rc" || "$calls" -ne "$expected_calls" || "$warnings" -ne "$expected_warnings" ]]; then
    printf 'FAIL %s: rc=%s calls=%s warnings=%s (expected %s/%s/%s)\n' \
      "$name" "$rc" "$calls" "$warnings" "$expected_rc" "$expected_calls" "$expected_warnings" >&2
    exit 1
  fi
  printf 'PASS %s\n' "$name"
}

OLD="$(account 987 988 "$LEGACY")"
NEW_LOGIN="$(account 987 988 "$NEW")"
CUSTOM="$(account 987 988 /home/custom)"
OWNER=987:988:700

test_case idle_success                0 "$OLD" "$NEW_LOGIN" "$OWNER" 0 1 0
test_case active_worker_busy          8 "$OLD" "$OLD" "$OWNER"       0 1 1
test_case busy_but_home_changed       8 "$OLD" "$NEW_LOGIN" "$OWNER" 86 1 0
test_case other_usermod_failure       1 "$OLD" "$OLD" "$OWNER"       86 1 0
test_case already_migrated            8 "$NEW_LOGIN" "$NEW_LOGIN" "$OWNER" 0 0 0
test_case customized_home             8 "$CUSTOM" "$CUSTOM" "$OWNER" 0 0 0

# R1: account identity must survive both success and busy-user paths.
test_case busy_changed_uid            8 "$OLD" "$(account 999 988 "$LEGACY")" "$OWNER" 86 1 0
test_case busy_changed_gid            8 "$OLD" "$(account 987 999 "$LEGACY")" "$OWNER" 86 1 0
test_case busy_account_disappeared    8 "$OLD" __missing__ "$OWNER" 86 1 0
test_case success_changed_uid         0 "$OLD" "$(account 999 988 "$NEW")" "$OWNER" 86 1 0
test_case success_changed_gid         0 "$OLD" "$(account 987 999 "$NEW")" "$OWNER" 86 1 0
test_case success_unexpected_home     0 "$OLD" "$OLD" "$OWNER"       86 1 0
test_case busy_changed_shell          8 "$OLD" "$(account 987 988 "$LEGACY" /usr/sbin/nologin)" "$OWNER" 86 1 0

# After migration/defer, the pre-provisioned private HOME must still be owned
# by exactly the captured account UID/GID and have private mode 0700.
test_case private_home_wrong_uid      8 "$OLD" "$OLD" 999:988:700   86 1 0
test_case private_home_wrong_gid      8 "$OLD" "$OLD" 987:999:700   86 1 0
test_case private_home_wrong_mode     8 "$OLD" "$OLD" 987:988:755   86 1 0
test_case private_home_stat_failed    8 "$OLD" "$OLD" __missing__   86 1 0

test_case missing_initial_account     8 __missing__ "$OLD" "$OWNER" 86 0 0
test_case malformed_initial_identity  8 "$(account X 988 "$LEGACY")" "$OLD" "$OWNER" 86 0 0
test_case malformed_followup_account  8 "$OLD" malformed "$OWNER"   86 1 0

echo 'worker HOME migration identity / ownership regression suite passed'
