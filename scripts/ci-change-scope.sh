#!/usr/bin/env bash
set -Eeuo pipefail

runtime_changed=false
go_changed=false
browser_flow_changed=false
shell_changed=false
platform_changed=false
arm64_lifecycle_changed=false
cloudflare_security_changed=false
root_trust_security_changed=false
stable_provenance_changed=false
stable_update_trust_changed=false

set_all(){
  runtime_changed=true
  go_changed=true
  browser_flow_changed=true
  shell_changed=true
  platform_changed=true
  arm64_lifecycle_changed=true
  cloudflare_security_changed=true
  root_trust_security_changed=true
  stable_provenance_changed=true
  stable_update_trust_changed=true
}

set_security_all(){
  cloudflare_security_changed=true
  root_trust_security_changed=true
  stable_provenance_changed=true
  stable_update_trust_changed=true
}

emit_scope(){
  printf 'runtime_changed=%s\n' "$runtime_changed"
  printf 'go_changed=%s\n' "$go_changed"
  printf 'browser_flow_changed=%s\n' "$browser_flow_changed"
  printf 'shell_changed=%s\n' "$shell_changed"
  printf 'platform_changed=%s\n' "$platform_changed"
  printf 'arm64_lifecycle_changed=%s\n' "$arm64_lifecycle_changed"
  printf 'cloudflare_security_changed=%s\n' "$cloudflare_security_changed"
  printf 'root_trust_security_changed=%s\n' "$root_trust_security_changed"
  printf 'stable_provenance_changed=%s\n' "$stable_provenance_changed"
  printf 'stable_update_trust_changed=%s\n' "$stable_update_trust_changed"
}

classify_paths(){
  local count=0 path
  while IFS= read -r -d '' path; do
    count=$((count + 1))
    case "$path" in
      AGENTS.md|SECURITY.md|LICENSE|.gitignore|docs/*)
        ;;
      README.md)
        stable_provenance_changed=true
        ;;
      internal/browser/*|internal/manifest/*)
        runtime_changed=true
        go_changed=true
        browser_flow_changed=true
        ;;
      internal/audit/*|internal/config/*|internal/credential/*|internal/executor/*|internal/mcp/*|internal/policy/*|cmd/*)
        runtime_changed=true
        go_changed=true
        set_security_all
        ;;
      install.sh)
        runtime_changed=true
        shell_changed=true
        platform_changed=true
        arm64_lifecycle_changed=true
        root_trust_security_changed=true
        stable_provenance_changed=true
        ;;
      manage.sh)
        runtime_changed=true
        shell_changed=true
        arm64_lifecycle_changed=true
        cloudflare_security_changed=true
        root_trust_security_changed=true
        ;;
      update.sh)
        runtime_changed=true
        shell_changed=true
        root_trust_security_changed=true
        stable_update_trust_changed=true
        ;;
      uninstall.sh|ensure-lifecycle-lock.sh)
        runtime_changed=true
        shell_changed=true
        arm64_lifecycle_changed=true
        root_trust_security_changed=true
        ;;
      scripts/build-release.sh)
        runtime_changed=true
        shell_changed=true
        stable_provenance_changed=true
        ;;
      scripts/install-stable.sh)
        runtime_changed=true
        shell_changed=true
        platform_changed=true
        root_trust_security_changed=true
        stable_provenance_changed=true
        ;;
      scripts/release-arches.txt)
        runtime_changed=true
        platform_changed=true
        arm64_lifecycle_changed=true
        stable_provenance_changed=true
        ;;
      tests/cloudflare_transaction.sh|tests/cloudflare_crash_recovery.sh|tests/cloudflare_phase_recovery.sh|tests/cloudflare_rulesets_pagination.sh)
        runtime_changed=true
        shell_changed=true
        cloudflare_security_changed=true
        ;;
      tests/root_trust_boundary.sh|tests/root_trust_migration.sh)
        runtime_changed=true
        shell_changed=true
        arm64_lifecycle_changed=true
        root_trust_security_changed=true
        ;;
      tests/stable_bootstrap.sh)
        runtime_changed=true
        shell_changed=true
        root_trust_security_changed=true
        stable_provenance_changed=true
        ;;
      tests/platform_compatibility.sh)
        runtime_changed=true
        shell_changed=true
        platform_changed=true
        arm64_lifecycle_changed=true
        ;;
      go.mod|go.sum)
        runtime_changed=true
        go_changed=true
        platform_changed=true
        arm64_lifecycle_changed=true
        set_security_all
        ;;
      scripts/ci-change-scope.sh|tests/ci_change_scope.sh|.github/workflows/*)
        set_all
        ;;
      scripts/dev-check.sh)
        runtime_changed=true
        shell_changed=true
        ;;
      tests/*|scripts/*|*)
        set_all
        ;;
    esac
  done

  if [ "$count" -eq 0 ]; then
    set_all
  fi
  emit_scope
}

classify_range(){
  local base="$1" head="$2" sha
  for sha in "$base" "$head"; do
    git cat-file -e "$sha^{commit}" 2>/dev/null || {
      echo "unknown commit for validation scope: $sha" >&2
      exit 2
    }
  done
  git diff --check "$base" "$head"
  git diff --no-renames --name-only -z "$base" "$head" | classify_paths
}

if [ "${1:-}" = "--files" ]; then
  shift
  [ "$#" -gt 0 ] || { echo 'usage: ci-change-scope.sh --files PATH...' >&2; exit 2; }
  printf '%s\0' "$@" | classify_paths
  exit 0
fi

if [ "${1:-}" = "--range" ]; then
  [ "$#" -eq 3 ] || { echo 'usage: ci-change-scope.sh --range BASE HEAD' >&2; exit 2; }
  classify_range "$2" "$3"
  exit 0
fi

: "${GITHUB_EVENT_NAME:=}"
: "${GITHUB_EVENT_PATH:=}"
[ -n "$GITHUB_EVENT_PATH" ] && [ -r "$GITHUB_EVENT_PATH" ] || {
  set_all
  emit_scope
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
    set_all
    emit_scope
    exit 0
    ;;
esac

if [ -z "$base" ] || [ -z "$head" ] || [[ "$base" =~ ^0+$ ]]; then
  set_all
  emit_scope
  exit 0
fi

for sha in "$base" "$head"; do
  if ! git cat-file -e "$sha^{commit}" 2>/dev/null; then
    git fetch --no-tags --depth=1 origin "$sha"
  fi
done

classify_range "$base" "$head"
