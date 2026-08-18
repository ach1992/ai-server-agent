from pathlib import Path

manage = Path('manage.sh')
text = manage.read_text()
replacements = {
'''  if res="$(cf_get_optional "/zones/$zone_id/dns_records/$dns_id")"; then
    jq -ce '.result | select(type=="object")' <<<"$res" || return 2
    return 0
  fi
  rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
''':
'''  if res="$(cf_get_optional "/zones/$zone_id/dns_records/$dns_id")"; then
    jq -ce '.result | select(type=="object")' <<<"$res" || return 2
    return 0
  else
    rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
  fi
''',
'''  if ! res="$(cf_get_optional "/zones/$zone_id/rulesets/$ruleset_id")"; then
    rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
  fi
''':
'''  if res="$(cf_get_optional "/zones/$zone_id/rulesets/$ruleset_id")"; then
    :
  else
    rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
  fi
''',
'''  if res="$(cf_get_optional "/certificates/$cert_id")"; then
    jq -ce '.result | select(type=="object")' <<<"$res" || return 2
    return 0
  fi
  rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
''':
'''  if res="$(cf_get_optional "/certificates/$cert_id")"; then
    jq -ce '.result | select(type=="object")' <<<"$res" || return 2
    return 0
  else
    rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
  fi
''',
}
for old, new in replacements.items():
    count = text.count(old)
    if count != 1:
        raise SystemExit(f'unexpected optional-read helper shape: {count}')
    text = text.replace(old, new)
manage.write_text(text)

test = Path('tests/cloudflare_rulesets_discovery.sh')
t = test.read_text()
anchor = "printf 'Cloudflare Rulesets phase-entrypoint discovery contract passed.\\n'\n"
addition = r'''# Optional-read wrappers must preserve the first-class 404/absence status.
cf_get_optional(){ return 3; }
for helper in dns rule cert; do
  set +e
  case "$helper" in
    dns) cf_get_dns_record zone1 missing >/dev/null 2>&1 ;;
    rule) cf_get_rule zone1 ruleset1 missing >/dev/null 2>&1 ;;
    cert) cf_get_origin_cert missing >/dev/null 2>&1 ;;
  esac
  rc=$?
  set -e
  [ "$rc" -eq 3 ] || fail "$helper optional-read wrapper lost 404 absence status: $rc"
done

printf 'Cloudflare Rulesets phase-entrypoint discovery contract passed.\n'
'''
if t.count(anchor) != 1:
    raise SystemExit(f'unexpected discovery test footer: {t.count(anchor)}')
test.write_text(t.replace(anchor, addition))
