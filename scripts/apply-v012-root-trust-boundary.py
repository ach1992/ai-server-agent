from pathlib import Path


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected one marker, found {count}: {old[:80]!r}")
    p.write_text(text.replace(old, new, 1))

# install.sh: split root-trusted control metadata from runtime state and make
# the state hierarchy itself root-controlled.
replace_once(
    "install.sh",
    'CONFIG_FILE="$CONFIG_DIR/config.json"\nINSTALL_STATE="$STATE_DIR/install-state.env"\nMANAGED_STATE="$CONFIG_DIR/managed.json"\n',
    'CONFIG_FILE="$CONFIG_DIR/config.json"\nCONTROL_DIR="$CONFIG_DIR/control"\nINSTALL_STATE="$CONTROL_DIR/install-state.json"\nMANAGED_STATE="$CONFIG_DIR/managed.json"\n',
)
replace_once(
    "install.sh",
    'install -d -m 0750 -o root -g "$AGENT_USER" "$CONFIG_DIR"\ninstall -d -m 0711 -o "$AGENT_USER" -g "$AGENT_USER" "$STATE_DIR"\ninstall -d -m 2750 -o root -g "$AGENT_USER" "$LOG_DIR"\ninstall -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$WORKSPACE_DIR"\ninstall -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$STATE_DIR/runtime"\ninstall -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$STATE_DIR/jobs"\ninstall -d -m 0755 -o root -g root "$LIB_DIR"\n',
    'install -d -m 0750 -o root -g "$AGENT_USER" "$CONFIG_DIR"\ninstall -d -m 0700 -o root -g root "$CONTROL_DIR"\ninstall -d -m 0711 -o root -g root "$STATE_DIR"\ninstall -d -m 2750 -o root -g "$AGENT_USER" "$LOG_DIR"\ninstall -d -m 0750 -o "$WORKER_USER" -g "$WORKER_USER" "$WORKSPACE_DIR"\ninstall -d -m 0711 -o root -g root "$STATE_DIR/runtime"\ninstall -d -m 0711 -o root -g root "$STATE_DIR/jobs"\ninstall -d -m 0755 -o root -g root "$LIB_DIR"\n',
)
replace_once(
    "install.sh",
    '''cat > "$INSTALL_STATE" <<EOF_STATE
CHANNEL=$STATE_CHANNEL
VERSION=$STATE_VERSION
REF=$STATE_REF
TRACK_REF=$TRACK_REF
EOF_STATE
chown root:"$AGENT_USER" "$INSTALL_STATE"; chmod 0640 "$INSTALL_STATE"
''',
    '''state_tmp="$(mktemp "$CONTROL_DIR/.install-state.XXXXXX")"
jq -n \\
  --arg channel "$STATE_CHANNEL" \\
  --arg version "$STATE_VERSION" \\
  --arg ref "$STATE_REF" \\
  --arg track_ref "$TRACK_REF" \\
  '{channel:$channel,version:$version,ref:$ref,track_ref:$track_ref}' > "$state_tmp"
chown root:root "$state_tmp"; chmod 0600 "$state_tmp"
mv -f "$state_tmp" "$INSTALL_STATE"
# v0.1.1 and earlier stored executable shell metadata in the runtime state
# directory. Never read it during migration; remove only the legacy directory
# entry after the new root-only JSON state has been committed.
rm -f "$STATE_DIR/install-state.env"
''',
)
replace_once(
    "install.sh",
    'ProtectHome=true\nReadWritePaths=$STATE_DIR\nUMask=0027\n',
    'ProtectHome=true\nUMask=0027\n',
)

