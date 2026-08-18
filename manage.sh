#!/usr/bin/env bash
set -Eeuo pipefail

CONFIG_DIR="/etc/ai-server-agent"
CONFIG_FILE="$CONFIG_DIR/config.json"
STATE_DIR="/var/lib/ai-server-agent"
CONTROL_DIR="$CONFIG_DIR/control"
INSTALL_STATE="$CONTROL_DIR/install-state.json"
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
CF_TXN_STATE="$CONTROL_DIR/cloudflare-transaction.json"
CF_PENDING_KIND=""
CF_PENDING_ZONE=""
CF_PENDING_HOST=""
CF_PENDING_VALUE=""
CF_PENDING_PHASE=""
CF_PENDING_MARKER=""
CF_PENDING_FINGERPRINT=""
CF_TXN_PHASE="prepared"
CF_TXN_BACKUP_DIR="$CONTROL_DIR/cloudflare-transaction-backup"
CF_TXN_BACKUP_READY=false
MANAGEMENT_LOCK="$CONTROL_DIR/management.lock"
MANAGEMENT_LOCK_FD=""
CF_RESULT_DNS_FINGERPRINT=""
CF_RESULT_ORIGIN_FINGERPRINT=""
CF_RESULT_SSL_FINGERPRINT=""
CF_COMMIT_HOST=""
CF_COMMIT_PORT=""
CF_COMMIT_ZONE_ID=""
CF_COMMIT_ZONE_NAME=""
CF_COMMIT_DNS_ID=""
CF_COMMIT_DNS_OWNED=false
CF_COMMIT_ORIGIN_RULESET_ID=""
CF_COMMIT_ORIGIN_RULE_ID=""
CF_COMMIT_SSL_RULESET_ID=""
CF_COMMIT_SSL_RULE_ID=""
CF_COMMIT_CERT_ID=""
CF_COMMIT_DNS_FINGERPRINT=""
CF_COMMIT_ORIGIN_FINGERPRINT=""
CF_COMMIT_SSL_FINGERPRINT=""

log(){ printf '[ai-server-agent] %s\n' "$*"; }
warn(){ printf '[ai-server-agent] WARNING: %s\n' "$*" >&2; }
die(){ printf '[ai-server-agent] ERROR: %s\n' "$*" >&2; exit 1; }
need_root(){ [ "$(id -u)" -eq 0 ] || die "Run as root (for example: sudo ai-server-agent-manage)."; }
need_cmd(){ command -v "$1" >/dev/null 2>&1 || die "$1 is required. Run the installer/update repair path first."; }

acquire_management_lock(){
  [ -n "${MANAGEMENT_LOCK_FD:-}" ] && return 0
  need_cmd flock
  install -d -o root -g root -m 0700 "$CONTROL_DIR" || die "Could not secure the management control directory."
  [ ! -L "$MANAGEMENT_LOCK" ] || die "Refusing symlinked management lock: $MANAGEMENT_LOCK"
  ( umask 077; : >> "$MANAGEMENT_LOCK" ) || die "Could not create the management lock."
  [ -f "$MANAGEMENT_LOCK" ] && [ ! -L "$MANAGEMENT_LOCK" ] || die "Management lock is not a regular file."
  chown root:root "$MANAGEMENT_LOCK" || die "Could not secure the management lock owner."
  chmod 0600 "$MANAGEMENT_LOCK" || die "Could not secure the management lock mode."
  [ "$(stat -c '%u:%g:%a' "$MANAGEMENT_LOCK" 2>/dev/null)" = "0:0:600" ] || die "Management lock ownership/mode is unsafe."
  exec {MANAGEMENT_LOCK_FD}>>"$MANAGEMENT_LOCK" || die "Could not open the management lock."
  flock -n "$MANAGEMENT_LOCK_FD" || die "Another AI Server Agent connection-management operation is already active. Retry after it finishes."
}

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
  local json channel version ref track_ref
  if [ ! -e "$INSTALL_STATE" ]; then printf 'unknown|unknown|\n'; return 0; fi
  if [ ! -f "$INSTALL_STATE" ] || [ -L "$INSTALL_STATE" ] || [ "$(stat -c '%s' "$INSTALL_STATE" 2>/dev/null || printf 999999)" -gt 4096 ]; then
    warn "Install identity metadata is invalid; refusing to execute or trust it."
    printf 'unknown|unknown|\n'
    return 0
  fi
  json="$(cat -- "$INSTALL_STATE" 2>/dev/null || true)"
  if ! jq -e 'type=="object" and (keys|sort)==["channel","ref","track_ref","version"] and (.channel|type)=="string" and (.version|type)=="string" and (.ref|type)=="string" and (.track_ref|type)=="string"' >/dev/null 2>&1 <<<"$json"; then
    warn "Install identity metadata has an invalid schema; refusing to trust it."
    printf 'unknown|unknown|\n'
    return 0
  fi
  channel="$(jq -r '.channel' <<<"$json")"; version="$(jq -r '.version' <<<"$json")"; ref="$(jq -r '.ref' <<<"$json")"; track_ref="$(jq -r '.track_ref' <<<"$json")"
  case "$channel" in
    stable)
      if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || [ "$ref" != "$version" ] || [ "$track_ref" != "$version" ]; then
        warn "Stable install identity metadata is inconsistent; refusing to trust it."
        printf 'unknown|unknown|\n'; return 0
      fi
      ;;
    source)
      if [ "$version" != source ] || ! [[ "$ref" =~ ^([0-9a-f]{40}|binary)$ ]] || ! [[ "$track_ref" =~ ^[A-Za-z0-9._/-]+$|^[0-9a-fA-F]{40}$ ]]; then
        warn "Source install identity metadata is invalid; refusing to trust it."
        printf 'unknown|unknown|\n'; return 0
      fi
      ;;
    *) warn "Install identity channel is invalid; refusing to trust it."; printf 'unknown|unknown|\n'; return 0 ;;
  esac
  printf '%s|%s|%s\n' "$channel" "$version" "$ref"
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
  local ruleset_base cursor="" encoded_cursor page_path page_out next_cursor pages=0 merged='[]'
  local -a retry_args=()
  cfg="$(mktemp)"; chmod 0600 "$cfg"
  printf 'header = "Authorization: Bearer %s"\n' "$CF_TOKEN" > "$cfg"
  case "$method" in GET|PATCH|DELETE) retry_args=(--retry 2) ;; esac

  # The Rulesets list endpoint is cursor-paginated and currently caps per_page at 50.
  # Existing callers request a complete list with the legacy per_page=100 sentinel;
  # translate that logical request into bounded cursor traversal so absence decisions
  # are made only after every returned page has been inspected.
  if [ "$method" = GET ] && [[ "$path" =~ ^(/zones/[^/?]+/rulesets)\?per_page=100$ ]]; then
    ruleset_base="${BASH_REMATCH[1]}"
    while :; do
      pages=$((pages + 1)); [ "$pages" -le 1000 ] || { rm -f "$cfg"; return 1; }
      if [ -n "$cursor" ]; then
        encoded_cursor="$(jq -rn --arg cursor "$cursor" '$cursor|@uri')"
        page_path="$ruleset_base?per_page=50&cursor=$encoded_cursor"
      else
        page_path="$ruleset_base?per_page=50"
      fi
      page_out="$(curl -sS --fail-with-body "${retry_args[@]}" --request GET --config "$cfg" -H 'Content-Type: application/json' "$CF_API$page_path")" || { rm -f "$cfg"; return 1; }
      if ! jq -e 'def valid_kind: .=="managed" or .=="custom" or .=="root" or .=="zone"; def valid_phase: .=="ddos_l4" or .=="ddos_l7" or .=="http_config_settings" or .=="http_custom_errors" or .=="http_log_custom_fields" or .=="http_ratelimit" or .=="http_request_cache_settings" or .=="http_request_dynamic_redirect" or .=="http_request_firewall_custom" or .=="http_request_firewall_managed" or .=="http_request_late_transform" or .=="http_request_origin" or .=="http_request_redirect" or .=="http_request_sanitize" or .=="http_request_sbfm" or .=="http_request_transform" or .=="http_response_cache_settings" or .=="http_response_compression" or .=="http_response_firewall_managed" or .=="http_response_headers_transform" or .=="magic_transit" or .=="magic_transit_ids_managed" or .=="magic_transit_managed" or .=="magic_transit_ratelimit"; .success == true and (.result|type)=="array" and all(.result[]; type=="object" and (.id|type)=="string" and (.id|length)>0 and (.kind|type)=="string" and (.kind|valid_kind) and (.phase|type)=="string" and (.phase|valid_phase)) and (.result_info|type)=="object" and (.result_info.cursors|type)=="object" and (((.result_info.cursors|has("after"))|not) or ((.result_info.cursors.after|type)=="string" and (.result_info.cursors.after|length)>0))' >/dev/null 2>&1 <<<"$page_out"; then
        jq -r '.errors[]?.message // empty' <<<"$page_out" >&2 || true
        rm -f "$cfg"; return 1
      fi
      merged="$(jq -cn --argjson acc "$merged" --argjson response "$page_out" '$acc + $response.result')"
      next_cursor="$(jq -r 'if (.result_info.cursors|has("after")) then .result_info.cursors.after else empty end' <<<"$page_out")"
      [ -n "$next_cursor" ] || break
      [ "$next_cursor" != "$cursor" ] || { rm -f "$cfg"; return 1; }
      cursor="$next_cursor"
    done
    rm -f "$cfg"
    jq -cn --argjson result "$merged" '{success:true,result:$result}'
    return 0
  fi

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

cf_get_optional(){
  local path="$1" cfg response status body
  cfg="$(mktemp)"; chmod 0600 "$cfg"
  printf 'header = "Authorization: Bearer %s"\n' "$CF_TOKEN" > "$cfg"
  response="$(curl -sS --retry 2 --request GET --config "$cfg" -H 'Content-Type: application/json' -w '\n%{http_code}' "$CF_API$path")" || { rm -f "$cfg"; return 2; }
  rm -f "$cfg"
  status="${response##*$'\n'}"; body="${response%$'\n'*}"
  [ "$status" = "404" ] && return 3
  [[ "$status" =~ ^2[0-9][0-9]$ ]] || return 2
  jq -e '.success == true' >/dev/null 2>&1 <<<"$body" || return 2
  printf '%s' "$body"
}

cf_dns_fingerprint(){
  jq -cS '{id:(.id // ""),type:(.type // ""),name:(.name // ""),content:(.content // ""),ttl:(.ttl // 0),proxied:(.proxied // false),comment:(.comment // "")}' | sha256sum | awk '{print $1}'
}

cf_rule_fingerprint(){
  jq -cS '{id:(.id // ""),ref:(.ref // ""),description:(.description // ""),expression:(.expression // ""),action:(.action // ""),action_parameters:(.action_parameters // {}),enabled:(.enabled // false)}' | sha256sum | awk '{print $1}'
}

cf_dns_intent_fingerprint(){
  jq -cS '{type:(.type // ""),name:(.name // ""),content:(.content // ""),ttl:(.ttl // 0),proxied:(.proxied // false),comment:(.comment // "")}' | sha256sum | awk '{print $1}'
}

cf_rule_intent_fingerprint(){
  jq -cS '{ref:(.ref // ""),description:(.description // ""),expression:(.expression // ""),action:(.action // ""),action_parameters:(.action_parameters // {}),enabled:(.enabled // false)}' | sha256sum | awk '{print $1}'
}

cf_origin_cert_intent_fingerprint(){
  jq -cS '{csr:(.csr // ""),hostnames:((.hostnames // []) | sort),request_type:(.request_type // ""),requested_validity:(.requested_validity // 0)}' | sha256sum | awk '{print $1}'
}

cf_get_dns_record(){
  local zone_id="$1" dns_id="$2" res rc
  if res="$(cf_get_optional "/zones/$zone_id/dns_records/$dns_id")"; then
    jq -ce '.result | select(type=="object")' <<<"$res" || return 2
    return 0
  fi
  rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
}

cf_get_rule(){
  local zone_id="$1" ruleset_id="$2" rule_id="$3" res rc rule
  if ! res="$(cf_get_optional "/zones/$zone_id/rulesets/$ruleset_id")"; then
    rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
  fi
  rule="$(jq -c --arg id "$rule_id" '.result.rules[]? | select(.id==$id)' <<<"$res" | head -n1)"
  [ -n "$rule" ] || return 3
  printf '%s\n' "$rule"
}

