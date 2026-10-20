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
uninstall_helper="$library/uninstall.sh"
config=/etc/ai-server-agent/config.json
identity=/etc/ai-server-agent/control/install-state.json

[ -x "$manager" ] && [ -f "$helper" ] && [ ! -L "$library" ] &&
  [ -f "$config" ] && [ -f "$identity" ] && [ -f "$uninstall_helper" ] || {
    echo 'Expected fresh disposable installation is missing' >&2
    exit 1
  }
[ "$(/usr/bin/stat -c '%u' "$library")" = 0 ] || exit 1
[ "$(/usr/bin/stat -c '%u' "$helper")" = 0 ] || exit 1
original_library_mode="$(/usr/bin/stat -c '%a' "$library")"
original_helper_mode="$(/usr/bin/stat -c '%a' "$helper")"
original_uninstall_mode="$(/usr/bin/stat -c '%a' "$uninstall_helper")"
agent_binary=/usr/local/bin/ai-server-agent
agent_bin_dir=/usr/local/bin
credentials=/etc/ai-server-agent/mcp-credentials.json
[ -f "$agent_binary" ] && [ ! -L "$agent_binary" ] && [ -x "$agent_binary" ] &&
  [ -f "$credentials" ] && [ ! -L "$credentials" ] || {
    echo 'Expected installed Agent and credential store are missing' >&2; exit 1
  }
