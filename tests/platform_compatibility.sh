#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="$ROOT/install.sh"
BOOTSTRAP="$ROOT/scripts/install-stable.sh"
ARCH_FILE="$ROOT/scripts/release-arches.txt"

expect_pass(){
  local os_id="$1" version="$2" arch="$3" out
  out="$(bash "$INSTALLER" --check-platform "$os_id" "$version" "$arch")" || {
    echo "expected supported platform to pass: $os_id $version $arch" >&2
    return 1
  }
  grep -qF 'Compatibility check passed:' <<<"$out"
}

expect_fail(){
  local os_id="$1" version="$2" arch="$3" expected="$4" out
  if out="$(bash "$INSTALLER" --check-platform "$os_id" "$version" "$arch" 2>&1)"; then
    echo "expected unsupported platform to fail: $os_id $version $arch" >&2
    return 1
  fi
  grep -qF "$expected" <<<"$out"
}

installer_ubuntu="$(sed -n 's/^UBUNTU_MIN_VERSION=//p' "$INSTALLER" | head -n1)"
bootstrap_ubuntu="$(sed -n 's/^UBUNTU_MIN_VERSION=//p' "$BOOTSTRAP" | head -n1)"
installer_debian="$(sed -n 's/^DEBIAN_MIN_VERSION=//p' "$INSTALLER" | head -n1)"
bootstrap_debian="$(sed -n 's/^DEBIAN_MIN_VERSION=//p' "$BOOTSTRAP" | head -n1)"
[ "$installer_ubuntu" = "$bootstrap_ubuntu" ] || { echo 'Ubuntu minimum-version policy drifted between installer and bootstrap' >&2; exit 1; }
[ "$installer_debian" = "$bootstrap_debian" ] || { echo 'Debian minimum-version policy drifted between installer and bootstrap' >&2; exit 1; }

expect_pass ubuntu 22.04 x86_64
expect_pass ubuntu 24.04 amd64
expect_pass ubuntu 26.04 aarch64
expect_pass debian 11 amd64
expect_pass debian 12 arm64
expect_pass debian 13 aarch64

expect_fail ubuntu 20.04 amd64 'Ubuntu 22.04 or newer is required'
expect_fail debian 10 arm64 'Debian 11 or newer is required'
expect_fail alpine 3.22 amd64 'Unsupported OS: alpine'
expect_fail ubuntu 24.04 riscv64 'Unsupported architecture: riscv64'

mapfile -t release_arches < <(grep -Ev '^[[:space:]]*(#|$)' "$ARCH_FILE")
printf '%s\n' "${release_arches[@]}" | grep -qx amd64
printf '%s\n' "${release_arches[@]}" | grep -qx arm64
for arch in "${release_arches[@]}"; do
  expect_pass ubuntu "$installer_ubuntu" "$arch"
done

echo 'platform compatibility tests passed'
