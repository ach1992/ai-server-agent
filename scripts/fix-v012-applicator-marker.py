from pathlib import Path

path = Path("scripts/apply-v012-independent-review-fixes.py")
text = path.read_text()
old = '''manage = replace_once(manage, 'CF_TOKEN=""\\n', 'CF_TOKEN=""\\nCF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"\\n')'''
new = '''manage = replace_once(manage, 'CF_API="https://api.cloudflare.com/client/v4"\\nCF_TOKEN=""\\n', 'CF_API="https://api.cloudflare.com/client/v4"\\nCF_TOKEN=""\\nCF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"\\n')'''
if text.count(old) != 1:
    raise SystemExit(f"expected one applicator marker, got {text.count(old)}")
path.write_text(text.replace(old, new, 1))
print("applicator marker fixed")
