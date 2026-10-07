# Architecture

## 1. Supported release target

The supported stable release target is **Ubuntu 22.04+ and Debian 11+ on amd64/x86_64 and arm64/aarch64**, on dedicated development/test servers with systemd; current `main` follows the same platform policy. The identity of the latest immutable release is intentionally not hard-coded in this architecture document; GitHub Releases is authoritative, with README carrying the supported stable install command.

Platform compatibility is intentionally small and explicit: `install.sh` owns runtime distro/version validation and architecture normalization; the standalone stable bootstrap mirrors only the minimum distro policy needed before it can safely install `jq`; `scripts/release-arches.txt` owns the release artifact architecture list; CI asserts those boundaries stay aligned. New supported platforms should extend those policy/list/test boundaries rather than introduce parallel installer or release paths.

The control plane intentionally coexists with normal server software. It does not require or own nginx, Apache, Caddy, Docker, PHP, databases, Node.js, Python, `cloudflared`, or ports 80/443.

## 2. Process and trust boundaries

AI Server Agent is split into two long-running services:

- `ai-server-agent.service`: MCP/API process running as the unprivileged `aiagent` account.
- `ai-server-agent-executor.service`: root executor reachable through the Agent-owned Unix socket.

Ordinary shell work runs as `aiworker` in `/srv/ai-workspace`. Root shell work is explicit and uses the same policy/approval evaluation before execution.

The arbitrary root-shell capability is intentionally broad. Pattern/path policy and `approval_required` are defense-in-depth guardrails for recognized risky operations; they are **not** a complete sandbox or proof that every destructive/equivalent shell expression can be classified from command text. Any external principal/client granted root-shell authority must therefore be treated as root-capable. Structured privileged operations may narrow common workflows in the future, but they do not redefine the trust boundary of the escape hatch.

The current MCP `read_file` and `write_file` surfaces also execute through the privileged root executor. `read_file` is broad root-readable host-file authority, and `write_file` is broad root-file mutation that can create root-equivalent effects. Protected Agent-resource approval checks are additional guardrails, not a complete sensitive-file sandbox. Any integration/permission layer must represent that authority honestly rather than treating “read-only” as low privilege or file write as a non-root shortcut.

The public MCP surface uses bearer authentication. Direct public mode also requires native TLS. The bearer-authenticated MCP control plane is one authorization domain: root and worker jobs are different execution modes, not different external principals.

### AI-client / vendor boundary

The Agent core is **AI-client and AI-vendor neutral**.

Its durable responsibilities are server capabilities, execution policy, approval enforcement, workspace/files/jobs/browser behavior, lifecycle, audit/security and MCP tool semantics. None of those may depend on ChatGPT/OpenAI UI behavior, product plans, confirmation UX or a provider-specific safety layer.

MCP is the current primary common external protocol; it is not an OpenAI-specific protocol. ChatGPT is the current first validated/documented client path. A future supported AI/MCP client must reuse the same core capabilities and server-side safety rather than forking executor/policy logic.

Keep these dimensions separate:

```text
AI client/profile integration
        |
protocol/authentication adapter
        |
MCP (current primary surface)
        |
AI Server Agent core
        |
executor / policy / jobs / files / browser / lifecycle
```

Provider-specific setup/authentication/connectivity remains at explicit integration boundaries. For example, `docs/CONNECT_CHATGPT.md` and any OpenAI-specific tunnel mode are ChatGPT/OpenAI integration surfaces, not core architecture.

Do not add a speculative provider/plugin framework. A new client/provider is admitted only after current protocol/auth/tool/approval/network behavior is verified and bounded. Passing ChatGPT acceptance proves the ChatGPT integration, not universal compatibility.

The Agent must not require OpenAI credentials for core operation. Provider-specific credentials/state, if introduced later, remain isolated from core state and from other providers.

### Root command environment

Root commands must not inherit worker-controlled ambient shell state. The executor uses:

