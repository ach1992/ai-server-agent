from pathlib import Path


def replace_between(text: str, start: str, end: str, replacement: str) -> str:
    i = text.find(start)
    if i < 0:
        raise SystemExit(f"start marker not found: {start}")
    j = text.find(end, i + len(start))
    if j < 0:
        raise SystemExit(f"end marker not found after {start}: {end}")
    return text[:i] + replacement.rstrip() + "\n\n" + text[j:]


def replace_once(text: str, old: str, new: str) -> str:
    if text.count(old) != 1:
        raise SystemExit(f"expected exactly one occurrence, got {text.count(old)}: {old[:120]}")
    return text.replace(old, new, 1)

install_path = Path("install.sh")
install = install_path.read_text()
install = replace_once(
    install,
    'if [ "$RESOLVE_REF_ONLY" -eq 1 ]; then resolve_source_ref "$REF"; exit 0; fi\nif [ "$CHECK_ONLY" -eq 1 ]; then log "Compatibility check passed: $PRETTY_NAME, $ARCH, systemd available."; exit 0; fi',
    'if [ "$AGENT_VERSION" != "source" ] && [ -n "${AI_SERVER_AGENT_BINARY:-}" ]; then\n'
    '  die "AI_SERVER_AGENT_BINARY is disabled for stable releases. Stable installs must use the release archive verified by SHA256SUMS."\n'
    'fi\n'
    'if [ "$RESOLVE_REF_ONLY" -eq 1 ]; then resolve_source_ref "$REF"; exit 0; fi\n'
    'if [ "$CHECK_ONLY" -eq 1 ]; then log "Compatibility check passed: $PRETTY_NAME, $ARCH, systemd available."; exit 0; fi'
)
install_path.write_text(install)

update_path = Path("update.sh")
update = update_path.read_text()
update = replace_once(
    update,
    '''latest_stable(){
  local json tag
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "https://api.github.com/repos/$REPO/releases/latest")" || { echo "Could not read latest GitHub release" >&2; exit 1; }
  tag="$(printf '%s' "$json" | jq -r '.tag_name // empty')"
  [[ "$tag" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+$ ]] || { echo "Latest release tag is not a stable semantic version: $tag" >&2; exit 1; }
  printf '%s\\n' "$tag"
}''',
    '''latest_stable(){
  local json tag
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2026-03-10' "https://api.github.com/repos/$REPO/releases/latest")" || { echo "Could not read latest GitHub release" >&2; exit 1; }
  jq -e '.draft == false and .prerelease == false and .immutable == true' >/dev/null <<<"$json" || { echo "Latest GitHub release is not a published immutable stable release" >&2; exit 1; }
  tag="$(printf '%s' "$json" | jq -r '.tag_name // empty')"
  [[ "$tag" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+$ ]] || { echo "Latest release tag is not a stable semantic version: $tag" >&2; exit 1; }
  printf '%s\\n' "$tag"
}

verify_stable_release(){
  local version="$1" json
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2026-03-10' "https://api.github.com/repos/$REPO/releases/tags/$version")" || { echo "Could not read GitHub release $version" >&2; exit 1; }
  jq -e --arg version "$version" '.tag_name == $version and .draft == false and .prerelease == false and .immutable == true' >/dev/null <<<"$json" || { echo "Stable update requires a published immutable release for $version" >&2; exit 1; }
}'''
)
update = replace_once(
    update,
    '''  TARGET_TRACK_REF="$TARGET_VERSION"
else''',
    '''  TARGET_TRACK_REF="$TARGET_VERSION"
  [ -z "${AI_SERVER_AGENT_BINARY:-}" ] || { echo "AI_SERVER_AGENT_BINARY is disabled for stable updates" >&2; exit 1; }
  verify_stable_release "$TARGET_VERSION"
else'''
)
update = replace_once(
    update,
    '''TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
curl -fsSLG \\
  -H 'Accept: application/vnd.github.raw+json' \\
  -H 'X-GitHub-Api-Version: 2022-11-28' \\
  --data-urlencode "ref=$TARGET_REF" \\
  "https://api.github.com/repos/$REPO/contents/install.sh" \\
  -o "$TMP/install.sh"
chmod +x "$TMP/install.sh"

AI_SERVER_AGENT_VERSION="$TARGET_VERSION" \\
AI_SERVER_AGENT_REF="$TARGET_REF" \\
AI_SERVER_AGENT_TRACK_REF="$TARGET_TRACK_REF" \\
AI_SERVER_AGENT_NONINTERACTIVE=1 \\
AI_SERVER_AGENT_SETUP_MODE=keep \\
  bash "$TMP/install.sh"''',
    '''TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
if [ "$CHANNEL" = "stable" ]; then
  curl -fsSL "https://github.com/$REPO/releases/download/$TARGET_VERSION/install.sh" -o "$TMP/install.sh"
  grep -qF "export AI_SERVER_AGENT_VERSION=$TARGET_VERSION" "$TMP/install.sh" || { echo "Release installer does not pin $TARGET_VERSION" >&2; exit 1; }
  grep -qF "export AI_SERVER_AGENT_REF=$TARGET_VERSION" "$TMP/install.sh" || { echo "Release installer ref does not pin $TARGET_VERSION" >&2; exit 1; }
  chmod +x "$TMP/install.sh"
  AI_SERVER_AGENT_NONINTERACTIVE=1 \\
  AI_SERVER_AGENT_SETUP_MODE=keep \\
    bash "$TMP/install.sh"
else
  curl -fsSLG \\
    -H 'Accept: application/vnd.github.raw+json' \\
    -H 'X-GitHub-Api-Version: 2022-11-28' \\
    --data-urlencode "ref=$TARGET_REF" \\
    "https://api.github.com/repos/$REPO/contents/install.sh" \\
    -o "$TMP/install.sh"
  chmod +x "$TMP/install.sh"
  AI_SERVER_AGENT_VERSION="$TARGET_VERSION" \\
  AI_SERVER_AGENT_REF="$TARGET_REF" \\
  AI_SERVER_AGENT_TRACK_REF="$TARGET_TRACK_REF" \\
  AI_SERVER_AGENT_NONINTERACTIVE=1 \\
  AI_SERVER_AGENT_SETUP_MODE=keep \\
    bash "$TMP/install.sh"
fi'''
)
update_path.write_text(update)

