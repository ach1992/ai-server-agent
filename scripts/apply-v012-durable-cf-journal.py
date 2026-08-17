from pathlib import Path


def replace_once(text: str, old: str, new: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"expected exactly one marker, got {count}: {old[:100]!r}")
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
    'CF_PENDING_PHASE=""\n',
    'CF_PENDING_PHASE=""\nCF_PENDING_MARKER=""\n',
)

helpers = r'''cf_clear_pending_write(){
  CF_PENDING_KIND=""; CF_PENDING_ZONE=""; CF_PENDING_HOST=""; CF_PENDING_VALUE=""; CF_PENDING_PHASE=""; CF_PENDING_MARKER=""
}

cf_new_ownership_marker(){ openssl rand -hex 16; }

save_current_cloudflare_transaction_state(){
  save_cloudflare_transaction_state \
    "${host:-${CF_PENDING_HOST:-}}" "${zone_id:-${CF_PENDING_ZONE:-}}" \
    "${dns_id:-${CF_RESULT_DNS_ID:-}}" "${dns_action:-${CF_RESULT_DNS_ACTION:-}}" \
    "${dns_old_content:-${CF_RESULT_DNS_OLD_CONTENT:-}}" "${dns_old_proxied:-${CF_RESULT_DNS_OLD_PROXIED:-}}" "${dns_old_ttl:-${CF_RESULT_DNS_OLD_TTL:-}}" \
    "${origin_ruleset_id:-${CF_RESULT_ORIGIN_RULESET_ID:-}}" "${origin_rule_id:-${CF_RESULT_ORIGIN_RULE_ID:-}}" "${origin_action:-${CF_RESULT_ORIGIN_ACTION:-}}" "${old_port:-}" \
    "${ssl_ruleset_id:-${CF_RESULT_SSL_RULESET_ID:-}}" "${ssl_rule_id:-${CF_RESULT_SSL_RULE_ID:-}}" "${ssl_action:-${CF_RESULT_SSL_ACTION:-}}" \
    "${cert_id:-${CF_RESULT_CERT_ID:-}}"
}

cf_checkpoint_transaction(){
  save_current_cloudflare_transaction_state || die "Could not durably checkpoint the Cloudflare transaction before continuing."
}

cf_set_pending_write(){
  CF_PENDING_KIND="$1"; CF_PENDING_ZONE="$2"; CF_PENDING_HOST="$3"; CF_PENDING_VALUE="$4"; CF_PENDING_PHASE="${5:-}"; CF_PENDING_MARKER="${6:-}"
  save_current_cloudflare_transaction_state || die "Could not durably journal the Cloudflare create intent before remote mutation. No create request was sent."
}

cf_commit_pending_write(){
  local old_kind="$CF_PENDING_KIND" old_zone="$CF_PENDING_ZONE" old_host="$CF_PENDING_HOST" old_value="$CF_PENDING_VALUE" old_phase="$CF_PENDING_PHASE" old_marker="$CF_PENDING_MARKER"
  cf_clear_pending_write
  if save_current_cloudflare_transaction_state; then return 0; fi
  CF_PENDING_KIND="$old_kind"; CF_PENDING_ZONE="$old_zone"; CF_PENDING_HOST="$old_host"; CF_PENDING_VALUE="$old_value"; CF_PENDING_PHASE="$old_phase"; CF_PENDING_MARKER="$old_marker"
  die "Cloudflare create succeeded but the confirmed result could not be durably checkpointed. Recovery intent remains active."
}

cf_finish_transaction_step(){
  if [ -n "$CF_PENDING_KIND" ]; then cf_commit_pending_write; else cf_checkpoint_transaction; fi
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

cf_find_dns_by_marker(){
  local zone_id="$1" host="$2" ip="$3" marker="$4" res ids exact_count total found
  res="$(cf_api GET "/zones/$zone_id/dns_records?name=$host&per_page=100")" || return 2
  ids="$(jq -r --arg ip "$ip" --arg marker "$marker" '.result[]? | select(.type=="A" and .content==$ip and .proxied==true and (.comment // "")==("Managed by AI Server Agent txn:"+$marker)) | .id // empty' <<<"$res")"
  exact_count="$(grep -cve '^$' <<<"$ids" || true)"
  if [ "$exact_count" -eq 1 ]; then grep -v '^$' <<<"$ids" | head -n1; return 0; fi
  [ "$exact_count" -eq 0 ] || return 2
  total="$(jq '.result | length' <<<"$res")"
  [ "$total" -eq 0 ] && return 1
  return 2
}

cf_find_rule_by_marker(){
  local zone_id="$1" phase="$2" ref="$3" marker="$4" list ruleset_id ruleset exact_ids ref_ids id exact_count=0 ref_count=0 found=""
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || return 2
  while IFS= read -r ruleset_id; do
    [ -n "$ruleset_id" ] || continue
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || return 2
    exact_ids="$(jq -r --arg ref "$ref" --arg marker "$marker" '.result.rules[]? | select((.ref // "")==$ref and ((.description // "") | endswith(" txn:"+$marker))) | .id // empty' <<<"$ruleset")"
    ref_ids="$(jq -r --arg ref "$ref" '.result.rules[]? | select((.ref // "")==$ref) | .id // empty' <<<"$ruleset")"
    while IFS= read -r id; do [ -n "$id" ] || continue; found="$ruleset_id|$id"; exact_count=$((exact_count + 1)); done <<<"$exact_ids"
    while IFS= read -r id; do [ -n "$id" ] || continue; ref_count=$((ref_count + 1)); done <<<"$ref_ids"
  done < <(jq -r --arg phase "$phase" '.result[]? | select(.kind=="zone" and .phase==$phase) | .id // empty' <<<"$list")
  [ "$exact_count" -eq 1 ] && { printf '%s\n' "$found"; return 0; }
  [ "$exact_count" -eq 0 ] || return 2
  [ "$ref_count" -eq 0 ] && return 1
  return 2
}

cf_find_ruleset_by_marker(){
  local zone_id="$1" phase="$2" marker="$3" list ruleset_id ruleset phase_count=0 exact_count=0 found="" desc
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || return 2
  while IFS= read -r ruleset_id; do
    [ -n "$ruleset_id" ] || continue
    phase_count=$((phase_count + 1))
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || return 2
    desc="$(jq -r '.result.description // ""' <<<"$ruleset")"
    if [[ "$desc" == *" txn:$marker" ]]; then found="$ruleset_id"; exact_count=$((exact_count + 1)); fi
  done < <(jq -r --arg phase "$phase" '.result[]? | select(.kind=="zone" and .phase==$phase) | .id // empty' <<<"$list")
  [ "$exact_count" -eq 1 ] && { printf '%s\n' "$found"; return 0; }
  [ "$exact_count" -eq 0 ] || return 2
  [ "$phase_count" -eq 0 ] && return 1
  return 2
}

cf_recover_pending_write(){
  local found rc ruleset_id rule_id ruleset rule_count marked_count
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
      if found="$(cf_find_dns_by_marker "$CF_PENDING_ZONE" "$CF_PENDING_HOST" "$CF_PENDING_VALUE" "$CF_PENDING_MARKER")"; then
        cf_delete_owned "/zones/$CF_PENDING_ZONE/dns_records/$found" || return 1
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    origin-rule-create|ssl-rule-create)
      if found="$(cf_find_rule_by_marker "$CF_PENDING_ZONE" "$CF_PENDING_PHASE" "$CF_PENDING_VALUE" "$CF_PENDING_MARKER")"; then
        IFS='|' read -r ruleset_id rule_id <<<"$found"
        cf_delete_owned "/zones/$CF_PENDING_ZONE/rulesets/$ruleset_id/rules/$rule_id" || return 1
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    origin-ruleset-create|ssl-ruleset-create)
      if found="$(cf_find_ruleset_by_marker "$CF_PENDING_ZONE" "$CF_PENDING_PHASE" "$CF_PENDING_MARKER")"; then
        ruleset="$(cf_api GET "/zones/$CF_PENDING_ZONE/rulesets/$found")" || return 1
        rule_count="$(jq '.result.rules | length' <<<"$ruleset")"
        marked_count="$(jq -r --arg ref "$CF_PENDING_VALUE" --arg marker "$CF_PENDING_MARKER" '[.result.rules[]? | select((.ref // "")==$ref and ((.description // "") | endswith(" txn:"+$marker)))] | length' <<<"$ruleset")"
        if [ "$rule_count" -eq 0 ] || { [ "$rule_count" -eq 1 ] && [ "$marked_count" -eq 1 ]; }; then
          cf_delete_owned "/zones/$CF_PENDING_ZONE/rulesets/$found" || return 1
        else
          return 1
        fi
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    *) return 1 ;;
  esac
  cf_clear_pending_write
}
'''
text = replace_between(text, 'cf_clear_pending_write(){', 'delete_recorded_cloudflare_resources(){', helpers)

