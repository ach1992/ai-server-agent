#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
command -v go >/dev/null
command -v jq >/dev/null
# Immutable v0.1.14 release: real pre-retirement named-credential schema.
BASE=6468d48b4ff5f1f82ad446531dff9951e86a0e3c
if ! git cat-file -e "$BASE^{commit}" 2>/dev/null; then
  git fetch -q --depth=1 origin refs/tags/v0.1.14
fi
git cat-file -e "$BASE^{commit}" || { echo 'Historical named-credential base missing; require complete checkout' >&2; exit 1; }
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
mkdir -p "$TMP/old-source"
git archive "$BASE" | tar -x -C "$TMP/old-source"
(cd "$TMP/old-source" && CGO_ENABLED=0 go build -trimpath -o "$TMP/old-agent" ./cmd/ai-server-agent)
CGO_ENABLED=0 go build -trimpath -o "$TMP/new-agent" ./cmd/ai-server-agent
awk '/^verify_existing_runtime_compatibility\(\)\{/ {grab=1} grab {print} grab && /^}$/ {exit}' install.sh > "$TMP/preflight-fn.sh"
grep -q '^verify_existing_runtime_compatibility(){' "$TMP/preflight-fn.sh"
# Execute the exact production preflight function in a subprocess with no
# privileged installer side effects, systemd changes, or real credentials.
source "$TMP/preflight-fn.sh"
die(){ printf 'PREFLIGHT_REFUSED: %s\n' "$*" >&2; exit 86; }
FRESH_INSTALL=0
CONFIG_DIR="$TMP"
CONFIG_FILE="$TMP/config.json"
CREDENTIAL_STORE="$TMP/creds.json"
MCP_CREDENTIAL_STORE="$CREDENTIAL_STORE"
jq -n --arg path "$CREDENTIAL_STORE" '{listen_address:"127.0.0.1:3210",mcp_path:"/mcp",health_path:"/healthz",auth_mode:"bearer",credential_store_file:$path,executor_socket:"/run/example.sock",executor_token_file:"/tmp/example.token",state_dir:"/tmp/example-state",workspace_dir:"/tmp/example-workspace",agent_user:"aiagent",worker_user:"aiworker"}' > "$CONFIG_FILE"
write_fixture(){
  local mode="$1"
  jq -n --arg mode "$mode" '
    def direct: {principal:{id:"direct-default",class:"direct",name:"direct/default"},verifier_algorithm:"sha256-v1",verifier:("a"*64),created_at:"2026-10-08T00:00:00Z",enabled:($mode!="gateway-only"),revoked_at:(if $mode=="gateway-only" then "2026-10-09T00:00:00Z" else null end)} | if .revoked_at==null then del(.revoked_at) else . end;
    def gateway: {principal:{id:"mcp-gateway",class:"gateway",name:"mcp-gateway"},verifier_algorithm:"sha256-v1",verifier:("b"*64),created_at:"2026-10-08T00:00:00Z",enabled:($mode!="gateway-revoked"),revoked_at:(if $mode=="gateway-revoked" then "2026-10-09T00:00:00Z" else null end)} | if .revoked_at==null then del(.revoked_at) else . end;
    def secondary: {principal:{id:"direct-secondary",class:"direct",name:"direct/secondary"},verifier_algorithm:"sha256-v1",verifier:("b"*64),created_at:"2026-10-08T00:00:00Z",enabled:($mode!="secondary-revoked"),revoked_at:(if $mode=="secondary-revoked" then "2026-10-09T00:00:00Z" else null end)} | if .revoked_at==null then del(.revoked_at) else . end;
    {version:1,credentials:([direct] + (if $mode=="direct-only" then [] elif $mode=="gateway-only" or $mode=="gateway-active" or $mode=="gateway-revoked" then [gateway] else [secondary] end))}
  ' > "$CREDENTIAL_STORE"
}
assert_accept(){
  local binary="$1"
  (verify_existing_runtime_compatibility "$binary") >/dev/null || { echo "Incorrectly blocked $(basename "$binary") for $MODE" >&2; exit 1; }
}
assert_refuse(){
  local binary="$1"
  if (verify_existing_runtime_compatibility "$binary") >"$TMP/reject.out" 2>&1; then
    echo "UNSAFE ACCEPTANCE: $(basename "$binary") for $MODE" >&2; exit 1
  fi
  grep -q 'PREFLIGHT_REFUSED:' "$TMP/reject.out" || { echo 'Missing actionable refusal' >&2; exit 1; }
}
MODE=direct-only; write_fixture "$MODE"
assert_accept "$TMP/new-agent"
assert_accept "$TMP/old-agent"
MODE=gateway-active; write_fixture "$MODE"
assert_accept "$TMP/new-agent"
assert_refuse "$TMP/old-agent"
MODE=gateway-only; write_fixture "$MODE"
assert_refuse "$TMP/new-agent"
assert_refuse "$TMP/old-agent"
MODE=gateway-revoked; write_fixture "$MODE"
assert_accept "$TMP/new-agent"
assert_accept "$TMP/old-agent"
MODE=secondary-active; write_fixture "$MODE"
assert_accept "$TMP/new-agent"
assert_refuse "$TMP/old-agent"
MODE=secondary-revoked; write_fixture "$MODE"
assert_accept "$TMP/new-agent"
assert_refuse "$TMP/old-agent"
# The staged-store command validates the supplied candidate instead of the
# running store; an invalid replacement must fail without any writes.
printf '%s\n' '{"version":1,"credentials":[]}' > "$TMP/invalid.json"
if "$TMP/new-agent" -config "$CONFIG_FILE" validate-credential-store "$TMP/invalid.json" >/dev/null 2>&1; then
  echo 'Bad staged credentials accepted' >&2; exit 1