manage_path = Path("manage.sh")
manage = manage_path.read_text()
manage = replace_once(manage, 'CF_TOKEN=""\n', 'CF_TOKEN=""\nCF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"\n')
manage = replace_between(manage, 'cf_api(){', 'cf_delete_owned(){', r'''cf_api(){
  local method="$1" path="$2" body="${3:-}" cfg out
  local -a retry_args=()
  cfg="$(mktemp)"; chmod 0600 "$cfg"
  printf 'header = "Authorization: Bearer %s"\n' "$CF_TOKEN" > "$cfg"
  case "$method" in GET|PATCH|DELETE) retry_args=(--retry 2) ;; esac
  if [ -n "$body" ]; then
    out="$(curl -sS --fail-with-body "${retry_args[@]}" --request "$method" --config "$cfg" -H 'Content-Type: application/json' --data-binary "$body" "$CF_API$path")" || { rm -f "$cfg"; return 1; }
  else
    out="$(curl -sS --fail-with-body "${retry_args[@]}" --request "$method" --config "$cfg" -H 'Content-Type: application/json' "$CF_API$path")" || { rm -f "$cfg"; return 1; }
  fi
  rm -f "$cfg"
  if ! jq -e '.success == true' >/dev/null 2>&1 <<<"$out"; then
    jq -r '.errors[]?.message // empty' <<<"$out" >&2 || true
    return 1
  fi
  printf '%s' "$out"
}''')
manage = replace_between(manage, 'cf_issue_origin_cert(){', 'cf_reconcile_dns(){', r'''cf_issue_origin_cert(){
  local host="$1" stage="$2" key="$stage/new.key" csr="$stage/new.csr" crt="$stage/new.crt" body res key_pub cert_pub
  CF_RESULT_CERT_ID=""
  umask 077
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$key" >/dev/null 2>&1
  openssl req -new -sha256 -key "$key" -out "$csr" -subj "/CN=$host" -addext "subjectAltName=DNS:$host"
  body="$(jq -n --rawfile csr "$csr" --arg host "$host" '{hostnames:[$host],request_type:"origin-rsa",requested_validity:1095,csr:$csr}')"
  res="$(cf_api POST '/certificates' "$body")" || die "Cloudflare Origin CA certificate issuance failed. Check token access and hostname ownership."
  jq -r '.result.certificate // empty' <<<"$res" > "$crt"
  CF_RESULT_CERT_ID="$(jq -r '.result.id // empty' <<<"$res")"
  [ -s "$crt" ] && [ -n "$CF_RESULT_CERT_ID" ] || die "Cloudflare returned an incomplete Origin CA certificate response."
  openssl pkey -in "$key" -check -noout >/dev/null 2>&1 || die "Generated private key failed validation."
  openssl x509 -in "$crt" -noout -checkhost "$host" >/dev/null || die "Issued certificate does not match $host."
  key_pub="$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  cert_pub="$(openssl x509 -in "$crt" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  [ "$key_pub" = "$cert_pub" ] || die "Issued certificate does not match the generated private key."
}''')
manage = replace_between(manage, 'cf_reconcile_dns(){', 'cf_reconcile_origin_rule(){', r'''cf_reconcile_dns(){
  local zone_id="$1" host="$2" ip="$3" owned_dns_id="${4:-}" res count id type content proxied ttl body owned=false
  CF_RESULT_DNS_ID=""; CF_RESULT_DNS_OWNED=false; CF_RESULT_DNS_ACTION=""; CF_RESULT_DNS_OLD_CONTENT=""; CF_RESULT_DNS_OLD_PROXIED=""; CF_RESULT_DNS_OLD_TTL=""
  res="$(cf_api GET "/zones/$zone_id/dns_records?name=$host&per_page=100")" || die "Cloudflare DNS lookup failed."
  count="$(jq '.result | length' <<<"$res")"
  if [ "$count" -eq 0 ]; then
    body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
    res="$(cf_api POST "/zones/$zone_id/dns_records" "$body")" || die "Could not create Cloudflare DNS record for $host."
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
  body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
  cf_api PATCH "/zones/$zone_id/dns_records/$id" "$body" >/dev/null || die "Could not update Cloudflare DNS record for $host."
}''')
manage = replace_between(manage, 'cf_reconcile_origin_rule(){', 'cf_reconcile_ssl_config_rule(){', r'''cf_reconcile_origin_rule(){
  local zone_id="$1" host="$2" port="$3" owned_ruleset="${4:-}" owned_rule="${5:-}"
  local list ruleset_id rule_ref rule_id ref_match rule_body create_body res ruleset
  CF_RESULT_ORIGIN_RULESET_ID=""; CF_RESULT_ORIGIN_RULE_ID=""; CF_RESULT_ORIGIN_ACTION=""
  rule_ref="ai_server_agent_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" '{ref:$ref,description:"AI Server Agent origin port",expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_request_origin") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    create_body="$(jq -n --argjson rule "$rule_body" '{name:"AI Server Agent Origin Rules",description:"Hostname-scoped origin routing managed by AI Server Agent",kind:"zone",phase:"http_request_origin",rules:[$rule]}')"
    res="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Could not create Cloudflare Origin Rules ruleset."
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
        res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body")" || die "Could not recreate Agent Origin Rule."
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=recreated
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Origin Rule already uses the Agent ref. Refusing to adopt or overwrite it."
      res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body")" || die "Could not create Agent Origin Rule."
      CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=created
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare Origin Rule."
  rule_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
  [ -n "$rule_id" ] || die "Cloudflare Origin Rule was not reconciled cleanly."
  CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$rule_id"
}''')
manage = replace_between(manage, 'cf_reconcile_ssl_config_rule(){', 'save_cloudflare_state(){', r'''cf_reconcile_ssl_config_rule(){
  local zone_id="$1" host="$2" owned_ruleset="${3:-}" owned_rule="${4:-}"
  local list ruleset_id rule_ref rule_id ref_match rule_body create_body res ruleset
  CF_RESULT_SSL_RULESET_ID=""; CF_RESULT_SSL_RULE_ID=""; CF_RESULT_SSL_ACTION=""
  rule_ref="ai_server_agent_ssl_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" '{ref:$ref,description:"AI Server Agent strict SSL",expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_config_settings") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    create_body="$(jq -n --argjson rule "$rule_body" '{name:"AI Server Agent Configuration Rules",description:"Hostname-scoped configuration managed by AI Server Agent",kind:"zone",phase:"http_config_settings",rules:[$rule]}')"
    res="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Could not create Cloudflare Configuration Rules ruleset. Check Config Rules Edit permission."
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
        res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body")" || die "Could not recreate Agent strict SSL Configuration Rule."
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=recreated
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Configuration Rule already uses the Agent SSL ref. Refusing to adopt or overwrite it."
      res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body")" || die "Could not create Agent strict SSL Configuration Rule."
      CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=created
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare strict SSL Configuration Rule."
  rule_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict") | .id' <<<"$ruleset" | head -n1)"
  [ -n "$rule_id" ] || die "Cloudflare strict SSL Configuration Rule was not reconciled cleanly."
  CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$rule_id"
}''')
manage = replace_once(manage, 'save_local_state(){', r'''save_previous_cloudflare_certificate(){
  local host="$1" cert_id="$2" tmp old
  [ -n "$cert_id" ] || return 0
  tmp="$(mktemp)"; if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --arg cert_id "$cert_id" '.cloudflare_previous_certificate={hostname:$host,origin_certificate_id:$cert_id}' <<<"$old" > "$tmp"
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
}

clear_previous_cloudflare_certificate(){
  local tmp old
  [ -s "$MANAGED_STATE" ] || return 0
  tmp="$(mktemp)"; old="$(cat "$MANAGED_STATE")"
  jq 'del(.cloudflare_previous_certificate)' <<<"$old" > "$tmp"
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
}

save_cloudflare_transaction_state(){
  local host="$1" zone_id="$2" dns_id="$3" dns_action="$4" dns_old_content="$5" dns_old_proxied="$6" dns_old_ttl="$7" origin_ruleset="$8" origin_rule="$9" origin_action="${10}" old_port="${11}" ssl_ruleset="${12}" ssl_rule="${13}" ssl_action="${14}" cert_id="${15}" tmp
  tmp="$(mktemp)"
  jq -n --arg host "$host" --arg zone_id "$zone_id" --arg dns_id "$dns_id" --arg dns_action "$dns_action" --arg dns_old_content "$dns_old_content" --arg dns_old_proxied "$dns_old_proxied" --arg dns_old_ttl "$dns_old_ttl" --arg origin_ruleset "$origin_ruleset" --arg origin_rule "$origin_rule" --arg origin_action "$origin_action" --arg old_port "$old_port" --arg ssl_ruleset "$ssl_ruleset" --arg ssl_rule "$ssl_rule" --arg ssl_action "$ssl_action" --arg cert_id "$cert_id" \
    '{hostname:$host,zone_id:$zone_id,dns:{id:$dns_id,action:$dns_action,old_content:$dns_old_content,old_proxied:$dns_old_proxied,old_ttl:$dns_old_ttl},origin:{ruleset_id:$origin_ruleset,rule_id:$origin_rule,action:$origin_action,old_port:$old_port},ssl:{ruleset_id:$ssl_ruleset,rule_id:$ssl_rule,action:$ssl_action},certificate_id:$cert_id}' > "$tmp" || { rm -f "$tmp"; return 1; }
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$CF_TXN_STATE" || { rm -f "$tmp"; return 1; }
  rm -f "$tmp"
}

clear_cloudflare_transaction_state(){ rm -f "$CF_TXN_STATE"; }

save_local_state(){''')
manage = replace_between(manage, 'rollback_new_cf_resources(){', 'cleanup_old_cloudflare(){', r'''restore_updated_dns_record(){
  local zone_id="$1" host="$2" dns_id="$3" old_content="$4" old_proxied="$5" old_ttl="$6" body
  [ -n "$zone_id" ] && [ -n "$host" ] && [ -n "$dns_id" ] && [ -n "$old_content" ] && [ -n "$old_proxied" ] && [ -n "$old_ttl" ] || return 1
  body="$(jq -n --arg name "$host" --arg content "$old_content" --argjson proxied "$old_proxied" --argjson ttl "$old_ttl" '{type:"A",name:$name,content:$content,ttl:$ttl,proxied:$proxied,comment:"Managed by AI Server Agent"}')"
  cf_api PATCH "/zones/$zone_id/dns_records/$dns_id" "$body" >/dev/null || { warn "Could not restore the previous Agent-owned Cloudflare DNS record automatically."; return 1; }
}

restore_updated_origin_rule(){
  local zone_id="$1" host="$2" old_port="$3" origin_ruleset="$4" origin_rule="$5" rule_ref body
  [ -n "$zone_id" ] && [ -n "$host" ] && [ -n "$old_port" ] && [ -n "$origin_ruleset" ] && [ -n "$origin_rule" ] || return 1
  rule_ref="ai_server_agent_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$old_port" '{ref:$ref,description:"AI Server Agent origin port",expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
  cf_api PATCH "/zones/$zone_id/rulesets/$origin_ruleset/rules/$origin_rule" "$body" >/dev/null || { warn "Could not restore the previous Agent-owned Cloudflare origin port rule automatically."; return 1; }
}

rollback_new_cf_resources(){
  local host="$1" zone_id="$2" dns_id="$3" dns_action="$4" dns_old_content="$5" dns_old_proxied="$6" dns_old_ttl="$7" origin_ruleset="$8" origin_rule="$9" origin_action="${10}" old_port="${11}" ssl_ruleset="${12}" ssl_rule="${13}" ssl_action="${14}" cert_id="${15}"
  local keep_dns_id="" keep_dns_action="" keep_dns_old_content="" keep_dns_old_proxied="" keep_dns_old_ttl=""
  local keep_origin_ruleset="" keep_origin_rule="" keep_origin_action="" keep_ssl_ruleset="" keep_ssl_rule="" keep_ssl_action="" keep_cert_id="" failed=0
  case "$dns_action" in
    created) if [ -n "$dns_id" ] && ! cf_delete_owned "/zones/$zone_id/dns_records/$dns_id"; then keep_dns_id="$dns_id"; keep_dns_action=created; failed=1; fi ;;
    updated) if ! restore_updated_dns_record "$zone_id" "$host" "$dns_id" "$dns_old_content" "$dns_old_proxied" "$dns_old_ttl"; then keep_dns_id="$dns_id"; keep_dns_action=updated; keep_dns_old_content="$dns_old_content"; keep_dns_old_proxied="$dns_old_proxied"; keep_dns_old_ttl="$dns_old_ttl"; failed=1; fi ;;
  esac
  case "$origin_action" in
    ruleset-created) if [ -n "$origin_ruleset" ] && ! cf_delete_owned "/zones/$zone_id/rulesets/$origin_ruleset"; then keep_origin_ruleset="$origin_ruleset"; keep_origin_rule="$origin_rule"; keep_origin_action=ruleset-created; failed=1; fi ;;
    created|recreated) if [ -n "$origin_rule" ] && [ -n "$origin_ruleset" ] && ! cf_delete_owned "/zones/$zone_id/rulesets/$origin_ruleset/rules/$origin_rule"; then keep_origin_ruleset="$origin_ruleset"; keep_origin_rule="$origin_rule"; keep_origin_action="$origin_action"; failed=1; fi ;;
    updated) if ! restore_updated_origin_rule "$zone_id" "$host" "$old_port" "$origin_ruleset" "$origin_rule"; then keep_origin_ruleset="$origin_ruleset"; keep_origin_rule="$origin_rule"; keep_origin_action=updated; failed=1; fi ;;
  esac
  case "$ssl_action" in
    ruleset-created) if [ -n "$ssl_ruleset" ] && ! cf_delete_owned "/zones/$zone_id/rulesets/$ssl_ruleset"; then keep_ssl_ruleset="$ssl_ruleset"; keep_ssl_rule="$ssl_rule"; keep_ssl_action=ruleset-created; failed=1; fi ;;
    created|recreated) if [ -n "$ssl_rule" ] && [ -n "$ssl_ruleset" ] && ! cf_delete_owned "/zones/$zone_id/rulesets/$ssl_ruleset/rules/$ssl_rule"; then keep_ssl_ruleset="$ssl_ruleset"; keep_ssl_rule="$ssl_rule"; keep_ssl_action="$ssl_action"; failed=1; fi ;;
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
  rollback_new_cf_resources "$host" "$zone_id" "$dns_id" "$dns_action" "$dns_old_content" "$dns_old_proxied" "$dns_old_ttl" "$origin_ruleset" "$origin_rule" "$origin_action" "$old_port" "$ssl_ruleset" "$ssl_rule" "$ssl_action" "$cert_id"
}''')
manage = replace_between(manage, 'cleanup_old_cloudflare(){', 'configure_cloudflare(){', r'''cleanup_old_cloudflare(){
  local old_host="$1" old_zone="$2" old_dns="$3" old_dns_owned="$4" old_origin_ruleset="$5" old_origin_rule="$6" old_ssl_ruleset="$7" old_ssl_rule="$8" old_cert="$9" new_host="${10}" new_cert="${11}" new_zone="${12}"
  [ -n "$old_host" ] || return 0
  if [ "$old_host" = "$new_host" ]; then
    if [ -n "$old_cert" ] && [ "$old_cert" != "$new_cert" ]; then
      if confirm "Revoke the previous Cloudflare Origin CA certificate now that the new certificate is verified?" yes; then
        if cf_delete_owned "/certificates/$old_cert"; then clear_previous_cloudflare_certificate; else warn "Previous Origin CA certificate could not be revoked; its ID remains recorded for later cleanup."; fi
      else
        warn "Previous Origin CA certificate was preserved and remains recorded for later cleanup."
      fi
    fi
    return 0
  fi
  printf '\nOld managed hostname: %s\n' "$old_host"
  if [ "$old_zone" != "$new_zone" ]; then
    warn "The old hostname belongs to a different Cloudflare zone. Its Agent-owned resources remain recorded for later cleanup with a token scoped to that old zone."
    return 0
  fi
  if ! confirm "Remove the old Agent-managed DNS, Origin Rule, strict SSL Configuration Rule, and certificate now?" yes; then
    warn "Old Agent-managed Cloudflare resources were preserved and remain recorded for later cleanup."
    return 0
  fi
  if delete_recorded_cloudflare_resources "$old_zone" "$old_dns" "$old_dns_owned" "$old_origin_ruleset" "$old_origin_rule" "$old_ssl_ruleset" "$old_ssl_rule" "$old_cert"; then
    clear_previous_cloudflare_state
    log "Old Agent-managed Cloudflare resources were removed or already absent."
  else
    warn "Old Cloudflare cleanup was incomplete. Recorded ownership state was preserved for a safe retry."
  fi
}''')
manage = replace_between(manage, 'configure_cloudflare(){', 'configure_local(){', r'''configure_cloudflare(){ (
  set -Eeuo pipefail
  need_cmd jq; need_cmd openssl; need_cmd curl; need_cmd sha256sum
  local old_host old_zone old_dns old_dns_owned old_origin_ruleset old_origin_rule old_ssl_ruleset old_ssl_rule old_cert old_port previous_zone previous_cert
  local host port zone_pair zone_id zone_name ip stage backup cert_id dns_id dns_owned dns_action dns_old_content dns_old_proxied dns_old_ttl
  local origin_ruleset_id origin_rule_id origin_action ssl_ruleset_id ssl_rule_id ssl_action config_backup managed_backup managed_existed=0
  local owned_dns_id="" owned_origin_ruleset="" owned_origin_rule="" owned_ssl_ruleset="" owned_ssl_rule="" local_mutation_started=0 managed_mutation_started=0 transaction_committed=0
  old_host="$(managed_get '.hostname')"; old_zone="$(managed_get '.cloudflare.zone_id')"; old_dns="$(managed_get '.cloudflare.dns_record_id')"; old_dns_owned="$(managed_get '.cloudflare.dns_record_owned')"; old_dns_owned="${old_dns_owned:-false}"
  old_origin_ruleset="$(managed_get '.cloudflare.origin_ruleset_id')"; old_origin_rule="$(managed_get '.cloudflare.origin_rule_id')"
  old_ssl_ruleset="$(managed_get '.cloudflare.ssl_config_ruleset_id')"; old_ssl_rule="$(managed_get '.cloudflare.ssl_config_rule_id')"; old_cert="$(managed_get '.cloudflare.origin_certificate_id')"
  old_port="$(current_port)"; previous_zone="$(managed_get '.cloudflare_previous.zone_id')"; previous_cert="$(managed_get '.cloudflare_previous_certificate.origin_certificate_id')"
  [ ! -s "$CF_TXN_STATE" ] || die "An incomplete Cloudflare rollback is recorded. Run 'sudo ai-server-agent-manage cloudflare-cleanup' before configuring Cloudflare again."
  [ -z "$previous_zone" ] || die "A previous Cloudflare hostname still has recorded Agent-managed resources. Run 'sudo ai-server-agent-manage cloudflare-cleanup' first."
  [ -z "$previous_cert" ] || die "A previous Cloudflare Origin CA certificate still needs cleanup. Run 'sudo ai-server-agent-manage cloudflare-cleanup' first."
  host="${AI_SERVER_AGENT_HOSTNAME:-}"; [ -n "$host" ] || host="$(prompt_value 'Public MCP hostname' "${old_host:-mcp.example.com}")"; host="${host,,}"; validate_hostname "$host"
  port="${AI_SERVER_AGENT_PORT:-$(current_port)}"; [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1024 ] && [ "$port" -le 65535 ] || die "Port must be between 1024 and 65535."
  printf '\nCloudflare will manage only this MCP hostname: proxied DNS, an origin-port rule, a hostname-scoped strict SSL Configuration Rule, and an Origin CA certificate.\n'
  printf 'The whole-zone SSL mode will not be changed.\n'
  print_cf_token_guidance "$host"
  load_cf_token
  zone_pair="$(cf_find_zone "$host")"; IFS='|' read -r zone_id zone_name <<<"$zone_pair"; log "Cloudflare zone: $zone_name"
  if [ "$old_host" = "$host" ] && [ "$old_zone" = "$zone_id" ]; then
    [ "$old_dns_owned" = "true" ] && owned_dns_id="$old_dns" || true
    owned_origin_ruleset="$old_origin_ruleset"; owned_origin_rule="$old_origin_rule"; owned_ssl_ruleset="$old_ssl_ruleset"; owned_ssl_rule="$old_ssl_rule"
  fi
  ip="$(cf_public_ipv4)"; log "Origin IPv4: $ip"
  stage="$(mktemp -d)"; backup="$stage/backup"; mkdir -m 0700 "$backup"; chmod 0700 "$stage"
  config_backup="$stage/config.backup"; managed_backup="$stage/managed.backup"
  cp -a "$CONFIG_FILE" "$config_backup"
  if [ -s "$MANAGED_STATE" ]; then cp -a "$MANAGED_STATE" "$managed_backup"; managed_existed=1; fi
  [ -e "$TLS_DIR/origin.key" ] && cp -p "$TLS_DIR/origin.key" "$backup/origin.key" || true
  [ -e "$TLS_DIR/origin.csr" ] && cp -p "$TLS_DIR/origin.csr" "$backup/origin.csr" || true
  [ -e "$TLS_DIR/origin.crt" ] && cp -p "$TLS_DIR/origin.crt" "$backup/origin.crt" || true

  cloudflare_transaction_exit(){
    local rc=$?
    trap - EXIT
    set +e
    if [ "$rc" -ne 0 ] && [ "$transaction_committed" -ne 1 ]; then
      if [ "$local_mutation_started" -eq 1 ]; then
        install -o root -g "$AGENT_USER" -m 0640 "$config_backup" "$CONFIG_FILE"
        restore_tls_backup "$backup"
        systemctl restart ai-server-agent-executor.service ai-server-agent.service >/dev/null 2>&1 || true
      fi
      if [ "$managed_mutation_started" -eq 1 ]; then
        if [ "$managed_existed" -eq 1 ]; then install -o root -g "$AGENT_USER" -m 0640 "$managed_backup" "$MANAGED_STATE"; else rm -f "$MANAGED_STATE"; fi
      fi
      rollback_new_cf_resources "$host" "$zone_id" "$dns_id" "$dns_action" "$dns_old_content" "$dns_old_proxied" "$dns_old_ttl" "$origin_ruleset_id" "$origin_rule_id" "$origin_action" "$old_port" "$ssl_ruleset_id" "$ssl_rule_id" "$ssl_action" "$cert_id" || true
    fi
    rm -rf "$stage"
    CF_TOKEN=""
    return "$rc"
  }
  trap cloudflare_transaction_exit EXIT

  cert_id=""; dns_id=""; dns_owned=false; dns_action=""; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""; origin_ruleset_id=""; origin_rule_id=""; origin_action=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""
  cf_issue_origin_cert "$host" "$stage"; cert_id="$CF_RESULT_CERT_ID"; log "Fresh Origin CA certificate issued and verified."
  cf_reconcile_dns "$zone_id" "$host" "$ip" "$owned_dns_id"; dns_id="$CF_RESULT_DNS_ID"; dns_owned="$CF_RESULT_DNS_OWNED"; dns_action="$CF_RESULT_DNS_ACTION"; dns_old_content="$CF_RESULT_DNS_OLD_CONTENT"; dns_old_proxied="$CF_RESULT_DNS_OLD_PROXIED"; dns_old_ttl="$CF_RESULT_DNS_OLD_TTL"
  if [ "$dns_owned" = "true" ]; then log "Cloudflare proxied DNS reconciled ($dns_action, Agent-owned)."; else log "Existing matching proxied DNS reused without taking ownership."; fi
  cf_reconcile_origin_rule "$zone_id" "$host" "$port" "$owned_origin_ruleset" "$owned_origin_rule"; origin_ruleset_id="$CF_RESULT_ORIGIN_RULESET_ID"; origin_rule_id="$CF_RESULT_ORIGIN_RULE_ID"; origin_action="$CF_RESULT_ORIGIN_ACTION"; log "Cloudflare origin port rule reconciled for port $port."
  cf_reconcile_ssl_config_rule "$zone_id" "$host" "$owned_ssl_ruleset" "$owned_ssl_rule"; ssl_ruleset_id="$CF_RESULT_SSL_RULESET_ID"; ssl_rule_id="$CF_RESULT_SSL_RULE_ID"; ssl_action="$CF_RESULT_SSL_ACTION"; log "Cloudflare strict SSL Configuration Rule reconciled for $host only."

  local_mutation_started=1
  install -d -o root -g "$AGENT_USER" -m 0750 "$TLS_DIR"
  install -o root -g "$AGENT_USER" -m 0640 "$stage/new.key" "$TLS_DIR/origin.key"
  install -o root -g root -m 0644 "$stage/new.csr" "$TLS_DIR/origin.csr"
  install -o root -g "$AGENT_USER" -m 0644 "$stage/new.crt" "$TLS_DIR/origin.crt"
  write_config public "$port" "$TLS_DIR/origin.crt" "$TLS_DIR/origin.key"
  restart_and_verify_local || die "Agent failed after TLS/public reconfiguration; transaction rollback started."
  verify_public "$host" || die "Public Cloudflare verification failed; transaction rollback started."

  managed_mutation_started=1
  if [ -n "$old_host" ] && [ "$old_host" != "$host" ]; then
    save_previous_cloudflare_state "$old_host" "$old_zone" "$old_dns" "$old_dns_owned" "$old_origin_ruleset" "$old_origin_rule" "$old_ssl_ruleset" "$old_ssl_rule" "$old_cert"
  elif [ "$old_host" = "$host" ] && [ -n "$old_cert" ] && [ "$old_cert" != "$cert_id" ]; then
    save_previous_cloudflare_certificate "$old_host" "$old_cert"
  fi
  save_cloudflare_state "$host" "$port" "$zone_id" "$zone_name" "$dns_id" "$dns_owned" "$origin_ruleset_id" "$origin_rule_id" "$ssl_ruleset_id" "$ssl_rule_id" "$cert_id"
  transaction_committed=1
  clear_cloudflare_transaction_state
  log "Public HTTPS health, unauthenticated rejection, and authenticated MCP initialize all passed."
  cleanup_old_cloudflare "$old_host" "$old_zone" "$old_dns" "$old_dns_owned" "$old_origin_ruleset" "$old_origin_rule" "$old_ssl_ruleset" "$old_ssl_rule" "$old_cert" "$host" "$cert_id" "$zone_id"
  printf '\n%sServer setup complete.%s\n' "$GREEN" "$RESET"
  printf 'MCP URL: %shttps://%s/mcp%s\n' "$BOLD" "$host" "$RESET"
  printf 'Next: choose ChatGPT Setup from the menu.\n'
) }''')
manage = replace_between(manage, 'cloudflare_cleanup(){', 'update_agent(){', r'''cloudflare_cleanup(){
  local source zone_id dns_id dns_owned origin_ruleset_id origin_rule_id ssl_ruleset_id ssl_rule_id cert_id host tmp old previous_zone previous_cert
  if [ -s "$CF_TXN_STATE" ]; then
    host="$(json_get "$CF_TXN_STATE" '.hostname')"
    printf 'Incomplete Cloudflare transaction for: %s\n' "$host"
    confirm "Retry the recorded rollback now?" no || { echo "Cancelled."; return 0; }
    print_cf_token_guidance "$host"; load_cf_token
    if recover_cloudflare_transaction; then CF_TOKEN=""; log "Recorded Cloudflare rollback completed."; return 0; fi
    CF_TOKEN=""; die "Cloudflare rollback is still incomplete. Recovery state remains in $CF_TXN_STATE."
  fi
  previous_cert="$(managed_get '.cloudflare_previous_certificate.origin_certificate_id')"
  if [ -n "$previous_cert" ]; then
    host="$(managed_get '.cloudflare_previous_certificate.hostname')"
    printf 'Previous Cloudflare Origin CA certificate for: %s\n' "$host"
    confirm "Revoke this previously replaced certificate now?" no || { echo "Cancelled."; return 0; }
    print_cf_token_guidance "$host"; load_cf_token
    if cf_delete_owned "/certificates/$previous_cert"; then CF_TOKEN=""; clear_previous_cloudflare_certificate; log "Previous Origin CA certificate revoked or already absent."; return 0; fi
    CF_TOKEN=""; die "Previous certificate could not be revoked. Its ID remains recorded for a safe retry."
  fi
  previous_zone="$(managed_get '.cloudflare_previous.zone_id')"
  if [ -n "$previous_zone" ]; then
    source=previous
    zone_id="$previous_zone"; host="$(managed_get '.cloudflare_previous.hostname')"
    dns_id="$(managed_get '.cloudflare_previous.dns_record_id')"; dns_owned="$(managed_get '.cloudflare_previous.dns_record_owned')"; dns_owned="${dns_owned:-false}"
    origin_ruleset_id="$(managed_get '.cloudflare_previous.origin_ruleset_id')"; origin_rule_id="$(managed_get '.cloudflare_previous.origin_rule_id')"
    ssl_ruleset_id="$(managed_get '.cloudflare_previous.ssl_config_ruleset_id')"; ssl_rule_id="$(managed_get '.cloudflare_previous.ssl_config_rule_id')"; cert_id="$(managed_get '.cloudflare_previous.origin_certificate_id')"
    printf 'Deferred Cloudflare cleanup hostname: %s\n' "$host"
  else
    source=current
    zone_id="$(managed_get '.cloudflare.zone_id')"; dns_id="$(managed_get '.cloudflare.dns_record_id')"; dns_owned="$(managed_get '.cloudflare.dns_record_owned')"; dns_owned="${dns_owned:-false}"
    origin_ruleset_id="$(managed_get '.cloudflare.origin_ruleset_id')"; origin_rule_id="$(managed_get '.cloudflare.origin_rule_id')"
    ssl_ruleset_id="$(managed_get '.cloudflare.ssl_config_ruleset_id')"; ssl_rule_id="$(managed_get '.cloudflare.ssl_config_rule_id')"; cert_id="$(managed_get '.cloudflare.origin_certificate_id')"; host="$(managed_get '.hostname')"
    [ -n "$zone_id" ] || { log "No recorded Cloudflare-managed resources."; return 0; }
    if [ "$(current_mode)" = "public" ] && [ "$(managed_get '.active_provider')" = "cloudflare" ]; then die "This Cloudflare hostname is currently carrying the MCP connection. Switch to local/manual mode before cleanup."; fi
    printf 'Recorded Cloudflare hostname: %s\n' "$host"
  fi
  if [ "$dns_owned" = "true" ]; then printf 'The DNS record is recorded as Agent-owned and can be removed by this cleanup.\n'; else printf 'The DNS record is external/reused and will be preserved.\n'; fi
  confirm "Delete the recorded Agent-owned Cloudflare DNS (if any), Origin Rule, strict SSL Configuration Rule, and Origin CA certificate?" no || { echo "Cancelled."; return 0; }
  print_cf_token_guidance "$host"; load_cf_token
  if ! delete_recorded_cloudflare_resources "$zone_id" "$dns_id" "$dns_owned" "$origin_ruleset_id" "$origin_rule_id" "$ssl_ruleset_id" "$ssl_rule_id" "$cert_id"; then CF_TOKEN=""; die "Cloudflare cleanup was incomplete. Recorded ownership state was preserved for a safe retry."; fi
  CF_TOKEN=""
  if [ "$source" = previous ]; then clear_previous_cloudflare_state; else tmp="$(mktemp)"; old="$(cat "$MANAGED_STATE")"; jq '.cloudflare={}' <<<"$old" > "$tmp"; install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"; fi
  log "Recorded Agent-owned Cloudflare resources were removed or already absent; external DNS and unrecorded rules were preserved."
}''')
manage = replace_once(manage, '    if [ -n "$(managed_get \'.cloudflare.zone_id\')" ] || [ -n "$(managed_get \'.cloudflare_previous.zone_id\')" ]; then warn "Cloudflare-managed resources are still recorded. Use Cloudflare cleanup first if you want them removed too."; fi', '    if [ -s "$CF_TXN_STATE" ] || [ -n "$(managed_get \'.cloudflare.zone_id\')" ] || [ -n "$(managed_get \'.cloudflare_previous.zone_id\')" ] || [ -n "$(managed_get \'.cloudflare_previous_certificate.origin_certificate_id\')" ]; then warn "Cloudflare-managed resources or recovery state are still recorded. Use Cloudflare cleanup first if you want them removed too."; fi')
manage = replace_once(manage, 'need_root\n[ -s "$CONFIG_FILE" ] || die "AI Server Agent is not installed or $CONFIG_FILE is missing."', 'if [ "${AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY:-0}" = "1" ]; then return 0 2>/dev/null || exit 0; fi\n\nneed_root\n[ -s "$CONFIG_FILE" ] || die "AI Server Agent is not installed or $CONFIG_FILE is missing."')
manage_path.write_text(manage)

