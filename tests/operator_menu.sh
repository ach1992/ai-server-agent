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
source "$ASA_MANAGE_SOURCE"
export NO_COLOR=1 TERM=dumb COLUMNS=60 LINES=24
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
  printf 'BEFORE_SIMULATED_FAILURE\n'
  die 'TEST ONLY: failed status must not end menu'
  printf 'AFTER_SIMULATED_FAILURE\n'
}
credential_status(){ printf 'Dummy credentials (no secrets)\n'; }
credential_issue(){ echo 'ISSUED_UNEXPECTEDLY'; exit 1; }
credential_revoke(){ echo 'REVOKED_UNEXPECTEDLY'; exit 1; }
profile_show(){ printf 'Profile: validation\n'; }
profile_apply(){ echo 'PROFILE_CHANGED_UNEXPECTEDLY'; exit 1; }
runtime_apply(){ echo 'RUNTIME_CHANGED_UNEXPECTEDLY'; exit 1; }
configure_cloudflare(){ echo 'CLOUDFLARE_CHANGED_UNEXPECTEDLY'; exit 1; }
configure_local(){ echo 'CONNECTION_CHANGED_UNEXPECTEDLY'; exit 1; }
configure_manual_tls(){ echo 'TLS_CHANGED_UNEXPECTEDLY'; exit 1; }
case "${1:-}" in
  menu) menu; printf 'MENU_EXITED_CLEANLY\n' ;;
  first-run)
    set +e
    first_run
    code=$?
    set -e
    printf 'FIRST_RUN_EXIT_CODE=%s\n' "$code"
    [ "$code" -eq 20 ] ;;
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

echo 'Operator menu TTY navigation/strict failure/back/cancel/no-color PASS'
