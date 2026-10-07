# AI Server Agent

AI Server Agent turns a dedicated Linux development/test server into a bearer-authenticated MCP control endpoint. ChatGPT is the current first validated/documented AI client integration; the Agent core and MCP capability/security model are intentionally AI-client/vendor neutral. Ordinary commands run as an unprivileged worker; host-level actions go through a separate root executor with policy and approval guardrails.

The project is intentionally a **development/test-server control plane**, not a general hosting panel. It gives an authorized MCP client enough capability to work on a dedicated Linux host while keeping the Agent's own control plane, credentials, connectivity, and destructive operations behind explicit boundaries.

## Platform support

Stable `v0.1.9` and the current `main` branch support:

- **Ubuntu 22.04 or newer**
- **Debian 11 or newer** (Debian 11 application compatibility is retained, but upstream Debian LTS ended on 2026-08-31; use Debian 12+ for an officially security-maintained base unless you have an ELTS arrangement)
- **amd64/x86_64 and arm64/aarch64**
- systemd
- a dedicated development/test server where you are comfortable granting an AI-controlled MCP endpoint the documented capabilities

OS support is minimum-version based, while CI keeps explicit compatibility cases for current Ubuntu/Debian releases and native lifecycle coverage on both supported CPU architectures. Stable release architectures are declared in `scripts/release-arches.txt`; installer architecture aliases are normalized at the install boundary so adding a future architecture does not require rewriting release publication logic.

`v0.1.7` is the first stable release with this expanded platform matrix.

AI Server Agent does not require nginx, Apache, Caddy, Docker, PHP, a database, Node.js, Python, `cloudflared`, or a hosting panel as core dependencies, and it does not need to take over ports 80/443.

## Install the latest stable release

Stable installation starts with a small bootstrap loaded from an **immutable published release tag**, separate from the release `install.sh` asset it authenticates. The current v0.1 bootstrap trust anchor is the immutable `v0.1.9` release tag. GitHub locks the associated tag when an immutable release is published, so this path does not depend on a feature branch or merge strategy.

`v0.1.9` is published as an immutable release. Install the latest stable release with:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/v0.1.9/scripts/install-stable.sh | bash
```

For exact `v0.1.9` installation through the same immutable bootstrap:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/v0.1.9/scripts/install-stable.sh | bash -s -- v0.1.9
```

Do **not** use `releases/latest/download/install.sh | sudo bash` as the stable trust path: that executes release-supplied code as root before the same asset can be authenticated.

The stable bootstrap does **not** fall back to `main`. It:

1. resolves the requested release or GitHub's latest release metadata;
2. requires a published, non-draft, non-prerelease, immutable release;
3. requires exactly one uploaded `install.sh` asset at the exact tag-scoped release URL;
4. requires the GitHub release asset `sha256:` digest;
5. downloads `install.sh` without root execution and verifies its bytes against that digest;
6. crosses the privilege boundary only after that unprivileged verification succeeds;
7. copies the candidate into a root-controlled staging directory, re-verifies the same authenticated digest on the protected copy, and executes only that protected copy.

### What the verified release installer does

The release-scoped `install.sh` is generated with its version/ref pinned to the release tag. After the bootstrap authenticates it, the installer:

1. checks the supported stable OS/architecture;
2. downloads the matching release archive and `SHA256SUMS`;
3. verifies the archive checksum before installation;
4. installs the Agent/services and management helpers;
5. starts the core Agent locally and verifies health;
6. optionally launches the first-run connection wizard.

Stable installation rejects a local `AI_SERVER_AGENT_BINARY` override. Stable payload identity is the published immutable release, not a mutable branch.

## What gets installed

Main paths:

