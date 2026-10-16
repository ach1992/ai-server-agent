#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="$ROOT/scripts/install-stable.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

bash -n "$BOOTSTRAP"
# The public bootstrap URL is permanent but its source on main is mutable.
# The selected release, verified installer and root staging remain immutable.
bootstrap_url='https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh'
default_command="curl -fsSL $bootstrap_url | bash"
version_regex='^curl -fsSL https://raw\.githubusercontent\.com/ach1992/ai-server-agent/main/scripts/install-stable\.sh \| bash -s -- v[0-9]+\.[0-9]+\.[0-9]+$'
readme_version=''
for doc in "$ROOT/README.md" "$ROOT/docs/OPERATIONS.md"; do
  count="$(grep -Fxc "$default_command" "$doc" || true)"
  [ "$count" -eq 1 ] || {
    echo "$doc must document exactly one permanent version-free main-branch bootstrap command" >&2
    exit 1
  }
  if ! grep -Eq "$version_regex" "$doc"; then
    echo "$doc must document one real specific-version example from the same bootstrap URL" >&2
    exit 1
  fi
  sample="$(sed -nE 's#^curl -fsSL https://raw\.githubusercontent\.com/ach1992/ai-server-agent/main/scripts/install-stable\.sh \| bash -s -- (v[0-9]+\.[0-9]+\.[0-9]+)$#\1#p' "$doc")"
  [ "$(printf '%s\n' "$sample" | wc -l)" -eq 1 ] || {
    echo "$doc must contain exactly one specific-version example" >&2
    exit 1
  }
  if [ -z "$readme_version" ]; then
    readme_version="$sample"
  elif [ "$readme_version" != "$sample" ]; then
    echo 'README and Operations specific-version examples disagree' >&2
    exit 1
  fi
  if grep -Eq '^curl -fsSL https://raw\.githubusercontent\.com/ach1992/ai-server-agent/([0-9a-f]{40}|v[0-9]+\.[0-9]+\.[0-9]+)/scripts/install-stable\.sh' "$doc"; then
    echo "$doc must not revert to a versioned/commit-pinned public bootstrap URL" >&2
    exit 1
  fi
  if grep -Eq '^curl -fsSL .* \| sudo bash' "$doc"; then
    echo "$doc must not directly elevate the mutable bootstrap" >&2
    exit 1
  fi
  if grep -qF 'curl -fsSL https://github.com/ach1992/ai-server-agent/releases/latest/download/install.sh | sudo bash' "$doc"; then
    echo "$doc restored unauthenticated release-installer-to-root execution" >&2
    exit 1
  fi
done

make_fixture(){
  local immutable="${1:-true}" wrong_digest="${2:-false}" bad_url="${3:-false}"
  FIXTURE="$TMP/fixture-$RANDOM-$RANDOM"
  mkdir -p "$FIXTURE/bin" "$FIXTURE/poison-bin"
  EXEC_MARKER="$FIXTURE/executed"
  ENV_MARKER="$FIXTURE/installer-environment"
  DOWNLOAD_MARKER="$FIXTURE/downloaded"
  SUDO_MARKER="$FIXTURE/sudo-called"
  ATTACK_MARKER="$FIXTURE/attacker-executed"
  PATH_ATTACK_MARKER="$FIXTURE/privileged-path-command-executed"
  MALICIOUS_INSTALLER="$FIXTURE/malicious-install.sh"
  cat > "$FIXTURE/install.sh" <<'INSTALLER'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'executed\n' > "$EXEC_MARKER"
printf '%s\n' "${AI_SERVER_AGENT_ENVIRONMENT_PROFILE-<unset>}" "${AI_SERVER_AGENT_SERVER_LABEL-<unset>}" "${AI_SERVER_AGENT_NONINTERACTIVE-<unset>}" > "$ENV_MARKER"
INSTALLER
  cat > "$MALICIOUS_INSTALLER" <<'MALICIOUS'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'attacker-executed\n' > "$ATTACK_MARKER"
MALICIOUS
  cat > "$FIXTURE/poison-bin/install" <<'POISON_INSTALL'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'poison-install\n' > "$PATH_ATTACK_MARKER"
exec /usr/bin/install "$@"
POISON_INSTALL
  chmod +x "$FIXTURE/poison-bin/install"
  INSTALLER_DIGEST="sha256:$(sha256sum "$FIXTURE/install.sh" | awk '{print $1}')"
  if [ "$wrong_digest" = true ]; then
    INSTALLER_DIGEST="sha256:$(printf 'different bytes\n' | sha256sum | awk '{print $1}')"
  fi
  INSTALLER_URL="https://github.com/ach1992/ai-server-agent/releases/download/v0.1.2/install.sh"
  if [ "$bad_url" = true ]; then
    INSTALLER_URL="https://example.invalid/install.sh"
  fi
  IMMUTABLE="$immutable"

  cat > "$FIXTURE/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -Eeuo pipefail
out=''
url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -H) shift 2 ;;
    -o) out="$2"; shift 2 ;;
    --proto) shift 2 ;;
    --tlsv1.2) shift ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
