# Installation, Configuration and Operations

**Audience:** Server owner/operator. This guide details the current published Agent release and management commands. For the product's durable purpose, read [PROJECT-SPEC.md](PROJECT-SPEC.md). [README.md](../README.md) is the short entrypoint. GitHub Releases is authoritative for current Stable identity, while the source branch may contain unfinished features.

## Platform support

The supported Stable platform policy is:

- **Ubuntu 22.04 or newer**
- **Debian 11 or newer** (Debian 11 application compatibility is retained, but upstream Debian LTS ended on 2026-08-31; use Debian 12+ for an officially security-maintained base unless you have an ELTS arrangement)
- **amd64/x86_64 and arm64/aarch64**
- systemd
- a dedicated development/test server where you are comfortable granting an AI-controlled MCP endpoint the documented capabilities

OS support is minimum-version based, while CI keeps explicit compatibility cases for current Ubuntu/Debian releases and native lifecycle coverage on both supported CPU architectures. Stable release architectures are declared in `scripts/release-arches.txt`; installer architecture aliases are normalized at the install boundary so adding a future architecture does not require rewriting release publication logic.

AI Server Agent does not require nginx, Apache, Caddy, Docker, PHP, a database, Node.js, Python, `cloudflared`, or a hosting panel as core dependencies, and it does not need to take over ports 80/443.

## Install the latest stable release

Stable installation uses the maintained `scripts/install-stable.sh` bootstrap from the repository's `main` branch. The **public command never changes when a new Stable is published**: it contains no release tag or commit SHA. The bootstrap source is mutable and therefore trusted as reviewed, protected project source; unlike a pinned commit, its bytes are not immutable. The actual privileged installer is still selected from **published immutable GitHub Releases** and SHA-256-checked before execution.

To install the **latest published immutable Stable**:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | bash
```

To install a **specific published Stable**, for example `v0.1.17`:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | bash -s -- v0.1.17
```

**Fresh installations only:** both commands reject a machine with Agent-owned
installation files, state, or services, including incomplete installs. The
bootstrap checks before release requests or optional privileged APT
prerequisites, then checks again in its privileged handoff. A specific Stable
tag is **not** a downgrade mechanism. To update an installed Agent, run
`sudo ai-server-agent-manage update`; to return to an older version, follow
the credential preparation and protected-updater instructions below. Never
rerun a historical installer or bootstrap over an occupied installation.

The default resolves GitHub's newest published non-draft, non-prerelease, immutable release; the optional tag selects the named release and fails closed if it is not immutable. When `jq` is missing, the bootstrap may first invoke `sudo apt-get update` and `sudo apt-get install ca-certificates jq` for supported Ubuntu/Debian hosts using configured APT repositories, **before** fetching or verifying release metadata. This privileged **OS prerequisite** operation is distinct from privileged execution of release `install.sh`: the latter occurs only **after** SHA-256 verification, followed by digest re-verification in protected root staging. **Do not use `curl ... | sudo bash`** for this initial bootstrap, and do not execute a moving release asset as root. If stronger bootstrap-source provenance is required, operators can independently review or pin the source, but the normal one-line install command deliberately uses the permanent `main` URL. The installed manager already supports version-free Stable updates via `sudo ai-server-agent-manage update`.

Do **not** use `releases/latest/download/install.sh | sudo bash` as the stable trust path: that executes release-supplied code as root before the same asset can be authenticated.

Although its public **source** comes from reviewed `main`, the Stable bootstrap does **not** install Agent binaries or release assets from `main` or fall back to a source build. It:

1. checks required bootstrap tools; if `jq` is absent, on supported Debian/Ubuntu hosts only, validates the host and installs the OS prerequisite using privileged APT (before release authentication);
2. resolves the requested release or GitHub's latest release metadata;
3. requires a published, non-draft, non-prerelease, immutable release;
4. requires exactly one uploaded `install.sh` asset at the exact tag-scoped release URL;
5. requires the GitHub release asset `sha256:` digest;
6. downloads `install.sh` without root execution and verifies its bytes against that digest;
7. enters the separate privileged **release-installer staging** path only after verifying downloaded installer bytes (the optional OS prerequisite privilege operation precedes this);
8. copies the candidate into a root-controlled staging directory, re-verifies the same authenticated digest on the protected copy, and executes only that protected copy.

### Fresh-install baseline and upgrade compatibility