| Path | Purpose |
| --- | --- |
| `/usr/local/bin/ai-server-agent` | Agent binary |
| `/usr/local/sbin/ai-server-agent-manage` | supported management entrypoint |
| `/usr/local/lib/ai-server-agent/` | management/update/uninstall implementation |
| `/etc/ai-server-agent/` | root-controlled configuration, TLS and managed state |
| `/etc/ai-server-agent/control/` | root-only install identity and recovery control state |
| `/var/lib/ai-server-agent/` | Agent runtime/state, browser profile data and persistent-job metadata |
| `/var/log/ai-server-agent/` | logs/audit data |
| `/srv/ai-workspace/` | `aiworker` project workspace; intentionally preserved by purge |
| `/run/lock/ai-server-agent/` | root lifecycle serialization lock |

System users:

- `aiagent`: unprivileged MCP/API service account;
- `aiworker`: ordinary command/workspace account.

Services:

- `ai-server-agent.service`
- `ai-server-agent-executor.service`

## First-run choices

A successful core install is independent from public connection setup. The wizard offers:

1. **Cloudflare domain** — guided hostname-scoped HTTPS setup;
2. **Local/private** — keep the Agent loopback-only;
3. **Existing certificate** — advanced manual native TLS;
4. **Configure later** — finish the healthy core installation now and run management later.

Choosing **Configure later** is not an installation failure.

After installation, the main entrypoint is:

```bash
sudo ai-server-agent-manage
```

## Guided Cloudflare setup

The recommended direct-public path uses Cloudflare for the selected MCP hostname only. It does not change the zone-wide SSL mode.

Before entering a token, the manager prints the required scope. Restrict zone permissions to the intended zone. Because the current Cloudflare phase-entrypoint read requires an accepted account Rulesets permission in the live guided flow, restrict that account resource to the account that owns the selected zone. The token needs these permissions:

- `Zone > Zone > Read`
- `Zone > DNS > Edit`
- `Zone > SSL and Certificates > Edit`
- `Zone > Origin Rules > Edit`
- `Zone > Config Rules > Edit`
- `Account > Account Rulesets > Read`

The manager creates/reconciles only the selected hostname's resources:

- proxied `A` record;
- hostname-scoped Origin Rule for the Agent port;
- hostname-scoped Configuration Rule setting SSL to `strict`;
- Origin CA certificate.

### Token handling

The Cloudflare API token is entered in a **hidden terminal prompt**. Do not paste it into ChatGPT, chat, tickets, screenshots, logs or source control.

The interactive manager uses the token only for the current command and does not store it in Agent managed state. Noninteractive automation can supply a protected token file through the documented environment variable instead of putting the token on a command line.

### Ownership and recovery

The manager does not silently adopt or overwrite conflicting external Cloudflare state.

Ruleset reads are reconciled through a fail-closed identity/version chain so Cloudflare's retained-empty response variants do not become silent absence guesses. When a Rulesets read cannot be trusted, the manager reports the failing stage and non-secret phase/Ruleset/version context where available. It does not print the Cloudflare API token or Authorization material.

Transaction-created resources are durably journaled. Confirmed Agent-owned resources are fingerprinted and re-read before destructive cleanup. If a POST response is lost, recovery requires both an unpredictable ownership marker and the exact durable pre-POST representation fingerprint before deleting the discovered resource. Concurrent representation drift fails closed.

Cloudflare Ruleset recovery deletes only the exact Agent-owned rule, never a shared Ruleset container. Equivalent external/manual hostname-scoped rules are not silently adopted or deleted; recorded Agent-owned rules remain authoritative on rerun, while stale ownership plus an external semantic equivalent fails closed.

If old Agent-managed resources remain after a hostname/certificate change, use the explicit Cloudflare cleanup path; do not rely on purge to delete remote resources.

## Local/private mode

Local mode keeps the Agent on loopback. This is suitable when another trusted private connectivity mechanism will carry MCP traffic.

Use the management menu or:

```bash
sudo ai-server-agent-manage configure-local
```

## Manual native TLS

For an existing certificate/key pair:

```bash
sudo ai-server-agent-manage configure-manual-tls
```

The manager validates the certificate hostname and key pairing before switching the Agent to public mode. Public mode requires native TLS; plaintext public binding is not the supported direct-public model.

