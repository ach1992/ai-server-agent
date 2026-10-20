#!/usr/bin/env bash
set -Eeuo pipefail

# A disposable GitHub-hosted runner can start with root:root 0777
# /usr/local/bin. Production refuses that state; CI must explicitly
# normalize ONLY the fresh runner after a preflight-negative proof.
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] &&
  [ "${EUID:-}" -eq 0 ] || {
    echo "Refusing to normalize privileged paths outside a root GitHub-hosted disposable runner" >&2
    exit 1
  }
for path in / /usr /usr/local; do
  [ -d "$path" ] && [ ! -L "$path" ] &&
    [ "$(/usr/bin/stat -c '%u:%g' -- "$path")" = "0:0" ] || {
      echo "Unsafe ancestor $path; refusing CI preparation" >&2
      exit 1
    }
  mode="$(/usr/bin/stat -c '%a' -- "$path")"
  (( (8#$mode & 0022) == 0 )) || {
    echo "Unsafe shared ancestor $path; refusing CI preparation" >&2
    exit 1
  }
done

for path in /usr/local/bin /usr/local/sbin /usr/local/lib; do
  [ -d "$path" ] && [ ! -L "$path" ] &&
    [ "$(/usr/bin/stat -c '%u:%g' -- "$path")" = "0:0" ] || {
      echo "Unsafe CI directory $path; refusing preparation" >&2
      exit 1
    }
  # The directory was proven root-owned with a trusted parent, so only
  # root may replace the inode. No real/occupied host calls this script.
  chmod 0755 -- "$path"
  [ "$(/usr/bin/stat -c '%u:%g:%a' -- "$path")" = "0:0:755" ] || {
    echo "Cannot verify trusted CI directory $path" >&2
    exit 1
  }
done
echo "DISPOSABLE_ROOT_INSTALL_PATHS_PREPARED"