cf_get_origin_cert(){
  local cert_id="$1" res rc
  if res="$(cf_get_optional "/certificates/$cert_id")"; then
    jq -ce '.result | select(type=="object")' <<<"$res" || return 2
    return 0
  fi
  rc=$?; [ "$rc" -eq 3 ] && return 3; return 2
}

cf_delete_dns_if_expected(){
  local zone_id="$1" dns_id="$2" expected="$3" current rc actual
  [ -n "$zone_id" ] && [ -n "$dns_id" ] && [ -n "$expected" ] || return 1
  if current="$(cf_get_dns_record "$zone_id" "$dns_id")"; then
    actual="$(cf_dns_fingerprint <<<"$current")"
  else
    rc=$?; [ "$rc" -eq 3 ] && return 0; return 1
  fi
  [ "$actual" = "$expected" ] || { warn "Recorded DNS $dns_id changed since Agent ownership was checkpointed; refusing automatic deletion."; return 1; }
  cf_delete_owned "/zones/$zone_id/dns_records/$dns_id"
}

cf_delete_rule_if_expected(){
  local zone_id="$1" ruleset_id="$2" rule_id="$3" expected="$4" current rc actual
  [ -n "$zone_id" ] && [ -n "$ruleset_id" ] && [ -n "$rule_id" ] && [ -n "$expected" ] || return 1
  if current="$(cf_get_rule "$zone_id" "$ruleset_id" "$rule_id")"; then
    actual="$(cf_rule_fingerprint <<<"$current")"
  else
    rc=$?; [ "$rc" -eq 3 ] && return 0; return 1
  fi
  [ "$actual" = "$expected" ] || { warn "Recorded Cloudflare rule $rule_id changed since Agent ownership was checkpointed; refusing automatic deletion."; return 1; }
  cf_delete_owned "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id"
}

cf_delete_pending_dns_if_expected(){
  local zone_id="$1" dns_id="$2" expected="$3" current rc actual
  [ -n "$zone_id" ] && [ -n "$dns_id" ] && [ -n "$expected" ] || return 1
  if current="$(cf_get_dns_record "$zone_id" "$dns_id")"; then
    actual="$(cf_dns_intent_fingerprint <<<"$current")"
  else
    rc=$?; [ "$rc" -eq 3 ] && return 0; return 1
  fi
  [ "$actual" = "$expected" ] || { warn "Response-lost DNS $dns_id no longer matches the durable create representation; refusing automatic deletion."; return 1; }
  cf_delete_owned "/zones/$zone_id/dns_records/$dns_id"
}

cf_delete_pending_rule_if_expected(){
  local zone_id="$1" ruleset_id="$2" rule_id="$3" expected="$4" current rc actual
  [ -n "$zone_id" ] && [ -n "$ruleset_id" ] && [ -n "$rule_id" ] && [ -n "$expected" ] || return 1
  if current="$(cf_get_rule "$zone_id" "$ruleset_id" "$rule_id")"; then
    actual="$(cf_rule_intent_fingerprint <<<"$current")"
  else
    rc=$?; [ "$rc" -eq 3 ] && return 0; return 1
  fi
  [ "$actual" = "$expected" ] || { warn "Response-lost Cloudflare rule $rule_id no longer matches the durable create representation; refusing automatic deletion."; return 1; }
  cf_delete_owned "/zones/$zone_id/rulesets/$ruleset_id/rules/$rule_id"
}

cf_delete_pending_origin_cert_if_expected(){
  local cert_id="$1" expected="$2" current rc actual
  [ -n "$cert_id" ] && [ -n "$expected" ] || return 1
  if current="$(cf_get_origin_cert "$cert_id")"; then
    actual="$(cf_origin_cert_intent_fingerprint <<<"$current")"
  else
    rc=$?; [ "$rc" -eq 3 ] && return 0; return 1
  fi
  [ "$actual" = "$expected" ] || { warn "Response-lost Origin CA certificate $cert_id no longer matches the durable create representation; refusing automatic revocation."; return 1; }
  cf_delete_owned "/certificates/$cert_id"
}

cf_clear_pending_write(){
  CF_PENDING_KIND=""; CF_PENDING_ZONE=""; CF_PENDING_HOST=""; CF_PENDING_VALUE=""; CF_PENDING_PHASE=""; CF_PENDING_MARKER=""; CF_PENDING_FINGERPRINT=""
}

cf_clear_commit_intent(){
  CF_COMMIT_HOST=""; CF_COMMIT_PORT=""; CF_COMMIT_ZONE_ID=""; CF_COMMIT_ZONE_NAME=""; CF_COMMIT_DNS_ID=""; CF_COMMIT_DNS_OWNED=false
  CF_COMMIT_ORIGIN_RULESET_ID=""; CF_COMMIT_ORIGIN_RULE_ID=""; CF_COMMIT_SSL_RULESET_ID=""; CF_COMMIT_SSL_RULE_ID=""; CF_COMMIT_CERT_ID=""
  CF_COMMIT_DNS_FINGERPRINT=""; CF_COMMIT_ORIGIN_FINGERPRINT=""; CF_COMMIT_SSL_FINGERPRINT=""
}

cf_new_ownership_marker(){ openssl rand -hex 16; }

save_current_cloudflare_transaction_state(){
  save_cloudflare_transaction_state \
    "${host:-${CF_PENDING_HOST:-}}" "${zone_id:-${CF_PENDING_ZONE:-}}" \
    "${dns_id:-${CF_RESULT_DNS_ID:-}}" "${dns_action:-${CF_RESULT_DNS_ACTION:-}}" \
    "${origin_ruleset_id:-${CF_RESULT_ORIGIN_RULESET_ID:-}}" "${origin_rule_id:-${CF_RESULT_ORIGIN_RULE_ID:-}}" "${origin_action:-${CF_RESULT_ORIGIN_ACTION:-}}" \
    "${ssl_ruleset_id:-${CF_RESULT_SSL_RULESET_ID:-}}" "${ssl_rule_id:-${CF_RESULT_SSL_RULE_ID:-}}" "${ssl_action:-${CF_RESULT_SSL_ACTION:-}}" \
    "${cert_id:-${CF_RESULT_CERT_ID:-}}" \
    "${dns_fingerprint:-${CF_RESULT_DNS_FINGERPRINT:-}}" "${origin_fingerprint:-${CF_RESULT_ORIGIN_FINGERPRINT:-}}" "${ssl_fingerprint:-${CF_RESULT_SSL_FINGERPRINT:-}}"
}

cf_checkpoint_transaction(){
  save_current_cloudflare_transaction_state || die "Could not durably checkpoint the Cloudflare transaction before continuing."
}

cf_set_pending_write(){
  CF_PENDING_KIND="$1"; CF_PENDING_ZONE="$2"; CF_PENDING_HOST="$3"; CF_PENDING_VALUE="$4"; CF_PENDING_PHASE="${5:-}"; CF_PENDING_MARKER="${6:-}"; CF_PENDING_FINGERPRINT="${7:-}"
  save_current_cloudflare_transaction_state || die "Could not durably journal the Cloudflare create intent before remote mutation. No create request was sent."
}

cf_commit_pending_write(){
  local old_kind="$CF_PENDING_KIND" old_zone="$CF_PENDING_ZONE" old_host="$CF_PENDING_HOST" old_value="$CF_PENDING_VALUE" old_phase="$CF_PENDING_PHASE" old_marker="$CF_PENDING_MARKER" old_fingerprint="$CF_PENDING_FINGERPRINT"
  cf_clear_pending_write
  if save_current_cloudflare_transaction_state; then return 0; fi
  CF_PENDING_KIND="$old_kind"; CF_PENDING_ZONE="$old_zone"; CF_PENDING_HOST="$old_host"; CF_PENDING_VALUE="$old_value"; CF_PENDING_PHASE="$old_phase"; CF_PENDING_MARKER="$old_marker"; CF_PENDING_FINGERPRINT="$old_fingerprint"
  die "Cloudflare create succeeded but the confirmed result could not be durably checkpointed. Recovery intent remains active."
}

cf_finish_transaction_step(){
  if [ -n "$CF_PENDING_KIND" ]; then cf_commit_pending_write; else cf_checkpoint_transaction; fi
}

cf_set_commit_intent(){
  CF_COMMIT_HOST="$1"; CF_COMMIT_PORT="$2"; CF_COMMIT_ZONE_ID="$3"; CF_COMMIT_ZONE_NAME="$4"; CF_COMMIT_DNS_ID="$5"; CF_COMMIT_DNS_OWNED="$6"
  CF_COMMIT_ORIGIN_RULESET_ID="$7"; CF_COMMIT_ORIGIN_RULE_ID="$8"; CF_COMMIT_SSL_RULESET_ID="$9"; CF_COMMIT_SSL_RULE_ID="${10}"; CF_COMMIT_CERT_ID="${11}"
  CF_COMMIT_DNS_FINGERPRINT="${12}"; CF_COMMIT_ORIGIN_FINGERPRINT="${13}"; CF_COMMIT_SSL_FINGERPRINT="${14}"
  CF_TXN_PHASE=committing
  cf_checkpoint_transaction
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
  local zone_id="$1" host="$2" ip="$3" marker="$4" res ids exact_count total
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

cf_recover_pending_write(){
  local found rc ruleset_id rule_id
  [ -n "$CF_PENDING_KIND" ] || return 0
  case "$CF_PENDING_KIND" in
    origin-cert-create)
      if found="$(cf_find_origin_cert_by_csr "$CF_PENDING_ZONE" "$CF_PENDING_VALUE")"; then
        cf_delete_pending_origin_cert_if_expected "$found" "$CF_PENDING_FINGERPRINT" || return 1
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    dns-create)
      if found="$(cf_find_dns_by_marker "$CF_PENDING_ZONE" "$CF_PENDING_HOST" "$CF_PENDING_VALUE" "$CF_PENDING_MARKER")"; then
        cf_delete_pending_dns_if_expected "$CF_PENDING_ZONE" "$found" "$CF_PENDING_FINGERPRINT" || return 1
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    origin-rule-create|ssl-rule-create)
      if found="$(cf_find_rule_by_marker "$CF_PENDING_ZONE" "$CF_PENDING_PHASE" "$CF_PENDING_VALUE" "$CF_PENDING_MARKER")"; then
        IFS='|' read -r ruleset_id rule_id <<<"$found"
        cf_delete_pending_rule_if_expected "$CF_PENDING_ZONE" "$ruleset_id" "$rule_id" "$CF_PENDING_FINGERPRINT" || return 1
      else
        rc=$?; [ "$rc" -eq 1 ] || return 1
      fi
      ;;
    *) return 1 ;;
  esac
  cf_clear_pending_write
}

