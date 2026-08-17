from pathlib import Path


def replace_once(text: str, old: str, new: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"expected exactly one marker, got {count}: {old[:120]!r}")
    return text.replace(old, new, 1)


def replace_between(text: str, start: str, end: str, replacement: str) -> str:
    i = text.find(start)
    if i < 0:
        raise SystemExit(f"start marker not found: {start}")
    j = text.find(end, i + len(start))
    if j < 0:
        raise SystemExit(f"end marker not found: {end}")
    return text[:i] + replacement.rstrip() + "\n\n" + text[j:]


path = Path("manage.sh")
text = path.read_text()
text = replace_once(
    text,
    'CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"\n',
    'CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"\n'
    'CF_PENDING_KIND=""\nCF_PENDING_ZONE=""\nCF_PENDING_HOST=""\nCF_PENDING_VALUE=""\nCF_PENDING_PHASE=""\n'
)

pending_helpers = r'''cf_clear_pending_write(){
  CF_PENDING_KIND=""; CF_PENDING_ZONE=""; CF_PENDING_HOST=""; CF_PENDING_VALUE=""; CF_PENDING_PHASE=""
}

cf_set_pending_write(){
  CF_PENDING_KIND="$1"; CF_PENDING_ZONE="$2"; CF_PENDING_HOST="$3"; CF_PENDING_VALUE="$4"; CF_PENDING_PHASE="${5:-}"
}

cf_find_origin_cert_by_csr(){
  local zone_id="$1" csr="$2" page=1 total_pages=1 res ids count=0 found="" id
  while [ "$page" -le "$total_pages" ]; do
    res="$(cf_api GET "/certificates?zone_id=$zone_id&per_page=50&page=$page")" || return 2
    ids="$(jq -r --arg csr "$csr" '.result[]? | select((.csr // "") == $csr) | .id // empty' <<<"$res")"
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      found="$id"; count=$((count + 1))
    done <<<"$ids"
    total_pages="$(jq -r '.result_info.total_pages // 1' <<<"$res")"
    [[ "$total_pages" =~ ^[0-9]+$ ]] || total_pages=1
    page=$((page + 1))
  done
  [ "$count" -eq 1 ] || { [ "$count" -eq 0 ] && return 1; return 2; }
  printf '%s\n' "$found"
}

cf_find_dns_by_signature(){
  local zone_id="$1" host="$2" ip="$3" res ids count found
  res="$(cf_api GET "/zones/$zone_id/dns_records?name=$host&per_page=100")" || return 2
  ids="$(jq -r --arg ip "$ip" '.result[]? | select(.type=="A" and .content==$ip and .proxied==true and (.comment // "")=="Managed by AI Server Agent") | .id // empty' <<<"$res")"
  count="$(grep -cve '^$' <<<"$ids" || true)"
  [ "$count" -eq 1 ] || { [ "$count" -eq 0 ] && return 1; return 2; }
  found="$(grep -v '^$' <<<"$ids" | head -n1)"
  printf '%s\n' "$found"
}

cf_find_rule_by_ref(){
  local zone_id="$1" phase="$2" ref="$3" list ruleset_id ruleset ids id count=0 found=""
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || return 2
  while IFS= read -r ruleset_id; do
    [ -n "$ruleset_id" ] || continue
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || return 2
    ids="$(jq -r --arg ref "$ref" '.result.rules[]? | select((.ref // "") == $ref) | .id // empty' <<<"$ruleset")"
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      found="$ruleset_id|$id"; count=$((count + 1))
    done <<<"$ids"
  done < <(jq -r --arg phase "$phase" '.result[]? | select(.kind=="zone" and .phase==$phase) | .id // empty' <<<"$list")
  [ "$count" -eq 1 ] || { [ "$count" -eq 0 ] && return 1; return 2; }
  printf '%s\n' "$found"
}

cf_recover_pending_write(){
  local found rc ruleset_id rule_id
  [ -n "$CF_PENDING_KIND" ] || return 0
  case "$CF_PENDING_KIND" in
    origin-cert-create)
      if found="$(cf_find_origin_cert_by_csr "$CF_PENDING_ZONE" "$CF_PENDING_VALUE")"; then
        cf_delete_owned "/certificates/$found" || return 1
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    dns-create)
      if found="$(cf_find_dns_by_signature "$CF_PENDING_ZONE" "$CF_PENDING_HOST" "$CF_PENDING_VALUE")"; then
        cf_delete_owned "/zones/$CF_PENDING_ZONE/dns_records/$found" || return 1
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    origin-rule-create|ssl-rule-create)
      if found="$(cf_find_rule_by_ref "$CF_PENDING_ZONE" "$CF_PENDING_PHASE" "$CF_PENDING_VALUE")"; then
        IFS='|' read -r ruleset_id rule_id <<<"$found"
        cf_delete_owned "/zones/$CF_PENDING_ZONE/rulesets/$ruleset_id/rules/$rule_id" || return 1
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    *) return 1 ;;
  esac
  cf_clear_pending_write
}
'''
text = replace_once(text, 'delete_recorded_cloudflare_resources(){', pending_helpers + '\ndelete_recorded_cloudflare_resources(){')

