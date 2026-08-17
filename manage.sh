#!/usr/bin/env bash
set -Eeuo pipefail

CONFIG_DIR="/etc/ai-server-agent"
CONFIG_FILE="$CONFIG_DIR/config.json"
STATE_DIR="/var/lib/ai-server-agent"
INSTALL_STATE="$STATE_DIR/install-state.env"
MANAGED_STATE="$CONFIG_DIR/managed.json"
TLS_DIR="$CONFIG_DIR/tls"
AUTH_HEADER_FILE="$CONFIG_DIR/mcp.authorization"
LIB_DIR="/usr/local/lib/ai-server-agent"
UPDATE_HELPER="$LIB_DIR/update.sh"
UNINSTALL_HELPER="$LIB_DIR/uninstall.sh"
AGENT_USER="aiagent"
DEFAULT_PORT=3210
CF_API="https://api.cloudflare.com/client/v4"
CF_TOKEN=""

log(){ printf '[ai-server-agent] %s\n' "$*"; }
warn(){ printf '[ai-server-agent] WARNING: %s\n' "$*" >&2; }
die(){ printf '[ai-server-agent] ERROR: %s\n' "$*" >&2; exit 1; }
need_root(){ [ "$(id -u)" -eq 0 ] || die "Run as root (for example: sudo ai-server-agent-manage)."; }
need_cmd(){ command -v "$1" >/dev/null 2>&1 || die "$1 is required. Run the installer/update repair path first."; }

if [ -t 1 ]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; GREEN=$'\033[32m'; CYAN=$'\033[36m'; YELLOW=$'\033[33m'; RED=$'\033[31m'; RESET=$'\033[0m'
else
  BOLD=""; DIM=""; GREEN=""; CYAN=""; YELLOW=""; RED=""; RESET=""
fi

header(){
  printf '\n%sAI Server Agent%s\n' "$BOLD" "$RESET"
  printf '%sSimple setup and management for your MCP server%s\n\n' "$DIM" "$RESET"
}

pause(){
  if [ -r /dev/tty ]; then read -r -p "Press Enter to continue... " _ </dev/tty || true; fi
}

confirm(){
  local prompt="$1" default="${2:-no}" ans
  if [ ! -r /dev/tty ]; then
    [ "$default" = "yes" ]
    return
  fi
  if [ "$default" = "yes" ]; then
    read -r -p "$prompt [Y/n]: " ans </dev/tty
    case "$ans" in ''|y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
  else
    read -r -p "$prompt [y/N]: " ans </dev/tty
    case "$ans" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
  fi
}

prompt_value(){
  local prompt="$1" default="${2:-}" value
  [ -r /dev/tty ] || die "Interactive input requires a terminal. Use the documented AI_SERVER_AGENT_* variables for automation."
  if [ -n "$default" ]; then
    read -r -p "$prompt [$default]: " value </dev/tty
    printf '%s\n' "${value:-$default}"
  else
    read -r -p "$prompt: " value </dev/tty
    printf '%s\n' "$value"
  fi
}

json_get(){
  local file="$1" expr="$2"
  [ -s "$file" ] || return 0
  jq -r "$expr // empty" "$file" 2>/dev/null || true
}

config_get(){ json_get "$CONFIG_FILE" ".${1}"; }
managed_get(){ json_get "$MANAGED_STATE" "$1"; }

current_port(){
  local listen
  listen="$(config_get listen_address)"
  case "${listen##*:}" in ''|*[!0-9]*) printf '%s\n' "$DEFAULT_PORT" ;; *) printf '%s\n' "${listen##*:}" ;; esac
}

current_mode(){
  local listen
  listen="$(config_get listen_address)"
  case "$listen" in 0.0.0.0:*) printf 'public\n' ;; *) printf 'local\n' ;; esac
}

installed_identity(){
  local CHANNEL="unknown" VERSION="unknown" REF=""
  if [ -r "$INSTALL_STATE" ]; then
    # Root-owned, installer-generated metadata containing only validated channel/version/ref values.
    # shellcheck disable=SC1090
    . "$INSTALL_STATE"
  fi
  printf '%s|%s|%s\n' "${CHANNEL:-unknown}" "${VERSION:-unknown}" "${REF:-}"
}

write_config(){
  local mode="$1" port="$2" cert="$3" key="$4" bind tmp
  [ -s "$CONFIG_FILE" ] || die "Agent config is missing: $CONFIG_FILE"
  [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1024 ] && [ "$port" -le 65535 ] || die "Port must be between 1024 and 65535."
  case "$mode" in local) bind="127.0.0.1:$port" ;; public) bind="0.0.0.0:$port" ;; *) die "Invalid mode: $mode" ;; esac
  if [ "$mode" = "public" ]; then
    [ -r "$cert" ] && [ -r "$key" ] || die "Public mode requires a readable certificate and private key."
  fi
  tmp="$(mktemp)"
  jq --arg listen "$bind" --arg cert "$cert" --arg key "$key" \
    '.listen_address=$listen | .tls_cert_file=$cert | .tls_key_file=$key' "$CONFIG_FILE" > "$tmp"
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$CONFIG_FILE"
  rm -f "$tmp"
}

