#!/usr/bin/env bash
set -Eeuo pipefail

# Run only as root on a disposable Ubuntu/Debian CI image. All mutated paths
# are private children of a newly created root-owned /run directory. Never
# chmod/chown the actual /usr/local hierarchy or an occupied installation.
[ "${EUID}" -eq 0 ] || { echo "root required for isolated installer trust fixture" >&2; exit 1; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[ -d /run ] && [ ! -L /run ] && [ "$(/usr/bin/stat -c '%u' /run)" = 0 ] ||
  { echo "root-owned /run required for isolated trust fixture" >&2; exit 1; }
scratch="$(mktemp -d /run/asa-install-path-test.XXXXXXXX)"
trap 'rm -rf -- "$scratch"' EXIT
chmod 0700 "$scratch"

# Exercise the exact production validator without sourcing/installing the
# installer (which is a standalone root installer, not an importable library).
probe="$scratch/probe.sh"
{
  printf '#!/usr/bin/env bash\nset -Eeuo pipefail\n'
  awk '/^root_path_violation\(\)\{/{emit=1} emit{print} emit && /^}/{exit}' "$ROOT/install.sh"
  awk '/^verify_privileged_install_path\(\)\{/{emit=1} emit{print} emit && /^}/{exit}' "$ROOT/install.sh"
  printf 'verify_privileged_install_path "$1" "$2"\n'
} > "$probe"
[ "$(grep -c '^verify_privileged_install_path(){' "$probe")" -eq 1 ] || {
  echo "could not extract real installer privileged-path validator" >&2; exit 1;
}
bash -n "$probe"

# The actual installer also applies this validator to every root PATH entry
# before invoking systemctl, curl, apt, installation helpers or lifecycle lock.
search_probe="$scratch/search-probe.sh"
{
  printf '#!/usr/bin/env bash\nset -Eeuo pipefail\n'
  awk '/^root_path_violation\(\)\{/{emit=1} emit{print} emit && /^}/{exit}' "$ROOT/install.sh"
  awk '/^verify_privileged_install_path\(\)\{/{emit=1} emit{print} emit && /^}/{exit}' "$ROOT/install.sh"
  awk '/^verify_privileged_search_path\(\)\{/{emit=1} emit{print} emit && /^}/{exit}' "$ROOT/install.sh"
  awk '/^verify_privileged_command_targets\(\)\{/{emit=1} emit{print} emit && /^}/{exit}' "$ROOT/install.sh"
  printf 'verify_privileged_search_path\nverify_privileged_command_targets\n'
} > "$search_probe"
[ "$(grep -c '^verify_privileged_search_path(){' "$search_probe")" -eq 1 ]
bash -n "$search_probe"

mkdir -m 0755 "$scratch/trusted" "$scratch/trusted/bin"
install -m 0755 /bin/true "$scratch/trusted/bin/agent"
bash "$probe" "$scratch/trusted/bin/agent" file
bash "$probe" "$scratch/trusted/bin/missing-agent" file
bash "$probe" "$scratch/trusted/bin" directory

denied(){
  local label="$1" path="$2" kind="$3"
  if bash "$probe" "$path" "$kind" > "$scratch/rejected.out" 2>&1; then
    echo "unsafe privileged install path accepted: $label ($path)" >&2
    exit 1
  fi
  grep -Eiq 'privileged install|unsafe symlink' "$scratch/rejected.out" || {
    echo "path rejected for wrong reason: $label" >&2; cat "$scratch/rejected.out" >&2; exit 1;
  }
}
chmod 0777 "$scratch/trusted/bin"
denied "world-writable ancestor" "$scratch/trusted/bin/agent" file
chmod 0755 "$scratch/trusted/bin"

chmod 0775 "$scratch/trusted"
denied "group-writable ancestor" "$scratch/trusted/bin/agent" file
chmod 0755 "$scratch/trusted"

chown 65534:65534 "$scratch/trusted/bin"
denied "non-root ancestor" "$scratch/trusted/bin/agent" file
chown 0:0 "$scratch/trusted/bin"

chmod 0666 "$scratch/trusted/bin/agent"
denied "writable executable" "$scratch/trusted/bin/agent" file
chmod 0755 "$scratch/trusted/bin/agent"

chown 65534:65534 "$scratch/trusted/bin/agent"
denied "non-root executable" "$scratch/trusted/bin/agent" file
chown 0:0 "$scratch/trusted/bin/agent"

ln -s "$scratch/trusted/bin" "$scratch/link-parent"
denied "symlinked ancestor" "$scratch/link-parent/agent" file
ln -s "$scratch/trusted/bin/agent" "$scratch/trusted/bin/link-agent"
denied "symlinked executable" "$scratch/trusted/bin/link-agent" file
denied "nonregular executable" "$scratch/trusted/bin" file
ln "$scratch/trusted/bin/agent" "$scratch/trusted/bin/hardlinked-agent"
denied "hardlinked privileged target" "$scratch/trusted/bin/agent" file
rm -f "$scratch/trusted/bin/hardlinked-agent"
denied "missing parent" "$scratch/missing/bin/agent" file
denied "parent traversal" "$scratch/trusted/../trusted/bin/agent" file

bash "$probe" "$scratch/trusted/bin/agent" file
/usr/bin/env PATH="$scratch/trusted/bin:/usr/bin:/bin" /usr/bin/bash "$search_probe"
if /usr/bin/env PATH="$scratch/trusted/bin:/tmp:/usr/bin" /usr/bin/bash "$search_probe" > "$scratch/path-rejected.out" 2>&1; then
  echo "root PATH accepted world-writable /tmp" >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users:' "$scratch/path-rejected.out"
if /usr/bin/env PATH="$scratch/link-parent:/usr/bin" /usr/bin/bash "$search_probe" > "$scratch/path-rejected.out" 2>&1; then
  echo "root PATH accepted symlinked search directory" >&2; exit 1
fi
grep -qF 'Unsafe symlink in privileged install path' "$scratch/path-rejected.out"
if /usr/bin/env PATH=":/usr/bin" /usr/bin/bash "$search_probe" > "$scratch/path-rejected.out" 2>&1; then
  echo "root PATH accepted an empty search component" >&2; exit 1
fi
grep -qF 'Refusing an empty/current-directory component in root PATH' "$scratch/path-rejected.out"

# Reviewer BLOCKER-01: line-oriented read previously validated only the
# first line. This must refuse controls before ANY selected command can run.
for hostile_path in $'/usr/bin\n:/tmp' $'/usr/bin\t:/bin' $'/usr/bin\r:/bin'; do
  if /usr/bin/env PATH="$hostile_path" /usr/bin/bash "$search_probe" > "$scratch/path-rejected.out" 2>&1; then
    echo "control-character root PATH bypassed complete parsing" >&2; exit 1
  fi
  grep -qF 'Refusing control characters in root PATH' "$scratch/path-rejected.out"
done
if /usr/bin/env PATH=$'/usr/bin\n:/tmp' /usr/bin/bash "$ROOT/install.sh" --check-platform ubuntu 22.04 x86_64 > "$scratch/platform-rejected.out" 2>&1; then
  echo "full root platform entrypoint accepted multiline PATH" >&2; exit 1
fi
grep -qF 'Refusing control characters in root PATH' "$scratch/platform-rejected.out"

# Reviewer REQUIRED-01: directory mode cannot vouch for selected command
# bytes. All fixtures live inside root-owned /run; no real executable is
# modified. A marker detects malicious tr execution before preflight.
marker="$scratch/untrusted-command-executed"
printf '#!/bin/sh\nprintf executed > %q\n' "$marker" > "$scratch/trusted/bin/tr"
chmod 0777 "$scratch/trusted/bin/tr"
if /usr/bin/env PATH="$scratch/trusted/bin:/usr/bin:/bin" /usr/bin/bash "$search_probe" > "$scratch/path-rejected.out" 2>&1; then
  echo "writable selected command leaf was accepted" >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/path-rejected.out"
[ ! -e "$marker" ] || { echo "untrusted command ran before rejection" >&2; exit 1; }

# Reviewer BLOCKER-02: the REAL root installer --check-platform entrypoint,
# not just an extracted helper, must fail before unsafe tr runs.
if /usr/bin/env PATH="$scratch/trusted/bin:/usr/bin:/bin" /usr/bin/bash "$ROOT/install.sh" --check-platform ubuntu 22.04 x86_64 > "$scratch/platform-rejected.out" 2>&1; then
  echo "root --check-platform executed through untrusted command PATH" >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/platform-rejected.out"
[ ! -e "$marker" ] || { echo "unsafe tr ran during root platform preflight" >&2; exit 1; }

chmod 0755 "$scratch/trusted/bin/tr"
chown 65534:65534 "$scratch/trusted/bin/tr"
if /usr/bin/env PATH="$scratch/trusted/bin:/usr/bin:/bin" /usr/bin/bash "$search_probe" > "$scratch/path-rejected.out" 2>&1; then
  echo "non-root selected command leaf was accepted" >&2; exit 1
fi
grep -qF 'not root-owned' "$scratch/path-rejected.out"
chown 0:0 "$scratch/trusted/bin/tr"
rm -f "$scratch/trusted/bin/tr"

mkdir -m 0777 "$scratch/untrusted"
printf '#!/bin/sh\nexit 0\n' > "$scratch/untrusted/tr"
chmod 0755 "$scratch/untrusted/tr"
ln -s "$scratch/untrusted/tr" "$scratch/trusted/bin/tr"
if /usr/bin/env PATH="$scratch/trusted/bin:/usr/bin:/bin" /usr/bin/bash "$search_probe" > "$scratch/path-rejected.out" 2>&1; then
  echo "selected command symlink escaped to an untrusted target" >&2; exit 1
fi
grep -qF 'Privileged install path is writable by non-root users' "$scratch/path-rejected.out"
rm -f "$scratch/trusted/bin/tr"

# An explicitly trusted root-owned /run shim remains supported. This
# preserves real systemctl and curl CI fault/digest fixtures.
printf '#!/bin/sh\nexit 0\n' > "$scratch/trusted/bin/curl"
printf '#!/bin/sh\nexit 0\n' > "$scratch/trusted/bin/systemctl"
chmod 0755 "$scratch/trusted/bin/curl" "$scratch/trusted/bin/systemctl"
/usr/bin/env PATH="$scratch/trusted/bin:/usr/bin:/bin" /usr/bin/bash "$search_probe"

# Validate root platform success and rejection using the full installer.
# Neither path may acquire a lifecycle lock, install packages or touch
# services/configuration; this is a read-only platform contract.
/usr/bin/env PATH=/usr/bin:/bin /usr/bin/bash "$ROOT/install.sh" --check-platform ubuntu 22.04 x86_64 > "$scratch/platform-valid.out"
grep -qF 'Compatibility check passed' "$scratch/platform-valid.out"
if /usr/bin/env PATH=/usr/bin:/bin /usr/bin/bash "$ROOT/install.sh" --check-platform unknown-os 22.04 x86_64 > "$scratch/platform-invalid.out" 2>&1; then
  echo "unsupported root platform unexpectedly accepted" >&2; exit 1
fi
grep -qF 'Unsupported OS' "$scratch/platform-invalid.out"

echo "ROOT_PRIVILEGED_INSTALL_PATH_PREFLIGHT_PASS"
