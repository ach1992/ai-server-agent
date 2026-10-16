#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v script >/dev/null 2>&1 || { echo 'script(1) is required for operator PTY tests' >&2; exit 1; }
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'rc=$?; printf "Operator presentation failure: line %s, status %s, command: %s\n" "$LINENO" "$rc" "$BASH_COMMAND" >&2' ERR
cat > "$TMP/fixture.sh" <<'FIXTURE'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ASA_SOURCE"
RUNTIME_BINARY=/bin/true
CF_TXN_STATE="$ASA_TMP/no-pending-transaction"
installed_identity(){ printf 'source|source|0123456789abcdef0123456789abcdef01234567\n'; }
config_get(){
  case "$1" in
    environment_profile) printf 'validation\n' ;;
    server_label)
      if [ "${ASA_LABEL_WITH_CONTROL:-0}" = 1 ]; then printf 'QA Console\033[31m\033]0;window-title\007\n'; else printf 'QA Console\n'; fi ;;
    listen_address) printf '127.0.0.1:3210\n' ;;
  esac
}
managed_get(){ return 0; }
current_mode(){ printf 'local\n'; }
current_port(){ printf '3210\n'; }
acquire_management_lock(){ :; }
write_config(){ printf 'CONFIG_MUTATED=%s/%s\n' "$1" "$2"; }
restart_and_verify_local(){ printf 'SERVICES_RESTARTED\n'; }
save_local_state(){ printf 'MANAGED_STATE_MUTATED\n'; }
systemctl(){
  case "${1:-}" in
    is-active) printf 'active\n' ;;
    *) echo 'UNEXPECTED_SYSTEMCTL_ACTION' >&2; exit 1 ;;
  esac
}
curl(){ return 0; }
acquire_cloudflare_mcp_bearer(){ CF_MCP_AUTH_TOKEN='fixture-not-secret'; }
print_cf_token_guidance(){ printf 'TEST_CLOUDFLARE_READ_ONLY_GUIDANCE\n'; }
load_cf_token(){ CF_TOKEN='fixture'; }
cf_find_zone(){ printf 'zone-fixture|example.com\n'; }
cf_public_ipv4(){ printf '203.0.113.10\n'; }
cf_issue_origin_cert(){ printf 'UNSAFE_CLOUDFLARE_WRITE\n'; exit 1; }
update_agent(){ printf 'UNSAFE_UPDATE_STARTED\n'; exit 1; }
repair(){ printf 'UNSAFE_REPAIR_STARTED\n'; exit 1; }
case "${1:-}" in
  main) menu ;;
  local) configure_local ;;
  cloudflare) configure_cloudflare ;;
  tls) configure_manual_tls ;;
  status) status ;;
  log) log 'Informational operation message with a very very long address /absolute/path/with/hard-to-wrap-components/and-even-more-text/below'; ui_notice success 'Verified completion'; warn 'Operator must examine this warning before retrying';;
  *) echo 'Unknown test fixture' >&2; exit 1 ;;
esac
FIXTURE
chmod 700 "$TMP/fixture.sh"

for width in 40 60 100; do
  printf '0\n' | ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" NO_COLOR=1 TERM=dumb COLUMNS="$width" LINES=24 \
    script -q -e -c "bash '$TMP/fixture.sh' main" /dev/null > "$TMP/main-$width.out"
  tr -d '\r' < "$TMP/main-$width.out" > "$TMP/main-$width.txt"
  grep -Fq 'QA Console (validation)' "$TMP/main-$width.txt"
  grep -Fq 'ach1992/ai-server-agent' "$TMP/main-$width.txt"
  grep -Fq '12) Server purpose / label' "$TMP/main-$width.txt"
  grep -Fq ' 8) Purge Agent-owned data' "$TMP/main-$width.txt"
  if LC_ALL=C grep -q $'\033' "$TMP/main-$width.txt"; then
    echo "No-color output at width $width emitted ANSI" >&2; exit 1
  fi
  if awk -v width="$width" 'length($0)>width && $0 !~ /^0$/ { bad=1 } END {exit !bad}' "$TMP/main-$width.txt"; then
    echo "Operator menu overflowed width $width" >&2; exit 1
  fi
