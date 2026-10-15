#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v script >/dev/null || { echo "script(1) required for operator TTY test" >&2; exit 1; }
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cat > "$TMP/console.sh" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
export NO_COLOR=1 TERM=dumb COLUMNS=60 LINES=24
source "$ASA_MANAGE_SOURCE"
RUNTIME_BINARY=/bin/true
installed_identity(){ printf 'source|source|0123456789abcdef0123456789abcdef01234567\n'; }
config_get(){
  case "$1" in
    server_label) printf 'QA Console\n' ;;
    environment_profile) printf 'validation\n' ;;
    listen_address) printf '127.0.0.1:3210\n' ;;
    *) return 0 ;;
  esac
}
managed_get(){ return 0; }
current_mode(){ printf 'local\n'; }
current_port(){ printf '3210\n'; }
status(){
  if [ "${ASA_SIMULATE_SLOW_VIEW:-0}" = 1 ]; then
    printf 'READ_ONLY_STARTED\n'
    : > "$ASA_SIGNAL_MARKER"
    sleep 15
    printf 'READ_ONLY_FINISHED_UNEXPECTEDLY\n'
    return 0
  fi
  printf 'BEFORE_SIMULATED_FAILURE\n'
  die 'TEST ONLY: failed status must not end menu'
  printf 'AFTER_SIMULATED_FAILURE\n'
}
credential_status(){ printf 'Dummy credentials (no secrets)\n'; }
credential_issue(){ echo 'ISSUED_UNEXPECTEDLY'; exit 1; }
credential_revoke(){ echo 'REVOKED_UNEXPECTEDLY'; exit 1; }
profile_show(){ printf 'Profile: validation\n'; }
profile_apply(){ echo 'PROFILE_CHANGED_UNEXPECTEDLY'; exit 1; }
runtime_apply(){
  if [ "${ASA_SIMULATE_PROTECTED_WRITE:-0}" = 1 ]; then
    printf 'PROTECTED_WRITE_STARTED\n'
    : > "$ASA_SIGNAL_MARKER"
    sleep 2
    printf 'PROTECTED_WRITE_COMPLETED\n'
    return 0
  fi
  echo 'RUNTIME_CHANGED_UNEXPECTEDLY'; exit 1
}
configure_cloudflare(){
  if [ "${ASA_SIMULATE_FAILED_CLOUDFLARE:-0}" = 1 ]; then
    echo 'BEGIN_CLOUDFLARE_FAILED_SETUP'
    die 'TEST ONLY: aborted before any external change'
    echo 'AFTER_CLOUDFLARE_FAILURE'
  fi
  echo 'CLOUDFLARE_CHANGED_UNEXPECTEDLY'; exit 1
}
configure_local(){
  if [ "${ASA_SIMULATE_PRECOMMIT_CANCEL:-0}" = 1 ]; then
    local port
    [ -r /dev/tty ] && port="$(prompt_value 'Local MCP port' 3210)"
    printf 'UNSAFE_LOCAL_CHANGE_AFTER_CANCEL=%s\n' "$port"
    return 0
  fi
  if [ "${ASA_SIMULATE_LOCAL_SUCCESS:-0}" = 1 ]; then
    if [ "${ASA_REQUIRE_USED_FD3_CLOSED:-0}" = 1 ]; then
      [ -z "${AI_SERVER_AGENT_MCP_AUTH_FD:-}" ] || { echo 'ONE_USE_FD3_ENV_REUSED_UNEXPECTEDLY'; exit 1; }
      [ ! -e "/proc/$BASHPID/fd/3" ] || { echo 'ONE_USE_FD3_STILL_OPEN_UNEXPECTEDLY'; exit 1; }
      echo 'ONE_USE_FD3_CLOSED_AND_UNSET'
    fi
    echo 'SIMULATED_LOCAL_CONFIG_SUCCESS'; return 0
  fi
  echo 'CONNECTION_CHANGED_UNEXPECTEDLY'; exit 1
}
chatgpt_setup(){ echo 'SIMULATED_CLIENT_GUIDANCE'; }
configure_manual_tls(){ echo 'TLS_CHANGED_UNEXPECTEDLY'; exit 1; }
case "${1:-}" in
  menu|menu-slow-view|menu-protected|menu-cancel-before-commit) menu; printf 'MENU_EXITED_CLEANLY\n' ;;
  first-run|first-run-failed-setup|first-run-success|first-run-retry-fd3)
    if [ "$1" = first-run-failed-setup ] || [ "$1" = first-run-retry-fd3 ]; then
      export ASA_SIMULATE_FAILED_CLOUDFLARE=1
    fi
    if [ "$1" = first-run-success ] || [ "$1" = first-run-retry-fd3 ]; then
      export ASA_SIMULATE_LOCAL_SUCCESS=1
    fi
    set +e
    first_run
    code=$?
    set -e
    printf 'FIRST_RUN_EXIT_CODE=%s\n' "$code"
    if [ "$1" = first-run-success ] || [ "$1" = first-run-retry-fd3 ]; then [ "$code" -eq 0 ]; else [ "$code" -eq 20 ]; fi ;;
