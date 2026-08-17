#!/usr/bin/env bash
set -Eeuo pipefail

[ "$(id -u)" -eq 0 ] || { echo "Run as root" >&2; exit 1; }
PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

if [ "${AI_SERVER_AGENT_YES:-0}" != "1" ]; then
  read -r -p "Remove AI Server Agent services, binary, and management command? [y/N] " ans
  [[ "$ans" =~ ^[Yy]$ ]] || exit 0
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
  echo "Purged Agent-owned config, state, logs, optional runtimes, management helpers, and the aiagent service account/group. Workspace /srv/ai-workspace and its aiworker account/group were intentionally preserved."
else
  echo "Agent services, binary, management command, and installed helper scripts removed. Config, state, logs, optional runtimes, users, groups, and workspace were preserved. Reinstall to restore management, or use --purge to remove Agent-owned state/runtime; aiworker/workspace remain protected."
fi