## Client and vendor neutrality

MCP is the current primary common client boundary; it is not OpenAI-specific. ChatGPT is the current supported/validated client path and keeps dedicated setup documentation below.

Future AI/MCP clients may be supported through bounded client-specific integration/authentication paths, but they must reuse the same Agent tools, executor, policy, approval and lifecycle semantics. A provider-specific UI, credential, tunnel or confirmation mechanism must not become a core safety dependency.

Support for another commercial AI client is claimed only after its current protocol/auth/tool behavior has been verified and accepted; this architecture does not imply universal compatibility.

## ChatGPT setup

After public setup succeeds:

```bash
sudo ai-server-agent-manage chatgpt-setup
```

The manager prints the MCP URL and the protected bearer-auth setup guidance. The Authorization value is stored on the server and is revealed only after explicit terminal confirmation.

ChatGPT full MCP/custom-app support is an evolving client-side feature. For managed workspaces, creation and administration currently use the available Plugins/custom-MCP surfaces and workspace controls; exact labels and navigation can change independently of the Agent. Use the current OpenAI product guidance and live UI at connection time rather than treating a historical `Developer mode`, `Apps -> Create`, screenshot, or menu sequence as a protocol contract.

See [docs/CONNECT_CHATGPT.md](docs/CONNECT_CHATGPT.md) for connection topologies and the validation checklist. When that document's client-side UI wording differs from the current ChatGPT product, current OpenAI guidance and the live UI are authoritative for the client-side steps; the Agent-side endpoint/auth/tool contract remains the durable part documented here.

The public endpoint remains bearer-authenticated. Treat the bearer credential as a privileged server-control credential and provide it only to the trusted ChatGPT connection UI when configuring the app.

## MCP capability surface

Once a supported MCP client is connected, the Agent exposes a compact tool surface designed for real server work:

| Tool | Purpose |
| --- | --- |
| `agent_environment` | read the current self-preservation manifest before host-wide changes |
| `run_command` | run ordinary Bash as `aiworker` in `/srv/ai-workspace` |
| `run_root_command` | run Bash as root, subject to executor policy/approval guardrails |
| `start_job` | start a persistent transient-systemd background job that survives MCP/client disconnects |
| `job_status` / `job_output` / `job_stop` | inspect, read output from, or stop a persistent Agent job |
| `read_file` | read a host file through the privileged root executor; this is broad root-readable file authority, and protected Agent state may additionally require approval |
| `write_file` | write complete host-file content through the privileged root executor; this is root-capable host mutation, and protected Agent state may additionally require approval |
| `browser_setup` | install the optional private Node.js + Playwright + Chromium runtime and required shared libraries |
| `browser_run` | run Playwright JavaScript in server-side headless Chromium using a persistent browser profile |

### Ordinary commands and root commands

Use `run_command` for normal development work, builds, tests, Git, project package managers and diagnostics that do not require host privilege. It runs as `aiworker` with `/srv/ai-workspace` as HOME/CWD. Synchronous `run_command` / `run_root_command` calls are limited to five minutes; work expected to run longer or produce high output should use `start_job`. Command bodies are limited to 256 KiB and are delivered to Bash through stdin rather than being copied into the spawned process argv. Synchronous stdout/stderr is captured with a 1 MiB production-time head/tail bound and returns explicit encoding, raw-byte, truncation and duration/timeout metadata; binary output is base64-encoded instead of being treated as UTF-8. Active synchronous worker and root execution have separate bounded capacity and fail immediately with a structured `busy/resource_limit` result instead of entering a hidden queue. Cancellation/timeout terminates the command process group with TERM followed by a bounded KILL fallback, and ordinary same-process-group background children are cleaned up; a command that deliberately escapes that process group cannot be claimed as stopped, so durable/background work belongs in `start_job` or an intentionally managed service.

