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

print_cf_token_guidance(){
  local host="$1"
  cat <<EOF_GUIDANCE

Cloudflare API token requirements for $host:
  Resource scope: Include -> Specific zone -> the zone containing this hostname
  Permissions:
    Zone > Zone > Read
    Zone > DNS > Edit
    Zone > SSL and Certificates > Edit
    Zone > Origin Rules > Edit
    Zone > Config Rules > Edit

Create or edit the token in Cloudflare, then enter it only in the hidden terminal prompt below.
Do not paste the token into ChatGPT, chat, logs, tickets, screenshots, or source control.
AI Server Agent uses the token only for this command and does not store it.
EOF_GUIDANCE
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

cf_delete_owned(){
  local path="$1" cfg response status body
  cfg="$(mktemp)"; chmod 0600 "$cfg"
  printf 'header = "Authorization: Bearer %s"\n' "$CF_TOKEN" > "$cfg"
  response="$(curl -sS --retry 2 --request DELETE --config "$cfg" -H 'Content-Type: application/json' -w '\n%{http_code}' "$CF_API$path")" || { rm -f "$cfg"; return 1; }
  rm -f "$cfg"
  status="${response##*$'\n'}"; body="${response%$'\n'*}"
  [ "$status" = "404" ] && return 0
  [[ "$status" =~ ^2[0-9][0-9]$ ]] || { jq -r '.errors[]?.message // empty' <<<"$body" >&2 || true; return 1; }
  jq -e '.success == true' >/dev/null 2>&1 <<<"$body" || { jq -r '.errors[]?.message // empty' <<<"$body" >&2 || true; return 1; }
}

delete_recorded_cloudflare_resources(){
  local zone_id="$1" dns_id="$2" dns_owned="$3" origin_ruleset="$4" origin_rule="$5" ssl_ruleset="$6" ssl_rule="$7" cert_id="$8" failed=0
  if [ "$dns_owned" = "true" ] && [ -n "$dns_id" ] && ! cf_delete_owned "/zones/$zone_id/dns_records/$dns_id"; then warn "Could not delete recorded Agent-owned DNS record $dns_id."; failed=1; fi
  if [ -n "$origin_rule" ] && [ -n "$origin_ruleset" ] && ! cf_delete_owned "/zones/$zone_id/rulesets/$origin_ruleset/rules/$origin_rule"; then warn "Could not delete recorded Agent-owned Origin Rule $origin_rule."; failed=1; fi
  if [ -n "$ssl_rule" ] && [ -n "$ssl_ruleset" ] && ! cf_delete_owned "/zones/$zone_id/rulesets/$ssl_ruleset/rules/$ssl_rule"; then warn "Could not delete recorded Agent-owned Configuration Rule $ssl_rule."; failed=1; fi
  if [ -n "$cert_id" ] && ! cf_delete_owned "/certificates/$cert_id"; then warn "Could not revoke recorded Origin CA certificate $cert_id."; failed=1; fi
  [ "$failed" -eq 0 ]
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
  local zone_id="$1" host="$2" ip="$3" owned_dns_id="${4:-}" res count id type content proxied comment ttl body owned=false
  res="$(cf_api GET "/zones/$zone_id/dns_records?name=$host&per_page=100")" || die "Cloudflare DNS lookup failed."
  count="$(jq '.result | length' <<<"$res")"
  if [ "$count" -eq 0 ]; then
    body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
    res="$(cf_api POST "/zones/$zone_id/dns_records" "$body")" || die "Could not create Cloudflare DNS record for $host."
    printf '%s|true|created|||\n' "$(jq -r '.result.id' <<<"$res")"
    return
  fi
  [ "$count" -eq 1 ] || die "Multiple DNS records already exist for $host. Refusing ambiguous replacement."
  id="$(jq -r '.result[0].id' <<<"$res")"; type="$(jq -r '.result[0].type' <<<"$res")"; content="$(jq -r '.result[0].content' <<<"$res")"; proxied="$(jq -r '.result[0].proxied' <<<"$res")"; comment="$(jq -r '.result[0].comment // ""' <<<"$res")"; ttl="$(jq -r '.result[0].ttl' <<<"$res")"
  [ "$type" = "A" ] || die "$host already has a $type record. Use another hostname or resolve the DNS conflict manually."
  if [ -n "$owned_dns_id" ]; then
    [ "$id" = "$owned_dns_id" ] || die "The DNS record ID for $host no longer matches recorded Agent ownership. Refusing to adopt or modify the replacement record."
    owned=true
  fi
  if [ "$content" = "$ip" ] && [ "$proxied" = "true" ]; then
    if [ "$owned" = "true" ]; then printf '%s|true|existing-managed|||\n' "$id"; else printf '%s|false|existing-external|||\n' "$id"; fi
    return
  fi
  if [ "$owned" != "true" ]; then
    die "Existing A record for $host is not recorded as Agent-owned and does not match this server. Refusing to modify or adopt it automatically. Use another hostname or update/remove that record manually, then rerun setup."
  fi
  body="$(jq -n --arg name "$host" --arg ip "$ip" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:"Managed by AI Server Agent"}')"
  cf_api PATCH "/zones/$zone_id/dns_records/$id" "$body" >/dev/null || die "Could not update Cloudflare DNS record for $host."
  printf '%s|true|updated|%s|%s|%s\n' "$id" "$content" "$proxied" "$ttl"
}

cf_reconcile_origin_rule(){
  local zone_id="$1" host="$2" port="$3" owned_ruleset="${4:-}" owned_rule="${5:-}"
  local list ruleset_id rule_ref rule_id ref_match rule_body create_body ruleset action
  rule_ref="ai_server_agent_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" '{ref:$ref,description:"AI Server Agent origin port",expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_request_origin") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    create_body="$(jq -n --argjson rule "$rule_body" '{name:"AI Server Agent Origin Rules",description:"Hostname-scoped origin routing managed by AI Server Agent",kind:"zone",phase:"http_request_origin",rules:[$rule]}')"
    ruleset="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Could not create Cloudflare Origin Rules ruleset."
    ruleset_id="$(jq -r '.result.id // empty' <<<"$ruleset")"; action=created
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Origin Rules ruleset."
    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then
      rule_id="$(jq -r --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref) | .id' <<<"$ruleset" | head -n1)"
      if [ -n "$rule_id" ]; then
        cf_api PATCH "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id" "$rule_body" >/dev/null || die "Could not update Agent-owned Origin Rule."
        action=updated
      elif [ -n "$ref_match" ]; then
        die "An Origin Rule uses the Agent ref but is not the rule recorded as Agent-owned. Refusing to adopt or overwrite it."
      else
        cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body" >/dev/null || die "Could not recreate Agent Origin Rule."
        action=recreated
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Origin Rule already uses the Agent ref. Refusing to adopt or overwrite it."
      cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body" >/dev/null || die "Could not create Agent Origin Rule."
      action=created
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare Origin Rule."
  rule_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
  [ -n "$ruleset_id" ] && [ -n "$rule_id" ] || die "Cloudflare Origin Rule was not reconciled cleanly."
  printf '%s|%s|%s\n' "$ruleset_id" "$rule_id" "$action"
}

cf_reconcile_ssl_config_rule(){
  local zone_id="$1" host="$2" owned_ruleset="${3:-}" owned_rule="${4:-}"
  local list ruleset_id rule_ref rule_id ref_match rule_body create_body ruleset action
  rule_ref="ai_server_agent_ssl_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" '{ref:$ref,description:"AI Server Agent strict SSL",expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_config_settings") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    create_body="$(jq -n --argjson rule "$rule_body" '{name:"AI Server Agent Configuration Rules",description:"Hostname-scoped configuration managed by AI Server Agent",kind:"zone",phase:"http_config_settings",rules:[$rule]}')"
    ruleset="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Could not create Cloudflare Configuration Rules ruleset. Check Config Rules Edit permission."
    ruleset_id="$(jq -r '.result.id // empty' <<<"$ruleset")"; action=created
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Configuration Rules ruleset."
    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then
      rule_id="$(jq -r --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref) | .id' <<<"$ruleset" | head -n1)"
      if [ -n "$rule_id" ]; then
        cf_api PATCH "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id" "$rule_body" >/dev/null || die "Could not update Agent-owned strict SSL Configuration Rule."
        action=updated
      elif [ -n "$ref_match" ]; then
        die "A Configuration Rule uses the Agent SSL ref but is not the rule recorded as Agent-owned. Refusing to adopt or overwrite it."
      else
        cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body" >/dev/null || die "Could not recreate Agent strict SSL Configuration Rule."
        action=recreated
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Configuration Rule already uses the Agent SSL ref. Refusing to adopt or overwrite it."
      cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$rule_body" >/dev/null || die "Could not create Agent strict SSL Configuration Rule."
      action=created
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare strict SSL Configuration Rule."
  rule_id="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict") | .id' <<<"$ruleset" | head -n1)"
  [ -n "$ruleset_id" ] && [ -n "$rule_id" ] || die "Cloudflare strict SSL Configuration Rule was not reconciled cleanly."
  printf '%s|%s|%s\n' "$ruleset_id" "$rule_id" "$action"
}