done
for width in 40 60; do
  first_rows="$(awk '/^AI Server Agent/{seen=1} seen{rows++} /Choose:/ && seen{print rows; exit}' "$TMP/main-$width.txt")"
  [ -n "$first_rows" ] && [ "$first_rows" -le 24 ] || {
    echo "The first $width-column menu used $first_rows rows in a 24-row terminal" >&2; exit 1
  }
done

printf '0\n' | ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" TERM=xterm NO_COLOR= COLUMNS=80 LINES=30 \
  script -q -e -c "bash '$TMP/fixture.sh' main" /dev/null > "$TMP/color.out"
for code in '31' '33' '36'; do
  if ! LC_ALL=C grep -Fq "$(printf '\033[%sm' "$code")" "$TMP/color.out"; then
    echo "Missing terminal semantic color $code" >&2; exit 1
  fi
done

# Inspect read-only status and severity messages at narrow width with no
# real host/service access. Text must be useful even without colors.
for view in status log; do
  ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" NO_COLOR=1 TERM=dumb COLUMNS=40 LINES=24 \
    script -q -e -c "bash '$TMP/fixture.sh' $view" /dev/null < /dev/null > "$TMP/$view.out" 2>&1
  tr -d '\r' < "$TMP/$view.out" > "$TMP/$view.txt"
  if awk 'length($0)>40 && $0 !~ /^Script /{bad=1} END {exit !bad}' "$TMP/$view.txt"; then
    echo "Read-only $view overflowed 40 columns" >&2
    awk 'length($0)>40 {printf "LENGTH=%s CONTENT=%s\n", length($0), $0}' "$TMP/$view.txt" >&2
    exit 1
  fi
done
grep -Fq 'Service status' "$TMP/status.out"
grep -Fq 'MCP endpoint' "$TMP/status.out"
grep -Fq 'INFO' "$TMP/log.out"
grep -Fq 'SUCCESS' "$TMP/log.out"
grep -Fq 'WARNING' "$TMP/log.out"

# Invalid dynamic terminal bytes are stripped even when they originate in
# config/metadata and render within the contextual header.
printf '0\n' | ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" ASA_LABEL_WITH_CONTROL=1 NO_COLOR=1 TERM=dumb COLUMNS=60 LINES=24 \
  script -q -e -c "bash '$TMP/fixture.sh' main" /dev/null > "$TMP/control-label.out"
! LC_ALL=C grep -q $'\033' "$TMP/control-label.out"
grep -Fq 'QA Console' "$TMP/control-label.out"

# A curl-piped installer keeps a controlling TTY on stdout/stderr but not
# stdin. Both installer and manager must still find the real /dev/tty without
# mistaking /dev/tty mode bits for an actual terminal in unattended CI.
cat > "$TMP/tty-from-pipe.sh" <<'PTY_FIXTURE'
#!/usr/bin/env bash
set -Eeuo pipefail
[ ! -t 0 ] || { echo 'EXPECTED_STDIN_PIPE'; exit 11; }
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ASA_SOURCE"
ui_has_interactive_tty || { echo 'MANAGER_MISSED_TTY'; exit 12; }
source "$ASA_TMP/only-installer-tty.sh"
installer_has_interactive_tty || { echo 'INSTALLER_MISSED_TTY'; exit 13; }
printf 'PIPE_WITH_CONTROLLING_TTY_PASS\n'
PTY_FIXTURE
sed -n '/^installer_has_interactive_tty(){/,/^}/p' "$ROOT/install.sh" > "$TMP/only-installer-tty.sh"
chmod 700 "$TMP/tty-from-pipe.sh"
ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" NO_COLOR=1 TERM=dumb \
  script -q -e -c "printf 'stdin bytes' | bash '$TMP/tty-from-pipe.sh'" /dev/null < /dev/null > "$TMP/piped-tty.out" 2>&1
