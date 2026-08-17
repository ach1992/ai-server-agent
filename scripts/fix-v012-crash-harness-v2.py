from pathlib import Path

path = Path('scripts/apply-v012-durable-cf-journal.py')
text = path.read_text()
start = text.find("crash = r'''#!/usr/bin/env bash")
if start < 0:
    raise SystemExit('crash template start missing')
end_marker = "'''\nPath(\"tests/cloudflare_crash_recovery.sh\").write_text(crash)"
end = text.find(end_marker, start)
if end < 0:
    raise SystemExit('crash template end missing')
new_crash = r"""crash = r'''#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; TLS_DIR="$CONFIG_DIR/tls"; CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"; REMOTE="$TMP/remote.json"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$TLS_DIR"
child="$TMP/crash-child.sh"
cat > "$child" <<'CHILD'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
AGENT_USER=root; CF_TOKEN=test
cf_new_ownership_marker(){ printf 'crashnonce0123456789abcdef01234567\n'; }
cf_api(){
  local method="$1" path="$2" body="${3:-}"
  if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then printf '%s' '{"success":true,"result":[]}'; return 0; fi
  if [ "$method" = POST ] && [ "$path" = /zones/zone1/dns_records ]; then
    printf '%s' "$body" | jq -c '. + {id:"dns-crashed"}' > "$REMOTE"
    while :; do sleep 1; done
  fi
  return 2
}
cf_reconcile_dns zone1 mcp.example.com 203.0.113.10 ""
CHILD
chmod +x "$child"

ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" setsid bash "$child" >/tmp/cloudflare-crash-child.out 2>&1 &
child_pid=$!
for _ in $(seq 1 50); do [ -s "$REMOTE" ] && break; sleep 0.1; done
test -s "$REMOTE" || { cat /tmp/cloudflare-crash-child.out >&2 || true; echo 'mock Cloudflare create did not reach remote-commit point' >&2; exit 1; }
kill -KILL -- "-$child_pid"
set +e
wait "$child_pid" >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ]
test -s "$CF_TXN_STATE"
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create
marker="$(jq -r '.pending.marker' "$CF_TXN_STATE")"
test -n "$marker"
test "$(jq -r '.comment' "$REMOTE")" = "Managed by AI Server Agent txn:$marker"

# New process: reload only the durable journal and remote state, then recover.
ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$ROOT/manage.sh"
  AGENT_USER=root; CF_TOKEN=test
  cf_api(){
    method="$1"; path="$2"
    if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then
      if [ -s "$REMOTE" ]; then jq -c "{success:true,result:[.] }" "$REMOTE"; else printf "%s" "{\"success\":true,\"result\":[]}"; fi
      return 0
    fi
    return 2
  }
  cf_delete_owned(){ case "$1" in /zones/zone1/dns_records/dns-crashed) rm -f "$REMOTE"; return 0 ;; *) return 1 ;; esac; }
  recover_cloudflare_transaction
  test ! -e "$CF_TXN_STATE"
  test ! -e "$REMOTE"
'

echo 'cloudflare crash-recovery test passed'
'''
"""
text = text[:start] + new_crash + text[end + 4:]
path.write_text(text)
print('crash harness process group isolation fixed')
