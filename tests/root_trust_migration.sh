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
# A symlinked LEGACY browser parent is removed without following it, then
# safely replaced by the new root-owned container for the isolated Browser.
test ! -L "$STATE_DIR/runtime/browser"
test "$(stat -c '%U:%G:%a' "$STATE_DIR/runtime/browser")" = root:root:755
test "$(stat -c '%U:%G:%a' "$STATE_DIR/runtime/browser/isolated")" = aibrowser:aibrowser:700
test "$(stat -c '%U:%G:%a' "$STATE_DIR/browser-home")" = aibrowser:aibrowser:700
test "$(getent passwd aibrowser | cut -d: -f6-7)" = "$STATE_DIR/browser-home:/usr/sbin/nologin"
test "$(id -G aibrowser)" = "$(id -g aibrowser)"
test "$(id -u aibrowser)" != "$(id -u aiworker)"
test "$(id -g aibrowser)" != "$(id -g aiworker)"
# Account-scoped files are invisible to an otherwise authorized worker UID.
runuser -u aibrowser -- /usr/bin/sh -c 'printf private > /var/lib/ai-server-agent/runtime/browser/isolated/private.marker'
if runuser -u aiworker -- /usr/bin/cat "$STATE_DIR/runtime/browser/isolated/private.marker" >/dev/null 2>&1; then
  echo "isolated Browser data readable from worker UID" >&2
  exit 1
fi

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

# A legacy real Browser data parent can survive an ordinary update with
# aiworker:aiworker 0750. That blocks aiagent from even lstat'ing the
# worker-private profile and makes browser_status report it missing/unsafe.
# Repair only the exact Agent-owned parent while preserving profile bytes and
# worker ownership/mode.
install -d -m 0750 -o aiworker -g aiworker "$STATE_DIR/runtime/browser"
install -d -m 0700 -o aiworker -g aiworker "$STATE_DIR/runtime/browser/profile"
printf 'browser-profile-sentinel\n' > "$STATE_DIR/runtime/browser/profile/sentinel"
chown aiworker:aiworker "$STATE_DIR/runtime/browser/profile/sentinel"
AI_SERVER_AGENT_BINARY="$BINARY" AI_SERVER_AGENT_NONINTERACTIVE=1 bash "$ROOT/install.sh"

test "$(stat -c '%U:%G:%a' "$STATE_DIR/runtime/browser")" = root:root:755
test "$(stat -c '%U:%G:%a' "$STATE_DIR/runtime/browser/profile")" = aiworker:aiworker:700
test "$(stat -c '%U:%G' "$STATE_DIR/runtime/browser/profile/sentinel")" = aiworker:aiworker
grep -qF browser-profile-sentinel "$STATE_DIR/runtime/browser/profile/sentinel"

echo "root trust migration tests passed"
