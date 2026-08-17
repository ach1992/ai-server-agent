#!/usr/bin/env bash
set -Eeuo pipefail

REPO="ach1992/ai-server-agent"
API_VERSION="2026-03-10"
VERSION="${1:-}"

log(){ printf '[ai-server-agent] %s\n' "$*"; }
die(){ printf '[ai-server-agent] ERROR: %s\n' "$*" >&2; exit 1; }

case "$#" in
  0|1) ;;
  *) die "Usage: install-stable.sh [vMAJOR.MINOR.PATCH]" ;;
esac
if [ -n "$VERSION" ] && ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  die "Stable version must look like v0.1.2."
fi

for cmd in curl sha256sum mktemp sudo; do
  command -v "$cmd" >/dev/null 2>&1 || die "$cmd is required for the verified stable bootstrap."
done
if ! command -v jq >/dev/null 2>&1; then
  [ -r /etc/os-release ] || die "jq is required and /etc/os-release is unavailable for prerequisite installation."
  # shellcheck disable=SC1091
  . /etc/os-release
  [ "${ID:-}" = ubuntu ] && [ "${VERSION_ID:-}" = 22.04 ] || die "jq is required. Install jq before running this bootstrap on this system."
  log "Installing the jq prerequisite from Ubuntu repositories."
  sudo apt-get update -qq
  sudo apt-get install -y -qq ca-certificates jq >/dev/null
  command -v jq >/dev/null 2>&1 || die "jq installation did not provide the jq command."
fi

if [ -n "$VERSION" ]; then
  release_url="https://api.github.com/repos/$REPO/releases/tags/$VERSION"
else
  release_url="https://api.github.com/repos/$REPO/releases/latest"
fi

release_json="$(curl -fsSL \
  --proto '=https' --tlsv1.2 \
  -H 'Accept: application/vnd.github+json' \
  -H "X-GitHub-Api-Version: $API_VERSION" \
  "$release_url")" || die "Could not read GitHub stable release metadata."

if [ -n "$VERSION" ]; then
  jq -e --arg version "$VERSION" \
    '.tag_name == $version and .draft == false and .prerelease == false and .immutable == true' \
    >/dev/null <<<"$release_json" || die "Requested stable release is missing, mutable, draft, or prerelease: $VERSION"
else
  jq -e '.draft == false and .prerelease == false and .immutable == true' \
    >/dev/null <<<"$release_json" || die "Latest GitHub release is not a published immutable stable release."
  VERSION="$(jq -r '.tag_name // empty' <<<"$release_json")"
  [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "Latest release tag is not a stable semantic version: $VERSION"
fi

asset_json="$(jq -ce '[.assets[]? | select(.name=="install.sh" and .state=="uploaded")] | select(length==1) | .[0]' <<<"$release_json")" || \
  die "Stable release $VERSION must contain exactly one uploaded install.sh asset."
installer_url="$(jq -r '.browser_download_url // empty' <<<"$asset_json")"
installer_digest="$(jq -r '.digest // empty' <<<"$asset_json")"
expected_url="https://github.com/$REPO/releases/download/$VERSION/install.sh"
[ "$installer_url" = "$expected_url" ] || die "Stable release install.sh asset URL does not match the exact release tag."
[[ "$installer_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "Stable release install.sh asset is missing a valid SHA-256 digest."

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
umask 077
curl -fsSL --proto '=https' --tlsv1.2 "$installer_url" -o "$tmp/install.sh" || die "Could not download the stable installer asset."
[ -s "$tmp/install.sh" ] || die "Downloaded stable installer asset is empty."
actual_digest="sha256:$(sha256sum "$tmp/install.sh" | awk '{print $1}')"
[ "$actual_digest" = "$installer_digest" ] || \
  die "Stable release install.sh digest mismatch: expected $installer_digest, got $actual_digest"
chmod 0500 "$tmp/install.sh"

log "Verified immutable stable installer $VERSION before privileged staging."
sudo bash -s -- "$tmp/install.sh" "$installer_digest" <<'ROOT_INSTALL'
set -Eeuo pipefail
source_installer="${1:?missing verified installer path}"
expected_digest="${2:?missing verified installer digest}"

[[ "$expected_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  printf '[ai-server-agent] ERROR: Invalid privileged staging digest.\n' >&2
  exit 1
}
for cmd in install mktemp sha256sum; do
  command -v "$cmd" >/dev/null 2>&1 || {
    printf '[ai-server-agent] ERROR: %s is required for privileged installer staging.\n' "$cmd" >&2
    exit 1
  }
done

root_stage="$(mktemp -d /var/tmp/ai-server-agent-bootstrap.XXXXXX)"
trap 'rm -rf -- "$root_stage"' EXIT
chown root:root "$root_stage"
chmod 0700 "$root_stage"
install -o root -g root -m 0500 -- "$source_installer" "$root_stage/install.sh"
actual_digest="sha256:$(sha256sum "$root_stage/install.sh" | awk '{print $1}')"
[ "$actual_digest" = "$expected_digest" ] || {
  printf '[ai-server-agent] ERROR: Privileged staging digest mismatch: expected %s, got %s\n' "$expected_digest" "$actual_digest" >&2
  exit 1
}
printf '[ai-server-agent] Privileged staging re-verified the authenticated installer bytes.\n'
bash "$root_stage/install.sh"
ROOT_INSTALL
