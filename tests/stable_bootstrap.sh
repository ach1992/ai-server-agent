#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE_BOOTSTRAP="$ROOT/scripts/install-stable.sh"
BOOTSTRAP="$SOURCE_BOOTSTRAP"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# This behavioral suite is also invoked by root_trust_boundary.sh *after*
# Agent installation. Keep real root/credential/service state untouched:
# substitute only the fixed detection paths on such an occupied test host.
# The disposable installed-systemd suite separately runs the unchanged
# production bootstrap against actual installed paths and credentials.
rewrite_bootstrap_install_markers(){
  local source="$1" dest="$2" prefix="$3"
  sed \
    -e "s#/etc/ai-server-agent#$prefix/config#g" \
    -e "s#/var/lib/ai-server-agent#$prefix/state#g" \
    -e "s#/usr/local/bin/ai-server-agent#$prefix/bin#g" \
    -e "s#/usr/local/sbin/ai-server-agent-manage#$prefix/manage#g" \
    -e "s#/usr/local/lib/ai-server-agent#$prefix/lib#g" \
    -e "s#/etc/systemd/system/ai-server-agent.service#$prefix/agent.service#g" \
    -e "s#/etc/systemd/system/ai-server-agent-executor.service#$prefix/executor.service#g" \
    "$source" > "$dest"
}
markers=(
  /etc/ai-server-agent /var/lib/ai-server-agent
  /usr/local/bin/ai-server-agent /usr/local/sbin/ai-server-agent-manage
  /usr/local/lib/ai-server-agent /etc/systemd/system/ai-server-agent.service
  /etc/systemd/system/ai-server-agent-executor.service
)
occupied=false
for marker in "${markers[@]}"; do
  [ "$(grep -Fc "$marker" "$SOURCE_BOOTSTRAP")" -eq 2 ] || {
    echo "Stable bootstrap guard missing or changed: $marker" >&2
    exit 1
  }
  if [ -e "$marker" ] || [ -L "$marker" ]; then occupied=true; fi
done
if [ "$occupied" = true ]; then
  BOOTSTRAP="$TMP/bootstrap-isolated-host.sh"
  rewrite_bootstrap_install_markers "$SOURCE_BOOTSTRAP" "$BOOTSTRAP" "$TMP/isolation"
fi
bash -n "$SOURCE_BOOTSTRAP" "$BOOTSTRAP"
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
  RELEASE_REQUEST_MARKER="$FIXTURE/release-request"
  PREREQ_MARKER="$FIXTURE/prerequisite-actions"
  EVENT_MARKER="$FIXTURE/observed-events"
  NO_JQ_BIN="$FIXTURE/no-jq-bin"
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
    printf '%s\n' "$url" >> "$RELEASE_REQUEST_MARKER"
    # Only a private test fixture: simulate another installation beginning
    # after the caller's unprivileged check but before the root handoff.
    if [ -n "${BOOTSTRAP_LATE_INSTALL_MARKER:-}" ]; then
      mkdir -p -- "$BOOTSTRAP_LATE_INSTALL_MARKER"
    fi
    printf 'release-metadata\n' >> "$EVENT_MARKER"
    printf '{"tag_name":"v0.1.2","draft":false,"prerelease":false,"immutable":%s,"assets":[{"name":"install.sh","state":"uploaded","digest":"%s","browser_download_url":"%s"}]}\n' "$IMMUTABLE" "$INSTALLER_DIGEST" "$INSTALLER_URL"
    ;;
  https://github.com/ach1992/ai-server-agent/releases/download/v0.1.2/install.sh)
    [ -n "$out" ] || exit 92
    cp "$FIXTURE_INSTALLER" "$out"
    printf 'release-installer-download\n' >> "$EVENT_MARKER"
    printf 'downloaded\n' > "$DOWNLOAD_MARKER"
    ;;
  *) echo "unexpected curl URL: $url" >&2; exit 91 ;;
esac
CURL
  cat > "$FIXTURE/bin/sudo" <<'SUDO'
#!/usr/bin/env bash
set -Eeuo pipefail
# This fake sudo never launches apt-get. It verifies supported-host
# prerequisite argument ordering and exposes jq only after mock install.
if [ "$1" = /usr/bin/env ]; then
  [ "$2" = DEBIAN_FRONTEND=noninteractive ] && [ "$3" = /usr/bin/apt-get ] || exit 94
  if [ "$4" = update ] && [ "$5" = -qq ] && [ "$#" -eq 5 ]; then
    printf 'apt-update\n' >> "$PREREQ_MARKER"
    printf 'apt-update\n' >> "$EVENT_MARKER"
    exit 0
  fi
  if [ "$4" = install ] && [ "$5" = -y ] && [ "$6" = -qq ] &&
     [ "$7" = ca-certificates ] && [ "$8" = jq ] && [ "$#" -eq 8 ]; then
    printf 'apt-install-jq\n' >> "$PREREQ_MARKER"
    printf 'apt-install-jq\n' >> "$EVENT_MARKER"
    ln -s /usr/bin/jq "$NO_JQ_BIN/jq"
    exit 0
  fi
  echo 'unexpected prerequisite apt-get command' >&2
  exit 94