delete_recorded_cloudflare_resources(){
  local zone_id="$1" dns_id="$2" dns_owned="$3" origin_ruleset="$4" origin_rule="$5" ssl_ruleset="$6" ssl_rule="$7" cert_id="$8" dns_fingerprint="${9:-}" origin_fingerprint="${10:-}" ssl_fingerprint="${11:-}" failed=0
  if [ "$dns_owned" = "true" ] && [ -n "$dns_id" ] && ! cf_delete_dns_if_expected "$zone_id" "$dns_id" "$dns_fingerprint"; then warn "Could not safely delete recorded Agent-owned DNS record $dns_id; ownership representation was preserved for manual resolution."; failed=1; fi
  if [ -n "$origin_rule" ] && [ -n "$origin_ruleset" ] && ! cf_delete_rule_if_expected "$zone_id" "$origin_ruleset" "$origin_rule" "$origin_fingerprint"; then warn "Could not safely delete recorded Agent-owned Origin Rule $origin_rule; ownership representation was preserved for manual resolution."; failed=1; fi
  if [ -n "$ssl_rule" ] && [ -n "$ssl_ruleset" ] && ! cf_delete_rule_if_expected "$zone_id" "$ssl_ruleset" "$ssl_rule" "$ssl_fingerprint"; then warn "Could not safely delete recorded Agent-owned Configuration Rule $ssl_rule; ownership representation was preserved for manual resolution."; failed=1; fi
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
  local zone_id="$1" host="$2" stage="$3" key="$stage/new.key" csr="$stage/new.csr" crt="$stage/new.crt" csr_value body res key_pub cert_pub pending_fingerprint cert_record
  CF_RESULT_CERT_ID=""
  umask 077
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$key" >/dev/null 2>&1
  openssl req -new -sha256 -key "$key" -out "$csr" -subj "/CN=$host" -addext "subjectAltName=DNS:$host"
  csr_value="$(cat "$csr")"
  body="$(jq -n --arg csr "$csr_value" --arg host "$host" '{hostnames:[$host],request_type:"origin-rsa",requested_validity:1095,csr:$csr}')"
  pending_fingerprint="$(cf_origin_cert_intent_fingerprint <<<"$body")"
  cf_set_pending_write origin-cert-create "$zone_id" "$host" "$csr_value" "" "" "$pending_fingerprint"
  res="$(cf_api POST '/certificates' "$body")" || die "Cloudflare Origin CA certificate issuance response was not confirmed. Durable transaction recovery will verify the exact create representation before any cleanup."
  cert_record="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid Origin CA certificate response."
  [ "$(cf_origin_cert_intent_fingerprint <<<"$cert_record")" = "$pending_fingerprint" ] || die "Cloudflare returned Origin CA certificate metadata that does not match the durable create representation."
  jq -r '.certificate // empty' <<<"$cert_record" > "$crt"
  CF_RESULT_CERT_ID="$(jq -r '.id // empty' <<<"$cert_record")"
  [ -s "$crt" ] && [ -n "$CF_RESULT_CERT_ID" ] || die "Cloudflare returned an incomplete Origin CA certificate response."
  openssl pkey -in "$key" -check -noout >/dev/null 2>&1 || die "Generated private key failed validation."
  openssl x509 -in "$crt" -noout -checkhost "$host" >/dev/null || die "Issued certificate does not match $host."
  key_pub="$(openssl pkey -in "$key" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  cert_pub="$(openssl x509 -in "$crt" -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
  [ "$key_pub" = "$cert_pub" ] || die "Issued certificate does not match the generated private key."
}

cf_reconcile_dns(){
  local zone_id="$1" host="$2" ip="$3" owned_dns_id="${4:-}" owned_fingerprint="${5:-}" res count id type content proxied record current_fingerprint comment body marker pending_fingerprint
  CF_RESULT_DNS_ID=""; CF_RESULT_DNS_OWNED=false; CF_RESULT_DNS_ACTION=""; CF_RESULT_DNS_FINGERPRINT=""
  res="$(cf_api GET "/zones/$zone_id/dns_records?name=$host&per_page=100")" || die "Cloudflare DNS lookup failed."
  count="$(jq '.result | length' <<<"$res")"
  if [ "$count" -eq 0 ]; then
    marker="$(cf_new_ownership_marker)"; comment="Managed by AI Server Agent txn:$marker"
    body="$(jq -n --arg name "$host" --arg ip "$ip" --arg comment "$comment" '{type:"A",name:$name,content:$ip,ttl:1,proxied:true,comment:$comment}')"
    pending_fingerprint="$(cf_dns_intent_fingerprint <<<"$body")"
    cf_set_pending_write dns-create "$zone_id" "$host" "$ip" "" "$marker" "$pending_fingerprint"
    res="$(cf_api POST "/zones/$zone_id/dns_records" "$body")" || die "Cloudflare DNS create response was not confirmed. Durable transaction recovery will verify the exact create representation before any cleanup."
    id="$(jq -r '.result.id // empty' <<<"$res")"; [ -n "$id" ] || die "Cloudflare did not return the created DNS record ID."
    record="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid created DNS record."
    CF_RESULT_DNS_ID="$id"; CF_RESULT_DNS_OWNED=true; CF_RESULT_DNS_ACTION=created; CF_RESULT_DNS_FINGERPRINT="$(cf_dns_fingerprint <<<"$record")"
    return
  fi
  [ "$count" -eq 1 ] || die "Multiple DNS records already exist for $host. Refusing ambiguous replacement."
  record="$(jq -ce '.result[0] | select(type=="object")' <<<"$res")" || die "Cloudflare DNS response was invalid."
  id="$(jq -r '.id' <<<"$record")"; type="$(jq -r '.type' <<<"$record")"; content="$(jq -r '.content' <<<"$record")"; proxied="$(jq -r '.proxied' <<<"$record")"
  [ "$type" = "A" ] || die "$host already has a $type record. Use another hostname or resolve the DNS conflict manually."
  current_fingerprint="$(cf_dns_fingerprint <<<"$record")"
  CF_RESULT_DNS_ID="$id"; CF_RESULT_DNS_FINGERPRINT="$current_fingerprint"
  if [ -n "$owned_dns_id" ]; then
    [ "$id" = "$owned_dns_id" ] || die "The DNS record ID for $host no longer matches recorded Agent ownership. Refusing to adopt or modify the replacement record."
    [ -z "$owned_fingerprint" ] || [ "$owned_fingerprint" = "$current_fingerprint" ] || die "The recorded Agent-owned DNS representation changed outside this transaction. Refusing automatic mutation; resolve the Cloudflare drift explicitly."
    CF_RESULT_DNS_OWNED=true
    [ "$content" = "$ip" ] && [ "$proxied" = "true" ] || die "The Agent-owned DNS record needs an in-place update, but Cloudflare DNS mutation has no conditional compare-and-swap used by this manager. Refusing to overwrite concurrent state; clean up or reconcile the record explicitly, then rerun setup."
    return
  fi
  [ "$content" = "$ip" ] && [ "$proxied" = "true" ] || die "Existing A record for $host is not recorded as Agent-owned and does not match this server. Refusing to modify or adopt it automatically. Use another hostname or update/remove that record manually, then rerun setup."
  CF_RESULT_DNS_OWNED=false
}

cf_reconcile_origin_rule(){
  local zone_id="$1" host="$2" port="$3" owned_ruleset="${4:-}" owned_rule="${5:-}" owned_fingerprint="${6:-}"
  local list ruleset_id rule_ref ref_match rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint
  CF_RESULT_ORIGIN_RULESET_ID=""; CF_RESULT_ORIGIN_RULE_ID=""; CF_RESULT_ORIGIN_ACTION=""; CF_RESULT_ORIGIN_FINGERPRINT=""
  rule_ref="ai_server_agent_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" '{ref:$ref,description:"AI Server Agent origin port",expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_request_origin") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    marker="$(cf_new_ownership_marker)"; ruleset_desc="Hostname-scoped origin routing managed by AI Server Agent"
    pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" --arg desc "AI Server Agent origin port txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
    pending_fingerprint="$(cf_rule_intent_fingerprint <<<"$pending_rule_body")"
    create_body="$(jq -n --argjson rule "$pending_rule_body" --arg desc "$ruleset_desc" '{name:"AI Server Agent Origin Rules",description:$desc,kind:"zone",phase:"http_request_origin",rules:[$rule]}')"
    cf_set_pending_write origin-rule-create "$zone_id" "$host" "$rule_ref" http_request_origin "$marker" "$pending_fingerprint"
    res="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Cloudflare Origin Rules create response was not confirmed. Durable recovery will verify the exact marked rule representation before any cleanup."
    CF_RESULT_ORIGIN_RULESET_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.rules[0].id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=created
    [ -n "$CF_RESULT_ORIGIN_RULESET_ID" ] && [ -n "$CF_RESULT_ORIGIN_RULE_ID" ] || die "Cloudflare did not return the created Origin Rules IDs."
    rule="$(jq -ce '.result.rules[0] | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid created Origin Rule."
    CF_RESULT_ORIGIN_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
    ruleset_id="$CF_RESULT_ORIGIN_RULESET_ID"
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Origin Rules ruleset."
    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then
      rule="$(jq -c --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref)' <<<"$ruleset" | head -n1)"
      if [ -n "$rule" ]; then
        current_fingerprint="$(cf_rule_fingerprint <<<"$rule")"
        [ -z "$owned_fingerprint" ] || [ "$owned_fingerprint" = "$current_fingerprint" ] || die "The recorded Agent-owned Origin Rule changed outside this transaction. Refusing automatic mutation; resolve the Cloudflare drift explicitly."
        jq -e --argjson expected "$rule_body" '.ref==$expected.ref and .expression==$expected.expression and .action==$expected.action and .action_parameters==$expected.action_parameters and .enabled==$expected.enabled' >/dev/null <<<"$rule" || die "The Agent-owned Origin Rule no longer matches the requested canonical state. Refusing an unconditional in-place update; clean up or reconcile it explicitly, then rerun setup."
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$owned_rule"; CF_RESULT_ORIGIN_FINGERPRINT="$current_fingerprint"
      elif [ -n "$ref_match" ]; then
        die "An Origin Rule uses the Agent ref but is not the rule recorded as Agent-owned. Refusing to adopt or overwrite it."
      else
        marker="$(cf_new_ownership_marker)"
        pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" --arg desc "AI Server Agent origin port txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
        pending_fingerprint="$(cf_rule_intent_fingerprint <<<"$pending_rule_body")"
        cf_set_pending_write origin-rule-create "$zone_id" "$host" "$rule_ref" http_request_origin "$marker" "$pending_fingerprint"
        res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$pending_rule_body")" || die "Cloudflare Origin Rule recreate response was not confirmed. Durable recovery will verify the exact marked rule representation before any cleanup."
        CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=created
        rule="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid recreated Origin Rule."
        CF_RESULT_ORIGIN_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Origin Rule already uses the Agent ref. Refusing to adopt or overwrite it."
      marker="$(cf_new_ownership_marker)"
      pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --argjson port "$port" --arg desc "AI Server Agent origin port txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"route",action_parameters:{origin:{port:$port}},enabled:true}')"
      pending_fingerprint="$(cf_rule_intent_fingerprint <<<"$pending_rule_body")"
      cf_set_pending_write origin-rule-create "$zone_id" "$host" "$rule_ref" http_request_origin "$marker" "$pending_fingerprint"
      res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$pending_rule_body")" || die "Cloudflare Origin Rule create response was not confirmed. Durable recovery will verify the exact marked rule representation before any cleanup."
      CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_ORIGIN_ACTION=created
      rule="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid created Origin Rule."
      CF_RESULT_ORIGIN_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare Origin Rule."
  if [ -n "$CF_PENDING_MARKER" ]; then
    rule="$(jq -c --arg ref "$rule_ref" --arg marker "$CF_PENDING_MARKER" '.result.rules[]? | select(.ref==$ref and ((.description // "") | endswith(" txn:"+$marker)))' <<<"$ruleset" | head -n1)"
  else
    rule="$(jq -c --arg id "$CF_RESULT_ORIGIN_RULE_ID" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref)' <<<"$ruleset" | head -n1)"
  fi
  [ -n "$rule" ] || die "Cloudflare Origin Rule was not reconciled cleanly."
  verify_id="$(jq -r '.id // empty' <<<"$rule")"
  [ -z "$CF_RESULT_ORIGIN_RULE_ID" ] || [ "$CF_RESULT_ORIGIN_RULE_ID" = "$verify_id" ] || die "Cloudflare Origin Rule verification returned an unexpected rule ID."
  current_fingerprint="$(cf_rule_fingerprint <<<"$rule")"
  [ -z "$CF_RESULT_ORIGIN_FINGERPRINT" ] || [ "$CF_RESULT_ORIGIN_FINGERPRINT" = "$current_fingerprint" ] || die "Cloudflare Origin Rule changed during reconciliation. Refusing to checkpoint ambiguous ownership."
  CF_RESULT_ORIGIN_RULESET_ID="$ruleset_id"; CF_RESULT_ORIGIN_RULE_ID="$verify_id"; CF_RESULT_ORIGIN_FINGERPRINT="$current_fingerprint"
}

cf_reconcile_ssl_config_rule(){
  local zone_id="$1" host="$2" owned_ruleset="${3:-}" owned_rule="${4:-}" owned_fingerprint="${5:-}"
  local list ruleset_id rule_ref ref_match rule_body create_body res ruleset marker pending_rule_body ruleset_desc verify_id rule current_fingerprint pending_fingerprint
  CF_RESULT_SSL_RULESET_ID=""; CF_RESULT_SSL_RULE_ID=""; CF_RESULT_SSL_ACTION=""; CF_RESULT_SSL_FINGERPRINT=""
  rule_ref="ai_server_agent_ssl_$(printf '%s' "$host" | sha256sum | cut -c1-16)"
  rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" '{ref:$ref,description:"AI Server Agent strict SSL",expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
  list="$(cf_api GET "/zones/$zone_id/rulesets?per_page=100")" || die "Cloudflare ruleset lookup failed."
  ruleset_id="$(jq -r '.result[]? | select(.kind=="zone" and .phase=="http_config_settings") | .id' <<<"$list" | head -n1)"
  if [ -z "$ruleset_id" ]; then
    marker="$(cf_new_ownership_marker)"; ruleset_desc="Hostname-scoped configuration managed by AI Server Agent"
    pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --arg desc "AI Server Agent strict SSL txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
    pending_fingerprint="$(cf_rule_intent_fingerprint <<<"$pending_rule_body")"
    create_body="$(jq -n --argjson rule "$pending_rule_body" --arg desc "$ruleset_desc" '{name:"AI Server Agent Configuration Rules",description:$desc,kind:"zone",phase:"http_config_settings",rules:[$rule]}')"
    cf_set_pending_write ssl-rule-create "$zone_id" "$host" "$rule_ref" http_config_settings "$marker" "$pending_fingerprint"
    res="$(cf_api POST "/zones/$zone_id/rulesets" "$create_body")" || die "Cloudflare Configuration Rules create response was not confirmed. Durable recovery will verify the exact marked rule representation before any cleanup."
    CF_RESULT_SSL_RULESET_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.rules[0].id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=created
    [ -n "$CF_RESULT_SSL_RULESET_ID" ] && [ -n "$CF_RESULT_SSL_RULE_ID" ] || die "Cloudflare did not return the created Configuration Rules IDs."
    rule="$(jq -ce '.result.rules[0] | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid created Configuration Rule."
    CF_RESULT_SSL_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
    ruleset_id="$CF_RESULT_SSL_RULESET_ID"
  else
    ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not read Cloudflare Configuration Rules ruleset."
    ref_match="$(jq -r --arg ref "$rule_ref" '.result.rules[]? | select(.ref==$ref) | .id' <<<"$ruleset" | head -n1)"
    if [ -n "$owned_rule" ] && [ "$owned_ruleset" = "$ruleset_id" ]; then
      rule="$(jq -c --arg id "$owned_rule" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref)' <<<"$ruleset" | head -n1)"
      if [ -n "$rule" ]; then
        current_fingerprint="$(cf_rule_fingerprint <<<"$rule")"
        [ -z "$owned_fingerprint" ] || [ "$owned_fingerprint" = "$current_fingerprint" ] || die "The recorded Agent-owned Configuration Rule changed outside this transaction. Refusing automatic mutation; resolve the Cloudflare drift explicitly."
        jq -e --argjson expected "$rule_body" '.ref==$expected.ref and .expression==$expected.expression and .action==$expected.action and .action_parameters==$expected.action_parameters and .enabled==$expected.enabled' >/dev/null <<<"$rule" || die "The Agent-owned Configuration Rule no longer matches strict hostname-scoped canonical state. Refusing an unconditional in-place update; clean up or reconcile it explicitly, then rerun setup."
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$owned_rule"; CF_RESULT_SSL_FINGERPRINT="$current_fingerprint"
      elif [ -n "$ref_match" ]; then
        die "A Configuration Rule uses the Agent SSL ref but is not the rule recorded as Agent-owned. Refusing to adopt or overwrite it."
      else
        marker="$(cf_new_ownership_marker)"
        pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --arg desc "AI Server Agent strict SSL txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
        pending_fingerprint="$(cf_rule_intent_fingerprint <<<"$pending_rule_body")"
        cf_set_pending_write ssl-rule-create "$zone_id" "$host" "$rule_ref" http_config_settings "$marker" "$pending_fingerprint"
        res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$pending_rule_body")" || die "Cloudflare Configuration Rule recreate response was not confirmed. Durable recovery will verify the exact marked rule representation before any cleanup."
        CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=created
        rule="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid recreated Configuration Rule."
        CF_RESULT_SSL_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
      fi
    else
      [ -z "$ref_match" ] || die "An unowned Configuration Rule already uses the Agent SSL ref. Refusing to adopt or overwrite it."
      marker="$(cf_new_ownership_marker)"
      pending_rule_body="$(jq -n --arg ref "$rule_ref" --arg host "$host" --arg desc "AI Server Agent strict SSL txn:$marker" '{ref:$ref,description:$desc,expression:("http.host eq \""+$host+"\""),action:"set_config",action_parameters:{ssl:"strict"},enabled:true}')"
      pending_fingerprint="$(cf_rule_intent_fingerprint <<<"$pending_rule_body")"
      cf_set_pending_write ssl-rule-create "$zone_id" "$host" "$rule_ref" http_config_settings "$marker" "$pending_fingerprint"
      res="$(cf_api POST "/zones/$zone_id/rulesets/$ruleset_id/rules" "$pending_rule_body")" || die "Cloudflare Configuration Rule create response was not confirmed. Durable recovery will verify the exact marked rule representation before any cleanup."
      CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$(jq -r '.result.id // empty' <<<"$res")"; CF_RESULT_SSL_ACTION=created
      rule="$(jq -ce '.result | select(type=="object")' <<<"$res")" || die "Cloudflare returned an invalid created Configuration Rule."
      CF_RESULT_SSL_FINGERPRINT="$(cf_rule_fingerprint <<<"$rule")"
    fi
  fi
  ruleset="$(cf_api GET "/zones/$zone_id/rulesets/$ruleset_id")" || die "Could not verify Cloudflare strict SSL Configuration Rule."
  if [ -n "$CF_PENDING_MARKER" ]; then
    rule="$(jq -c --arg ref "$rule_ref" --arg marker "$CF_PENDING_MARKER" '.result.rules[]? | select(.ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict" and ((.description // "") | endswith(" txn:"+$marker)))' <<<"$ruleset" | head -n1)"
  else
    rule="$(jq -c --arg id "$CF_RESULT_SSL_RULE_ID" --arg ref "$rule_ref" '.result.rules[]? | select(.id==$id and .ref==$ref and .action=="set_config" and .action_parameters.ssl=="strict")' <<<"$ruleset" | head -n1)"
  fi
  [ -n "$rule" ] || die "Cloudflare strict SSL Configuration Rule was not reconciled cleanly."
  verify_id="$(jq -r '.id // empty' <<<"$rule")"
  [ -z "$CF_RESULT_SSL_RULE_ID" ] || [ "$CF_RESULT_SSL_RULE_ID" = "$verify_id" ] || die "Cloudflare Configuration Rule verification returned an unexpected rule ID."
  current_fingerprint="$(cf_rule_fingerprint <<<"$rule")"
  [ -z "$CF_RESULT_SSL_FINGERPRINT" ] || [ "$CF_RESULT_SSL_FINGERPRINT" = "$current_fingerprint" ] || die "Cloudflare Configuration Rule changed during reconciliation. Refusing to checkpoint ambiguous ownership."
  CF_RESULT_SSL_RULESET_ID="$ruleset_id"; CF_RESULT_SSL_RULE_ID="$verify_id"; CF_RESULT_SSL_FINGERPRINT="$current_fingerprint"
}

save_cloudflare_state(){
  local host="$1" port="$2" zone_id="$3" zone_name="$4" dns_id="$5" dns_owned="$6" origin_ruleset_id="$7" origin_rule_id="$8" ssl_ruleset_id="$9" ssl_rule_id="${10}" cert_id="${11}" dns_fingerprint="${12}" origin_fingerprint="${13}" ssl_fingerprint="${14}" tmp old
  tmp="$(mktemp)"
  if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --argjson port "$port" --arg zone_id "$zone_id" --arg zone_name "$zone_name" --arg dns_id "$dns_id" --argjson dns_owned "$dns_owned" --arg dns_fingerprint "$dns_fingerprint" --arg origin_ruleset_id "$origin_ruleset_id" --arg origin_rule_id "$origin_rule_id" --arg origin_fingerprint "$origin_fingerprint" --arg ssl_ruleset_id "$ssl_ruleset_id" --arg ssl_rule_id "$ssl_rule_id" --arg ssl_fingerprint "$ssl_fingerprint" --arg cert_id "$cert_id" \
    '.active_provider="cloudflare" | .hostname=$host | .port=$port | .cloudflare={zone_id:$zone_id,zone_name:$zone_name,dns_record_id:$dns_id,dns_record_owned:$dns_owned,dns_record_fingerprint:$dns_fingerprint,origin_ruleset_id:$origin_ruleset_id,origin_rule_id:$origin_rule_id,origin_rule_fingerprint:$origin_fingerprint,ssl_config_ruleset_id:$ssl_ruleset_id,ssl_config_rule_id:$ssl_rule_id,ssl_config_rule_fingerprint:$ssl_fingerprint,origin_certificate_id:$cert_id}' <<<"$old" > "$tmp"
  atomic_install_file "$tmp" "$MANAGED_STATE" root "$AGENT_USER" 0640 || { rm -f "$tmp"; return 1; }; rm -f "$tmp"
}

save_previous_cloudflare_state(){
  local host="$1" zone_id="$2" dns_id="$3" dns_owned="$4" origin_ruleset_id="$5" origin_rule_id="$6" ssl_ruleset_id="$7" ssl_rule_id="$8" cert_id="$9" dns_fingerprint="${10:-}" origin_fingerprint="${11:-}" ssl_fingerprint="${12:-}" tmp old
  [ -n "$zone_id" ] || return 0
  tmp="$(mktemp)"
  if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --arg zone_id "$zone_id" --arg dns_id "$dns_id" --argjson dns_owned "$dns_owned" --arg dns_fingerprint "$dns_fingerprint" --arg origin_ruleset_id "$origin_ruleset_id" --arg origin_rule_id "$origin_rule_id" --arg origin_fingerprint "$origin_fingerprint" --arg ssl_ruleset_id "$ssl_ruleset_id" --arg ssl_rule_id "$ssl_rule_id" --arg ssl_fingerprint "$ssl_fingerprint" --arg cert_id "$cert_id" \
    '.cloudflare_previous={hostname:$host,zone_id:$zone_id,dns_record_id:$dns_id,dns_record_owned:$dns_owned,dns_record_fingerprint:$dns_fingerprint,origin_ruleset_id:$origin_ruleset_id,origin_rule_id:$origin_rule_id,origin_rule_fingerprint:$origin_fingerprint,ssl_config_ruleset_id:$ssl_ruleset_id,ssl_config_rule_id:$ssl_rule_id,ssl_config_rule_fingerprint:$ssl_fingerprint,origin_certificate_id:$cert_id}' <<<"$old" > "$tmp"
  atomic_install_file "$tmp" "$MANAGED_STATE" root "$AGENT_USER" 0640 || { rm -f "$tmp"; return 1; }; rm -f "$tmp"
}

clear_previous_cloudflare_state(){
  local tmp old
  [ -s "$MANAGED_STATE" ] || return 0
  tmp="$(mktemp)"; old="$(cat "$MANAGED_STATE")"
  jq 'del(.cloudflare_previous)' <<<"$old" > "$tmp"
  atomic_install_file "$tmp" "$MANAGED_STATE" root "$AGENT_USER" 0640 || { rm -f "$tmp"; return 1; }; rm -f "$tmp"
}

save_previous_cloudflare_certificate(){
  local host="$1" cert_id="$2" tmp old
  [ -n "$cert_id" ] || return 0
  tmp="$(mktemp)"; if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --arg cert_id "$cert_id" '.cloudflare_previous_certificate={hostname:$host,origin_certificate_id:$cert_id}' <<<"$old" > "$tmp"
  atomic_install_file "$tmp" "$MANAGED_STATE" root "$AGENT_USER" 0640 || { rm -f "$tmp"; return 1; }; rm -f "$tmp"
}

clear_previous_cloudflare_certificate(){
  local tmp old
  [ -s "$MANAGED_STATE" ] || return 0
  tmp="$(mktemp)"; old="$(cat "$MANAGED_STATE")"
  jq 'del(.cloudflare_previous_certificate)' <<<"$old" > "$tmp"
  atomic_install_file "$tmp" "$MANAGED_STATE" root "$AGENT_USER" 0640 || { rm -f "$tmp"; return 1; }; rm -f "$tmp"
}

atomic_install_file(){
  local src="$1" dest="$2" owner="$3" group="$4" mode="$5" dir tmp
  dir="$(dirname "$dest")"
  tmp="$(mktemp "$dir/.ai-server-agent-atomic.XXXXXX")" || return 1
  install -o "$owner" -g "$group" -m "$mode" "$src" "$tmp" || { rm -f "$tmp"; return 1; }
  mv -f "$tmp" "$dest" || { rm -f "$tmp"; return 1; }
}

prepare_cloudflare_local_backup(){
  acquire_management_lock
  local tmp managed_existed=false key_existed=false csr_existed=false crt_existed=false f
  install -d -o root -g root -m 0700 "$CONTROL_DIR" || return 1
  [ ! -L "$CONFIG_FILE" ] && [ -f "$CONFIG_FILE" ] || return 1
  [ ! -L "$CF_TXN_BACKUP_DIR" ] || return 1
  rm -rf -- "$CF_TXN_BACKUP_DIR"
  tmp="$(mktemp -d "$CONTROL_DIR/.cloudflare-backup.XXXXXX")" || return 1
  chmod 0700 "$tmp" || { rm -rf "$tmp"; return 1; }
  cp -p -- "$CONFIG_FILE" "$tmp/config.before" || { rm -rf "$tmp"; return 1; }
  if [ -e "$MANAGED_STATE" ]; then
    [ ! -L "$MANAGED_STATE" ] && [ -f "$MANAGED_STATE" ] || { rm -rf "$tmp"; return 1; }
    cp -p -- "$MANAGED_STATE" "$tmp/managed.before" || { rm -rf "$tmp"; return 1; }
    managed_existed=true
  fi
  for f in origin.key origin.csr origin.crt; do
    if [ -e "$TLS_DIR/$f" ]; then
      [ ! -L "$TLS_DIR/$f" ] && [ -f "$TLS_DIR/$f" ] || { rm -rf "$tmp"; return 1; }
      cp -p -- "$TLS_DIR/$f" "$tmp/$f.before" || { rm -rf "$tmp"; return 1; }
      case "$f" in origin.key) key_existed=true ;; origin.csr) csr_existed=true ;; origin.crt) crt_existed=true ;; esac
    fi
  done
  jq -n --argjson managed "$managed_existed" --argjson key "$key_existed" --argjson csr "$csr_existed" --argjson crt "$crt_existed" \
    '{version:1,managed_existed:$managed,tls_key_existed:$key,tls_csr_existed:$csr,tls_crt_existed:$crt}' > "$tmp/manifest.json" || { rm -rf "$tmp"; return 1; }
  chown -R root:root "$tmp" || { rm -rf "$tmp"; return 1; }
  chmod 0700 "$tmp" || { rm -rf "$tmp"; return 1; }
  find "$tmp" -maxdepth 1 -type f -exec chmod 0600 {} + || { rm -rf "$tmp"; return 1; }
  find "$tmp" -maxdepth 1 -type f -exec sync -f {} \; || { rm -rf "$tmp"; return 1; }
  mv "$tmp" "$CF_TXN_BACKUP_DIR" || { rm -rf "$tmp"; return 1; }
  sync -f "$CONTROL_DIR" || return 1
  CF_TXN_BACKUP_READY=true
}

