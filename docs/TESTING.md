# Testing and Validation

This document describes the validation model that applies to the current v0.1 codebase. It is not a historical release log.

## 1. Supported stable test target

Stable v0.1 release support is intentionally narrow:

- Ubuntu 22.04 LTS
- amd64/x86_64
- systemd
- dedicated development/test server use

CI and release artifacts must not imply stable support for Debian, Ubuntu 24.04, arm64, or other platforms merely because the source installer can run on a broader development/source matrix.

The source-install compatibility path currently accepts Ubuntu 22.04+ and Debian 11+ on amd64/arm64. That path is useful for development, but it is not the stable v0.1 release matrix.

## 2. Validation layers

Use the narrowest relevant check first, then the applicable broader layer.

### Go correctness

CI runs:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/ai-server-agent ./cmd/ai-server-agent
```

These cover Go unit/behavior tests, race detection and the production binary build.

### Shell syntax

CI validates the established installer, updater, uninstaller, management, release-builder and security-test paths. `tests/stable_bootstrap.sh` also syntax-checks the commit-pinned stable bootstrap before exercising it behaviorally.

### Ubuntu 22.04 lifecycle integration

The main CI workflow performs a real privileged lifecycle on an Ubuntu 22.04 amd64 GitHub runner:

1. install a locally built Agent binary;
2. verify both systemd services are active;
3. verify the installed management/update paths;
4. execute root trust-boundary tests;
5. safe-uninstall and verify preserved Agent data/users/workspace;
6. reinstall;
7. purge and verify Agent-owned config/state/log/runtime and `aiagent` are removed;
8. verify `aiworker` and `/srv/ai-workspace` remain.

A separate first-run test proves that choosing "Configure later" is a successful core installation, not an installer failure.

## 3. High Assurance Security workflow

`.github/workflows/security.yml` is the stronger validation path for the high-risk surfaces.

### Cloudflare transaction behavior

`tests/cloudflare_transaction.sh` exercises production shell functions with a mocked Cloudflare provider. It covers, among other things:

- rollback of transaction-created resources;
- preserved recovery state after incomplete rollback;
- current-representation fingerprints before destructive cleanup;
- response-lost DNS/rule create recovery with durable nonce + semantic fingerprint;
- fail-closed handling of representation drift and competing lookalikes;
- exact-rule cleanup rather than deleting a shared Ruleset container;
- management-lock exclusion behavior.

The Cloudflare provider is mocked. This is behavioral testing of production transaction/recovery code, not a live-zone integration test.

### Crash recovery

`tests/cloudflare_crash_recovery.sh` performs a real process `SIGKILL` after a mocked remote create has committed but before the POST response returns. The durable pre-POST journal and local rollback snapshot must survive, and a fresh process must recover through the exact representation-check/delete path.

`tests/cloudflare_phase_recovery.sh` performs real `SIGKILL` + fresh-process recovery at durable local/commit boundaries, including:

- local mutation while phase is `applying`;
- persisted `rolling_back` recovery;
- `committing` with matching trusted managed state;
- `committed` finalization;
- malformed/contradictory journal rejection;
- pending kind/phase/identity relationships;
- terminal-state consistency.

### Root trust boundary

`tests/root_trust_boundary.sh` and `tests/root_trust_migration.sh` exercise hostile legacy layouts, symlink/replacement attempts, root-only control state, root-controlled state containers and the global lifecycle lock.

The lifecycle overlap tests use the installed management wrapper and the real `/run/lock/ai-server-agent/management.lock` namespace to verify that configure/update/install/purge cannot overlap before mutation.

`tests/root_trust_boundary.sh` also invokes `tests/stable_bootstrap.sh`, so the initial stable-install pre-execution trust boundary is exercised by the existing High Assurance root-trust job rather than by a separate duplicate workflow.

### Privileged shell isolation

Go tests exercise root command environment isolation and the persistent-job command construction. The current persistent-job isolation test uses a fake `systemd-run` command to inspect/execute the generated invocation.

**Known coverage limit:** CI does not currently exercise an actual privileged systemd transient job end-to-end. A real Ubuntu/systemd transient-unit integration test is useful additional confidence, but it is not a substitute for the existing command/environment tests and is not currently a v0.1.2 release blocker by itself.

## 4. Stable installer and updater trust

### Stable bootstrap

Initial stable installation deliberately separates the bootstrap trust root from the release installer it authenticates. The supported one-line command fetches `scripts/install-stable.sh` from an exact Git commit, not from a mutable branch and not from the release asset that is about to be verified.

`tests/stable_bootstrap.sh` uses a deterministic mocked GitHub HTTP surface plus a fake `sudo` boundary and verifies that the bootstrap:

- accepts a valid latest immutable release and an explicit valid stable tag;
- requires exactly the expected tag-scoped `install.sh` asset representation;
- rejects a mutable release before downloading or executing the release installer;
- rejects an unexpected asset URL before installer download/execution;
- rejects an installer digest mismatch after download but before `sudo` or installer execution;
- calls privileged installer execution only after the downloaded bytes match the release asset `sha256:` digest.

This test is behavioral for bootstrap control flow and privilege ordering. GitHub Release HTTP is mocked; it does not prove future GitHub repository settings or external network behavior.

### Release-scoped installer

The security workflow builds the candidate release assets and verifies:

- `install.sh` is generated with the expected version/ref at the release-scoped header;
- stable binary override is rejected;
- corrupted archive bytes are rejected by `SHA256SUMS` verification;
- stable v0.1 artifacts remain amd64-only.

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
- ChatGPT Business tool discovery and end-to-end MCP use;
- an actual persistent job through a real transient `systemd-run` unit;
- current repository Rulesets bypass configuration;
- future release immutability settings before publication;
- production VPS behavior outside the supported validated target.

Do not replace these gaps with grep tests that merely search for reassuring strings.

## 8. Developer checks

On a compatible development host, a useful pre-push sequence is:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh scripts/install-stable.sh tests/*.sh
bash tests/stable_bootstrap.sh
```

For privileged/cloudflare/root-boundary changes, run the applicable security tests on a disposable supported Ubuntu 22.04 environment when practical. Never point test fixtures at the live Cloudflare zone or a production VPS.

## 9. Candidate review and release validation

Independent HIGH_ASSURANCE review is an integration/release gate for a frozen candidate, not an iterative lint service for a moving implementation. During active development, use self-review, targeted behavioral tests and current CI to converge. When the accepted feature scope is complete, freeze an exact base/HEAD and obtain independent review if the risk/profile requires it. If a BLOCKER/REQUIRED finding changes the candidate, that review identity is obsolete; fix the root cause, revalidate and refreeze before another independent gate review.

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