case "$url" in
  https://api.github.com/repos/ach1992/ai-server-agent/releases/latest|https://api.github.com/repos/ach1992/ai-server-agent/releases/tags/v0.1.2)
    printf '{"tag_name":"v0.1.2","draft":false,"prerelease":false,"immutable":%s,"assets":[{"name":"install.sh","state":"uploaded","digest":"%s","browser_download_url":"%s"}]}\n' "$IMMUTABLE" "$INSTALLER_DIGEST" "$INSTALLER_URL"
    ;;
  https://github.com/ach1992/ai-server-agent/releases/download/v0.1.2/install.sh)
    [ -n "$out" ] || exit 92
    cp "$FIXTURE_INSTALLER" "$out"
    printf 'downloaded\n' > "$DOWNLOAD_MARKER"
    ;;
  *) echo "unexpected curl URL: $url" >&2; exit 91 ;;
esac
CURL
  cat > "$FIXTURE/bin/sudo" <<'SUDO'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'sudo-called\n' > "$SUDO_MARKER"
[ "$1" = /bin/bash ] && [ "$2" = --noprofile ] && [ "$3" = --norc ] && [ "$4" = -s ] && [ "$5" = -- ] || exit 93
source_installer="$6"
if [ "${RACE_SOURCE_REPLACEMENT:-false}" = true ]; then
  rm -f -- "$source_installer"
  cp -- "$MALICIOUS_INSTALLER" "$source_installer"
  chmod 0500 "$source_installer"
fi
export PATH="$POISON_BIN:$PATH"
# Emulate sudo env_reset so the test proves explicit, positional forwarding.
unset AI_SERVER_AGENT_ENVIRONMENT_PROFILE AI_SERVER_AGENT_SERVER_LABEL AI_SERVER_AGENT_NONINTERACTIVE
exec "$@"
SUDO
  chmod +x "$FIXTURE/bin/curl" "$FIXTURE/bin/sudo"
  export FIXTURE_INSTALLER="$FIXTURE/install.sh" EXEC_MARKER ENV_MARKER DOWNLOAD_MARKER SUDO_MARKER ATTACK_MARKER PATH_ATTACK_MARKER MALICIOUS_INSTALLER INSTALLER_DIGEST INSTALLER_URL IMMUTABLE
  export POISON_BIN="$FIXTURE/poison-bin"
  export RACE_SOURCE_REPLACEMENT=false
  export PATH="$FIXTURE/bin:/usr/bin:/bin"
}

make_fixture true false false
bash "$BOOTSTRAP" > "$FIXTURE/out"
grep -qF 'Verified immutable stable installer v0.1.2 before privileged staging.' "$FIXTURE/out"
grep -qF 'Privileged staging re-verified the authenticated installer bytes.' "$FIXTURE/out"
test -s "$DOWNLOAD_MARKER"
test -s "$SUDO_MARKER"
test -s "$EXEC_MARKER"
test ! -e "$PATH_ATTACK_MARKER"
test "$(sed -n '1p' "$ENV_MARKER")" = '<unset>'
test "$(sed -n '2p' "$ENV_MARKER")" = '<unset>'
test "$(sed -n '3p' "$ENV_MARKER")" = '<unset>'