- `HOME=/root`;
- working directory `/root`;
- a minimal explicit environment and fixed command `PATH`;
- `/bin/bash --noprofile --norc -c`;
- no inherited `BASH_ENV` or `ENV`.

Worker commands currently retain `/srv/ai-workspace` as HOME/CWD.

### Synchronous executor foundation

Synchronous MCP command calls carry request cancellation into the private executor. Process-group termination sends TERM, waits for a bounded grace period, then uses KILL when required; ordinary same-process-group children are also cleaned up after the shell exits. Direct `run_command` / `run_root_command` calls use a five-minute execution budget. The private executor enforces a 30-minute hard/default ceiling for internal synchronous `run` callers so browser-specific budgets can be tightened separately without permitting unbounded execution. A child that deliberately escapes the command process group cannot be proven stopped by this mechanism; such work is outside the supported synchronous model and belongs in `start_job` or an intentionally managed service.

Worker and root synchronous execution use separate non-queueing capacity guards. Worker capacity is `min(GOMAXPROCS, 4)` with a floor of one; root capacity is one. Saturation returns a structured retryable `busy/resource_limit` result immediately rather than maintaining an implicit queue. The private executor response frame is capped at 8 MiB on both the writer and client-reader boundary; an oversized frame fails closed.

Command bodies are capped at 256 KiB and enter the sanitized Bash process through stdin rather than a raw `-c <command>` argv element. Synchronous stdout/stderr is retained in a 1 MiB in-memory head/tail collector while the process runs, so producer volume cannot grow executor memory without bound. Results preserve raw byte counts, returned-byte counts, UTF-8 versus base64 encoding, truncation/omission metadata, exit status, duration and timeout state. MCP structured content carries the full bounded result; the human-readable text fallback does not duplicate a large output field.

**Target Developer Runtime changes this separation:** project worktrees remain under the workspace root, while worker HOME/config/cache state moves to Agent-managed locations outside project worktrees. This is accepted target architecture, not a statement that the current runtime already implements the split. See `docs/DEVELOPER-RUNTIME.md`.

### Persistent jobs

Persistent jobs run as transient systemd units and survive MCP/client reconnects. Job metadata lives under the root-controlled state container `/var/lib/ai-server-agent/jobs`. The executor admits at most four active Agent jobs and fails immediately with a retryable resource-limit result when that capacity is full; it does not maintain a hidden scheduler or queue.

The raw shell body is capped at 256 KiB and written to a short-lived protected handoff file before launch. `systemd-run` receives only fixed runner/path arguments plus the sanitized execution environment; the fixed `job-runner` validates and consumes the handoff, removes it, and feeds the body to Bash through stdin. This removes the Agent-created raw command copy from systemd/process argv and from the slice's durable idempotency state. Full secret-safe audit/correlation policy remains owned by #41.

Each persistent job uses an 8 MiB bounded on-disk ring for combined stdout/stderr. The ring stores logical `available_from` / `current_end` offsets, while `job_output` returns bounded ranges with `requested_offset`, `available_from_offset`, `next_offset`, `current_end`, EOF and retention-truncation metadata. Binary output is base64-encoded. The newest 32 terminal job artifact sets are retained; interrupted jobs whose unit is no longer active/pending without a durable exit status are persisted as `unknown_completion` (including a still-loaded but inactive/failed transient unit), and older terminal artifacts plus matching idempotency claims are removed. Before accepting a new job, the executor also checks an exact state-filesystem reserve derived from the hard global persistent-job recovery-claim bound times the per-job log bound, plus fixed safety overhead. This keeps abnormal retained launch/job states inside the same modeled disk envelope rather than assuming every job reaches a normal completed status.

Job log/status/runner files are created with exclusive, no-follow semantics. Reads reject symlinks, non-regular files, unexpected owners, and world-writable files. Root-owned versus `aiworker`-owned job files record execution provenance and protect filesystem replacement; they are not a separate bearer-auth authorization partition.