save_cloudflare_state(){
  local host="$1" port="$2" zone_id="$3" zone_name="$4" dns_id="$5" dns_owned="$6" origin_ruleset_id="$7" origin_rule_id="$8" ssl_ruleset_id="$9" ssl_rule_id="${10}" cert_id="${11}" tmp old
  tmp="$(mktemp)"
  if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --argjson port "$port" --arg zone_id "$zone_id" --arg zone_name "$zone_name" --arg dns_id "$dns_id" --argjson dns_owned "$dns_owned" --arg origin_ruleset_id "$origin_ruleset_id" --arg origin_rule_id "$origin_rule_id" --arg ssl_ruleset_id "$ssl_ruleset_id" --arg ssl_rule_id "$ssl_rule_id" --arg cert_id "$cert_id" \
    '.active_provider="cloudflare" | .hostname=$host | .port=$port | .cloudflare={zone_id:$zone_id,zone_name:$zone_name,dns_record_id:$dns_id,dns_record_owned:$dns_owned,origin_ruleset_id:$origin_ruleset_id,origin_rule_id:$origin_rule_id,ssl_config_ruleset_id:$ssl_ruleset_id,ssl_config_rule_id:$ssl_rule_id,origin_certificate_id:$cert_id}' <<<"$old" > "$tmp"
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
}

