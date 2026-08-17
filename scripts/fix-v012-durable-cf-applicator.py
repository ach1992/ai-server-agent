from pathlib import Path

path = Path('scripts/apply-v012-durable-cf-journal.py')
text = path.read_text()
old = """text = replace_once(\n    text,\n    'CF_PENDING_PHASE=\"\"\\n',\n    'CF_PENDING_PHASE=\"\"\\nCF_PENDING_MARKER=\"\"\\n',\n)"""
new = """text = replace_once(\n    text,\n    'CF_PENDING_HOST=\"\"\\nCF_PENDING_VALUE=\"\"\\nCF_PENDING_PHASE=\"\"\\n\\nlog(){',\n    'CF_PENDING_HOST=\"\"\\nCF_PENDING_VALUE=\"\"\\nCF_PENDING_PHASE=\"\"\\nCF_PENDING_MARKER=\"\"\\n\\nlog(){',\n)"""
if text.count(old) != 1:
    raise SystemExit(f'expected one ambiguous marker block, got {text.count(old)}')
path.write_text(text.replace(old, new, 1))
print('durable journal applicator marker fixed')