# update.sh: never source state as shell. Strictly parse allowlisted JSON.
replace_once(
    "update.sh",
    'INSTALL_STATE="${AI_SERVER_AGENT_INSTALL_STATE:-/var/lib/ai-server-agent/install-state.env}"\n',
    'DEFAULT_INSTALL_STATE="/etc/ai-server-agent/control/install-state.json"\nINSTALL_STATE="${AI_SERVER_AGENT_INSTALL_STATE:-$DEFAULT_INSTALL_STATE}"\n',
)
replace_once(
    "update.sh",
    '''CHANNEL="${AI_SERVER_AGENT_UPDATE_CHANNEL:-}"
VERSION=""
REF=""
TRACK_REF=""
if [ -r "$INSTALL_STATE" ]; then
  # Root-owned, installer-generated metadata with validated values only.
  # shellcheck disable=SC1090
  . "$INSTALL_STATE"
fi
CHANNEL="${AI_SERVER_AGENT_UPDATE_CHANNEL:-${CHANNEL:-source}}"
''',
    '''CHANNEL="${AI_SERVER_AGENT_UPDATE_CHANNEL:-}"
VERSION=""
REF=""
TRACK_REF=""

load_install_state(){
  local file="$1" json keys
  [ -e "$file" ] || return 0
  [ -f "$file" ] && [ ! -L "$file" ] || { echo "Install state must be a regular non-symlink file: $file" >&2; exit 1; }
  [ "$(stat -c '%s' "$file")" -le 4096 ] || { echo "Install state is unexpectedly large: $file" >&2; exit 1; }
  json="$(cat -- "$file")" || { echo "Could not read install state: $file" >&2; exit 1; }
  jq -e 'type=="object" and (keys|sort)==["channel","ref","track_ref","version"] and (.channel|type)=="string" and (.version|type)=="string" and (.ref|type)=="string" and (.track_ref|type)=="string"' >/dev/null <<<"$json" || {
    echo "Install state has an invalid schema: $file" >&2
    exit 1
  }
  CHANNEL="$(jq -r '.channel' <<<"$json")"
  VERSION="$(jq -r '.version' <<<"$json")"
  REF="$(jq -r '.ref' <<<"$json")"
  TRACK_REF="$(jq -r '.track_ref' <<<"$json")"
  case "$CHANNEL" in
    stable)
      [[ "$VERSION" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+$ ]] || { echo "Invalid stable install version metadata" >&2; exit 1; }
      [ "$REF" = "$VERSION" ] && [ "$TRACK_REF" = "$VERSION" ] || { echo "Stable install metadata must pin version/ref/track_ref to the same tag" >&2; exit 1; }
      ;;
    source)
      [ "$VERSION" = source ] || { echo "Source install metadata must use version=source" >&2; exit 1; }
      [[ "$REF" =~ ^([0-9a-f]{40}|binary)$ ]] || { echo "Invalid source install ref metadata" >&2; exit 1; }
      [[ "$TRACK_REF" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]] || { echo "Invalid source install tracking ref metadata" >&2; exit 1; }
      ;;
    *) echo "Invalid install channel metadata: $CHANNEL" >&2; exit 1 ;;
  esac
}

load_install_state "$INSTALL_STATE"
CHANNEL="${AI_SERVER_AGENT_UPDATE_CHANNEL:-${CHANNEL:-source}}"
''',
)