restart_and_verify_local(){
  local port
  port="$(current_port)"
  systemctl restart ai-server-agent-executor.service ai-server-agent.service
  sleep 1
  systemctl is-active --quiet ai-server-agent-executor.service || return 1
  systemctl is-active --quiet ai-server-agent.service || return 1
  if [ -n "$(config_get tls_cert_file)" ]; then
    curl -kfsS "https://127.0.0.1:$port/healthz" >/dev/null
  else
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null
  fi
}

status(){
  need_cmd jq
  local identity channel version ref mode port cert host provider health service executor endpoint
  identity="$(installed_identity)"; IFS='|' read -r channel version ref <<<"$identity"
  mode="$(current_mode)"; port="$(current_port)"; cert="$(config_get tls_cert_file)"
  host="$(managed_get '.hostname')"; provider="$(managed_get '.active_provider')"
  service="$(systemctl is-active ai-server-agent.service 2>/dev/null || true)"
  executor="$(systemctl is-active ai-server-agent-executor.service 2>/dev/null || true)"
  health="failed"
  if [ -n "$cert" ]; then curl -kfsS "https://127.0.0.1:$port/healthz" >/dev/null 2>&1 && health=ok || true
  else curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && health=ok || true
  fi
  if [ "$mode" = "public" ] && [ -n "$host" ]; then endpoint="https://$host/mcp"; else endpoint="http://127.0.0.1:$port/mcp"; fi
  printf '%sStatus%s\n' "$BOLD" "$RESET"
  printf '  Version:       %s (%s)\n' "$version" "$channel"
  [ -n "$ref" ] && printf '  Ref:           %s\n' "$ref"
  printf '  Agent:         %s\n' "$service"
  printf '  Executor:      %s\n' "$executor"
  printf '  Local health:  %s\n' "$health"
  printf '  Mode:          %s\n' "$mode"
  printf '  Port:          %s\n' "$port"
  [ -n "$provider" ] && printf '  Setup:         %s\n' "$provider"
  [ -n "$host" ] && printf '  Domain:        %s\n' "$host"
  printf '  MCP endpoint:  %s\n' "$endpoint"
}

reveal_auth(){
  [ -s "$AUTH_HEADER_FILE" ] || die "Authorization header file is missing."
  printf '%sThis is a privileged bearer credential. Do not paste it into chat, logs, tickets, or source control.%s\n' "$YELLOW" "$RESET"
  confirm "Reveal the MCP Authorization header in this terminal?" no || { echo "Not shown."; return 0; }
  cat "$AUTH_HEADER_FILE"
}

chatgpt_setup(){
  local mode port host endpoint
  mode="$(current_mode)"; port="$(current_port)"; host="$(managed_get '.hostname')"
  printf '%sChatGPT setup%s\n\n' "$BOLD" "$RESET"
  if [ "$mode" = "public" ] && [ -n "$host" ]; then
    endpoint="https://$host/mcp"
    printf '%sServer-side setup is complete.%s\n' "$GREEN" "$RESET"
    printf 'MCP URL: %s%s%s\n\n' "$BOLD" "$endpoint" "$RESET"
    printf 'In ChatGPT Business on web:\n'
    printf '  1. Enable Developer mode if required by your workspace.\n'
    printf '  2. Open Workspace Settings -> Apps -> Create.\n'
    printf '  3. Enter the MCP URL above and choose the available bearer/auth option.\n'
    printf '  4. Use the protected Authorization value only when ChatGPT asks for it.\n'
    printf '  5. Scan tools, create the app, then test agent_environment and run_command.\n\n'
    printf 'Protected Authorization file: %s\n' "$AUTH_HEADER_FILE"
    if confirm "Reveal the Authorization header now?" no; then reveal_auth; fi
  else
    printf 'The Agent is loopback-only at http://127.0.0.1:%s/mcp.\n' "$port"
    printf 'ChatGPT cannot connect directly to a local/private MCP endpoint. Use Secure MCP Tunnel,\n'
    printf 'or choose the Cloudflare domain setup from this menu.\n'
  fi
}

