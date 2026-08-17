from pathlib import Path


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    if text.count(old) != 1:
        raise SystemExit(f"{path}: marker count={text.count(old)} for {old[:80]!r}")
    p.write_text(text.replace(old, new, 1))

replace_once(
    "install.sh",
    '''case "$STATE_CHANNEL" in stable|source) ;; *) die "invalid install channel" ;; esac
[[ "$STATE_VERSION" =~ ^(source|v[0-9]+\\.[0-9]+\\.[0-9]+)$ ]] || die "invalid install version metadata"
[[ "$STATE_REF" =~ ^([0-9a-f]{40}|v[0-9]+\\.[0-9]+\\.[0-9]+|binary)$ ]] || die "invalid install ref metadata: $STATE_REF"
[[ "$TRACK_REF" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]] || die "invalid install tracking ref metadata: $TRACK_REF"
''',
    '''case "$STATE_CHANNEL" in stable|source) ;; *) die "invalid install channel" ;; esac
[[ "$STATE_VERSION" =~ ^(source|v[0-9]+\\.[0-9]+\\.[0-9]+)$ ]] || die "invalid install version metadata"
[[ "$STATE_REF" =~ ^([0-9a-f]{40}|v[0-9]+\\.[0-9]+\\.[0-9]+|binary)$ ]] || die "invalid install ref metadata: $STATE_REF"
[[ "$TRACK_REF" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]] || die "invalid install tracking ref metadata: $TRACK_REF"
if [ "$STATE_CHANNEL" = stable ]; then
  [ "$STATE_REF" = "$STATE_VERSION" ] && [ "$TRACK_REF" = "$STATE_VERSION" ] || die "Stable install metadata must pin version/ref/track_ref to the same tag."
else
  [ "$STATE_VERSION" = source ] || die "Source install metadata must use version=source."
  [[ "$STATE_REF" =~ ^([0-9a-f]{40}|binary)$ ]] || die "Source install metadata must use an immutable commit SHA or binary marker."
fi
''',
)

replace_once(
    "update.sh",
    '''load_install_state(){
  local file="$1" json keys
  [ -e "$file" ] || return 0
''',
    '''load_install_state(){
  local file="$1" json keys
  [ -e "$file" ] || { echo "Trusted install state is missing: $file. Refusing to guess an update channel or ref." >&2; exit 1; }
''',
)

replace_once(
    "tests/root_trust_boundary.sh",
    '''[ "$(sha256sum "$INSTALL_STATE" | awk '{print $1}')" = "$state_hash" ]

# Neither unprivileged principal may replace root-consumed state containers.
''',
    '''[ "$(sha256sum "$INSTALL_STATE" | awk '{print $1}')" = "$state_hash" ]

# Missing trusted identity must fail closed rather than drifting to source/main.
state_backup="$(mktemp)"
cp -- "$INSTALL_STATE" "$state_backup"
rm -f -- "$INSTALL_STATE"
if /usr/local/lib/ai-server-agent/update.sh --plan >/tmp/missing-install-state.out 2>&1; then
  echo "updater guessed a channel/ref without trusted install state" >&2
  exit 1
fi
grep -qF 'Trusted install state is missing' /tmp/missing-install-state.out
install -o root -g root -m 0600 "$state_backup" "$INSTALL_STATE"
rm -f "$state_backup"

# Neither unprivileged principal may replace root-consumed state containers.
''',
)

print('final trust fail-closed guards applied')