# manage.sh: root-only control directory for install identity and Cloudflare
# transaction journal; parse identity as strict JSON rather than shell code.
replace_once(
    "manage.sh",
    'STATE_DIR="/var/lib/ai-server-agent"\nINSTALL_STATE="$STATE_DIR/install-state.env"\nMANAGED_STATE="$CONFIG_DIR/managed.json"\n',
    'STATE_DIR="/var/lib/ai-server-agent"\nCONTROL_DIR="$CONFIG_DIR/control"\nINSTALL_STATE="$CONTROL_DIR/install-state.json"\nMANAGED_STATE="$CONFIG_DIR/managed.json"\n',
)
replace_once(
    "manage.sh",
    'CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"\n',
    'CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"\n',
)
replace_once(
    "manage.sh",
    '''installed_identity(){
  local CHANNEL="unknown" VERSION="unknown" REF=""
  if [ -r "$INSTALL_STATE" ]; then
    # Root-owned, installer-generated metadata containing only validated channel/version/ref values.
    # shellcheck disable=SC1090
    . "$INSTALL_STATE"
  fi
  printf '%s|%s|%s\\n' "${CHANNEL:-unknown}" "${VERSION:-unknown}" "${REF:-}"
}
''',
    '''installed_identity(){
  local json channel version ref track_ref
  if [ ! -e "$INSTALL_STATE" ]; then printf 'unknown|unknown|\\n'; return 0; fi
  if [ ! -f "$INSTALL_STATE" ] || [ -L "$INSTALL_STATE" ] || [ "$(stat -c '%s' "$INSTALL_STATE" 2>/dev/null || printf 999999)" -gt 4096 ]; then
    warn "Install identity metadata is invalid; refusing to execute or trust it."
    printf 'unknown|unknown|\\n'
    return 0
  fi
  json="$(cat -- "$INSTALL_STATE" 2>/dev/null || true)"
  if ! jq -e 'type=="object" and (keys|sort)==["channel","ref","track_ref","version"] and (.channel|type)=="string" and (.version|type)=="string" and (.ref|type)=="string" and (.track_ref|type)=="string"' >/dev/null 2>&1 <<<"$json"; then
    warn "Install identity metadata has an invalid schema; refusing to trust it."
    printf 'unknown|unknown|\\n'
    return 0
  fi
  channel="$(jq -r '.channel' <<<"$json")"; version="$(jq -r '.version' <<<"$json")"; ref="$(jq -r '.ref' <<<"$json")"; track_ref="$(jq -r '.track_ref' <<<"$json")"
  case "$channel" in
    stable)
      if ! [[ "$version" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+$ ]] || [ "$ref" != "$version" ] || [ "$track_ref" != "$version" ]; then
        warn "Stable install identity metadata is inconsistent; refusing to trust it."
        printf 'unknown|unknown|\\n'; return 0
      fi
      ;;
    source)
      if [ "$version" != source ] || ! [[ "$ref" =~ ^([0-9a-f]{40}|binary)$ ]] || ! [[ "$track_ref" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]]; then
        warn "Source install identity metadata is invalid; refusing to trust it."
        printf 'unknown|unknown|\\n'; return 0
      fi
      ;;
    *) warn "Install identity channel is invalid; refusing to trust it."; printf 'unknown|unknown|\\n'; return 0 ;;
  esac
  printf '%s|%s|%s\\n' "$channel" "$version" "$ref"
}
''',
)
replace_once(
    "manage.sh",
    '  install -d -o root -g "$AGENT_USER" -m 0750 "$STATE_DIR" || return 1\n  tmp="$(mktemp "$STATE_DIR/.cloudflare-transaction.XXXXXX")" || return 1\n',
    '  install -d -o root -g root -m 0700 "$CONTROL_DIR" || return 1\n  tmp="$(mktemp "$CONTROL_DIR/.cloudflare-transaction.XXXXXX")" || return 1\n',
)
replace_once(
    "manage.sh",
    '  chown root:"$AGENT_USER" "$tmp" || { rm -f "$tmp"; return 1; }\n  chmod 0640 "$tmp" || { rm -f "$tmp"; return 1; }\n',
    '  chown root:root "$tmp" || { rm -f "$tmp"; return 1; }\n  chmod 0600 "$tmp" || { rm -f "$tmp"; return 1; }\n',
)

# Tests: transaction journals now live in the root-only control path.
replace_once(
    "tests/cloudflare_transaction.sh",
    'STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; TLS_DIR="$CONFIG_DIR/tls"; CONFIG_FILE="$CONFIG_DIR/config.json"; MANAGED_STATE="$CONFIG_DIR/managed.json"; CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"; AUTH_HEADER_FILE="$CONFIG_DIR/mcp.authorization"\nmkdir -p "$STATE_DIR" "$CONFIG_DIR" "$TLS_DIR"\n',
    'STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; CONTROL_DIR="$CONFIG_DIR/control"; TLS_DIR="$CONFIG_DIR/tls"; CONFIG_FILE="$CONFIG_DIR/config.json"; MANAGED_STATE="$CONFIG_DIR/managed.json"; CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"; AUTH_HEADER_FILE="$CONFIG_DIR/mcp.authorization"\nmkdir -p "$STATE_DIR" "$CONFIG_DIR" "$CONTROL_DIR" "$TLS_DIR"\n',
)
replace_once(
    "tests/cloudflare_crash_recovery.sh",
    'STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; TLS_DIR="$CONFIG_DIR/tls"; CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"; REMOTE="$TMP/remote.json"\nmkdir -p "$STATE_DIR" "$CONFIG_DIR" "$TLS_DIR"\n',
    'STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; CONTROL_DIR="$CONFIG_DIR/control"; TLS_DIR="$CONFIG_DIR/tls"; CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"; REMOTE="$TMP/remote.json"\nmkdir -p "$STATE_DIR" "$CONFIG_DIR" "$CONTROL_DIR" "$TLS_DIR"\n',
)
replace_once(
    "tests/cloudflare_crash_recovery.sh",
    'STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"\n',
    'STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"\n',
)
# The same rebinding occurs in the fresh recovery process.
replace_once(
    "tests/cloudflare_crash_recovery.sh",
    'ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" timeout --signal=KILL 1s bash "$child" >/tmp/cloudflare-crash-child.out 2>&1\n',
    'ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" timeout --signal=KILL 1s bash "$child" >/tmp/cloudflare-crash-child.out 2>&1\n',
)
replace_once(
    "tests/cloudflare_crash_recovery.sh",
    'ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c \'\n',
    'ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_CONTROL_DIR="$CONTROL_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c \'\n',
)
# second occurrence inside recovery subshell
text = Path("tests/cloudflare_crash_recovery.sh").read_text()
needle = '  STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"\n'
if text.count(needle) != 1:
    raise SystemExit(f"crash recovery inner rebind marker count={text.count(needle)}")