validate_cloudflare_local_backup(){
  local meta
  [ -d "$CF_TXN_BACKUP_DIR" ] && [ ! -L "$CF_TXN_BACKUP_DIR" ] || return 1
  [ "$(stat -c '%u:%g:%a' "$CF_TXN_BACKUP_DIR" 2>/dev/null)" = "0:0:700" ] || return 1
  meta="$CF_TXN_BACKUP_DIR/manifest.json"
  [ -f "$meta" ] && [ ! -L "$meta" ] || return 1
  jq -e 'type=="object" and .version==1 and (keys|sort)==(["managed_existed","tls_crt_existed","tls_csr_existed","tls_key_existed","version"]|sort) and (.managed_existed|type)=="boolean" and (.tls_key_existed|type)=="boolean" and (.tls_csr_existed|type)=="boolean" and (.tls_crt_existed|type)=="boolean"' "$meta" >/dev/null 2>&1 || return 1
  [ -f "$CF_TXN_BACKUP_DIR/config.before" ] && [ ! -L "$CF_TXN_BACKUP_DIR/config.before" ] || return 1
}

restore_cloudflare_local_backup(){
  local managed_existed key_existed csr_existed crt_existed
  validate_cloudflare_local_backup || return 1
  managed_existed="$(jq -r '.managed_existed' "$CF_TXN_BACKUP_DIR/manifest.json")"
  key_existed="$(jq -r '.tls_key_existed' "$CF_TXN_BACKUP_DIR/manifest.json")"
  csr_existed="$(jq -r '.tls_csr_existed' "$CF_TXN_BACKUP_DIR/manifest.json")"
  crt_existed="$(jq -r '.tls_crt_existed' "$CF_TXN_BACKUP_DIR/manifest.json")"
  atomic_install_file "$CF_TXN_BACKUP_DIR/config.before" "$CONFIG_FILE" root "$AGENT_USER" 0640 || return 1
  if [ "$managed_existed" = true ]; then
    [ -f "$CF_TXN_BACKUP_DIR/managed.before" ] && [ ! -L "$CF_TXN_BACKUP_DIR/managed.before" ] || return 1
    atomic_install_file "$CF_TXN_BACKUP_DIR/managed.before" "$MANAGED_STATE" root "$AGENT_USER" 0640 || return 1
  else
    rm -f -- "$MANAGED_STATE" || return 1
  fi
  install -d -o root -g "$AGENT_USER" -m 0750 "$TLS_DIR" || return 1
  if [ "$key_existed" = true ]; then atomic_install_file "$CF_TXN_BACKUP_DIR/origin.key.before" "$TLS_DIR/origin.key" root "$AGENT_USER" 0640 || return 1; else rm -f -- "$TLS_DIR/origin.key" || return 1; fi
  if [ "$csr_existed" = true ]; then atomic_install_file "$CF_TXN_BACKUP_DIR/origin.csr.before" "$TLS_DIR/origin.csr" root root 0644 || return 1; else rm -f -- "$TLS_DIR/origin.csr" || return 1; fi
  if [ "$crt_existed" = true ]; then atomic_install_file "$CF_TXN_BACKUP_DIR/origin.crt.before" "$TLS_DIR/origin.crt" root "$AGENT_USER" 0644 || return 1; else rm -f -- "$TLS_DIR/origin.crt" || return 1; fi
  systemctl restart ai-server-agent-executor.service ai-server-agent.service >/dev/null 2>&1 || return 1
}

