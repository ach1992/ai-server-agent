from pathlib import Path

path = Path("scripts/apply-v012-cloudflare-write-unknown.py")
text = path.read_text()
old = "if grep -qF '/zones/zone1/rulesets/shared-set' \"$DELETE_LOG\" | grep -vq '/rules/'; then echo 'rollback deleted a whole ruleset' >&2; exit 1; fi"
new = "if grep -Fxq '/zones/zone1/rulesets/shared-set' \"$DELETE_LOG\"; then echo 'rollback deleted a whole ruleset' >&2; exit 1; fi"
if text.count(old) != 1:
    raise SystemExit(f"expected one whole-ruleset assertion marker, got {text.count(old)}")
text = text.replace(old, new, 1)
text = text.replace("echo 'cloudflare transaction tests passed' '''", "echo 'cloudflare transaction tests passed'\n'''")
path.write_text(text)
print('write-unknown applicator assertions fixed')
