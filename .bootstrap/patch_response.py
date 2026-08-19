#!/usr/bin/env python3
from pathlib import Path
import hashlib

p=Path('manage.sh')
data=p.read_bytes()
sha=hashlib.sha1(f'blob {len(data)}\0'.encode()+data).hexdigest()
if sha != '5d61c7ba29221362b9f3c366e34dc711716eaa20':
    raise SystemExit(f'unexpected manage.sh base blob: {sha}')
s=data.decode()
repls=[
('''        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=created
        rule="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid recreated Origin Rule."
        CF_RESULT_ORIGIN_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"''',
'''        [ "$(jq -r '.result.id // empty' <<<"$res")" = "$ruleset_id" ] || die "Cloudflare returned an unexpected Origin Ruleset ID after rule creation."
        rule="$(jq -ce --arg ref "$rule_ref" --arg marker "$marker" '[.result.rules[]? | select(.ref==$ref and ((.description // "") | endswith(" txn:"+$marker)))] | select(length==1) | .[0]' <<<"$res")" || die "Cloudflare returned an invalid or ambiguous recreated Origin Rule."
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.id // empty' <<<"$rule")"; CF_RESULT_ORIGIN_ACTION=created
        [ -n "$CF_RESULT_ORIGIN_RULE_ID" ] || die "Cloudflare did not return the recreated Origin Rule ID."
        CF_RESULT_ORIGIN_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
        log "Cloudflare diagnostic: Origin rule create response matched rule $CF_RESULT_ORIGIN_RULE_ID in ruleset $ruleset_id."'''),
('''      CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=created
      rule="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid created Origin Rule."
      CF_RESULT_ORIGIN_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"''',
'''      [ "$(jq -r '.result.id // empty' <<<"$res")" = "$ruleset_id" ] || die "Cloudflare returned an unexpected Origin Ruleset ID after rule creation."
      rule="$(jq -ce --arg ref "$rule_ref" --arg marker "$marker" '[.result.rules[]? | select(.ref==$ref and ((.description // "") | endswith(" txn:"+$marker)))] | select(length==1) | .[0]' <<<"$res")" || die "Cloudflare returned an invalid or ambiguous created Origin Rule."
      CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.id // empty' <<<"$rule")"; CF_RESULT_ORIGIN_ACTION=created
      [ -n "$CF_RESULT_ORIGIN_RULE_ID" ] || die "Cloudflare did not return the created Origin Rule ID."
      CF_RESULT_ORIGIN_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
      log "Cloudflare diagnostic: Origin rule create response matched rule $CF_RESULT_ORIGIN_RULE_ID in ruleset $ruleset_id."'''),
('''        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=created
        rule="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid recreated Configuration Rule."
        CF_RESULT_SSL_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"''',
'''        [ "$(jq -r '.result.id // empty' <<<"$res")" = "$ruleset_id" ] || die "Cloudflare returned an unexpected Configuration Ruleset ID after rule creation."
        rule="$(jq -ce --arg ref "$rule_ref" --arg marker "$marker" '[.result.rules[]? | select(.ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict" and ((.description // "") | endswith(" txn:"+$marker)))] | select(length==1) | .[0]' <<<"$res")" || die "Cloudflare returned an invalid or ambiguous recreated Configuration Rule."
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.id // empty' <<<"$rule")"; CF_RESULT_SSL_ACTION=created
        [ -n "$CF_RESULT_SSL_RULE_ID" ] || die "Cloudflare did not return the recreated Configuration Rule ID."
        CF_RESULT_SSL_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
        log "Cloudflare diagnostic: Configuration rule create response matched rule $CF_RESULT_SSL_RULE_ID in ruleset $ruleset_id."'''),
('''      CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=created
      rule="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid created Configuration Rule."
      CF_RESULT_SSL_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"''',
'''      [ "$(jq -r '.result.id // empty' <<<"$res")" = "$ruleset_id" ] || die "Cloudflare returned an unexpected Configuration Ruleset ID after rule creation."
      rule="$(jq -ce --arg ref "$rule_ref" --arg marker "$marker" '[.result.rules[]? | select(.ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict" and ((.description // "") | endswith(" txn:"+$marker)))] | select(length==1) | .[0]' <<<"$res")" || die "Cloudflare returned an invalid or ambiguous created Configuration Rule."
      CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.id // empty' <<<"$rule")"; CF_RESULT_SSL_ACTION=created
      [ -n "$CF_RESULT_SSL_RULE_ID" ] || die "Cloudflare did not return the created Configuration Rule ID."
      CF_RESULT_SSL_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
      log "Cloudflare diagnostic: Configuration rule create response matched rule $CF_RESULT_SSL_RULE_ID in ruleset $ruleset_id."''')]
for old,new in repls:
    if s.count(old)!=1: raise SystemExit('response pattern drift')
    s=s.replace(old,new,1)
p.write_text(s)