save_cloudflare_transaction_state(){
  local host="$1" zone_id="$2" dns_id="$3" dns_action="$4" origin_ruleset="$5" origin_rule="$6" origin_action="$7" ssl_ruleset="$8" ssl_rule="$9" ssl_action="${10}" cert_id="${11}" dns_fingerprint="${12:-}" origin_fingerprint="${13:-}" ssl_fingerprint="${14:-}" tmp
  install -d -o root -g root -m 0700 "$CONTROL_DIR" || return 1
  tmp="$(mktemp "$CONTROL_DIR/.cloudflare-transaction.XXXXXX")" || return 1
  chmod 0600 "$tmp"
  jq -n \
    --arg phase "$CF_TXN_PHASE" --argjson backup_ready "$CF_TXN_BACKUP_READY" \
    --arg host "$host" --arg zone_id "$zone_id" --arg dns_id "$dns_id" --arg dns_action "$dns_action" --arg dns_fingerprint "$dns_fingerprint" \
    --arg origin_ruleset "$origin_ruleset" --arg origin_rule "$origin_rule" --arg origin_action "$origin_action" --arg origin_fingerprint "$origin_fingerprint" \
    --arg ssl_ruleset "$ssl_ruleset" --arg ssl_rule "$ssl_rule" --arg ssl_action "$ssl_action" --arg ssl_fingerprint "$ssl_fingerprint" --arg cert_id "$cert_id" \
    --arg pending_kind "$CF_PENDING_KIND" --arg pending_zone "$CF_PENDING_ZONE" --arg pending_host "$CF_PENDING_HOST" --arg pending_value "$CF_PENDING_VALUE" --arg pending_phase "$CF_PENDING_PHASE" --arg pending_marker "$CF_PENDING_MARKER" --arg pending_fingerprint "$CF_PENDING_FINGERPRINT" \
    --arg commit_host "$CF_COMMIT_HOST" --arg commit_port "$CF_COMMIT_PORT" --arg commit_zone_id "$CF_COMMIT_ZONE_ID" --arg commit_zone_name "$CF_COMMIT_ZONE_NAME" --arg commit_dns_id "$CF_COMMIT_DNS_ID" --argjson commit_dns_owned "$CF_COMMIT_DNS_OWNED" \
    --arg commit_origin_ruleset "$CF_COMMIT_ORIGIN_RULESET_ID" --arg commit_origin_rule "$CF_COMMIT_ORIGIN_RULE_ID" --arg commit_ssl_ruleset "$CF_COMMIT_SSL_RULESET_ID" --arg commit_ssl_rule "$CF_COMMIT_SSL_RULE_ID" --arg commit_cert "$CF_COMMIT_CERT_ID" \
    --arg commit_dns_fingerprint "$CF_COMMIT_DNS_FINGERPRINT" --arg commit_origin_fingerprint "$CF_COMMIT_ORIGIN_FINGERPRINT" --arg commit_ssl_fingerprint "$CF_COMMIT_SSL_FINGERPRINT" \
    '{version:3,phase:$phase,backup_ready:$backup_ready,hostname:$host,zone_id:$zone_id,dns:{id:$dns_id,action:$dns_action,fingerprint:$dns_fingerprint},origin:{ruleset_id:$origin_ruleset,rule_id:$origin_rule,action:$origin_action,fingerprint:$origin_fingerprint},ssl:{ruleset_id:$ssl_ruleset,rule_id:$ssl_rule,action:$ssl_action,fingerprint:$ssl_fingerprint},certificate_id:$cert_id,pending:{kind:$pending_kind,zone_id:$pending_zone,hostname:$pending_host,value:$pending_value,phase:$pending_phase,marker:$pending_marker,fingerprint:$pending_fingerprint},commit:{hostname:$commit_host,port:$commit_port,zone_id:$commit_zone_id,zone_name:$commit_zone_name,dns_id:$commit_dns_id,dns_owned:$commit_dns_owned,dns_fingerprint:$commit_dns_fingerprint,origin_ruleset_id:$commit_origin_ruleset,origin_rule_id:$commit_origin_rule,origin_rule_fingerprint:$commit_origin_fingerprint,ssl_ruleset_id:$commit_ssl_ruleset,ssl_rule_id:$commit_ssl_rule,ssl_rule_fingerprint:$commit_ssl_fingerprint,certificate_id:$commit_cert}}' > "$tmp" || { rm -f "$tmp"; return 1; }
  chown root:root "$tmp" || { rm -f "$tmp"; return 1; }
  chmod 0600 "$tmp" || { rm -f "$tmp"; return 1; }
  sync -f "$tmp" || { rm -f "$tmp"; return 1; }
  mv -f "$tmp" "$CF_TXN_STATE" || { rm -f "$tmp"; return 1; }
  sync -f "$CONTROL_DIR" || return 1
}

