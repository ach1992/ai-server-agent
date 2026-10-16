#!/usr/bin/env bash
set -Eeuo pipefail

# Standalone lifecycle helpers stay safe for scripts: no ANSI unless TTY,
# always include a textual severity and preserve the command exit status.
cli_message(){
  local severity="$1" color="" message
  shift
  message="$(printf '%s' "$*" | LC_ALL=C tr -d '\000-\037\177')"
  if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-dumb}" != dumb ]; then
    case "$severity" in
      SUCCESS) color=$'\033[32m' ;;
      WARNING) color=$'\033[33m' ;;
      ERROR) color=$'\033[31m' ;;
    esac
    printf '%s%s:%s %s\n' "$color" "$severity" $'\033[0m' "$message"
  else
    printf '[ai-server-agent] %s: %s\n' "$severity" "$message"
  fi
}
cli_error(){ cli_message ERROR "$*" >&2; }
cli_success(){ cli_message SUCCESS "$*"; }

[ "$(id -u)" -eq 0 ] || { cli_error "Run as root (for example: sudo bash update.sh)."; exit 1; }
REPO="ach1992/ai-server-agent"
DEFAULT_INSTALL_STATE="/etc/ai-server-agent/control/install-state.json"
INSTALL_STATE="${AI_SERVER_AGENT_INSTALL_STATE:-$DEFAULT_INSTALL_STATE}"
PLAN_ONLY=0
[ "${1:-}" = "--plan" ] && PLAN_ONLY=1

LIFECYCLE_LOCK_DIR=/run/lock/ai-server-agent
LIFECYCLE_LOCK="$LIFECYCLE_LOCK_DIR/management.lock"
acquire_lifecycle_lock(){
  command -v flock >/dev/null 2>&1 || { cli_error "flock is required for privileged lifecycle serialization"; exit 1; }
  [ ! -L "$LIFECYCLE_LOCK_DIR" ] || { cli_error "Refusing symlinked lifecycle lock directory: $LIFECYCLE_LOCK_DIR"; exit 1; }
  install -d -o root -g root -m 0700 "$LIFECYCLE_LOCK_DIR"
  chown root:root "$LIFECYCLE_LOCK_DIR"; chmod 0700 "$LIFECYCLE_LOCK_DIR"
  [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK_DIR" 2>/dev/null)" = "0:0:700" ] || { cli_error "Lifecycle lock directory ownership/mode is unsafe"; exit 1; }
  [ ! -L "$LIFECYCLE_LOCK" ] || { cli_error "Refusing symlinked lifecycle lock: $LIFECYCLE_LOCK"; exit 1; }
  ( umask 077; : >> "$LIFECYCLE_LOCK" )
  [ -f "$LIFECYCLE_LOCK" ] && [ ! -L "$LIFECYCLE_LOCK" ] || { cli_error "Lifecycle lock is not a regular file"; exit 1; }
  chown root:root "$LIFECYCLE_LOCK"; chmod 0600 "$LIFECYCLE_LOCK"
  [ "$(stat -c '%u:%g:%a' "$LIFECYCLE_LOCK" 2>/dev/null)" = "0:0:600" ] || { cli_error "Lifecycle lock ownership/mode is unsafe"; exit 1; }
  if [ "$(readlink /proc/$$/fd/9 2>/dev/null || true)" = "$LIFECYCLE_LOCK" ]; then
    flock -n 9 || { cli_error "Another AI Server Agent management operation is already active. Retry after it finishes."; exit 1; }
    return 0
  fi
  exec 9>>"$LIFECYCLE_LOCK"
  flock -n 9 || { cli_error "Another AI Server Agent management operation is already active. Retry after it finishes."; exit 1; }
}

CHANNEL="${AI_SERVER_AGENT_UPDATE_CHANNEL:-}"
VERSION=""
REF=""
TRACK_REF=""

load_install_state(){
  local file="$1" json
  [ -e "$file" ] || { cli_error "Trusted install state is missing: $file. Refusing to guess an update channel or ref."; exit 1; }
  [ -f "$file" ] && [ ! -L "$file" ] || { cli_error "Install state must be a regular non-symlink file: $file"; exit 1; }
  [ "$(stat -c '%s' "$file")" -le 4096 ] || { cli_error "Install state is unexpectedly large: $file"; exit 1; }
  json="$(cat -- "$file")" || { cli_error "Could not read install state: $file"; exit 1; }
  jq -e 'type=="object" and (keys|sort)==["channel","ref","track_ref","version"] and (.channel|type)=="string" and (.version|type)=="string" and (.ref|type)=="string" and (.track_ref|type)=="string"' >/dev/null <<<"$json" || {
    cli_error "Install state has an invalid schema: $file"
    exit 1
  }
  CHANNEL="$(jq -r '.channel' <<<"$json")"
  VERSION="$(jq -r '.version' <<<"$json")"
  REF="$(jq -r '.ref' <<<"$json")"
  TRACK_REF="$(jq -r '.track_ref' <<<"$json")"
  case "$CHANNEL" in
    stable)
      [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { cli_error "Invalid stable install version metadata"; exit 1; }
      [ "$REF" = "$VERSION" ] && [ "$TRACK_REF" = "$VERSION" ] || { cli_error "Stable install metadata must pin version/ref/track_ref to the same tag"; exit 1; }
      ;;
    source)
      [ "$VERSION" = source ] || { cli_error "Source install metadata must use version=source"; exit 1; }
      [[ "$REF" =~ ^([0-9a-f]{40}|binary)$ ]] || { cli_error "Invalid source install ref metadata"; exit 1; }
      [[ "$TRACK_REF" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]] || { cli_error "Invalid source install tracking ref metadata"; exit 1; }
      ;;
    *) cli_error "Invalid install channel metadata: $CHANNEL"; exit 1 ;;
  esac
}