`/srv/ai-workspace` is persistent. Connected models are instructed to inspect and reuse existing repositories, worktrees and task environments before creating duplicates, prefer `git worktree` when another checkout of the same repository is appropriate, and never treat dirty, untracked, ambiguous or unknown workspace state as safe to delete.

Use `run_root_command` only when host-level privilege is genuinely required. Normal root commands can execute directly, but commands that reference protected Agent resources, can interrupt connectivity/control-plane services, or match destructive-operation policy return `approval_required` first. The connected client/model should explain the exact risk and retry with `approval=true` only after explicit user confirmation.

This is a safety guardrail, not a claim that arbitrary root shell access is mathematically incapable of causing damage. Root remains powerful; the design combines AI-visible self-preservation instructions, server-enforced approval policy, minimal/sanitized root execution environment, audit logging and explicit human gates for known high-risk categories.

### Self-preservation manifest

Before host-wide package, service, firewall, network, disk, user, web-stack or control-panel changes, connected clients/models are instructed to call `agent_environment` and preserve the critical resources it reports.

The manifest identifies, among other things:

- `ai-server-agent.service` and `ai-server-agent-executor.service`;
- `/usr/local/bin/ai-server-agent`;
- `/etc/ai-server-agent`;
- `/var/lib/ai-server-agent`;
- `/var/log/ai-server-agent`;
- the private executor Unix socket under `/run/ai-server-agent/`;
- the configured MCP listen endpoint/port;
- required host primitives such as Bash, systemd and `systemd-run`;
- read-only capacity for the filesystem backing the configured workspace, including an advisory warning when available space is below 2 GiB or 10%; filesystem telemetry never performs cleanup and a telemetry read failure does not make `agent_environment` fail.

The executor separately protects Agent names/paths/socket/listen address and known connection-risk/destructive command patterns. The intent is that ChatGPT both **knows what must survive** and is **server-side gated** when a command directly threatens those resources.

### File I/O and downloads

`read_file` and `write_file` operate on host paths through the Agent. Ordinary project files can also be created/read through `run_command` as `aiworker` inside `/srv/ai-workspace`.

These MCP file tools are intentionally privileged host-file operations, not an unprivileged workspace filesystem. A caller allowed to use `read_file` may reach root-readable sensitive files; `write_file` can produce root-equivalent host changes. Agent protected-resource approval is defense-in-depth and does not classify every sensitive path on the host.

`read_file` is a bounded regular-file ranged read. It accepts raw-byte `offset`/`limit` values (maximum 1 MiB per call), returns `next_offset`, `file_size`, `file_version`, `eof`, byte/truncation metadata and explicit `utf-8` or `base64` encoding, and never loads an arbitrary whole file before applying the limit. A caller assembling several ranges should send the previous `file_version`; replacement/truncation/modification fails with `file_changed` instead of silently stitching different generations together. Kernel/control pseudo-filesystems and non-regular targets such as FIFOs, sockets and devices are not treated as ordinary files.

`write_file` remains a bounded complete replacement for ordinary source/config/text files (maximum 1 MiB). The destination parent must already exist. The executor resolves the parent through a held directory descriptor, rejects final symlinks/special nodes, writes and fsyncs a private temporary file, and preserves existing ownership and mode unless an explicit mode is supplied. Existing files may use the prior `file_version` as an optimistic precondition; those writes use an atomic exchange so the exact destination displaced at commit time can be identity-checked against the validated object. If another object won the pathname race, the executor restores that displaced object only when rollback identity can be proven and returns `file_changed`; any uncertain committed/final state returns `unknown_completion` rather than false success. New-file workflows use `must_not_exist` with `RENAME_NOREPLACE`. Ordinary success is returned only when the final pathname is verified to name the prepared replacement before and after parent-directory fsync.

Protected-resource approval is evaluated against both the requested path and the safely resolved effective path, so a normal symlinked parent/alias cannot turn an Agent-protected target into an unprotected spelling. Unsafe or ambiguous file types fail closed; broad arbitrary host access remains available only through the explicitly privileged shell boundary.