Path("tests/cloudflare_crash_recovery.sh").write_text(text.replace(needle, '  STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"\n', 1))

# Executor: root-owned jobs container and no-follow exclusive job files.
replace_once(
    "internal/executor/executor.go",
    'func (s *Server) startJob(req Request) Response {\n',
    '''func trustedDir(path string) error {
\tfi, err := os.Lstat(path)
\tif err != nil {
\t\treturn err
\t}
\tif fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
\t\treturn fmt.Errorf("trusted path is not a real directory: %s", path)
\t}
\tst, ok := fi.Sys().(*syscall.Stat_t)
\tif !ok || st.Uid != uint32(os.Geteuid()) {
\t\treturn fmt.Errorf("trusted directory is not owned by executor uid: %s", path)
\t}
\tif fi.Mode().Perm()&0022 != 0 {
\t\treturn fmt.Errorf("trusted directory is group/other writable: %s", path)
\t}
\treturn nil
}

func createJobFile(path string, uid, gid uint32) error {
\tf, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0640)
\tif err != nil {
\t\treturn err
\t}
\tdefer f.Close()
\tif err := f.Chown(int(uid), int(gid)); err != nil {
\t\treturn err
\t}
\treturn f.Chmod(0640)
}

func (s *Server) startJob(req Request) Response {
''',
)
replace_once(
    "internal/executor/executor.go",
    '''\tid := fmt.Sprintf("%d", time.Now().UnixNano())
\tlogPath := filepath.Join(s.cfg.StateDir, "jobs", id+".log")
\tstatusPath := filepath.Join(s.cfg.StateDir, "jobs", id+".status")
\tif err := os.MkdirAll(filepath.Dir(logPath), 0750); err != nil {
\t\treturn Response{Error: err.Error()}
\t}
\tunit := "ai-job-" + id
\tmode := ""
\tif !req.Root {
\t\tmode = "--uid=" + s.cfg.WorkerUser
\t}
''',
    '''\tid := fmt.Sprintf("%d", time.Now().UnixNano())
\tjobsDir := filepath.Join(s.cfg.StateDir, "jobs")
\tif err := trustedDir(s.cfg.StateDir); err != nil {
\t\treturn Response{Error: err.Error()}
\t}
\tif err := trustedDir(jobsDir); err != nil {
\t\treturn Response{Error: err.Error()}
\t}
\tlogPath := filepath.Join(jobsDir, id+".log")
\tstatusPath := filepath.Join(jobsDir, id+".status")
\tfileUID, fileGID := uint32(0), uint32(0)
\tmode := ""
\tif !req.Root {
\t\tfileUID, fileGID = s.workerUID, s.workerGID
\t\tmode = "--uid=" + s.cfg.WorkerUser
\t}
\tif err := createJobFile(logPath, fileUID, fileGID); err != nil {
\t\treturn Response{Error: "prepare job log: " + err.Error()}
\t}
\tif err := createJobFile(statusPath, fileUID, fileGID); err != nil {
\t\t_ = os.Remove(logPath)
\t\treturn Response{Error: "prepare job status: " + err.Error()}
\t}
\tunit := "ai-job-" + id
''',
)
replace_once(
    "internal/executor/executor.go",
    '''\tif b, er := os.ReadFile(statusPath); er == nil {
\t\treturn Response{OK: true, Status: "completed", Output: strings.TrimSpace(string(b))}
\t}
''',
    '''\tif b, er := os.ReadFile(statusPath); er == nil {
\t\tstatus := strings.TrimSpace(string(b))
\t\tif status != "" {
\t\t\tif _, parseErr := strconv.Atoi(status); parseErr == nil {
\t\t\t\treturn Response{OK: true, Status: "completed", Output: status}
\t\t\t}
\t\t}
\t}
''',
)

