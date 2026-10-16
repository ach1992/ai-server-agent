#!/usr/bin/env bash
set -Eeuo pipefail

# Standalone lifecycle helpers stay safe for scripts: no ANSI unless TTY,
# always include a textual severity and preserve the command exit status.
cli_message(){
  local severity="$1" color="" message
  shift
  message="$(printf '%s' "$*" | LC_ALL=C tr -d '\000-\037\177')"
  if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-dumb}" != dumb ]; then
    case "$severity" in
      SUCCESS) color=$'\033[32m' ;;
      WARNING) color=$'\033[33m' ;;
      ERROR) color=$'\033[31m' ;;
    esac
    printf '%s%s:%s %s\n' "$color" "$severity" $'\033[0m' "$message"
  else
    printf '[ai-server-agent] %s: %s\n' "$severity" "$message"
  fi
}
cli_error(){ cli_message ERROR "$*" >&2; }
cli_success(){ cli_message SUCCESS "$*"; }

[ "$(id -u)" -eq 0 ] || { cli_error "Run as root (for example: sudo bash uninstall.sh)."; exit 1; }
PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

LIFECYCLE_LOCK_DIR=/run/lock/ai-server-agent
LIFECYCLE_LOCK="$LIFECYCLE_LOCK_DIR/management.lock"
acquire_lifecycle_lock(){
  command -v flock >/dev/null 2>&1 || { cli_error "flock is required for privileged lifecycle serialization"; exit 1; }
  [ ! -L "$LIFECYCLE_LOCK_DIR" ] || { cli_error "Refusing symlinked lifecycle lock directory: $LIFECYCLE_LOCK_DIR"; exit 1; }
  install -d -o root -g root -m 0700 "$LIFECYCLE_LOCK_DIR"
  chown root:root "$LIFECYCLE_LOCK_DIR"; chmod 0700 "$LIFECYCLE_LOCK_DIR"
  [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK_DIR" 2>/dev/null)" = "0:0:700" ] || { cli_error "Lifecycle lock directory ownership/mode is unsafe"; exit 1; }
  [ ! -L "$LIFECYCLE_LOCK" ] || { cli_error "Refusing symlinked lifecycle lock: $LIFECYCLE_LOCK"; exit 1; }
  ( umask 077; : >> "$LIFECYCLE_LOCK" )
  [ -f "$LIFECYCLE_LOCK" ] && [ ! -L "$LIFECYCLE_LOCK" ] || { cli_error "Lifecycle lock is not a regular file"; exit 1; }
  chown root:root "$LIFECYCLE_LOCK"; chmod 0600 "$LIFECYCLE_LOCK"
  [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK" 2>/dev/null)" = "0:0:600" ] || { cli_error "Lifecycle lock ownership/mode is unsafe"; exit 1; }
  if [ "$(readlink /proc/$$/fd/9 2>/dev/null || true)" = "$LIFECYCLE_LOCK" ]; then
    flock -n 9 || { cli_error "Another AI Server Agent management operation is already active. Retry after it finishes."; exit 1; }
    return 0
  fi
  exec 9>>"$LIFECYCLE_LOCK"
  flock -n 9 || { cli_error "Another AI Server Agent management operation is already active. Retry after it finishes."; exit 1; }
}

if [ "${AI_SERVER_AGENT_YES:-0}" != "1" ]; then
  [ -t 0 ] && [ -r /dev/tty ] || { cli_error 'Uninstall/purge confirmation requires a terminal. For an explicitly authorized scripted operation, use AI_SERVER_AGENT_YES=1.'; exit 1; }
  if [ "$PURGE" -eq 1 ]; then
    prompt='PURGE Agent-owned config, state, logs and runtime, plus aiagent identity? Workspace and aiworker are preserved. [y/N] '
  else
    prompt='Remove Agent services/binaries only, preserving config, state, users and workspace? [y/N] '
  fi
  ans=''
  IFS= read -r -p "$prompt" ans </dev/tty || exit 20
  [[ "$ans" =~ ^[Yy]$ ]] || { cli_message INFO 'Cancelled; no uninstall/purge started.'; exit 0; }
fi

acquire_lifecycle_lock

if ! active_jobs="$(systemctl list-units --type=service --state=activating,active,deactivating,reloading --no-legend --no-pager 'ai-job-*.service' 2>/dev/null)"; then
  cli_error "Unable to determine whether AI Server Agent persistent jobs are active; refusing uninstall."
  exit 1
fi
if [ -n "$(printf '%s\n' "$active_jobs" | sed '/^[[:space:]]*$/d')" ]; then
  cli_error "Active AI Server Agent persistent jobs exist; stop them before uninstall or purge to avoid orphaning host work."
  printf '%s\n' "$active_jobs" >&2
  exit 1
fi

systemctl disable --now ai-server-agent.service ai-server-agent-executor.service 2>/dev/null || true
rm -f /etc/systemd/system/ai-server-agent.service /etc/systemd/system/ai-server-agent-executor.service
rm -f /usr/local/bin/ai-server-agent /usr/local/sbin/ai-server-agent-manage
rm -rf /usr/local/lib/ai-server-agent
systemctl daemon-reload

if [ "$PURGE" -eq 1 ]; then
  rm -rf /etc/ai-server-agent /var/lib/ai-server-agent /var/log/ai-server-agent /opt/ai-server-agent
  userdel aiagent 2>/dev/null || true
  groupdel aiagent 2>/dev/null || true
  [ -f "$LIFECYCLE_LOCK" ] && [ ! -L "$LIFECYCLE_LOCK" ] && [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK" 2>/dev/null)" = "0:0:600" ] || {
    cli_error "Lifecycle lock namespace was lost during purge; refusing to report a clean purge."
    exit 1
  }
  cli_success "Purged Agent-owned config, state, logs, optional runtimes, management helpers, and the aiagent service account/group. Workspace /srv/ai-workspace and its aiworker account/group were intentionally preserved."
else
  cli_success "Agent services, binary, management command, and installed helper scripts removed. Config, state, logs, optional runtimes, users, groups, and workspace were preserved. Reinstall to restore management, or use --purge to remove Agent-owned state/runtime; aiworker/workspace remain protected."
fi