fi
printf 'sudo-called\n' > "$SUDO_MARKER"
[ "$1" = /bin/bash ] && [ "$2" = --noprofile ] && [ "$3" = --norc ] && [ "$4" = -s ] && [ "$5" = -- ] || exit 93
printf 'privileged-release-stage\n' >> "$EVENT_MARKER"
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
  export FIXTURE_INSTALLER="$FIXTURE/install.sh" EXEC_MARKER ENV_MARKER DOWNLOAD_MARKER RELEASE_REQUEST_MARKER PREREQ_MARKER EVENT_MARKER NO_JQ_BIN SUDO_MARKER ATTACK_MARKER PATH_ATTACK_MARKER MALICIOUS_INSTALLER INSTALLER_DIGEST INSTALLER_URL IMMUTABLE
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
AI_SERVER_AGENT_ENVIRONMENT_PROFILE=production AI_SERVER_AGENT_SERVER_LABEL='Live Host' \
  AI_SERVER_AGENT_NONINTERACTIVE=1 bash "$BOOTSTRAP" > "$FIXTURE/out"
test -s "$SUDO_MARKER"
test -s "$EXEC_MARKER"
test "$(sed -n '1p' "$ENV_MARKER")" = production
test "$(sed -n '2p' "$ENV_MARKER")" = 'Live Host'
test "$(sed -n '3p' "$ENV_MARKER")" = 1

make_fixture true false false
if AI_SERVER_AGENT_ENVIRONMENT_PROFILE=Production bash "$BOOTSTRAP" > "$FIXTURE/out" 2>&1; then
  echo 'unknown purpose passed privileged bootstrap validation' >&2
  exit 1
fi
grep -Fq 'Invalid environment purpose' "$FIXTURE/out"
test -s "$SUDO_MARKER"
test ! -e "$EXEC_MARKER"

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

# Exercise both fresh-only checks without modifying the real host's Agent
# paths. Only the canonical path constant is redirected to a private marker;
# the production logic, release verification and privilege handoff are intact.
make_install_guard_fixture(){
  EXISTING_MARKER="$FIXTURE/marker-root/config"
  rewrite_bootstrap_install_markers "$SOURCE_BOOTSTRAP" "$FIXTURE/bootstrap-marker-fixture.sh" "$FIXTURE/marker-root"
  [ "$(grep -Fc "$EXISTING_MARKER" "$FIXTURE/bootstrap-marker-fixture.sh")" -eq 2 ] || {
    echo 'Expected both bootstrap installation guards in the fixture' >&2
    exit 1
  }
}

make_fixture true false false
make_install_guard_fixture
mkdir -p "$EXISTING_MARKER"
if bash "$FIXTURE/bootstrap-marker-fixture.sh" v0.1.2 > "$FIXTURE/out" 2>&1; then
  echo 'already-installed host was accepted by the Stable bootstrap' >&2
  exit 1
fi
grep -qF 'Stable bootstrap is for fresh installations only' "$FIXTURE/out"
test ! -e "$RELEASE_REQUEST_MARKER"
test ! -e "$DOWNLOAD_MARKER"
test ! -e "$SUDO_MARKER"
test ! -e "$EXEC_MARKER"
test ! -e "$PREREQ_MARKER"

make_fixture true false false
make_install_guard_fixture
BOOTSTRAP_LATE_INSTALL_MARKER="$EXISTING_MARKER" \
  bash "$FIXTURE/bootstrap-marker-fixture.sh" v0.1.2 > "$FIXTURE/out" 2>&1 &&
  { echo 'late install bypassed the privileged Stable bootstrap guard' >&2; exit 1; }
grep -qF 'Existing Agent state was detected at privileged handoff' "$FIXTURE/out"
test -d "$EXISTING_MARKER"
test -s "$RELEASE_REQUEST_MARKER"
test -s "$DOWNLOAD_MARKER"
test -s "$SUDO_MARKER"
test ! -e "$EXEC_MARKER"
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