The host can download project dependencies or public files through ordinary command-line tools such as `curl` when the project needs them. Agent credentials/config/state remain protected and must not be copied into chat, source control or public logs.

The MCP file tools are content/path based; they are **not a generic automatic synchronization layer for arbitrary ChatGPT UI attachments**. If a workflow needs a user attachment transferred to/from the VPS, use an explicit supported transfer path and validate that path for the specific client/runtime instead of assuming attachment sync from `read_file`/`write_file` alone.

### Persistent jobs

Use `start_job` for commands that should continue if ChatGPT disconnects or the MCP request ends. The Agent uses transient systemd units and stores job state under the root-controlled Agent state directory. Non-root jobs run as `aiworker`; root jobs go through the same approval policy before starting.

Persistent execution is fail-fast and bounded: at most four Agent jobs may be active at once, there is no hidden queue, each job retains at most 8 MiB of stdout/stderr in a bounded on-disk ring, and the newest 32 terminal job artifact sets are retained. `job_output` uses logical byte offsets and reports the earliest retained offset when older bytes have rolled out; binary chunks are returned as base64. Before accepting new disk-growing job work the executor also requires a state-filesystem safety reserve derived from the hard global persistent-job recovery-state bound, so even abnormal retained jobs remain inside the modeled disk envelope.

`start_job` accepts an optional caller-generated `operation_id`. Retrying the same material request with the same key returns the original job handle; reusing that key for different command/root material fails with `idempotency_conflict`. The durable idempotency/recovery record contains a keyed fingerprint rather than the shell body. Every job gets a bounded internal recovery claim even when the caller does not supply `operation_id`; the caller key only adds replay identity. The protected shell body itself is handed to a fixed `job-runner` entrypoint through a normally short-lived protected job file rather than `bash -c <command>` argv. The runner scrubs the command contents as soon as it has safely consumed them (and on definitive setup failures where it can); privileged terminal reconciliation retires any leftover handoff before reporting a terminal result. A worker-writable status marker never overrides authoritative systemd active/pending state. If the executor dies in the pre-launch window, later job admission retires stale handoffs after the executor's broad connection deadline when no runner/unit evidence exists.

`start_job` also participates in the global lifecycle lock: admission through launch holds a shared lock, while install/update/uninstall/purge use the exclusive lifecycle lock. That makes destructive lifecycle removal mutually exclusive with a new persistent-job launch instead of relying on a racy one-time active-job check. Safe uninstall and purge still refuse to proceed while any `ai-job-*.service` unit is active. Stop the jobs first, then retry the lifecycle action.

### Browser capability

Browser automation is optional and installed on demand. The core MCP service does not depend on Node.js or Chromium.

When browser work is first needed, `browser_setup` can install a private root-owned Node.js/Playwright/Chromium engine under `/opt/ai-server-agent/browser` and the required system libraries. Because this changes host packages/runtime, the setup path requires explicit approval before installation.

After setup, `browser_run` uses **server-side headless Chromium**, not the user's personal desktop browser. Browser profile/session data is kept separately under Agent state so cookies/session state can persist across browser runs without making browser binaries writable by the Agent service account.

## Management commands

Interactive management:

```bash
sudo ai-server-agent-manage
```

Useful subcommands:

```bash
sudo ai-server-agent-manage status
sudo ai-server-agent-manage chatgpt-setup
sudo ai-server-agent-manage configure-cloudflare
sudo ai-server-agent-manage configure-local
sudo ai-server-agent-manage configure-manual-tls
sudo ai-server-agent-manage cloudflare-cleanup
sudo ai-server-agent-manage update
sudo ai-server-agent-manage repair
sudo ai-server-agent-manage uninstall
sudo ai-server-agent-manage purge
```

Privileged install/update/manage/uninstall/purge operations share a root-only lifecycle lock under `/run/lock/ai-server-agent`. A concurrent management operation fails before state mutation rather than racing another lifecycle operation.

## Stable updates

For a stable installation:

```bash
sudo ai-server-agent-manage update
```

The stable updater does not trust a mutable `main` installer. It:

1. reads strict root-only install identity;
2. resolves the latest published immutable stable release unless an explicit stable tag is requested;
3. requires the release to be non-draft, non-prerelease and immutable;
4. requires exactly one uploaded tag-scoped `install.sh` asset;
5. verifies the downloaded `install.sh` bytes against the SHA-256 digest recorded on that GitHub release asset **before executing it**;
6. lets the release-scoped installer verify the release archive against `SHA256SUMS`.

Missing/malformed/contradictory stable install identity fails closed. It does not silently become a source/`main` update.

## Repair

```bash
sudo ai-server-agent-manage repair
```

Repair first restarts and validates the existing services. If local health still fails, it invokes the channel-aware updater rather than guessing a different installation source.

## Uninstall and purge

### Safe uninstall

```bash
sudo ai-server-agent-manage uninstall
```

Safe uninstall removes services, the binary and management command while preserving configuration/state/users/workspace needed for reinstall or repair.

### Purge

```bash
sudo ai-server-agent-manage purge
```

Purge removes Agent-owned server config/state/log/runtime and the `aiagent` identity.

Purge intentionally preserves:

- `/srv/ai-workspace`
- the `aiworker` user and group

Purge does **not** silently delete Cloudflare resources. If recorded Cloudflare resources should be removed, run `cloudflare-cleanup` through the supported management path first.

## Security model

Key boundaries:

- public MCP access requires bearer authentication;
- direct public mode requires native TLS;
- ordinary commands run as `aiworker`;
- root commands are intentional capabilities evaluated by executor policy/approval guardrails;
- connected MCP clients/models are instructed to inspect the current `agent_environment` manifest before host-wide changes;
- protected Agent resources and known connection-risk/destructive root command patterns require explicit approval before execution;
- root shell execution uses `/root` HOME/CWD, a minimal explicit environment and shell startup-file suppression;
- root-consumed control state is stored under root-controlled directories and validated before use;
- persistent job files use no-follow/exclusive creation and checked reads under root-controlled state containers;
- worker-writable workspace/state must not implicitly influence root execution;
- Cloudflare destructive recovery requires ownership plus current representation proof;
- stable install/update/release identity must remain immutable and must not drift to `main`;
- release installer bytes must be authenticated before privileged execution through a bootstrap source anchored to an immutable release tag;
- optional browser binaries are root-owned and browser profile/session data is isolated under Agent state;
- secrets such as the Agent bearer and Cloudflare token must never be persisted in repository content, issue comments, screenshots, chat transcripts or ordinary shell history.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for details.

## Release model

Stable release publication is manual and exact-SHA based. The release workflow requires successful `main` CI and High Assurance Security for the exact release SHA and promotes the already-built CI release artifact rather than rebuilding it.

Immediately before publication, the operator must separately verify repository controls that the workflow credential cannot authoritatively prove, including release immutability and release-tag protection/no-bypass conditions. Automation must not treat an omitted Rulesets `bypass_actors` field as proof that no bypass exists.

The release workflow then creates the previously absent tag at the exact validated SHA, publishes with `--verify-tag`, and performs post-publication immutable-release/tag/attestation checks. Once that release is immutable, its associated tag is the durable source identity for the stable bootstrap path.

Documentation-only commits do not imply a release. The release workflow is manually dispatched with an explicit tag and exact validated `main` SHA.

## Source/development install

Source installation is intentionally separate from stable installation. It may track a source ref and resolves that ref to a commit SHA before installation/update.

Example development checkout:

```bash
git clone https://github.com/ach1992/ai-server-agent.git
cd ai-server-agent
sudo bash install.sh
```

Do not present a mutable source/`main` path as an equivalent stable installer.

## Development and testing

See [docs/TESTING.md](docs/TESTING.md) for the current validation model and known coverage limits.

