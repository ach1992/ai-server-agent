#!/usr/bin/env bash
set -Eeuo pipefail

# Real hostile NSS accounts/groups, ONLY on an opted-in disposable GitHub CI
# runner, BEFORE initial Agent installation. Never run on an occupied host.
[ "$(id -u)" -eq 0 ] &&
[ "${GITHUB_ACTIONS:-}" = true ] &&
[ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] &&
[ "${ASA_DISPOSABLE_BROWSER_ALIAS_ACCEPTANCE:-}" = 1 ] || {
  echo 'Browser alias host fixture requires disposable GitHub runner' >&2
  exit 1
}
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="${AI_SERVER_AGENT_BINARY:?trusted candidate binary required}"
STATE=/var/lib/ai-server-agent
HOME_PATH="$STATE/browser-home"
for account in aiagent aiworker; do id "$account" >/dev/null; done
test ! -e /etc/ai-server-agent &&
test ! -e /usr/local/bin/ai-server-agent &&
test ! -e "$HOME_PATH" &&
! id aibrowser >/dev/null 2>&1 &&
! getent group aibrowser >/dev/null 2>&1 || {
  echo 'Browser alias fixture requires virgin Agent and Browser identity' >&2; exit 1
}
scratch="$(mktemp -d /var/tmp/asa-browser-alias.XXXXXXXX)"
created_user=0
created_group=0
cleanup_case(){
  if [ "$created_user" -eq 1 ]; then
    local record
    record="$(getent passwd aibrowser)" || return 1
    [ "$(printf '%s' "$record" | cut -d: -f6-7)" = "$HOME_PATH:/usr/sbin/nologin" ] || return 1
    userdel aibrowser || return 1
    created_user=0
  fi
  if [ "$created_group" -eq 1 ]; then
    groupdel -f aibrowser || return 1
    created_group=0
  fi
}
finish(){
  local rc="$?"
  trap - EXIT
  cleanup_case || rc=1
  rm -rf -- "$scratch"
  exit "$rc"
}
trap finish EXIT
make_group(){
  case "$1" in
    default) groupadd --system aibrowser ;;
    agent) groupadd --non-unique --gid "$(id -g aiagent)" aibrowser ;;
    worker) groupadd --non-unique --gid "$(id -g aiworker)" aibrowser ;;
    *) exit 3 ;;
  esac
  created_group=1
}
make_user(){
  case "$1" in
    normal) useradd --system --gid aibrowser --no-create-home --home-dir "$HOME_PATH" --shell /usr/sbin/nologin aibrowser ;;
    agent) useradd --non-unique --uid "$(id -u aiagent)" --gid aibrowser --no-create-home --home-dir "$HOME_PATH" --shell /usr/sbin/nologin aibrowser ;;
    worker) useradd --non-unique --uid "$(id -u aiworker)" --gid aibrowser --no-create-home --home-dir "$HOME_PATH" --shell /usr/sbin/nologin aibrowser ;;
    *) exit 3 ;;
  esac
  created_user=1
}
expect_refused(){
  local label="$1" expected="$2" result=0 before_passwd before_group
  before_passwd="$(getent passwd aibrowser || true)"
  before_group="$(getent group aibrowser || true)"
  env AI_SERVER_AGENT_BINARY="$BINARY" AI_SERVER_AGENT_NONINTERACTIVE=1 \
    bash "$ROOT/install.sh" > "$scratch/refusal.log" 2>&1 || result=$?
  if [ "$result" -eq 0 ] || ! grep -qF "$expected" "$scratch/refusal.log"; then
    echo "Browser identity refusal wrong: $label (exit $result)" >&2
    grep -E 'Browser|ERROR' "$scratch/refusal.log" | tail -n 12 >&2 || :
    exit 1
  fi
  [ "$(getent passwd aibrowser || true)" = "$before_passwd" ] &&
  [ "$(getent group aibrowser || true)" = "$before_group" ] &&
  test ! -e /etc/ai-server-agent/config.json &&
  test ! -e /etc/ai-server-agent/mcp-credentials.json &&
  test ! -e /usr/local/bin/ai-server-agent &&
  test ! -e "$HOME_PATH" &&
  test -L "$STATE/runtime/browser" &&
  test -L "$STATE/jobs" &&
  test -L "$STATE/AI_ENVIRONMENT.json" &&
  test ! -e /tmp/ai-server-agent-legacy-state-executed || {
    echo "Browser alias refusal mutated protected Agent state: $label" >&2; exit 1
  }
  echo "PASS disposable Browser numeric alias refused safely: $label"
}
run_case(){
  local label="$1" group_kind="$2" user_kind="$3" expected="$4"
  make_group "$group_kind"
  if [ "$user_kind" != group-only ]; then make_user "$user_kind"; fi
  expect_refused "$label" "$expected"
  cleanup_case
}
run_case 'UID aliases aiagent' default agent 'Browser UID aliases protected aiagent'
run_case 'UID aliases aiworker' default worker 'Browser UID aliases protected aiworker'
run_case 'GID aliases aiagent' agent normal 'Browser GID aliases protected aiagent'
run_case 'GID aliases aiworker' worker normal 'Browser GID aliases protected aiworker'
run_case 'group-only GID aliases aiagent' agent group-only 'Browser GID aliases protected aiagent'
echo 'disposable Browser alias refusals preserved accounts and Agent credentials'
