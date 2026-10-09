#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCOPE="$ROOT/scripts/ci-change-scope.sh"
BASE=''
HEAD_REF=HEAD
PLAN_ONLY=false

usage(){
  cat <<'USAGE'
usage: scripts/dev-check.sh [--base REF] [--head REF] [--plan]

Runs safe local pre-push validation selected from the same conservative path
classifier used by GitHub Actions. Privileged/system lifecycle suites remain CI
or dedicated-test-environment responsibilities; this helper reports them.
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --base)
      [ "$#" -ge 2 ] || { usage >&2; exit 2; }
      BASE="$2"
      shift 2
      ;;
    --head)
      [ "$#" -ge 2 ] || { usage >&2; exit 2; }
      HEAD_REF="$2"
      shift 2
      ;;
    --plan)
      PLAN_ONLY=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

cd "$ROOT"

if [ -z "$BASE" ]; then
  if git rev-parse --verify --quiet refs/remotes/origin/main >/dev/null; then
    BASE=origin/main
  elif git rev-parse --verify --quiet refs/heads/main >/dev/null; then
    BASE=main
  else
    echo 'cannot infer base; pass --base REF' >&2
    exit 2
  fi
fi

git rev-parse --verify "$BASE^{commit}" >/dev/null
git rev-parse --verify "$HEAD_REF^{commit}" >/dev/null

scope="$(bash "$SCOPE" --range "$BASE" "$HEAD_REF")"
printf '%s\n' "$scope"

flag(){
  local name="$1"
  printf '%s\n' "$scope" | sed -n "s/^$name=//p"
}

cat <<EOF_PLAN
local_plan:
  go_checks=$(flag go_changed)
  shell_checks=$(flag shell_changed)
  normal_ci=$(flag runtime_changed)
  debian_platform=$(flag platform_changed)
  arm64_lifecycle=$(flag arm64_lifecycle_changed)
  security_cloudflare=$(flag cloudflare_security_changed)
  security_root_trust=$(flag root_trust_security_changed)
  security_stable_provenance=$(flag stable_provenance_changed)
  security_stable_update=$(flag stable_update_trust_changed)
EOF_PLAN

if [ "$PLAN_ONLY" = true ]; then
  exit 0
fi

bash -n scripts/ci-change-scope.sh scripts/dev-check.sh tests/ci_change_scope.sh
bash tests/ci_change_scope.sh

if [ "$(flag go_changed)" = true ]; then
  unformatted="$(gofmt -l .)"
  if [ -n "$unformatted" ]; then
    echo 'gofmt required for:' >&2
    printf '%s\n' "$unformatted" >&2
    exit 1
  fi
  go vet ./...
  cc="${CC:-$(go env CC)}"
  if [ -n "$cc" ] && command -v "${cc%% *}" >/dev/null 2>&1; then
    CGO_ENABLED=1 go test -race -vet=off ./...
  else
    go test ./...
    echo "Local race tests deferred to CI: no C compiler is available."
  fi
fi

if [ "$(flag shell_changed)" = true ]; then
  bash -n install.sh update.sh uninstall.sh manage.sh ensure-lifecycle-lock.sh scripts/*.sh tests/*.sh
fi

echo 'Local development checks passed.'
echo 'Privileged/platform/security suites shown in local_plan remain CI or dedicated-test-environment evidence.'