save_previous_cloudflare_state(){
  local host="$1" zone_id="$2" dns_id="$3" dns_owned="$4" origin_ruleset_id="$5" origin_rule_id="$6" ssl_ruleset_id="$7" ssl_rule_id="$8" cert_id="$9" tmp old
  [ -n "$zone_id" ] || return 0
  tmp="$(mktemp)"
  if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --arg zone_id "$zone_id" --arg dns_id "$dns_id" --argjson dns_owned "$dns_owned" --arg origin_ruleset_id "$origin_ruleset_id" --arg origin_rule_id "$origin_rule_id" --arg ssl_ruleset_id "$ssl_ruleset_id" --arg ssl_rule_id "$ssl_rule_id" --arg cert_id "$cert_id" \
    '.cloudflare_previous={hostname:$host,zone_id:$zone_id,dns_record_id:$dns_id,dns_record_owned:$dns_owned,origin_ruleset_id:$origin_ruleset_id,origin_rule_id:$origin_rule_id,ssl_config_ruleset_id:$ssl_ruleset_id,ssl_config_rule_id:$ssl_rule_id,origin_certificate_id:$cert_id}' <<<"$old" > "$tmp"
  install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
}

clear_previous_cloudflare_state(){
  local tmp old
  [ -s "$MANAGED_STATE" ] || return 0
  tmp="$(mktemp)"; old="$(cat "$MANAGED_STATE")"
  jq 'del(.cloudflare_previous)' <<<"$old" > "$tmp"
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

rollback_new_cf_resources(){
  local zone_id="$1" dns_id="$2" dns_action="$3" origin_ruleset="$4" origin_rule="$5" origin_action="$6" ssl_ruleset="$7" ssl_rule="$8" ssl_action="$9" cert_id="${10}"
  if [ "$dns_action" = "created" ] && [ -n "$dns_id" ]; then cf_delete_owned "/zones/$zone_id/dns_records/$dns_id" || true; fi
  case "$origin_action" in created|recreated) [ -n "$origin_rule" ] && [ -n "$origin_ruleset" ] && cf_delete_owned "/zones/$zone_id/rulesets/$origin_ruleset/rules/$origin_rule" || true ;; esac
  case "$ssl_action" in created|recreated) [ -n "$ssl_rule" ] && [ -n "$ssl_ruleset" ] && cf_delete_owned "/zones/$zone_id/rulesets/$ssl_ruleset/rules/$ssl_rule" || true ;; esac
  [ -n "$cert_id" ] && cf_delete_owned "/certificates/$cert_id" || true
}