# Unit coverage for trusted directory and symlink-resistant job-file creation.
Path("internal/executor/executor_test.go").write_text(Path("internal/executor/executor_test.go").read_text().replace(
    'import (\n\t"testing"\n',
    'import (\n\t"os"\n\t"path/filepath"\n\t"testing"\n'
) + r'''

func TestTrustedDirRejectsWritableAndSymlink(t *testing.T) {
\tdir := t.TempDir()
\ttrusted := filepath.Join(dir, "trusted")
\tif err := os.Mkdir(trusted, 0711); err != nil {
\t\tt.Fatal(err)
\t}
\tif err := trustedDir(trusted); err != nil {
\t\tt.Fatalf("trustedDir rejected executor-owned 0711 dir: %v", err)
\t}
\tif err := os.Chmod(trusted, 0733); err != nil {
\t\tt.Fatal(err)
\t}
\tif err := trustedDir(trusted); err == nil {
\t\tt.Fatal("trustedDir accepted group/other-writable directory")
\t}
\tlink := filepath.Join(dir, "link")
\tif err := os.Symlink(trusted, link); err != nil {
\t\tt.Fatal(err)
\t}
\tif err := trustedDir(link); err == nil {
\t\tt.Fatal("trustedDir accepted symlink")
\t}
}

func TestCreateJobFileRejectsExistingSymlink(t *testing.T) {
\tdir := t.TempDir()
\ttarget := filepath.Join(dir, "target")
\tif err := os.WriteFile(target, []byte("sentinel"), 0600); err != nil {
\t\tt.Fatal(err)
\t}
\tlink := filepath.Join(dir, "job.log")
\tif err := os.Symlink(target, link); err != nil {
\t\tt.Fatal(err)
\t}
\tuid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
\tif err := createJobFile(link, uid, gid); err == nil {
\t\tt.Fatal("createJobFile followed or replaced an existing symlink")
\t}
\tb, err := os.ReadFile(target)
\tif err != nil {
\t\tt.Fatal(err)
\t}
\tif string(b) != "sentinel" {
\t\tt.Fatalf("symlink target changed: %q", string(b))
\t}
}
''')

