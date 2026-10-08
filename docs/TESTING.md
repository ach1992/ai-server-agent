# Testing and Validation

This document describes the validation model that applies to the current v0.1 codebase. It is not a historical release log.

## 1. Supported platform policy and validation matrix

The stable platform policy validated by current `main` is:

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

The executor-foundation tests specifically exercise separate non-queueing worker/root capacity, structured resource-limit results, bounded timeout selection, graceful TERM/KILL process-group cancellation, same-process-group background cleanup, peer-close context cancellation, and both sides of the private executor response-frame limit. The command/jobs tests additionally cover production-time head/tail output bounding, binary-safe base64 results, physical persistent-log bounds with logical retention offsets, protected command-file consumption, idempotent retry/conflict behavior, raw-command absence from Agent-created systemd argv/audit/idempotency claims, and fail-fast persistent-job capacity, replay admission enforcement, interrupted-job reconciliation, and bounded terminal-artifact retention. Recovery regressions specifically cover failed `systemd-run` with a loaded-but-inactive unit, authoritative active-unit state dominating worker-writable numeric/unknown status markers, command-handoff retirement across runner setup/validation failures, surfaced terminal-cleanup errors, and shared lifecycle-lock exclusion held through persistent-job launch. These tests complement, rather than replace, the privileged lifecycle/security jobs.

### Shell syntax

CI validates the established installer, updater, uninstaller, management, release-builder and security-test paths. `tests/stable_bootstrap.sh` also syntax-checks the stable bootstrap before exercising it behaviorally.

CI change scope is fail-closed and path-aware. Changes limited to `README.md`, `AGENTS.md`, `SECURITY.md`, `LICENSE`, `.gitignore`, or `docs/**` do not re-run runtime/OS/security suites that cannot validate those files. The always-run change-scope job validates the exact base-to-head diff with `git diff --check`; required non-matrix jobs are conditionally skipped (GitHub reports a skipped job as successful), while the Debian matrix keeps lightweight per-matrix checks present and skips only its expensive host validation. Any classifier failure forces the required validation jobs to fail rather than silently skip; a missing/unknown classifier output also falls back to the full suite and only explicit `runtime_changed=false` may skip heavy validation. Runtime, test, script, workflow, dependency or other unrecognized paths fail safe to the full suite. A docs-only `main` push still builds and uploads the exact-SHA release candidate artifact because the release workflow promotes CI-produced artifacts for the selected main SHA.

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

Issue #42 owns the current acceptance state and exact-run evidence. These opt-in fixtures prove boundaries that ordinary unit tests cannot exercise together. Build the supplied candidate and test binaries as the ordinary development user after inspecting the source; execute only the identified fixtures as root on a dedicated test host with the existing `aiworker` account:

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

A successful direct wire fixture is not proof of ChatGPT's presentation of `structuredContent`, or real Gateway forwarding. Those require the current supported client and an implemented compatible Gateway connector. Likewise, wire-size measurements alone do not settle the synchronous inline budget's client/Gateway usability requirement. Keep the unresolved checks open in Issue #42; do not replace them with a mock proxy, a higher global cap or a repeated broad unit suite.

### Bounded privileged file path

Go tests exercise the root file-tool data contract without broad host mutation: ranged reads, raw offsets/continuation metadata, binary base64 encoding, file-version consistency conflicts, the 1 MiB read/write bounds, atomic replacement, mode preservation, `must_not_exist`, missing-parent refusal, symlinked protected-path approval, final-symlink rejection and FIFO/special-file rejection.

The optimistic existing-target path also has deterministic commit-boundary race hooks used only by executor tests. Regression coverage replaces the destination after the final precondition lookup but before commit and proves exchange/identity verification restores the concurrent object and returns `file_changed`; replaces the pathname after the Agent commit but before final verification and proves the result is `unknown_completion`, never `written`; verifies unchanged versioned replacement still succeeds; and verifies a new destination appearing at commit time is preserved by `RENAME_NOREPLACE`.

The implementation resolves parent directories and read targets through stable file descriptors, so the tests assert behavior through the same production helpers rather than a separate mock path. Repository CI/race runs own the broader exact-head evidence; later Issue #42 integrated direct-client/Gateway acceptance remains intentionally separate from this file-path slice.

### Bounded browser path

Go tests cover the browser slice without mutating a live external site or installing host packages. They verify the 128 KiB script bound, 90-second default/five-minute maximum timeout contract, fail-fast browser mutex semantics, setup approval boundary, secure TLS default plus explicit request-scoped exception, temporary-file script handoff, per-run working-directory confinement for ordinary relative artifacts, the 256 MiB disposable-data cap plus state-filesystem reserve, lifecycle-lock exclusion for both full setup and already-current cleanup, disposable cache cleanup contract, exact embedded Playwright lock/integrity pins, architecture-specific browser content-tree digests, and the pinned runtime convergence command, single-backup crash recovery, and fail-closed ambiguous-backup handling.