esac
SH
chmod 700 "$TMP/console.sh"

# Invalid main selection -> controlled retry, failed status -> controlled
# recovery, runtime key cancellation, credential refusal, profile cancellation
# and bad nested connection selection -> Back -> Exit.
cat > "$TMP/menu.in" <<'INPUT'
99

1

11
1
b
0
10
1
n

0
12
3
b
0
3
99

0
0
INPUT
ASA_MANAGE_SOURCE="$ROOT/manage.sh" script -q -e -c "bash '$TMP/console.sh' menu" /dev/null < "$TMP/menu.in" > "$TMP/menu.out"
grep -Fq 'Management console' "$TMP/menu.out"
# The identity header and first prompt must remain on a 24-row console.
first_screen_lines="$(awk '/^AI Server Agent/ {seen=1} seen {count++} /^Choose:/ && seen {print count; exit}' "$TMP/menu.out")"
[ -n "$first_screen_lines" ] && [ "$first_screen_lines" -le 24 ] || {
  printf '24-row console first screen uses %s rows\n' "$first_screen_lines" >&2
  exit 1
}
grep -Fq '0123456789ab' "$TMP/menu.out"
grep -Fq 'QA Console (validation)' "$TMP/menu.out"
grep -Fq 'Local/private' "$TMP/menu.out"
grep -Fq 'Unknown option' "$TMP/menu.out"
grep -Fq 'BEFORE_SIMULATED_FAILURE' "$TMP/menu.out"
grep -Fq 'Action failed (exit 1)' "$TMP/menu.out"
grep -Fq 'Cancelled; credentials unchanged' "$TMP/menu.out"
grep -Fq 'MENU_EXITED_CLEANLY' "$TMP/menu.out"
if grep -Eq 'AFTER_SIMULATED_FAILURE|ISSUED_UNEXPECTEDLY|REVOKED_UNEXPECTEDLY|PROFILE_CHANGED_UNEXPECTEDLY|RUNTIME_CHANGED_UNEXPECTEDLY|CLOUDFLARE_CHANGED_UNEXPECTEDLY|CONNECTION_CHANGED_UNEXPECTEDLY|TLS_CHANGED_UNEXPECTEDLY' "$TMP/menu.out"; then
  echo 'Menu leaked a forbidden action or suppressed the failing child errexit' >&2
  exit 1
fi
if LC_ALL=C grep -q $'\033' "$TMP/menu.out"; then
  echo 'NO_COLOR/TERM=dumb interactive console emitted ANSI escapes' >&2
  exit 1
fi
# Test real dynamic header fields at narrow width, excluding input echo.
if grep -E '^  (Repository|Build|Server|Connection)' "$TMP/menu.out" | awk 'length($0)>61 {bad=1} END{exit bad?0:1}'; then
  echo 'Operator header overflowed the 60-column terminal' >&2
  exit 1
fi

cat > "$TMP/first.in" <<'INPUT'
bad