# Regression matrix: bootstrap with jq present/absent, latest/pinned release,
# and controlled Ubuntu/Debian os-release content. The missing-jq path is
# isolated: PATH excludes jq until fake sudo handles the exact APT request;
# no host packages, release assets or Agent services are ever changed.
for fixture_distro in debian ubuntu; do
  for fixture_jq in present missing; do
    for fixture_select in latest explicit; do
      make_fixture true false false
      OS_RELEASE_FIXTURE="$FIXTURE/os-release"
      case "$fixture_distro" in
        debian)
          printf 'ID=debian\nVERSION_ID="13"\nVERSION="13 (trixie)"\n' > "$OS_RELEASE_FIXTURE"
          ;;
        ubuntu)
          printf 'ID=ubuntu\nVERSION_ID="24.04"\nVERSION="24.04.3 LTS (Noble Numbat)"\n' > "$OS_RELEASE_FIXTURE"
          ;;
      esac
      export OS_RELEASE_FIXTURE

      # This test-only copy swaps the OS-release *input*, not the production
      # bootstrap implementation or its trust/privilege control flow.
      sed 's#^  \. /etc/os-release$#  . "$OS_RELEASE_FIXTURE"#' "$BOOTSTRAP" > "$FIXTURE/bootstrap-os-fixture.sh"
      grep -qF '  . "$OS_RELEASE_FIXTURE"' "$FIXTURE/bootstrap-os-fixture.sh"
      if grep -qF '  . /etc/os-release' "$FIXTURE/bootstrap-os-fixture.sh"; then
        echo 'OS-release fixture substitution failed closed' >&2
        exit 1
      fi

      test_path="$PATH"
      if [ "$fixture_jq" = missing ]; then
        mkdir -p "$NO_JQ_BIN"
        for tool in bash sha256sum mktemp awk chmod cp rm ln; do
          ln -s "$(command -v "$tool")" "$NO_JQ_BIN/$tool"
        done
        ln -s "$FIXTURE/bin/curl" "$NO_JQ_BIN/curl"
        ln -s "$FIXTURE/bin/sudo" "$NO_JQ_BIN/sudo"
        test_path="$NO_JQ_BIN"
        if PATH="$test_path" /bin/bash -c 'command -v jq >/dev/null 2>&1'; then
          echo 'Missing-jq fixture unexpectedly exposes jq before mocked provisioning' >&2
          exit 1
        fi
      fi

      case "$fixture_select" in
        latest)
          expected_release_url='https://api.github.com/repos/ach1992/ai-server-agent/releases/latest'
          PATH="$test_path" /bin/bash "$FIXTURE/bootstrap-os-fixture.sh" > "$FIXTURE/out"
          ;;
        explicit)
          expected_release_url='https://api.github.com/repos/ach1992/ai-server-agent/releases/tags/v0.1.2'
          PATH="$test_path" /bin/bash "$FIXTURE/bootstrap-os-fixture.sh" v0.1.2 > "$FIXTURE/out"
          ;;
      esac

      if [ "$(wc -l < "$RELEASE_REQUEST_MARKER")" -ne 1 ] ||
         ! grep -Fxq "$expected_release_url" "$RELEASE_REQUEST_MARKER"; then
        echo "Wrong Stable release endpoint after $fixture_distro/$fixture_jq/$fixture_select" >&2
        exit 1
      fi
      grep -qF 'Verified immutable stable installer v0.1.2 before privileged staging.' "$FIXTURE/out"
      grep -qF 'Privileged staging re-verified the authenticated installer bytes.' "$FIXTURE/out"
      test -s "$DOWNLOAD_MARKER"
      test -s "$SUDO_MARKER"
      test -s "$EXEC_MARKER"
      test ! -e "$ATTACK_MARKER"
      test ! -e "$PATH_ATTACK_MARKER"

      # Privileged APT may precede verification when jq is absent, but the
      # release-installer root stage must always follow authenticated bytes.
      if [ "$fixture_jq" = missing ]; then
        expected_events=$'apt-update\napt-install-jq\nrelease-metadata\nrelease-installer-download\nprivileged-release-stage'
      else
        expected_events=$'release-metadata\nrelease-installer-download\nprivileged-release-stage'
      fi
      if [ "$(cat "$EVENT_MARKER")" != "$expected_events" ]; then
        echo "Unexpected privilege/verification order: $fixture_distro/$fixture_jq/$fixture_select" >&2
        exit 1
      fi

      if [ "$fixture_jq" = missing ]; then
        if [ "$(wc -l < "$PREREQ_MARKER")" -ne 2 ] ||
           [ "$(sed -n '1p' "$PREREQ_MARKER")" != apt-update ] ||
           [ "$(sed -n '2p' "$PREREQ_MARKER")" != apt-install-jq ] ||
           [ ! -L "$NO_JQ_BIN/jq" ]; then
          echo "Incorrect jq prerequisite provisioning: $fixture_distro/$fixture_select" >&2
          exit 1
        fi
      elif [ -e "$PREREQ_MARKER" ]; then
        echo "Unexpected sudo/apt before verified installer: $fixture_distro/$fixture_select" >&2
        exit 1
      fi
    done
  done
done

echo 'stable bootstrap trust tests passed'
