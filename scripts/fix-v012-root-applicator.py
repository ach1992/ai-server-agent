from pathlib import Path

p = Path('scripts/apply-v012-root-trust-boundary.py')
text = p.read_text()
old = '''replace_once(\n    "tests/cloudflare_crash_recovery.sh",\n    'STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"\\n',\n    'STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"\\n',\n)\n'''
new = '''text = Path("tests/cloudflare_crash_recovery.sh").read_text()\nneedle = 'STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"\\n'\nreplacement = 'STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CF_TXN_STATE="$TEST_CF_TXN_STATE"\\n'\nif text.count(needle) != 2:\n    raise SystemExit(f"crash recovery rebind marker count={text.count(needle)}")\nPath("tests/cloudflare_crash_recovery.sh").write_text(text.replace(needle, replacement, 1))\n'''
if text.count(old) != 1:
    raise SystemExit(f'applicator source marker count={text.count(old)}')
p.write_text(text.replace(old, new, 1))
print('root trust applicator crash-test targeting fixed')
