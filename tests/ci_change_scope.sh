#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCOPE="$ROOT/scripts/ci-change-scope.sh"

expect_scope(){
  local expected="$1"; shift
  local actual
  actual="$(bash "$SCOPE" --files "$@")"
  [ "$actual" = "runtime_changed=$expected" ] || {
    echo "unexpected scope for $*: $actual (expected runtime_changed=$expected)" >&2
    exit 1
  }
}

expect_scope false README.md docs/ARCHITECTURE.md AGENTS.md SECURITY.md LICENSE .gitignore
expect_scope false docs/nested/example.md
expect_scope true internal/mcp/server.go
expect_scope true install.sh
expect_scope true go.mod
expect_scope true .github/workflows/ci.yml
expect_scope true scripts/ci-change-scope.sh
expect_scope true tests/ci_change_scope.sh

echo 'CI change-scope tests passed'