new_reconcile = r'''cf_issue_origin_cert(){
  local zone_id="$1" host="$2" stage="$3" key="$stage/new.key" csr="$stage/new.csr" crt="$stage/new.crt" csr_value body res key_pub cert_pub
  CF_RESULT_CERT_ID=""
  umask 077
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$key" >/dev/null 2>&1
  openssl req -new -sha256 -key "$key" -out "$csr" -subj "/CN=$host" -addext "subjectAltName=DNS:$host"
  csr_value="$(cat "$csr")"
  body="$(jq -n --arg csr "$csr_value" --arg host "$host" '{hostnames:[$host],request_type:"origin-rsa",requested_validity:1095,csr:$csr}')"
  cf_set_pending_write origin-cert-create "$zone_id" "$host" "$csr_value"
  res="$(cf_api POST '/certificates' "$body")" || die "Cloudflare Origin CA certificate issuance response was not confirmed. Transaction recovery will reconcile the CSR before continuing."
  jq -r '.result.certificate // empty' <<<"$res" > "$crt"
  CF_RESULT_CERT_ID="$(jq -r '.result.id // empty' <<<"$res")"
  [ -s "$crt" ] && [ -n "$CF_RESULT_CERT_ID" ] || die "Cloudflare returned an incomplete Origin CA certificate response."
  openssl pkey -in "$key" -check -noout >/dev/null 2>&1 || die "Generated private key failed validation."
  openssl x509 -in "$crt" -noout -checkhost "$host" >/dev/null || die "Issued certificate does not match $host."
  key_pub="$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  cert_pub="$(openssl x509 -in "$crt" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  [ "$key_pub" = "$cert_pub" ] || die "Issued certificate does not match the generated private key."
  cf_clear_pending_write
}

cf_reconcile_dns(){
  local zone_id="$1" host="$2" ip="$3" owned_dns_id="${4:-}" res count id type content proxied ttl body owned=false
  CF_RESULT_DNS_ID=""; CF_RESULT_DNS_OWNED=false; CF_RESULT_DNS_ACTION=""; CF_RESULT_DNS_OLD_CONTENT=""; CF_RESULT_DNS_OLD_PROXIED=""; CF_RESULT_DNS_OLD_TTL=""
  res="$(cf_api GET "/zones/$zone_id/dns_records?name=$host&per_page=100")" || die "Cloudflare DNS lookup failed."
  count="$(jq '.result | length' <<<"$res")"
  if [ "$count" -eq 0 ]; then
    body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
    cf_set_pending_write dns-create "$zone_id" "$host" "$ip"
    res="$(cf_api POST "/zones/$zone_id/dns_records" "$body")" || die "Cloudflare DNS create response was not confirmed. Transaction recovery will reconcile the exact hostname/IP signature."
    id="$(jq -r '.result.id // empty' <<<"$res")"; [ -n "$id" ] || die "Cloudflare did not return the created DNS record ID."
    CF_RESULT_DNS_ID="$id"; CF_RESULT_DNS_OWNED=true; CF_RESULT_DNS_ACTION=created
    cf_clear_pending_write
    return
  fi
  [ "$count" -eq 1 ] || die "Multiple DNS records already exist for $host. Refusing ambiguous replacement."
  id="$(jq -r '.result[0].id' <<<"$res")"; type="$(jq -r '.result[0].type' <<<"$res")"; content="$(jq -r '.result[0].content' <<<"$res")"; proxied="$(jq -r '.result[0].proxied' <<<"$res")"; ttl="$(jq -r '.result[0].ttl' <<<"$res")"
  [ "$type" = "A" ] || die "$host already has a $type record. Use another hostname or resolve the DNS conflict manually."
  if [ -n "$owned_dns_id" ]; then
    [ "$id" = "$owned_dns_id" ] || die "The DNS record ID for $host no longer matches recorded Agent ownership. Refusing to adopt or modify the replacement record."
    owned=true
  fi
  CF_RESULT_DNS_ID="$id"; CF_RESULT_DNS_OWNED="$owned"
  if [ "$content" = "$ip" ] && [ "$proxied" = "true" ]; then
    if [ "$owned" = "true" ]; then CF_RESULT_DNS_ACTION=existing-managed; else CF_RESULT_DNS_ACTION=existing-external; fi
    return
  fi
  [ "$owned" = "true" ] || die "Existing A record for $host is not recorded as Agent-owned and does not match this server. Refusing to modify or adopt it automatically. Use another hostname or update/remove that record manually, then rerun setup."
  CF_RESULT_DNS_ACTION=updated; CF_RESULT_DNS_OLD_CONTENT="$content"; CF_RESULT_DNS_OLD_PROXIED="$proxied"; CF_RESULT_DNS_OLD_TTL="$ttl"
  body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
  cf_api PATCH "/zones/$zone_id/dns_records/$id" "$body" >/dev/null || die "Could not update Cloudflare DNS record for $host."
}

cf_reconcile_origin_rule(){
  local zone_id="$1" host="$2" port="$3" owned_ruleset="${4:-}" owned_rule="${5:-}"
  local list ruleset_id rule_ref rule_id ref_match rule_body create_body res ruleset
  CF_RESULT_ORIGIN_RULESET_ID=""; CF_RESULT_ORIGIN_RULE_ID=""; CF_RESULT_ORIGIN_ACTION=""
  rule_ref="ai_server_agent_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" '{ref:$ref,description:"AI Server Agent origin port",expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_request_origin") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    create_body="$(jq -n --argjson rule "$rule_body" '{name:"AI Server Agent Origin Rules",description:"Hostname-scoped origin routing managed by AI Server Agent",kind:"zone",phase:"http_request_origin",rules:[$rule]}')"
    cf_set_pending_write origin-rule-create "$zone_id" "$host" "$rule_ref" http_request_origin
    res="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Cloudflare Origin Rules create response was not confirmed. Transaction recovery will reconcile the deterministic rule ref."
    CF_RESULT_ORIGIN_RULESET_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.rules[0].id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=ruleset-created
    [ -n "$CF_RESULT_ORIGIN_RULESET_ID" ] || die "Cloudflare did not return the created Origin Rules ruleset ID."
    ruleset_id="$CF_RESULT_ORIGIN_RULESET_ID"
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Origin Rules ruleset."
    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then
      rule_id="$(jq -r --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref) | .id' <<<"$ruleset" | head -n1)"
      if [ -n "$rule_id" ]; then
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$rule_id"; CF_RESULT_ORIGIN_ACTION=updated
        cf_api PATCH "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id" "$rule_body" >/dev/null || die "Could not update Agent-owned Origin Rule."
      elif [ -n "$ref_match" ]; then
        die "An Origin Rule uses the Agent ref but is not the rule recorded as Agent-owned. Refusing to adopt or overwrite it."
      else
        cf_set_pending_write origin-rule-create "$zone_id" "$host" "$rule_ref" http_request_origin
        res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body")" || die "Cloudflare Origin Rule recreate response was not confirmed. Transaction recovery will reconcile the deterministic rule ref."
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=recreated
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Origin Rule already uses the Agent ref. Refusing to adopt or overwrite it."
      cf_set_pending_write origin-rule-create "$zone_id" "$host" "$rule_ref" http_request_origin
      res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body")" || die "Cloudflare Origin Rule create response was not confirmed. Transaction recovery will reconcile the deterministic rule ref."
      CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=created
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare Origin Rule."
  rule_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
  [ -n "$rule_id" ] || die "Cloudflare Origin Rule was not reconciled cleanly."
  CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$rule_id"
  cf_clear_pending_write
}

cf_reconcile_ssl_config_rule(){
  local zone_id="$1" host="$2" owned_ruleset="${3:-}" owned_rule="${4:-}"
  local list ruleset_id rule_ref rule_id ref_match rule_body create_body res ruleset
  CF_RESULT_SSL_RULESET_ID=""; CF_RESULT_SSL_RULE_ID=""; CF_RESULT_SSL_ACTION=""
  rule_ref="ai_server_agent_ssl_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" '{ref:$ref,description:"AI Server Agent strict SSL",expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_config_settings") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    create_body="$(jq -n --argjson rule "$rule_body" '{name:"AI Server Agent Configuration Rules",description:"Hostname-scoped configuration managed by AI Server Agent",kind:"zone",phase:"http_config_settings",rules:[$rule]}')"
    cf_set_pending_write ssl-rule-create "$zone_id" "$host" "$rule_ref" http_config_settings
    res="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Cloudflare Configuration Rules create response was not confirmed. Transaction recovery will reconcile the deterministic rule ref."
    CF_RESULT_SSL_RULESET_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.rules[0].id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=ruleset-created
    [ -n "$CF_RESULT_SSL_RULESET_ID" ] || die "Cloudflare did not return the created Configuration Rules ruleset ID."
    ruleset_id="$CF_RESULT_SSL_RULESET_ID"
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Configuration Rules ruleset."
    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then
      rule_id="$(jq -r --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref) | .id' <<<"$ruleset" | head -n1)"
      if [ -n "$rule_id" ]; then
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$rule_id"; CF_RESULT_SSL_ACTION=updated
        cf_api PATCH "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id" "$rule_body" >/dev/null || die "Could not update Agent-owned strict SSL Configuration Rule."
      elif [ -n "$ref_match" ]; then
        die "A Configuration Rule uses the Agent SSL ref but is not the rule recorded as Agent-owned. Refusing to adopt or overwrite it."
      else
        cf_set_pending_write ssl-rule-create "$zone_id" "$host" "$rule_ref" http_config_settings
        res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body")" || die "Cloudflare Configuration Rule recreate response was not confirmed. Transaction recovery will reconcile the deterministic rule ref."
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=recreated
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Configuration Rule already uses the Agent SSL ref. Refusing to adopt or overwrite it."
      cf_set_pending_write ssl-rule-create "$zone_id" "$host" "$rule_ref" http_config_settings
      res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body")" || die "Cloudflare Configuration Rule create response was not confirmed. Transaction recovery will reconcile the deterministic rule ref."
      CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=created
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare strict SSL Configuration Rule."
  rule_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict") | .id' <<<"$ruleset" | head -n1)"
  [ -n "$rule_id" ] || die "Cloudflare strict SSL Configuration Rule was not reconciled cleanly."
  CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$rule_id"
  cf_clear_pending_write
}'''
text = replace_between(text, 'cf_issue_origin_cert(){', 'save_cloudflare_state(){', new_reconcile)

