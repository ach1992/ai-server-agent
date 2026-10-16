# Installation, Configuration and Operations

**Audience:** Server owner/operator. This guide details the current published Agent release and management commands. For the product's durable purpose, read [PROJECT-SPEC.md](PROJECT-SPEC.md). [README.md](../README.md) is the short entrypoint. GitHub Releases is authoritative for current Stable identity, while the source branch may contain unfinished features.

## Platform support

The stable supported-platform policy (including published immutable [`v0.1.12`](https://github.com/ach1992/ai-server-agent/releases/tag/v0.1.12) and later compatible source/releases) is:

- **Ubuntu 22.04 or newer**
- **Debian 11 or newer** (Debian 11 application compatibility is retained, but upstream Debian LTS ended on 2026-08-31; use Debian 12+ for an officially security-maintained base unless you have an ELTS arrangement)
- **amd64/x86_64 and arm64/aarch64**
- systemd
- a dedicated development/test server where you are comfortable granting an AI-controlled MCP endpoint the documented capabilities

OS support is minimum-version based, while CI keeps explicit compatibility cases for current Ubuntu/Debian releases and native lifecycle coverage on both supported CPU architectures. Stable release architectures are declared in `scripts/release-arches.txt`; installer architecture aliases are normalized at the install boundary so adding a future architecture does not require rewriting release publication logic.

`v0.1.7` is the first stable release with this expanded platform matrix.

AI Server Agent does not require nginx, Apache, Caddy, Docker, PHP, a database, Node.js, Python, `cloudflared`, or a hosting panel as core dependencies, and it does not need to take over ports 80/443.

## Install the latest stable release

Stable installation starts with a small bootstrap loaded from an **immutable published release tag**, separate from the release `install.sh` asset it authenticates. The currently documented verified bootstrap trust anchor is the immutable [`v0.1.12` release](https://github.com/ach1992/ai-server-agent/releases/tag/v0.1.12), not the mutable `main` branch. This anchor can resolve a newer *published* immutable Stable; it is not a declaration that `v0.1.12` remains the latest release. GitHub Releases, rather than this page, remain authoritative if a newer version is published.

To install the **latest immutable Stable** identified by the GitHub Releases API using that pinned bootstrap:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/v0.1.12/scripts/install-stable.sh | bash
```

To install the **older immutable `v0.1.12`** explicitly instead of following the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/v0.1.12/scripts/install-stable.sh | bash -s -- v0.1.12
```

The bootstrap authenticates the release-scoped installer digest **before** privileged execution. The first command resolves GitHub's current latest immutable release; the second requires the named release. On the next release, update the documented bootstrap trust anchor only after its tag is immutable and its bootstrap has been verified.

Do **not** use `releases/latest/download/install.sh | sudo bash` as the stable trust path: that executes release-supplied code as root before the same asset can be authenticated.

The stable bootstrap does **not** fall back to `main`. It:

1. resolves the requested release or GitHub's latest release metadata;
2. requires a published, non-draft, non-prerelease, immutable release;
3. requires exactly one uploaded `install.sh` asset at the exact tag-scoped release URL;
4. requires the GitHub release asset `sha256:` digest;
5. downloads `install.sh` without root execution and verifies its bytes against that digest;
6. crosses the privilege boundary only after that unprivileged verification succeeds;
7. copies the candidate into a root-controlled staging directory, re-verifies the same authenticated digest on the protected copy, and executes only that protected copy.

### Fresh-install baseline and upgrade compatibility

The first multi-language Stable slice of Issue #140 is accepted against **first-ever installation on an empty supported host**. Passing that fresh-install test does **not** prove a state-preserving in-place upgrade from the older `v0.1.12` installation or validate migration of its credentials, active jobs, Browser profiles and terminals. Do not assume an occupied older Agent host can safely auto-update to the new baseline without a separately verified backup/recovery and upgrade procedure. This limitation concerns old-to-new installed state, **not** ordinary backward-compatible MCP tool names or the Agent's required restart/reconnect/cleanup guarantees within the new baseline. Later upgrades from the new baseline need their own compatibility proof.

Optional language-server/debugger runtimes are **not** core dependencies or automatically installed. Missing complete, administrator-controlled and version-verified adapter resources return explicit unavailable/dependency errors. For supported language methods/modes and deferred functionality, see [Tool Reference](TOOL-REFERENCE.md#optional-multi-language-code-and-debugger-readiness) and [Issue #149](https://github.com/ach1992/ai-server-agent/issues/149). Real DAP launches execute selected workspace code; use an authorized test host, not an assumed read-only inspection.

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

### Interactive console navigation and safe cancellation

The interactive manager shows the verified installed build, server identity/purpose and non-secret connection details in its header. Use **0/Back** to leave a menu, **b** to cancel setup inputs before any commit, or **No** to decline a confirmation. An invalid choice stays in the menu. Failed actions report their result and keep the console usable when the manager is still installed. The menus use text-backed semantic signals (`INFO`, `SUCCESS`, `WARNING`, `ERROR`) and consistent choice coloring: cyan navigation/action, yellow for caution, red for destructive operations; `NO_COLOR` and non-TTY output remain readable without ANSI. Review the [CLI inventory/presentation contract](OPERATOR-CLI-UX.md) for the source interface and test boundary. Run `ai-server-agent-manage help` to inspect the command groups without opening the interactive menu. Before interactive runtime/purpose edits or update/repair, the manager asks for explicit confirmation; noninteractive subcommands retain their existing calling contracts.

**Ctrl+C** cancels a running *read-only* view (status/inspection) or a pending menu selection without ending the manager. In contrast, runtime/profile/credential/connection changes, Cloudflare transactions, updates, repair and uninstall may be holding a lifecycle lock, committing configuration, changing remote resources or restarting services. These protected operations **do not accept Ctrl+C cancellation mid-change**, because interruption could bypass rollback/recovery. Cancel at an offered input/confirmation, or let the protected action finish; if it fails, check its recovery/status message before retrying. Closing the terminal, killing the manager, or losing SSH cannot be assumed to roll back an active transaction. For unattended automation, use the documented noninteractive subcommands, whose existing exit/signal contracts are unchanged.

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

The interactive manager uses the Cloudflare API token only for the current command and does not store it in Agent managed state. Noninteractive automation can supply a protected Cloudflare API token file through the documented environment variable instead of putting the token on a command line.

Cloudflare verification separately requires an **active MCP bearer**, not the Cloudflare API token. A fresh installer supplies its newly issued direct/default bearer to the manager over a one-use inherited file descriptor; no recoverable plaintext file is created. Later interactive Cloudflare reconfiguration requests the current MCP bearer in a hidden terminal prompt and validates it against the local Agent **before any Cloudflare mutation**, then proves public HTTPS `/healthz`, unauthorized MCP rejection, and authenticated MCP initialization. Existing bearer plaintext is not recoverable from `mcp-credentials.json` (verifier digests only); if lost, use `sudo ai-server-agent-manage credential-rotate direct-default`, securely update the connected client, and retry. Do **not** recreate legacy `/etc/ai-server-agent/mcp.authorization`. For noninteractive reconfiguration with an existing credential, provision a one-use descriptor 3 to `AI_SERVER_AGENT_MCP_AUTH_FD=3` using your automation secret store; never put a bearer in an argument, environment value or repository.

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

## ChatGPT setup

After public setup succeeds:

```bash
sudo ai-server-agent-manage chatgpt-setup
```

The manager prints the MCP URL and bearer-auth setup guidance. Active bearer plaintext is **not** retained for generic re-display: the server stores a versioned one-way verifier plus non-secret principal metadata. Use `sudo ai-server-agent-manage credential-rotate direct-default` to issue a replacement direct credential and reveal it once in the protected terminal at issuance.

ChatGPT full MCP/custom-app support is an evolving client-side feature. For managed workspaces, creation and administration currently use the available Plugins/custom-MCP surfaces and workspace controls; exact labels and navigation can change independently of the Agent. Use the current OpenAI product guidance and live UI at connection time rather than treating a historical `Developer mode`, `Apps -> Create`, screenshot, or menu sequence as a protocol contract.

See [docs/CONNECT_CHATGPT.md](CONNECT_CHATGPT.md) for connection topologies and the validation checklist. When that document's client-side UI wording differs from the current ChatGPT product, current OpenAI guidance and the live UI are authoritative for the client-side steps; the Agent-side endpoint/auth/tool contract remains the durable part documented here.

The public endpoint remains bearer-authenticated. Treat the bearer credential as a privileged server-control credential and provide it only to the trusted ChatGPT connection UI when configuring the app.

### MCP credential lifecycle

The Agent keeps a bounded named-principal credential set. `direct/default` is the normal direct-client principal; `mcp-gateway` is an optional separately managed principal for a future Gateway connection and does not make Gateway a runtime dependency. The server derives principal ID/class/name from the matched verifier and attaches it to request context; callers cannot select or override that identity.

Use `sudo ai-server-agent-manage credential-status` for non-secret state, `credential-rotate direct-default|mcp-gateway` to issue or rotate a credential, and `credential-revoke direct-default|mcp-gateway` to revoke one. Rotation is immediate after verified service cutover: there is no grace overlap. Revoking the last active credential is refused. Safe uninstall preserves the credential store; update/repair preserve it; purge removes Agent-owned credential state so a later fresh install gets fresh credentials.

Existing installations migrate the current bearer into `direct/default` without forced rotation. Migration activates and verifies the verifier-backed store before the legacy plaintext token/header file is removed; on failed activation the previous known-good legacy configuration is restored for recovery.

## Management commands

Interactive management:

```bash
sudo ai-server-agent-manage
```

Useful subcommands:

```bash
sudo ai-server-agent-manage status
sudo ai-server-agent-manage runtime-show
sudo ai-server-agent-manage runtime-set http_idle_timeout_seconds 300
sudo ai-server-agent-manage runtime-reset http_idle_timeout_seconds
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

**Development/Validation purpose profiles:** included in immutable Stable `v0.1.13` and later supporting builds (not in `v0.1.12`). For the version-gated `profile-show`, `profile-set`, `profile-label` and `profile-label-clear` commands, first-install options, Agent-only restart/reconnect behavior, and direct-client metadata, see [Environment Purpose Profiles](ENVIRONMENT-PROFILES.md). Profiles do **not** change MCP/root/worker permissions or the independent connection mode. Verify the installed release contains the feature before using these commands.

Runtime settings persist in the existing root-managed `/etc/ai-server-agent/config.json`. `runtime-show` lists the effective configured values, safe value ranges and units; `runtime-set` and `runtime-reset` validate with the installed binary **before** updating the config, restart Agent/Executor, verify local health, and restore the prior config on verified restart failure. Run those commands in an independent SSH/operator terminal because restarting the Agent may disconnect an MCP client. No source edit or new release is needed to change supported settings **once this capability has been installed**. A configured value is not guaranteed active until both services restart and health is checked. An invalid value never restarts them. These are per-request/network operational budgets, not a way to disable authentication, bypass approval or transfer an unlimited file in one MCP response. Large authorized files use bounded version-pinned reads and the separate staged worker upload contract; real-client attachment and typed-artifact acceptance remain Issue #99.

### Execution concurrency (optional)

The console's **Runtime limits and execution capacity** section offers separate guided controls for synchronous unprivileged commands and persistent task jobs. The safe defaults remain CPU-aware commands (`worker_command_slots=0`: `min(GOMAXPROCS, 4)`) and four active tasks (`persistent_task_slots=4`). An operator can optionally set either independently, up to eight:

```bash
sudo ai-server-agent-manage runtime-show
sudo ai-server-agent-manage runtime-set worker_command_slots 8
sudo ai-server-agent-manage runtime-set persistent_task_slots 8
```

These controls do not change the fixed one-at-a-time root command limit or two persistent service slots. Invalid values are rejected before changes, and all accepted changes use the existing privileged lock, validated restart, health check and rollback path. `runtime-reset worker_command_slots` and `runtime-reset persistent_task_slots` restore defaults.

Lowering `persistent_task_slots` does **not** stop already-running tasks. `runtime_list` may temporarily report more active tasks than the newly configured limit; new task admission stays blocked until active tasks fall below that limit. The two persistent service slots remain independently available subject to their own quota and filesystem safety reserve. Job-log and audit disk reservations continue to cover the historical active task exposure up to the hard limit of eight.

Eight available slots is an **admission ceiling**, not a CPU/RAM guarantee. On small hosts (notably two-vCPU machines with limited RAM or exhausted swap), leave the defaults or raise gradually while watching real resource usage. Only use these commands after the installed Agent version supports them; the source implementation alone does not modify the running service.

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
- MCP bearer credentials are named principals (`direct/default` and optional `mcp-gateway`) with independently revocable/rotatable verifier records; bearer plaintext is not retained after issuance/migration;
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
- secrets such as an issued Agent bearer and Cloudflare token must never be persisted in repository content, issue comments, screenshots, chat transcripts or ordinary shell history; the installed MCP credential store contains verifier digests and non-secret principal metadata only.

See [docs/ARCHITECTURE.md](ARCHITECTURE.md) for details.

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

## Troubleshooting and actual acceptance

Check the installed Agent version, local health, service/journal output, and the structured error returned by the failed tool. Do not assume that source-only features are installed or that a ChatGPT client displays every typed/binary result.

For observed installed behavior, acceptance gaps, and recovery proof, consult [Issue #59](https://github.com/ach1992/ai-server-agent/issues/59), [Issue #99](https://github.com/ach1992/ai-server-agent/issues/99), and the exact immutable [GitHub release](https://github.com/ach1992/ai-server-agent/releases). Earlier release-specific acceptance histories belong to those GitHub records, not to this operator guide.
