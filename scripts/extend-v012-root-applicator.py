from pathlib import Path

p = Path('scripts/apply-v012-root-trust-boundary.py')
text = p.read_text()
marker = "# ROOT_TRUST_EXTENSION_V2"
if marker in text:
    raise SystemExit('root trust extension already present')
extension = r'''

# ROOT_TRUST_EXTENSION_V2
# The MCP service needs to refresh only the informational manifest. Keep the
# state/container directory entries root-controlled and whitelist exactly that
# pre-created untrusted output file for the network-facing service.
replace_once(
    "install.sh",
    'install -d -m 0750 -o root -g "$AGENT_USER" "$CONFIG_DIR"\ninstall -d -m 0700 -o root -g root "$CONTROL_DIR"\ninstall -d -m 0711 -o root -g root "$STATE_DIR"\ninstall -d -m 2750 -o root -g "$AGENT_USER" "$LOG_DIR"\ninstall -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$WORKSPACE_DIR"\ninstall -d -m 0711 -o root -g root "$STATE_DIR/runtime"\ninstall -d -m 0711 -o root -g root "$STATE_DIR/jobs"\ninstall -d -m 0755 -o root -g root "$LIB_DIR"\n',
    '''install -d -m 0750 -o root -g "$AGENT_USER" "$CONFIG_DIR"
install -d -m 0700 -o root -g root "$CONTROL_DIR"
[ ! -L "$STATE_DIR" ] || die "Refusing a symlinked state directory: $STATE_DIR"
install -d -m 0711 -o root -g root "$STATE_DIR"
install -d -m 2750 -o root -g "$AGENT_USER" "$LOG_DIR"
install -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$WORKSPACE_DIR"
secure_state_container(){
  local path="$1"
  if [ -L "$path" ]; then
    warn "Removing unsafe legacy state-container symlink without following it: $path"
    rm -f -- "$path"
  fi
  if [ -e "$path" ] && [ ! -d "$path" ]; then
    die "State container is not a real directory: $path"
  fi
  install -d -m 0711 -o root -g root "$path"
}
secure_state_container "$STATE_DIR/runtime"
secure_state_container "$STATE_DIR/jobs"
# AI_ENVIRONMENT.json is informational output written by aiagent. Its parent is
# root-controlled, so the service can update the file but cannot replace the
# directory entry with a symlink or another inode.
rm -f -- "$STATE_DIR/AI_ENVIRONMENT.json"
install -o "$AGENT_USER" -g "$AGENT_USER" -m 0640 /dev/null "$STATE_DIR/AI_ENVIRONMENT.json"
install -d -m 0755 -o root -g root "$LIB_DIR"
''',
)
replace_once(
    "install.sh",
    'ProtectHome=true\nUMask=0027\n',
    'ProtectHome=true\nReadWritePaths=$STATE_DIR/AI_ENVIRONMENT.json\nUMask=0027\n',
)

# Tighten the behavioral trust-boundary test: only the manifest file may be
# writable by aiagent; its directory entry and every root-trusted control file
# remain non-replaceable. Also prove old shell-style state is data, never code.
replace_once(
    "tests/root_trust_boundary.sh",
    '''# The network-facing service has no writable exception for the state hierarchy.
if systemctl show ai-server-agent.service -p ReadWritePaths --value | grep -qF "$STATE_DIR"; then
  echo "aiagent service still has a writable state-dir exception" >&2
  exit 1
fi
''',
    '''# The network-facing service has exactly one writable runtime output file,
# not a writable state-directory hierarchy.
readwrite_paths="$(systemctl show ai-server-agent.service -p ReadWritePaths --value)"
[ "$readwrite_paths" = "$STATE_DIR/AI_ENVIRONMENT.json" ] || {
  echo "unexpected aiagent ReadWritePaths: $readwrite_paths" >&2
  exit 1
}
assert_owner_mode "$STATE_DIR/AI_ENVIRONMENT.json" aiagent aiagent 640
expect_denied aiagent rm -f "$STATE_DIR/AI_ENVIRONMENT.json"
expect_denied aiagent mv "$STATE_DIR/AI_ENVIRONMENT.json" "$STATE_DIR/AI_ENVIRONMENT.replaced"
expect_denied aiagent ln -sfn /tmp/attacker "$STATE_DIR/AI_ENVIRONMENT.json"
''',
)
replace_once(
    "tests/root_trust_boundary.sh",
    '''state_hash="$(sha256sum "$INSTALL_STATE" | awk '{print $1}')"
expect_denied aiagent rm -f "$INSTALL_STATE"
expect_denied aiagent mv "$INSTALL_STATE" "$CONTROL_DIR/install-state.replaced"
expect_denied aiagent ln -sfn /tmp/attacker "$INSTALL_STATE"
expect_denied aiagent touch "$CONTROL_DIR/attacker"
expect_denied aiagent sh -c "printf attacker > '$INSTALL_STATE'"
[ "$(sha256sum "$INSTALL_STATE" | awk '{print $1}')" = "$state_hash" ]
''',
    '''state_hash="$(sha256sum "$INSTALL_STATE" | awk '{print $1}')"
