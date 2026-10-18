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

# Signal test 2: distinguish a genuine in-flight protected Ctrl+C from
# ordinary restart failure. Both use the real installed runtime_apply,
# rollback and menu. The first run is a NO-SIGNAL negative control; the
# second uses the actual controlling PTY and requires the installed INT
# handler's UI_SIGINT_CAUGHT value to be observed by a test-only pause hook.
# No production signal handler or privileged runtime function is replaced.
mkdir "$tmp/bin"
cat > "$tmp/bin/systemctl" <<'SYSTEMCTL'
#!/usr/bin/env bash
set -Eeuo pipefail
if [ "${1:-}" = restart ] && [ ! -e "$ASA_OPERATOR_RESTART_MARKER" ]; then
  : > "$ASA_OPERATOR_RESTART_MARKER"
  # Hold the real staged transaction in-flight until the PTY producer has
  # reached the signal gate. Never wait indefinitely, including CI failures.
  ready=false
  for _ in $(seq 1 240); do
    if [ -e "$ASA_OPERATOR_RELEASE_MARKER" ]; then ready=true; break; fi
    sleep 0.05
  done
  [ "$ready" = true ] || exit 92
  exit 93 # deterministic restart failure, independent of Ctrl+C
fi
exec /usr/bin/systemctl "$@"
SYSTEMCTL
chmod 0755 "$tmp/bin/systemctl"
cat > "$tmp/protected.sh" <<'PROTECTED'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source /usr/local/lib/ai-server-agent/manage.sh
# Preserve installed menu/INT handler and runtime_apply unchanged.
# A test-only pause wrapper observes the installed handler's flag just
# after the protected action returns and before the next interactive input.
pause(){
  if [ "${UI_SIGINT_CAUGHT:-0}" = 1 ]; then
    : > "$ASA_OPERATOR_SIGNAL_MARKER"
  fi
  if ui_has_interactive_tty; then read -r -p "Press Enter to continue... " _ </dev/tty || true; fi
}
menu
echo MENU_EXITED
PROTECTED
chmod 0700 "$tmp/protected.sh"

# The negative control must complete the same protected restart/rollback
# without a PTY signal and MUST NOT satisfy the protected-signal witness.
run_protected_case(){
  local mode="$1" restart_marker="$tmp/restart-$1" signal_marker="$tmp/signal-$1"
  local release_marker="$tmp/release-$1" output="$tmp/protected-$1.out"
  local ready_failure="$tmp/not-ready-$1" protected_rc
  set +e
  {
    printf '11\n3\ncommand_timeout_seconds\n15\ny\n'
    ready=false
    for _ in $(seq 1 300); do
      if [ -e "$restart_marker" ]; then ready=true; break; fi
      sleep 0.1
    done
    if [ "$ready" = true ]; then
      if [ "$mode" = signal ]; then printf '\003'; fi
      # The subprocess stays blocked until the signal byte was sent.
      sleep 0.35
    else
      : > "$ready_failure"
    fi
    : > "$release_marker"
    for _ in $(seq 1 700); do
      grep -Fq 'Runtime update failed;' "$output" 2>/dev/null && break
      sleep 0.1
    done
    printf '\n0\n0\n'
  } | NO_COLOR=1 TERM=dumb COLUMNS=60 LINES=24 timeout 110 script -qefc \
    "sudo env NO_COLOR=1 TERM=dumb ASA_OPERATOR_RESTART_MARKER='$restart_marker' ASA_OPERATOR_RELEASE_MARKER='$release_marker' ASA_OPERATOR_SIGNAL_MARKER='$signal_marker' PATH='$tmp/bin:/usr/bin:/bin' bash '$tmp/protected.sh'" \
    /dev/null > "$output" 2>&1
  protected_rc=$?
  set -e
  case "$protected_rc" in 0|130) ;; *) fail "Installed protected PTY ($mode) exit $protected_rc" ;; esac
  [ ! -e "$ready_failure" ] && [ -e "$restart_marker" ] ||
    fail "Protected ($mode) transaction was not in flight before the signal gate"
  grep -Fq 'Runtime update failed;' "$output" &&
    grep -Fq 'restored.' "$output" ||
    fail "Installed protected ($mode) action did not verify rollback"
  grep -Fq 'Action failed (exit 1)' "$output" ||
    fail "Enclosing menu hid the protected ($mode) transaction failure"
  grep -Fq 'MENU_EXITED' "$output" ||
    fail "Protected ($mode) action abandoned installed menu"
  if [ "$mode" = signal ]; then
    [ -e "$signal_marker" ] ||
      fail 'Real protected Ctrl+C was not witnessed by the installed menu INT handler'
  else
    [ ! -e "$signal_marker" ] ||
      fail 'Negative control recorded an unexpected protected INT'
    # Identical normal failure/rollback without Ctrl+C lacks the REQUIRED
    # signal witness. It is not a substitute for the positive signal case.
    echo 'INSTALLED_OPERATOR_PROTECTED_NO_SIGNAL_NEGATIVE_CONTROL_PASS'
  fi
  [ "$(config_hash)" = "$(printf '%s\n' "$before" | sed -n '1p')" ] ||
    fail "Protected ($mode) failure did not restore exact original config"
  [ "$(credentials_hash)" = "$(printf '%s\n' "$before" | sed -n '2p')" ] ||
    fail "Protected ($mode) failure modified credential store"
  [ "$(managed_hash)" = "$(printf '%s\n' "$before" | sed -n '3p')" ] ||
    fail "Protected ($mode) failure modified management state"
  sudo test -z "$(sudo find /etc/ai-server-agent -maxdepth 1 -name '.runtime-old.*' -print)" ||
    fail "Protected ($mode) rollback left a stale protected snapshot"
  for service in ai-server-agent.service ai-server-agent-executor.service; do
    sudo systemctl is-active --quiet "$service" ||
      fail "Protected ($mode) rollback left $service unhealthy"
  done
  sudo cmp -s /tmp/ai-server-agent /usr/local/bin/ai-server-agent ||
    fail "Installed binary changed during protected ($mode) rollback"
}

run_protected_case no-signal
# Protect systemd start-rate budgets after multiple installed restart tests.
sleep 12
run_protected_case signal
echo 'INSTALLED_OPERATOR_CONSOLE_ACCEPTANCE_PASS real_menu=yes 50col_no_color=yes invalid_retry=yes input_cancel_no_mutation=yes read_only_sigint=recovered protected_sigint=witnessed_in_flight_then_verified_rollback no_signal_negative_control=passed credentials=preserved installed_services=healthy'