Every persistent launch gets a root-owned recovery claim containing a keyed fingerprint over the material command/root request rather than plaintext command content; callers may additionally supply `operation_id` for durable retry identity. Same-key/same-material retries recover or return the same job handle; same-key/different-material reuse fails closed. A crash while only the pre-launch claim exists can rebuild the protected handoff from a fingerprint-matched retry; once launch may have happened, reconciliation requires durable runner/unit evidence and never starts a differently identified duplicate merely because completion is uncertain. Stale pre-launch claims older than the executor's broad connection deadline are reconciled on later job admission: execution evidence promotes them to started, while no evidence retires the protected handoff and marks the claim failed. Failed claims and the total recovery-claim namespace are explicitly bounded; claims for completed jobs age out with completed-artifact retention, so recovery metadata cannot become an unbounded second job database.

The current bearer-authenticated direct surface has one authenticated-principal namespace, so `operation_id` is unique within that current auth domain. #43 owns the later named-principal expansion; when that lands, durable idempotency identity must include the authenticated principal as well as the caller operation ID without changing the caller-visible retry semantics.

Safe uninstall/purge first reconciles active `ai-job-*.service` units and refuses while any are active, preventing removal of the Agent control plane from silently orphaning still-running persistent work.

### Accepted Gateway-compatible execution/data boundary

Before AI Server Agent can claim stable MCP Gateway compatibility, the owner-activated minimum slices in Issues #41 and #42 must be integrated. The rules below are the accepted target boundary for that compatibility claim; they are not a statement that current `main` already implements every bound. README/current source remain authoritative for shipped behavior until those slices integrate.

The durable execution/data rules are:

- ordinary synchronous commands remain low-overhead bounded request/response operations;
- command/browser output is bounded while it is produced, with explicit truncation/encoding/timing metadata rather than unbounded buffering followed by silent clipping;
- expected long/high-output work uses persistent jobs plus ranged `job_output`, not ever-larger synchronous timeouts;
- file reads are ranged/bounded and binary-safe; ordinary complete-file writes are bounded, atomic where filesystem semantics allow, and symlink-safe;
- persistent job count and underlying log growth are bounded while offset/continuation semantics remain reliable;
- browser input/runtime/output is bounded and one hung run cannot hold the browser path forever;
- transport timeout/disconnect is not treated as proof that a mutating host command stopped;
- structured machine results are the integration contract; large payloads are not duplicated in full merely to provide both text and structured representations.

The durable local audit rules are:

- external principal + request/operation/job/approval correlation is non-secret and structured;
- raw arbitrary shell command text is not persisted by default as the audit source of truth;
- audit storage is locally bounded/rotated and remains usable without a mandatory telemetry service.

These are host-protection and integration-contract requirements, not a move toward a distributed scheduler, generic file-transfer service, mandatory Redis/queue, container runtime, or fleet controller.

## 3. Filesystem trust model

Important paths:

| Path | Purpose / trust |
| --- | --- |
| `/usr/local/bin/ai-server-agent` | installed agent binary |
| `/usr/local/sbin/ai-server-agent-manage` | supported privileged management entrypoint/wrapper |
| `/usr/local/lib/ai-server-agent` | installed management/update/uninstall implementation |
| `/etc/ai-server-agent` | root-controlled configuration |
| `/etc/ai-server-agent/control` | root-only install identity, Cloudflare journal/backup, internal control state |
| `/var/lib/ai-server-agent` | root-controlled state container |
| `/var/lib/ai-server-agent/runtime` | root-controlled runtime container |
| `/var/lib/ai-server-agent/jobs` | root-controlled persistent-job container |
| `/var/log/ai-server-agent` | Agent logs/audit data |
| `/srv/ai-workspace` | `aiworker` writable project workspace; preserved by purge |
| `/run/lock/ai-server-agent` | purge-safe root lifecycle lock namespace |

Root-consumed control state must not be replaceable by `aiworker`/`aiagent`. Install/migration paths reject or repair unsafe legacy container layouts and symlinks where the supported migration semantics allow it.

