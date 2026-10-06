#!/usr/bin/env bash
set -Eeuo pipefail

emit_runtime(){
  printf 'runtime_changed=%s\n' "$1"
}

classify_paths(){
  local runtime=false count=0 path
  while IFS= read -r -d '' path; do
    count=$((count + 1))
    case "$path" in
      README.md|AGENTS.md|SECURITY.md|LICENSE|.gitignore|docs/*)
        ;;
      *)
        runtime=true
        ;;
    esac
  done

  # An empty/unknown change set must fail safe to full validation.
  if [ "$count" -eq 0 ]; then
    runtime=true
  fi
  emit_runtime "$runtime"
}

if [ "${1:-}" = "--files" ]; then
  shift
  [ "$#" -gt 0 ] || { echo 'usage: ci-change-scope.sh --files PATH...' >&2; exit 2; }
  printf '%s\0' "$@" | classify_paths
  exit 0
fi

: "${GITHUB_EVENT_NAME:=}"
: "${GITHUB_EVENT_PATH:=}"
[ -n "$GITHUB_EVENT_PATH" ] && [ -r "$GITHUB_EVENT_PATH" ] || {
  emit_runtime true
  exit 0
}

base=''
head=''
case "$GITHUB_EVENT_NAME" in
  pull_request)
    base="$(jq -r '.pull_request.base.sha // empty' "$GITHUB_EVENT_PATH")"
    head="$(jq -r '.pull_request.head.sha // empty' "$GITHUB_EVENT_PATH")"
    ;;
  push)
    base="$(jq -r '.before // empty' "$GITHUB_EVENT_PATH")"
    head="$(jq -r '.after // empty' "$GITHUB_EVENT_PATH")"
    ;;
  *)
    emit_runtime true
    exit 0
    ;;
esac

if [ -z "$base" ] || [ -z "$head" ] || [[ "$base" =~ ^0+$ ]]; then
  emit_runtime true
  exit 0
fi

for sha in "$base" "$head"; do
  if ! git cat-file -e "$sha^{commit}" 2>/dev/null; then
    git fetch --no-tags --depth=1 origin "$sha"
  fi
done

# Keep even documentation-only changes subject to a meaningful exact-diff check.
git diff --check "$base" "$head"

# Disable rename detection so moving a runtime file into docs cannot hide the
# deleted runtime path from classification.
git diff --no-renames --name-only -z "$base" "$head" | classify_paths
