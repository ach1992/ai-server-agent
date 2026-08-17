#!/usr/bin/env bash
set -Eeuo pipefail

[ "$(id -u)" -eq 0 ] || { echo "Run as root" >&2; exit 1; }
REPO="ach1992/ai-server-agent"
DEFAULT_INSTALL_STATE="/etc/ai-server-agent/control/install-state.json"
INSTALL_STATE="${AI_SERVER_AGENT_INSTALL_STATE:-$DEFAULT_INSTALL_STATE}"
PLAN_ONLY=0
[ "${1:-}" = "--plan" ] && PLAN_ONLY=1

CHANNEL="${AI_SERVER_AGENT_UPDATE_CHANNEL:-}"
VERSION=""
REF=""
TRACK_REF=""

load_install_state(){
  local file="$1" json keys
  [ -e "$file" ] || return 0
  [ -f "$file" ] && [ ! -L "$file" ] || { echo "Install state must be a regular non-symlink file: $file" >&2; exit 1; }
  [ "$(stat -c '%s' "$file")" -le 4096 ] || { echo "Install state is unexpectedly large: $file" >&2; exit 1; }
  json="$(cat -- "$file")" || { echo "Could not read install state: $file" >&2; exit 1; }
  jq -e 'type=="object" and (keys|sort)==["channel","ref","track_ref","version"] and (.channel|type)=="string" and (.version|type)=="string" and (.ref|type)=="string" and (.track_ref|type)=="string"' >/dev/null <<<"$json" || {
    echo "Install state has an invalid schema: $file" >&2
    exit 1
  }
  CHANNEL="$(jq -r '.channel' <<<"$json")"
  VERSION="$(jq -r '.version' <<<"$json")"
  REF="$(jq -r '.ref' <<<"$json")"
  TRACK_REF="$(jq -r '.track_ref' <<<"$json")"
  case "$CHANNEL" in
    stable)
      [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "Invalid stable install version metadata" >&2; exit 1; }
      [ "$REF" = "$VERSION" ] && [ "$TRACK_REF" = "$VERSION" ] || { echo "Stable install metadata must pin version/ref/track_ref to the same tag" >&2; exit 1; }
      ;;
    source)
      [ "$VERSION" = source ] || { echo "Source install metadata must use version=source" >&2; exit 1; }
      [[ "$REF" =~ ^([0-9a-f]{40}|binary)$ ]] || { echo "Invalid source install ref metadata" >&2; exit 1; }
      [[ "$TRACK_REF" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]] || { echo "Invalid source install tracking ref metadata" >&2; exit 1; }
      ;;
    *) echo "Invalid install channel metadata: $CHANNEL" >&2; exit 1 ;;
  esac
}

load_install_state "$INSTALL_STATE"
CHANNEL="${AI_SERVER_AGENT_UPDATE_CHANNEL:-${CHANNEL:-source}}"

case "$CHANNEL" in stable|source) ;; *) echo "Invalid update channel: $CHANNEL" >&2; exit 1 ;; esac

urlencode_ref(){
  local input="$1" output="" ch hex i
  LC_ALL=C
  for ((i=0; i<${#input}; i++)); do
    ch="${input:i:1}"
    case "$ch" in [a-zA-Z0-9.~_-]) output+="$ch" ;; *) printf -v hex '%%%02X' "'$ch"; output+="$hex" ;; esac
  done
  printf '%s' "$output"
}

resolve_source_ref(){
  local ref="$1" encoded json sha
  if [[ "$ref" =~ ^[0-9a-fA-F]{40}$ ]]; then printf '%s\n' "${ref,,}"; return 0; fi
  encoded="$(urlencode_ref "$ref")"
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "https://api.github.com/repos/$REPO/commits/$encoded")" || { echo "Could not resolve GitHub source ref '$ref'" >&2; exit 1; }
  sha="$(printf '%s' "$json" | grep -oE '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]{40}"' | head -n1 | grep -oE '[0-9a-f]{40}' || true)"
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "GitHub source ref '$ref' did not resolve to a commit SHA" >&2; exit 1; }
  printf '%s\n' "$sha"
}