Issue #140's first optional multi-language slice was published starting in immutable Stable `v0.1.13` and remains part of later releases. Optional language-adapter readiness must be checked on the **actual installed host**; source tests do not install those dependencies. [#137](https://github.com/ach1992/ai-server-agent/issues/137) and [#59](https://github.com/ach1992/ai-server-agent/issues/59) record the Owner-accepted bounded real-host/client validation, not a claim that every optional adapter or failure case was tested. Additional invasive fresh-host or fault tests are not release prerequisites without a concrete new risk.

Do **not** infer state-preserving migration of a historical `v0.1.12` installation, including credentials, active jobs, Browser profiles or terminals, from a new-baseline clean-install test. Before updating an occupied older host, separately verify backup/recovery and the applicable update procedure. This does not remove backward-compatible MCP names or the Agent's required restart/reconnect/cleanup guarantees within the new baseline. Subsequent baseline upgrades require evidence appropriate to their actual changes.

Optional language-server/debugger runtimes are **not** core dependencies or automatically installed. Missing complete, administrator-controlled and version-verified adapter resources return explicit unavailable/dependency errors. In a connected MCP client, check `debug_adapter_status(language=php)` before attempting PHP CLI or Service-owned HTTP debugging; a successful tool call returning `status=unavailable` is not usable adapter readiness. Source integration or a Stable upgrade alone does not provision Xdebug, the pinned PHP DBGp-to-DAP adapter, or its trusted runtime. For supported language methods/modes and deferred functionality, see [Tool Reference](TOOL-REFERENCE.md#optional-multi-language-code-and-debugger-readiness) and [Issue #149](https://github.com/ach1992/ai-server-agent/issues/149). Real DAP launches execute selected workspace code; use an authorized test host, not an assumed read-only inspection.

**Bash Shell DAP kernel IPC preflight (source-only until explicitly published):** On the intended, separately authorized host, inspect `cat /proc/sys/kernel/yama/ptrace_scope`. Shell DAP requires the Yama policy to exist and return **1, 2, or 3**, in addition to the existing root Executor, private cgroup-v2 and trusted Bash checks. An absent file, `0`, or unreadable/invalid policy means `debug_adapter_status(language=shell)` reports unavailable and the Shell session fails closed. The Root Executor creates 0600 root-owned anonymous event/command pipes before worker credential drop; the Yama requirement additionally prevents an unrelated same-UID process from copying those inherited FDs using `pidfd_getfd`. The Agent **does not change any host-wide ptrace/sysctl value**. Altering that operator-owned system security policy is a separate, explicit host decision; do not change occupied development/validation/production hosts merely to run a source test. CI separately proves same-UID peer denial with an ephemeral root-owned fixture. No currently published Stable or occupied Agent is claimed to include this capability.

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

The interactive manager shows the verified installed build, server identity/purpose and non-secret connection details in its header. Use **0/Back** to leave a menu, **b** to cancel setup inputs before any commit, or **No** to decline a confirmation. An invalid choice stays in the menu. Failed actions report their result and keep the console usable when the manager is still installed. The menus use text-backed semantic signals (`INFO`, `SUCCESS`, `WARNING`, `ERROR`) and consistent choice coloring: cyan navigation/action, yellow for caution, red for destructive operations; `NO_COLOR` and non-TTY output remain readable without ANSI. Verify the actual version/identity shown in the header; repeatable test procedures and limitations are in [Testing](TESTING.md#operator-console--first-run-ux-acceptance); bounded installed-console acceptance completed under [closed Issue #147](https://github.com/ach1992/ai-server-agent/issues/147) and shipped in immutable Stable v0.1.18. Do not repeat its installed fault-injection cases on an occupied host. Run `ai-server-agent-manage help` to inspect the command groups without opening the interactive menu. When a one-line installer streams script bytes through stdin, the interactive flow still detects a real controlling terminal on stdout/stderr; a fully detached or redirected process will not be asked for input. Before interactive runtime/purpose edits or update/repair, the manager asks for explicit confirmation; noninteractive subcommands retain their existing calling contracts.

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

The Agent keeps a bounded named-principal credential set. `direct/default` is the normal direct-MCP client principal. An optional `direct/secondary` identity lets a second client use an independently rotatable/revocable bearer while sharing the same supported tools and approval policy. No separate connector is installed. Any older stored `mcp-gateway` identity is retained only as an inert compatibility record: its bearer cannot authenticate and a new secondary issue replaces the record. Callers cannot select or override their authenticated principal.

Use `sudo ai-server-agent-manage credential-status` for non-secret state, `credential-rotate direct-default|direct-secondary` to issue or rotate one credential, and `credential-revoke direct-default|direct-secondary` to revoke one. Secondary credentials are optional, not created at installation. Rotation is immediate after verified service cutover: there is no grace overlap. Revoking the last **usable** credential is refused, even if an inert retired record remains in the store. Safe uninstall preserves the credential store; update/repair preserve it; purge removes Agent-owned credential state so a later fresh install gets fresh credentials.

Existing installations migrate the current bearer into `direct/default` without forced rotation. Migration activates and verifies the verifier-backed store before the legacy plaintext token/header file is removed; on failed activation the previous known-good legacy configuration is restored for recovery.

### Credential compatibility when upgrading or rolling back

Before replacing an installed binary, the installer asks the **target** Agent to
validate the current named credential store. Older binaries without that
read-only command must prove they understand named credentials and pass a
narrow, fail-closed legacy-schema check. An incompatible store aborts **before**
the executable, credential store, config or active services are replaced.
Do not work around this by copying binaries manually.

If an older store has `direct/default` revoked and only the retired
`mcp-gateway` identity enabled, the new Agent will deliberately refuse it:
the retired bearer cannot authenticate. On the still-working old installation,
explicitly issue/rotate `direct-default`, save the one-time bearer securely,
and verify that the direct connection works **before** retrying the upgrade.
No credentials are silently generated during upgrade.

To return to an older version that does not recognize `direct/secondary`, first
verify that your existing `direct/default` bearer works and run:

```bash
sudo ai-server-agent-manage credential-prepare-rollback
```

This is an interactive, explicitly confirmed **destructive credential
operation**, not part of automatic update: it removes all optional credential
records (including revoked `direct/secondary` and any retired Gateway record)
while preserving the active `direct/default` verifier, and restarts/health-
checks the current Agent with recovery on failure. Secondary clients lose
access immediately. The removed records cannot be recovered from retained
plaintext; issue fresh secondary credentials after returning to a compatible
newer version. Only after this cleanup should an operator retry a rollback through the
**installed, protected updater**; for example, to select published immutable
Stable `v0.1.14` explicitly (only where this older version is otherwise
compatible):

```bash
sudo env AI_SERVER_AGENT_UPDATE_CHANNEL=stable AI_SERVER_AGENT_VERSION=v0.1.14 /usr/local/sbin/ai-server-agent-manage update
```

The updater independently checks credentials before invoking the downloaded
historical installer. Neither public Stable bootstrap command is an update
or downgrade route for an existing installation.

A raw rollback of an `enabled=true` retired Gateway record could reactivate
its bearer; a raw rollback with even a revoked `direct/secondary` record could
prevent the older Agent from starting. Both cases are intentionally blocked.

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

**Environment purpose profiles:** Development/Validation are included in immutable Stable `v0.1.13` and later; **Production was introduced in immutable Stable `v0.1.15`** (not `v0.1.14`). The three profiles describe code/build/debug work, candidate testing/acceptance, or live workloads with a request for fresh human consent before each change. Production is **guidance only, not enforced read-only access or verified per-action approval**. For `profile-show`, `profile-set`, `profile-label` and `profile-label-clear`, installer options, Agent-only restart/reconnect, and MCP discovery, see [Environment Purpose Profiles](ENVIRONMENT-PROFILES.md). Profiles do not change existing MCP/root/worker permissions or connection mode.

Runtime settings persist in the existing root-managed `/etc/ai-server-agent/config.json`. `runtime-show` lists the effective configured values, safe value ranges and units; `runtime-set` and `runtime-reset` validate with the installed binary **before** updating the config, restart Agent/Executor, verify local health, and restore the prior config on verified restart failure. Run those commands in an independent SSH/operator terminal because restarting the Agent may disconnect an MCP client. No source edit or new release is needed to change supported settings **once this capability has been installed**. A configured value is not guaranteed active until both services restart and health is checked. An invalid value never restarts them. These are per-request/network operational budgets, not a way to disable authentication, bypass approval or transfer an unlimited file in one MCP response. Large authorized files use bounded version-pinned reads and the separate staged worker upload contract; real-client attachment and typed-artifact acceptance remain Issue #99.

### Execution concurrency (optional)

The console's **Runtime limits and execution capacity** section offers independent controls for synchronous unprivileged commands, persistent tasks, persistent services and shared LSP/DAP/Terminal/Browser sessions. Safe defaults remain CPU-aware worker commands (`worker_command_slots=0`: `min(GOMAXPROCS, 4)`), four active tasks (`persistent_task_slots=4`) and two active services (`persistent_service_slots=2`). An operator can configure each independently up to **64** without rebuilding the Agent:

```bash
sudo ai-server-agent-manage runtime-show
sudo ai-server-agent-manage runtime-set worker_command_slots 8
sudo ai-server-agent-manage runtime-set persistent_task_slots 64
sudo ai-server-agent-manage runtime-set persistent_service_slots 16
sudo ai-server-agent-manage runtime-set stdio_session_slots 16
```

All four capacity keys are validated (`worker_command_slots`: 0 for auto or 1–64, tasks/services/shared stdio sessions: 1–64). They do not change the fixed one-at-a-time **privileged root command** limit. Invalid values are rejected before changes; accepted changes use the existing privileged lifecycle lock, validated Agent/Executor restart, health verification and rollback. Use `runtime-reset <key>` to restore the key's default.

Lowering **either** persistent job limit does **not** stop existing tasks/services: `runtime_list` reports their real active counts, and new admission for that job kind remains blocked until it falls below the new limit. A different job kind retains its own capacity. The trusted active inventory, bounded retained-claim namespace, and log/audit disk reservations account for all running jobs across managed restarts and downscaling. The authenticated `runtime_list` reports the configured task, service and shared-session limits, including capacity still occupied by older services after a downscale. At the 64+64 maximum, the required safety reserve reaches approximately 1.6 GiB; without adequate free disk new job admission fails with a typed `resource_limit` rather than risking log/audit integrity. After a `stdio_session_slots` downscale, older durable Terminal scopes retain their protected reservations. Reconnect with the original authenticated `session_id`/owner/workspace/authority and then close with the new `session_epoch`; previously authorized recovery is not a new admission. New LSP/DAP/Browser/Terminal starts remain blocked while aggregate occupancy exceeds the current configured limit. Untrusted registry entries and unverifiable scope cleanup never free capacity.

Sixty-four slots is a **per-kind admission ceiling**, not a CPU/RAM/FD/systemd guarantee or a scheduler. High settings permit 64 persistent tasks **and** 64 persistent services plus separately configured worker commands; provision CPU, memory, processes, file descriptors and disk accordingly, and raise gradually based on observed usage. On small hosts leave defaults. Existing protocol payload, root-action approval, timeout, log-size and trust boundaries are unchanged. The commands only work after this source change is published and installed; **do not run them against older Stable binaries**. Because management restarts Agent/Executor, change settings from an independent operator SSH terminal, not the MCP connection being restarted.

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
- MCP bearer credentials are named direct-client principals (`direct/default` and optional `direct/secondary`) with independently revocable/rotatable verifier records; bearer plaintext is not retained after issuance/migration;
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
- Stable installation **target** and release identity must remain immutable; do not treat the `main` bootstrap source as the selected release or fall back to `main` binaries;
- the reviewed mutable `main` bootstrap is an initial source-trust boundary; the selected release must be immutable and the release installer bytes must be authenticated before privileged execution;
- optional browser binaries are root-owned and browser profile/session data is isolated under Agent state;
- secrets such as an issued Agent bearer and Cloudflare token must never be persisted in repository content, issue comments, screenshots, chat transcripts or ordinary shell history; the installed MCP credential store contains verifier digests and non-secret principal metadata only.

The authenticated `agent_environment` tool and `/agent-environment.json` endpoint rebuild their self-preservation manifest for each request. The optional LSP/DAP adapter `installed` flags still require fresh, complete, root-provenance and pinned-content integrity validation. To avoid serial multi-tree verification delaying the manifest, these independent readiness checks overlap with **at most two concurrent probes per request**; results are collected before responding, never cached or inferred from path existence. This speeds up the advisory environment view without changing authentication, privileged action policy, or the separate adapter validation/containment requirements at `code_*` and `debug_*` use.

See [docs/ARCHITECTURE.md](ARCHITECTURE.md) for details.

## Release model

Stable release publication is manual and exact-SHA based. The release workflow requires successful `main` CI and High Assurance Security for the exact release SHA and promotes the already-built CI release artifact rather than rebuilding it.

Immediately before publication, the operator must separately verify repository controls that the workflow credential cannot authoritatively prove, including release immutability and release-tag protection/no-bypass conditions. Automation must not treat an omitted Rulesets `bypass_actors` field as proof that no bypass exists.

The release workflow then creates the previously absent tag at the exact validated SHA, publishes with `--verify-tag`, and performs post-publication immutable-release/tag/attestation checks. Once that release is immutable, its tag is the durable identity of the verified release artifacts; the permanent public bootstrap source remains the separately reviewed `main/scripts/install-stable.sh` path.

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