new_txn = r'''save_cloudflare_transaction_state(){
  local host="$1" zone_id="$2" dns_id="$3" dns_action="$4" dns_old_content="$5" dns_old_proxied="$6" dns_old_ttl="$7" origin_ruleset="$8" origin_rule="$9" origin_action="${10}" old_port="${11}" ssl_ruleset="${12}" ssl_rule="${13}" ssl_action="${14}" cert_id="${15}" tmp
  tmp="$(mktemp)"
  jq -n --arg host "$host" --arg zone_id "$zone_id" --arg dns_id "$dns_id" --arg dns_action "$dns_action" --arg dns_old_content "$dns_old_content" --arg dns_old_proxied "$dns_old_proxied" --arg dns_old_ttl "$dns_old_ttl" --arg origin_ruleset "$origin_ruleset" --arg origin_rule "$origin_rule" --arg origin_action "$origin_action" --arg old_port "$old_port" --arg ssl_ruleset "$ssl_ruleset" --arg ssl_rule "$ssl_rule" --arg ssl_action "$ssl_action" --arg cert_id "$cert_id" --arg pending_kind "$CF_PENDING_KIND" --arg pending_zone "$CF_PENDING_ZONE" --arg pending_host "$CF_PENDING_HOST" --arg pending_value "$CF_PENDING_VALUE" --arg pending_phase "$CF_PENDING_PHASE" \
    '{hostname:$host,zone_id:$zone_id,dns:{id:$dns_id,action:$dns_action,old_content:$dns_old_content,old_proxied:$dns_old_proxied,old_ttl:$dns_old_ttl},origin:{ruleset_id:$origin_ruleset,rule_id:$origin_rule,action:$origin_action,old_port:$old_port},ssl:{ruleset_id:$ssl_ruleset,rule_id:$ssl_rule,action:$ssl_action},certificate_id:$cert_id,pending:{kind:$pending_kind,zone_id:$pending_zone,hostname:$pending_host,value:$pending_value,phase:$pending_phase}}' > "$tmp" || { rm -f "$tmp"; return 1; }
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$CF_TXN_STATE" || { rm -f "$tmp"; return 1; }
  rm -f "$tmp"
}

clear_cloudflare_transaction_state(){ rm -f "$CF_TXN_STATE"; }'''
text = replace_between(text, 'save_cloudflare_transaction_state(){', 'save_local_state(){', new_txn)

