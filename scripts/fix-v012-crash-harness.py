from pathlib import Path

path = Path('scripts/apply-v012-durable-cf-journal.py')
text = path.read_text()
old = '''set +e
ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c '\n  set -Eeuo pipefail\n  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1\n  source "$ROOT/manage.sh"\n  AGENT_USER=root; CF_TOKEN=test\n  cf_new_ownership_marker(){ printf "crashnonce0123456789abcdef01234567\\\\n"; }\n  cf_api(){\n    method="$1"; path="$2"; body="${3:-}"\n    if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then printf "%s" "{\\"success\\":true,\\"result\\":[]}"; return 0; fi\n    if [ "$method" = POST ] && [ "$path" = /zones/zone1/dns_records ]; then\n      printf "%s" "$body" | jq -c ". + {id:\\"dns-crashed\\"}" > "$REMOTE"\n      kill -KILL "$BASHPID"\n    fi\n    return 2\n  }\n  cf_reconcile_dns zone1 mcp.example.com 203.0.113.10 ""\n'\nrc=$?\nset -e\n[ "$rc" -ne 0 ]\n'''
new = '''child="$TMP/crash-child.sh"
cat > "$child" <<'CHILD'
#!/usr/bin/env bash
set -Eeuo pipefail
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
AGENT_USER=root; CF_TOKEN=test
cf_new_ownership_marker(){ printf 'crashnonce0123456789abcdef01234567\\n'; }
cf_api(){
  local method="$1" path="$2" body="${3:-}"
  if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then printf '%s' '{"success":true,"result":[]}'; return 0; fi
  if [ "$method" = POST ] && [ "$path" = /zones/zone1/dns_records ]; then
    printf '%s' "$body" | jq -c '. + {id:"dns-crashed"}' > "$REMOTE"
    kill -KILL "$PPID"
    sleep 10
  fi
  return 2
}
cf_reconcile_dns zone1 mcp.example.com 203.0.113.10 ""
CHILD
chmod +x "$child"
set +e
ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c 'exec bash "$1"' _ "$child" >/tmp/cloudflare-crash-child.out 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || { echo 'expected simulated process crash' >&2; exit 1; }
'''
if text.count(old) != 1:
    raise SystemExit(f'expected crash harness block once, got {text.count(old)}')
path.write_text(text.replace(old, new, 1))
print('crash harness supervisor fixed')