tests_dir = Path("tests")
tests_dir.mkdir(exist_ok=True)
Path("tests/cloudflare_transaction.sh").write_text(r'''#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY=1
source "$ROOT/manage.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
AGENT_USER=root
STATE_DIR="$TMP/state"; CONFIG_DIR="$TMP/config"; TLS_DIR="$CONFIG_DIR/tls"; CONFIG_FILE="$CONFIG_DIR/config.json"; MANAGED_STATE="$CONFIG_DIR/managed.json"; CF_TXN_STATE="$STATE_DIR/cloudflare-transaction.json"; AUTH_HEADER_FILE="$CONFIG_DIR/mcp.authorization"
mkdir -p "$STATE_DIR" "$CONFIG_DIR" "$TLS_DIR"
printf '{"listen_address":"127.0.0.1:3210","tls_cert_file":"","tls_key_file":""}\n' > "$CONFIG_FILE"
printf 'Bearer test\n' > "$AUTH_HEADER_FILE"
printf '{}\n' > "$MANAGED_STATE"
need_cmd(){ :; }
load_cf_token(){ CF_TOKEN=test; }
cf_find_zone(){ printf 'zone1|example.com\n'; }
cf_public_ipv4(){ printf '203.0.113.10\n'; }
restart_and_verify_local(){ return 0; }
verify_public(){ return 0; }
systemctl(){ return 0; }
confirm(){ return 1; }
DELETE_LOG="$TMP/deletes.log"
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
mock_cert(){ local stage="$2"; printf 'key\n' > "$stage/new.key"; printf 'csr\n' > "$stage/new.csr"; printf 'crt\n' > "$stage/new.crt"; CF_RESULT_CERT_ID=cert-new; }
mock_dns_created(){ CF_RESULT_DNS_ID=dns-new; CF_RESULT_DNS_OWNED=true; CF_RESULT_DNS_ACTION=created; CF_RESULT_DNS_OLD_CONTENT=""; CF_RESULT_DNS_OLD_PROXIED=""; CF_RESULT_DNS_OLD_TTL=""; }
mock_origin_fail(){ return 1; }
mock_origin_created(){ CF_RESULT_ORIGIN_RULESET_ID=origin-set; CF_RESULT_ORIGIN_RULE_ID=origin-rule; CF_RESULT_ORIGIN_ACTION=created; }
mock_ssl_created(){ CF_RESULT_SSL_RULESET_ID=ssl-set; CF_RESULT_SSL_RULE_ID=ssl-rule; CF_RESULT_SSL_ACTION=created; }
AI_SERVER_AGENT_HOSTNAME=mcp.example.com
AI_SERVER_AGENT_PORT=3210
cf_issue_origin_cert(){ mock_cert "$@"; }
cf_reconcile_dns(){ mock_dns_created; }
cf_reconcile_origin_rule(){ mock_origin_fail; }
cf_reconcile_ssl_config_rule(){ mock_ssl_created; }
if configure_cloudflare >/dev/null 2>&1; then echo 'expected origin-stage failure' >&2; exit 1; fi
grep -qF '/zones/zone1/dns_records/dns-new' "$DELETE_LOG"
grep -qF '/certificates/cert-new' "$DELETE_LOG"
test ! -e "$CF_TXN_STATE"
: > "$DELETE_LOG"
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; case "$1" in */dns_records/dns-new) return 1 ;; *) return 0 ;; esac; }
if configure_cloudflare >/dev/null 2>&1; then echo 'expected rollback-delete failure' >&2; exit 1; fi
test -s "$CF_TXN_STATE"
test "$(jq -r '.dns.id' "$CF_TXN_STATE")" = dns-new
test "$(jq -r '.dns.action' "$CF_TXN_STATE")" = created
cf_delete_owned(){ printf '%s\n' "$1" >> "$DELETE_LOG"; return 0; }
recover_cloudflare_transaction
test ! -e "$CF_TXN_STATE"
: > "$DELETE_LOG"
printf '{}\n' > "$MANAGED_STATE"
cf_reconcile_origin_rule(){ mock_origin_created; }
cf_reconcile_ssl_config_rule(){ mock_ssl_created; }
save_cloudflare_state(){ return 1; }
if configure_cloudflare >/dev/null 2>&1; then echo 'expected state-write failure' >&2; exit 1; fi
grep -qF '/zones/zone1/dns_records/dns-new' "$DELETE_LOG"
grep -qF '/zones/zone1/rulesets/origin-set/rules/origin-rule' "$DELETE_LOG"
grep -qF '/zones/zone1/rulesets/ssl-set/rules/ssl-rule' "$DELETE_LOG"
grep -qF '/certificates/cert-new' "$DELETE_LOG"
test ! -e "$CF_TXN_STATE"
printf '{"active_provider":"cloudflare","hostname":"mcp.example.com","cloudflare":{"zone_id":"zone1","origin_certificate_id":"cert-new"}}\n' > "$MANAGED_STATE"
save_previous_cloudflare_certificate mcp.example.com cert-old
test "$(jq -r '.cloudflare_previous_certificate.origin_certificate_id' "$MANAGED_STATE")" = cert-old
confirm(){ return 1; }
cleanup_old_cloudflare mcp.example.com zone1 '' false '' '' '' '' cert-old mcp.example.com cert-new zone1 >/dev/null
test "$(jq -r '.cloudflare_previous_certificate.origin_certificate_id' "$MANAGED_STATE")" = cert-old
confirm(){ return 0; }
cf_delete_owned(){ return 1; }
cleanup_old_cloudflare mcp.example.com zone1 '' false '' '' '' '' cert-old mcp.example.com cert-new zone1 >/dev/null
test "$(jq -r '.cloudflare_previous_certificate.origin_certificate_id' "$MANAGED_STATE")" = cert-old
cf_delete_owned(){ return 0; }
cleanup_old_cloudflare mcp.example.com zone1 '' false '' '' '' '' cert-old mcp.example.com cert-new zone1 >/dev/null
test "$(jq -r '.cloudflare_previous_certificate.origin_certificate_id // empty' "$MANAGED_STATE")" = ''
echo 'cloudflare transaction tests passed'
''')
Path("tests/cloudflare_transaction.sh").chmod(0o755)
print("review fixes applied")
