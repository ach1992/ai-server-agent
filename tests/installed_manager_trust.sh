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

# Bash -p alone does not remove BASH_ENV or exported BASH_FUNC_* from
# subprocess environments. Exercise a trusted, root-owned Bash flock shim
# DURING THE WRAPPER'S LOCK SETUP (before manage.sh is invoked), not merely
# a child of the final management implementation. This also probes the
# supported inherited credential FD3 without reading any real credential.
rm -f -- "$scratch/bin/tr"
cat > "$scratch/startup.sh" <<'UNTRUSTED_STARTUP'
printf 'executed\n' > "$ASA_TEST_EXECUTION_MARKER"
UNTRUSTED_STARTUP
cat > "$scratch/bin/flock" <<'FLOCK_PROBE'
#!/bin/bash
printf 'called\n' > "$ASA_TEST_EARLY_SHIM_WITNESS"
if /usr/bin/env | /usr/bin/grep -Eq '^(BASH_FUNC_asa_env_probe%%|BASH_ENV)='; then
  printf 'inherited\n' > "$ASA_TEST_EXECUTION_MARKER"
fi
if declare -F asa_env_probe >/dev/null; then asa_env_probe; fi
if [ "${ASA_TEST_EXPECT_CREDENTIAL_FD:-}" = 1 ]; then
  [ "${AI_SERVER_AGENT_MCP_AUTH_FD:-}" = 3 ] && [ -r /proc/self/fd/3 ] || {
    printf 'credential fd missing\n' > "$ASA_TEST_EXECUTION_MARKER"
    exit 1
  }
fi
exec /usr/bin/flock "$@"
FLOCK_PROBE
chmod 0755 "$scratch/bin/flock"
asa_env_probe(){ printf 'imported\n' > "$ASA_TEST_EXECUTION_MARKER"; }
export -f asa_env_probe
(
  exec 3<<<'synthetic-nonsecret-fd3-payload'
  ASA_TEST_EXPECT_CREDENTIAL_FD=1 AI_SERVER_AGENT_MCP_AUTH_FD=3 \
    ASA_TEST_EXECUTION_MARKER="$marker" \
    ASA_TEST_EARLY_SHIM_WITNESS="$scratch/early-flock-called" \
    BASH_ENV="$scratch/startup.sh" PATH="$scratch/bin:/usr/bin:/bin" \
    "$manager" help > "$scratch/safe.out"
)
[ -f "$scratch/early-flock-called" ] || { echo 'pre-handoff Bash flock shim did not run' >&2; exit 1; }
[ ! -e "$marker" ] || { echo 'Bash environment leaked before management handoff' >&2; exit 1; }
rm -f -- "$scratch/bin/flock"

# Continue proving post-handoff isolation through a trusted root-owned
# systemctl shim, without changing actual installed helper scripts.
cat > "$scratch/bin/systemctl" <<'SYSTEMCTL_PROBE'
#!/bin/sh
printf 'called\n' > "$ASA_TEST_SHIM_WITNESS"
if /usr/bin/env | /usr/bin/grep -Eq '^(BASH_FUNC_asa_env_probe%%|BASH_ENV)='; then
  printf 'inherited\n' > "$ASA_TEST_EXECUTION_MARKER"
fi
exec /usr/bin/systemctl "$@"
SYSTEMCTL_PROBE
chmod 0755 "$scratch/bin/systemctl"
asa_env_probe(){ :; }
export -f asa_env_probe
ASA_TEST_EXECUTION_MARKER="$marker" ASA_TEST_SHIM_WITNESS="$scratch/systemctl-called" \
  BASH_ENV="$scratch/startup.sh" PATH="$scratch/bin:/usr/bin:/bin" \
  "$manager" status > "$scratch/safe.out"
unset -f asa_env_probe
[ -f "$scratch/systemctl-called" ] || { echo 'trusted status subprocess fixture did not execute' >&2; exit 1; }
[ ! -e "$marker" ] || { echo 'BASH_ENV or exported Bash functions escaped the wrapper' >&2; exit 1; }

test "$(/usr/bin/sha256sum "$config")" = "$config_hash"
test "$(/usr/bin/sha256sum "$identity")" = "$identity_hash"
test "$(/usr/bin/stat -c '%a' "$library")" = "$original_library_mode"
test "$(/usr/bin/stat -c '%a' "$helper")" = "$original_helper_mode"
systemctl is-active --quiet ai-server-agent.service
systemctl is-active --quiet ai-server-agent-executor.service

echo 'ROOT_INSTALLED_MANAGER_DRIFT_TRUST_PASS'