validate_hostname(){
  local host="$1"
  [[ "$host" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "Invalid hostname: $host"
  [[ "$host" == *.* ]] || die "Use a fully qualified hostname such as mcp.example.com."
}

load_cf_token(){
  if [ -n "${AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE:-}" ]; then
    [ -r "$AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE" ] || die "Cloudflare token file is not readable."
    CF_TOKEN="$(tr -d '\r\n' < "$AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE")"
  elif [ -n "${AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN:-}" ]; then
    CF_TOKEN="$AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN"
  else
    [ -r /dev/tty ] || die "Set AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE for noninteractive Cloudflare setup."
    printf 'Cloudflare API token (input hidden; not stored): ' >/dev/tty
    read -r -s CF_TOKEN </dev/tty
    printf '\n' >/dev/tty
  fi
  [ -n "$CF_TOKEN" ] || die "Cloudflare API token is empty."
}

cf_api(){
  local method="$1" path="$2" body="${3:-}" cfg out
  cfg="$(mktemp)"; chmod 0600 "$cfg"
  printf 'header = "Authorization: Bearer %s"\n' "$CF_TOKEN" > "$cfg"
  if [ -n "$body" ]; then
    out="$(curl -sS --fail-with-body --retry 2 --request "$method" --config "$cfg" -H 'Content-Type: application/json' --data-binary "$body" "$CF_API$path")" || { rm -f "$cfg"; return 1; }
  else
    out="$(curl -sS --fail-with-body --retry 2 --request "$method" --config "$cfg" -H 'Content-Type: application/json' "$CF_API$path")" || { rm -f "$cfg"; return 1; }
  fi
  rm -f "$cfg"
  if ! jq -e '.success == true' >/dev/null 2>&1 <<<"$out"; then
    jq -r '.errors[]?.message // empty' <<<"$out" >&2 || true
    return 1
  fi
  printf '%s' "$out"
}

cf_find_zone(){
  local host="$1" candidate res id
  candidate="$host"
  while [[ "$candidate" == *.* ]]; do
    res="$(cf_api GET "/zones?name=$candidate&status=active&per_page=1")" || die "Cloudflare zone lookup failed. Check token access."
    id="$(jq -r '.result[0].id // empty' <<<"$res")"
    if [ -n "$id" ]; then printf '%s|%s\n' "$id" "$candidate"; return 0; fi
    candidate="${candidate#*.}"
  done
  die "No active Cloudflare zone was found for $host using this token."
}

cf_public_ipv4(){
  local ip
  ip="${AI_SERVER_AGENT_PUBLIC_IPV4:-}"
  if [ -z "$ip" ]; then ip="$(curl -4fsS https://www.cloudflare.com/cdn-cgi/trace 2>/dev/null | sed -n 's/^ip=//p' | head -n1 || true)"; fi
  if [[ ! "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] && [ -r /dev/tty ]; then ip="$(prompt_value 'Public IPv4 address for this server')"; fi
  [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "Could not determine a public IPv4 address. Set AI_SERVER_AGENT_PUBLIC_IPV4."
  printf '%s\n' "$ip"
}

cf_require_strict_ssl(){
  local zone_id="$1" res value
  res="$(cf_api GET "/zones/$zone_id/settings/ssl")" || die "Could not read Cloudflare SSL mode."
  value="$(jq -r '.result.value // empty' <<<"$res")"
  [ "$value" = "strict" ] && return 0
  warn "Cloudflare SSL mode for this zone is '$value'. The managed Origin CA path requires Full (strict)."
  if { [ -r /dev/tty ] && confirm "Change the entire Cloudflare zone SSL mode to Full (strict)? This can affect other origins in the zone." no; } || [ "${AI_SERVER_AGENT_ALLOW_ZONE_SSL_CHANGE:-0}" = "1" ]; then
    cf_api PATCH "/zones/$zone_id/settings/ssl" '{"value":"strict"}' >/dev/null || die "Could not change Cloudflare SSL mode. Change it to Full (strict) and rerun setup."
    log "Cloudflare SSL mode changed to Full (strict)."
  else
    die "No zone-wide SSL setting was changed. Set Full (strict) in Cloudflare or explicitly allow the change and rerun."
  fi
}

cf_issue_origin_cert(){
  local host="$1" stage="$2" key="$stage/new.key" csr="$stage/new.csr" crt="$stage/new.crt" body res cert_id key_pub cert_pub
  umask 077
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$key" >/dev/null 2>&1
  openssl req -new -sha256 -key "$key" -out "$csr" -subj "/CN=$host" -addext "subjectAltName=DNS:$host"
  body="$(jq -n --rawfile csr "$csr" --arg host "$host" '{hostnames:[$host],request_type:"origin-rsa",requested_validity:1095,csr:$csr}')"
  res="$(cf_api POST '/certificates' "$body")" || die "Cloudflare Origin CA certificate issuance failed. Check token access and hostname ownership."
  jq -r '.result.certificate // empty' <<<"$res" > "$crt"
  cert_id="$(jq -r '.result.id // empty' <<<"$res")"
  [ -s "$crt" ] && [ -n "$cert_id" ] || die "Cloudflare returned an incomplete Origin CA certificate response."
  openssl pkey -in "$key" -check -noout >/dev/null 2>&1 || die "Generated private key failed validation."
  openssl x509 -in "$crt" -noout -checkhost "$host" >/dev/null || die "Issued certificate does not match $host."
  key_pub="$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  cert_pub="$(openssl x509 -in "$crt" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  [ "$key_pub" = "$cert_pub" ] || die "Issued certificate does not match the generated private key."
  printf '%s\n' "$cert_id"
}

cf_reconcile_dns(){
  local zone_id="$1" host="$2" ip="$3" res count id type content proxied comment body
  res="$(cf_api GET "/zones/$zone_id/dns_records?name=$host&per_page=100")" || die "Cloudflare DNS lookup failed."
  count="$(jq '.result | length' <<<"$res")"
  if [ "$count" -eq 0 ]; then
    body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
    res="$(cf_api POST "/zones/$zone_id/dns_records" "$body")" || die "Could not create Cloudflare DNS record for $host."
    printf '%s|created\n' "$(jq -r '.result.id' <<<"$res")"
    return
  fi
  [ "$count" -eq 1 ] || die "Multiple DNS records already exist for $host. Refusing ambiguous replacement."
  id="$(jq -r '.result[0].id' <<<"$res")"; type="$(jq -r '.result[0].type' <<<"$res")"; content="$(jq -r '.result[0].content' <<<"$res")"; proxied="$(jq -r '.result[0].proxied' <<<"$res")"; comment="$(jq -r '.result[0].comment // ""' <<<"$res")"
  [ "$type" = "A" ] || die "$host already has a $type record. Use another hostname or resolve the DNS conflict manually."
  if [ "$content" = "$ip" ] && [ "$proxied" = "true" ]; then printf '%s|existing\n' "$id"; return; fi
  if [ "$comment" != "Managed by AI Server Agent" ]; then
    warn "Existing A record for $host points to $content (proxied=$proxied) and is not marked Agent-managed."
    if [ -r /dev/tty ]; then confirm "Replace this hostname-scoped DNS record with $ip and enable proxy?" no || die "DNS record left unchanged."
    else [ "${AI_SERVER_AGENT_REPLACE_EXISTING_DNS:-0}" = "1" ] || die "Refusing to replace existing DNS in noninteractive mode."; fi
  fi
  body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
  cf_api PATCH "/zones/$zone_id/dns_records/$id" "$body" >/dev/null || die "Could not update Cloudflare DNS record for $host."
  printf '%s|updated\n' "$id"
}

cf_reconcile_origin_rule(){
  local zone_id="$1" host="$2" port="$3" list ruleset_id rule_ref rule_id rule_body create_body ruleset
  rule_ref="ai_server_agent_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" '{ref:$ref,description:"AI Server Agent origin port",expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_request_origin") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    create_body="$(jq -n --argjson rule "$rule_body" '{name:"AI Server Agent Origin Rules",description:"Hostname-scoped origin routing managed by AI Server Agent",kind:"zone",phase:"http_request_origin",rules:[$rule]}')"
    ruleset="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Could not create Cloudflare Origin Rules ruleset."
    ruleset_id="$(jq -r '.result.id // empty' <<<"$ruleset")"
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Origin Rules ruleset."
    rule_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$rule_id" ]; then
      cf_api PATCH "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id" "$rule_body" >/dev/null || die "Could not update Agent Origin Rule."
    else
      cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body" >/dev/null || die "Could not create Agent Origin Rule."
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare Origin Rule."
  rule_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
  [ -n "$ruleset_id" ] && [ -n "$rule_id" ] || die "Cloudflare Origin Rule was not reconciled cleanly."
  printf '%s|%s\n' "$ruleset_id" "$rule_id"
}

save_cloudflare_state(){
  local host="$1" port="$2" zone_id="$3" zone_name="$4" dns_id="$5" ruleset_id="$6" rule_id="$7" cert_id="$8" tmp old
  tmp="$(mktemp)"
  if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --argjson port "$port" --arg zone_id "$zone_id" --arg zone_name "$zone_name" --arg dns_id "$dns_id" --arg ruleset_id "$ruleset_id" --arg rule_id "$rule_id" --arg cert_id "$cert_id" \
    '.active_provider="cloudflare" | .hostname=$host | .port=$port | .cloudflare={zone_id:$zone_id,zone_name:$zone_name,dns_record_id:$dns_id,origin_ruleset_id:$ruleset_id,origin_rule_id:$rule_id,origin_certificate_id:$cert_id}' <<<"$old" > "$tmp"
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
}

save_local_state(){
  local port="$1" tmp old
  tmp="$(mktemp)"; if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --argjson port "$port" '.active_provider="local" | .port=$port' <<<"$old" > "$tmp"
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
}

save_manual_state(){
  local host="$1" port="$2" tmp old
  tmp="$(mktemp)"; if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --argjson port "$port" '.active_provider="manual" | .hostname=$host | .port=$port' <<<"$old" > "$tmp"
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
}

verify_public(){
  local host="$1" auth payload unauth_code auth_code out i ok=1
  payload='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"ai-server-agent-manager","version":"1"}}}'
  for i in $(seq 1 20); do
    if curl -fsS --connect-timeout 5 "https://$host/healthz" >/dev/null 2>&1; then ok=0; break; fi
    sleep 3
  done
  [ "$ok" -eq 0 ] || return 1
  unauth_code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' --data "$payload" "https://$host/mcp" || true)"
  [ "$unauth_code" = "401" ] || return 1
  auth="$(<"$AUTH_HEADER_FILE")"; out="$(mktemp)"
  auth_code="$(printf 'header = "Authorization: %s"\n' "$auth" | curl --config - -sS -o "$out" -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' --data "$payload" "https://$host/mcp" || true)"
  unset auth
  if [ "$auth_code" = "200" ] && grep -q '"name":"ai-server-agent"' "$out"; then ok=0; else ok=1; fi
  rm -f "$out"
  return "$ok"
}

restore_tls_backup(){
  local backup="$1"
  [ -e "$backup/origin.key" ] && install -o root -g "$AGENT_USER" -m 0640 "$backup/origin.key" "$TLS_DIR/origin.key" || rm -f "$TLS_DIR/origin.key"
  [ -e "$backup/origin.csr" ] && install -o root -g root -m 0644 "$backup/origin.csr" "$TLS_DIR/origin.csr" || rm -f "$TLS_DIR/origin.csr"
  [ -e "$backup/origin.crt" ] && install -o root -g "$AGENT_USER" -m 0644 "$backup/origin.crt" "$TLS_DIR/origin.crt" || rm -f "$TLS_DIR/origin.crt"
}

cleanup_old_cloudflare(){
  local old_host="$1" old_zone="$2" old_dns="$3" old_ruleset="$4" old_rule="$5" old_cert="$6" new_host="$7" new_cert="$8"
  [ -n "$old_host" ] || return 0
  if [ "$old_host" = "$new_host" ]; then
    if [ -n "$old_cert" ] && [ "$old_cert" != "$new_cert" ] && confirm "Revoke the previous Cloudflare Origin CA certificate now that the new certificate is verified?" yes; then
      cf_api DELETE "/certificates/$old_cert" >/dev/null || warn "Previous Origin CA certificate could not be revoked automatically."
    fi
    return 0
  fi
  printf '\nOld managed hostname: %s\n' "$old_host"
  confirm "Remove the old Agent-managed Cloudflare DNS/rule/certificate resources now?" yes || return 0
  [ -n "$old_dns" ] && cf_api DELETE "/zones/$old_zone/dns_records/$old_dns" >/dev/null || true
  [ -n "$old_rule" ] && [ -n "$old_ruleset" ] && cf_api DELETE "/zones/$old_zone/rulesets/$old_ruleset/rules/$old_rule" >/dev/null || true
  [ -n "$old_cert" ] && cf_api DELETE "/certificates/$old_cert" >/dev/null || true
  log "Old Agent-managed Cloudflare resources cleanup attempted using recorded IDs."
}

configure_cloudflare(){
  need_cmd jq; need_cmd openssl; need_cmd curl; need_cmd sha256sum
  local old_host old_zone old_dns old_ruleset old_rule old_cert host port zone_pair zone_id zone_name ip stage backup cert_id dns_pair dns_id rule_pair ruleset_id rule_id config_backup
  old_host="$(managed_get '.hostname')"; old_zone="$(managed_get '.cloudflare.zone_id')"; old_dns="$(managed_get '.cloudflare.dns_record_id')"; old_ruleset="$(managed_get '.cloudflare.origin_ruleset_id')"; old_rule="$(managed_get '.cloudflare.origin_rule_id')"; old_cert="$(managed_get '.cloudflare.origin_certificate_id')"
  host="${AI_SERVER_AGENT_HOSTNAME:-}"; [ -n "$host" ] || host="$(prompt_value 'Public MCP hostname' "${old_host:-mcp.example.com}")"; host="${host,,}"; validate_hostname "$host"
  port="${AI_SERVER_AGENT_PORT:-$(current_port)}"; [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1024 ] && [ "$port" -le 65535 ] || die "Port must be between 1024 and 65535."
  printf '\nCloudflare will manage only this MCP hostname, its origin-port rule, and an Origin CA certificate.\n'
  printf 'The API token is read securely and is not stored after this command exits.\n'
  load_cf_token
  zone_pair="$(cf_find_zone "$host")"; IFS='|' read -r zone_id zone_name <<<"$zone_pair"; log "Cloudflare zone: $zone_name"
  cf_require_strict_ssl "$zone_id"
  ip="$(cf_public_ipv4)"; log "Origin IPv4: $ip"
  stage="$(mktemp -d)"; backup="$(mktemp -d)"; chmod 0700 "$stage" "$backup"
  [ -e "$TLS_DIR/origin.key" ] && cp -p "$TLS_DIR/origin.key" "$backup/origin.key" || true
  [ -e "$TLS_DIR/origin.csr" ] && cp -p "$TLS_DIR/origin.csr" "$backup/origin.csr" || true
  [ -e "$TLS_DIR/origin.crt" ] && cp -p "$TLS_DIR/origin.crt" "$backup/origin.crt" || true
  cert_id="$(cf_issue_origin_cert "$host" "$stage")"; log "Fresh Origin CA certificate issued and verified."
  dns_pair="$(cf_reconcile_dns "$zone_id" "$host" "$ip")"; IFS='|' read -r dns_id _ <<<"$dns_pair"; log "Cloudflare proxied DNS reconciled."
  rule_pair="$(cf_reconcile_origin_rule "$zone_id" "$host" "$port")"; IFS='|' read -r ruleset_id rule_id <<<"$rule_pair"; log "Cloudflare origin port rule reconciled for port $port."
  install -d -o root -g "$AGENT_USER" -m 0750 "$TLS_DIR"
  install -o root -g "$AGENT_USER" -m 0640 "$stage/new.key" "$TLS_DIR/origin.key"
  install -o root -g root -m 0644 "$stage/new.csr" "$TLS_DIR/origin.csr"
  install -o root -g "$AGENT_USER" -m 0644 "$stage/new.crt" "$TLS_DIR/origin.crt"
  config_backup="$(mktemp)"; cp -a "$CONFIG_FILE" "$config_backup"
  write_config public "$port" "$TLS_DIR/origin.crt" "$TLS_DIR/origin.key"
  if ! restart_and_verify_local; then
    install -o root -g "$AGENT_USER" -m 0640 "$config_backup" "$CONFIG_FILE"; restore_tls_backup "$backup"; systemctl restart ai-server-agent-executor.service ai-server-agent.service || true
    cf_api DELETE "/certificates/$cert_id" >/dev/null || true
    rm -rf "$stage" "$backup"; rm -f "$config_backup"; CF_TOKEN=""
    die "Agent failed after TLS/public reconfiguration; previous local Agent state was restored."
  fi
  if ! verify_public "$host"; then
    install -o root -g "$AGENT_USER" -m 0640 "$config_backup" "$CONFIG_FILE"; restore_tls_backup "$backup"; systemctl restart ai-server-agent-executor.service ai-server-agent.service || true
    cf_api DELETE "/certificates/$cert_id" >/dev/null || true
    rm -rf "$stage" "$backup"; rm -f "$config_backup"; CF_TOKEN=""
    die "Public Cloudflare verification failed. Previous Agent state was restored; hostname-scoped DNS/rule changes remain for an idempotent retry."
  fi
  rm -rf "$stage" "$backup"; rm -f "$config_backup"
  save_cloudflare_state "$host" "$port" "$zone_id" "$zone_name" "$dns_id" "$ruleset_id" "$rule_id" "$cert_id"
  log "Public HTTPS health, unauthenticated rejection, and authenticated MCP initialize all passed."
  cleanup_old_cloudflare "$old_host" "$old_zone" "$old_dns" "$old_ruleset" "$old_rule" "$old_cert" "$host" "$cert_id"
  CF_TOKEN=""
  printf '\n%sServer setup complete.%s\n' "$GREEN" "$RESET"
  printf 'MCP URL: %shttps://%s/mcp%s\n' "$BOLD" "$host" "$RESET"
  printf 'Next: choose ChatGPT Setup from the menu.\n'
}

configure_local(){
  local port
  port="${AI_SERVER_AGENT_PORT:-$(current_port)}"; [ -r /dev/tty ] && port="$(prompt_value 'Local MCP port' "$port")"
  write_config local "$port" "" ""
  restart_and_verify_local || die "Agent did not recover in local mode."
  save_local_state "$port"
  log "Agent is now loopback-only. Existing TLS files and Cloudflare metadata were not deleted."
}

configure_manual_tls(){
  need_cmd openssl; need_cmd jq
  local host port src_crt src_key key_pub cert_pub config_backup backup
  host="${AI_SERVER_AGENT_HOSTNAME:-}"; [ -n "$host" ] || host="$(prompt_value 'Public MCP hostname')"; host="${host,,}"; validate_hostname "$host"
  port="${AI_SERVER_AGENT_PORT:-$(current_port)}"; [ -r /dev/tty ] && port="$(prompt_value 'Public MCP port' "$port")"
  src_crt="${AI_SERVER_AGENT_TLS_CERT_FILE:-}"; [ -n "$src_crt" ] || src_crt="$(prompt_value 'Existing TLS certificate PEM path')"
  src_key="${AI_SERVER_AGENT_TLS_KEY_FILE:-}"; [ -n "$src_key" ] || src_key="$(prompt_value 'Existing TLS private key PEM path')"
  [ -r "$src_crt" ] && [ -r "$src_key" ] || die "Certificate/key path is not readable."
  openssl x509 -in "$src_crt" -noout -checkhost "$host" >/dev/null || die "Certificate does not match $host."
  key_pub="$(openssl pkey -in "$src_key" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"; cert_pub="$(openssl x509 -in "$src_crt" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"; [ "$key_pub" = "$cert_pub" ] || die "Certificate and private key do not match."
  backup="$(mktemp -d)"; [ -e "$TLS_DIR/origin.key" ] && cp -p "$TLS_DIR/origin.key" "$backup/origin.key" || true; [ -e "$TLS_DIR/origin.crt" ] && cp -p "$TLS_DIR/origin.crt" "$backup/origin.crt" || true
  install -d -o root -g "$AGENT_USER" -m 0750 "$TLS_DIR"; install -o root -g "$AGENT_USER" -m 0640 "$src_key" "$TLS_DIR/origin.key"; install -o root -g "$AGENT_USER" -m 0644 "$src_crt" "$TLS_DIR/origin.crt"
  config_backup="$(mktemp)"; cp -a "$CONFIG_FILE" "$config_backup"; write_config public "$port" "$TLS_DIR/origin.crt" "$TLS_DIR/origin.key"
  if ! restart_and_verify_local; then install -o root -g "$AGENT_USER" -m 0640 "$config_backup" "$CONFIG_FILE"; restore_tls_backup "$backup"; systemctl restart ai-server-agent-executor.service ai-server-agent.service || true; rm -rf "$backup"; rm -f "$config_backup"; die "Manual TLS configuration failed; previous state restored."; fi
  rm -rf "$backup"; rm -f "$config_backup"; save_manual_state "$host" "$port"
  log "Manual TLS/public mode configured. DNS/edge routing remains your responsibility."
}

cloudflare_cleanup(){
  local zone_id dns_id ruleset_id rule_id cert_id host tmp old
  zone_id="$(managed_get '.cloudflare.zone_id')"; dns_id="$(managed_get '.cloudflare.dns_record_id')"; ruleset_id="$(managed_get '.cloudflare.origin_ruleset_id')"; rule_id="$(managed_get '.cloudflare.origin_rule_id')"; cert_id="$(managed_get '.cloudflare.origin_certificate_id')"; host="$(managed_get '.hostname')"
  [ -n "$zone_id" ] || { log "No recorded Cloudflare-managed resources."; return 0; }
  if [ "$(current_mode)" = "public" ] && [ "$(managed_get '.active_provider')" = "cloudflare" ]; then
    die "This Cloudflare hostname is currently carrying the MCP connection. Switch to local/manual mode before cleanup."
  fi
  printf 'Recorded Agent-managed Cloudflare hostname: %s\n' "$host"
  confirm "Delete the recorded DNS record, Origin Rule, and Origin CA certificate?" no || { echo "Cancelled."; return 0; }
  load_cf_token
  [ -n "$dns_id" ] && cf_api DELETE "/zones/$zone_id/dns_records/$dns_id" >/dev/null || true
  [ -n "$rule_id" ] && [ -n "$ruleset_id" ] && cf_api DELETE "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id" >/dev/null || true
  [ -n "$cert_id" ] && cf_api DELETE "/certificates/$cert_id" >/dev/null || true
  CF_TOKEN=""
  tmp="$(mktemp)"; old="$(cat "$MANAGED_STATE")"; jq '.cloudflare={}' <<<"$old" > "$tmp"; install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
  log "Recorded Cloudflare resources cleanup attempted."
}

update_agent(){ [ -x "$UPDATE_HELPER" ] || die "Update helper is missing: $UPDATE_HELPER"; "$UPDATE_HELPER"; }

repair(){
  printf 'Repair restarts and validates the services. If local health still fails, it runs the channel-aware updater.\n'
  if restart_and_verify_local; then log "Services and local health are healthy."; else warn "Local health failed; attempting channel-aware reinstall/update."; update_agent; fi
}

run_uninstall(){
  local purge="${1:-0}"
  [ -x "$UNINSTALL_HELPER" ] || die "Uninstall helper is missing: $UNINSTALL_HELPER"
  if [ "$purge" = "1" ]; then
    printf '%sPURGE removes Agent-owned config/state/log/runtime and the aiagent identity.%s\n' "$RED" "$RESET"
    printf 'It still preserves /srv/ai-workspace and the aiworker identity.\n'
    if [ -n "$(managed_get '.cloudflare.zone_id')" ]; then warn "Cloudflare-managed resources are still recorded. Use Cloudflare cleanup first if you want them removed too."; fi
    confirm "Purge Agent-owned server data?" no || { echo "Cancelled."; return 0; }
    AI_SERVER_AGENT_YES=1 "$UNINSTALL_HELPER" --purge
  else
    printf 'Safe uninstall removes services, binary, and management command while preserving config/state/users/workspace.\n'
    confirm "Uninstall AI Server Agent?" no || { echo "Cancelled."; return 0; }
    AI_SERVER_AGENT_YES=1 "$UNINSTALL_HELPER"
  fi
}

connection_menu(){
  while true; do
    header
    printf 'Connection setup\n'
    printf '  1) Cloudflare domain (guided, recommended for direct HTTPS)\n'
    printf '  2) Local/private mode\n'
    printf '  3) Existing certificate (advanced)\n'
    printf '  0) Back\n\n'
    read -r -p 'Choose: ' choice </dev/tty
    case "$choice" in
      1) configure_cloudflare; pause; return ;;
      2) configure_local; pause; return ;;
      3) configure_manual_tls; pause; return ;;
      0) return ;;
      *) echo "Invalid choice."; pause ;;
    esac
  done
}

first_run(){
  header
  printf 'The Agent core is installed and healthy. Choose how ChatGPT will reach it.\n\n'
  printf '  1) %sCloudflare domain%s - guided HTTPS/domain setup\n' "$GREEN" "$RESET"
  printf '  2) Local/private - keep loopback-only for Secure MCP Tunnel\n'
  printf '  3) Existing certificate - advanced/manual TLS\n\n'
  local choice
  read -r -p 'Choose [1]: ' choice </dev/tty; choice="${choice:-1}"
  case "$choice" in 1) configure_cloudflare ;; 2) configure_local ;; 3) configure_manual_tls ;; *) die "Invalid choice." ;; esac
  printf '\n%sDone.%s Use %ssudo ai-server-agent-manage%s any time to change settings.\n\n' "$GREEN" "$RESET" "$BOLD" "$RESET"
  chatgpt_setup
}