[ "$(/usr/bin/stat -c '%u' "$agent_binary")" = 0 ] || exit 1
original_agent_mode="$(/usr/bin/stat -c '%a' "$agent_binary")"
original_agent_dir_mode="$(/usr/bin/stat -c '%a' "$agent_bin_dir")"
(( (8#$original_agent_mode & 0022) == 0 )) || exit 1
(( (8#$original_agent_dir_mode & 0022) == 0 )) || exit 1
(( (8#$original_library_mode & 0022) == 0 )) || exit 1
(( (8#$original_helper_mode & 0022) == 0 )) || exit 1

scratch="$(mktemp -d /run/asa-manager-trust.XXXXXXXX)"
chmod 0700 "$scratch"
restore(){
  /usr/bin/chmod "$original_helper_mode" "$helper" 2>/dev/null || :
  /usr/bin/chmod "$original_uninstall_mode" "$uninstall_helper" 2>/dev/null || :
  /usr/bin/chmod "$original_agent_mode" "$agent_binary" 2>/dev/null || :
  /usr/bin/chmod "$original_agent_dir_mode" "$agent_bin_dir" 2>/dev/null || :
  /usr/bin/chmod "$original_library_mode" "$library" 2>/dev/null || :
  rm -rf -- "$scratch"
}
trap restore EXIT

config_hash="$(/usr/bin/sha256sum "$config")"
identity_hash="$(/usr/bin/sha256sum "$identity")"
credentials_hash="$(/usr/bin/sha256sum "$credentials")"
"$manager" help > "$scratch/safe.out"
grep -qF 'AI Server Agent management' "$scratch/safe.out"

# Root source-mode overrides are separately executable privilege boundaries.
# Exercise the complete installer --check preflight on this disposable runner,
# using a private root-owned candidate rather than the installed live binary.
source_root="${BASH_SOURCE[0]%/*}/.."
source_stage=/run/asa-trusted-source-bin/ai-server-agent
[ -x "$source_stage" ] && [ ! -L "$source_stage" ] || {
  echo 'Missing trusted CI source-binary fixture' >&2; exit 1
}
mkdir -m 0755 "$scratch/source-override"
source_candidate="$scratch/source-override/ai-server-agent"
install -o root -g root -m 0755 "$source_stage" "$source_candidate"
source_candidate_hash="$(/usr/bin/sha256sum "$source_candidate")"
source_check(){
  AI_SERVER_AGENT_BINARY="$1" bash "$source_root/install.sh" --check
}
source_check "$source_candidate" > "$scratch/safe.out"
grep -qF 'Compatibility and privileged install-path checks passed' "$scratch/safe.out"
source_denied(){
  local label="$1" target="$2" diagnostic="$3"
  if source_check "$target" > "$scratch/denied.out" 2>&1; then
    echo "Installer accepted unsafe source override: $label" >&2; exit 1
  fi
  grep -qF "$diagnostic" "$scratch/denied.out" || {
    echo "Source override rejected for wrong reason: $label" >&2; exit 1
  }
}
chmod 0777 "$source_candidate"
source_denied 'writable leaf' "$source_candidate" "Privileged install path is writable by non-root users: $source_candidate"
chmod 0755 "$source_candidate"
chmod 0777 "$scratch/source-override"
source_denied 'writable ancestor' "$source_candidate" "Privileged install path is writable by non-root users: $scratch/source-override"
chmod 0755 "$scratch/source-override"
ln -s "$source_candidate" "$scratch/source-override/link-agent"
source_denied 'symlinked leaf' "$scratch/source-override/link-agent" 'Unsafe symlink in privileged install path:'
rm -f "$scratch/source-override/link-agent"
ln "$source_candidate" "$scratch/source-override/linked-agent"
source_denied 'hardlinked leaf' "$source_candidate" 'Privileged install target must not be hardlinked:'
rm -f "$scratch/source-override/linked-agent"
chmod 0644 "$source_candidate"
source_denied 'nonexecutable leaf' "$source_candidate" 'Source Agent binary must be an existing executable file:'
chmod 0755 "$source_candidate"
source_denied 'missing leaf' "$scratch/source-override/missing-agent" 'Source Agent binary must be an existing executable file:'
source_denied 'relative path' 'relative-agent' 'Privileged install path must be absolute:'
test "$(/usr/bin/sha256sum "$source_candidate")" = "$source_candidate_hash"
source_check "$source_candidate" > "$scratch/safe.out"
grep -qF 'Compatibility and privileged install-path checks passed' "$scratch/safe.out"

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

# Directly invoked installed update/uninstall helpers are separate privileged
# entrypoints. Exercise the real installed scripts without changing services.
direct_update="$library/update.sh"
direct_uninstall="$uninstall_helper"
[ -x "$direct_update" ] && [ -x "$direct_uninstall" ] || {
  echo 'Installed direct lifecycle helpers are missing' >&2; exit 1
}
update_mode="$(/usr/bin/stat -c '%a' "$direct_update")"
uninstall_mode="$original_uninstall_mode"
cat > "$scratch/test-install-state.json" <<'STABLE_STATE'
{"channel":"stable","version":"v0.1.18","ref":"v0.1.18","track_ref":"v0.1.18"}
STABLE_STATE
chmod 0600 "$scratch/test-install-state.json"
plan_update(){
  AI_SERVER_AGENT_INSTALL_STATE="$scratch/test-install-state.json" \
    AI_SERVER_AGENT_VERSION=v0.1.18 "$direct_update" --plan
}
refuse_uninstall(){
  AI_SERVER_AGENT_YES=0 "$direct_uninstall" --purge </dev/null
}
plan_update > "$scratch/safe.out"
grep -qF 'Target version: v0.1.18' "$scratch/safe.out"
if refuse_uninstall > "$scratch/denied.out" 2>&1; then
  echo 'Direct purge unexpectedly proceeded without confirmation' >&2; exit 1
fi
grep -qF 'confirmation requires a terminal' "$scratch/denied.out"

# The direct updater executes the active Agent via an absolute pathname.
# Keep its ancestor out of PATH: selected-command checks alone cannot help.
agent_hash="$(/usr/bin/sha256sum "$agent_binary")"
chmod 0777 "$agent_binary"
if PATH=/usr/bin:/bin plan_update > "$scratch/denied.out" 2>&1; then
  echo 'Updater accepted a writable active binary during --plan' >&2; exit 1
fi
grep -qF "Privileged install path is writable by non-root users: $agent_binary" "$scratch/denied.out"

# Test the non-plan entrypoint as well, with an offline curl guard so a broken
# preflight cannot download, execute or install anything on this CI runner.
cat > "$scratch/bin/curl" <<'OFFLINE_CURL'
#!/bin/sh
printf 'unexpected network\n' > "$ASA_TEST_UPDATE_NETWORK_MARKER"
exit 97
OFFLINE_CURL
chmod 0755 "$scratch/bin/curl"
if ASA_TEST_UPDATE_NETWORK_MARKER="$scratch/unexpected-update-network" \
  PATH="$scratch/bin:/usr/bin:/bin" \
  AI_SERVER_AGENT_INSTALL_STATE="$scratch/test-install-state.json" \
  AI_SERVER_AGENT_VERSION=v0.1.18 \
  "$direct_update" > "$scratch/denied.out" 2>&1; then
  echo 'Updater accepted writable active binary for real update' >&2; exit 1
fi
grep -qF "Privileged install path is writable by non-root users: $agent_binary" "$scratch/denied.out"
[ ! -e "$scratch/unexpected-update-network" ] ||
  { echo 'Update reached network before unsafe Agent binary rejection' >&2; exit 1; }
rm -f -- "$scratch/bin/curl"
chmod "$original_agent_mode" "$agent_binary"

chmod 0777 "$agent_bin_dir"
if PATH=/usr/bin:/bin plan_update > "$scratch/denied.out" 2>&1; then
  echo 'Updater accepted writable active binary ancestor outside PATH' >&2; exit 1
fi
grep -qF "Privileged install path is writable by non-root users: $agent_bin_dir" "$scratch/denied.out"
chmod "$original_agent_dir_mode" "$agent_bin_dir"
test "$(/usr/bin/sha256sum "$agent_binary")" = "$agent_hash"

chmod 0777 "$direct_update"
if plan_update > "$scratch/denied.out" 2>&1; then
  echo 'Direct updater accepted writable installed executable' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
chmod "$update_mode" "$direct_update"
chmod 0777 "$direct_uninstall"
if refuse_uninstall > "$scratch/denied.out" 2>&1; then
  echo 'Direct uninstaller accepted writable installed executable' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
chmod "$uninstall_mode" "$direct_uninstall"

chmod 0777 "$library"
if plan_update > "$scratch/denied.out" 2>&1; then
  echo 'Direct updater accepted writable ancestor' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
if refuse_uninstall > "$scratch/denied.out" 2>&1; then
  echo 'Direct uninstaller accepted writable ancestor' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
chmod "$original_library_mode" "$library"

# Unsafe selected command leaf must be rejected before execution.
cat > "$scratch/bin/tr" <<'BAD_DIRECT_TR'
#!/bin/sh
printf 'executed\n' > "$ASA_TEST_EXECUTION_MARKER"
BAD_DIRECT_TR
chmod 0777 "$scratch/bin/tr"
if ASA_TEST_EXECUTION_MARKER="$marker" PATH="$scratch/bin:/usr/bin:/bin" \
  plan_update > "$scratch/denied.out" 2>&1; then
  echo 'Direct updater accepted unsafe selected tr' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
if ASA_TEST_EXECUTION_MARKER="$marker" PATH="$scratch/bin:/usr/bin:/bin" \
  refuse_uninstall > "$scratch/denied.out" 2>&1; then
  echo 'Direct uninstaller accepted unsafe selected tr' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
[ ! -e "$marker" ] || { echo 'Unsafe direct helper command executed' >&2; exit 1; }
rm -f -- "$scratch/bin/tr"

# Purge-only command selection is also part of the guarded root command set:
# absent from ordinary update paths, userdel/groupdel must not be overlooked.
for destructive_command in userdel groupdel; do
  cat > "$scratch/bin/$destructive_command" <<'BAD_DESTRUCTIVE'
#!/bin/sh
printf 'executed\n' > "$ASA_TEST_EXECUTION_MARKER"
BAD_DESTRUCTIVE
  chmod 0777 "$scratch/bin/$destructive_command"
  if ASA_TEST_EXECUTION_MARKER="$marker" PATH="$scratch/bin:/usr/bin:/bin" \
    refuse_uninstall > "$scratch/denied.out" 2>&1; then
    echo "Direct uninstaller accepted unsafe $destructive_command" >&2; exit 1
  fi
  grep -qF 'Privileged install path is writable by non-root users' "$scratch/denied.out"
  [ ! -e "$marker" ] || { echo 'Unsafe purge command executed before validation' >&2; exit 1; }
  rm -f -- "$scratch/bin/$destructive_command"
done

# Trusted Bash command shims must not receive inherited BASH_ENV or exported
# Bash functions; sanitized re-exec must preserve the synthetic FD3.
cat > "$scratch/bin/jq" <<'DIRECT_JQ'
#!/bin/bash
printf 'called\n' > "$ASA_TEST_DIRECT_JQ_WITNESS"
if /usr/bin/env | /usr/bin/grep -Eq '^(BASH_FUNC_asa_direct_env_probe%%|BASH_ENV)='; then
  printf 'leaked\n' > "$ASA_TEST_EXECUTION_MARKER"
fi
if declare -F asa_direct_env_probe >/dev/null; then asa_direct_env_probe; fi
[ "$ASA_TEST_EXPECT_CREDENTIAL_FD" = 1 ] && [ -r /proc/self/fd/3 ] || {
  printf 'fd-missing\n' > "$ASA_TEST_EXECUTION_MARKER"
  exit 1
}
exec /usr/bin/jq "$@"
DIRECT_JQ
cat > "$scratch/bin/tr" <<'DIRECT_TR'
#!/bin/bash
printf 'called\n' > "$ASA_TEST_DIRECT_TR_WITNESS"
if /usr/bin/env | /usr/bin/grep -Eq '^(BASH_FUNC_asa_direct_env_probe%%|BASH_ENV)='; then
  printf 'leaked\n' > "$ASA_TEST_EXECUTION_MARKER"
fi
if declare -F asa_direct_env_probe >/dev/null; then asa_direct_env_probe; fi
[ "$ASA_TEST_EXPECT_CREDENTIAL_FD" = 1 ] && [ -r /proc/self/fd/3 ] || {
  printf 'fd-missing\n' > "$ASA_TEST_EXECUTION_MARKER"
  exit 1
}
exec /usr/bin/tr "$@"
DIRECT_TR
chmod 0755 "$scratch/bin/jq" "$scratch/bin/tr"
asa_direct_env_probe(){ printf 'imported\n' > "$ASA_TEST_EXECUTION_MARKER"; }
export -f asa_direct_env_probe
(
  exec 3<<<'synthetic-nonsecret-fd3-payload'
  ASA_TEST_EXPECT_CREDENTIAL_FD=1 \
    ASA_TEST_EXECUTION_MARKER="$marker" \
    ASA_TEST_DIRECT_JQ_WITNESS="$scratch/direct-jq-called" \
    BASH_ENV="$scratch/startup.sh" PATH="$scratch/bin:/usr/bin:/bin" \
    plan_update > "$scratch/safe.out"
  if ASA_TEST_EXPECT_CREDENTIAL_FD=1 \
    ASA_TEST_EXECUTION_MARKER="$marker" \
    ASA_TEST_DIRECT_TR_WITNESS="$scratch/direct-tr-called" \
    BASH_ENV="$scratch/startup.sh" PATH="$scratch/bin:/usr/bin:/bin" \
    refuse_uninstall > "$scratch/denied.out" 2>&1; then
    echo 'Direct purge unexpectedly passed confirmation' >&2; exit 1
  fi
)
unset -f asa_direct_env_probe
[ -e "$scratch/direct-jq-called" ] || { echo 'Direct update trusted jq shim not reached' >&2; exit 1; }
[ -e "$scratch/direct-tr-called" ] || { echo 'Direct uninstall trusted tr shim not reached' >&2; exit 1; }
grep -qF 'confirmation requires a terminal' "$scratch/denied.out"
[ ! -e "$marker" ] || { echo 'Direct lifecycle inherited Bash execution state' >&2; exit 1; }
rm -f -- "$scratch/bin/jq" "$scratch/bin/tr"

# systemd must run the real generated guard before *both* service ExecStart
# paths. The aiagent service uses the read-only mode under its existing User=
# and ProtectSystem/NoNewPrivileges sandbox; Executor uses the same guarded
# script as root before the unchanged lifecycle-lock setup.
startup_guard="$library/ensure-lifecycle-lock.sh"
[ -x "$startup_guard" ] && [ "$(/usr/bin/stat -c '%u:%a' "$startup_guard")" = '0:755' ] ||
  { echo 'Installed systemd guard is not a trusted readable executable' >&2; exit 1; }
agent_prestart="$(systemctl show ai-server-agent.service -p ExecStartPre --value)"
executor_prestart="$(systemctl show ai-server-agent-executor.service -p ExecStartPre --value)"
[[ "$agent_prestart" == *"$startup_guard --verify-agent-binary"* ]] ||
  { echo 'Agent systemd unit does not use the non-root guarded preflight' >&2; exit 1; }
[[ "$executor_prestart" == *"$startup_guard"* && "$executor_prestart" != *--verify-agent-binary* ]] ||
  { echo 'Executor systemd unit does not use the root guarded preflight' >&2; exit 1; }
[ "$(systemctl show ai-server-agent.service -p User --value)" = aiagent ] ||
  { echo 'Agent unit lost its non-root service identity' >&2; exit 1; }
[ "$(systemctl show ai-server-agent.service -p NoNewPrivileges --value)" = yes ] ||
  { echo 'Agent unit lost NoNewPrivileges=yes' >&2; exit 1; }
[ "$(systemctl show ai-server-agent.service -p ProtectSystem --value)" = strict ] ||
  { echo 'Agent unit lost ProtectSystem=strict' >&2; exit 1; }
runuser -u aiagent -- "$startup_guard" --verify-agent-binary

# A previously trusted executable can drift *after installation*. The
# started service must refuse to launch it, not merely have a correct unit
# string. All mode drift and service stops are CI-only and restored.
systemctl stop ai-server-agent.service
chmod 0777 "$agent_binary"
if systemctl start ai-server-agent.service >"$scratch/agent-start-denied.out" 2>&1; then
  echo 'systemd Agent ExecStart accepted writable binary leaf' >&2; exit 1
fi
systemctl is-active --quiet ai-server-agent.service &&
  { echo 'Agent activated despite writable binary leaf' >&2; exit 1; }
if runuser -u aiagent -- "$startup_guard" --verify-agent-binary >"$scratch/agent-guard-denied.out" 2>&1; then
  echo 'Unprivileged start guard accepted writable executable' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users:' "$scratch/agent-guard-denied.out"
journalctl -u ai-server-agent.service -n 100 --no-pager -o cat >"$scratch/agent-start-journal.out"
grep -qF 'Privileged install path is writable by non-root users:' "$scratch/agent-start-journal.out" ||
  { echo 'Actual Agent systemd startup did not report a binary trust refusal' >&2; exit 1; }
systemctl stop ai-server-agent.service || :
chmod "$original_agent_mode" "$agent_binary"
systemctl reset-failed ai-server-agent.service
systemctl start ai-server-agent.service

# The privileged Executor must likewise reject a post-install writable
# /usr/local/bin ancestor before creating a lock or starting the root binary.
systemctl stop ai-server-agent.service ai-server-agent-executor.service
chmod 0777 "$agent_bin_dir"
if systemctl start ai-server-agent-executor.service >"$scratch/executor-start-denied.out" 2>&1; then
  echo 'systemd Executor ExecStart accepted writable Agent ancestor' >&2; exit 1
fi
systemctl is-active --quiet ai-server-agent-executor.service &&
  { echo 'Executor activated despite writable Agent ancestor' >&2; exit 1; }
if "$startup_guard" >"$scratch/executor-guard-denied.out" 2>&1; then
  echo 'Root startup guard accepted writable binary ancestor' >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users:' "$scratch/executor-guard-denied.out"
journalctl -u ai-server-agent-executor.service -n 100 --no-pager -o cat >"$scratch/executor-start-journal.out"
grep -qF 'Privileged install path is writable by non-root users:' "$scratch/executor-start-journal.out" ||
  { echo 'Actual Executor systemd startup did not report a binary trust refusal' >&2; exit 1; }
systemctl stop ai-server-agent-executor.service || :
chmod "$original_agent_dir_mode" "$agent_bin_dir"
systemctl reset-failed ai-server-agent.service ai-server-agent-executor.service
systemctl start ai-server-agent-executor.service ai-server-agent.service
for attempt in {1..40}; do
  if systemctl is-active --quiet ai-server-agent-executor.service &&
     systemctl is-active --quiet ai-server-agent.service &&
     curl -fsS --max-time 2 http://127.0.0.1:3210/healthz >/dev/null 2>&1; then break; fi
  sleep 0.25
done
systemctl is-active --quiet ai-server-agent-executor.service
systemctl is-active --quiet ai-server-agent.service
curl -fsS --max-time 2 http://127.0.0.1:3210/healthz >/dev/null

test "$(/usr/bin/sha256sum "$config")" = "$config_hash"
test "$(/usr/bin/sha256sum "$identity")" = "$identity_hash"
test "$(/usr/bin/sha256sum "$credentials")" = "$credentials_hash"
test "$(/usr/bin/stat -c '%a' "$agent_binary")" = "$original_agent_mode"
test "$(/usr/bin/stat -c '%a' "$agent_bin_dir")" = "$original_agent_dir_mode"
test "$(/usr/bin/stat -c '%a' "$library")" = "$original_library_mode"
test "$(/usr/bin/stat -c '%a' "$helper")" = "$original_helper_mode"
test "$(/usr/bin/stat -c '%a' "$uninstall_helper")" = "$original_uninstall_mode"
systemctl is-active --quiet ai-server-agent.service
systemctl is-active --quiet ai-server-agent-executor.service

echo 'ROOT_INSTALLED_MANAGER_DRIFT_TRUST_PASS'