validate_cloudflare_transaction_state(){
  local f="${1:-$CF_TXN_STATE}" size
  [ -f "$f" ] && [ ! -L "$f" ] || return 1
  [ "$(stat -c '%u:%g:%a' "$f" 2>/dev/null)" = "0:0:600" ] || return 1
  size="$(stat -c '%s' "$f" 2>/dev/null || printf 999999)"; [ "$size" -gt 0 ] && [ "$size" -le 131072 ] || return 1
  jq -e '
    def hex64: type=="string" and test("^[0-9a-f]{64}$");
    def marker: type=="string" and test("^[0-9a-fA-F]{16,128}$");
    def empty_commit:
      .dns_owned==false and .hostname=="" and .port=="" and .zone_id=="" and .zone_name=="" and
      .dns_id=="" and .dns_fingerprint=="" and .origin_ruleset_id=="" and .origin_rule_id=="" and
      .origin_rule_fingerprint=="" and .ssl_ruleset_id=="" and .ssl_rule_id=="" and
      .ssl_rule_fingerprint=="" and .certificate_id=="";
    type=="object" and .version==3 and
    (keys|sort)==(["backup_ready","certificate_id","commit","dns","hostname","origin","pending","phase","ssl","version","zone_id"]|sort) and
    (.phase=="prepared" or .phase=="applying" or .phase=="committing" or .phase=="committed" or .phase=="rolling_back" or .phase=="rolled_back") and
    .backup_ready==true and (.hostname|type)=="string" and (.hostname|length)>0 and (.zone_id|type)=="string" and (.zone_id|length)>0 and (.certificate_id|type)=="string" and
    (.dns|type)=="object" and (.dns|keys|sort)==(["action","fingerprint","id"]|sort) and
    (.dns.id|type)=="string" and (.dns.action|type)=="string" and (.dns.fingerprint|type)=="string" and (.dns.action=="" or .dns.action=="created") and
    (if .dns.id=="" then .dns.fingerprint=="" else (.dns.fingerprint|hex64) end) and
    (if .dns.action=="created" then (.dns.id|length)>0 else true end) and
    (.origin|type)=="object" and (.origin|keys|sort)==(["action","fingerprint","rule_id","ruleset_id"]|sort) and
    (.origin.ruleset_id|type)=="string" and (.origin.rule_id|type)=="string" and (.origin.action|type)=="string" and (.origin.fingerprint|type)=="string" and (.origin.action=="" or .origin.action=="created") and
    (if .origin.rule_id=="" then .origin.fingerprint=="" else ((.origin.ruleset_id|length)>0 and (.origin.fingerprint|hex64)) end) and
    (if .origin.action=="created" then ((.origin.ruleset_id|length)>0 and (.origin.rule_id|length)>0) else true end) and
    (.ssl|type)=="object" and (.ssl|keys|sort)==(["action","fingerprint","rule_id","ruleset_id"]|sort) and
    (.ssl.ruleset_id|type)=="string" and (.ssl.rule_id|type)=="string" and (.ssl.action|type)=="string" and (.ssl.fingerprint|type)=="string" and (.ssl.action=="" or .ssl.action=="created") and
    (if .ssl.rule_id=="" then .ssl.fingerprint=="" else ((.ssl.ruleset_id|length)>0 and (.ssl.fingerprint|hex64)) end) and
    (if .ssl.action=="created" then ((.ssl.ruleset_id|length)>0 and (.ssl.rule_id|length)>0) else true end) and
    (.pending|type)=="object" and (.pending|keys|sort)==(["fingerprint","hostname","kind","marker","phase","value","zone_id"]|sort) and
    (.pending.kind|type)=="string" and (.pending.zone_id|type)=="string" and (.pending.hostname|type)=="string" and (.pending.value|type)=="string" and (.pending.phase|type)=="string" and (.pending.marker|type)=="string" and (.pending.fingerprint|type)=="string" and
    (.pending.kind=="" or .pending.kind=="origin-cert-create" or .pending.kind=="dns-create" or .pending.kind=="origin-rule-create" or .pending.kind=="ssl-rule-create") and
    (if .pending.kind=="" then
       (.pending.zone_id=="" and .pending.hostname=="" and .pending.value=="" and .pending.phase=="" and .pending.marker=="" and .pending.fingerprint=="")
     else
       ((.phase=="prepared" or .phase=="rolling_back") and .pending.zone_id==.zone_id and .pending.hostname==.hostname and (.pending.value|length)>0 and
        (if .pending.kind=="origin-cert-create" then
           (.pending.phase=="" and .pending.marker=="" and (.pending.fingerprint|hex64))
         elif .pending.kind=="dns-create" then
           (.pending.phase=="" and (.pending.marker|marker) and (.pending.fingerprint|hex64))
         elif .pending.kind=="origin-rule-create" then
           (.pending.phase=="http_request_origin" and (.pending.marker|marker) and (.pending.fingerprint|hex64))
         else
           (.pending.phase=="http_config_settings" and (.pending.marker|marker) and (.pending.fingerprint|hex64))
         end))
     end) and
    (.commit|type)=="object" and (.commit|keys|sort)==(["certificate_id","dns_fingerprint","dns_id","dns_owned","hostname","origin_rule_fingerprint","origin_rule_id","origin_ruleset_id","port","ssl_rule_fingerprint","ssl_rule_id","ssl_ruleset_id","zone_id","zone_name"]|sort) and
    (.commit.hostname|type)=="string" and (.commit.port|type)=="string" and (.commit.zone_id|type)=="string" and (.commit.zone_name|type)=="string" and (.commit.dns_id|type)=="string" and (.commit.dns_owned|type)=="boolean" and (.commit.dns_fingerprint|type)=="string" and (.commit.origin_ruleset_id|type)=="string" and (.commit.origin_rule_id|type)=="string" and (.commit.origin_rule_fingerprint|type)=="string" and (.commit.ssl_ruleset_id|type)=="string" and (.commit.ssl_rule_id|type)=="string" and (.commit.ssl_rule_fingerprint|type)=="string" and (.commit.certificate_id|type)=="string" and
    (if (.phase=="committing" or .phase=="committed") then
       (.pending.kind=="" and .commit.hostname==.hostname and .commit.zone_id==.zone_id and (.commit.port|test("^[0-9]+$")) and (.commit.zone_name|length)>0 and
        .commit.dns_id==.dns.id and .commit.dns_fingerprint==.dns.fingerprint and (.commit.dns_fingerprint|hex64) and
        .commit.origin_ruleset_id==.origin.ruleset_id and .commit.origin_rule_id==.origin.rule_id and .commit.origin_rule_fingerprint==.origin.fingerprint and (.commit.origin_rule_fingerprint|hex64) and
        .commit.ssl_ruleset_id==.ssl.ruleset_id and .commit.ssl_rule_id==.ssl.rule_id and .commit.ssl_rule_fingerprint==.ssl.fingerprint and (.commit.ssl_rule_fingerprint|hex64) and
        .commit.certificate_id==.certificate_id and (.commit.certificate_id|length)>0)
     else
       (.commit|empty_commit)
     end) and
    (if .phase=="applying" then .pending.kind=="" else true end) and
    (if .phase=="rolled_back" then
       (.pending.kind=="" and .dns.id=="" and .dns.action=="" and .dns.fingerprint=="" and
        .origin.ruleset_id=="" and .origin.rule_id=="" and .origin.action=="" and .origin.fingerprint=="" and
        .ssl.ruleset_id=="" and .ssl.rule_id=="" and .ssl.action=="" and .ssl.fingerprint=="" and .certificate_id=="")
     else true end)
  ' "$f" >/dev/null 2>&1
}

load_cloudflare_transaction_globals(){
  CF_TXN_PHASE="$(jq -r '.phase' "$CF_TXN_STATE")"; CF_TXN_BACKUP_READY="$(jq -r '.backup_ready' "$CF_TXN_STATE")"
  CF_PENDING_KIND="$(jq -r '.pending.kind' "$CF_TXN_STATE")"; CF_PENDING_ZONE="$(jq -r '.pending.zone_id' "$CF_TXN_STATE")"; CF_PENDING_HOST="$(jq -r '.pending.hostname' "$CF_TXN_STATE")"; CF_PENDING_VALUE="$(jq -r '.pending.value' "$CF_TXN_STATE")"; CF_PENDING_PHASE="$(jq -r '.pending.phase' "$CF_TXN_STATE")"; CF_PENDING_MARKER="$(jq -r '.pending.marker' "$CF_TXN_STATE")"; CF_PENDING_FINGERPRINT="$(jq -r '.pending.fingerprint' "$CF_TXN_STATE")"
  CF_COMMIT_HOST="$(jq -r '.commit.hostname' "$CF_TXN_STATE")"; CF_COMMIT_PORT="$(jq -r '.commit.port' "$CF_TXN_STATE")"; CF_COMMIT_ZONE_ID="$(jq -r '.commit.zone_id' "$CF_TXN_STATE")"; CF_COMMIT_ZONE_NAME="$(jq -r '.commit.zone_name' "$CF_TXN_STATE")"; CF_COMMIT_DNS_ID="$(jq -r '.commit.dns_id' "$CF_TXN_STATE")"; CF_COMMIT_DNS_OWNED="$(jq -r '.commit.dns_owned' "$CF_TXN_STATE")"
  CF_COMMIT_ORIGIN_RULESET_ID="$(jq -r '.commit.origin_ruleset_id' "$CF_TXN_STATE")"; CF_COMMIT_ORIGIN_RULE_ID="$(jq -r '.commit.origin_rule_id' "$CF_TXN_STATE")"; CF_COMMIT_SSL_RULESET_ID="$(jq -r '.commit.ssl_ruleset_id' "$CF_TXN_STATE")"; CF_COMMIT_SSL_RULE_ID="$(jq -r '.commit.ssl_rule_id' "$CF_TXN_STATE")"; CF_COMMIT_CERT_ID="$(jq -r '.commit.certificate_id' "$CF_TXN_STATE")"
  CF_COMMIT_DNS_FINGERPRINT="$(jq -r '.commit.dns_fingerprint' "$CF_TXN_STATE")"; CF_COMMIT_ORIGIN_FINGERPRINT="$(jq -r '.commit.origin_rule_fingerprint' "$CF_TXN_STATE")"; CF_COMMIT_SSL_FINGERPRINT="$(jq -r '.commit.ssl_rule_fingerprint' "$CF_TXN_STATE")"
}

managed_state_matches_transaction_commit(){
  local expected_gid size
  [ -f "$MANAGED_STATE" ] && [ ! -L "$MANAGED_STATE" ] || return 1
  expected_gid="$(id -g "$AGENT_USER" 2>/dev/null)" || return 1
  [ "$(stat -c '%u:%g:%a' "$MANAGED_STATE" 2>/dev/null)" = "0:${expected_gid}:640" ] || return 1
  size="$(stat -c '%s' "$MANAGED_STATE" 2>/dev/null || printf 999999)"; [ "$size" -gt 0 ] && [ "$size" -le 131072 ] || return 1
  jq -e --arg host "$CF_COMMIT_HOST" --argjson port "$CF_COMMIT_PORT" --arg zone_id "$CF_COMMIT_ZONE_ID" --arg zone_name "$CF_COMMIT_ZONE_NAME" --arg dns_id "$CF_COMMIT_DNS_ID" --argjson dns_owned "$CF_COMMIT_DNS_OWNED" --arg dns_fingerprint "$CF_COMMIT_DNS_FINGERPRINT" --arg origin_ruleset "$CF_COMMIT_ORIGIN_RULESET_ID" --arg origin_rule "$CF_COMMIT_ORIGIN_RULE_ID" --arg origin_fingerprint "$CF_COMMIT_ORIGIN_FINGERPRINT" --arg ssl_ruleset "$CF_COMMIT_SSL_RULESET_ID" --arg ssl_rule "$CF_COMMIT_SSL_RULE_ID" --arg ssl_fingerprint "$CF_COMMIT_SSL_FINGERPRINT" --arg cert "$CF_COMMIT_CERT_ID" '
    .active_provider=="cloudflare" and .hostname==$host and .port==$port and .cloudflare.zone_id==$zone_id and .cloudflare.zone_name==$zone_name and .cloudflare.dns_record_id==$dns_id and .cloudflare.dns_record_owned==$dns_owned and .cloudflare.dns_record_fingerprint==$dns_fingerprint and .cloudflare.origin_ruleset_id==$origin_ruleset and .cloudflare.origin_rule_id==$origin_rule and .cloudflare.origin_rule_fingerprint==$origin_fingerprint and .cloudflare.ssl_config_ruleset_id==$ssl_ruleset and .cloudflare.ssl_config_rule_id==$ssl_rule and .cloudflare.ssl_config_rule_fingerprint==$ssl_fingerprint and .cloudflare.origin_certificate_id==$cert
  ' "$MANAGED_STATE" >/dev/null 2>&1
}

cf_reset_transaction_globals(){
  CF_TXN_PHASE=prepared
  CF_TXN_BACKUP_READY=false
  cf_clear_pending_write
  cf_clear_commit_intent
}

finalize_cloudflare_transaction_state(){
  [ ! -L "$CF_TXN_BACKUP_DIR" ] || return 1
  rm -rf -- "$CF_TXN_BACKUP_DIR" || return 1
  rm -f -- "$CF_TXN_STATE" || return 1
  sync -f "$CONTROL_DIR" || return 1
  cf_reset_transaction_globals
}

clear_cloudflare_transaction_state(){ finalize_cloudflare_transaction_state; }