reconcile = r'''cf_issue_origin_cert(){
  local zone_id="$1" host="$2" stage="$3" key="$stage/new.key" csr="$stage/new.csr" crt="$stage/new.crt" csr_value body res key_pub cert_pub
  CF_RESULT_CERT_ID=""
  umask 077
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$key" >/dev/null 2>&1
  openssl req -new -sha256 -key "$key" -out "$csr" -subj "/CN=$host" -addext "subjectAltName=DNS:$host"
  csr_value="$(cat "$csr")"
  body="$(jq -n --arg csr "$csr_value" --arg host "$host" '{hostnames:[$host],request_type:"origin-rsa",requested_validity:1095,csr:$csr}')"
  cf_set_pending_write origin-cert-create "$zone_id" "$host" "$csr_value" "" ""
  res="$(cf_api POST '/certificates' "$body")" || die "Cloudflare Origin CA certificate issuance response was not confirmed. Durable transaction recovery will reconcile the exact CSR before continuing."
  jq -r '.result.certificate // empty' <<<"$res" > "$crt"
  CF_RESULT_CERT_ID="$(jq -r '.result.id // empty' <<<"$res")"
  [ -s "$crt" ] && [ -n "$CF_RESULT_CERT_ID" ] || die "Cloudflare returned an incomplete Origin CA certificate response."
  openssl pkey -in "$key" -check -noout >/dev/null 2>&1 || die "Generated private key failed validation."
  openssl x509 -in "$crt" -noout -checkhost "$host" >/dev/null || die "Issued certificate does not match $host."
  key_pub="$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  cert_pub="$(openssl x509 -in "$crt" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  [ "$key_pub" = "$cert_pub" ] || die "Issued certificate does not match the generated private key."
}

cf_reconcile_dns(){
  local zone_id="$1" host="$2" ip="$3" owned_dns_id="${4:-}" res count id type content proxied ttl body owned=false marker comment
  CF_RESULT_DNS_ID=""; CF_RESULT_DNS_OWNED=false; CF_RESULT_DNS_ACTION=""; CF_RESULT_DNS_OLD_CONTENT=""; CF_RESULT_DNS_OLD_PROXIED=""; CF_RESULT_DNS_OLD_TTL=""
  res="$(cf_api GET "/zones/$zone_id/dns_records?name=$host&per_page=100")" || die "Cloudflare DNS lookup failed."
  count="$(jq '.result | length' <<<"$res")"
  if [ "$count" -eq 0 ]; then
    marker="$(cf_new_ownership_marker)"; comment="Managed by AI Server Agent txn:$marker"
    body="$(jq -n --arg name "$host" --arg ip "$ip" --arg comment "$comment" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:$comment}')"
    cf_set_pending_write dns-create "$zone_id" "$host" "$ip" "" "$marker"
    res="$(cf_api POST "/zones/$zone_id/dns_records" "$body")" || die "Cloudflare DNS create response was not confirmed. Durable transaction recovery will require the exact ownership marker."
    id="$(jq -r '.result.id // empty' <<<"$res")"; [ -n "$id" ] || die "Cloudflare did not return the created DNS record ID."
    CF_RESULT_DNS_ID="$id"; CF_RESULT_DNS_OWNED=true; CF_RESULT_DNS_ACTION=created
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
  cf_checkpoint_transaction
  body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
  cf_api PATCH "/zones/$zone_id/dns_records/$id" "$body" >/dev/null || die "Could not update Cloudflare DNS record for $host."
}

cf_reconcile_origin_rule(){
  local zone_id="$1" host="$2" port="$3" owned_ruleset="${4:-}" owned_rule="${5:-}"
  local list ruleset_id rule_ref rule_id ref_match rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id
  CF_RESULT_ORIGIN_RULESET_ID=""; CF_RESULT_ORIGIN_RULE_ID=""; CF_RESULT_ORIGIN_ACTION=""
  rule_ref="ai_server_agent_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" '{ref:$ref,description:"AI Server Agent origin port",expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_request_origin") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    marker="$(cf_new_ownership_marker)"; ruleset_desc="Hostname-scoped origin routing managed by AI Server Agent txn:$marker"
    pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" --arg desc "AI Server Agent origin port txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
    create_body="$(jq -n --argjson rule "$pending_rule_body" --arg desc "$ruleset_desc" '{name:"AI Server Agent Origin Rules",description:$desc,kind:"zone",phase:"http_request_origin",rules:[$rule]}')"
    cf_set_pending_write origin-ruleset-create "$zone_id" "$host" "$rule_ref" http_request_origin "$marker"
    res="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Cloudflare Origin Rules create response was not confirmed. Durable recovery will require the exact ownership marker."
    CF_RESULT_ORIGIN_RULESET_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.rules[0].id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=ruleset-created
    [ -n "$CF_RESULT_ORIGIN_RULESET_ID" ] && [ -n "$CF_RESULT_ORIGIN_RULE_ID" ] || die "Cloudflare did not return the created Origin Rules IDs."
    ruleset_id="$CF_RESULT_ORIGIN_RULESET_ID"
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Origin Rules ruleset."
    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then
      rule_id="$(jq -r --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref) | .id' <<<"$ruleset" | head -n1)"
      if [ -n "$rule_id" ]; then
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$rule_id"; CF_RESULT_ORIGIN_ACTION=updated
        cf_checkpoint_transaction
        cf_api PATCH "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id" "$rule_body" >/dev/null || die "Could not update Agent-owned Origin Rule."
      elif [ -n "$ref_match" ]; then
        die "An Origin Rule uses the Agent ref but is not the rule recorded as Agent-owned. Refusing to adopt or overwrite it."
      else
        marker="$(cf_new_ownership_marker)"
        pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" --arg desc "AI Server Agent origin port txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
        cf_set_pending_write origin-rule-create "$zone_id" "$host" "$rule_ref" http_request_origin "$marker"
        res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$pending_rule_body")" || die "Cloudflare Origin Rule recreate response was not confirmed. Durable recovery will require the exact ownership marker."
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=recreated
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Origin Rule already uses the Agent ref. Refusing to adopt or overwrite it."
      marker="$(cf_new_ownership_marker)"
      pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" --arg desc "AI Server Agent origin port txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
      cf_set_pending_write origin-rule-create "$zone_id" "$host" "$rule_ref" http_request_origin "$marker"
      res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$pending_rule_body")" || die "Cloudflare Origin Rule create response was not confirmed. Durable recovery will require the exact ownership marker."
      CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=created
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare Origin Rule."
  if [ -n "$CF_PENDING_MARKER" ]; then
    verify_id="$(jq -r --arg ref "$rule_ref" --arg marker "$CF_PENDING_MARKER" '.result.rules[]? | select(.ref==$ref and ((.description // "") | endswith(" txn:"+$marker))) | .id' <<<"$ruleset" | head -n1)"
  else
    verify_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
  fi
  [ -n "$verify_id" ] || die "Cloudflare Origin Rule was not reconciled cleanly."
  [ -z "$CF_RESULT_ORIGIN_RULE_ID" ] || [ "$CF_RESULT_ORIGIN_RULE_ID" = "$verify_id" ] || die "Cloudflare Origin Rule verification returned an unexpected rule ID."
  CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$verify_id"
}

cf_reconcile_ssl_config_rule(){
  local zone_id="$1" host="$2" owned_ruleset="${3:-}" owned_rule="${4:-}"
  local list ruleset_id rule_ref rule_id ref_match rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id
  CF_RESULT_SSL_RULESET_ID=""; CF_RESULT_SSL_RULE_ID=""; CF_RESULT_SSL_ACTION=""
  rule_ref="ai_server_agent_ssl_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" '{ref:$ref,description:"AI Server Agent strict SSL",expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_config_settings") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    marker="$(cf_new_ownership_marker)"; ruleset_desc="Hostname-scoped configuration managed by AI Server Agent txn:$marker"
    pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --arg desc "AI Server Agent strict SSL txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
    create_body="$(jq -n --argjson rule "$pending_rule_body" --arg desc "$ruleset_desc" '{name:"AI Server Agent Configuration Rules",description:$desc,kind:"zone",phase:"http_config_settings",rules:[$rule]}')"
    cf_set_pending_write ssl-ruleset-create "$zone_id" "$host" "$rule_ref" http_config_settings "$marker"
    res="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Cloudflare Configuration Rules create response was not confirmed. Durable recovery will require the exact ownership marker."
    CF_RESULT_SSL_RULESET_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.rules[0].id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=ruleset-created
    [ -n "$CF_RESULT_SSL_RULESET_ID" ] && [ -n "$CF_RESULT_SSL_RULE_ID" ] || die "Cloudflare did not return the created Configuration Rules IDs."
    ruleset_id="$CF_RESULT_SSL_RULESET_ID"
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Configuration Rules ruleset."
    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then
      rule_id="$(jq -r --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref) | .id' <<<"$ruleset" | head -n1)"
      if [ -n "$rule_id" ]; then
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$rule_id"; CF_RESULT_SSL_ACTION=updated
        cf_checkpoint_transaction
        cf_api PATCH "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id" "$rule_body" >/dev/null || die "Could not update Agent-owned strict SSL Configuration Rule."
      elif [ -n "$ref_match" ]; then
        die "A Configuration Rule uses the Agent SSL ref but is not the rule recorded as Agent-owned. Refusing to adopt or overwrite it."
      else
        marker="$(cf_new_ownership_marker)"
        pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --arg desc "AI Server Agent strict SSL txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
        cf_set_pending_write ssl-rule-create "$zone_id" "$host" "$rule_ref" http_config_settings "$marker"
        res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$pending_rule_body")" || die "Cloudflare Configuration Rule recreate response was not confirmed. Durable recovery will require the exact ownership marker."
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=recreated
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Configuration Rule already uses the Agent SSL ref. Refusing to adopt or overwrite it."
      marker="$(cf_new_ownership_marker)"
      pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --arg desc "AI Server Agent strict SSL txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
      cf_set_pending_write ssl-rule-create "$zone_id" "$host" "$rule_ref" http_config_settings "$marker"
      res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$pending_rule_body")" || die "Cloudflare Configuration Rule create response was not confirmed. Durable recovery will require the exact ownership marker."
      CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=created
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare strict SSL Configuration Rule."
  if [ -n "$CF_PENDING_MARKER" ]; then
    verify_id="$(jq -r --arg ref "$rule_ref" --arg marker "$CF_PENDING_MARKER" '.result.rules[]? | select(.ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict" and ((.description // "") | endswith(" txn:"+$marker))) | .id' <<<"$ruleset" | head -n1)"
  else
    verify_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict") | .id' <<<"$ruleset" | head -n1)"
  fi
  [ -n "$verify_id" ] || die "Cloudflare strict SSL Configuration Rule was not reconciled cleanly."
  [ -z "$CF_RESULT_SSL_RULE_ID" ] || [ "$CF_RESULT_SSL_RULE_ID" = "$verify_id" ] || die "Cloudflare Configuration Rule verification returned an unexpected rule ID."
  CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$verify_id"
}
'''
text = replace_between(text, 'cf_issue_origin_cert(){', 'save_cloudflare_state(){', reconcile)

