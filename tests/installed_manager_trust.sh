#!/bin/bash
set -Eeuo pipefail

# Opt-in destructive-mode test. This runner is disposable; NEVER alter an
# occupied host's installed lib directory to simulate unsafe permissions.
[ "${EUID}" -eq 0 ] &&
  [ "${GITHUB_ACTIONS:-}" = true ] &&
  [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] &&
  [ "${ASA_DISPOSABLE_MANAGER_TRUST:-}" = 1 ] || {
    printf 'Refusing installed management trust drift test outside disposable root CI\n' >&2
    exit 1
  }

manager=/usr/local/sbin/ai-server-agent-manage
library=/usr/local/lib/ai-server-agent
helper="$library/update.sh"
config=/etc/ai-server-agent/config.json
identity=/etc/ai-server-agent/control/install-state.json

[ -x "$manager" ] && [ -f "$helper" ] && [ ! -L "$library" ] &&
  [ -f "$config" ] && [ -f "$identity" ] || {
    echo 'Expected fresh disposable installation is missing' >&2
    exit 1
  }
[ "$(/usr/bin/stat -c '%u' "$library")" = 0 ] || exit 1
[ "$(/usr/bin/stat -c '%u' "$helper")" = 0 ] || exit 1
original_library_mode="$(/usr/bin/stat -c '%a' "$library")"
original_helper_mode="$(/usr/bin/stat -c '%a' "$helper")"
(( (8#$original_library_mode & 0022) == 0 )) || exit 1
(( (8#$original_helper_mode & 0022) == 0 )) || exit 1

scratch="$(mktemp -d /run/asa-manager-trust.XXXXXXXX)"
chmod 0700 "$scratch"
restore(){
  /usr/bin/chmod "$original_helper_mode" "$helper" 2>/dev/null || :
  /usr/bin/chmod "$original_library_mode" "$library" 2>/dev/null || :
  rm -rf -- "$scratch"
}
trap restore EXIT

config_hash="$(/usr/bin/sha256sum "$config")"
identity_hash="$(/usr/bin/sha256sum "$identity")"
"$manager" help > "$scratch/safe.out"
grep -qF 'AI Server Agent management' "$scratch/safe.out"

# Drifting only the installed helper's permissions must fail BEFORE control
# state, lifecycle locks, privileged commands or selected helper execution.
chmod 0777 "$helper"
if "$manager" help > "$scratch/denied.out" 2>&1; then
  echo 'installed manager accepted writable update helper' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
chmod "$original_helper_mode" "$helper"

# Parent-directory drift is a distinct replacement attack, even if all
# installed helper leaves remain root:root mode 0755/0700.
chmod 0777 "$library"
if "$manager" help > "$scratch/denied.out" 2>&1; then
  echo 'installed manager accepted writable library ancestor' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
chmod "$original_library_mode" "$library"

# A trusted PATH directory can contain an untrusted selected executable.
# The wrapper must refuse the selected leaf before hostile tr is executed.
mkdir -m 0755 "$scratch/bin"
cat > "$scratch/bin/tr" <<'BAD_TR'
#!/bin/sh
printf 'unsafe\n' > "$ASA_TEST_EXECUTION_MARKER"
BAD_TR
chmod 0777 "$scratch/bin/tr"
marker="$scratch/hostile-command-executed"
if ASA_TEST_EXECUTION_MARKER="$marker" PATH="$scratch/bin:/usr/bin:/bin" "$manager" help > "$scratch/denied.out" 2>&1; then
  echo 'installed manager accepted writable PATH-selected command' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
[ ! -e "$marker" ] || { echo 'untrusted command ran before wrapper preflight' >&2; exit 1; }

# /bin/bash -p avoids attacker-supplied noninteractive Bash startup hooks for
# both the installed wrapper and the subsequent management implementation.
cat > "$scratch/startup.sh" <<'UNTRUSTED_STARTUP'
printf 'executed\n' > "$ASA_TEST_EXECUTION_MARKER"
UNTRUSTED_STARTUP
ASA_TEST_EXECUTION_MARKER="$marker" BASH_ENV="$scratch/startup.sh" "$manager" help > "$scratch/safe.out"
[ ! -e "$marker" ] || { echo 'BASH_ENV was executed by installed management' >&2; exit 1; }

test "$(/usr/bin/sha256sum "$config")" = "$config_hash"
test "$(/usr/bin/sha256sum "$identity")" = "$identity_hash"
test "$(/usr/bin/stat -c '%a' "$library")" = "$original_library_mode"
test "$(/usr/bin/stat -c '%a' "$helper")" = "$original_helper_mode"
systemctl is-active --quiet ai-server-agent.service
systemctl is-active --quiet ai-server-agent-executor.service

echo 'ROOT_INSTALLED_MANAGER_DRIFT_TRUST_PASS'