A synthetic runtime fixture verifies that `browser_status` requires the desired manifest, actual Node version, exact Playwright package version, Playwright Chromium/FFmpeg metadata, expected installation markers, and the architecture-appropriate Chromium/headless-shell/FFmpeg executables as non-symlink regular executable files before reporting ready. MCP discovery tests verify `browser_status`, `timeout_ms`, `ignore_https_errors`, shared-profile disclosure, TLS-default disclosure and typed structured browser output. Exact-head CI owns the race-enabled Go test pass and supported-platform lifecycle matrix. Live browser setup remains an explicit host-package mutation and is not performed merely to satisfy unit review evidence; a later Issue #42 integrated acceptance slice owns end-to-end direct-client/Gateway/browser evidence and numeric tuning.

## 4. Stable installer and updater trust

### Stable bootstrap

Initial stable installation deliberately separates the bootstrap trust root from the release installer it authenticates. The supported one-line command loads `scripts/install-stable.sh` from the immutable Git tag named by the current README stable-install command, not from a mutable branch, a transient feature commit, or the release asset that is about to be verified. That immutable-release tag is the durable source identity for the bootstrap after publication.

`tests/stable_bootstrap.sh` guards the documented immutable-tag source and uses a deterministic mocked GitHub HTTP surface plus a fake `sudo` boundary to verify that the bootstrap:

- accepts a valid latest immutable release and an explicit valid stable tag;
- requires exactly the expected tag-scoped `install.sh` asset representation;
- rejects a mutable release before downloading or executing the release installer;
- rejects an unexpected asset URL before installer download/execution;
- rejects an installer digest mismatch after download but before the privileged handoff;
- copies the candidate through the privileged boundary into a root-controlled staging directory and re-verifies the same expected release-asset digest there before execution;
- fails closed when the invoking-user-writable installer pathname is adversarially replaced after the first verification but before privileged staging;
- proves that neither the original nor substituted installer executes when privileged re-verification fails.

This test is behavioral for bootstrap control flow, privilege ordering, and the local verified-path replacement threat. GitHub Release HTTP and the `sudo` command are mocked; the root-trust High Assurance job executes the same production bootstrap/test under the privileged supported-runner context. The test does not prove future GitHub repository settings or external network behavior.

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

On a compatible development host, a useful pre-push sequence is:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race -vet=off ./...
bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh scripts/install-stable.sh scripts/ci-change-scope.sh tests/*.sh
bash tests/stable_bootstrap.sh
```

For privileged/cloudflare/root-boundary changes, run the applicable security tests on a disposable supported environment matching the affected platform when practical. Ubuntu 22.04 amd64 remains the reference High Assurance environment; architecture-specific behavior also receives native arm64 lifecycle coverage. Never point test fixtures at the live Cloudflare zone or a production VPS.

## 9. Candidate review and release validation

Independent HIGH_ASSURANCE review is an integration/release gate for a frozen candidate, not an iterative lint service for a moving implementation. During active development, use self-review, targeted behavioral tests and current CI to converge. When the accepted feature scope is complete, freeze an exact base/HEAD and obtain independent review if the risk/profile requires it. If a BLOCKER/REQUIRED finding changes the candidate, that review identity is obsolete; fix the root cause, revalidate and refreeze before another independent gate review.

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

> This section defines acceptance for the accepted target architecture. It does **not** claim current `main` already ships these capabilities.

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

### Repository environment/toolchain discovery

`internal/executor/environment_test.go` validates the lightweight #64 discovery/reuse boundary against real temporary Git repositories. Coverage includes fresh-repository recovery from tracked manifests, compatible/incompatible worker-cache toolchain reuse, native language and package-manager declarations, package scripts without execution, mise/devenv/Dev Container/Dagger detection without automatic provisioning, explicit multi-mechanism/package-manager selection state, conflicting exact pins, untracked declaration durability warnings, workspace path-escape/symlink rejection, and bounded tool-version output. `internal/mcp/server_test.go` also keeps the public tool read-only, idempotent, local-only and bound to an explicit repository path with a typed output schema.

These tests deliberately do not install environment managers or run repository tasks. Managed provisioning for a concrete mechanism requires its own scope/evidence; this slice proves discovery/reuse and clear failure/unknown states only. Repository-owned dependencies/toolchains remain distinct from Agent-managed capability tooling.

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

### Failure and recovery

Exercise as applicable:

- wrong/stale workspace identity;
- concurrent file change during structured edit;
- structural/LSP result bounding;
- LSP/DAP crash and restart;
- stateful-session stale ID/principal mismatch;
- PTY output flood, reconnect, resize and long-lived soak;
- browser hang/output/artifact bounds;
- Agent/executor restart;
- full development-server loss with GitHub-only recovery.

### Performance/complexity guard

Measure representative workflow latency, remote/tool-call count, idle/active process footprint and recovery friction where useful. Do not introduce caches, daemons, alternate backends or infrastructure solely from theoretical performance concerns; escalate only from measured or repeatedly observed need.

Optional code-server, managed Incus, Ctags, alternate terminal backends and generic provider/tool-manager frameworks are not Core-v1 acceptance requirements unless their contracts are explicitly reactivated from new evidence.