old_txn = '''save_cloudflare_transaction_state(){
  local host="$1" zone_id="$2" dns_id="$3" dns_action="$4" dns_old_content="$5" dns_old_proxied="$6" dns_old_ttl="$7" origin_ruleset="$8" origin_rule="$9" origin_action="${10}" old_port="${11}" ssl_ruleset="${12}" ssl_rule="${13}" ssl_action="${14}" cert_id="${15}" tmp
  tmp="$(mktemp)"
  jq -n --arg host "$host" --arg zone_id "$zone_id" --arg dns_id "$dns_id" --arg dns_action "$dns_action" --arg dns_old_content "$dns_old_content" --arg dns_old_proxied "$dns_old_proxied" --arg dns_old_ttl "$dns_old_ttl" --arg origin_ruleset "$origin_ruleset" --arg origin_rule "$origin_rule" --arg origin_action "$origin_action" --arg old_port "$old_port" --arg ssl_ruleset "$ssl_ruleset" --arg ssl_rule "$ssl_rule" --arg ssl_action "$ssl_action" --arg cert_id "$cert_id" --arg pending_kind "$CF_PENDING_KIND" --arg pending_zone "$CF_PENDING_ZONE" --arg pending_host "$CF_PENDING_HOST" --arg pending_value "$CF_PENDING_VALUE" --arg pending_phase "$CF_PENDING_PHASE" \\
    '{hostname:$host,zone_id:$zone_id,dns:{id:$dns_id,action:$dns_action,old_content:$dns_old_content,old_proxied:$dns_old_proxied,old_ttl:$dns_old_ttl},origin:{ruleset_id:$origin_ruleset,rule_id:$origin_rule,action:$origin_action,old_port:$old_port},ssl:{ruleset_id:$ssl_ruleset,rule_id:$ssl_rule,action:$ssl_action},certificate_id:$cert_id,pending:{kind:$pending_kind,zone_id:$pending_zone,hostname:$pending_host,value:$pending_value,phase:$pending_phase}}' > "$tmp" || { rm -f "$tmp"; return 1; }
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$CF_TXN_STATE" || { rm -f "$tmp"; return 1; }
  rm -f "$tmp"
}
'''
new_txn = r'''save_cloudflare_transaction_state(){
  local host="$1" zone_id="$2" dns_id="$3" dns_action="$4" dns_old_content="$5" dns_old_proxied="$6" dns_old_ttl="$7" origin_ruleset="$8" origin_rule="$9" origin_action="${10}" old_port="${11}" ssl_ruleset="${12}" ssl_rule="${13}" ssl_action="${14}" cert_id="${15}" tmp
  install -d -o root -g "$AGENT_USER" -m 0750 "$STATE_DIR" || return 1
  tmp="$(mktemp "$STATE_DIR/.cloudflare-transaction.XXXXXX")" || return 1
  chmod 0600 "$tmp"
  jq -n --arg host "$host" --arg zone_id "$zone_id" --arg dns_id "$dns_id" --arg dns_action "$dns_action" --arg dns_old_content "$dns_old_content" --arg dns_old_proxied "$dns_old_proxied" --arg dns_old_ttl "$dns_old_ttl" --arg origin_ruleset "$origin_ruleset" --arg origin_rule "$origin_rule" --arg origin_action "$origin_action" --arg old_port "$old_port" --arg ssl_ruleset "$ssl_ruleset" --arg ssl_rule "$ssl_rule" --arg ssl_action "$ssl_action" --arg cert_id "$cert_id" --arg pending_kind "$CF_PENDING_KIND" --arg pending_zone "$CF_PENDING_ZONE" --arg pending_host "$CF_PENDING_HOST" --arg pending_value "$CF_PENDING_VALUE" --arg pending_phase "$CF_PENDING_PHASE" --arg pending_marker "$CF_PENDING_MARKER" \
    '{hostname:$host,zone_id:$zone_id,dns:{id:$dns_id,action:$dns_action,old_content:$dns_old_content,old_proxied:$dns_old_proxied,old_ttl:$dns_old_ttl},origin:{ruleset_id:$origin_ruleset,rule_id:$origin_rule,action:$origin_action,old_port:$old_port},ssl:{ruleset_id:$ssl_ruleset,rule_id:$ssl_rule,action:$ssl_action},certificate_id:$cert_id,pending:{kind:$pending_kind,zone_id:$pending_zone,hostname:$pending_host,value:$pending_value,phase:$pending_phase,marker:$pending_marker}}' > "$tmp" || { rm -f "$tmp"; return 1; }
  chown root:"$AGENT_USER" "$tmp" || { rm -f "$tmp"; return 1; }
  chmod 0640 "$tmp" || { rm -f "$tmp"; return 1; }
  mv -f "$tmp" "$CF_TXN_STATE" || { rm -f "$tmp"; return 1; }
}
'''
text = replace_once(text, old_txn, new_txn)