restore_updated_dns_record(){
  local zone_id="$1" host="$2" dns_id="$3" dns_action="$4" old_content="$5" old_proxied="$6" old_ttl="$7" body
  [ "$dns_action" = "updated" ] || return 0
  [ -n "$zone_id" ] && [ -n "$host" ] && [ -n "$dns_id" ] && [ -n "$old_content" ] && [ -n "$old_proxied" ] && [ -n "$old_ttl" ] || return 0
  body="$(jq -n --arg name "$host" --arg content "$old_content" --argjson proxied "$old_proxied" --argjson ttl "$old_ttl" '{type:"A",name:$name,content:$content,ttl:$ttl,proxied:$proxied,comment:"Managed by AI Server Agent"}')"
  if ! cf_api PATCH "/zones/$zone_id/dns_records/$dns_id" "$body" >/dev/null; then
    warn "Could not restore the previous Agent-owned Cloudflare DNS record automatically. Re-run Cloudflare setup or repair the recorded DNS record before relying on the public endpoint."
  fi
}

restore_updated_origin_rule(){
  local zone_id="$1" host="$2" old_port="$3" origin_ruleset="$4" origin_rule="$5" origin_action="$6"
  [ "$origin_action" = "updated" ] || return 0
  [ -n "$zone_id" ] && [ -n "$host" ] && [ -n "$origin_ruleset" ] && [ -n "$origin_rule" ] || return 0
  if ! cf_reconcile_origin_rule "$zone_id" "$host" "$old_port" "$origin_ruleset" "$origin_rule" >/dev/null; then
    warn "Could not restore the previous Agent-owned Cloudflare origin port rule automatically. Re-run Cloudflare setup or repair the recorded rule before relying on the public endpoint."
  fi
}

cleanup_old_cloudflare(){
  local old_host="$1" old_zone="$2" old_dns="$3" old_dns_owned="$4" old_origin_ruleset="$5" old_origin_rule="$6" old_ssl_ruleset="$7" old_ssl_rule="$8" old_cert="$9" new_host="${10}" new_cert="${11}" new_zone="${12}"
  [ -n "$old_host" ] || return 0
  if [ "$old_host" = "$new_host" ]; then
    if [ -n "$old_cert" ] && [ "$old_cert" != "$new_cert" ] && confirm "Revoke the previous Cloudflare Origin CA certificate now that the new certificate is verified?" yes; then
      cf_delete_owned "/certificates/$old_cert" || warn "Previous Origin CA certificate could not be revoked automatically."
    fi
    return 0
  fi
  save_previous_cloudflare_state "$old_host" "$old_zone" "$old_dns" "$old_dns_owned" "$old_origin_ruleset" "$old_origin_rule" "$old_ssl_ruleset" "$old_ssl_rule" "$old_cert"
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
}

