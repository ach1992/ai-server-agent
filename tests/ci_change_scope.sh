#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCOPE="$ROOT/scripts/ci-change-scope.sh"

scope_for(){
  bash "$SCOPE" --files "$@"
}

expect_flag(){
  local expected="$1" flag="$2"
  shift 2
  local output actual
  output="$(scope_for "$@")"
  actual="$(printf '%s\n' "$output" | sed -n "s/^$flag=//p")"
  [ "$actual" = "$expected" ] || {
    echo "unexpected $flag for $*: $actual (expected $expected)" >&2
    printf '%s\n' "$output" >&2
    exit 1
  }
}

expect_docs_only(){
  local paths=("$@")
  expect_flag false runtime_changed "${paths[@]}"
  expect_flag false browser_flow_changed "${paths[@]}"
  expect_flag false platform_changed "${paths[@]}"
  expect_flag false cloudflare_security_changed "${paths[@]}"
  expect_flag false root_trust_security_changed "${paths[@]}"
  expect_flag false stable_provenance_changed "${paths[@]}"
  expect_flag false stable_update_trust_changed "${paths[@]}"
}

expect_docs_only AGENTS.md SECURITY.md LICENSE .gitignore docs/ARCHITECTURE.md
expect_docs_only docs/nested/example.md

expect_flag false runtime_changed README.md
expect_flag true stable_provenance_changed README.md

expect_flag true runtime_changed internal/browser/browser.go
expect_flag true go_changed internal/browser/browser.go
expect_flag true browser_flow_changed internal/browser/browser.go
expect_flag false browser_flow_changed internal/mcp/server.go
expect_flag false platform_changed internal/browser/browser.go
expect_flag false cloudflare_security_changed internal/browser/browser.go
expect_flag false root_trust_security_changed internal/browser/browser.go
expect_flag false stable_provenance_changed internal/browser/browser.go
expect_flag false stable_update_trust_changed internal/browser/browser.go

for path in internal/mcp/server.go internal/executor/executor.go internal/credential/credential.go cmd/ai-server-agent/main.go; do
  expect_flag true runtime_changed "$path"
  expect_flag true cloudflare_security_changed "$path"
  expect_flag true root_trust_security_changed "$path"
  expect_flag true stable_provenance_changed "$path"
  expect_flag true stable_update_trust_changed "$path"
done

expect_flag true platform_changed install.sh
expect_flag true arm64_lifecycle_changed install.sh
expect_flag true root_trust_security_changed install.sh
expect_flag true stable_provenance_changed install.sh
expect_flag false stable_update_trust_changed install.sh

expect_flag true cloudflare_security_changed manage.sh
expect_flag true root_trust_security_changed manage.sh
expect_flag false stable_provenance_changed manage.sh

expect_flag true root_trust_security_changed update.sh
expect_flag true stable_update_trust_changed update.sh
expect_flag false cloudflare_security_changed update.sh

expect_flag true platform_changed tests/platform_compatibility.sh
expect_flag true cloudflare_security_changed tests/cloudflare_transaction.sh
expect_flag true root_trust_security_changed tests/root_trust_boundary.sh
expect_flag true stable_provenance_changed tests/stable_bootstrap.sh

expect_flag true runtime_changed docs/TESTING.md internal/browser/browser.go
expect_flag true stable_provenance_changed README.md internal/browser/browser.go
expect_flag false cloudflare_security_changed README.md internal/browser/browser.go

for path in .github/workflows/ci.yml scripts/ci-change-scope.sh tests/ci_change_scope.sh new-unknown-root-file.conf internal/new-security-unknown/file.go; do
  expect_flag true runtime_changed "$path"
  expect_flag true browser_flow_changed "$path"
  expect_flag true platform_changed "$path"
  expect_flag true arm64_lifecycle_changed "$path"
  expect_flag true cloudflare_security_changed "$path"
  expect_flag true root_trust_security_changed "$path"
  expect_flag true stable_provenance_changed "$path"
  expect_flag true stable_update_trust_changed "$path"
done

range_tmp="$(mktemp -d)"
trap 'rm -rf "$range_tmp"' EXIT
git -C "$range_tmp" init -q
git -C "$range_tmp" config user.name 'scope-test'
git -C "$range_tmp" config user.email 'scope-test@example.invalid'
mkdir -p "$range_tmp/docs"
printf 'base\n' > "$range_tmp/docs/base.md"
git -C "$range_tmp" add docs/base.md
git -C "$range_tmp" commit -q -m base
range_base="$(git -C "$range_tmp" rev-parse HEAD)"
printf 'docs\n' > "$range_tmp/docs/change.md"
git -C "$range_tmp" add docs/change.md
git -C "$range_tmp" commit -q -m docs
range_output="$(cd "$range_tmp" && bash "$SCOPE" --range "$range_base" HEAD)"
[ "$(printf '%s\n' "$range_output" | sed -n 's/^runtime_changed=//p')" = false ] || {
  echo 'docs-only --range classification was not lightweight' >&2
  printf '%s\n' "$range_output" >&2
  exit 1
}

mkdir -p "$range_tmp/internal/browser"
printf 'package browser\n' > "$range_tmp/internal/browser/renamed.go"
git -C "$range_tmp" add internal/browser/renamed.go
git -C "$range_tmp" commit -q -m runtime
rename_base="$(git -C "$range_tmp" rev-parse HEAD)"
git -C "$range_tmp" mv internal/browser/renamed.go docs/renamed.go
git -C "$range_tmp" commit -q -m rename
range_output="$(cd "$range_tmp" && bash "$SCOPE" --range "$rename_base" HEAD)"
[ "$(printf '%s\n' "$range_output" | sed -n 's/^runtime_changed=//p')" = true ] || {
  echo 'runtime-to-docs rename hid the runtime source path' >&2
  printf '%s\n' "$range_output" >&2
  exit 1
}

echo 'CI change-scope tests passed'