# Behavioral test against the actual installed identities and paths.
Path("tests/root_trust_boundary.sh").write_text(r'''#!/usr/bin/env bash
set -Eeuo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run root_trust_boundary.sh as root" >&2; exit 1; }
CONTROL_DIR=/etc/ai-server-agent/control
INSTALL_STATE="$CONTROL_DIR/install-state.json"
CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"
STATE_DIR=/var/lib/ai-server-agent
MANAGE=/usr/local/sbin/ai-server-agent-manage

assert_owner_mode(){
  local path="$1" owner="$2" group="$3" mode="$4" actual
  actual="$(stat -c '%U:%G:%a' "$path")"
  [ "$actual" = "$owner:$group:$mode" ] || { echo "$path ownership/mode = $actual, expected $owner:$group:$mode" >&2; exit 1; }
}
expect_denied(){
  local user="$1"; shift
  if runuser -u "$user" -- "$@" >/dev/null 2>&1; then
    echo "unexpected write capability for $user: $*" >&2
    exit 1
  fi
}

assert_owner_mode /etc/ai-server-agent root aiagent 750
assert_owner_mode "$CONTROL_DIR" root root 700
assert_owner_mode "$INSTALL_STATE" root root 600
assert_owner_mode "$STATE_DIR" root root 711
assert_owner_mode "$STATE_DIR/runtime" root root 711
assert_owner_mode "$STATE_DIR/jobs" root root 711
[ ! -L "$INSTALL_STATE" ]
jq -e 'type=="object" and (keys|sort)==["channel","ref","track_ref","version"]' "$INSTALL_STATE" >/dev/null

# The network-facing service has no writable exception for the state hierarchy.
if systemctl show ai-server-agent.service -p ReadWritePaths --value | grep -qF "$STATE_DIR"; then
  echo "aiagent service still has a writable state-dir exception" >&2
  exit 1
fi

state_hash="$(sha256sum "$INSTALL_STATE" | awk '{print $1}')"
expect_denied aiagent rm -f "$INSTALL_STATE"
expect_denied aiagent mv "$INSTALL_STATE" "$CONTROL_DIR/install-state.replaced"
expect_denied aiagent ln -sfn /tmp/attacker "$INSTALL_STATE"
expect_denied aiagent touch "$CONTROL_DIR/attacker"
expect_denied aiagent sh -c "printf attacker > '$INSTALL_STATE'"
[ "$(sha256sum "$INSTALL_STATE" | awk '{print $1}')" = "$state_hash" ]

# Neither unprivileged principal may replace root-consumed state containers.
for user in aiagent aiworker; do
  expect_denied "$user" mv "$STATE_DIR/jobs" "$STATE_DIR/jobs.replaced"
  expect_denied "$user" mv "$STATE_DIR/runtime" "$STATE_DIR/runtime.replaced"
  expect_denied "$user" touch "$STATE_DIR/attacker"
done

# Create the real Cloudflare transaction journal through production code.
AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1 bash -c '
  source /usr/local/sbin/ai-server-agent-manage
  CF_PENDING_KIND=dns-create
  CF_PENDING_ZONE=zone-test
  CF_PENDING_HOST=mcp.example.com
  CF_PENDING_VALUE=203.0.113.10
  CF_PENDING_MARKER=trust-boundary-marker
  save_cloudflare_transaction_state mcp.example.com zone-test "" "" "" "" "" "" "" "" 3210 "" "" "" ""
'
assert_owner_mode "$CF_TXN_STATE" root root 600
journal_hash="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"
expect_denied aiagent rm -f "$CF_TXN_STATE"
expect_denied aiagent mv "$CF_TXN_STATE" "$CONTROL_DIR/cloudflare-transaction.replaced"
expect_denied aiagent ln -sfn /tmp/attacker "$CF_TXN_STATE"
expect_denied aiagent sh -c "printf attacker > '$CF_TXN_STATE'"
[ "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$journal_hash" ]
rm -f "$CF_TXN_STATE"

echo "root trust-boundary tests passed"
''')

# CI: JSON install-state contract + root trust-boundary behavior.
replace_once(
    ".github/workflows/ci.yml",
    'run: bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh tests/cloudflare_transaction.sh\n',
    'run: bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh tests/cloudflare_transaction.sh tests/cloudflare_crash_recovery.sh tests/root_trust_boundary.sh\n',
)
replace_once(
    ".github/workflows/ci.yml",
    "          grep -qF 'CHANNEL=$STATE_CHANNEL' install.sh\n          grep -qF 'TRACK_REF=$TRACK_REF' install.sh\n",
    "          grep -qF 'INSTALL_STATE=\"$CONTROL_DIR/install-state.json\"' install.sh\n          grep -qF \"'{channel:\u0024channel,version:\u0024version,ref:\u0024ref,track_ref:\u0024track_ref}'\" install.sh\n          if grep -qF '. \"$INSTALL_STATE\"' update.sh manage.sh; then\n            echo 'trusted install state must never be shell-sourced' >&2\n            exit 1\n          fi\n",
)
replace_once(
    ".github/workflows/ci.yml",
    "          grep -qF 'CF_TXN_STATE=\"$STATE_DIR/cloudflare-transaction.json\"' manage.sh\n",
    "          grep -qF 'CF_TXN_STATE=\"$CONTROL_DIR/cloudflare-transaction.json\"' manage.sh\n          grep -qF 'install -d -o root -g root -m 0700 \"$CONTROL_DIR\"' manage.sh\n",
)
replace_once(
    ".github/workflows/ci.yml",
    '''          state="$(mktemp)"
          cat > "$state" <<'STATE'
          CHANNEL=stable
          VERSION=v0.1.1
          REF=v0.1.1
          TRACK_REF=v0.1.1
          STATE
''',
    '''          state="$(mktemp)"
          cat > "$state" <<'STATE'
          {"channel":"stable","version":"v0.1.1","ref":"v0.1.1","track_ref":"v0.1.1"}
          STATE
''',
)
replace_once(
    ".github/workflows/ci.yml",
    "          sudo grep -qF 'CHANNEL=source' /var/lib/ai-server-agent/install-state.env\n",
    "          sudo jq -e '.channel == \"source\" and .version == \"source\"' /etc/ai-server-agent/control/install-state.json >/dev/null\n          sudo bash tests/root_trust_boundary.sh\n",
)
replace_once(
    ".github/workflows/ci.yml",
    "          sudo test ! -e /var/lib/ai-server-agent\n",
    "          sudo test ! -e /var/lib/ai-server-agent\n          sudo test ! -e /etc/ai-server-agent/control\n",
)

