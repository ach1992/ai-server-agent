#!/usr/bin/env bash
set -Eeuo pipefail

# Exercise the real installer ref resolver without making a live GitHub API
# call. This is a read-only --resolve-ref path on a disposable CI runner.
cd "$(dirname "$0")/.."
command -v sudo >/dev/null || { echo 'sudo is required for the installer contract fixture' >&2; exit 1; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
chmod 0755 "$tmp" "$tmp/bin"

cat > "$tmp/bin/curl" <<'MOCK_CURL'
#!/usr/bin/env bash
set -Eeuo pipefail

# No fallback to real curl or the network: unexpected flags/URLs must fail.
[ "$#" -eq 6 ] && [ "$1" = '-fsSL' ] && [ "$2" = '-H' ] \
  && [ "$3" = 'Accept: application/vnd.github+json' ] \
  && [ "$4" = '-H' ] && [ "$5" = 'X-GitHub-Api-Version: 2022-11-28' ] || exit 96
base='https://api.github.com/repos/ach1992/ai-server-agent/commits'
case "$SOURCE_REF_TEST_SCENARIO:$6" in
  "main:$base/main"|"malformed:$base/main"|"denied:$base/main")
    ;;
  "slash:$base/feature%2Ftest")
    ;;
  *) exit 97 ;;
esac

case "$SOURCE_REF_TEST_SCENARIO" in
  main|slash)
    printf '{"sha":"97aa670f1e72279349aea557bf25abaf6c2fd7ab"}\n'
    ;;
  malformed)
    printf '{"sha":"invalid-sha"}\n'
    ;;
  denied)
    printf 'fixture: simulated HTTP 403\n' >&2
    exit 22
    ;;
  *) exit 98 ;;
esac
MOCK_CURL
chmod 0755 "$tmp/bin/curl"
fixture_path="$tmp/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
fixture_sha='97aa670f1e72279349aea557bf25abaf6c2fd7ab'

resolve(){
  local scenario="$1" ref="$2"
  # The explicit PATH overrides sudo's secure_path after transition to root.
  # The installer exits at --resolve-ref before any package/service mutation.
  # A denied localhost proxy also prevents an accidental real HTTPS curl
  # from reaching a live API if the command starts bypassing the PATH stub.
  sudo env PATH="$fixture_path" SOURCE_REF_TEST_SCENARIO="$scenario" \
    HTTPS_PROXY=http://127.0.0.1:1 HTTP_PROXY=http://127.0.0.1:1 \
    ALL_PROXY=http://127.0.0.1:1 NO_PROXY= \
    https_proxy=http://127.0.0.1:1 http_proxy=http://127.0.0.1:1 \
    all_proxy=http://127.0.0.1:1 no_proxy= \
    AI_SERVER_AGENT_REF="$ref" /usr/bin/bash install.sh --resolve-ref
}

test "$(resolve main main)" = "$fixture_sha"
test "$(resolve slash feature/test)" = "$fixture_sha"

# An already immutable ref must never require curl (or accept a substitute).
test "$(resolve forbidden "$fixture_sha")" = "$fixture_sha"

for scenario in malformed denied; do
  if output="$(resolve "$scenario" main 2>&1)"; then
    printf 'source ref %s unexpectedly accepted\n' "$scenario" >&2
    exit 1
  fi
  case "$scenario" in
    malformed) grep -qF "did not resolve to a commit SHA" <<<"$output" ;;
    denied) grep -qF "Could not resolve GitHub source ref" <<<"$output" ;;
  esac
done

# Even a syntactically valid ref may not cause an unexpected API request.
if resolve main unexpected/branch >/dev/null 2>&1; then
  echo 'unexpected source ref request passed the strict API fixture' >&2
  exit 1
fi

echo 'Deterministic source-ref resolution contract: PASS'
