# Testing and Validation

[PROJECT-SPEC.md](PROJECT-SPEC.md) owns product success criteria. This document describes how current implementation and future accepted capabilities prove them. [README.md](../README.md) is the user-facing entrypoint, not a release-evidence archive.

This is the durable testing/acceptance model, **not** a historical release log. Official [GitHub Releases](https://github.com/ach1992/ai-server-agent/releases) establish published identity. [Issues #137](https://github.com/ach1992/ai-server-agent/issues/137) and [#59](https://github.com/ach1992/ai-server-agent/issues/59) record the Owner-accepted bounded first-install/workflow evidence and the explicit decision not to require an exhaustive host-loss/soak matrix. [Issue #99](https://github.com/ach1992/ai-server-agent/issues/99) retains conditional native file/media client support; previous transfer evidence does not imply native attachment rendering. New or high-risk changes still require the relevant exact-candidate tests and review.

## Installed Environment Profile acceptance (availability is version-dependent)

The original Development/Validation purpose profiles (original source/installed acceptance: completed [#143](https://github.com/ach1992/ai-server-agent/issues/143)) and the later version-gated Production advisory profile have a focused verification contract:

- Fresh interactive installation clearly explains and records the chosen purpose; noninteractive install uses an explicit safe default or configured purpose.
- A post-install operator can inspect and switch the non-secret purpose/label without reinstalling; invalid or contradictory values fail clearly and preserve the previous effective configuration.
- Reinstall/update/repair of the **new baseline** preserve the selected profile, with no speculative migration promise from legacy Agents.
- All three profiles advertise an unchanged MCP tool/capability inventory and Root/worker authorization model. Production must explicitly advise passive inspection and fresh consent before every change while acknowledging the Agent cannot verify approval; profile selection must not provision/revoke credentials or silently restrict tools.
- Authenticated `agent_environment` and MCP `initialize` instructions advertise accurate purpose; client-specific actual delivery of instructions is verified and documented rather than assumed. Exercise the standalone direct-MCP client path.
- Verify the feature on a fresh authorized installation, with configuration/management/service restart and rollback/error behavior appropriate to its implementation. For Production, check authenticated MCP guidance, unchanged tool/Root/worker inventory and credential identity, plus a successful switch back to Development/Validation; older releases without Production support (including `v0.1.14`) must not be assumed to accept its config during rollback. No production or connected-occupied-host mutation without separate approval.

Source/CI coverage does not substitute for installed feature proof. Consult the installed release, completed #143, and the bounded evidence in completed #137/#59; do not reactivate an exhaustive clean-host or cross-client program without a concrete need. Older Stable releases may not include these commands. For an upgraded real MCP client, distinguish **fresh server `tools/list`** from potentially cached client-facing tool descriptions and schemas; retry supported client discovery/refresh before diagnosing the server binary. Do not use an Agent restart or speculative client UI work as a substitute for identifying that boundary.

## Direct MCP transfer acceptance

Run `TestDirectMCPTransferSourceAcceptance` for the normal unprivileged official MCP SDK integration suite. For an **opt-in existing installed executable**, run `TestDirectMCPTransferInstalledAcceptance` with its absolute executable path and pinned SHA256; this launches isolated real Agent and Executor subprocesses rather than touching protected services. Both modes verify 32 KiB text, 5 MiB+43-byte and 33 MiB+13-byte binary transfers, resumability, exact integrity, versioned ranged egress and atomic commit. Installed mode additionally tests isolated Executor restart, different principals/workspaces, malformed requests, security boundaries, resource budgets, measured HTTP bodies and worker process resources, and runs a **separate compiled example MCP client** that resumes an interrupted upload and reconstructs a new local binary file.

The exact commands, acceptance criteria and honest limitations are maintained once in [WORKSPACE-TRANSFER-ACCEPTANCE.md](WORKSPACE-TRANSFER-ACCEPTANCE.md). The runnable consumer is [`examples/workspace-transfer-client`](../examples/workspace-transfer-client). `-short` omits the expensive real-process suite. Neither mode requires root, service restart or Stable publication. Both modes use disposable same-UID test state; **neither** proves the protected live root/worker split, connected ChatGPT UI attachment presentation or Stable deployment. Native client attachment rendering remains conditional under #99; the extra #59 full-matrix program was closed as not planned.

## Operator console / first-run UX acceptance

The stable manager/installer UX is documented for operators in [Operations](OPERATIONS.md#interactive-console-navigation-and-safe-cancellation). Test actual behavior rather than maintaining a second inventory of UI source functions or error strings:

- Run `tests/operator_menu.sh` and `tests/operator_presentation.sh` for real pseudo-TTY menu navigation, contextual installed identity, readable narrow terminals, `NO_COLOR`/non-TTY semantics, invalid input recovery, nested Back/Cancel and sensitive output sanitization.
- Verify first-run installer/profile/connection paths, protected precommit cancellation and default-No destructive confirmation without changes. Read-only Ctrl+C should return safely where supported; protected lifecycle/Cloudflare changes must not abandon transaction state or claim an unverified rollback.
- Cover the meaningful operator-visible paths beyond the main menu: credential issue/revoke/one-time bearer reveal, runtime/profile adjustment, update/repair/uninstall, and diagnostic/rollback outcomes. Check both piped-installer input **with a real controlling PTY** and fully unattended input **without an available `/dev/tty`**, including 80x24/narrow display, UTF-8 retention, safe control-character escaping and secret-safe output. Preserve the distinction between reproducible simulated tests and separately authorized installed/Cloudflare acceptance.
- The opt-in `tests/installed_operator_console.sh` runs **only** on a disposable GitHub-hosted CI runner after a fresh exact-candidate install and the installed runtime acceptance. It uses the installed `ai-server-agent-manage` entrypoint and actual PTY input to prove 50-column `NO_COLOR`, nested invalid/retry/Back and pre-commit `b` without config/credential/state/PID mutation. It injects a bounded in-flight Ctrl+C during an installed manager read-only view and during an installed protected `runtime_apply` transaction with a one-shot restart failure. The latter must complete with verified exact configuration rollback and healthy services before returning to the menu. A no-Ctrl+C negative control exercises that same rollback but must lack the installed menu's SIGINT witness; the positive path gates signal delivery on an in-flight restart marker, observes the original installed handler's flag and requires both witness and recovery. The test is root-capable and deliberately opt-in; **do not run it on occupied Development/Validation hosts or against real Cloudflare**. The injected slow read-only view proves the installed menu signal handling, not the latency of a real service command. The already-established separate `tests/installed_runtime_settings.sh` covers actual rollback/health under a controlled failure.
- Distinguish hermetic PTY and mocked failure behavior from installed first-run validation on a fresh authorized disposable host. [Issue #147](https://github.com/ach1992/ai-server-agent/issues/147) is closed/completed: its bounded installed-console and Ctrl+C recovery acceptance passed in immutable Stable v0.1.18. That CI-backed acceptance does not imply human-observed use on every supported host or completion of unrun combinations. Revisit only when a concrete defect, risk or relevant behavior change warrants targeted validation.

## 1. Supported platform policy and validation matrix

The supported platform policy validated by current source is:

- Ubuntu 22.04 or newer
- Debian 11 or newer
- amd64/x86_64
- arm64/aarch64
- systemd
- dedicated development/test server use

Release-specific status is not duplicated in this validation model; GitHub Releases is authoritative for the latest immutable stable version, and README carries the current stable install command.

`tests/platform_compatibility.sh` is the deterministic policy contract. It covers Ubuntu 22.04/24.04/26.04, Debian 11/12/13, both supported architecture aliases, rejection of older/unsupported systems, alignment of installer/bootstrap distro minimums, and the release architecture list. CI also uses real Debian 11/12/13 containers. Debian 12/13 install the Agent's minimal host dependencies from their maintained repositories and run the stable host check. Debian 11, whose upstream LTS ended on 2026-08-31, is retained as a legacy application-compatibility contract without making CI or the installer rewrite its package sources. Native lifecycle validation remains proportional: the main CI job exercises Ubuntu 22.04 amd64 deeply, while a dedicated GitHub-hosted arm64 job builds/tests and performs install/systemd/root-boundary validation on Ubuntu 24.04 arm64. The release builder cross-builds and checksums every architecture declared in `scripts/release-arches.txt`.

Adding a platform is not a reason to duplicate the full security suite across every matrix cell. Extend the policy/list and focused compatibility evidence, then add native lifecycle coverage where a materially distinct runtime behavior requires it.

## 2. Validation layers

Use the narrowest relevant check first, then the applicable broader layer.

### Go correctness

CI runs:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race -vet=off ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/ai-server-agent ./cmd/ai-server-agent
```

These cover Go unit/behavior tests, race detection and the production binary build.

When SQL SELECT alias navigation changes, the Go-scope CI acceptance runs a real unprivileged Worker/Landlock `code_definition`/`code_references` path across multiple Migration files, and a separate bearer-authenticated direct MCP SDK/Agent/Executor path for `SELECT/JOIN` aliases. The CI job explicitly rejects a skipped worker/MCP test and requires the direct-client `DIRECT_REAL_SQL_ALIAS_MCP_PASS` marker, matching the exact candidate HEAD/tree. These run on disposable isolated fixtures and never connect to a database; they prove source-integrated protocol behavior, not a refreshed installed Stable or a ChatGPT UI. Unavailable `/usr/bin/rg` or Landlock v2 cannot be reported as an acceptance PASS.

The main CI job additionally requires **non-skipped real worker/Landlock SQL materialized-view navigation** when Go source or its tests change: it executes `TestSQLMaterializedViewWorkerCrossFileHistory` under `ASA_REQUIRE_SQL_MATERIALIZED_WORKER=1` and checks the named verbose PASS, refusing SKIP. Unlike the regular nonverbose package suite, this verifies actual unprivileged helper invocation, bounded cross-file search, source versioning and PostgreSQL migration lifecycle/ambiguity behavior on the exact PR HEAD. The strict test mode rejects missing Landlock v2 or trusted `/usr/bin/rg` instead of treating missing prerequisites as success. On the disposable CI Ubuntu runner, the test job provisions the signed distribution `ripgrep` package *only if the trusted system path is absent*, then checks every executable-path component for root ownership, writable modes and symlinks; this does not change any installed Agent host. It does **not** establish installed Agent or live database behavior.

After trusted ripgrep provisioning, the same CI job also enforces **non-skipped ordinary PostgreSQL relation-move Worker/Landlock acceptance** through `TestSQLPostgresOrdinaryMovesRealWorkerDefinition` with `ASA_REQUIRE_SQL_ORDINARY_WORKER=1`. It requires a non-root Worker, Landlock ABI v2 and trusted `/usr/bin/rg`; CI checks explicit PASS for schema move, VIEW rename and TABLE rename and rejects a SKIP. A native arm64 portable unit run may still skip unavailable optional search dependencies, but that is never counted as meeting the separate mandatory Ubuntu Worker gate.

Configurable worker-command, persistent-task, persistent-service and shared stdio-session slot regressions prove validated auto/1..64 and 1..64 per-kind admissions, CPU-aware command defaults, unchanged root command isolation, protected high-water restart recovery, inventory/log/audit space reservations, operator persistence, installed-runtime visibility, and safe rollback.

The opt-in `tests/installed_runtime_settings.sh` acceptance runs only on a freshly installed disposable GitHub Actions Ubuntu runner, after private bearer activation. It uses real systemd Agent/Executor services, privileged `runtime-set/reset`, authenticated direct MCP calls and actual worker/job/transfer operations to distinguish persisted settings from effective behavior: eight concurrent worker commands, eight real tasks downscaled to one without cancelling existing units, independent service admission (source-level 64-task/64-service boundary proofs). Mixed LSP/DAP plus recovered Terminal registry proofs also cover cross-restart 64-slot occupancy, 64-to-1 downscaling, non-double-counted Control Mode reconnects, concurrency with newly published scope reservations, invalid root-owned record fail-closed behavior, and optional real tmux reconnect/close with exact owner and epoch checks, transfer quota exhaustion and abort recovery, 10-second command timeout followed by reset, and one-shot real restart-failure rollback. It must never run on occupied Development/Validation servers and does not prove client-specific ChatGPT UI refresh or arbitrary host crash recovery. The separate hermetic `tests/operator_menu.sh` drives a real pseudo-TTY to exercise narrow-console presentation, nested Back/Cancel paths, invalid inputs, no-color output, and failure isolation without privileged mutations. It is run by `tests/runtime_settings.sh`; it does **not** replace real first-install console acceptance.

The executor-foundation tests specifically exercise separate non-queueing worker/root capacity, structured resource-limit results, bounded timeout selection, graceful TERM/KILL process-group cancellation, same-process-group background cleanup, peer-close context cancellation, and both sides of the private executor response-frame limit. The command/jobs tests additionally cover production-time head/tail output bounding, binary-safe base64 results, physical persistent-log bounds with logical retention offsets, protected command-file consumption, idempotent retry/conflict behavior, raw-command absence from Agent-created systemd argv/audit/idempotency claims, and fail-fast persistent-job capacity, replay admission enforcement, interrupted-job reconciliation, and bounded terminal-artifact retention. Recovery regressions specifically cover failed `systemd-run` with a loaded-but-inactive unit, authoritative active-unit state dominating worker-writable numeric/unknown status markers, command-handoff retirement across runner setup/validation failures, surfaced terminal-cleanup errors, and shared lifecycle-lock exclusion held through persistent-job launch. These tests complement, rather than replace, the privileged lifecycle/security jobs.

### Shell syntax

CI validates the established installer, updater, uninstaller, management, release-builder and security-test paths. `tests/stable_bootstrap.sh` also syntax-checks the stable bootstrap before exercising it behaviorally.

**Privileged installer path-trust source acceptance (Issue #188):** The candidate root installer refuses symlinked, non-root-owned, group/world-writable or hardlinked executable/management destinations and refuses untrusted root `PATH` entries **before lifecycle locks, package operations, binary replacement, credentials or service activation**. `tests/privileged_install_paths.sh` exercises the exact production preflight functions against private root-owned `/run` fixtures (secure positive; writable/non-root/symlink/hardlink/missing-parent negatives). Remediation of the independent review's privileged command-search findings additionally covers lossless PATH parsing and control-character rejection, selected command leaf ownership/mode and resolved symlink ancestry (not merely PATH directory modes), supported trusted `/run` command shims, and both successful/denied full root `--check-platform` invocations without executing an unsafe `tr`. Root-mode command target trust is checked before any normal PATH-resolved tool use and again after provisioning optional packages; non-root platform probes retain their existing contract. The root-trust workflow additionally verifies that the full installer `--check` fails on the GitHub image's untrusted destination, when present. Only then does `scripts/ci-prepare-trusted-install-paths.sh` normalize known `root:root` `/usr/local/{bin,sbin,lib}` **on an expressly GitHub-hosted disposable runner**. The production installer never repairs shared real-host directories automatically; an unsafe operator host must be examined and remediated separately. The installed management wrapper regenerates those root path and selected-command predicates from the installer and checks its own path plus management/update/uninstall/startup helper paths before a lifecycle lock or helper execution. A CI-only, explicitly disposable installed test rejects post-install writable library drift, checks no config mutation, and verifies injected Bash startup files or exported functions cannot reach the installed manager's subprocesses. This is defense in depth for a still-trusted wrapper: if an adversary can replace the wrapper itself before root invokes it, no guard inside that replaced script can execute. Direct updater/uninstaller invocations, systemd startup drift, and source binary override remain separate Issue #188 boundaries. These source checks are not a published Stable security guarantee or a real installed-client proof.

CI change scope is fail-closed and path-aware. Changes limited to `README.md`, `AGENTS.md`, `SECURITY.md`, `LICENSE`, `.gitignore`, or `docs/**` do not re-run runtime/OS/security suites that cannot validate those files. The always-run change-scope job validates the exact base-to-head diff with `git diff --check`; required non-matrix jobs are conditionally skipped (GitHub reports a skipped job as successful), while the Debian matrix keeps lightweight per-matrix checks present and skips only its expensive host validation. Any classifier failure forces the required validation jobs to fail rather than silently skip; a missing/unknown classifier output also falls back to the full suite and only explicit `runtime_changed=false` may skip heavy validation. Runtime, test, script, workflow, dependency or other unrecognized paths fail safe to the full suite. A docs-only `main` push still builds and uploads the exact-SHA release candidate artifact because the release workflow promotes CI-produced artifacts for the selected main SHA.

The classifier also emits conservative validation-surface flags consumed by both local development and CI. Known ordinary Go-only paths keep normal Go/race and native arm64 build/test coverage while avoiding unrelated privileged suites. Debian host validation is selected only for platform/install-support surfaces; the native arm64 lifecycle step is selected only for lifecycle-relevant surfaces. High Assurance jobs are selected independently for Cloudflare transaction, root-trust, stable provenance, and stable-update surfaces. Security/auth/policy/privilege wiring, dependency changes, workflow changes, classifier/self-test changes, and unknown/new paths fail closed to broader coverage. `README.md` is runtime-light but still selects stable-provenance validation because stable bootstrap acceptance verifies its permanent `main` URL, explicit-version example and controlled release-installer handoff.
Within the normal CI job, Go format/vet/race/build work is selected for Go surfaces, shell syntax for shell surfaces, and the heavier installer/Cloudflare/root-trust/update/release/lifecycle steps use the same relevant flags. Main-branch release-candidate artifact production remains exact-SHA and is not weakened by PR fast-path selection.

This is validation selection, not risk classification: a task that is HIGH/HIGH_ASSURANCE still follows its accepted review/evidence contract even when one unrelated suite is provably irrelevant. Existing job/check names stay stable; a skipped irrelevant job is not evidence for a required high-risk surface.

### Avoid repeated tests after an identical squash merge

Pull requests remain the **normal evidence owner**: required Go/race, security, host/architecture and lifecycle tests run as selected by the existing fail-closed path classifier. Those check names and branch protection still gate merging. `push` to `main` remains enabled because an explicitly dispatched immutable stable release requires successful **main-SHA** CI and High Assurance Security workflow runs and an exact-main-SHA release candidate artifact from that CI run.

On a main push, both workflows first execute the small, read-only `scripts/ci-reuse-pr-validation.sh` proof. To safely reuse the PR result, the script requires all of the following:

- A single normal non-force/non-creation main commit with the exact pushed parent; a uniquely associated merged same-repository PR whose recorded pre-merge base equals that parent. GitHub's commit-to-PR association endpoint is paginated: the selector explicitly fetches page 1 (at most 100 items) and probes page 2. It accepts page 1 only when its array has fewer than 100 items **and** page 2 is explicitly empty; a full page, nonempty page 2 (including unrelated PRs), missing/malformed response, or API error forces full main validation. This bounded two-page proof deliberately sacrifices optimization for unusually large association sets rather than assuming uniqueness from incomplete API data;
- The merged commit's **Git tree** exactly equals the tested PR head's Git tree, and that PR head contains the parent, so the PR synthetic merge could not introduce untested base changes;
- For each workflow, all pre-merge runs on the same source head SHA/ref, workflow and repository are examined BEFORE the latest result is selected. Each run is classified as exact merged PR, provably foreign PR, or unresolved. A provably foreign run cannot mask a failed/missing exact run, while even one unresolved pre-merge candidate forces full validation rather than allowing an older exact green run to survive. The latest exact run/attempt must be unique, successful and completed before merge.
- GitHub's Actions API sometimes clears `workflow_run.pull_requests` after PR merge. New CI and Security PR runs therefore persist GitHub-event-derived (not PR-title-derived) PR number, base ref and exact base/head SHAs in their persistent GitHub-generated `run-name`. The proof uses the unambiguous `pull_requests` association when present; if that array is **exactly empty**, it may fall back only to an exact canonical event-generated run title. Missing/malformed, contradictory, multiple or foreign linkage always forces full main tests. Older PR runs without this new durable identity may legitimately take the full-validation path once during rollout;
- GitHub API results must be complete (explicit second-page emptiness for commit-to-PR links, and complete run-list counts for workflow evidence) and coherent for event, SHA, repository and workflow, including timestamps and attempt numbers. Tied latest exact runs or multiple associated PRs claiming the same squash merge are ambiguous. Missing, failed, stale, contradictory or unavailable evidence always returns the false reuse decision, triggering the original full validation.

With complete proof, expensive Go/race/system lifecycle/architecture and privileged security jobs **do not repeat** on the same code; the three Debian matrix runners are skipped entirely on proven identical main pushes, while their required PR check contexts remain unchanged. The main CI still **builds, checks and uploads** the uniquely named main-SHA release artifact in the `main-release-artifact` job; the main Security workflow's change-scope job records the successful proof, and the Release workflow retains its existing dual-main-workflow and exact-artifact gates. Docs-only main changes also use the artifact-only job as before. A direct push, force/multiple-commit push, stale PR base, changed tree, failed PR workflow, or API error continues through full main validation. This is no bypass of PR required checks or review authority. CI owns the deterministic selector regression test (`tests/ci_pr_validation_reuse.sh`); the Security change-scope job only syntax-checks and runs that shared selector rather than duplicating the entire classifier test suite.

This distinction does **not** waive a security-sensitive change's author-separated HIGH_ASSURANCE review, a new candidate's exact-head CI, the Release workflow's publication controls, or tests whose actual assumptions changed.

### Native lifecycle integration

The main CI workflow performs a real privileged lifecycle on an Ubuntu 22.04 amd64 GitHub runner, and the dedicated arm64 job performs native build/install/systemd/root-boundary validation on an Ubuntu 24.04 arm64 GitHub runner. The deep amd64 lifecycle covers:

1. install a locally built Agent binary;
2. verify both systemd services are active;
3. verify the installed management/update paths;
4. execute root trust-boundary tests;
5. prove uninstall cannot acquire the exclusive lifecycle lock while a persistent-job-style shared holder exists;
6. create a real transient `ai-job-*.service` guard fixture and prove safe uninstall refuses while it is active without stopping that work;
7. stop the fixture, then safe-uninstall and verify preserved Agent data/users/workspace;
8. reinstall;
9. purge and verify Agent-owned config/state/log/runtime and `aiagent` are removed;
10. verify `aiworker` and `/srv/ai-workspace` remain.

A separate first-run test proves that choosing "Configure later" is a successful core installation, not an installer failure.

## 3. High Assurance Security workflow

`.github/workflows/security.yml` is the stronger validation path for the high-risk surfaces.

### Cloudflare transaction behavior

`tests/cloudflare_transaction.sh` exercises production shell functions with a mocked Cloudflare provider. It covers, among other things:

- manager-side fresh-install bearer handoff via one-use fd without the deleted `mcp.authorization` legacy file, acceptance of both newline-terminated and EOF-only bearer framing, empty/wrong credential fail-closed behavior, and authenticated PUBLIC MCP initialize verification;
- rollback of transaction-created resources;
- preserved recovery state after incomplete rollback;
- current-representation fingerprints before destructive cleanup;
- response-lost DNS/rule create recovery with durable nonce + semantic fingerprint;
- response-lost Origin CA discovery by generated CSR plus complete create-semantic fingerprint and exact-certificate-ID re-read before revoke;
- fail-closed handling of DNS/rule/certificate representation drift, exact-ID read failure, and competing lookalikes;
- recorded Rule-ID ownership checks independent of mutable `ref`, with drift blocking replacement creation;
- marker-based response-lost Rule recovery independent of `ref`, followed by full durable fingerprint verification before deletion;
- exact-rule cleanup rather than deleting a shared Ruleset container;
- management-lock exclusion behavior.

`tests/fresh_cloudflare_handoff.sh` runs on the disposable High Assurance Security runner after a clean fixture teardown. It uses the actual production `install.sh` with noninteractive Cloudflare selection and the locally built Agent binary; the newly issued active MCP credential crosses the real installed manager process boundary on FD3, is verified against the live loopback Agent, and never touches the obsolete plaintext Authorization file. The test deliberately withholds the Cloudflare API token file, asserts a successful *pre-provider* credential check and an expected stop at token acquisition, and verifies that no Cloudflare journal/TLS resource was staged and that local Agent and Executor remain healthy. The fixture is purged on exit. This is a production installer/manager behavioral acceptance, **not** a live external Cloudflare API or native ChatGPT client acceptance. The required success marker is emitted only after successful authenticated local MCP verification, so a missing/corrupt FD3 or the original stale-file path fails the test.

`tests/cloudflare_rulesets_pagination.sh` is retained as the historical test filename, but the production contract it now exercises is phase-entrypoint discovery rather than broad Rulesets pagination. It verifies direct zone phase-entrypoint reads for `http_request_origin` and `http_config_settings`, authoritative 404-as-absence handling, provider-error propagation, entrypoint/rule schema validation, malformed successful exact-Ruleset responses, duplicate Rule IDs, and valid exact/empty Ruleset lookups. A malformed or ambiguous provider representation must never become proof of absence.

The Cloudflare provider is mocked. This is behavioral testing of production transaction/recovery code, not a live-zone integration test.

### Crash recovery

`tests/cloudflare_crash_recovery.sh` performs real process `SIGKILL` after mocked remote creates have committed but before the POST response returns. The durable pre-POST journal and local rollback snapshot must survive, and a fresh process must recover through the production discovery + exact-representation re-read/delete path.

The crash suite covers both:

- DNS response-lost create using the Agent nonce/marker and exact semantic fingerprint;
- Origin CA response-lost create using the generated CSR for discovery, a complete `csr`/hostname-set/`request_type`/`requested_validity` fingerprint, exact certificate-ID GET, and representation-checked revoke.

`tests/cloudflare_phase_recovery.sh` performs real `SIGKILL` + fresh-process recovery at durable local/commit boundaries, including:

- local mutation while phase is `applying`;
- persisted `rolling_back` recovery;
- `committing` with matching trusted managed state;
- `committed` finalization;
- malformed/contradictory journal rejection;
- pending kind/phase/identity relationships;
- required Origin CA pending semantic fingerprint evidence;
- terminal-state consistency.

### Root trust boundary

`tests/root_trust_boundary.sh` and `tests/root_trust_migration.sh` exercise hostile legacy layouts, symlink/replacement attempts, root-only control state, root-controlled state containers and the global lifecycle lock.

The lifecycle overlap tests use the installed management wrapper and the real `/run/lock/ai-server-agent/management.lock` namespace to verify that configure/update/install/purge cannot overlap before mutation. Privileged lifecycle CI also clears that volatile namespace to model reboot, verifies executor startup recreates the root-only directory/file, proves symlinked unsafe state fails closed, and then repeats the shared-vs-exclusive exclusion checks.

`tests/root_trust_boundary.sh` also invokes `tests/stable_bootstrap.sh`, so the initial stable-install privilege handoff is exercised by the existing High Assurance root-trust job rather than by a separate duplicate workflow.

### Privileged shell isolation

Go tests exercise root command environment isolation and the persistent-job command construction. The current persistent-job isolation test uses a fake `systemd-run` command to inspect/execute the generated invocation.

CI does not currently exercise an actual privileged systemd transient job end-to-end. The opt-in `tests/integrated_data_path.py` acceptance fixture below covers real transient jobs on a dedicated Linux/systemd test host. This complements the existing command/environment tests; it is not part of every ordinary CI run.

### Integrated resource-governance acceptance

Completed Issue #42 preserves the accepted resource/data-path contract and exact-run evidence for these opt-in fixtures. Native client typed-artifact work is conditional under #99, not an unresolved #42 or closed #59 test-program gate. These opt-in fixtures prove boundaries that ordinary unit tests cannot exercise together. Build the supplied candidate and test binaries as the ordinary development user after inspecting the source; execute only the identified fixtures as root on a dedicated test host with the existing `aiworker` account:

```bash
CGO_ENABLED=0 go build -trimpath -o /absolute/task/path/candidate ./cmd/ai-server-agent
go test -c -race -o /absolute/task/path/executor.test ./internal/executor
go test -c -race -o /absolute/task/path/browser.test ./internal/browser

sudo env GOMAXPROCS=1 /absolute/task/path/executor.test -test.run '^TestIntegratedExecutorCapacityAndDataPaths$' -test.v -test.timeout 60s
sudo env GOMAXPROCS=4 /absolute/task/path/executor.test -test.run '^TestIntegratedExecutorCapacityAndDataPaths$' -test.v -test.timeout 60s
sudo python3 tests/integrated_data_path.py /absolute/task/path/candidate
sudo env AI_SERVER_AGENT_BROWSER_ACCEPTANCE_RUNTIME=/absolute/task/path/verified-runtime AI_SERVER_AGENT_BROWSER_ACCEPTANCE_WORKER=aiworker /absolute/task/path/browser.test -test.run '^TestBrowserPinnedRuntimeAcceptance$' -test.v -test.timeout 90s
```

The race builds need a C compiler; Python 3 is only a dependency of this optional test fixture. Neither is added to the Agent's core host footprint. Without the opt-in environment/privilege prerequisites, the Go fixtures skip; a skip is not live acceptance evidence.

- The executor fixture saturates separate worker/root budgets with noisy commands, concurrently reads regular-file ranges and a growing retained job log through the real Unix-socket handler/client, then verifies capacity recovery. The one/four `GOMAXPROCS` cases exercise different admitted capacities, rather than duplicate the same proof.
- The MCP/systemd fixture launches its own loopback MCP server and root executor with temporary credentials/state. It measures complete small output and bounded head/tail wire responses, binary/ranged-file behavior, real job replay/conflict/capacity, logical log retention and job survival across restart of those fixture processes. It preserves unrelated active jobs and stops only its own transient units. It never installs, updates or restarts the connected Agent services. If cleanup cannot be verified, it retains the fixture for recovery.
- The browser fixture uses a verified task-owned copy of the pinned runtime with its own socket/profile/state. It verifies shared cookies/storage, default TLS and a scoped exception, timeout/busy/recovery, bounded output and disposable-artifact refusal. Its reconstructed manifest proves runtime execution compatibility, not `browser_setup` provenance or convergence; existing setup behavioral tests cover those separately. Verify copied runtime versions, locked dependency identities and browser content-tree hashes against the release manifest before running it.

A successful direct wire fixture is not proof that every supported AI client can see `structuredContent`. Issue #42's direct-client/resource acceptance is complete; model-visible preview, fallback, binary/artifact and client-budget gaps belong to #99 only when real clients expose a material unmet need. Wire-size tests and small text-fallback unit tests are useful but cannot replace current-client evidence; do not increase the global frame cap to compensate.

### Bounded privileged file path

Metadata-first workspace_stat tests require size/version-only results without payload bytes for empty, binary and multi-GiB sparse files, bounded windowed reads with exact version, plus rejection of stale versions, symlinks, FIFO and traversal. This uses the same worker-only Landlock/openat2 authorization boundary; real model-visible native typed-artifact capability remains conditional under #99. Go tests exercise the root file-tool data contract without broad host mutation: ranged reads, raw offsets/continuation metadata, binary base64 encoding, file-version consistency conflicts, the 1 MiB read/write bounds, atomic replacement, mode preservation, `must_not_exist`, missing-parent refusal, symlinked protected-path approval, final-symlink rejection and FIFO/special-file rejection.

The staged worker binary-transfer suite covers 110-byte, 32 KiB, over-5 MiB and 33 MiB fixtures, per-chunk and whole-object SHA-256, idempotent begin/chunk/commit, durable offset+SHA-256 preparation checkpoints, stale destination and integrity refusal, mismatched replay, disk/session quotas, terminal retained-data accounting, expired/aborted state, symlinks, traversal and principal/workspace/destination mismatch. Disposable child processes use real `os.Exit` fault injection after `begin` mkdir/data/manifest, after prepared temp creation, before and after a prepared window checkpoint, after commit intent, after rename but before durable receipt, and after aborted/committed receipt but before disk cleanup; later calls prove exact byte identity and forward progress without re-copying checkpointed windows or double-publishing. Crash/GC tests additionally prove `begin_after_mkdir` and `begin_after_data` material reservations survive independent GC and reject a different-material same-key request, then expire and release the key. Controlled >2-second stalls at begin and chunk checkpoints verify fresh TTL assignment. An `O_TMPFILE` crash before the durable inode journal exposes no named file; after the journal, both before-link and after-link crashes recover. Substituting an unrelated worker-owned regular file at the deterministic name after the pre-journal crash proves `commit`, `abort`, and GC all fail closed without deletion. For existing-file atomic replacement, real killed child tests inject a concurrent rename just before exchange, kill immediately after RENAME_EXCHANGE and after verified rollback, and prove status recovery restores the competing inode and journals terminal `conflicted`; another test requires durable `exchange_verified` before deleting the displaced original, and missing displaced proof refuses committed success. A dedicated no-client-traffic GC test proves expired staging and target temp cleanup, including expired interrupted existing-file replacement rollback. `TestWorkspaceTransferActualWorkerHelperLandlock` compiles the current Agent and exercises real aiworker self-exec/Landlock including multi-window commit and startup periodic expiry; in ordinary unsupported environments it skips, but the exact-candidate CI proof sets `ASA_REQUIRE_TRANSFER_LANDLOCK=1`, fails if the platform lacks Landlock ABI v2 and explicitly rejects a skipped acceptance. Source-side success is not proof of installed Stable, native ChatGPT attachment handling or typed model content. Fixture dimensions are not product-wide file ceilings.

The optimistic existing-target path also has deterministic commit-boundary race hooks used only by executor tests. Regression coverage replaces the destination after the final precondition lookup but before commit and proves exchange/identity verification restores the concurrent object and returns `file_changed`; replaces the pathname after the Agent commit but before final verification and proves the result is `unknown_completion`, never `written`; verifies unchanged versioned replacement still succeeds; and verifies a new destination appearing at commit time is preserved by `RENAME_NOREPLACE`.

The implementation resolves parent directories and read targets through stable file descriptors, so the tests assert behavior through the same production helpers rather than a separate mock path. Repository CI/race runs own the broader exact-head evidence; #99 still requires direct AI-client presentation and typed-artifact acceptance.

### Bounded browser path

Go tests cover the browser slice without mutating a live external site or installing host packages. They verify the 128 KiB script bound, 90-second default/five-minute maximum timeout contract, fail-fast browser mutex semantics, setup approval boundary, secure TLS default plus explicit request-scoped exception, temporary-file script handoff, per-run working-directory confinement for ordinary relative artifacts, the 256 MiB disposable-data cap plus state-filesystem reserve, lifecycle-lock exclusion for both full setup and already-current cleanup, disposable cache cleanup contract, exact embedded Playwright lock/integrity pins, architecture-specific browser content-tree digests, and the pinned runtime convergence command, single-backup crash recovery, and fail-closed ambiguous-backup handling.

A synthetic runtime fixture verifies that `browser_status` requires the desired manifest, actual Node version, exact Playwright package version, Playwright Chromium/FFmpeg metadata, expected installation markers, and the architecture-appropriate Chromium/headless-shell/FFmpeg executables as non-symlink regular executable files before reporting ready. MCP discovery tests verify `browser_status`, `timeout_ms`, `ignore_https_errors`, shared-profile disclosure, TLS-default disclosure and typed structured browser output. Exact-head CI owns the race-enabled Go test pass and supported-platform lifecycle matrix. Live browser setup remains an explicit host-package mutation and is not performed merely to satisfy unit review evidence. First-class Browser/E2E behavior is owned by #63, AI-facing artifact presentation and client sizing by #99/#59; #42's resource foundation is already accepted.

## 4. Stable installer and updater trust

### Stable bootstrap

Initial Stable installation separates the **mutable, reviewed `main` bootstrap source** from the immutable release installer it authenticates. Both documented one-line commands fetch `scripts/install-stable.sh` from the same permanent `main` URL with no commit or version in the URL. The default resolves GitHub's latest published immutable Stable; `bash -s -- vX.Y.Z` selects a specific published immutable tag. The mutable bootstrap does **not** have the independent provenance of an immutable pin, so branch protection/review is a separate supply-chain control. The release installer must still be authenticated before privileged execution, with fail-closed staging and SHA-256 verification retained. Since `main` is immediately public, a bootstrap change must work with the latest **already published** Stable before it merges, not only with a future unshipped release.

`tests/stable_bootstrap.sh` guards the documented permanent `main` bootstrap URL and explicit-version example and uses deterministic mocked GitHub HTTP and `sudo`/APT boundaries (no real package changes) to verify that the bootstrap:

- accepts a valid latest immutable release and an explicit valid stable tag **on a fresh host**, both with `jq` already installed and after its simulated absence/prerequisite provisioning on supported Debian and Ubuntu hosts;
- rejects existing/partial Agent installations before release lookups or optional APT operations and again at the privileged handoff if an installation appears during the bootstrap;
- requires exactly the expected tag-scoped `install.sh` asset representation;
- rejects a mutable release before downloading or executing the release installer;
- rejects an unexpected asset URL before installer download/execution;
- rejects an installer digest mismatch after download but before the privileged handoff;
- copies the candidate through the privileged boundary into a root-controlled staging directory and re-verifies the same expected release-asset digest there before execution;
- fails closed when the invoking-user-writable installer pathname is adversarially replaced after the first verification but before privileged staging;
- proves that neither the original nor substituted installer executes when privileged re-verification fails.

This test is behavioral for bootstrap control flow, privilege ordering, and the local verified-path replacement threat. GitHub Release HTTP and the `sudo` command are mocked; the root-trust High Assurance job executes the same bootstrap control flow under the privileged supported-runner context. When that job already has an Agent installation, only fixed Agent-presence paths in the test's bootstrap copy are redirected to private fixtures; actual installed-path refusal is verified separately below. The test does not prove future GitHub repository settings or external network behavior.

On the separate disposable installed-systemd CI runner,
`tests/credential_installed_rollback.sh` additionally proves that the
**actual public bootstrap entrypoint** refuses occupied installs containing an
enabled retired Gateway record or active/revoked secondary credentials without
changing the installed binary, config, store or services. After explicitly
confirmed credential cleanup, the **protected updater** uses the published
immutable `v0.1.14` release asset to complete a real downgrade. The other
historical compatibility assertions build the binary from the immutable
`v0.1.14` source commit; that source-built binary is not itself the published
release artifact or proof of a published-release installation.

### Release-scoped installer

The security workflow builds the candidate release assets and verifies:

- `install.sh` is generated with the expected version/ref at the release-scoped header;
- stable binary override is rejected;
- corrupted archive bytes are rejected by `SHA256SUMS` verification;
- the release builder emits and checksum-verifies every architecture declared in `scripts/release-arches.txt`, currently amd64 and arm64.

The release-scoped installer is not its own trust root; the stable bootstrap or already-installed trusted updater authenticates its bytes before execution.

### Stable updater

Updater tests use a mocked GitHub HTTP surface so they can exercise trust decisions deterministically without depending on an unpublished release.

They verify that the updater:

- refuses a mutable/non-immutable stable release before installer execution;
- accepts a valid immutable release asset only when `install.sh` bytes match the GitHub release asset SHA-256 digest;
- rejects digest mismatch before executing the downloaded installer;
- never turns the stable path into an implicit `main` source update.

These tests validate updater behavior. They do not prove the future repository settings used at release time.

## 5. Release-provenance validation

The release workflow is structurally checked in High Assurance Security. The checks verify that it is manual-dispatch, exact-SHA based, requires successful `main` CI/Security provenance, promotes the CI artifact, performs create-only exact tag creation, uses `--verify-tag`, and verifies immutable release/attestation state after publication.

### Operator-only repository properties

Some release properties cannot be authoritatively proven by the workflow's `GITHUB_TOKEN`.

In particular, GitHub may omit Rulesets `bypass_actors` unless the caller has sufficient ruleset access. Therefore CI must **not** interpret an omitted field as an empty bypass list.

Immediately before release dispatch, the operator must verify the actual repository settings and provide the workflow's explicit confirmations for:

- release immutability enabled;
- active release-tag protection covering the intended release tag pattern, blocking update/delete and having no bypass actor that can move/delete the release tag.

Those are human/operator gates, not green-CI claims.

## 6. Static contract checks

CI contains grep/static assertions for security-sensitive implementation shape, including examples such as:

- root shell startup flags/environment;
- expected management/menu/install identity paths;
- Cloudflare hostname-scoped rule constructs;
- no whole-zone SSL mutation fallback;
- native TLS/bearer configuration;
- release workflow ordering and required controls.

These checks are useful regression tripwires. They are **not** independent proof that the production behavior is secure or correct. High-risk invariants should have behavioral tests where practical, and a static contract should be removed or revised when it no longer represents a real invariant.

## 7. What CI does not prove

A green CI/High Assurance result does not by itself prove:

- live Cloudflare API permissions or behavior on a real user zone;
- real public DNS/TLS propagation;
- ChatGPT Business tool discovery and end-to-end MCP use (this proves the current ChatGPT integration only, not universal AI-client compatibility);
- an end-to-end `start_job` MCP/executor call through the production fixed runner and a real transient `systemd-run` unit (CI does exercise a real matching transient unit for the uninstall guard, but that is not a substitute for this direct-client path);
- current repository Rulesets bypass configuration;
- future release immutability settings before publication;
- production VPS behavior outside the supported validated target.

Do not replace these gaps with grep tests that merely search for reassuring strings.

## 8. Developer checks

On a compatible development host, after coherent local commits, prefer the shared planner:

```bash
bash scripts/dev-check.sh --base origin/main --head HEAD
# or inspect selection without running local checks:
bash scripts/dev-check.sh --base origin/main --head HEAD --plan
```

Use the actual integration base for stacked/alternate-target work. The helper validates the committed base-to-head range, runs the safe local Go/shell checks selected by `scripts/ci-change-scope.sh`, and prints the remaining CI/dedicated-environment obligations. When the host has no C compiler, it runs ordinary Go tests locally and leaves race detection explicitly to CI rather than forcing a host-wide compiler install for the fast path. During active editing, keep using the narrowest discriminating test first. For a deliberately full local sweep, use:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race -vet=off ./...
bash -n install.sh update.sh uninstall.sh manage.sh ensure-lifecycle-lock.sh scripts/build-release.sh scripts/install-stable.sh scripts/ci-change-scope.sh scripts/dev-check.sh tests/*.sh
bash tests/stable_bootstrap.sh
```

For privileged/cloudflare/root-boundary changes, run the applicable security tests on a disposable supported environment matching the affected platform when practical. Ubuntu 22.04 amd64 remains the reference High Assurance environment; architecture-specific behavior also receives native arm64 lifecycle coverage. Never point test fixtures at the live Cloudflare zone or a production VPS.

## 9. Candidate review and release validation

Independent HIGH_ASSURANCE review is an integration/release gate for a frozen candidate, not an iterative lint service for a moving implementation. During active development, use self-review, targeted behavioral tests and current CI to converge. When the accepted feature scope is complete, freeze an exact base/HEAD and obtain independent review if the risk/profile requires it. If a BLOCKER/REQUIRED finding changes the candidate, that review identity is obsolete; fix the root cause, revalidate and refreeze before another independent gate review.

Waiting for an independent review is not a reason to idle unrelated development. Safe independent work may continue from the appropriate current base in a separate worktree while a candidate is frozen. Do not modify the frozen candidate, merge work ahead of it, or move an exact reviewed integration target when that would invalidate the review envelope; serialize only the affected final integration/review path.

GitHub Copilot pull-request review is not part of this project's review model. Do not request, enable, or use Copilot review for iterative review, project review evidence, or an independent HIGH_ASSURANCE gate. Historical Copilot review results may be retained as past finding context, but they do not satisfy a current review requirement.

When review independent from the Master is required, the Master must prepare a ready-to-paste `INDEPENDENT REVIEW CHAT` prompt using the `github-project-orchestrator` independent-review handoff. The user relays that prompt to a separate fresh reviewer context, person, or review tool. The returned review must identify the exact candidate SHA it reviewed, state `APPROVE` or `CHANGES_REQUIRED`, and list evidence-backed findings; the Master then reconciles the result and refreshes candidate/base identity before relying on it.

Before a stable release is published:

1. the intended change must be reviewed against the current base-to-head diff;
2. PR-head CI and High Assurance Security must be green for the exact frozen candidate;
3. required independent review must pass the exact frozen candidate;
4. after integration, **main-push** CI and High Assurance Security must be green for the exact release SHA;
5. the release workflow must promote the exact CI-produced artifact for that SHA;
6. the operator repository-setting gates must be verified immediately before release dispatch;
7. post-publication checks must verify immutable release state, exact tag SHA and release/asset attestations.

For real VPS validation, use a dedicated/replacement supported server. Human-operated privileged validation should proceed one safe operation at a time, with the expected result and rollback understood before the next state mutation.

## 10. Preservation acceptance

Any lifecycle validation involving uninstall/purge must continue to prove:

- `/srv/ai-workspace` is preserved;
- `aiworker` and its group are preserved;
- safe uninstall preserves Agent configuration/state needed for reinstall/repair;
- purge removes Agent-owned config/state/log/runtime and `aiagent` without silently deleting Cloudflare resources.

## 11. Developer Runtime target validation

The detailed target capability architecture is `docs/DEVELOPER-RUNTIME.md`.

> This section defines acceptance for the accepted target architecture. Its principal feature surfaces are already present in `main`; successful source/CI tests must **not** be confused with optional runtime readiness, real connected-client acceptance or a published Stable release. Record those layers separately in #59.

Developer Runtime validation should prove the common high-value development loop rather than synthetic parity with a complete human IDE.

Representative integrated path:

```text
recover exact repository/worktree
-> text / structural / semantic inspection
-> safe edit/refactor
-> build/test/job
-> interactive PTY when needed
-> DAP debug when runtime state matters
-> Playwright browser/E2E validation when user-facing behavior matters
-> diff / commit / push
-> recover again from GitHub evidence
```

### Structural search

`internal/executor/workspace_search_test.go` keeps the first-class `workspace_search` surface bounded and read-only. Tests cover explicit workspace/path containment including symlink escape rejection, structured zero-based file/range/capture output, no-match exit semantics, result-limit and timeout partial-state reporting, malformed/out-of-workspace engine output rejection, engine/version validation, repository-config isolation through `--config /dev/null`, and process cancellation when enough results have been collected. MCP discovery tests preserve the text -> structural -> semantic responsibility ladder and prevent rewrite/apply inputs from entering the public schema.

A gated real-engine acceptance (`AST_GREP_ACCEPTANCE_BIN`) runs the same production wrapper against an explicitly supplied ast-grep binary. The #62 candidate evidence exercised both the minimum-supported 0.40.5 contract and 0.45.3 with release-asset SHA-256 verification before execution. A separate rewrite-plan evaluation proves ast-grep can calculate a replacement while the source checksum remains unchanged; the first-class tool exposes no automatic apply path. Managed installation is intentionally not performed merely to satisfy a read-only search call.

### Direct MCP / real gopls semantic acceptance

The opt-in `TestDirectMCPGoSemanticClientAcceptance` verifies the actual **public** bearer-authenticated MCP Streamable HTTP path, its private Unix Executor transport, worker-only Landlock file inspection, root-owned trusted gopls binary and response presentation. The source-built Agent/Executor and workspace are disposable test fixtures; **no installed Agent or Executor service is upgraded or restarted**. A local accepted-host command is:

```bash
ASA_MCP_GOPLS_ACCEPTANCE=1 go test -count=1 -timeout=5m -v ./internal/mcp -run '^TestDirectMCPGoSemanticClientAcceptance$'
```

This requires non-root Linux, working Landlock, a Go toolchain on the worker's standard safe PATH and an operator-provided trusted `/usr/local/bin/gopls` or `/usr/bin/gopls`. Standard unit tests intentionally skip the optional real-language-server test. The `lsp_changed` CI job provisions **version-pinned gopls v0.23.0** in the disposable GitHub-hosted runner's root-owned `/usr/bin/gopls` production-like path; CI rejects any higher-priority `/usr/local/bin/gopls`, proves the selected executable is byte-identical (SHA-256) to the version-pinned build and trusted by the unchanged production lookup, and explicitly **fails if the public-client proof is skipped**. It preserves the existing private executor real-gopls proof too.

The complete consumer scenario covers a real 53,153-byte Go document; `code_definition`, `code_references`, `code_symbols` and `code_diagnostics`; SDK-visible typed structured responses and bounded complete small text fallbacks; reconnecting a fresh MCP session; worker-derived cross-file target versions before and after a separate version-pinned workspace edit; stale source rejection; changed-source diagnostics; and refusal of out-of-workspace source paths. It logs a sample elapsed duration and source SHA-256, **not** installed-server CPU/RSS/wire-size metrics. This is a direct Go MCP SDK *source-build* acceptance, not proof of native ChatGPT UI model visibility, installed-current-main readiness, arbitrary-size LSP, root-to-worker privilege separation or Stable publication. The installed/client/restart/resource delivery gates remain owned by #56 and #59.

### Repository environment/toolchain discovery

`internal/executor/environment_test.go` validates the lightweight #64 discovery/reuse boundary against real temporary Git repositories. Coverage includes fresh-repository recovery from tracked manifests, compatible/incompatible worker-cache toolchain reuse, native language and package-manager declarations, package scripts without execution, mise/devenv/Dev Container/Dagger detection without automatic provisioning, explicit multi-mechanism/package-manager selection state, conflicting exact pins, untracked declaration durability warnings, workspace path-escape/symlink rejection, and bounded tool-version output. `internal/mcp/server_test.go` also keeps the public tool read-only, idempotent, local-only and bound to an explicit repository path with a typed output schema.

These tests deliberately do not install environment managers or run repository tasks. Managed provisioning for a concrete mechanism requires its own scope/evidence; this slice proves discovery/reuse and clear failure/unknown states only. Repository-owned dependencies/toolchains remain distinct from Agent-managed capability tooling.
### Repository/worktree lifecycle

`internal/executor/repository_test.go` exercises the first repository/worktree Layer-1 slice against real temporary Git repositories. Coverage includes clean and staged/unstaged/untracked state, detached HEAD, upstream ahead/behind divergence, merge-conflict/in-progress state, remote-identity discovery independent of directory names, linked-worktree deduplication, branch-already-in-use rejection, exact-start-SHA staleness, fresh-clone recovery from a durable remote branch, explicit remote-verification failure, main-worktree protection, and removal only after clean exact-HEAD plus remote-durability proof or an explicit disposable decision. Regression tests also prove structured Git does not execute repository hooks/external fsmonitor configuration, refuses checkout-filter execution, keeps working-tree and Git/common metadata inside the configured workspace, and rejects optimistic removal when HEAD changes before the final mutation.

The structured surface intentionally does not wrap ordinary Git verbs. Push/commit/fetch/diff/log remain CLI operations; remote reconciliation uses `repository_inspect` when exact remote-HEAD proof is required. CI remains the authoritative Go-version/race/platform validation when the local development host cannot faithfully provide the repository-declared environment.

### Value evidence

For each Layer-1 surface, prove the reason it was promoted above direct CLI use:

- repository/worktree identity prevents wrong-checkout ambiguity;
- safe editing detects stale/concurrent mutation and reports partial outcomes honestly;
- structural search proves at least one real AST-pattern query that regex/text search cannot express robustly and LSP does not naturally own;
- LSP proves semantic diagnostics/navigation/reference value;
- PTY proves persistent interactive prompt/TUI/control-key behavior;
- DAP proves breakpoint/stack/scope/variable debugging through the Go + Delve reference path;
- Browser proves compact Playwright-backed UI/E2E inspection and keeps one browser runtime/profile ownership model.

Do not add a test matrix or managed dependency for a deferred capability merely to claim completeness.

### Bash Shell DAP source acceptance (#149)

The worker-only Bash structured DAP uses the existing authenticated MCP `debug_*` operations and source-version-bounded worker cgroup session broker. Test `TestShellDAPWorker*` against an **actual** `/usr/bin/bash` with disposable test-local DAP pipes: stopOnEntry, qualified original-script breakpoints, provisional (not falsely verified) source-line status, bounded scalar-variable scopes, function stepIn/next/stepOut, strict Bash exit semantics, pre-activation no-user-script parsing (even malformed syntax), original Bash PID preserved across gated exec, bad launch/symlink/unknown-command refusal and graceful termination. The Executor (not the worker unit) owns the original source `file_version`/principal decision. The adapter's one bounded, descriptor-anchored worker read rechecks the same version before/after obtaining the bytes and seals the script in a nonwritable memfd; tests must prove **pathname replacement, symlink redirection and in-place writes after snapshot** never change executed bytes, verify sealed kernel flags, fail closed for stale/oversized files, and retain DAP source-line mapping. Existing `file_version` is metadata-based and can collide on very rapid same-size edits on coarse-timestamp filesystems; the immutable snapshot guarantees no further pathname reopening but is not a new cryptographic version scheme. Additional race tests require exact ascending outbound seq values in wire order under concurrent stopped/output/response events, verified original request/response identities, and post-spawn READY timeout/early-exit or START write failure irreversibly restricting the same session to status/stop. **Privileged IPC acceptance:** The production helper refuses startup without the original root broker's four exact root-owned 0600 pipe FDs. The original broker requires Yama `ptrace_scope >= 1` (same-UID `pidfd_getfd` uses `PTRACE_MODE_ATTACH_REALCREDS`) and fails closed where Yama is absent or zero; it does not change sysctl. The installed Root Executor uses `Group=aiagent` while retaining euid=0: all inherited protected pipe inodes must be explicitly set root:root 0600 by that original root broker (or fail closed), rather than inheriting the service's effective `aiagent` group. The disposable installed-service CI also repeats the real root IPC rejection/activation test as root with egid=aiagent; a root:root-only invocation cannot prove this group-boundary invariant. The shared root-broker private DAP cgroup creator likewise normalizes and verifies root:root 0700 before child admission, because cgroupfs directories created by root in the `Group=aiagent` unit may otherwise inherit GID=aiagent. The installed CI test requires the original private cgroup to be root:root 0700 and observes actual Bash membership and broker cleanup; it must not weaken that provenance check. The disposable root-only test `TestShellDAPRootOwnedIPCRejectsSameUIDPeerAndPreservesActivation` starts an actual worker-UID adapter and Bash using root-created pipe capabilities, verifies both worker-peer `/proc/PID/fd` reopen and `pidfd_getfd` denial for adapter and Bash, then confirms exact original Bash PID, stopped-event and authenticated activation still work. GitHub CI must execute this test **non-skipped** with `ASA_REQUIRE_ROOT_SHELL_IPC=1` on a fresh runner; ordinary worker unit tests alone cannot satisfy this IPC trust gate. Worker-side close is not root-cgroup cleanup proof. The separate opt-in `TestShellDAPInstalledPrivateCgroupLifecycleCI`, run non-skipped **only** in the disposable Ubuntu 22.04 GitHub CI runner after the source-installed Agent/Executor services have started, uses the production authenticated Unix Executor and one test-owned aiworker script to verify Root-created original private cgroup, actual worker Bash member, entry/breakpoint/stack/variables, denied unrelated principal, and broker-verified `debug_stop` cgroup removal. It never changes Yama or executes against occupied hosts; failure to obtain a real private-cgroup proof is a test failure, not a silent fallback. A prior disposable Ubuntu 22.04 CI runner had root-owned but world-writable `/usr/local/bin` (mode `0777`); the production `systemexec.Trusted` correctly denied the Agent executable there. CI first verifies that exact directory is a genuine root-owned, non-symlink directory and tightens it to `0755` **on the fresh GitHub runner only**, preserving the original production trust check. This CI-only preparation is not permission to change arbitrary real-host directory ownership or modes. This confirms a disposable source-installed Root Executor path, **not** published Stable, externally provisioned optional adapters, native arm64 installed Shell behavior or a connected ChatGPT client. `TestPublicDebugMCPPrincipalAndBoundedTypedArguments` verifies direct authenticated MCP input mapping for `language=shell, mode=bash` without granting root execution. Root-owned Agent/binary inode, native child Bash pidfd, atomic private cgroup membership before script activation, cross-principal/workspace rejection and verified descendant stop require disposable root-Executor/installed-client evidence; **unprivileged unit tests cannot prove these facts**. Do not bypass audit or attach an occupied Agent to obtain a synthetic green result. Extension-host JS/TS DAP remains deliberately unsupported. The Shell v1 hook is a debugging aid for cooperative Bash scripts, not a security sandbox or full POSIX-shell evaluator. Post-merge source, published Stable, installed server and actual connected-client acceptance are separate gates.

### Failure and recovery

Exercise as applicable:

- wrong/stale workspace identity;
- concurrent file change during structured edit;
- structural/LSP result bounding;
- LSP/DAP crash and restart;
- DAP adapter/debuggee abnormal exit with actual worker/Delve when supported: public breakpoint/stack baseline, single-use target identity, crash/exception evidence, `debug_status` `adapter_exited`/`cleanup_verified`/`adapter_exit_code`, fail-closed post-exit actions, verified `debug_stop` session retirement, new worker session capacity, and unverified cleanup remaining addressable; protocol `terminated` differs from an unexpected adapter crash. Deterministic `TestDAPStatusPoll*` coverage verifies event+EOF **before** broker completion does not poison terminal evidence. `TestDAPBrokerFirst*` uses a buffered Unix socket (not synchronous `net.Pipe`) to verify the inverse ordering: adapter writes complete terminal event/partial malformed suffix and closes -> broker has already exited and verified cleanup -> first status consumes buffered frames, preserves valid terminal events, marks malformed tails `uncertain`, never transmits on the dead DAP stream, and never reports a final protocol classification before exhausting the bounded input. The 12-frame read budget is explicitly tested across two polls with retained event ordering and no replay. Additional tests preserve sticky unknown sent `evaluate` and prevent concurrent stop/status double retirement; generic race checks alone do not prove these logical interleavings. `TestDAP*Recovery*` and `TestDAP*Termination*` run without optional Delve; the opt-in `TestRealGoDelveCrashKillsDebuggee` follows real Delve and worker process identities. On Linux kernels lacking group-scoped pidfd signalling (for example a 6.8 test host), the non-root real fixture fails closed; do not disable process containment to make it pass or infer production failure from this fixture alone. Production DAP uses root-owned cgroup v2 containment, requiring separately authorized installed-client acceptance; a green generic CI is not equivalent to this installed proof.
- PHP HTTP Service Job DAP (#149) has two **distinct** evidence layers. Unprivileged `TestPHPHTTP*`/`TestPHPDBGp*` protect Service Job claim/principal/workspace identity, bounded root-manager unit property parsing, deterministic 32-hex per-service socket/trigger, original PHP argv/127.0.0.1 listener-inode verification, same-UID spoof refusal, one-shot request admission under race, *kernel socket-inode and TCP 4-tuple request-specific proof with competing accepted localhost connections, including half-closed `CLOSE_WAIT` sockets*, nonblocking terminal-proxy failure health, typed before-send refusal even at either asynchronous post-Dial gate, atomic-before-write refusal and inspectable diagnostics, cancelled HTTP client-only detach, and preservation of socket replacement files. The **real** `TestApprovedRootPHPHTTPServiceOwnedDAP` is opt-in and **skips unless** a separately approved root test is executed on its pinned nonproduction fixture host with `ASA_ROOT_PHP_HTTP_DAP_APPROVED_HOST` set to that host; it never runs in ordinary CI. It creates **only its own transient systemd Service Jobs** (original PHP and a separate disposable fallback-stop fixture) and scratch files (never changes installed Agent services), through the real `start_job` claim writer and original `job-runner` handoff in the test-owned Go executable (rather than fabricating a protected claim), and preserves scratch on uncertain unit cleanup; requires a pinned original PHP 8.4/Xdebug/Node adapter chain, proves a genuine PHP HTTP request hits an exact file-version breakpoint through a kernel-verified Unix DBGp peer, obtains the actual PHP source stack, then `debug_stop` cleans its own adapter/socket while the **original unit, pinned PHP process and subsequent untriggered HTTP response survive**; it additionally verifies the separate adapter cgroup, wrong principal/claim/port/trigger/source refusal, one-shot HTTP dispatch, rejected forged DBGp peer, adapter SIGKILL containment and original Service survival, drift refusal after stopping the original Service, and an explicit test-owned `job_stop` with durable `verified_stopped` proof. Fallback cleanup verifies the captured original InvocationID, cgroup, MainPID and pinned runner against live systemd, **signals only that original runner through its pidfd**, waits for original exit and systemd inactivity, and **never calls name-targeted systemctl stop**; a replaced unit remains untouched and ambiguous resources are retained. Deterministic replacement-after-final-check and genuine original-PIDFD-termination tests cover that boundary. The opt-in real-root fixture also has a deterministic competing triggered localhost HTTP test after Agent request admission but before Dial, plus a terminal-invalid-peer-before-request test, both asynchronous post-Dial provenance-failure branches with a PHP execution witness, and actual fallback PIDFD-only cleanup of a separate test-owned systemd Service; those opt-in cases require a separately observed non-skipped PASS, never inferred from default CI. Test-gated source checks or an earlier standalone protocol probe do **not** satisfy this integration proof; a skipped fixture must not be reported as passing. New public MCP schemas additionally require a currently connected client to refresh tool discovery, and release-installed capability remains separate.
- stateful-session stale ID/principal mismatch;
- PTY output flood, reconnect, resize and long-lived soak;
- browser hang/output/artifact bounds;
- Agent/executor restart;
- full development-server loss with GitHub-only recovery.

### Performance/complexity guard

Measure representative workflow latency, remote/tool-call count, idle/active process footprint and recovery friction where useful. Do not introduce caches, daemons, alternate backends or infrastructure solely from theoretical performance concerns; escalate only from measured or repeatedly observed need.

Optional code-server, managed Incus, Ctags, alternate terminal backends and generic provider/tool-manager frameworks are not Core-v1 acceptance requirements unless their contracts are explicitly reactivated from new evidence.

### Shared-host first-install recovery target (Issue #137; verify the installed release)

The authoritative acceptance cases are maintained only in
[Issue #137](https://github.com/ach1992/ai-server-agent/issues/137).
For each release where this source feature is included, validate on an **authorized clean disposable
Agent host**, not by replacing an occupied service:
two Git worktrees plus a valid non-Git workdir; correct worker CWD,
shared private worker HOME/XDG across Command/Job/Terminal/LSP/DAP and
root/workspace mismatch rejection;
lost-chat new-job and same-principal terminal discovery via passive
read-only `runtime_list` (no cleanup or cross-principal leaks); material start
idempotency and unknown crash status; independently bounded task/service
slots (with service labels) on the same systemd engine; protected
uninstall/purge; Browser shared/isolated storage and its **existing
`browser_status`**, and bounded error/resource behavior. Keep #59's
full GitHub code/build/test/push/recovery acceptance separate.

**Do not create historical upgrade/migration test fixtures** for previous
Agent installations, claims, profiles, tool schemas or old client state.
Observe exact candidate/installed identity, cross-project safety and
resource footprint. The ~90% goal guides simple high-value scope, **not**
permission to skip required acceptance items; security/data-loss defects block.