text = replace_once(
    text,
    'CF_PENDING_KIND="$(json_get "$CF_TXN_STATE" \'.pending.kind\')"; CF_PENDING_ZONE="$(json_get "$CF_TXN_STATE" \'.pending.zone_id\')"; CF_PENDING_HOST="$(json_get "$CF_TXN_STATE" \'.pending.hostname\')"; CF_PENDING_VALUE="$(json_get "$CF_TXN_STATE" \'.pending.value\')"; CF_PENDING_PHASE="$(json_get "$CF_TXN_STATE" \'.pending.phase\')"\n',
    'CF_PENDING_KIND="$(json_get "$CF_TXN_STATE" \'.pending.kind\')"; CF_PENDING_ZONE="$(json_get "$CF_TXN_STATE" \'.pending.zone_id\')"; CF_PENDING_HOST="$(json_get "$CF_TXN_STATE" \'.pending.hostname\')"; CF_PENDING_VALUE="$(json_get "$CF_TXN_STATE" \'.pending.value\')"; CF_PENDING_PHASE="$(json_get "$CF_TXN_STATE" \'.pending.phase\')"; CF_PENDING_MARKER="$(json_get "$CF_TXN_STATE" \'.pending.marker\')"\n',
)

old_steps = '''  cert_id=""; dns_id=""; dns_owned=false; dns_action=""; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""; origin_ruleset_id=""; origin_rule_id=""; origin_action=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""
  cf_issue_origin_cert "$zone_id" "$host" "$stage"; cert_id="$CF_RESULT_CERT_ID"; log "Fresh Origin CA certificate issued and verified."
  cf_reconcile_dns "$zone_id" "$host" "$ip" "$owned_dns_id"; dns_id="$CF_RESULT_DNS_ID"; dns_owned="$CF_RESULT_DNS_OWNED"; dns_action="$CF_RESULT_DNS_ACTION"; dns_old_content="$CF_RESULT_DNS_OLD_CONTENT"; dns_old_proxied="$CF_RESULT_DNS_OLD_PROXIED"; dns_old_ttl="$CF_RESULT_DNS_OLD_TTL"
  if [ "$dns_owned" = "true" ]; then log "Cloudflare proxied DNS reconciled ($dns_action, Agent-owned)."; else log "Existing matching proxied DNS reused without taking ownership."; fi
  cf_reconcile_origin_rule "$zone_id" "$host" "$port" "$owned_origin_ruleset" "$owned_origin_rule"; origin_ruleset_id="$CF_RESULT_ORIGIN_RULESET_ID"; origin_rule_id="$CF_RESULT_ORIGIN_RULE_ID"; origin_action="$CF_RESULT_ORIGIN_ACTION"; log "Cloudflare origin port rule reconciled for port $port."
  cf_reconcile_ssl_config_rule "$zone_id" "$host" "$owned_ssl_ruleset" "$owned_ssl_rule"; ssl_ruleset_id="$CF_RESULT_SSL_RULESET_ID"; ssl_rule_id="$CF_RESULT_SSL_RULE_ID"; ssl_action="$CF_RESULT_SSL_ACTION"; log "Cloudflare strict SSL Configuration Rule reconciled for $host only."
'''
new_steps = '''  cert_id=""; dns_id=""; dns_owned=false; dns_action=""; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""; origin_ruleset_id=""; origin_rule_id=""; origin_action=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""
  cf_checkpoint_transaction
  cf_issue_origin_cert "$zone_id" "$host" "$stage"; cert_id="$CF_RESULT_CERT_ID"; cf_finish_transaction_step; log "Fresh Origin CA certificate issued and verified."
  cf_reconcile_dns "$zone_id" "$host" "$ip" "$owned_dns_id"; dns_id="$CF_RESULT_DNS_ID"; dns_owned="$CF_RESULT_DNS_OWNED"; dns_action="$CF_RESULT_DNS_ACTION"; dns_old_content="$CF_RESULT_DNS_OLD_CONTENT"; dns_old_proxied="$CF_RESULT_DNS_OLD_PROXIED"; dns_old_ttl="$CF_RESULT_DNS_OLD_TTL"; cf_finish_transaction_step
  if [ "$dns_owned" = "true" ]; then log "Cloudflare proxied DNS reconciled ($dns_action, Agent-owned)."; else log "Existing matching proxied DNS reused without taking ownership."; fi
  cf_reconcile_origin_rule "$zone_id" "$host" "$port" "$owned_origin_ruleset" "$owned_origin_rule"; origin_ruleset_id="$CF_RESULT_ORIGIN_RULESET_ID"; origin_rule_id="$CF_RESULT_ORIGIN_RULE_ID"; origin_action="$CF_RESULT_ORIGIN_ACTION"; cf_finish_transaction_step; log "Cloudflare origin port rule reconciled for port $port."
  cf_reconcile_ssl_config_rule "$zone_id" "$host" "$owned_ssl_ruleset" "$owned_ssl_rule"; ssl_ruleset_id="$CF_RESULT_SSL_RULESET_ID"; ssl_rule_id="$CF_RESULT_SSL_RULE_ID"; ssl_action="$CF_RESULT_SSL_ACTION"; cf_finish_transaction_step; log "Cloudflare strict SSL Configuration Rule reconciled for $host only."
'''
text = replace_once(text, old_steps, new_steps)
path.write_text(text)