grep -Fq 'PIPE_WITH_CONTROLLING_TTY_PASS' "$TMP/piped-tty.out"

# A No at the actual pre-commit confirmation must leave both config and
# service state untouched (unlike a callback mocked to return false).
printf '3210\nn\n' | ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" NO_COLOR=1 TERM=dumb \
  script -q -e -c "bash '$TMP/fixture.sh' local" /dev/null > "$TMP/local-no.out" 2>&1 || local_no_rc=$?
[ "${local_no_rc:-20}" -eq 20 ]
grep -Fq 'Cancelled before local connection changes.' "$TMP/local-no.out"
! grep -Eq 'CONFIG_MUTATED|SERVICES_RESTARTED|MANAGED_STATE_MUTATED' "$TMP/local-no.out"

printf '3210\ny\n' | ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" NO_COLOR=1 TERM=dumb \
  script -q -e -c "bash '$TMP/fixture.sh' local" /dev/null > "$TMP/local-yes.out"
grep -Fq 'CONFIG_MUTATED=local/3210' "$TMP/local-yes.out"
grep -Fq 'SERVICES_RESTARTED' "$TMP/local-yes.out"
grep -Fq 'MANAGED_STATE_MUTATED' "$TMP/local-yes.out"

# Simulate a protected Cloudflare preflight that completes all READ-only
# discovery, then cancel before the first remote/local journal write.
printf 'n\n' | AI_SERVER_AGENT_HOSTNAME=mcp.example.com ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" NO_COLOR=1 TERM=dumb \
  script -q -e -c "bash '$TMP/fixture.sh' cloudflare" /dev/null > "$TMP/cloudflare-no.out" 2>&1 || cloudflare_no_rc=$?
[ "${cloudflare_no_rc:-20}" -eq 20 ]
grep -Fq 'TEST_CLOUDFLARE_READ_ONLY_GUIDANCE' "$TMP/cloudflare-no.out"
grep -Fq 'Cancelled before Cloudflare changes.' "$TMP/cloudflare-no.out"
! grep -Fq 'UNSAFE_CLOUDFLARE_WRITE' "$TMP/cloudflare-no.out"

# A valid temporary certificate is inspected, but pressing No must not
# stage/copy private TLS material or restart the service.
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$TMP/tls.key" -out "$TMP/tls.crt" \
  -subj '/CN=mcp.example.com' -addext 'subjectAltName=DNS:mcp.example.com' -days 1 >/dev/null 2>&1
printf 'mcp.example.com\n3210\n%s\n%s\nn\n' "$TMP/tls.crt" "$TMP/tls.key" | ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" NO_COLOR=1 TERM=dumb \
  script -q -e -c "bash '$TMP/fixture.sh' tls" /dev/null > "$TMP/tls-no.out" 2>&1 || tls_no_rc=$?
[ "${tls_no_rc:-20}" -eq 20 ]
grep -Fq 'Cancelled before TLS files' "$TMP/tls-no.out"
! grep -Eq 'CONFIG_MUTATED|SERVICES_RESTARTED|MANAGED_STATE_MUTATED' "$TMP/tls-no.out"

# Menu-level confirmation must prevent accidental update/repair entirely.
printf '5\nn\n\n6\nn\n\n0\n' | ASA_SOURCE="$ROOT/manage.sh" ASA_TMP="$TMP" NO_COLOR=1 TERM=dumb \
  script -q -e -c "bash '$TMP/fixture.sh' main" /dev/null > "$TMP/operations-no.out"
grep -Fq 'Cancelled; update not started.' "$TMP/operations-no.out"
grep -Fq 'Cancelled; repair not started.' "$TMP/operations-no.out"
! grep -Eq 'UNSAFE_(UPDATE|REPAIR)_STARTED' "$TMP/operations-no.out"

