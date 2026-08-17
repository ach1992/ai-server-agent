from pathlib import Path

path = Path('tests/cloudflare_transaction.sh')
text = path.read_text()
marker = '# Ambiguous POST recovery: semantic DNS signature is removed even when no response ID was captured.\n'
insert = 'ORIG_CF_FIND_DNS_BY_MARKER="$(declare -f cf_find_dns_by_marker)"\nORIG_CF_FIND_RULE_BY_MARKER="$(declare -f cf_find_rule_by_marker)"\n'
if marker not in text:
    raise SystemExit('ambiguous recovery marker missing')
text = text.replace(marker, insert + marker, 1)
restore_marker = '# Durable pre-POST journal includes the unpredictable marker before any create request.\n'
restore = 'eval "$ORIG_CF_FIND_DNS_BY_MARKER"\neval "$ORIG_CF_FIND_RULE_BY_MARKER"\n\n'
if restore_marker not in text:
    raise SystemExit('durable journal test marker missing')
text = text.replace(restore_marker, restore + restore_marker, 1)
path.write_text(text)
print('durable Cloudflare test isolation fixed')