# Rewrite the focused transaction test so it tests nonce ownership rather than semantic adoption.
test_path = Path("tests/cloudflare_transaction.sh")
test = test_path.read_text()
test = test.replace('CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""\ncf_find_dns_by_signature(){ printf \'dns-ambiguous\\n\'; }', 'CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""; CF_PENDING_MARKER=marker-good\ncf_find_dns_by_marker(){ printf \'dns-ambiguous\\n\'; }')
test = test.replace('CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""\ncf_find_dns_by_signature(){ return 2; }', 'CF_PENDING_KIND=dns-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=203.0.113.10; CF_PENDING_PHASE=""; CF_PENDING_MARKER=marker-later\ncf_find_dns_by_marker(){ return 2; }')
test = test.replace('cf_find_dns_by_signature(){ printf \'dns-later\\n\'; }', 'cf_find_dns_by_marker(){ printf \'dns-later\\n\'; }')
test = test.replace('CF_PENDING_KIND=origin-rule-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=ai_server_agent_test; CF_PENDING_PHASE=http_request_origin\ncf_find_rule_by_ref(){ printf \'shared-set|ambiguous-rule\\n\'; }', 'CF_PENDING_KIND=origin-rule-create; CF_PENDING_ZONE=zone1; CF_PENDING_HOST=mcp.example.com; CF_PENDING_VALUE=ai_server_agent_test; CF_PENDING_PHASE=http_request_origin; CF_PENDING_MARKER=rule-marker\ncf_find_rule_by_marker(){ printf \'shared-set|ambiguous-rule\\n\'; }')
append = r'''

# Durable pre-POST journal includes the unpredictable marker before any create request.
rm -f "$CF_TXN_STATE"
host=mcp.example.com; zone_id=zone1; old_port=3210; dns_id=""; dns_action=""; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""; origin_ruleset_id=""; origin_rule_id=""; origin_action=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""; cert_id=""
cf_set_pending_write dns-create zone1 mcp.example.com 203.0.113.10 "" nonce-before-post
test -s "$CF_TXN_STATE"
test "$(jq -r '.pending.marker' "$CF_TXN_STATE")" = nonce-before-post
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create
clear_cloudflare_transaction_state; cf_clear_pending_write

# A same-shape concurrent DNS record without our nonce is ambiguous and must never be deleted.
cf_api(){ printf '%s' '{"success":true,"result":[{"id":"external-dns","type":"A","name":"mcp.example.com","content":"203.0.113.10","proxied":true,"comment":"Managed by AI Server Agent"}]}' ; }
if cf_find_dns_by_marker zone1 mcp.example.com 203.0.113.10 our-secret-marker >/dev/null; then echo 'concurrent DNS was treated as owned' >&2; exit 1; else test "$?" -eq 2; fi

# A matching deterministic ref without our nonce is ambiguous in both rule phases.
cf_api(){
  case "$2" in
    '/zones/zone1/rulesets?per_page=100') printf '%s' '{"success":true,"result":[{"id":"origin-set-race","kind":"zone","phase":"http_request_origin"}]}' ;;
    '/zones/zone1/rulesets/origin-set-race') printf '%s' '{"success":true,"result":{"rules":[{"id":"external-origin","ref":"ai_server_agent_test","description":"AI Server Agent origin port"}]}}' ;;
    *) return 2 ;;
  esac
}
if cf_find_rule_by_marker zone1 http_request_origin ai_server_agent_test our-origin-marker >/dev/null; then echo 'concurrent Origin Rule was treated as owned' >&2; exit 1; else test "$?" -eq 2; fi
cf_api(){
  case "$2" in
    '/zones/zone1/rulesets?per_page=100') printf '%s' '{"success":true,"result":[{"id":"ssl-set-race","kind":"zone","phase":"http_config_settings"}]}' ;;
    '/zones/zone1/rulesets/ssl-set-race') printf '%s' '{"success":true,"result":{"rules":[{"id":"external-ssl","ref":"ai_server_agent_ssl_test","description":"AI Server Agent strict SSL"}]}}' ;;
    *) return 2 ;;
  esac
}
if cf_find_rule_by_marker zone1 http_config_settings ai_server_agent_ssl_test our-ssl-marker >/dev/null; then echo 'concurrent Configuration Rule was treated as owned' >&2; exit 1; else test "$?" -eq 2; fi
'''
if "Durable pre-POST journal includes" not in test:
    test = test.rstrip() + append + "\necho 'cloudflare transaction tests passed'\n"
    # remove the earlier terminal echo so there is one final success marker
    first = test.find("echo 'cloudflare transaction tests passed'\n")
    last = test.rfind("echo 'cloudflare transaction tests passed'\n")
    if first != last:
        test = test[:first] + test[first + len("echo 'cloudflare transaction tests passed'\n"):]
