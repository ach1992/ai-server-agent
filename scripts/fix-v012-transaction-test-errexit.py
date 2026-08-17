from pathlib import Path

path = Path("tests/cloudflare_transaction.sh")
text = path.read_text()
marker = '''confirm(){ return 1; }
DELETE_LOG="$TMP/deletes.log"'''
insert = '''confirm(){ return 1; }
expect_configure_failure(){
  local label="$1" rc
  set +e
  ( set -Eeuo pipefail; configure_cloudflare >/dev/null 2>&1 )
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || { echo "expected $label" >&2; exit 1; }
}
DELETE_LOG="$TMP/deletes.log"'''
if text.count(marker) != 1:
    raise SystemExit(f"expected one helper insertion marker, got {text.count(marker)}")
text = text.replace(marker, insert, 1)
replacements = {
    "if configure_cloudflare >/dev/null 2>&1; then echo 'expected certificate helper failure' >&2; exit 1; fi": "expect_configure_failure 'certificate helper failure'",
    "if configure_cloudflare >/dev/null 2>&1; then echo 'expected origin-stage failure' >&2; exit 1; fi": "expect_configure_failure 'origin-stage failure'",
    "if configure_cloudflare >/dev/null 2>&1; then echo 'expected rollback-delete failure' >&2; exit 1; fi": "expect_configure_failure 'rollback-delete failure'",
    "if configure_cloudflare >/dev/null 2>&1; then echo 'expected state-write failure' >&2; exit 1; fi": "expect_configure_failure 'state-write failure'",
}
for old, new in replacements.items():
    if text.count(old) != 1:
        raise SystemExit(f"expected one failure assertion marker, got {text.count(old)}: {old}")
    text = text.replace(old, new, 1)
path.write_text(text)
print("transaction failure harness fixed")
