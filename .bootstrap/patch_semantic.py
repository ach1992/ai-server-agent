#!/usr/bin/env python3
from pathlib import Path
p=Path('manage.sh'); s=p.read_text()
os=s.index('cf_reconcile_origin_rule(){'); ss=s.index('cf_reconcile_ssl_config_rule(){'); sv=s.index('save_cloudflare_state(){')
pre,origin,ssl,post=s[:os],s[os:ss],s[ss:sv],s[sv:]
decl='  local phase_entry rc ruleset_id rule_ref ref_match id_count rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint'
if origin.count(decl)!=1 or ssl.count(decl)!=1: raise SystemExit('declaration drift')
origin=origin.replace(decl,decl+' semantic_ids semantic_count',1); ssl=ssl.replace(decl,decl+' semantic_ids semantic_count',1)
ref='''    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"'''
oref=r'''    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    semantic_ids="$(jq -r --arg host "$host" --argjson port "$port" '
      .result.rules[]?
      | ((.expression // "") | gsub("\\s+";"")) as $expr
      | select(.enabled == true and .action == "route" and (.action_parameters.origin.port // null) == $port and ($expr == ("http.hosteq\"" + $host + "\"") or $expr == ("(http.hosteq\"" + $host + "\")")))
      | .id // empty
    ' <<<"$ruleset")"
    semantic_count="$(grep -cve '^$' <<<"$semantic_ids" || true)"'''
sref=r'''    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    semantic_ids="$(jq -r --arg host "$host" '
      .result.rules[]?
      | ((.expression // "") | gsub("\\s+";"")) as $expr
      | select(.enabled == true and .action == "set_config" and (.action_parameters.ssl // "") == "strict" and ($expr == ("http.hosteq\"" + $host + "\"") or $expr == ("(http.hosteq\"" + $host + "\")")))
      | .id // empty
    ' <<<"$ruleset")"
    semantic_count="$(grep -cve '^$' <<<"$semantic_ids" || true)"'''
if origin.count(ref)!=1 or ssl.count(ref)!=1: raise SystemExit('ref discovery drift')
origin=origin.replace(ref,oref,1); ssl=ssl.replace(ref,sref,1)
o_old='''        jq -e --argjson expected "$rule_body" '.ref==$expected.ref and .expression==$expected.expression and .action==$expected.action and .action_parameters==$expected.action_parameters and .enabled==$expected.enabled' >/dev/null <<<"$rule" || die "The Agent-owned Origin Rule no longer matches the requested canonical state. Refusing an unconditional in-place update; clean up or reconcile it explicitly, then rerun setup."
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$owned_rule"; CF_RESULT_ORIGIN_FINGERPRINT="$current_fingerprint"'''
o_new=o_old.replace('        CF_RESULT_ORIGIN_RULESET_ID=', '        if [ "$semantic_count" -gt 1 ]; then\n          warn "Cloudflare diagnostic: found $semantic_count equivalent Origin Rules in ruleset $ruleset_id for $host:$port; continuing with recorded Agent-owned rule $owned_rule and preserving external equivalents."\n        fi\n        CF_RESULT_ORIGIN_RULESET_ID=')
s_old='''        jq -e --argjson expected "$rule_body" '.ref==$expected.ref and .expression==$expected.expression and .action==$expected.action and .action_parameters==$expected.action_parameters and .enabled==$expected.enabled' >/dev/null <<<"$rule" || die "The Agent-owned Configuration Rule no longer matches strict hostname-scoped canonical state. Refusing an unconditional in-place update; clean up or reconcile it explicitly, then rerun setup."
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$owned_rule"; CF_RESULT_SSL_FINGERPRINT="$current_fingerprint"'''
s_new=s_old.replace('        CF_RESULT_SSL_RULESET_ID=', '        if [ "$semantic_count" -gt 1 ]; then\n          warn "Cloudflare diagnostic: found $semantic_count equivalent strict SSL Configuration Rules in ruleset $ruleset_id for $host; continuing with recorded Agent-owned rule $owned_rule and preserving external equivalents."\n        fi\n        CF_RESULT_SSL_RULESET_ID=')
if origin.count(o_old)!=1 or ssl.count(s_old)!=1: raise SystemExit('owned branch drift')
origin=origin.replace(o_old,o_new,1); ssl=ssl.replace(s_old,s_new,1)
o_guard='''      [ -z "$ref_match" ] || die "An unowned Origin Rule already uses the Agent ref. Refusing to adopt or overwrite it."
      marker="$(cf_new_ownership_marker)"'''
s_guard='''      [ -z "$ref_match" ] || die "An unowned Configuration Rule already uses the Agent SSL ref. Refusing to adopt or overwrite it."
      marker="$(cf_new_ownership_marker)"'''
if origin.count(o_guard)!=1 or ssl.count(s_guard)!=1: raise SystemExit('unowned branch drift')
origin=origin.replace(o_guard,o_guard.replace('      marker=', '      [ "$semantic_count" -eq 0 ] || die "Cloudflare already has $semantic_count equivalent external Origin Rule(s) in ruleset $ruleset_id for $host:$port. Refusing to create a duplicate or adopt external rules automatically."\n      marker='),1)
ssl=ssl.replace(s_guard,s_guard.replace('      marker=', '      [ "$semantic_count" -eq 0 ] || die "Cloudflare already has $semantic_count equivalent external strict SSL Configuration Rule(s) in ruleset $ruleset_id for $host. Refusing to create a duplicate or adopt external rules automatically."\n      marker='),1)
p.write_text(pre+origin+ssl+post)