Typical non-destructive development checks:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race -vet=off ./...
bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh scripts/install-stable.sh scripts/ci-change-scope.sh tests/*.sh
```

Cloudflare, privileged lifecycle and release-provenance changes also have dedicated High Assurance Security coverage.

### Live acceptance baseline

Immutable `v0.1.5` received a real end-to-end acceptance on the supported dedicated VPS path, including:

- supported stable update and installed identity/health;
- real Cloudflare hostname-scoped reconciliation and public native-TLS connectivity;
- missing/invalid bearer rejection plus authenticated MCP initialize;
- real ChatGPT Business custom-MCP connection and tool discovery;
- `agent_environment` self-preservation discovery;
- ordinary `run_command` as `aiworker` in `/srv/ai-workspace`;
- root execution plus `approval_required` behavior for protected/connection-risk operations;
- protected-file guard behavior;
- server-side file read/write and outbound download;
- a real persistent transient-systemd job;
- on-demand Playwright/Chromium installation and a real `browser_run` page/DOM read.

The durable acceptance record is [Issue #11](https://github.com/ach1992/ai-server-agent/issues/11). It is historical evidence, not a reason to skip revalidation when a future change affects the relevant contract.

### When to repeat expensive live/fresh-install validation

Use change impact rather than ritual repetition:

- repeat clean/fresh-install validation when `install.sh`, `scripts/install-stable.sh`, lifecycle/bootstrap logic, supported OS/architecture assumptions, or a defect specifically involving fresh-install state changes;
- repeat live Cloudflare validation when Cloudflare reconciliation, ownership/recovery, TLS, DNS, public binding or provider API assumptions change;
- repeat real ChatGPT custom-MCP validation when endpoint/auth behavior, MCP tool schema/annotations, approval semantics, or important client-side compatibility assumptions change;
- repeat browser setup/run validation when browser installer/runtime/profile behavior changes;
- repeat privileged/root safety validation when executor policy, protected resources, root execution environment or approval behavior changes.

A documentation-only change that does not alter these contracts does not by itself require a new stable release or destructive fresh-install cycle.

## Project map and future development

For future work, use the repository as a graph of authoritative sources rather than reconstructing state from old chat history:

- **README.md** — supported installation, operation, capabilities and safety overview;
- **[AGENTS.md](AGENTS.md)** — stable engineering invariants, development rules and validation expectations for coding agents/contributors;
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — trust boundaries and implementation architecture;
- **[docs/DEVELOPER-RUNTIME.md](docs/DEVELOPER-RUNTIME.md)** — accepted target AI-native Developer Runtime architecture; clearly target-state documentation, not a claim that every capability is already shipped;
- **[docs/TESTING.md](docs/TESTING.md)** — validation model, behavioral coverage and known limits;
- **[docs/CONNECT_CHATGPT.md](docs/CONNECT_CHATGPT.md)** — ChatGPT connection topologies and client-side validation guidance; current OpenAI UI/docs override stale UI wording;
- **Issue #37** — historical decision record for the AI-client/vendor-neutral invariant now owned by `AGENTS.md` and `docs/ARCHITECTURE.md`;
- **Issue #47** — planned MCP Gateway integration with independent credentials and preserved direct-client operation;
- **GitHub Issues** — authoritative place for unresolved actionable work; do not create speculative backlog merely for ceremony;
- **Pull requests and commit history** — implementation/review/integration evidence;
- **GitHub Releases and attestations** — immutable stable-delivery identities and provenance;
- **Issue #11** — completed `v0.1.5` live VPS/Cloudflare/ChatGPT acceptance evidence;
- **Issue #12 / PR #19** — completed root-cause/fix evidence for the Cloudflare Rulesets response-contract defect that led to `v0.1.5`.

When new development begins, start from current `main`, inspect open Issues/PRs and the nearest relevant source/docs/tests, then create only the smallest durable Issue/PR state needed for the new outcome. Do not reopen historical acceptance work unless the same unresolved problem actually returns.

## License

See [LICENSE](LICENSE).
