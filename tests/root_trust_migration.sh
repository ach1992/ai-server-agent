#!/usr/bin/env bash
set -Eeuo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run root_trust_migration.sh as root" >&2; exit 1; }
BINARY="${AI_SERVER_AGENT_BINARY:?set AI_SERVER_AGENT_BINARY to the built test binary}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR=/var/lib/ai-server-agent
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

getent group aiagent >/dev/null 2>&1 || groupadd --system aiagent
getent group aiworker >/dev/null 2>&1 || groupadd --system aiworker
id aiagent >/dev/null 2>&1 || useradd --system --gid aiagent --home "$STATE_DIR" --shell /usr/sbin/nologin aiagent
id aiworker >/dev/null 2>&1 || useradd --system --gid aiworker --create-home --home-dir /srv/ai-workspace --shell /bin/bash aiworker
install -d -m 0711 -o aiagent -g aiagent "$STATE_DIR"
install -d -m 0750 -o aiworker -g aiworker "$STATE_DIR/runtime"
install -d -m 0755 -o root -g root "$scratch/browser-target" "$scratch/jobs-target"
printf 'manifest-sentinel\n' > "$scratch/manifest-target"
printf 'browser-sentinel\n' > "$scratch/browser-target/sentinel"
printf 'jobs-sentinel\n' > "$scratch/jobs-target/sentinel"
ln -s "$scratch/browser-target" "$STATE_DIR/runtime/browser"
ln -s "$scratch/jobs-target" "$STATE_DIR/jobs"
ln -s "$scratch/manifest-target" "$STATE_DIR/AI_ENVIRONMENT.json"
cat > "$STATE_DIR/install-state.env" <<'LEGACY'
CHANNEL=source
VERSION=$(touch /tmp/ai-server-agent-legacy-state-executed)
REF=main
TRACK_REF=main
LEGACY
chown aiagent:aiagent "$STATE_DIR/install-state.env"
rm -f /tmp/ai-server-agent-legacy-state-executed

AI_SERVER_AGENT_BINARY="$BINARY" AI_SERVER_AGENT_NONINTERACTIVE=1 bash "$ROOT/install.sh"

test ! -e /tmp/ai-server-agent-legacy-state-executed
test ! -L "$STATE_DIR/runtime"
test ! -L "$STATE_DIR/jobs"
test ! -L "$STATE_DIR/AI_ENVIRONMENT.json"
test -d "$STATE_DIR/runtime" && test -d "$STATE_DIR/jobs"
test ! -e "$STATE_DIR/runtime/browser"
test "$(stat -c '%U:%G:%a' "$STATE_DIR")" = root:root:711
test "$(stat -c '%U:%G:%a' "$STATE_DIR/runtime")" = root:root:711
test "$(stat -c '%U:%G:%a' "$STATE_DIR/jobs")" = root:root:711
test "$(stat -c '%U:%G:%a' "$STATE_DIR/AI_ENVIRONMENT.json")" = aiagent:aiagent:640
test "$(stat -c '%a' "$scratch/browser-target")" = 755
test "$(stat -c '%a' "$scratch/jobs-target")" = 755
grep -qF browser-sentinel "$scratch/browser-target/sentinel"
grep -qF jobs-sentinel "$scratch/jobs-target/sentinel"
grep -qF manifest-sentinel "$scratch/manifest-target"
test ! -e "$STATE_DIR/install-state.env"
jq -e '.channel=="source" and .version=="source"' /etc/ai-server-agent/control/install-state.json >/dev/null

echo "root trust migration tests passed"
