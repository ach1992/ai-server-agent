#!/usr/bin/env bash
# Real installed manager PTY acceptance. Explicit opt-in on a disposable CI
# runner only: it deliberately injects one restart failure and verifies rollback.
set -Eeuo pipefail

fail(){ printf 'INSTALLED OPERATOR CONSOLE FAIL: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -ne 0 ] || fail 'Run as unprivileged CI runner, not root'
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${ASA_DISPOSABLE_OPERATOR_ACCEPTANCE:-}" = 1 ] ||
  fail 'Disposable GitHub Actions explicit opt-in required'
for cmd in script sudo timeout jq sha256sum systemctl; do
  command -v "$cmd" >/dev/null || fail "Missing required command: $cmd"
done
[ -s /tmp/ai-server-agent ] || fail 'CI candidate binary missing'
sudo cmp -s /tmp/ai-server-agent /usr/local/bin/ai-server-agent ||
  fail 'Installed Agent binary does not match the checked candidate'
sudo test -r /usr/local/lib/ai-server-agent/manage.sh || fail 'Installed manager source missing'
for service in ai-server-agent.service ai-server-agent-executor.service; do
  sudo systemctl is-active --quiet "$service" || fail "Installed service inactive: $service"
done
sudo test -e /srv/ai-workspace/lifecycle-workspace-sentinel ||
  fail 'Disposable lifecycle workspace sentinel missing'

tmp="$(mktemp -d)"
chmod 0700 "$tmp"
trap 'rm -rf -- "$tmp"' EXIT
config_hash(){ sudo sha256sum /etc/ai-server-agent/config.json | awk '{print $1}'; }
credentials_hash(){ sudo sha256sum /etc/ai-server-agent/mcp-credentials.json | awk '{print $1}'; }
managed_hash(){ sudo sha256sum /etc/ai-server-agent/managed.json 2>/dev/null | awk '{print $1}'; }
service_pid(){ sudo systemctl show -p MainPID --value "$1"; }
snapshot(){
  config_hash
  credentials_hash
  managed_hash
  service_pid ai-server-agent.service
  service_pid ai-server-agent-executor.service
}
before="$(snapshot)"

# Exercise the REAL installed management entrypoint, not the repository mock:
# nested Connection -> invalid choice -> retry -> Local -> b cancel -> Back,
# Runtime and Purpose navigation, then Exit, at a narrow 50-column PTY.
cat > "$tmp/navigation.in" <<'INPUT'
3
88

2
b

0
11
0
12
0
0
INPUT
NO_COLOR=1 TERM=dumb COLUMNS=50 LINES=24 timeout 55 script -qefc \
  'sudo env NO_COLOR=1 TERM=dumb COLUMNS=50 LINES=24 ai-server-agent-manage menu' \
  /dev/null < "$tmp/navigation.in" > "$tmp/navigation.out" ||
  fail 'Installed nested menu did not exit successfully'
grep -Fq 'Management console' "$tmp/navigation.out" || fail 'Installed menu header missing'
grep -Fq 'Choose 1, 2, 3 or 0.' "$tmp/navigation.out" || fail 'Invalid-choice retry missing'
grep -Fq 'Cancelled at input prompt; no operation committed.' "$tmp/navigation.out" ||
  fail 'Real connection input b did not cancel before commit'
grep -Fq 'Connection setup was not verified' "$tmp/navigation.out" || fail 'Connection recovery guidance missing'
grep -Fq 'Runtime capacity / timeouts' "$tmp/navigation.out" || fail 'Runtime navigation missing'
grep -Fq 'Server purpose / label' "$tmp/navigation.out" || fail 'Purpose navigation missing'
if LC_ALL=C grep -q $'\033' "$tmp/navigation.out"; then fail 'NO_COLOR emitted ANSI'; fi
[ "$(snapshot)" = "$before" ] || fail 'Navigation and cancellation mutated config/credentials/state or restarted services'

# Signal test 1: a real Ctrl+C byte on the installed manager's controlling PTY
# cancels a deliberately slow read-only action, not the menu. Only the view is
# replaced to synchronize an in-flight signal; the installed menu/traps run.
cat > "$tmp/slow-read.sh" <<'READ_ONLY'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source /usr/local/lib/ai-server-agent/manage.sh
status(){
  echo READ_ONLY_STARTED
  : > "$ASA_OPERATOR_READ_MARKER"
  sleep 30
  echo READ_ONLY_FINISHED_UNEXPECTEDLY
}
menu
echo MENU_EXITED
READ_ONLY
chmod 0700 "$tmp/slow-read.sh"
read_marker="$tmp/read-start"
set +e
{
  printf '1\n'
  for _ in $(seq 1 160); do [ -f "$read_marker" ] && break; sleep 0.05; done
  printf '\003'
  sleep 0.4
  printf '\n0\n'
} | ASA_OPERATOR_READ_MARKER="$read_marker" NO_COLOR=1 TERM=dumb \
  timeout 18 script -qefc \
  "sudo env NO_COLOR=1 TERM=dumb ASA_OPERATOR_READ_MARKER='$read_marker' bash '$tmp/slow-read.sh'" \
  /dev/null > "$tmp/read-only.out" 2>&1
read_rc=$?
set -e
case "$read_rc" in 0|130) ;; *) fail "Installed read-only PTY exit $read_rc" ;; esac
[ -e "$read_marker" ] || fail 'Read-only signal was not delivered during the action'
grep -Fq 'Read-only operation interrupted (Ctrl+C)' "$tmp/read-only.out" ||
  fail 'Installed menu did not report read-only cancellation'