menu(){
  [ -r /dev/tty ] || die "The interactive menu requires a terminal. Run a subcommand for automation."
  while true; do
    header
    printf '  %s1)%s Status / health\n' "$CYAN" "$RESET"
    printf '  %s2)%s ChatGPT setup\n' "$CYAN" "$RESET"
    printf '  %s3)%s Configure or change domain / connection\n' "$CYAN" "$RESET"
    printf '  %s4)%s Rotate / renew Cloudflare TLS certificate\n' "$CYAN" "$RESET"
    printf '  %s5)%s Update Agent\n' "$CYAN" "$RESET"
    printf '  %s6)%s Repair / restart\n' "$CYAN" "$RESET"
    printf '  %s7)%s Safe uninstall (preserve data)\n' "$YELLOW" "$RESET"
    printf '  %s8)%s Purge Agent-owned server data\n' "$RED" "$RESET"
    printf '  %s9)%s Remove recorded Cloudflare resources\n' "$YELLOW" "$RESET"
    printf '  0) Exit\n\n'
    read -r -p 'Choose: ' choice </dev/tty
    case "$choice" in
      1) status; pause ;;
      2) chatgpt_setup; pause ;;
      3) connection_menu ;;
      4) [ "$(managed_get '.cloudflare.zone_id')" != "" ] || { warn "No Cloudflare-managed setup is recorded. Use option 3 first."; pause; continue; }; configure_cloudflare; pause ;;
      5) update_agent; pause ;;
      6) repair; pause ;;
      7) run_uninstall 0; return ;;
      8) run_uninstall 1; return ;;
      9) cloudflare_cleanup; pause ;;
      0) return ;;
      *) echo "Invalid choice."; pause ;;
    esac
  done
}

need_root
[ -s "$CONFIG_FILE" ] || die "AI Server Agent is not installed or $CONFIG_FILE is missing."

case "${1:-menu}" in
  menu) menu ;;
  first-run) first_run ;;
  status) status ;;
  chatgpt|chatgpt-setup) chatgpt_setup ;;
  reveal-auth) reveal_auth ;;
  setup|configure) connection_menu ;;
  configure-cloudflare) configure_cloudflare ;;
  configure-local) configure_local ;;
  configure-manual-tls) configure_manual_tls ;;
  cloudflare-cleanup) cloudflare_cleanup ;;
  update) update_agent ;;
  repair) repair ;;
  uninstall) run_uninstall 0 ;;
  purge) run_uninstall 1 ;;
  *) die "Unknown command: ${1:-}. Use menu, status, chatgpt-setup, configure-cloudflare, configure-local, configure-manual-tls, cloudflare-cleanup, update, repair, uninstall, or purge." ;;
esac