# High Assurance Security gets an actual installed-identity trust-boundary job.
security = Path(".github/workflows/security.yml").read_text()
insert_before = "\n  stable-provenance:\n"
if security.count(insert_before) != 1:
    raise SystemExit("security.yml stable-provenance marker missing/ambiguous")
trust_job = r'''
  root-trust-boundary:
    runs-on: ubuntu-22.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26.x'
          cache: false
      - name: Build local test binary
        run: CGO_ENABLED=0 go build -trimpath -o /tmp/ai-server-agent ./cmd/ai-server-agent
      - name: Install and verify root trust boundary
        run: |
          set -Eeuo pipefail
          sudo env AI_SERVER_AGENT_BINARY=/tmp/ai-server-agent AI_SERVER_AGENT_NONINTERACTIVE=1 bash install.sh
          sudo bash tests/root_trust_boundary.sh
      - name: Cleanup test installation
        if: always()
        run: sudo env AI_SERVER_AGENT_YES=1 bash uninstall.sh --purge || true
'''
security = security.replace(insert_before, "\n" + trust_job + insert_before, 1)
# Stable fixtures are JSON now.
security = security.replace('''          cat > "$state" <<'STATE'\n          CHANNEL=stable\n          VERSION=v0.1.1\n          REF=v0.1.1\n          TRACK_REF=v0.1.1\n          STATE\n''', '''          cat > "$state" <<'STATE'\n          {"channel":"stable","version":"v0.1.1","ref":"v0.1.1","track_ref":"v0.1.1"}\n          STATE\n''')
Path(".github/workflows/security.yml").write_text(security)

# Durable architecture/docs: state ownership is an explicit invariant.
arch = Path("docs/ARCHITECTURE.md").read_text()
anchor = "The services communicate through `/run/ai-server-agent/executor.sock` using a random local token. Normal project commands are dropped to `aiworker`; root commands run only when the root tool is selected.\n"
if arch.count(anchor) != 1:
    raise SystemExit("architecture anchor missing")
arch = arch.replace(anchor, anchor + "\nRoot-trusted control metadata is isolated under `/etc/ai-server-agent/control` (`root:root`, non-writable by `aiagent`/`aiworker`). Install identity is strict JSON and is never shell-sourced. The top-level `/var/lib/ai-server-agent` hierarchy plus its `jobs` and `runtime` container entries are root-owned so unprivileged identities cannot replace paths later consumed by the privileged executor; writable worker/browser files exist only beneath root-controlled directory entries.\n", 1)
Path("docs/ARCHITECTURE.md").write_text(arch)

readme = Path("README.md").read_text()
anchor = "- Keep the MCP bearer credential private.\n"
if readme.count(anchor) != 1:
    raise SystemExit("README security anchor missing")
readme = readme.replace(anchor, anchor + "- Root-trusted install/recovery metadata lives under `/etc/ai-server-agent/control` with root-only directory/file permissions; it is not stored in the unprivileged runtime state hierarchy and install identity is parsed as JSON rather than shell-sourced.\n- `/var/lib/ai-server-agent`, `jobs`, and `runtime` are root-controlled container entries; unprivileged writable leaves cannot replace paths later trusted by the privileged executor.\n", 1)
Path("README.md").write_text(readme)

print("root trust-boundary hardening applied")
