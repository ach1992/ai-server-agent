from pathlib import Path

path = Path('scripts/fix-v012-crash-harness-v2.py')
text = path.read_text()
old = '''source "$ROOT/manage.sh"
AGENT_USER=root; CF_TOKEN=test
cf_new_ownership_marker(){ printf 'crashnonce0123456789abcdef01234567\\n'; }
'''
new = '''source "$ROOT/manage.sh"
STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"
AGENT_USER=root; CF_TOKEN=test
cf_new_ownership_marker(){ printf 'crashnonce0123456789abcdef01234567\\n'; }
'''
if text.count(old) != 1:
    raise SystemExit(f'expected child source block once, got {text.count(old)}')
text = text.replace(old, new, 1)
old2 = '''ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" timeout --signal=KILL 1s bash "$child" >/tmp/cloudflare-crash-child.out 2>&1
'''
new2 = '''ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" timeout --signal=KILL 1s bash "$child" >/tmp/cloudflare-crash-child.out 2>&1
'''
if text.count(old2) != 1:
    raise SystemExit(f'expected child launch block once, got {text.count(old2)}')
text = text.replace(old2, new2, 1)
old3 = '''  source "$ROOT/manage.sh"
  AGENT_USER=root; CF_TOKEN=test
'''
new3 = '''  source "$ROOT/manage.sh"
  STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"
  AGENT_USER=root; CF_TOKEN=test
'''
if text.count(old3) != 1:
    raise SystemExit(f'expected recovery source block once, got {text.count(old3)}')
text = text.replace(old3, new3, 1)
old4 = '''ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c '
'''
new4 = '''ROOT="$ROOT" TEST_STATE_DIR="$STATE_DIR" TEST_CONFIG_DIR="$CONFIG_DIR" TEST_TLS_DIR="$TLS_DIR" TEST_CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c '
'''
if text.count(old4) != 1:
    raise SystemExit(f'expected recovery launch block once, got {text.count(old4)}')
path.write_text(text.replace(old4, new4, 1))
print('crash harness paths rebound after manage.sh source')