save_local_state(){
  local port="$1" tmp old
  tmp="$(mktemp)"; if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --argjson port "$port" '.active_provider="local" | .port=$port' <<<"$old" > "$tmp"
  atomic_install_file "$tmp" "$MANAGED_STATE" root "$AGENT_USER" 0640 || { rm -f "$tmp"; return 1; }; rm -f "$tmp"
}

save_manual_state(){
  local host="$1" port="$2" tmp old
  tmp="$(mktemp)"; if [ -s "$MANAGED_STATE" ]; then old="$(cat "$MANAGED_STATE")"; else old='{}'; fi
  jq --arg host "$host" --argjson port "$port" '.active_provider="manual" | .hostname=$host | .port=$port' <<<"$old" > "$tmp"
  atomic_install_file "$tmp" "$MANAGED_STATE" root "$AGENT_USER" 0640 || { rm -f "$tmp"; return 1; }; rm -f "$tmp"
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
  local host="$1" zone_id="$2" dns_id="$3" dns_action="$4" origin_ruleset="$5" origin_rule="$6" origin_action="$7" ssl_ruleset="$8" ssl_rule="$9" ssl_action="${10}" cert_id="${11}" dns_fingerprint="${12:-}" origin_fingerprint="${13:-}" ssl_fingerprint="${14:-}"
  local keep_dns_id="" keep_dns_action="" keep_dns_fingerprint="" keep_origin_ruleset="" keep_origin_rule="" keep_origin_action="" keep_origin_fingerprint="" keep_ssl_ruleset="" keep_ssl_rule="" keep_ssl_action="" keep_ssl_fingerprint="" keep_cert_id="" failed=0
  if ! cf_recover_pending_write; then failed=1; fi
  case "$dns_action" in
    created) if [ -n "$dns_id" ] && ! cf_delete_dns_if_expected "$zone_id" "$dns_id" "$dns_fingerprint"; then keep_dns_id="$dns_id"; keep_dns_action=created; keep_dns_fingerprint="$dns_fingerprint"; failed=1; fi ;;
  esac
  case "$origin_action" in
    created) if [ -n "$origin_rule" ] && [ -n "$origin_ruleset" ] && ! cf_delete_rule_if_expected "$zone_id" "$origin_ruleset" "$origin_rule" "$origin_fingerprint"; then keep_origin_ruleset="$origin_ruleset"; keep_origin_rule="$origin_rule"; keep_origin_action=created; keep_origin_fingerprint="$origin_fingerprint"; failed=1; fi ;;
  esac
  case "$ssl_action" in
    created) if [ -n "$ssl_rule" ] && [ -n "$ssl_ruleset" ] && ! cf_delete_rule_if_expected "$zone_id" "$ssl_ruleset" "$ssl_rule" "$ssl_fingerprint"; then keep_ssl_ruleset="$ssl_ruleset"; keep_ssl_rule="$ssl_rule"; keep_ssl_action=created; keep_ssl_fingerprint="$ssl_fingerprint"; failed=1; fi ;;
  esac
  if [ -n "$cert_id" ] && ! cf_delete_owned "/certificates/$cert_id"; then keep_cert_id="$cert_id"; failed=1; fi
  if [ "$failed" -eq 0 ]; then return 0; fi
  if save_cloudflare_transaction_state "$host" "$zone_id" "$keep_dns_id" "$keep_dns_action" "$keep_origin_ruleset" "$keep_origin_rule" "$keep_origin_action" "$keep_ssl_ruleset" "$keep_ssl_rule" "$keep_ssl_action" "$keep_cert_id" "$keep_dns_fingerprint" "$keep_origin_fingerprint" "$keep_ssl_fingerprint"; then
    warn "Cloudflare rollback was incomplete. Exact remaining ownership/recovery state was preserved in $CF_TXN_STATE."
  else
    warn "CRITICAL: Cloudflare rollback was incomplete and the recovery journal could not be written to $CF_TXN_STATE. Do not continue Cloudflare setup until the host filesystem issue is repaired."
  fi
  return 1
}

recover_cloudflare_transaction(){
  acquire_management_lock
  { [ -e "$CF_TXN_STATE" ] || [ -L "$CF_TXN_STATE" ]; } || return 0
  validate_cloudflare_transaction_state "$CF_TXN_STATE" || { warn "Cloudflare recovery journal is malformed or has unsafe ownership/mode. It was left untouched."; return 1; }
  local host zone_id dns_id dns_action dns_fingerprint origin_ruleset origin_rule origin_action origin_fingerprint ssl_ruleset ssl_rule ssl_action ssl_fingerprint cert_id
  load_cloudflare_transaction_globals
  host="$(jq -r '.hostname' "$CF_TXN_STATE")"; zone_id="$(jq -r '.zone_id' "$CF_TXN_STATE")"
  dns_id="$(jq -r '.dns.id' "$CF_TXN_STATE")"; dns_action="$(jq -r '.dns.action' "$CF_TXN_STATE")"; dns_fingerprint="$(jq -r '.dns.fingerprint' "$CF_TXN_STATE")"
  origin_ruleset="$(jq -r '.origin.ruleset_id' "$CF_TXN_STATE")"; origin_rule="$(jq -r '.origin.rule_id' "$CF_TXN_STATE")"; origin_action="$(jq -r '.origin.action' "$CF_TXN_STATE")"; origin_fingerprint="$(jq -r '.origin.fingerprint' "$CF_TXN_STATE")"
  ssl_ruleset="$(jq -r '.ssl.ruleset_id' "$CF_TXN_STATE")"; ssl_rule="$(jq -r '.ssl.rule_id' "$CF_TXN_STATE")"; ssl_action="$(jq -r '.ssl.action' "$CF_TXN_STATE")"; ssl_fingerprint="$(jq -r '.ssl.fingerprint' "$CF_TXN_STATE")"; cert_id="$(jq -r '.certificate_id' "$CF_TXN_STATE")"

  case "$CF_TXN_PHASE" in
    committed)
      managed_state_matches_transaction_commit || { warn "Committed Cloudflare journal does not match the current trusted managed state. Journal and backup were left untouched."; return 1; }
      finalize_cloudflare_transaction_state
      return
      ;;
    rolled_back)
      finalize_cloudflare_transaction_state
      return
      ;;
    committing)
      if managed_state_matches_transaction_commit; then
        CF_TXN_PHASE=committed
        save_cloudflare_transaction_state "$host" "$zone_id" "$dns_id" "$dns_action" "$origin_ruleset" "$origin_rule" "$origin_action" "$ssl_ruleset" "$ssl_rule" "$ssl_action" "$cert_id" "$dns_fingerprint" "$origin_fingerprint" "$ssl_fingerprint" || return 1
        finalize_cloudflare_transaction_state
        return
      fi
      restore_cloudflare_local_backup || { warn "Could not restore the durable pre-transaction local state. Cloudflare resources were not mutated."; return 1; }
      ;;
    applying)
      restore_cloudflare_local_backup || { warn "Could not restore the durable pre-transaction local state. Cloudflare resources were not mutated."; return 1; }
      ;;
    prepared|rolling_back) ;;
    *) return 1 ;;
  esac

  if [ "$CF_TXN_PHASE" != "rolling_back" ]; then
    cf_clear_commit_intent
    CF_TXN_PHASE=rolling_back
    save_cloudflare_transaction_state "$host" "$zone_id" "$dns_id" "$dns_action" "$origin_ruleset" "$origin_rule" "$origin_action" "$ssl_ruleset" "$ssl_rule" "$ssl_action" "$cert_id" "$dns_fingerprint" "$origin_fingerprint" "$ssl_fingerprint" || return 1
  fi

  rollback_new_cf_resources "$host" "$zone_id" "$dns_id" "$dns_action" "$origin_ruleset" "$origin_rule" "$origin_action" "$ssl_ruleset" "$ssl_rule" "$ssl_action" "$cert_id" "$dns_fingerprint" "$origin_fingerprint" "$ssl_fingerprint" || return 1
  cf_clear_pending_write
  cf_clear_commit_intent
  CF_TXN_PHASE=rolled_back
  save_cloudflare_transaction_state "$host" "$zone_id" "" "" "" "" "" "" "" "" "" "" "" "" || return 1
  finalize_cloudflare_transaction_state
}

cleanup_old_cloudflare(){
  local old_host="$1" old_zone="$2" old_dns="$3" old_dns_owned="$4" old_origin_ruleset="$5" old_origin_rule="$6" old_ssl_ruleset="$7" old_ssl_rule="$8" old_cert="$9" new_host="${10}" new_cert="${11}" new_zone="${12}" old_dns_fingerprint="${13:-}" old_origin_fingerprint="${14:-}" old_ssl_fingerprint="${15:-}"
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
  if delete_recorded_cloudflare_resources "$old_zone" "$old_dns" "$old_dns_owned" "$old_origin_ruleset" "$old_origin_rule" "$old_ssl_ruleset" "$old_ssl_rule" "$old_cert" "$old_dns_fingerprint" "$old_origin_fingerprint" "$old_ssl_fingerprint"; then
    clear_previous_cloudflare_state
    log "Old Agent-managed Cloudflare resources were removed or already absent."
  else
    warn "Old Cloudflare cleanup was incomplete. Recorded ownership state was preserved for a safe retry."
  fi
}