0
INPUT
ASA_MANAGE_SOURCE="$ROOT/manage.sh" script -q -e -c "bash '$TMP/console.sh' first-run" /dev/null < "$TMP/first.in" > "$TMP/first.out"
grep -Fq 'No setup action started' "$TMP/first.out"
grep -Fq 'FIRST_RUN_EXIT_CODE=20' "$TMP/first.out"
! grep -Fq 'CONNECTION_CHANGED_UNEXPECTEDLY' "$TMP/first.out"
! grep -Fq 'CLOUDFLARE_CHANGED_UNEXPECTEDLY' "$TMP/first.out"

cat > "$TMP/first-failed.in" <<'INPUT'
1

0
INPUT
ASA_MANAGE_SOURCE="$ROOT/manage.sh" script -q -e -c "bash '$TMP/console.sh' first-run-failed-setup" /dev/null < "$TMP/first-failed.in" > "$TMP/first-failed.out"
grep -Fq 'BEGIN_CLOUDFLARE_FAILED_SETUP' "$TMP/first-failed.out"
grep -Fq 'Action failed (exit 1)' "$TMP/first-failed.out"
grep -Fq 'Connection setup was not verified' "$TMP/first-failed.out"
grep -Fq 'FIRST_RUN_EXIT_CODE=20' "$TMP/first-failed.out"
! grep -Fq 'AFTER_CLOUDFLARE_FAILURE' "$TMP/first-failed.out"

cat > "$TMP/first-success.in" <<'INPUT'
2
INPUT
ASA_MANAGE_SOURCE="$ROOT/manage.sh" script -q -e -c "bash '$TMP/console.sh' first-run-success" /dev/null < "$TMP/first-success.in" > "$TMP/first-success.out"
grep -Fq 'SIMULATED_LOCAL_CONFIG_SUCCESS' "$TMP/first-success.out"
grep -Fq 'SIMULATED_CLIENT_GUIDANCE' "$TMP/first-success.out"
grep -Fq 'FIRST_RUN_EXIT_CODE=0' "$TMP/first-success.out"

# A pre-commit b from the actual setup prompt must stop before mutation
# without exiting the enclosing main menu.
cat > "$TMP/precommit.in" <<'INPUT'
3
2
b

0
INPUT
ASA_SIMULATE_PRECOMMIT_CANCEL=1 ASA_MANAGE_SOURCE="$ROOT/manage.sh" script -q -e -c "bash '$TMP/console.sh' menu-cancel-before-commit" /dev/null < "$TMP/precommit.in" > "$TMP/precommit.out"
grep -Fq 'Cancelled at input prompt; no operation committed.' "$TMP/precommit.out"
grep -Fq 'Operation deferred/cancelled (exit 20)' "$TMP/precommit.out"
grep -Fq 'MENU_EXITED_CLEANLY' "$TMP/precommit.out"
! grep -Fq 'UNSAFE_LOCAL_CHANGE_AFTER_CANCEL' "$TMP/precommit.out"

# script(1) itself may exit 130 when its PTY receives Ctrl+C. Check that
# the CHILD console returns safely, not the controlling util-linux process.
# A marker synchronizes the signal to an actively running read-only command.
read_marker="$TMP/read-only-started"
set +e
{
  printf '1\n'
  for i in $(seq 1 240); do [ -f "$read_marker" ] && break; sleep 0.05; done
  printf '\003'
  sleep 0.4
  printf '\n0\n'
} | ASA_SIMULATE_SLOW_VIEW=1 ASA_SIGNAL_MARKER="$read_marker" ASA_MANAGE_SOURCE="$ROOT/manage.sh" timeout 12 script -q -e -c "bash '$TMP/console.sh' menu-slow-view" /dev/null > "$TMP/sigint-read.out" 2>&1
read_rc=$?
set -e
case "$read_rc" in 0|130) ;; *) echo "Read-only signal fixture exit $read_rc" >&2; exit 1 ;; esac
[ -f "$read_marker" ]
grep -Fq 'READ_ONLY_STARTED' "$TMP/sigint-read.out"
grep -Fq 'Read-only operation interrupted (Ctrl+C); returned to the' "$TMP/sigint-read.out"
grep -Fq 'MENU_EXITED_CLEANLY' "$TMP/sigint-read.out"
! grep -Fq 'READ_ONLY_FINISHED_UNEXPECTEDLY' "$TMP/sigint-read.out"

