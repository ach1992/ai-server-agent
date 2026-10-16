# Operator CLI surface inventory and presentation contract

This document covers the **source CLI**, not a promise that the currently installed Stable has been updated. Issue [#147](https://github.com/ach1992/ai-server-agent/issues/147) owns interactive acceptance. The installer, management entrypoint and standalone lifecycle helpers must remain usable in plain non-TTY automation with no required TUI dependency.

## Surface inventory (source of actual strings is the named function)

| Route | Entry / screen / output | Source |
| --- | --- | --- |
| Interactive main menu | Identity/header, health, connection, operational, credentials and removal choices, exit | `manage.sh`: `header`, `menu`, `ui_*` |
| Connection submenu | Cloudflare, private/local, manual certificate, return | `manage.sh`: `connection_menu` |
| First connection | Initial connection selection, defer, failure/retry, verified completion | `manage.sh`: `first_run`; `install.sh`: `manage_connection_setup` |
| Runtime submenu | Current capacity, change/reset/advanced, ranges, return | `manage.sh`: `runtime_menu`, `runtime_show`, `runtime_apply` |
| Server purpose submenu | Development/validation/label, inherited permissions, return | `manage.sh`: `profile_menu`, `profile_show`, `profile_apply` |
| Credentials submenu | List, rotate, revoke, intentional one-time bearer reveal and reconnection warning | `manage.sh`: `credential_menu`, `credential_status`, `credential_issue`, `credential_revoke`, `reveal_auth` |
| Read-only pages | Health/service state, environment, client connection guide | `manage.sh`: `status`, `chatgpt_setup`, plus runtime/profile/credential views |
| Connection mutations | Guided Cloudflare, scoped DNS/rules/cert, conflicts, secure prompts, durable rollback and recovery | `manage.sh`: `configure_cloudflare`, `cf_*`, `recover_cloudflare_transaction`, `cleanup_old_cloudflare`, `cloudflare_cleanup` |
| Local/manual connection | Port/certificate inputs, precommit cancellation, health/rollback | `manage.sh`: `configure_local`, `configure_manual_tls` |
| Lifecycle from manager | Update/repair, safe uninstall, purge, Cloudflare cleanup, outcome/error messages | `manage.sh`: `update_agent`, `repair`, `run_uninstall`, `cloudflare_cleanup` |
| All manager subcommands | Arguments, usage/errors and machine-safe plain output | `manage.sh`: dispatch at end of file |
| Initial installer | OS/platform errors, profile selector, initial bearer reveal, setup and final summary | `install.sh`: top-level, `validate_platform`, credential/rollback paths, final summary |
| Update/uninstall helpers | Trusted source/stable identity, destructive confirmation, failure and preserved-state messages | `update.sh`, `uninstall.sh` |
| Immutable Stable bootstrap | Trust/prerequisite verification and staging diagnostics | `scripts/install-stable.sh` |

The inventory includes all **operator-visible route families**, including messages produced during error/rollback paths; individual diagnostic strings remain adjacent to their execution path to avoid stale parallel copies. For a line-level extraction of all literal and dynamic candidates in the five entry scripts, run:

```bash
rg -n 'printf|echo|read -r|(^|[;&[:space:]])(log|warn|die|confirm|ui_notice|ui_section|ui_wrap|ui_detail)\b|cat[[:space:]]*<<' manage.sh install.sh update.sh uninstall.sh scripts/install-stable.sh
```

This intentionally includes internal formatting/serialization calls, which must be inspected before classifying them as user output. Installer-generated service JSON, `jq` and external command output also require checking in their calling contexts. Do not treat a grep count as a validated screen render.

## Single interaction system

- **Terminal layout:** the interactive manager has one contextual identity header (actual install channel/version/source ref, hostname/validated label and purpose, connection mode and safe non-secret endpoint), section headings, consistently aligned action numbers, and one clear Back/Exit. No duplicated top-level menu or fictitious release identities. Work at both typical 80x24 and narrower terminals; avoid clipping URLs or long tokens; keep short-height menus readable.
- **Meaning before color:** `INFO`/navigation uses cyan, verified `SUCCESS` green, actionable `WARNING` yellow, `ERROR` red, destructive entries red, reversible sensitive entries yellow, optional supporting text subdued. All status words and instructions remain meaningful with `NO_COLOR` or redirected output, and use no Unicode-only icons or compulsory color.
- **Copy:** headings identify the task; warnings state the risk and operator response, errors explain the failure and next action where known, success is shown only after verified completion. Every destructive prompt states what is deleted versus preserved. Do not call an unverified recovery successful. Avoid long hardcoded lines; prefer one idea per wrapped paragraph.
- **Navigation:** retain existing numeric entry/CLI subcommand contracts. Invalid input and non-destructive failures return to the same menu; `0` means Back in submenus and Exit at root; `b` cancels an editable precommit input. Default for destructive confirmations remains No. Unchanged/aborted flows must not claim a successful mutation.
- **Cancellation / mutation boundary:** read-only operations may stop on Ctrl+C. Permit cancellation before the first side effect at a reversible precommit boundary. Once a lock-protected change/restart or Cloudflare transaction starts, interruption must not abandon state or falsely imply rollback; report that completion or verified recovery must finish. Any additional safe in-flight checkpoint must have a proven no-side-effect boundary, not just a signal handler.
- **Secrets / safety:** never print bearer tokens except the deliberate one-time terminal reveal; never put credentials, file contents, Cloudflare API tokens, TLS keys or arbitrary control bytes into headers, logs or confirmations. Keep lifecycle locks, root authority, rollback and current cloud transaction semantics unchanged.

## Verification matrix

1. Static source inventory and a diff audit for every operator-facing route, including installer/update/uninstall/error and uncommon recovery branches.
2. PTY menu tests for main/child flow, 24-row and narrow width, no-color, typed input, invalid choice, safe Back/No, error -> menu recovery, SIGINT read-only versus protected change.
3. Safe simulated installer/connection/credential/capacity workflows with no host/config/Cloudflare mutation; exercise prompts at precommit boundaries.
4. `bash -n`, `shellcheck` where available, the relevant repository's deterministic tests and required exact-head CI/Security for a stable PR candidate.
5. Record any remaining true installed-terminal visual acceptance as **unverified** until an authorized fresh/isolated installation actually exercises it. No occupied host upgrade or new Stable release follows automatically from source tests.