configure_cloudflare(){ (
  set -Eeuo pipefail
  acquire_management_lock
  need_cmd jq; need_cmd openssl; need_cmd curl; need_cmd sha256sum
  old_host=""; old_zone=""; old_dns=""; old_dns_owned=""; old_dns_fingerprint=""; old_origin_ruleset=""; old_origin_rule=""; old_origin_fingerprint=""; old_ssl_ruleset=""; old_ssl_rule=""; old_ssl_fingerprint=""; old_cert=""; old_port=""; previous_zone=""; previous_cert=""
  host=""; port=""; zone_pair=""; zone_id=""; zone_name=""; ip=""; stage=""; cert_id=""; dns_id=""; dns_owned=false; dns_action=""; dns_fingerprint=""
  origin_ruleset_id=""; origin_rule_id=""; origin_action=""; origin_fingerprint=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""; ssl_fingerprint=""
  owned_dns_id=""; owned_origin_ruleset=""; owned_origin_rule=""; owned_ssl_ruleset=""; owned_ssl_rule=""
  old_host="$(managed_get '.hostname')"; old_zone="$(managed_get '.cloudflare.zone_id')"; old_dns="$(managed_get '.cloudflare.dns_record_id')"; old_dns_owned="$(managed_get '.cloudflare.dns_record_owned')"; old_dns_owned="${old_dns_owned:-false}"; old_dns_fingerprint="$(managed_get '.cloudflare.dns_record_fingerprint')"
  old_origin_ruleset="$(managed_get '.cloudflare.origin_ruleset_id')"; old_origin_rule="$(managed_get '.cloudflare.origin_rule_id')"; old_origin_fingerprint="$(managed_get '.cloudflare.origin_rule_fingerprint')"
  old_ssl_ruleset="$(managed_get '.cloudflare.ssl_config_ruleset_id')"; old_ssl_rule="$(managed_get '.cloudflare.ssl_config_rule_id')"; old_ssl_fingerprint="$(managed_get '.cloudflare.ssl_config_rule_fingerprint')"; old_cert="$(managed_get '.cloudflare.origin_certificate_id')"
  old_port="$(current_port)"; previous_zone="$(managed_get '.cloudflare_previous.zone_id')"; previous_cert="$(managed_get '.cloudflare_previous_certificate.origin_certificate_id')"
  [ ! -e "$CF_TXN_STATE" ] && [ ! -L "$CF_TXN_STATE" ] || die "A Cloudflare transaction/recovery journal exists. Run 'sudo ai-server-agent-manage cloudflare-cleanup' before configuring Cloudflare again."
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
  stage="$(mktemp -d)"; chmod 0700 "$stage"

  cloudflare_transaction_exit(){
    local rc=$?
    trap - EXIT
    set +e
    if [ "$rc" -ne 0 ] && { [ -e "$CF_TXN_STATE" ] || [ -L "$CF_TXN_STATE" ]; }; then
      recover_cloudflare_transaction || warn "Cloudflare recovery remains incomplete; the durable journal was preserved."
    fi
    rm -rf "$stage"
    CF_TOKEN=""
    return "$rc"
  }
  trap cloudflare_transaction_exit EXIT

  cert_id=""; dns_id=""; dns_owned=false; dns_action=""; dns_fingerprint=""; origin_ruleset_id=""; origin_rule_id=""; origin_action=""; origin_fingerprint=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""; ssl_fingerprint=""
  CF_TXN_PHASE=prepared
  prepare_cloudflare_local_backup || die "Could not create the durable root-only local rollback snapshot. No Cloudflare mutation was started."
  cf_checkpoint_transaction
  cf_issue_origin_cert "$zone_id" "$host" "$stage"; cert_id="$CF_RESULT_CERT_ID"; cf_finish_transaction_step; log "Fresh Origin CA certificate issued and verified."
  cf_reconcile_dns "$zone_id" "$host" "$ip" "$owned_dns_id" "$old_dns_fingerprint"; dns_id="$CF_RESULT_DNS_ID"; dns_owned="$CF_RESULT_DNS_OWNED"; dns_action="$CF_RESULT_DNS_ACTION"; dns_fingerprint="$CF_RESULT_DNS_FINGERPRINT"; cf_finish_transaction_step
  if [ "$dns_owned" = "true" ]; then [ -n "$dns_action" ] && log "Cloudflare proxied DNS reconciled ($dns_action, Agent-owned)." || log "Cloudflare proxied DNS already matches the recorded Agent-owned state."; else log "Existing matching proxied DNS reused without taking ownership."; fi
  cf_reconcile_origin_rule "$zone_id" "$host" "$port" "$owned_origin_ruleset" "$owned_origin_rule" "$old_origin_fingerprint"; origin_ruleset_id="$CF_RESULT_ORIGIN_RULESET_ID"; origin_rule_id="$CF_RESULT_ORIGIN_RULE_ID"; origin_action="$CF_RESULT_ORIGIN_ACTION"; origin_fingerprint="$CF_RESULT_ORIGIN_FINGERPRINT"; cf_finish_transaction_step; log "Cloudflare origin port rule reconciled for port $port."
  cf_reconcile_ssl_config_rule "$zone_id" "$host" "$owned_ssl_ruleset" "$owned_ssl_rule" "$old_ssl_fingerprint"; ssl_ruleset_id="$CF_RESULT_SSL_RULESET_ID"; ssl_rule_id="$CF_RESULT_SSL_RULE_ID"; ssl_action="$CF_RESULT_SSL_ACTION"; ssl_fingerprint="$CF_RESULT_SSL_FINGERPRINT"; cf_finish_transaction_step; log "Cloudflare strict SSL Configuration Rule reconciled for $host only."

  CF_TXN_PHASE=applying
  cf_checkpoint_transaction
  install -d -o root -g "$AGENT_USER" -m 0750 "$TLS_DIR"
  install -o root -g "$AGENT_USER" -m 0640 "$stage/new.key" "$TLS_DIR/origin.key"
  install -o root -g root -m 0644 "$stage/new.csr" "$TLS_DIR/origin.csr"
  install -o root -g "$AGENT_USER" -m 0644 "$stage/new.crt" "$TLS_DIR/origin.crt"
  write_config public "$port" "$TLS_DIR/origin.crt" "$TLS_DIR/origin.key"
  restart_and_verify_local || die "Agent failed after TLS/public reconfiguration; transaction rollback started."
  verify_public "$host" || die "Public Cloudflare verification failed; transaction rollback started."

  cf_set_commit_intent "$host" "$port" "$zone_id" "$zone_name" "$dns_id" "$dns_owned" "$origin_ruleset_id" "$origin_rule_id" "$ssl_ruleset_id" "$ssl_rule_id" "$cert_id" "$dns_fingerprint" "$origin_fingerprint" "$ssl_fingerprint"
  if [ -n "$old_host" ] && [ "$old_host" != "$host" ]; then
    save_previous_cloudflare_state "$old_host" "$old_zone" "$old_dns" "$old_dns_owned" "$old_origin_ruleset" "$old_origin_rule" "$old_ssl_ruleset" "$old_ssl_rule" "$old_cert" "$old_dns_fingerprint" "$old_origin_fingerprint" "$old_ssl_fingerprint"
  elif [ "$old_host" = "$host" ] && [ -n "$old_cert" ] && [ "$old_cert" != "$cert_id" ]; then
    save_previous_cloudflare_certificate "$old_host" "$old_cert"
  fi
  save_cloudflare_state "$host" "$port" "$zone_id" "$zone_name" "$dns_id" "$dns_owned" "$origin_ruleset_id" "$origin_rule_id" "$ssl_ruleset_id" "$ssl_rule_id" "$cert_id" "$dns_fingerprint" "$origin_fingerprint" "$ssl_fingerprint"
  CF_TXN_PHASE=committed
  cf_checkpoint_transaction
  clear_cloudflare_transaction_state
  log "Public HTTPS health, unauthenticated rejection, and authenticated MCP initialize all passed."
  cleanup_old_cloudflare "$old_host" "$old_zone" "$old_dns" "$old_dns_owned" "$old_origin_ruleset" "$old_origin_rule" "$old_ssl_ruleset" "$old_ssl_rule" "$old_cert" "$host" "$cert_id" "$zone_id" "$old_dns_fingerprint" "$old_origin_fingerprint" "$old_ssl_fingerprint"
  printf '\n%sServer setup complete.%s\n' "$GREEN" "$RESET"
  printf 'MCP URL: %shttps://%s/mcp%s\n' "$BOLD" "$host" "$RESET"
  printf 'Next: choose ChatGPT Setup from the menu.\n'
) }

configure_local(){ (
  acquire_management_lock
  [ ! -e "$CF_TXN_STATE" ] && [ ! -L "$CF_TXN_STATE" ] || die "Resolve the pending Cloudflare transaction with cloudflare-cleanup before changing local connection state."
  local port
  port="${AI_SERVER_AGENT_PORT:-$(current_port)}"; [ -r /dev/tty ] && port="$(prompt_value 'Local MCP port' "$port")"
  write_config local "$port" "" ""
  restart_and_verify_local || die "Agent did not recover in local mode."
  save_local_state "$port"
  log "Agent is now loopback-only. Existing TLS files and Cloudflare metadata were not deleted."
) }

configure_manual_tls(){ (
  acquire_management_lock
  [ ! -e "$CF_TXN_STATE" ] && [ ! -L "$CF_TXN_STATE" ] || die "Resolve the pending Cloudflare transaction with cloudflare-cleanup before changing TLS/connection state."
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
) }

cloudflare_cleanup(){ (
  acquire_management_lock
  local source zone_id dns_id dns_owned dns_fingerprint origin_ruleset_id origin_rule_id origin_fingerprint ssl_ruleset_id ssl_rule_id ssl_fingerprint cert_id host tmp old previous_zone previous_cert phase
  if [ -e "$CF_TXN_STATE" ] || [ -L "$CF_TXN_STATE" ]; then
    validate_cloudflare_transaction_state "$CF_TXN_STATE" || die "Cloudflare recovery journal is malformed/unsafe. It was left untouched; repair or inspect it explicitly before any cleanup mutation."
    phase="$(jq -r '.phase' "$CF_TXN_STATE")"; host="$(jq -r '.hostname' "$CF_TXN_STATE")"
    printf 'Incomplete Cloudflare transaction for: %s (phase: %s)\n' "$host" "$phase"
    if [ "$phase" = committed ] || [ "$phase" = rolled_back ]; then
      recover_cloudflare_transaction || die "Could not finalize the completed Cloudflare transaction journal."
      log "Completed Cloudflare transaction journal finalized without remote rollback."
      return 0
    fi
    confirm "Retry the recorded rollback/recovery now?" no || { echo "Cancelled."; return 0; }
    print_cf_token_guidance "$host"; load_cf_token
    if recover_cloudflare_transaction; then CF_TOKEN=""; log "Recorded Cloudflare recovery completed."; return 0; fi
    CF_TOKEN=""; die "Cloudflare recovery is still incomplete. Recovery state remains in $CF_TXN_STATE."
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
    dns_id="$(managed_get '.cloudflare_previous.dns_record_id')"; dns_owned="$(managed_get '.cloudflare_previous.dns_record_owned')"; dns_owned="${dns_owned:-false}"; dns_fingerprint="$(managed_get '.cloudflare_previous.dns_record_fingerprint')"
    origin_ruleset_id="$(managed_get '.cloudflare_previous.origin_ruleset_id')"; origin_rule_id="$(managed_get '.cloudflare_previous.origin_rule_id')"; origin_fingerprint="$(managed_get '.cloudflare_previous.origin_rule_fingerprint')"
    ssl_ruleset_id="$(managed_get '.cloudflare_previous.ssl_config_ruleset_id')"; ssl_rule_id="$(managed_get '.cloudflare_previous.ssl_config_rule_id')"; ssl_fingerprint="$(managed_get '.cloudflare_previous.ssl_config_rule_fingerprint')"; cert_id="$(managed_get '.cloudflare_previous.origin_certificate_id')"
    printf 'Deferred Cloudflare cleanup hostname: %s\n' "$host"
  else
    source=current
    zone_id="$(managed_get '.cloudflare.zone_id')"; dns_id="$(managed_get '.cloudflare.dns_record_id')"; dns_owned="$(managed_get '.cloudflare.dns_record_owned')"; dns_owned="${dns_owned:-false}"; dns_fingerprint="$(managed_get '.cloudflare.dns_record_fingerprint')"
    origin_ruleset_id="$(managed_get '.cloudflare.origin_ruleset_id')"; origin_rule_id="$(managed_get '.cloudflare.origin_rule_id')"; origin_fingerprint="$(managed_get '.cloudflare.origin_rule_fingerprint')"
    ssl_ruleset_id="$(managed_get '.cloudflare.ssl_config_ruleset_id')"; ssl_rule_id="$(managed_get '.cloudflare.ssl_config_rule_id')"; ssl_fingerprint="$(managed_get '.cloudflare.ssl_config_rule_fingerprint')"; cert_id="$(managed_get '.cloudflare.origin_certificate_id')"; host="$(managed_get '.hostname')"
    [ -n "$zone_id" ] || { log "No recorded Cloudflare-managed resources."; return 0; }
    if [ "$(current_mode)" = "public" ] && [ "$(managed_get '.active_provider')" = "cloudflare" ]; then die "This Cloudflare hostname is currently carrying the MCP connection. Switch to local/manual mode before cleanup."; fi
    printf 'Recorded Cloudflare hostname: %s\n' "$host"
  fi
  if [ "$dns_owned" = "true" ]; then printf 'The DNS record is recorded as Agent-owned and can be removed by this cleanup.\n'; else printf 'The DNS record is external/reused and will be preserved.\n'; fi
  confirm "Delete the recorded Agent-owned Cloudflare DNS (if any), Origin Rule, strict SSL Configuration Rule, and Origin CA certificate?" no || { echo "Cancelled."; return 0; }
  print_cf_token_guidance "$host"; load_cf_token
  if ! delete_recorded_cloudflare_resources "$zone_id" "$dns_id" "$dns_owned" "$origin_ruleset_id" "$origin_rule_id" "$ssl_ruleset_id" "$ssl_rule_id" "$cert_id" "$dns_fingerprint" "$origin_fingerprint" "$ssl_fingerprint"; then CF_TOKEN=""; die "Cloudflare cleanup was incomplete or resource drift was detected. Recorded ownership state was preserved for a safe retry/manual resolution."; fi
  CF_TOKEN=""
  if [ "$source" = previous ]; then clear_previous_cloudflare_state; else tmp="$(mktemp)"; old="$(cat "$MANAGED_STATE")"; jq '.cloudflare={}' <<<"$old" > "$tmp"; atomic_install_file "$tmp" "$MANAGED_STATE" root "$AGENT_USER" 0640 || { rm -f "$tmp"; return 1; }; rm -f "$tmp"; fi
  log "Recorded Agent-owned Cloudflare resources were removed or already absent; external DNS and unrecorded rules were preserved."
) }

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
    if [ -s "$CF_TXN_STATE" ] || [ -n "$(managed_get '.cloudflare.zone_id')" ] || [ -n "$(managed_get '.cloudflare_previous.zone_id')" ] || [ -n "$(managed_get '.cloudflare_previous_certificate.origin_certificate_id')" ]; then warn "Cloudflare-managed resources or recovery state are still recorded. Use Cloudflare cleanup first if you want them removed too."; fi
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

if [ "${AI_SERVER_AGENT_MANAGE_LIBRARY_ONLY:-0}" = "1" ]; then return 0 2>/dev/null || exit 0; fi

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