fi
# A target predating named credentials could misleadingly accept a config that
# includes both auth sources. Never infer compatibility from that.
cat > "$TMP/too-old-agent" <<'SCRIPT'
#!/bin/sh
case "$*" in *validate-config) exit 0 ;; *) echo 'unknown command' >&2; exit 2 ;; esac
SCRIPT
chmod +x "$TMP/too-old-agent"
MODE=direct-only; write_fixture "$MODE"
assert_refuse "$TMP/too-old-agent"
echo 'Credential upgrade/rollback target-binary preflight PASS (7 old/new fixtures, invalid staged replacement, unsupported historical target)'
# Assert that the installed updater refuses to execute an unsafe historical
# installer before any target script is run (source/stable share this gate).
awk '/^verify_downloaded_installer_credential_compatibility\(\)\{/ {grab=1} grab {print} grab && /^}$/ {exit}' update.sh > "$TMP/updater-preflight-fn.sh"
grep -q '^verify_downloaded_installer_credential_compatibility(){' "$TMP/updater-preflight-fn.sh"
source "$TMP/updater-preflight-fn.sh"
cli_error(){ printf 'UPDATE_REFUSED: %s\n' "$*" >&2; }
assert_update_accept(){
  local installer="$1"
  verify_downloaded_installer_credential_compatibility "$installer" "$CONFIG_FILE" "$CREDENTIAL_STORE" "$TMP/new-agent" >/dev/null || {
    echo "Updater incorrectly refused $(basename "$installer") for $MODE" >&2; exit 1;
  }
}
assert_update_refuse(){
  local installer="$1"
  if verify_downloaded_installer_credential_compatibility "$installer" "$CONFIG_FILE" "$CREDENTIAL_STORE" "$TMP/new-agent" > "$TMP/update-refuse.out" 2>&1; then
    echo "UNSAFE UPDATER ACCEPTANCE $(basename "$installer") for $MODE" >&2; exit 1;
  fi
  grep -qF 'UPDATE_REFUSED:' "$TMP/update-refuse.out" || exit 1
}
MODE=direct-only; write_fixture "$MODE"
assert_update_accept install.sh
assert_update_accept "$TMP/old-source/install.sh"
MODE=gateway-active; write_fixture "$MODE"
assert_update_accept install.sh
assert_update_refuse "$TMP/old-source/install.sh"
MODE=gateway-only; write_fixture "$MODE"
assert_update_accept install.sh # target enforces its own exact-binary check
assert_update_refuse "$TMP/old-source/install.sh"
MODE=gateway-revoked; write_fixture "$MODE"
assert_update_refuse "$TMP/old-source/install.sh" # must explicitly prune first
MODE=secondary-active; write_fixture "$MODE"
assert_update_accept install.sh
assert_update_refuse "$TMP/old-source/install.sh"
MODE=secondary-revoked; write_fixture "$MODE"
assert_update_refuse "$TMP/old-source/install.sh"
MODE=direct-only; write_fixture "$MODE"
assert_update_refuse "$TMP/too-old-agent"
echo 'Updater downloaded-installer compatibility gate PASS (new, historical named and unsupported legacy target)'