configure_cloudflare(){
  need_cmd jq; need_cmd openssl; need_cmd curl; need_cmd sha256sum
  local old_host old_zone old_dns old_dns_owned old_origin_ruleset old_origin_rule old_ssl_ruleset old_ssl_rule old_cert old_port previous_zone
  local host port zone_pair zone_id zone_name ip stage backup cert_id dns_pair dns_id dns_owned dns_action dns_old_content dns_old_proxied dns_old_ttl
  local origin_pair origin_ruleset_id origin_rule_id origin_action ssl_pair ssl_ruleset_id ssl_rule_id ssl_action config_backup
  local owned_dns_id="" owned_origin_ruleset="" owned_origin_rule="" owned_ssl_ruleset="" owned_ssl_rule=""
  old_host="$(managed_get '.hostname')"; old_zone="$(managed_get '.cloudflare.zone_id')"; old_dns="$(managed_get '.cloudflare.dns_record_id')"; old_dns_owned="$(managed_get '.cloudflare.dns_record_owned')"; old_dns_owned="${old_dns_owned:-false}"
  old_origin_ruleset="$(managed_get '.cloudflare.origin_ruleset_id')"; old_origin_rule="$(managed_get '.cloudflare.origin_rule_id')"
  old_ssl_ruleset="$(managed_get '.cloudflare.ssl_config_ruleset_id')"; old_ssl_rule="$(managed_get '.cloudflare.ssl_config_rule_id')"; old_cert="$(managed_get '.cloudflare.origin_certificate_id')"
  old_port="$(current_port)"; previous_zone="$(managed_get '.cloudflare_previous.zone_id')"
  host="${AI_SERVER_AGENT_HOSTNAME:-}"; [ -n "$host" ] || host="$(prompt_value 'Public MCP hostname' "${old_host:-mcp.example.com}")"; host="${host,,}"; validate_hostname "$host"
  if [ -n "$previous_zone" ] && [ "$host" != "$old_host" ]; then
    die "A previous Cloudflare hostname still has recorded Agent-managed resources. Run 'sudo ai-server-agent-manage cloudflare-cleanup' before changing hostname again."
  fi
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
  stage="$(mktemp -d)"; backup="$(mktemp -d)"; chmod 0700 "$stage" "$backup"
  [ -e "$TLS_DIR/origin.key" ] && cp -p "$TLS_DIR/origin.key" "$backup/origin.key" || true
  [ -e "$TLS_DIR/origin.csr" ] && cp -p "$TLS_DIR/origin.csr" "$backup/origin.csr" || true
  [ -e "$TLS_DIR/origin.crt" ] && cp -p "$TLS_DIR/origin.crt" "$backup/origin.crt" || true
  cert_id="$(cf_issue_origin_cert "$host" "$stage")"; log "Fresh Origin CA certificate issued and verified."
  dns_pair="$(cf_reconcile_dns "$zone_id" "$host" "$ip" "$owned_dns_id")"; IFS='|' read -r dns_id dns_owned dns_action dns_old_content dns_old_proxied dns_old_ttl <<<"$dns_pair"
  if [ "$dns_owned" = "true" ]; then log "Cloudflare proxied DNS reconciled ($dns_action, Agent-owned)."; else log "Existing matching proxied DNS reused without taking ownership."; fi
  origin_pair="$(cf_reconcile_origin_rule "$zone_id" "$host" "$port" "$owned_origin_ruleset" "$owned_origin_rule")"; IFS='|' read -r origin_ruleset_id origin_rule_id origin_action <<<"$origin_pair"; log "Cloudflare origin port rule reconciled for port $port."
  ssl_pair="$(cf_reconcile_ssl_config_rule "$zone_id" "$host" "$owned_ssl_ruleset" "$owned_ssl_rule")"; IFS='|' read -r ssl_ruleset_id ssl_rule_id ssl_action <<<"$ssl_pair"; log "Cloudflare strict SSL Configuration Rule reconciled for $host only."
  install -d -o root -g "$AGENT_USER" -m 0750 "$TLS_DIR"
  install -o root -g "$AGENT_USER" -m 0640 "$stage/new.key" "$TLS_DIR/origin.key"
  install -o root -g root -m 0644 "$stage/new.csr" "$TLS_DIR/origin.csr"
  install -o root -g "$AGENT_USER" -m 0644 "$stage/new.crt" "$TLS_DIR/origin.crt"
  config_backup="$(mktemp)"; cp -a "$CONFIG_FILE" "$config_backup"
  write_config public "$port" "$TLS_DIR/origin.crt" "$TLS_DIR/origin.key"
  if ! restart_and_verify_local; then
    install -o root -g "$AGENT_USER" -m 0640 "$config_backup" "$CONFIG_FILE"; restore_tls_backup "$backup"; systemctl restart ai-server-agent-executor.service ai-server-agent.service || true
    rollback_new_cf_resources "$zone_id" "$dns_id" "$dns_action" "$origin_ruleset_id" "$origin_rule_id" "$origin_action" "$ssl_ruleset_id" "$ssl_rule_id" "$ssl_action" "$cert_id"
    if [ "$old_host" = "$host" ] && [ "$old_zone" = "$zone_id" ]; then
      restore_updated_dns_record "$old_zone" "$old_host" "$old_dns" "$dns_action" "$dns_old_content" "$dns_old_proxied" "$dns_old_ttl"
      restore_updated_origin_rule "$old_zone" "$old_host" "$old_port" "$old_origin_ruleset" "$old_origin_rule" "$origin_action"
    fi
    rm -rf "$stage" "$backup"; rm -f "$config_backup"; CF_TOKEN=""
    die "Agent failed after TLS/public reconfiguration; previous local Agent state was restored and newly created Cloudflare resources were rolled back where safe."
  fi
  if ! verify_public "$host"; then
    install -o root -g "$AGENT_USER" -m 0640 "$config_backup" "$CONFIG_FILE"; restore_tls_backup "$backup"; systemctl restart ai-server-agent-executor.service ai-server-agent.service || true
    rollback_new_cf_resources "$zone_id" "$dns_id" "$dns_action" "$origin_ruleset_id" "$origin_rule_id" "$origin_action" "$ssl_ruleset_id" "$ssl_rule_id" "$ssl_action" "$cert_id"
    if [ "$old_host" = "$host" ] && [ "$old_zone" = "$zone_id" ]; then
      restore_updated_dns_record "$old_zone" "$old_host" "$old_dns" "$dns_action" "$dns_old_content" "$dns_old_proxied" "$dns_old_ttl"
      restore_updated_origin_rule "$old_zone" "$old_host" "$old_port" "$old_origin_ruleset" "$old_origin_rule" "$origin_action"
    fi
    rm -rf "$stage" "$backup"; rm -f "$config_backup"; CF_TOKEN=""
    die "Public Cloudflare verification failed. Previous Agent state was restored and newly created Cloudflare resources were rolled back where safe."
  fi
  rm -rf "$stage" "$backup"; rm -f "$config_backup"
  save_cloudflare_state "$host" "$port" "$zone_id" "$zone_name" "$dns_id" "$dns_owned" "$origin_ruleset_id" "$origin_rule_id" "$ssl_ruleset_id" "$ssl_rule_id" "$cert_id"
  log "Public HTTPS health, unauthenticated rejection, and authenticated MCP initialize all passed."
  cleanup_old_cloudflare "$old_host" "$old_zone" "$old_dns" "$old_dns_owned" "$old_origin_ruleset" "$old_origin_rule" "$old_ssl_ruleset" "$old_ssl_rule" "$old_cert" "$host" "$cert_id" "$zone_id"
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
  local source zone_id dns_id dns_owned origin_ruleset_id origin_rule_id ssl_ruleset_id ssl_rule_id cert_id host tmp old previous_zone
  previous_zone="$(managed_get '.cloudflare_previous.zone_id')"
  if [ -n "$previous_zone" ]; then
    source=previous
    zone_id="$previous_zone"
    host="$(managed_get '.cloudflare_previous.hostname')"
    dns_id="$(managed_get '.cloudflare_previous.dns_record_id')"; dns_owned="$(managed_get '.cloudflare_previous.dns_record_owned')"; dns_owned="${dns_owned:-false}"
    origin_ruleset_id="$(managed_get '.cloudflare_previous.origin_ruleset_id')"; origin_rule_id="$(managed_get '.cloudflare_previous.origin_rule_id')"
    ssl_ruleset_id="$(managed_get '.cloudflare_previous.ssl_config_ruleset_id')"; ssl_rule_id="$(managed_get '.cloudflare_previous.ssl_config_rule_id')"
    cert_id="$(managed_get '.cloudflare_previous.origin_certificate_id')"
    printf 'Deferred Cloudflare cleanup hostname: %s\n' "$host"
  else
    source=current
    zone_id="$(managed_get '.cloudflare.zone_id')"; dns_id="$(managed_get '.cloudflare.dns_record_id')"; dns_owned="$(managed_get '.cloudflare.dns_record_owned')"; dns_owned="${dns_owned:-false}"
    origin_ruleset_id="$(managed_get '.cloudflare.origin_ruleset_id')"; origin_rule_id="$(managed_get '.cloudflare.origin_rule_id')"
    ssl_ruleset_id="$(managed_get '.cloudflare.ssl_config_ruleset_id')"; ssl_rule_id="$(managed_get '.cloudflare.ssl_config_rule_id')"
    cert_id="$(managed_get '.cloudflare.origin_certificate_id')"; host="$(managed_get '.hostname')"
    [ -n "$zone_id" ] || { log "No recorded Cloudflare-managed resources."; return 0; }
    if [ "$(current_mode)" = "public" ] && [ "$(managed_get '.active_provider')" = "cloudflare" ]; then
      die "This Cloudflare hostname is currently carrying the MCP connection. Switch to local/manual mode before cleanup."
    fi
    printf 'Recorded Cloudflare hostname: %s\n' "$host"
  fi
  if [ "$dns_owned" = "true" ]; then printf 'The DNS record is recorded as Agent-owned and can be removed by this cleanup.\n'; else printf 'The DNS record is external/reused and will be preserved.\n'; fi
  confirm "Delete the recorded Agent-owned Cloudflare DNS (if any), Origin Rule, strict SSL Configuration Rule, and Origin CA certificate?" no || { echo "Cancelled."; return 0; }
  print_cf_token_guidance "$host"
  load_cf_token
  if ! delete_recorded_cloudflare_resources "$zone_id" "$dns_id" "$dns_owned" "$origin_ruleset_id" "$origin_rule_id" "$ssl_ruleset_id" "$ssl_rule_id" "$cert_id"; then
    CF_TOKEN=""
    die "Cloudflare cleanup was incomplete. Recorded ownership state was preserved for a safe retry."
  fi
  CF_TOKEN=""
  if [ "$source" = previous ]; then
    clear_previous_cloudflare_state
  else
    tmp="$(mktemp)"; old="$(cat "$MANAGED_STATE")"; jq '.cloudflare={}' <<<"$old" > "$tmp"; install -o root -g "$AGENT_USER" -m 0640 "$tmp" "$MANAGED_STATE"; rm -f "$tmp"
  fi
  log "Recorded Agent-owned Cloudflare resources were removed or already absent; external DNS and unrecorded rules were preserved."
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
    if [ -n "$(managed_get '.cloudflare.zone_id')" ] || [ -n "$(managed_get '.cloudflare_previous.zone_id')" ]; then warn "Cloudflare-managed resources are still recorded. Use Cloudflare cleanup first if you want them removed too."; fi
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
  printf '  3) Existing certificate - advanced/manual TLS\n'
  printf '  0) Configure later - keep the healthy local Agent\n\n'
  local choice
  read -r -p 'Choose [1]: ' choice </dev/tty; choice="${choice:-1}"
  case "$choice" in
    1) configure_cloudflare ;;
    2) configure_local ;;
    3) configure_manual_tls ;;
    0) printf '\nSetup deferred. The Agent remains installed and healthy in local mode.\n'; return 20 ;;
    *) die "Invalid choice." ;;
  esac
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