test_path.write_text(test)

crash = r'''#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; TLS_DIR="$CONFIG_DIR/tls"; CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"; REMOTE="$TMP/remote.json"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$TLS_DIR"

set +e
ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$ROOT/manage.sh"
  AGENT_USER=root; CF_TOKEN=test
  cf_new_ownership_marker(){ printf "crashnonce0123456789abcdef01234567\\n"; }
  cf_api(){
    method="$1"; path="$2"; body="${3:-}"
    if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then printf "%s" "{\"success\":true,\"result\":[]}"; return 0; fi
    if [ "$method" = POST ] && [ "$path" = /zones/zone1/dns_records ]; then
      printf "%s" "$body" | jq -c ". + {id:\"dns-crashed\"}" > "$REMOTE"
      kill -KILL "$BASHPID"
    fi
    return 2
  }
  cf_reconcile_dns zone1 mcp.example.com 203.0.113.10 ""
'
rc=$?
set -e
[ "$rc" -ne 0 ]
test -s "$CF_TXN_STATE"
test -s "$REMOTE"
test "$(jq -r '.pending.kind' "$CF_TXN_STATE")" = dns-create
marker="$(jq -r '.pending.marker' "$CF_TXN_STATE")"
test -n "$marker"
test "$(jq -r '.comment' "$REMOTE")" = "Managed by AI Server Agent txn:$marker"

# New process: reload only the durable journal and remote state, then recover.
ROOT="$ROOT" STATE_DIR="$STATE_DIR" CONFIG_DIR="$CONFIG_DIR" TLS_DIR="$TLS_DIR" CF_TXN_STATE="$CF_TXN_STATE" REMOTE="$REMOTE" bash -c '
  set -Eeuo pipefail
  export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
  source "$ROOT/manage.sh"
  AGENT_USER=root; CF_TOKEN=test
  cf_api(){
    method="$1"; path="$2"
    if [ "$method" = GET ] && [[ "$path" == /zones/zone1/dns_records* ]]; then
      if [ -s "$REMOTE" ]; then jq -c "{success:true,result:[.] }" "$REMOTE"; else printf "%s" "{\"success\":true,\"result\":[]}"; fi
      return 0
    fi
    return 2
  }
  cf_delete_owned(){ case "$1" in /zones/zone1/dns_records/dns-crashed) rm -f "$REMOTE"; return 0 ;; *) return 1 ;; esac; }
  recover_cloudflare_transaction
  test ! -e "$CF_TXN_STATE"
  test ! -e "$REMOTE"
'

echo 'cloudflare crash-recovery test passed'
'''
Path("tests/cloudflare_crash_recovery.sh").write_text(crash)
print("durable Cloudflare journal hardening applied")