# Critical operations already running must NOT be abandoned on SIGINT.
# They should finish/recover before the normal Back flow resumes.
write_marker="$TMP/write-started"
set +e
{
  printf '11\n1\n8\n'
  for i in $(seq 1 240); do [ -f "$write_marker" ] && break; sleep 0.05; done
  printf '\003'
  sleep 2.4
  printf '\n0\n0\n'
} | ASA_SIMULATE_PROTECTED_WRITE=1 ASA_SIGNAL_MARKER="$write_marker" ASA_MANAGE_SOURCE="$ROOT/manage.sh" timeout 12 script -q -e -c "bash '$TMP/console.sh' menu-protected" /dev/null > "$TMP/sigint-write.out" 2>&1
write_rc=$?
set -e
case "$write_rc" in 0|130) ;; *) echo "Protected signal fixture exit $write_rc" >&2; exit 1 ;; esac
[ -f "$write_marker" ]
grep -Fq 'PROTECTED_WRITE_STARTED' "$TMP/sigint-write.out"
grep -Fq 'PROTECTED_WRITE_COMPLETED' "$TMP/sigint-write.out"
grep -Fq 'MENU_EXITED_CLEANLY' "$TMP/sigint-write.out"
! grep -Fq 'Action failed' "$TMP/sigint-write.out"

# Initial setup handoff is one-use even if Cloudflare fails. A later retry
# must use a new hidden prompt, never the inherited FD3 or its env marker.
cat > "$TMP/retry-fd3.in" <<'INPUT'
1

2
INPUT
ASA_REQUIRE_USED_FD3_CLOSED=1 AI_SERVER_AGENT_MCP_AUTH_FD=3 ASA_MANAGE_SOURCE="$ROOT/manage.sh" script -q -e -c "bash '$TMP/console.sh' first-run-retry-fd3" /dev/null < "$TMP/retry-fd3.in" 3<<<'one-use-placeholder' > "$TMP/retry-fd3.out"
grep -Fq 'BEGIN_CLOUDFLARE_FAILED_SETUP' "$TMP/retry-fd3.out"
grep -Fq 'ONE_USE_FD3_CLOSED_AND_UNSET' "$TMP/retry-fd3.out"
grep -Fq 'FIRST_RUN_EXIT_CODE=0' "$TMP/retry-fd3.out"
! grep -Eq 'ONE_USE_FD3_(ENV_REUSED|STILL_OPEN)_UNEXPECTEDLY' "$TMP/retry-fd3.out"

# A signal while answering a confirmation must be equivalent to No,
# without unset-answer expansion or credential change.
set +e
{
  printf '10\n1\n'
  sleep 0.7
  printf '\003'
  sleep 0.35
  printf '\n0\n0\n'
} | ASA_MANAGE_SOURCE="$ROOT/manage.sh" timeout 9 script -q -e -c "bash '$TMP/console.sh' menu" /dev/null > "$TMP/sigint-confirm.out" 2>&1
confirm_rc=$?
set -e
case "$confirm_rc" in 0|130) ;; *) echo "Confirmation signal fixture exit $confirm_rc" >&2; exit 1 ;; esac
grep -Fq 'Confirm credential_issue for direct-default?' "$TMP/sigint-confirm.out"
grep -Fq 'Cancelled; credentials unchanged' "$TMP/sigint-confirm.out"
grep -Fq 'MENU_EXITED_CLEANLY' "$TMP/sigint-confirm.out"
! grep -Eq 'ISSUED_UNEXPECTEDLY|REVOKED_UNEXPECTEDLY' "$TMP/sigint-confirm.out"

echo 'Operator menu TTY navigation/strict failure/Back/precommit/SIGINT read+confirm/protected commit/one-use FD3/no-color PASS'
