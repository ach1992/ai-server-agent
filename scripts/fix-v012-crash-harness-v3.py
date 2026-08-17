from pathlib import Path

path = Path('scripts/fix-v012-crash-harness-v2.py')
text = path.read_text()
old = '''ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" setsid bash "$child" >/tmp/cloudflare-crash-child.out 2>&1 &
child_pid=$!
for _ in $(seq 1 50); do [ -s "$REMOTE" ] && break; sleep 0.1; done
test -s "$REMOTE" || { cat /tmp/cloudflare-crash-child.out >&2 || true; echo 'mock Cloudflare create did not reach remote-commit point' >&2; exit 1; }
kill -KILL -- "-$child_pid"
set +e
wait "$child_pid" >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ]
'''
new = '''set +e
ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" timeout --signal=KILL 1s bash "$child" >/tmp/cloudflare-crash-child.out 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || { echo 'expected SIGKILL crash injection' >&2; exit 1; }
test -s "$REMOTE" || { cat /tmp/cloudflare-crash-child.out >&2 || true; echo 'mock Cloudflare create did not reach remote-commit point' >&2; exit 1; }
'''
if text.count(old) != 1:
    raise SystemExit(f'expected process-group block once, got {text.count(old)}')
path.write_text(text.replace(old, new, 1))
print('crash harness now uses timeout SIGKILL injection')