make_fixture true false false
AI_SERVER_AGENT_ENVIRONMENT_PROFILE=validation AI_SERVER_AGENT_SERVER_LABEL='QA آزمایش' \
  AI_SERVER_AGENT_NONINTERACTIVE=1 bash "$BOOTSTRAP" > "$FIXTURE/out"
test -s "$SUDO_MARKER"
test -s "$EXEC_MARKER"
test "$(sed -n '1p' "$ENV_MARKER")" = validation
test "$(sed -n '2p' "$ENV_MARKER")" = 'QA آزمایش'
test "$(sed -n '3p' "$ENV_MARKER")" = 1
test ! -e "$PATH_ATTACK_MARKER"

make_fixture true false false
ROOT_BIN="$FIXTURE/root-bin"
mkdir -p "$ROOT_BIN"
for cmd in bash curl sha256sum mktemp jq awk chmod rm cp; do
  ln -s "$(command -v "$cmd")" "$ROOT_BIN/$cmd"
done
PATH="$ROOT_BIN" bash "$BOOTSTRAP" > "$FIXTURE/out"
grep -qF 'Verified immutable stable installer v0.1.2 before privileged staging.' "$FIXTURE/out"
grep -qF 'Privileged staging re-verified the authenticated installer bytes.' "$FIXTURE/out"
test -s "$DOWNLOAD_MARKER"
test -s "$EXEC_MARKER"
test ! -e "$SUDO_MARKER"
test ! -e "$PATH_ATTACK_MARKER"

make_fixture true false false
bash "$BOOTSTRAP" v0.1.2 > "$FIXTURE/out"
test -s "$SUDO_MARKER"
test -s "$EXEC_MARKER"
test ! -e "$PATH_ATTACK_MARKER"

make_fixture true false false
export RACE_SOURCE_REPLACEMENT=true
if bash "$BOOTSTRAP" > "$FIXTURE/out" 2>&1; then
  echo 'unprivileged installer pathname replacement reached privileged execution' >&2
  exit 1
fi
grep -qF 'Privileged staging digest mismatch' "$FIXTURE/out"
test -s "$DOWNLOAD_MARKER"
test -s "$SUDO_MARKER"
test ! -e "$EXEC_MARKER"
test ! -e "$ATTACK_MARKER"
test ! -e "$PATH_ATTACK_MARKER"

make_fixture false false false
if bash "$BOOTSTRAP" > "$FIXTURE/out" 2>&1; then
  echo 'mutable release was accepted by stable bootstrap' >&2
  exit 1
fi
grep -qF 'not a published immutable stable release' "$FIXTURE/out"
test ! -e "$DOWNLOAD_MARKER"
test ! -e "$SUDO_MARKER"
test ! -e "$EXEC_MARKER"

make_fixture true true false
if bash "$BOOTSTRAP" > "$FIXTURE/out" 2>&1; then
  echo 'digest mismatch was accepted by stable bootstrap' >&2
  exit 1
fi
grep -qF 'digest mismatch' "$FIXTURE/out"
test -s "$DOWNLOAD_MARKER"
test ! -e "$SUDO_MARKER"
test ! -e "$EXEC_MARKER"

make_fixture true false true
if bash "$BOOTSTRAP" > "$FIXTURE/out" 2>&1; then
  echo 'wrong release asset URL was accepted by stable bootstrap' >&2
  exit 1
fi
grep -qF 'asset URL does not match the exact release tag' "$FIXTURE/out"
test ! -e "$DOWNLOAD_MARKER"
test ! -e "$SUDO_MARKER"
test ! -e "$EXEC_MARKER"

echo 'stable bootstrap trust tests passed'