## 4. Privileged management lifecycle

There are two lock layers with different scopes.

### Global lifecycle serialization

The installed `/usr/local/sbin/ai-server-agent-manage` wrapper acquires:

`/run/lock/ai-server-agent/management.lock`

before entering `manage.sh`. Install, update, uninstall and purge use the same root-only lifecycle lock (or inherit its open descriptor when invoked through management). This namespace is outside Agent config/state so purge cannot remove the lock while a concurrent operation is active.

This is the authoritative cross-operation lifecycle lock for privileged state mutation.

### Cloudflare/internal management lock

`manage.sh` also uses a root-only lock under `/etc/ai-server-agent/control` around connection-management internals. It protects transaction-local management state, but it is not the purge-safe global lifecycle namespace.

New privileged state-mutating entrypoints must participate in the global lifecycle serialization model rather than relying only on the internal control lock.

## 5. Connection modes

### Local/private

The default local endpoint binds to loopback and remains bearer-authenticated. This is appropriate for private connectivity such as a separately managed secure MCP tunnel.

### Manual public TLS

The operator supplies an existing certificate/key pair. The manager validates hostname/key pairing, installs them under `/etc/ai-server-agent/tls`, switches the Agent to public/native TLS, restarts services, and restores the previous local TLS/config state if activation fails.

### Guided Cloudflare

The Cloudflare path manages only the selected MCP hostname. It may create/reconcile:

- a proxied `A` DNS record;
- a hostname-scoped Origin Rule for the Agent port;
- a hostname-scoped Configuration Rule setting SSL to `strict`;
- a Cloudflare Origin CA certificate.

It does **not** change whole-zone SSL mode.

Existing external DNS/rules are not silently adopted or overwritten. The manager reuses a matching external proxied DNS record without taking ownership, and otherwise fails closed on conflicts or ambiguous ownership.

Origin Rule and Configuration Rule discovery is intentionally phase-specific. The manager reads the exact zone phase entrypoint for `http_request_origin` or `http_config_settings`; it does not enumerate the zone's broad Rulesets collection and infer absence from pagination. An authoritative 404 means the phase entrypoint is absent.

Cloudflare can retain an existing phase Ruleset after its last Rule is removed and, in live provider responses, omit `rules` from both the phase entrypoint and the exact current Ruleset. Ruleset interpretation therefore has one shared resolver used by discovery, post-create verification, and cleanup/recovery reads. The resolver validates the zone Ruleset identity, follows the exact current Ruleset ID, and—when the current response still omits `rules`—reads the exact current version and requires matching Ruleset ID, zone kind, phase, and version before normalizing an omitted/null rule collection to `rules:[]`. If the authoritative version read contains Rules, those Rules are preserved rather than assuming emptiness.

This consistency chain also acts as a concurrency boundary: phase/current/version identity drift, malformed or wrong-type Rules, duplicate Rule IDs, contradictory not-found state, or non-404 provider failures fail closed before any absence-based CREATE or destructive recovery decision. Cloudflare Rulesets failures emit secret-safe diagnostics identifying the failing read stage plus phase/Ruleset/version context when known; API tokens and Authorization material are never included in those diagnostics.

## 6. Cloudflare transaction and recovery model

Cloudflare configuration is a root-only durable transaction. The journal is:

`/etc/ai-server-agent/control/cloudflare-transaction.json`

and the durable pre-local-mutation rollback snapshot is:

`/etc/ai-server-agent/control/cloudflare-transaction-backup/`

The current journal schema is version 3.

### State machine

Normal commit path:

`prepared -> applying -> committing -> committed`

Recovery/rollback path:

`prepared | applying | unproven committing -> rolling_back -> rolled_back`

The journal validator enforces semantic relationships, not only JSON types:

- pending kind is bound to the correct Cloudflare ruleset phase;
- pending zone/hostname must match the transaction identity;
- every pending create kind carries the kind-specific durable ownership/representation evidence needed by its recovery path;
- pending creates cannot survive into local `applying`, `committing`, or `committed` state;
- commit identity must exactly match the transaction/resource identity;
- `rolled_back` is terminal only when pending evidence, commit intent, rollback-owned resources, and certificate ownership are empty;
- malformed, contradictory, unsafe-owner/mode, or unknown-schema journals fail closed and remain untouched.

### Durable local rollback snapshot

Before the first remote create, the manager creates the root-only local backup and records `backup_ready=true`. Local config/TLS/managed-state mutation does not begin until remote resources are reconciled and checkpointed.

If the process dies after local mutation but before a proven commit, a fresh process restores the durable local snapshot before entering `rolling_back`.

### Commit identity

Before replacing trusted managed state, the manager durably records the exact intended hostname, port, Cloudflare IDs, ownership flags, and fingerprints and moves to `committing`.

A fresh process may finalize `committing`/`committed` without rollback only when current trusted `managed.json` exactly matches that durable commit identity. Otherwise it restores local state and rolls back transaction-created remote resources.

### Remote ownership and concurrent drift

For confirmed Agent-owned DNS/rules, the manager stores canonical full-resource fingerprints and immediately re-reads the current resource before destructive cleanup. If representation changed, automatic deletion fails closed.

For response-lost POSTs, discovery identity alone is insufficient. Before the POST, the journal stores the pending create kind and transaction identity plus the exact request-controlled semantic fingerprint. DNS/rule creates also carry an unpredictable Agent marker/nonce used to discover only the Agent-created candidate.

Origin CA uses the newly generated CSR as its unpredictable discovery value. The durable Origin CA fingerprint covers `csr`, the hostname set, `request_type`, and `requested_validity`. Recovery first discovers a unique candidate by CSR, then GETs that exact certificate ID, recomputes the complete semantic fingerprint, and revokes only when it still equals the durable pre-POST intent. Ambiguity, representation mismatch, or exact-ID read failure preserves the pending recovery state and fails closed.

DNS/rule recovery likewise re-reads the exact discovered resource by ID, recomputes the semantic fingerprint, and deletes only if the current representation still matches the durable pre-POST representation. Competing lookalikes, multiple matches, missing representation proof, or drift remain unresolved rather than being destructively guessed.

Ruleset recovery deletes only the exact Agent rule, never the shared Ruleset container.

The current design deliberately avoids unconditional in-place mutation of previously recorded Cloudflare DNS/rules when a compare-and-swap guarantee is unavailable; drift is surfaced for explicit resolution instead.

## 7. Stable install/update trust

Stable and source channels are distinct.

### Stable bootstrap and first install

The release `install.sh` cannot authenticate itself before it executes, so it is deliberately **not** the stable trust root. Initial stable installation begins with `scripts/install-stable.sh` loaded from a Git tag already bound to a published immutable GitHub Release. The README stable-install command names the immutable release tag used as the bootstrap source; this architecture document intentionally does not pin that current release number.

GitHub immutable releases lock their associated tag against movement/deletion while the release exists, and a deleted immutable release does not permit reuse of the same tag name. The bootstrap source therefore does not depend on preservation of a feature branch or a particular merge strategy.

The immutable-tag bootstrap runs before release `install.sh` bytes receive root execution. It:

1. resolves an explicit stable tag or GitHub's latest release metadata;
2. requires the release to be non-draft, non-prerelease and immutable;
3. requires exactly one uploaded `install.sh` asset at the exact tag-scoped release URL;
4. requires the GitHub release asset `sha256:` digest;
5. downloads that release installer without privileged execution and verifies its bytes against the digest;
6. crosses the privilege boundary through a fixed bootstrap helper that copies the candidate into a root-owned `0700` staging directory;
7. recomputes the same expected release-asset digest on the root-controlled copy and fails closed on any mismatch;
8. executes only that re-verified root-controlled copy.