for user in aiagent aiworker; do
  expect_denied "$user" rm -f "$INSTALL_STATE"
  expect_denied "$user" mv "$INSTALL_STATE" "$CONTROL_DIR/install-state.replaced"
  expect_denied "$user" ln -sfn /tmp/attacker "$INSTALL_STATE"
  expect_denied "$user" touch "$CONTROL_DIR/attacker"
  expect_denied "$user" sh -c "printf attacker > '$INSTALL_STATE'"
done
[ "$(sha256sum "$INSTALL_STATE" | awk '{print $1}')" = "$state_hash" ]
''',
)
replace_once(
    "tests/root_trust_boundary.sh",
    '''journal_hash="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"
expect_denied aiagent rm -f "$CF_TXN_STATE"
expect_denied aiagent mv "$CF_TXN_STATE" "$CONTROL_DIR/cloudflare-transaction.replaced"
expect_denied aiagent ln -sfn /tmp/attacker "$CF_TXN_STATE"
expect_denied aiagent sh -c "printf attacker > '$CF_TXN_STATE'"
[ "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$journal_hash" ]
rm -f "$CF_TXN_STATE"

echo "root trust-boundary tests passed"
''',
    '''journal_hash="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"
for user in aiagent aiworker; do
  expect_denied "$user" rm -f "$CF_TXN_STATE"
  expect_denied "$user" mv "$CF_TXN_STATE" "$CONTROL_DIR/cloudflare-transaction.replaced"
  expect_denied "$user" ln -sfn /tmp/attacker "$CF_TXN_STATE"
  expect_denied "$user" sh -c "printf attacker > '$CF_TXN_STATE'"
done
[ "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$journal_hash" ]
rm -f "$CF_TXN_STATE"

# Old shell-style metadata and malformed custom state are never executed.
injection_marker=/tmp/ai-server-agent-state-injection-marker
malicious_state="$(mktemp)"
rm -f "$injection_marker"
cat > "$malicious_state" <<'MALICIOUS'
CHANNEL=stable
VERSION=$(touch /tmp/ai-server-agent-state-injection-marker)
REF=v9.9.9
TRACK_REF=v9.9.9
MALICIOUS
if AI_SERVER_AGENT_INSTALL_STATE="$malicious_state" /usr/local/lib/ai-server-agent/update.sh --plan >/tmp/invalid-install-state.out 2>&1; then
  echo "shell-style install state was accepted" >&2
  exit 1
fi
test ! -e "$injection_marker"
MALICIOUS_STATE="$malicious_state" MANAGE="$MANAGE" INJECTION_MARKER="$injection_marker" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$MANAGE"
  INSTALL_STATE="$MALICIOUS_STATE"
  installed_identity >/tmp/invalid-managed-identity.out
  test ! -e "$INJECTION_MARKER"
'
rm -f "$malicious_state" "$injection_marker"

echo "root trust-boundary tests passed"
''',
)

Path("tests/root_trust_migration.sh").write_text(r'''#!/usr/bin/env bash
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
install -d -m 0755 -o root -g root "$scratch/runtime-target" "$scratch/jobs-target"
printf 'manifest-sentinel\n' > "$scratch/manifest-target"
printf 'runtime-sentinel\n' > "$scratch/runtime-target/sentinel"
printf 'jobs-sentinel\n' > "$scratch/jobs-target/sentinel"
ln -s "$scratch/runtime-target" "$STATE_DIR/runtime"
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
test "$(stat -c '%U:%G:%a' "$STATE_DIR")" = root:root:711
test "$(stat -c '%U:%G:%a' "$STATE_DIR/runtime")" = root:root:711
test "$(stat -c '%U:%G:%a' "$STATE_DIR/jobs")" = root:root:711
test "$(stat -c '%U:%G:%a' "$STATE_DIR/AI_ENVIRONMENT.json")" = aiagent:aiagent:640
test "$(stat -c '%a' "$scratch/runtime-target")" = 755
test "$(stat -c '%a' "$scratch/jobs-target")" = 755
grep -qF runtime-sentinel "$scratch/runtime-target/sentinel"
grep -qF jobs-sentinel "$scratch/jobs-target/sentinel"
grep -qF manifest-sentinel "$scratch/manifest-target"
test ! -e "$STATE_DIR/install-state.env"
jq -e '.channel=="source" and .version=="source"' /etc/ai-server-agent/control/install-state.json >/dev/null

echo "root trust migration tests passed"
''')

replace_once(
    ".github/workflows/ci.yml",
    'bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh tests/cloudflare_transaction.sh tests/cloudflare_crash_recovery.sh tests/root_trust_boundary.sh',
    'bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh tests/cloudflare_transaction.sh tests/cloudflare_crash_recovery.sh tests/root_trust_boundary.sh tests/root_trust_migration.sh',
)
replace_once(
    ".github/workflows/security.yml",
    '          sudo env AI_SERVER_AGENT_BINARY=/tmp/ai-server-agent AI_SERVER_AGENT_NONINTERACTIVE=1 bash install.sh\n          sudo bash tests/root_trust_boundary.sh\n',
    '          sudo env AI_SERVER_AGENT_BINARY=/tmp/ai-server-agent bash tests/root_trust_migration.sh\n          sudo bash tests/root_trust_boundary.sh\n',
)
'''
p.write_text(text + extension)
print('root trust applicator extended for least-write runtime and safe migration')
