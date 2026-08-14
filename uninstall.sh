#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Run as root" >&2; exit 1; }
PURGE=0; [ "${1:-}" = "--purge" ] && PURGE=1
if [ "${AI_SERVER_AGENT_YES:-0}" != "1" ]; then
  read -r -p "Remove AI Server Agent services and binary? [y/N] " ans
  [[ "$ans" =~ ^[Yy]$ ]] || exit 0
fi
systemctl disable --now ai-server-agent.service ai-server-agent-executor.service 2>/dev/null || true
rm -f /etc/systemd/system/ai-server-agent.service /etc/systemd/system/ai-server-agent-executor.service /usr/local/bin/ai-server-agent
systemctl daemon-reload
if [ "$PURGE" -eq 1 ]; then
  rm -rf /etc/ai-server-agent /var/lib/ai-server-agent /var/log/ai-server-agent /opt/ai-server-agent
  userdel aiagent 2>/dev/null || true
  echo "Purged agent config, state, logs, optional runtimes, and the aiagent service account. Workspace /srv/ai-workspace and its aiworker account were intentionally preserved."
else
  echo "Agent services and binary removed. Config, state, logs, optional runtimes, users, and workspace were preserved. Use --purge to remove agent-owned state/runtime and the aiagent service account; aiworker/workspace remain protected."
fi