new_rollback = r'''rollback_new_cf_resources(){
  local host="$1" zone_id="$2" dns_id="$3" dns_action="$4" dns_old_content="$5" dns_old_proxied="$6" dns_old_ttl="$7" origin_ruleset="$8" origin_rule="$9" origin_action="${10}" old_port="${11}" ssl_ruleset="${12}" ssl_rule="${13}" ssl_action="${14}" cert_id="${15}"
  local keep_dns_id="" keep_dns_action="" keep_dns_old_content="" keep_dns_old_proxied="" keep_dns_old_ttl=""
  local keep_origin_ruleset="" keep_origin_rule="" keep_origin_action="" keep_ssl_ruleset="" keep_ssl_rule="" keep_ssl_action="" keep_cert_id="" failed=0
  if ! cf_recover_pending_write; then failed=1; fi
  case "$dns_action" in
    created) if [ -n "$dns_id" ] && ! cf_delete_owned "/zones/$zone_id/dns_records/$dns_id"; then keep_dns_id="$dns_id"; keep_dns_action=created; failed=1; fi ;;
    updated) if ! restore_updated_dns_record "$zone_id" "$host" "$dns_id" "$dns_old_content" "$dns_old_proxied" "$dns_old_ttl"; then keep_dns_id="$dns_id"; keep_dns_action=updated; keep_dns_old_content="$dns_old_content"; keep_dns_old_proxied="$dns_old_proxied"; keep_dns_old_ttl="$dns_old_ttl"; failed=1; fi ;;
  esac
  case "$origin_action" in
    ruleset-created|created|recreated) if [ -n "$origin_rule" ] && [ -n "$origin_ruleset" ] && ! cf_delete_owned "/zones/$zone_id/rulesets/$origin_ruleset/rules/$origin_rule"; then keep_origin_ruleset="$origin_ruleset"; keep_origin_rule="$origin_rule"; keep_origin_action="$origin_action"; failed=1; fi ;;
    updated) if ! restore_updated_origin_rule "$zone_id" "$host" "$old_port" "$origin_ruleset" "$origin_rule"; then keep_origin_ruleset="$origin_ruleset"; keep_origin_rule="$origin_rule"; keep_origin_action=updated; failed=1; fi ;;
  esac
  case "$ssl_action" in
    ruleset-created|created|recreated) if [ -n "$ssl_rule" ] && [ -n "$ssl_ruleset" ] && ! cf_delete_owned "/zones/$zone_id/rulesets/$ssl_ruleset/rules/$ssl_rule"; then keep_ssl_ruleset="$ssl_ruleset"; keep_ssl_rule="$ssl_rule"; keep_ssl_action="$ssl_action"; failed=1; fi ;;
  esac
  if [ -n "$cert_id" ] && ! cf_delete_owned "/certificates/$cert_id"; then keep_cert_id="$cert_id"; failed=1; fi
  if [ "$failed" -eq 0 ]; then clear_cloudflare_transaction_state; return 0; fi
  if save_cloudflare_transaction_state "$host" "$zone_id" "$keep_dns_id" "$keep_dns_action" "$keep_dns_old_content" "$keep_dns_old_proxied" "$keep_dns_old_ttl" "$keep_origin_ruleset" "$keep_origin_rule" "$keep_origin_action" "$old_port" "$keep_ssl_ruleset" "$keep_ssl_rule" "$keep_ssl_action" "$keep_cert_id"; then
    warn "Cloudflare rollback was incomplete. Exact remaining ownership/recovery state was preserved in $CF_TXN_STATE."
  else
    warn "CRITICAL: Cloudflare rollback was incomplete and the recovery journal could not be written to $CF_TXN_STATE. Do not continue Cloudflare setup until the host filesystem issue is repaired."
  fi
  return 1
}

recover_cloudflare_transaction(){
  [ -s "$CF_TXN_STATE" ] || return 0
  local host zone_id dns_id dns_action dns_old_content dns_old_proxied dns_old_ttl origin_ruleset origin_rule origin_action old_port ssl_ruleset ssl_rule ssl_action cert_id
  host="$(json_get "$CF_TXN_STATE" '.hostname')"; zone_id="$(json_get "$CF_TXN_STATE" '.zone_id')"
  dns_id="$(json_get "$CF_TXN_STATE" '.dns.id')"; dns_action="$(json_get "$CF_TXN_STATE" '.dns.action')"; dns_old_content="$(json_get "$CF_TXN_STATE" '.dns.old_content')"; dns_old_proxied="$(json_get "$CF_TXN_STATE" '.dns.old_proxied')"; dns_old_ttl="$(json_get "$CF_TXN_STATE" '.dns.old_ttl')"
  origin_ruleset="$(json_get "$CF_TXN_STATE" '.origin.ruleset_id')"; origin_rule="$(json_get "$CF_TXN_STATE" '.origin.rule_id')"; origin_action="$(json_get "$CF_TXN_STATE" '.origin.action')"; old_port="$(json_get "$CF_TXN_STATE" '.origin.old_port')"
  ssl_ruleset="$(json_get "$CF_TXN_STATE" '.ssl.ruleset_id')"; ssl_rule="$(json_get "$CF_TXN_STATE" '.ssl.rule_id')"; ssl_action="$(json_get "$CF_TXN_STATE" '.ssl.action')"; cert_id="$(json_get "$CF_TXN_STATE" '.certificate_id')"
  CF_PENDING_KIND="$(json_get "$CF_TXN_STATE" '.pending.kind')"; CF_PENDING_ZONE="$(json_get "$CF_TXN_STATE" '.pending.zone_id')"; CF_PENDING_HOST="$(json_get "$CF_TXN_STATE" '.pending.hostname')"; CF_PENDING_VALUE="$(json_get "$CF_TXN_STATE" '.pending.value')"; CF_PENDING_PHASE="$(json_get "$CF_TXN_STATE" '.pending.phase')"
  rollback_new_cf_resources "$host" "$zone_id" "$dns_id" "$dns_action" "$dns_old_content" "$dns_old_proxied" "$dns_old_ttl" "$origin_ruleset" "$origin_rule" "$origin_action" "$old_port" "$ssl_ruleset" "$ssl_rule" "$ssl_action" "$cert_id"
}'''
text = replace_between(text, 'rollback_new_cf_resources(){', 'cleanup_old_cloudflare(){', new_rollback)