# Exercise standalone lifecycle helper error output without privileges or
# installed services. The fake id prevents even a root CI runner proceeding.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/id" <<'FAKE_ID'
#!/bin/sh
if [ "${1:-}" = '-u' ]; then printf '1001\n'; else /usr/bin/id "$@"; fi
FAKE_ID
chmod 755 "$TMP/bin/id"
for helper in update.sh uninstall.sh; do
  if PATH="$TMP/bin:$PATH" NO_COLOR=1 TERM=dumb bash "$ROOT/$helper" > "$TMP/$helper.err" 2>&1; then
    echo "Non-root lifecycle helper accepted an unsafe call: $helper" >&2; exit 1
  fi
  grep -Fq '[ai-server-agent] ERROR: Run as root' "$TMP/$helper.err"
  ! LC_ALL=C grep -q $'\033' "$TMP/$helper.err"
done
if PATH="$TMP/bin:$PATH" TERM=xterm NO_COLOR= script -q -e -c "bash '$ROOT/uninstall.sh'" /dev/null < /dev/null > "$TMP/uninstall-color.out" 2>&1; then
  echo 'Non-root interactive uninstall unexpectedly proceeded' >&2; exit 1
fi
LC_ALL=C grep -Fq "$(printf '\033[31m')" "$TMP/uninstall-color.out"

# Installer --check-platform is an offline entrypoint. Exercise its
# real terminal error presentation without invoking apt/systemctl/root.
if NO_COLOR=1 TERM=dumb bash "$ROOT/install.sh" --check-platform ubuntu 24.04 unsupported > "$TMP/install-plain.out" 2>&1; then
  echo 'Invalid platform was accepted' >&2; exit 1
fi
grep -Fq '[ai-server-agent] ERROR: Unsupported architecture' "$TMP/install-plain.out"
! LC_ALL=C grep -q $'\033' "$TMP/install-plain.out"
if TERM=xterm script -q -e -c "bash '$ROOT/install.sh' --check-platform ubuntu 24.04 unsupported" /dev/null < /dev/null > "$TMP/install-color.out" 2>&1; then
  echo 'Invalid platform was accepted with TTY' >&2; exit 1
fi
LC_ALL=C grep -Fq "$(printf '\033[31m')" "$TMP/install-color.out"

# Check public management help without accessing installed services/secrets.
help_text="$(AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=0 COLUMNS=100 NO_COLOR=1 bash "$ROOT/manage.sh" help)"
grep -Fq 'Usage: sudo ai-server-agent-manage' <<< "$help_text"
grep -Fq 'Connection and lifecycle' <<< "$help_text"

# A provider-supplied control byte must never reach either TTY or
# redirected log output, including warnings and failures.
ASA_SOURCE="$ROOT/manage.sh" bash -c '
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1 NO_COLOR=1
  source "$ASA_SOURCE"
  log $'"'"'ordinary info\033[31m hostile'"'"'
  warn $'"'"'provider warning\033]0;injected\007'"'"'
  if (die $'"'"'failure\033[2J concealed'"'"') 2> /dev/null; then exit 21; fi
' > "$TMP/redirect-controls.out" 2>&1
! LC_ALL=C grep -q $'\033' "$TMP/redirect-controls.out"
grep -Fq '[ai-server-agent] WARNING:' "$TMP/redirect-controls.out"

# Control-sequence stripping must not corrupt printable UTF-8 labels.
ASA_SOURCE="$ROOT/manage.sh" bash -c '
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$ASA_SOURCE"
  value="$(ui_safe_value $'"'"'دفتر آزمایش\033[31mنام\033[0m'"'"')"
  [[ "$value" == *"دفتر آزمایش"* ]]
  [[ "$value" != *$'"'"'\033'"'"'* ]]
  [[ "$value" == *"نام"* ]]
'
echo 'Operator UI narrow terminal/color/no-color/UTF-8/real precommit No-and-Yes PASS'
