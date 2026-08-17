from pathlib import Path

path = Path('scripts/fix-v012-crash-harness-v2.py')
text = path.read_text()
old = '''[ "$rc" -ne 0 ] || { echo 'expected SIGKILL crash injection' >&2; exit 1; }
test -s "$REMOTE" || { cat /tmp/cloudflare-crash-child.out >&2 || true; echo 'mock Cloudflare create did not reach remote-commit point' >&2; exit 1; }
test -s "$CF_TXN_STATE"
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create
marker="$(jq -r '.pending.marker' "$CF_TXN_STATE")"
test -n "$marker"
test "$(jq -r '.comment' "$REMOTE")" = "Managed by AI Server Agent txn:$marker"
'''
new = '''[ "$rc" -ne 0 ] || { echo 'expected SIGKILL crash injection' >&2; exit 1; }
echo 'crash injection returned nonzero as expected'
test -s "$REMOTE" || { cat /tmp/cloudflare-crash-child.out >&2 || true; echo 'mock Cloudflare create did not reach remote-commit point' >&2; exit 1; }
echo 'remote create committed before crash'
test -s "$CF_TXN_STATE" || { echo 'durable transaction journal missing after crash' >&2; exit 1; }
echo 'durable transaction journal survived crash'
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create || { echo 'pending kind missing after crash' >&2; cat "$CF_TXN_STATE" >&2; exit 1; }
marker="$(jq -r '.pending.marker' "$CF_TXN_STATE")"
test -n "$marker" || { echo 'pending ownership marker missing after crash' >&2; exit 1; }
test "$(jq -r '.comment' "$REMOTE")" = "Managed by AI Server Agent txn:$marker" || { echo 'remote marker does not match durable journal marker' >&2; exit 1; }
echo 'remote ownership marker matches durable journal'
'''
if text.count(old) != 1:
    raise SystemExit(f'expected crash assertion block once, got {text.count(old)}')
text = text.replace(old, new, 1)
old2 = '''  recover_cloudflare_transaction
  test ! -e "$CF_TXN_STATE"
  test ! -e "$REMOTE"
'\n\necho 'cloudflare crash-recovery test passed'
'''
new2 = '''  recover_cloudflare_transaction
  test ! -e "$CF_TXN_STATE"
  test ! -e "$REMOTE"
' || { echo 'fresh-process crash recovery failed' >&2; test -e "$CF_TXN_STATE" && cat "$CF_TXN_STATE" >&2 || true; exit 1; }
echo 'fresh process recovered durable transaction journal'
\necho 'cloudflare crash-recovery test passed'
'''
if text.count(old2) != 1:
    raise SystemExit(f'expected fresh recovery block once, got {text.count(old2)}')
path.write_text(text.replace(old2, new2, 1))
print('crash harness invariants instrumented')