text = replace_once(text, '  cf_issue_origin_cert "$host" "$stage"; cert_id="$CF_RESULT_CERT_ID"; log "Fresh Origin CA certificate issued and verified."', '  cf_issue_origin_cert "$zone_id" "$host" "$stage"; cert_id="$CF_RESULT_CERT_ID"; log "Fresh Origin CA certificate issued and verified."')
path.write_text(text)

test_path = Path("tests/cloudflare_transaction.sh")
test = test_path.read_text()
test = replace_once(test, 'mock_cert(){ local stage="$2";', 'mock_cert(){ local stage="$3";')
test = replace_once(test, '  local stage="$2"\n  printf \'key\\n\'', '  local stage="$3"\n  printf \'key\\n\'')
append_marker = "echo 'cloudflare transaction tests passed'"
extra = r'''# Ambiguous POST recovery: semantic DNS signature is removed even when no response ID was captured.
: > "$DELETE_LOG"
CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""
cf_find_dns_by_signature(){ printf 'dns-ambiguous\n'; }
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''
grep -qF '/zones/zone1/dns_records/dns-ambiguous' "$DELETE_LOG"
test -z "$CF_PENDING_KIND"
test ! -e "$CF_TXN_STATE"

# If semantic recovery itself cannot read Cloudflare, pending intent is preserved for a later cleanup retry.
CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""
cf_find_dns_by_signature(){ return 2; }
if rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''; then echo 'expected pending recovery journal' >&2; exit 1; fi
test -s "$CF_TXN_STATE"
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create
test "$(jq -r '.pending.hostname' "$CF_TXN_STATE")" = mcp.example.com
cf_find_dns_by_signature(){ printf 'dns-later\n'; }
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
recover_cloudflare_transaction
test ! -e "$CF_TXN_STATE"
test -z "$CF_PENDING_KIND"

# Deterministic rule refs also recover ambiguous rule creates without deleting an entire shared ruleset.
: > "$DELETE_LOG"
CF_PENDING_KIND=origin-rule-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=ai_server_agent_test; CF_PENDING_PHASE=http_request_origin
cf_find_rule_by_ref(){ printf 'shared-set|ambiguous-rule\n'; }
rollback_new_cf_resources mcp.example.com zone1 '' '' '' '' '' '' '' '' 3210 '' '' '' ''
grep -qF '/zones/zone1/rulesets/shared-set/rules/ambiguous-rule' "$DELETE_LOG"
if grep -qF '/zones/zone1/rulesets/shared-set' "$DELETE_LOG" | grep -vq '/rules/'; then echo 'rollback deleted a whole ruleset' >&2; exit 1; fi

echo 'cloudflare transaction tests passed' '''
if test.count(append_marker) != 1:
    raise SystemExit('test append marker mismatch')
test = test.replace(append_marker, extra, 1)
test_path.write_text(test)
print('Cloudflare ambiguous-write hardening applied')
