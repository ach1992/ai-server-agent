#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
CONFIG_DIR="$(mktemp -d)"
trap 'rm -rf "$CONFIG_DIR"' EXIT
CONFIG_FILE="$CONFIG_DIR/config.json"
printf '{"listen_address":"127.0.0.1:3210","mcp_path":"/mcp","tls_cert_file":""}\n' > "$CONFIG_FILE"
current_port(){ printf '3210\n'; }
curl(){
  local out="" prev="" x
  for x in "$@"; do
    if [ "$prev" = "-o" ]; then out="$x"; fi
    prev="$x"
  done
  printf '%s' "$MOCK_BODY" > "$out"
  printf '%s' "$MOCK_STATUS"
  [ "$MOCK_FAILURE" -eq 0 ]
}
MOCK_BODY='{"schema_version":1,"purpose":"test","critical_components":[]}'
MOCK_FAILURE=0
MOCK_STATUS=200
verify_mcp_token_local "test" || { echo 'expected valid response' >&2; exit 1; }
for MOCK_STATUS in 400 401 404 500 503 000; do
  if verify_mcp_token_local "test"; then echo "accepted $MOCK_STATUS" >&2; exit 1; fi
done
MOCK_STATUS=200
MOCK_BODY='{"error":"unexpected"}'
if verify_mcp_token_local "test"; then echo 'accepted unexpected body' >&2; exit 1; fi
MOCK_BODY='{"schema_version":1,"purpose":"test","critical_components":[]}'
MOCK_FAILURE=1
if verify_mcp_token_local "test"; then echo 'accepted transport failure' >&2; exit 1; fi
printf 'credential activation response tests: PASS\n'
