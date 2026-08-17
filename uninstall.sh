#!/usr/bin/env bash
set -Eeuo pipefail

[ "$(id -u)" -eq 0 ] || { echo "Run as root" >&2; exit 1; }
PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

LIFECYCLE_LOCK_DIR=/run/lock/ai-server-agent
LIFECYCLE_LOCK="$LIFECYCLE_LOCK_DIR/management.lock"
acquire_lifecycle_lock(){
  command -v flock >/dev/null 2>&1 || { echo "flock is required for privileged lifecycle serialization" >&2; exit 1; }
  [ ! -L "$LIFECYCLE_LOCK_DIR" ] || { echo "Refusing symlinked lifecycle lock directory: $LIFECYCLE_LOCK_DIR" >&2; exit 1; }
  install -d -o root -g root -m 0700 "$LIFECYCLE_LOCK_DIR"
  chown root:root "$LIFECYCLE_LOCK_DIR"; chmod 0700 "$LIFECYCLE_LOCK_DIR"
  [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK_DIR" 2>/dev/null)" = "0:0:700" ] || { echo "Lifecycle lock directory ownership/mode is unsafe" >&2; exit 1; }
  [ ! -L "$LIFECYCLE_LOCK" ] || { echo "Refusing symlinked lifecycle lock: $LIFECYCLE_LOCK" >&2; exit 1; }
  ( umask 077; : >> "$LIFECYCLE_LOCK" )
  [ -f "$LIFECYCLE_LOCK" ] && [ ! -L "$LIFECYCLE_LOCK" ] || { echo "Lifecycle lock is not a regular file" >&2; exit 1; }
  chown root:root "$LIFECYCLE_LOCK"; chmod 0600 "$LIFECYCLE_LOCK"
  [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK" 2>/dev/null)" = "0:0:600" ] || { echo "Lifecycle lock ownership/mode is unsafe" >&2; exit 1; }
  if [ "$(readlink /proc/$$/fd/9 2>/dev/null || true)" = "$LIFECYCLE_LOCK" ]; then
    flock -n 9 || { echo "Another AI Server Agent management operation is already active. Retry after it finishes." >&2; exit 1; }
    return 0
  fi
  exec 9>>"$LIFECYCLE_LOCK"
  flock -n 9 || { echo "Another AI Server Agent management operation is already active. Retry after it finishes." >&2; exit 1; }
}

if [ "${AI_SERVER_AGENT_YES:-0}" != "1" ]; then
  read -r -p "Remove AI Server Agent services, binary, and management command? [y/N] " ans
  [[ "$ans" =~ ^[Yy]$ ]] || exit 0
fi

acquire_lifecycle_lock

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
    echo "Lifecycle lock namespace was lost during purge; refusing to report a clean purge." >&2
    exit 1
  }
  echo "Purged Agent-owned config, state, logs, optional runtimes, management helpers, and the aiagent service account/group. Workspace /srv/ai-workspace and its aiworker account/group were intentionally preserved."
else
  echo "Agent services, binary, management command, and installed helper scripts removed. Config, state, logs, optional runtimes, users, groups, and workspace were preserved. Reinstall to restore management, or use --purge to remove Agent-owned state/runtime; aiworker/workspace remain protected."
fi