load_install_state "$INSTALL_STATE"
CHANNEL="${AI_SERVER_AGENT_UPDATE_CHANNEL:-${CHANNEL:-source}}"
case "$CHANNEL" in stable|source) ;; *) cli_error "Invalid update channel: $CHANNEL"; exit 1 ;; esac
if [ "$PLAN_ONLY" -eq 0 ]; then acquire_lifecycle_lock; fi

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
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "https://api.github.com/repos/$REPO/commits/$encoded")" || { cli_error "Could not resolve GitHub source ref '$ref'"; exit 1; }
  sha="$(printf '%s' "$json" | grep -oE '"sha"[[:space:]]*:[[:space:]]*"[0-9a-f]{40}"' | head -n1 | grep -oE '[0-9a-f]{40}' || true)"
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { cli_error "GitHub source ref '$ref' did not resolve to a commit SHA"; exit 1; }
  printf '%s\n' "$sha"
}

latest_stable(){
  local json tag
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2026-03-10' "https://api.github.com/repos/$REPO/releases/latest")" || { cli_error "Could not read latest GitHub release"; exit 1; }
  jq -e '.draft == false and .prerelease == false and .immutable == true' >/dev/null <<<"$json" || { cli_error "Latest GitHub release is not a published immutable stable release"; exit 1; }
  tag="$(printf '%s' "$json" | jq -r '.tag_name // empty')"
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { cli_error "Latest release tag is not a stable semantic version: $tag"; exit 1; }
  printf '%s\n' "$tag"
}

verified_stable_release_json(){
  local version="$1" json
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2026-03-10' "https://api.github.com/repos/$REPO/releases/tags/$version")" || { cli_error "Could not read GitHub release $version"; exit 1; }
  jq -e --arg version "$version" '.tag_name == $version and .draft == false and .prerelease == false and .immutable == true' >/dev/null <<<"$json" || { cli_error "Stable update requires a published immutable release for $version"; exit 1; }
  printf '%s' "$json"
}

if [ "$CHANNEL" = "stable" ]; then
  TARGET_VERSION="${AI_SERVER_AGENT_VERSION:-}"
  [ -n "$TARGET_VERSION" ] || TARGET_VERSION="$(latest_stable)"
  [[ "$TARGET_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { cli_error "Stable updates require a tag like v0.1.1"; exit 1; }
  TARGET_REF="${AI_SERVER_AGENT_REF:-$TARGET_VERSION}"
  [ "$TARGET_REF" = "$TARGET_VERSION" ] || { cli_error "Stable update ref must match the stable version tag exactly ($TARGET_VERSION)."; exit 1; }
  TARGET_TRACK_REF="$TARGET_VERSION"
  [ -z "${AI_SERVER_AGENT_BINARY:-}" ] || { cli_error "AI_SERVER_AGENT_BINARY is disabled for stable updates"; exit 1; }
else
  TARGET_TRACK_REF="${AI_SERVER_AGENT_REF:-${TRACK_REF:-${REF:-main}}}"
  [[ "$TARGET_TRACK_REF" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]] || { cli_error "Invalid source update ref: $TARGET_TRACK_REF"; exit 1; }
  TARGET_REF="$(resolve_source_ref "$TARGET_TRACK_REF")"
  TARGET_VERSION=source
fi

printf '[ai-server-agent] Update channel: %s\n' "$CHANNEL"
printf '[ai-server-agent] Target version: %s\n' "$TARGET_VERSION"
printf '[ai-server-agent] Target ref: %s\n' "$TARGET_REF"
if [ "$PLAN_ONLY" -eq 1 ]; then exit 0; fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
if [ "$CHANNEL" = "stable" ]; then
  release_json="$(verified_stable_release_json "$TARGET_VERSION")"
  asset_json="$(jq -ce '[.assets[]? | select(.name=="install.sh" and .state=="uploaded")] | select(length==1) | .[0]' <<<"$release_json")" || {
    cli_error "Stable release $TARGET_VERSION must contain exactly one uploaded install.sh asset"
    exit 1
  }
  installer_url="$(jq -r '.browser_download_url // empty' <<<"$asset_json")"
  installer_digest="$(jq -r '.digest // empty' <<<"$asset_json")"
  expected_url="https://github.com/$REPO/releases/download/$TARGET_VERSION/install.sh"
  [ "$installer_url" = "$expected_url" ] || { cli_error "Stable release install.sh asset URL does not match the exact release tag"; exit 1; }
  [[ "$installer_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { cli_error "Stable release install.sh asset is missing a valid SHA-256 digest"; exit 1; }
  curl -fsSL "$installer_url" -o "$TMP/install.sh"
  actual_digest="sha256:$(sha256sum "$TMP/install.sh" | awk '{print $1}')"
  [ "$actual_digest" = "$installer_digest" ] || {
    cli_error "Stable release install.sh digest mismatch: expected $installer_digest, got $actual_digest"
    exit 1
  }
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

cli_success "Agent update completed on the '$CHANNEL' channel; Agent and Executor services are active."