latest_stable(){
  local json tag
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2026-03-10' "https://api.github.com/repos/$REPO/releases/latest")" || { echo "Could not read latest GitHub release" >&2; exit 1; }
  jq -e '.draft == false and .prerelease == false and .immutable == true' >/dev/null <<<"$json" || { echo "Latest GitHub release is not a published immutable stable release" >&2; exit 1; }
  tag="$(printf '%s' "$json" | jq -r '.tag_name // empty')"
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "Latest release tag is not a stable semantic version: $tag" >&2; exit 1; }
  printf '%s\n' "$tag"
}

verify_stable_release(){
  local version="$1" json
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2026-03-10' "https://api.github.com/repos/$REPO/releases/tags/$version")" || { echo "Could not read GitHub release $version" >&2; exit 1; }
  jq -e --arg version "$version" '.tag_name == $version and .draft == false and .prerelease == false and .immutable == true' >/dev/null <<<"$json" || { echo "Stable update requires a published immutable release for $version" >&2; exit 1; }
}

if [ "$CHANNEL" = "stable" ]; then
  TARGET_VERSION="${AI_SERVER_AGENT_VERSION:-}"
  [ -n "$TARGET_VERSION" ] || TARGET_VERSION="$(latest_stable)"
  [[ "$TARGET_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "Stable updates require a tag like v0.1.1" >&2; exit 1; }
  TARGET_REF="${AI_SERVER_AGENT_REF:-$TARGET_VERSION}"
  [ "$TARGET_REF" = "$TARGET_VERSION" ] || { echo "Stable update ref must match the stable version tag exactly ($TARGET_VERSION)." >&2; exit 1; }
  TARGET_TRACK_REF="$TARGET_VERSION"
  [ -z "${AI_SERVER_AGENT_BINARY:-}" ] || { echo "AI_SERVER_AGENT_BINARY is disabled for stable updates" >&2; exit 1; }
else
  TARGET_TRACK_REF="${AI_SERVER_AGENT_REF:-${TRACK_REF:-${REF:-main}}}"
  [[ "$TARGET_TRACK_REF" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]] || { echo "Invalid source update ref: $TARGET_TRACK_REF" >&2; exit 1; }
  TARGET_REF="$(resolve_source_ref "$TARGET_TRACK_REF")"
  TARGET_VERSION=source
fi

printf '[ai-server-agent] Update channel: %s\n' "$CHANNEL"
printf '[ai-server-agent] Target version: %s\n' "$TARGET_VERSION"
printf '[ai-server-agent] Target ref: %s\n' "$TARGET_REF"
if [ "$PLAN_ONLY" -eq 1 ]; then exit 0; fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
if [ "$CHANNEL" = "stable" ]; then
  verify_stable_release "$TARGET_VERSION"
  curl -fsSL "https://github.com/$REPO/releases/download/$TARGET_VERSION/install.sh" -o "$TMP/install.sh"
  grep -qF "export AI_SERVER_AGENT_VERSION=$TARGET_VERSION" "$TMP/install.sh" || { echo "Release installer does not pin $TARGET_VERSION" >&2; exit 1; }
  grep -qF "export AI_SERVER_AGENT_REF=$TARGET_VERSION" "$TMP/install.sh" || { echo "Release installer ref does not pin $TARGET_VERSION" >&2; exit 1; }
  chmod +x "$TMP/install.sh"
  AI_SERVER_AGENT_NONINTERACTIVE=1 \
  AI_SERVER_AGENT_SETUP_MODE=keep \
    bash "$TMP/install.sh"
else
  curl -fsSLG \
    -H 'Accept: application/vnd.github.raw+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    --data-urlencode "ref=$TARGET_REF" \
    "https://api.github.com/repos/$REPO/contents/install.sh" \
    -o "$TMP/install.sh"
  chmod +x "$TMP/install.sh"
  AI_SERVER_AGENT_VERSION="$TARGET_VERSION" \
  AI_SERVER_AGENT_REF="$TARGET_REF" \
  AI_SERVER_AGENT_TRACK_REF="$TARGET_TRACK_REF" \
  AI_SERVER_AGENT_NONINTERACTIVE=1 \
  AI_SERVER_AGENT_SETUP_MODE=keep \
    bash "$TMP/install.sh"
fi

sleep 1
systemctl is-active --quiet ai-server-agent-executor.service
systemctl is-active --quiet ai-server-agent.service

echo "AI Server Agent update completed on the '$CHANNEL' channel."