grep -Fq 'MENU_EXITED' "$tmp/read-only.out" || fail 'Read-only signal exited the enclosing menu'
! grep -Fq 'READ_ONLY_FINISHED_UNEXPECTEDLY' "$tmp/read-only.out" ||
  fail 'Read-only action ignored Ctrl+C'
[ "$(snapshot)" = "$before" ] || fail 'Read-only SIGINT changed protected host state'

# Give systemd a fresh restart-rate window after the prior installed
# runtime acceptance (which deliberately performs several real restarts).
sleep 12

# Signal test 2: a real Ctrl+C byte after a REAL runtime transaction staged
# a config change but before a deliberate one-shot systemctl restart failure.
# The installed menu must not abandon the protected operation; runtime_apply
# must restore the old config and verify installed health. No secrets or
# production/Cloudflare resources are touched.
mkdir "$tmp/bin"
cat > "$tmp/bin/systemctl" <<'SYSTEMCTL'
#!/usr/bin/env bash
set -Eeuo pipefail
if [ "${1:-}" = restart ] && [ ! -e "$ASA_OPERATOR_RESTART_MARKER" ]; then
  : > "$ASA_OPERATOR_RESTART_MARKER"
  sleep 2
  exit 93
fi
exec /usr/bin/systemctl "$@"
SYSTEMCTL
chmod 0755 "$tmp/bin/systemctl"
cat > "$tmp/protected.sh" <<'PROTECTED'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source /usr/local/lib/ai-server-agent/manage.sh
menu
echo MENU_EXITED
PROTECTED
chmod 0700 "$tmp/protected.sh"
restart_marker="$tmp/restart-attempt"
set +e
{
  printf '11\n3\ncommand_timeout_seconds\n15\ny\n'
  for _ in $(seq 1 300); do [ -f "$restart_marker" ] && break; sleep 0.1; done
  printf '\003'
  # The protected subprocess must complete its restart/rollback/recovery.
  # Wait for the real installed manager to report verified recovery.
  for _ in $(seq 1 700); do
    grep -Fq 'Runtime update failed;' "$tmp/protected.out" 2>/dev/null && break
    sleep 0.1
  done
  printf '\n0\n0\n'
} | NO_COLOR=1 TERM=dumb COLUMNS=60 LINES=24 timeout 110 script -qefc \
  "sudo env NO_COLOR=1 TERM=dumb ASA_OPERATOR_RESTART_MARKER='$restart_marker' PATH='$tmp/bin:/usr/bin:/bin' bash '$tmp/protected.sh'" \
  /dev/null > "$tmp/protected.out" 2>&1
protected_rc=$?
set -e
case "$protected_rc" in 0|130) ;; *) fail "Installed protected PTY exit $protected_rc" ;; esac
[ -e "$restart_marker" ] || fail 'One-shot systemctl fault did not run'
# The genuine interactive console wraps long errors. Match the two
# distinctive segments independently, not an impossible one-line literal.
grep -Fq 'Runtime update failed;' "$tmp/protected.out" &&
  grep -Fq 'restored.' "$tmp/protected.out" ||
  fail 'Installed protected action did not verify rollback'
grep -Fq 'Action failed (exit 1)' "$tmp/protected.out" ||
  fail 'Enclosing menu hid the protected transaction failure'
grep -Fq 'MENU_EXITED' "$tmp/protected.out" ||
  fail 'Ctrl+C abandoned installed protected action/menu'
[ "$(config_hash)" = "$(printf '%s\n' "$before" | sed -n '1p')" ] ||
  fail 'Protected failure did not restore exact old config bytes'
[ "$(credentials_hash)" = "$(printf '%s\n' "$before" | sed -n '2p')" ] ||
  fail 'Protected failure modified credential store'
[ "$(managed_hash)" = "$(printf '%s\n' "$before" | sed -n '3p')" ] ||
  fail 'Protected failure modified management state'
sudo test -z "$(sudo find /etc/ai-server-agent -maxdepth 1 -name '.runtime-old.*' -print)" ||
  fail 'Protected rollback left a stale protected snapshot'
for service in ai-server-agent.service ai-server-agent-executor.service; do
  sudo systemctl is-active --quiet "$service" || fail "Protected rollback left $service unhealthy"
done
sudo cmp -s /tmp/ai-server-agent /usr/local/bin/ai-server-agent ||
  fail 'Installed binary changed during protected rollback'
echo 'INSTALLED_OPERATOR_CONSOLE_ACCEPTANCE_PASS real_menu=yes 50col_no_color=yes invalid_retry=yes input_cancel_no_mutation=yes read_only_sigint=recovered protected_sigint=finished_then_verified_rollback credentials=preserved installed_services=healthy'
