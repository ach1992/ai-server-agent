#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="$ROOT/scripts/install-stable.sh"
BOOTSTRAP_REF=v0.1.2
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

bash -n "$BOOTSTRAP"
grep -qF "https://raw.githubusercontent.com/ach1992/ai-server-agent/$BOOTSTRAP_REF/scripts/install-stable.sh | bash" "$ROOT/README.md"
if grep -qF 'curl -fsSL https://github.com/ach1992/ai-server-agent/releases/latest/download/install.sh | sudo bash' "$ROOT/README.md"; then
  echo 'README restored an unauthenticated release-installer-to-root path' >&2
  exit 1
fi

make_fixture(){
  local immutable="${1:-true}" wrong_digest="${2:-false}" bad_url="${3:-false}"
  FIXTURE="$TMP/fixture-$RANDOM-$RANDOM"
  mkdir -p "$FIXTURE/bin" "$FIXTURE/poison-bin"
  EXEC_MARKER="$FIXTURE/executed"
  DOWNLOAD_MARKER="$FIXTURE/downloaded"
  SUDO_MARKER="$FIXTURE/sudo-called"
  ATTACK_MARKER="$FIXTURE/attacker-executed"
  PATH_ATTACK_MARKER="$FIXTURE/privileged-path-command-executed"
  MALICIOUS_INSTALLER="$FIXTURE/malicious-install.sh"
  cat > "$FIXTURE/install.sh" <<'INSTALLER'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'executed\n' > "$EXEC_MARKER"
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
exec "$@"
SUDO
  chmod +x "$FIXTURE/bin/curl" "$FIXTURE/bin/sudo"
  export FIXTURE_INSTALLER="$FIXTURE/install.sh" EXEC_MARKER DOWNLOAD_MARKER SUDO_MARKER ATTACK_MARKER PATH_ATTACK_MARKER MALICIOUS_INSTALLER INSTALLER_DIGEST INSTALLER_URL IMMUTABLE
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