The original invoking-user-writable pathname is therefore never trusted for root execution after the privilege boundary. Replacing that pathname between the unprivileged hash and `sudo` can at most cause the privileged re-verification to fail; it cannot make different bytes pass into the installer execution path.

The release-scoped `install.sh` is generated with its version/ref pinned to the release tag. Once authenticated by the bootstrap, it rejects `AI_SERVER_AGENT_BINARY` overrides, downloads the release archive plus `SHA256SUMS`, and continues only after archive checksum verification.

A direct `releases/.../install.sh | sudo bash` command is intentionally not a supported stable trust path because it would execute the asset before authenticating that same asset. A mutable branch is likewise not an acceptable stable bootstrap source.

Stable release architectures are enumerated in `scripts/release-arches.txt`; the current target publishes both amd64 and arm64 archives under the same checksum manifest.

### Stable update

Trusted install identity is strict root-only JSON under `/etc/ai-server-agent/control/install-state.json`. Missing, malformed, contradictory, or unsafe identity fails closed; the updater does not guess `main`.

The already-installed trusted updater applies the same release identity/digest invariant without needing the external bootstrap. For the stable channel it:

1. resolves an explicit tag or the latest published immutable release;
2. requires the release to be non-draft, non-prerelease and immutable;
3. requires exactly one uploaded `install.sh` asset at the exact tag-scoped release URL;
4. requires the GitHub release asset `sha256:` digest;
5. downloads `install.sh` and verifies its bytes against that digest before execution;
6. lets the release-scoped installer verify the archive with `SHA256SUMS`.

Source updates resolve the selected source ref to a commit SHA and remain a separate development/source mechanism.

## 8. Release provenance

Release publication is manual-dispatch and exact-SHA based. The workflow requires:

- an exact validated `main` SHA;
- successful `main` push CI and High Assurance Security for that SHA;
- promotion of the exact CI-produced release artifact rather than rebuilding the release payload;
- an absent release/tag before create-only tag creation;
- operator confirmation that repository release immutability is enabled;
- operator confirmation that the release-tag ruleset protects the release tag pattern from update/delete and has no bypass actor capable of moving/deleting the tag;
- exact tag creation at the validated SHA before `gh release create --verify-tag`;
- post-publication verification that the release is immutable, the tag still resolves to the validated SHA, and release/asset attestations verify.

The workflow intentionally does **not** treat a missing `bypass_actors` field from its `GITHUB_TOKEN` ruleset response as proof that no bypass actors exist. That property is an explicit operator gate because the workflow credential cannot authoritatively establish it.

## 9. Preservation and removal

Normal uninstall removes executable services/tooling while preserving configuration, Agent state, users, optional runtime data and workspace for reinstall/repair.

Purge removes Agent-owned config/state/log/runtime and the `aiagent` identity, but intentionally preserves:

- `/srv/ai-workspace`;
- `aiworker` and its group.

Cloudflare resources are not silently deleted by purge. Recorded Cloudflare resources must be cleaned through the ownership-aware Cloudflare cleanup path when desired.

## 10. Accepted Developer Runtime target architecture

`docs/DEVELOPER-RUNTIME.md` is the canonical owner of the accepted **target** AI-native Developer Runtime architecture.

The target is deliberately value-first:

- common, high-impact development workflows receive first-class structured capabilities;
- uncommon work remains possible through mature Linux/CLI/root/PTY escape hatches;
- future-proofing preserves cheap extension seams instead of pre-building hypothetical providers/backends;
- complexity must create observable workflow/correctness/reliability value.

Core target capabilities include repository/worktree identity, safe worker editing, text + ast-grep structural search, LSP, persistent worker/root PTY, DAP and Playwright-backed browser/E2E validation.

This architecture document remains authoritative for overall host/process/trust/lifecycle boundaries. `docs/DEVELOPER-RUNTIME.md` owns the detailed development-capability composition and Layer-1/Layer-2/deferred boundary.

Until the implementation work lands, README/current source remain authoritative for which capabilities are actually shipped. Do not infer runtime availability merely from target-architecture documentation.
