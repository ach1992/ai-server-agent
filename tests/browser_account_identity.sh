#!/usr/bin/env bash
set -Eeuo pipefail

# Pure fixture: exercise actual installer guard using synthetic NSS state.
# Real account creation/refusal is covered by disposable root CI separately.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
guard="$(awk '/^verify_isolated_browser_account_ids\(\)\{/{emit=1} emit{print} emit && /^}/{exit}' "$ROOT/install.sh")"
[[ "$guard" = verify_isolated_browser_account_ids* ]] && [[ "$guard" = *$'\n}' ]] || {
  echo "installer Browser identity guard missing" >&2; exit 1;
}
AGENT_USER=aiagent
WORKER_USER=aiworker
BROWSER_USER=aibrowser
BROWSER_HOME=/var/lib/ai-server-agent/browser-home
declare -A uids=() gids=() named_gids=() homes=() shells=() extra_groups=()
reset_fixture(){
  uids=([aiagent]=900 [aiworker]=901 [aibrowser]=902)
  gids=([aiagent]=900 [aiworker]=901 [aibrowser]=902)
  named_gids=([aiagent]=900 [aiworker]=901 [aibrowser]=902)
  homes=([aiagent]=/var/lib/ai-server-agent [aiworker]=/var/lib/ai-server-agent/worker-home [aibrowser]="$BROWSER_HOME")
  shells=([aiagent]=/usr/sbin/nologin [aiworker]=/bin/bash [aibrowser]=/usr/sbin/nologin)
  extra_groups=()
}
getent(){
  local table="$1" name="$2"
  case "$table" in
    passwd)
      [[ -n "${uids[$name]+x}" ]] || return 2
      printf '%s:x:%s:%s::%s:%s\n' "$name" "${uids[$name]}" "${gids[$name]}" "${homes[$name]}" "${shells[$name]}"
      ;;
    group)
      [[ -n "${named_gids[$name]+x}" ]] || return 2
      printf '%s:x:%s:\n' "$name" "${named_gids[$name]}"
      ;;
    *) return 2 ;;
  esac
}
id(){
  local option="$1" name="$2"
  [[ -n "${uids[$name]+x}" ]] || return 1
  case "$option" in
    -u) printf '%s\n' "${uids[$name]}" ;;
    -g) printf '%s\n' "${gids[$name]}" ;;
    -G) printf '%s\n' "${extra_groups[$name]:-${gids[$name]}}" ;;
    *) return 1 ;;
  esac
}
die(){ printf 'refused: %s\n' "$*" >&2; exit 19; }
eval "$guard"
run_case(){
  local label="$1" expected="$2" out rc=0
  out="$(mktemp)"
  ( verify_isolated_browser_account_ids ) > "$out" 2>&1 || rc=$?
  if [[ "$expected" = accept ]]; then
    if [[ "$rc" != 0 ]]; then
      cat "$out" >&2
      rm -f -- "$out"
      echo "safe fixture wrongly refused: $label" >&2; exit 1
    fi
  else
    if [[ "$rc" != 19 ]] || ! grep -qF "$expected" "$out"; then
      cat "$out" >&2
      rm -f -- "$out"
      echo "Browser alias was not refused correctly: $label ($rc)" >&2; exit 1
    fi
  fi
  rm -f -- "$out"
  echo "PASS $label"
}
reset_fixture; run_case "three independent identities" accept
reset_fixture; uids[aibrowser]=901; run_case "Browser UID aliases worker" "Browser UID aliases protected aiworker"
reset_fixture; gids[aibrowser]=901; named_gids[aibrowser]=901; run_case "Browser GID aliases worker" "Browser GID aliases protected aiworker"
reset_fixture; uids[aibrowser]=900; run_case "Browser UID aliases Agent" "Browser UID aliases protected aiagent"
reset_fixture; gids[aibrowser]=900; named_gids[aibrowser]=900; run_case "Browser GID aliases Agent" "Browser GID aliases protected aiagent"
reset_fixture; named_gids[aibrowser]=900; run_case "Browser named group aliases Agent" "dedicated primary group"
reset_fixture; uids[aibrowser]=0; run_case "zero Browser UID" "Invalid or inconsistent Browser numeric UID/GID"
reset_fixture; gids[aibrowser]=0; named_gids[aibrowser]=0; run_case "zero Browser GID" "Invalid dedicated Browser group identity"
reset_fixture; extra_groups[aibrowser]="902 900"; run_case "supplementary Agent group" "unexpected supplementary groups"
reset_fixture; homes[aibrowser]="/tmp/incorrect"; run_case "incorrect Browser HOME" "HOME/shell differs"
reset_fixture; shells[aibrowser]="/bin/bash"; run_case "incorrect Browser shell" "HOME/shell differs"
reset_fixture; unset 'uids[aibrowser]' 'gids[aibrowser]'; run_case "only Browser group precreated" accept
reset_fixture; unset 'uids[aibrowser]' 'gids[aibrowser]'; named_gids[aibrowser]=900; run_case "precreated Browser group aliases Agent GID" "Browser GID aliases protected aiagent"
reset_fixture; unset 'named_gids[aibrowser]'; run_case "Browser user without dedicated group" "must use its own dedicated primary group"
echo "browser account numeric NSS identity guard fixtures passed"
