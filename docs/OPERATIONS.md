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

To install a **specific published Stable**, for example `v0.1.20`:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | bash -s -- v0.1.20
```

**Fresh installations only:** both commands reject a machine with Agent-owned
installation files, state, or services, including incomplete installs. The
bootstrap checks before release requests or optional privileged APT
prerequisites, then checks again in its privileged handoff. A specific Stable
tag is **not** a downgrade mechanism. To update an installed Agent, run
`sudo ai-server-agent-manage update`. A rollback is supported only to a
published Stable satisfying the protected updater's security and credential
contracts (see below); old unguarded installers are refused. Never rerun a
historical installer or bootstrap over an occupied installation.

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

**Bash Shell DAP kernel IPC preflight (included in immutable Stable v0.1.19):** On the intended, separately authorized host, inspect `cat /proc/sys/kernel/yama/ptrace_scope`. Shell DAP requires the Yama policy to exist and return **1, 2, or 3**, in addition to the existing root Executor, private cgroup-v2 and trusted Bash checks. An absent file, `0`, or unreadable/invalid policy means `debug_adapter_status(language=shell)` reports unavailable and the Shell session fails closed. The Root Executor creates 0600 root-owned anonymous event/command pipes before worker credential drop; the Yama requirement additionally prevents an unrelated same-UID process from copying those inherited FDs using `pidfd_getfd`. The Agent **does not change any host-wide ptrace/sysctl value**. Altering that operator-owned system security policy is a separate, explicit host decision; do not change occupied development/validation/production hosts merely to run a source test. CI separately proves same-UID peer denial with an ephemeral root-owned fixture. Immutable Stable v0.1.19 includes this capability, but installed binary identity, adapter readiness and actual connected-client behavior require separate verification; no occupied host or sysctl change is implied.

### What the verified release installer does

The release-scoped `install.sh` is generated with its version/ref pinned to the release tag. After the bootstrap authenticates it, the installer:

1. checks the supported stable OS/architecture;
2. downloads the matching release archive and `SHA256SUMS`;
3. verifies the archive checksum before installation;
4. installs the Agent/services and management helpers;
5. starts the core Agent locally and verifies health;
6. optionally launches the first-run connection wizard.

Stable installation rejects a local `AI_SERVER_AGENT_BINARY` override. Stable payload identity is the published immutable release, not a mutable branch.

**Development/source binary overrides:** `AI_SERVER_AGENT_BINARY` is supported only for deliberate source-mode installation by an operator who trusts the original binary bytes and the installer checkout. The privileged installer validates that the supplied binary is an existing regular executable with root-owned, non-group/world-writable ancestry and leaf, and no symlink/hardlink replacement path, **before** locks, package operations or any privileged binary compatibility probe. Do not point this override directly at a build under `/tmp` or an unprivileged writable checkout. After independently verifying the candidate's provenance, stage it using `sudo install -o root -g root -m 0755` into a dedicated root-owned, non-writable directory (for example under `/run`) and supply its exact absolute path. The original bytes and the standalone installer still require operator trust; this path guard does not authenticate the authorship of arbitrary source code or turn a source build into an immutable Stable release.

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
- `aiworker`: ordinary command/workspace account;
- `aibrowser`: dedicated nologin UID/GID for isolated managed Chromium and private ZIP traces. Its numeric UID and GID must differ from both `aiagent` and `aiworker` (including named protected groups). Unsafe pre-existing aliases refuse install and isolated Browser admission, without renumbering accounts.

Services:

- `ai-server-agent.service`
- `ai-server-agent-executor.service`

**Service startup trust:** Both installed services verify the active Agent executable's root ownership, regular/executable type, link safety and nonwritable pathname ancestry before each systemd startup. The unprivileged Agent uses the same installed helper in read-only `--verify-agent-binary` `ExecStartPre` mode under its existing sandbox. The Root Executor uses the normal startup helper for the same binary preflight followed by root-only lifecycle lock creation. Unsafe filesystem drift refuses service startup with actionable diagnostics; do not auto-change permissions on an occupied host. The guard and OS interpreter must themselves be trusted before launch; older rollback installers can restore older startup checks.


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

The Agent supports **any practical number of independently named direct-MCP clients**, without a hard-coded two-client principal cap. Fresh installations still issue conventional `direct/default`; existing `direct/secondary` records remain fully supported. Additional client names such as `direct/chatgpt`, `direct/ci_runner` or `direct/development` use canonical IDs `direct-chatgpt`, `direct-ci_runner`, `direct-development`. Each receives its own independently rotatable/revocable 64-hex-character bearer, with the same supported tools and approval rules; a client cannot select or override another principal. An older stored `mcp-gateway` identity stays inert and its bearer never authenticates; new credential issuance removes that retired record. No second connector/permission engine is introduced.

Use `sudo ai-server-agent-manage credential-status` for non-secret inventory, `sudo ai-server-agent-manage credential-rotate direct-chatgpt` to issue or rotate a named client, and `sudo ai-server-agent-manage credential-revoke direct-chatgpt` to revoke it. `direct-default` and `direct-secondary` continue working as principal IDs. The interactive Credentials menu also accepts additional client IDs. Only lowercase ASCII letters, digits, underscores and hyphens are allowed after the `direct-` prefix to keep principal identity canonical and safe in shell/API/audit contexts. Rotation is immediate after verified service cutover, with no grace overlap. Revoking the last **usable** bearer is refused, even if a retired record remains. Safe uninstall/update/repair preserve the credential store; explicit purge removes it. This is capability availability, not automatic permission to issue a credential without operator authorization.

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

### Supported Stable rollback boundary

Rollback is a recovery path, **not** an entitlement to execute every historical
installer. The installed protected updater authenticates a published immutable
Stable installer against its GitHub release SHA-256 and then requires that
installer to declare `INSTALLED_PRIVILEGED_LIFECYCLE_TRUST_V1`. This means its
release contract includes the privileged helper pathname guards and both
systemd service startup checks. A missing contract refuses the update before
any older installer runs; there is no legacy bypass switch. Ordinary fresh
Stable installation through the public bootstrap follows its own release
verification and is not changed by this installed-updater policy.

When this hardened rollback policy was adopted, the latest published Stable
was `v0.1.18`, whose installer (and all earlier installers) lacks the named
privileged lifecycle trust contract. **Immutable Stable `v0.1.19` is the first
published candidate that declares that contract**; newer hardened releases
may consider it as a rollback target. A supported rollback still requires
a separately verified published immutable target, release digest and installer
contract, plus compatibility with the particular installed credentials and
persistent state. Publication or a marker alone does **not** make rollback
universally safe, and older pre-contract releases remain ineligible.

For a supported published rollback target, select its exact Stable tag through
the protected management updater using `AI_SERVER_AGENT_UPDATE_CHANNEL=stable`
and `AI_SERVER_AGENT_VERSION`. `update --plan` is an offline planning aid and
does not establish target eligibility; actual execution validates the asset.
Never rerun a historical installer or the fresh-install bootstrap over a live
installation, and do not substitute source-mode checkout selection for a
supported immutable Stable rollback.

**Credential compatibility is a separate requirement.** If an otherwise
eligible older Stable refuses a store containing `direct/secondary`, first
verify the existing `direct/default` bearer, and only when explicitly necessary
run:

```bash
sudo ai-server-agent-manage credential-prepare-rollback
```

This is an interactive, confirmed **destructive credential operation**.
It deletes optional principal records, including revoked `direct/secondary`
and retired Gateway entries, while keeping the active `direct/default`
verifier; it restarts and health-checks the current Agent with recovery on
failure. Other named clients immediately lose access and need new credentials
if later reintroduced. **Do not run it to try to make `v0.1.14` or another
unsupported legacy release eligible**: removing credentials cannot restore
missing privileged lifecycle trust.

The updater still refuses a target that would reactivate an enabled retired
Gateway bearer or whose incompatible secondary records could prevent service
startup. No credential cleanup silently occurs during an update.

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

**Recoverable synchronous command output (new compatible source only):** `run_command`/`run_root_command` return a 1 MiB head/tail preview, but preserve the **complete** byte stream when that preview or the text-only MCP response would otherwise lose content. A nonempty `job_id` identifies a root-private retained transcript readable through existing `job_output(job_id, offset=0, limit=4096)` and continued with returned `next_offset` until `eof`; binary windows remain Base64. `sync_output_spool_mib` defaults to **64 MiB** (operator range 8..1024 MiB), with explicit errors rather than silent loss on capacity/disk failures. Artifacts share the existing bounded completed-Job retention; no indefinitely durable transcript is promised. The independent `start_job` 8 MiB ring may discard old bytes with explicit `retention_truncated`: use it for unbounded/long-running commands and request appropriate follow-up windows.

### Execution concurrency (optional)

The console's **Runtime limits and execution capacity** section offers independent controls for synchronous unprivileged commands, persistent tasks, persistent services, shared LSP/DAP/Terminal/Browser sessions, and bounded concurrent Browser profiles. Safe defaults remain CPU-aware worker commands (`worker_command_slots=0`: `min(GOMAXPROCS, 64)`), six active tasks (`persistent_task_slots=6`) and three active services (`persistent_service_slots=3`). An operator can configure each independently up to **64** without rebuilding the Agent:

```bash
sudo ai-server-agent-manage runtime-show
sudo ai-server-agent-manage runtime-set worker_command_slots 8
sudo ai-server-agent-manage runtime-set persistent_task_slots 64
sudo ai-server-agent-manage runtime-set persistent_service_slots 16
sudo ai-server-agent-manage runtime-set stdio_session_slots 16
# Only if the installed Agent reports Browser concurrency support:
sudo ai-server-agent-manage runtime-set browser_concurrent_slots 2
```

All five capacity keys are validated (`worker_command_slots`: 0 for auto or 1–64, persistent tasks/services/shared stdio sessions and Browser concurrent slots: 1–64). They do not change the fixed one-at-a-time **privileged root command** limit. Invalid values are rejected before changes; accepted changes use the existing privileged lifecycle lock, validated Agent/Executor restart, health verification and rollback. Use `runtime-reset <key>` to restore the key's default.

Lowering **either** persistent job limit does **not** stop existing tasks/services: `runtime_list` reports their real active counts, and new admission for that job kind remains blocked until it falls below the new limit. A different job kind retains its own capacity. The trusted active inventory, bounded retained-claim namespace, and log/audit disk reservations account for all running jobs across managed restarts and downscaling. The authenticated `runtime_list` reports actual executor worker/root command slots and current admissions (`worker_limit`, `workers_active`, `root_limit`, `root_active`), as well as task, service and shared-session limits; this is more accurate than inferring capacity from host CPU in an operator shell. It also reports slots occupied by older services after a downscale. At the 64+64 maximum, the required safety reserve reaches approximately 1.6 GiB; without adequate free disk new job admission fails with a typed `resource_limit` rather than risking log/audit integrity. After a `stdio_session_slots` downscale, older durable Terminal scopes retain their protected reservations. Reconnect with the original authenticated `session_id`/owner/workspace/authority and then close with the new `session_epoch`; previously authorized recovery is not a new admission. New LSP/DAP/Browser/Terminal starts remain blocked while aggregate occupancy exceeds the current configured limit. Untrusted registry entries and unverifiable scope cleanup never free capacity. After installing a release containing Browser concurrency support, isolated Browser admission separately obeys `browser_concurrent_slots` (default 2, 1..64) and the single exclusive shared profile; maintenance requires zero active Browser reservations. Session lifetime is separate from configured slot count: legacy `v0.1.19` Terminal scopes retain their fixed one-hour systemd lifetime, while the subsequent reviewed Issue #197 Terminal v2 implementation uses authenticated one-hour idle-lease renewal through an independent PID1 timer and a finite 24-hour hard fallback. This behavior is available only when the installed immutable Stable release contains that implementation; merging source or publishing a new release does **not** automatically upgrade an occupied Agent. Verify the installed build and report an unverified renewal as failure rather than success.

Sixty-four slots is a **per-kind admission ceiling**, not a CPU/RAM/FD/systemd guarantee or a scheduler. High settings permit 64 persistent tasks **and** 64 persistent services plus separately configured worker commands; provision CPU, memory, processes, file descriptors and disk accordingly, and raise gradually based on observed usage. On small hosts, automatic Worker capacity equals effective CPUs (maximum 64), avoiding default oversubscription for CPU-heavy builds; the previous fixed four-slot auto cap on larger hosts is removed. Measured I/O-heavy workloads can benefit from an explicit larger override (1..64), but more active CPU-bound workers can markedly reduce throughput. An unused command slot allocates no child process. Existing protocol payload, root-action approval, timeout, log-size and trust boundaries are unchanged. Use only settings supported by the **installed** Agent binary: the Worker/Task/Service capacity settings shipped in v0.1.20, while Browser concurrency needs a later release that includes that feature. Do not infer support from current `main` or an unpublished candidate. Because management restarts Agent/Executor, change settings from an independent operator SSH terminal, not the MCP connection being restarted.

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
6. requires the authenticated installer to declare the reviewed installed privileged lifecycle/startup trust contract; older releases without it are refused even if credentials are compatible;
7. performs the existing credential/config checks before invoking the eligible installer;
8. lets the release-scoped installer verify the release archive against `SHA256SUMS`.

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

Purge removes Agent-owned server config/state/log/runtime and the `aiagent` and `aibrowser` identities.

Purge intentionally preserves:

- `/srv/ai-workspace`
- the `aiworker` user and group (the Agent-owned `aibrowser` identity is removed on purge)

Purge does **not** silently delete Cloudflare resources. If recorded Cloudflare resources should be removed, run `cloudflare-cleanup` through the supported management path first.

## Security model

Key boundaries:

- public MCP access requires bearer authentication;
- MCP bearer credentials are independent named direct-client principals (`direct/default`, `direct/secondary` and operator-issued additional clients) with independently revocable/rotatable verifier records; bearer plaintext is not retained after issuance/migration;
- direct public mode requires native TLS;
- ordinary commands run as `aiworker`; isolated managed Browser runs with `aibrowser` and private 0700 HOME/scratch, while default shared Browser and `browser_run` preserve worker ownership; explicit `chromium_sandbox=true` for isolated managed Browser additionally requests start-verified Chromium renderer seccomp-BPF/user+PID namespaces, without changing the common `aibrowser` UID or globally enabling this on all hosts;
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

**Optional Chromium sandbox readiness (Issue #203; published since Stable v0.1.21, selected installed Debian 13 AMD64 acceptance passed):** This opt-in is available only for `browser_session_open(profile_mode=isolated, chromium_sandbox=true)`. Shared `browser_session_open`, `browser_run` and `browser_e2e` keep their old behavior. Chromium requires functional unprivileged Linux user namespaces and seccomp-BPF; check `kernel.unprivileged_userns_clone`, `user.max_user_namespaces`, kernel namespace availability and the active AppArmor profile. In particular, Ubuntu 24.04+ can restrict unprivileged user namespaces through AppArmor; allowed package paths and locally installed policy can vary, so supported OS/version does **not** imply this optional mode works. A real renderer must have `Seccomp: 2`, `NoNewPrivs: 1`, and separate user/PID namespace in the original Browser cgroup under the dedicated `aibrowser` UID. The Executor returns only a startup proof (`verified_at_start`), never a continuous security guarantee. `browser_sandbox_unavailable` is a launch/prerequisite failure; `browser_sandbox_unverified` means the kernel/process proof was not established. Both refuse opening the requested sandboxed session and attempt whole-cgroup cleanup; if cleanup is uncertain, preserve and explicitly close its owner-bound `session_id` before retry. Do **not** solve readiness failures by disabling the sandbox, changing global sysctls/AppArmor or running Chromium as root. All isolated Browser sessions currently share one OS UID, so they are not per-principal OS sandboxes. Document host/kernel policy evidence before enabling this mode operationally. The authenticated Dev/Test Debian 13 AMD64 acceptance in [#203](https://github.com/ach1992/ai-server-agent/issues/203#issuecomment-6351256065) does not establish native ARM64 kernel support or universal host compatibility.

See [docs/ARCHITECTURE.md](ARCHITECTURE.md) for details.

## Release model

Stable release publication is manual and exact-SHA based. The release workflow requires successful `main` CI and High Assurance Security for the exact release SHA and promotes the already-built CI release artifact rather than rebuilding it.

Immediately before publication, the operator must separately verify repository controls that the workflow credential cannot authoritatively prove, including release immutability and release-tag protection/no-bypass conditions. Automation must not treat an omitted Rulesets `bypass_actors` field as proof that no bypass exists.

The release workflow then creates the previously absent tag at the exact validated SHA, publishes with `--verify-tag`, and performs post-publication immutable-release/tag/attestation checks. Once that release is immutable, its tag is the durable identity of the verified release artifacts; the permanent public bootstrap source remains the separately reviewed `main/scripts/install-stable.sh` path.

Documentation-only commits do not imply a release. The release workflow is manually dispatched with an explicit tag and exact validated `main` SHA.

Preparing the **next** Stable is a separate, reversible source change: choose an unused semantic tag, align `.github/workflows/ci.yml`'s `RELEASE_VERSION` with that proposed tag, and review the source-to-previous-Stable difference plus actual user/operator documentation. The fixed public bootstrap must continue installing the **latest already published** immutable release until publication; do not point it at the candidate or create a tag/draft Release during preparation. Verify the exact candidate PR's required CI/Security gates and independent review, then obtain separate Owner approval to merge. Only the ensuing exact `main` SHA with successful main-push CI/Security and an intact CI-built archive is eligible for the separate, Owner-authorized manual release workflow (see [Testing](TESTING.md#9-candidate-review-and-release-validation)). A published artifact is not evidence that occupied Agent/Executor services were upgraded or that an AI client has native file/image rendering.

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

For current release and installed-host evidence, consult [Issue #52](https://github.com/ach1992/ai-server-agent/issues/52) and the exact immutable [GitHub release](https://github.com/ach1992/ai-server-agent/releases); client-native file/media acceptance belongs to [Issue #99](https://github.com/ach1992/ai-server-agent/issues/99). Closed [Issue #59](https://github.com/ach1992/ai-server-agent/issues/59) records historical bounded workflow acceptance and the Owner's decision to stop the exhaustive test program, not a pending gate. Earlier release-specific evidence belongs to those GitHub records, not this operator guide.


## Managed test guests and service credentials (Issue #214; Browser acceptance #65)

**Scope:** These tools appear in compatible source/new builds; their presence does **not** prove that Incus, KVM or Playwright is installed on an occupied Agent. Run `agent_environment` first and identify the correct host `instance_id`. Use `managed_environment(operation="status")` to check optional local Incus without altering the host. Installation/initialization of Incus and bridge/storage setup are separate authorized host-administration decisions; the Agent never installs or initializes them on demand. `managed_environment(operation="list")` with no project discovers all visible local instances. A project-bound `inspect` returns native `volatile.uuid`, status, managed tag, resource limits and discovered guest addresses.

To create a disposable guest, choose a real existing Incus project/image and name, a stable work identifier (e.g. GitHub issue), a stable `operation_id` reused on uncertain retries, `type="container"` or `type="virtual-machine"`, and explicit CPU/memory/disk limits **plus the selected native `storage_pool`**. The Agent checks the selected pool's actual reported free bytes before launch, not free space on an unrelated Agent filesystem. A timeout, interrupted launch or temporary disappearance is `create_outcome_uncertain`; do not change `operation_id`, name or project and blindly resubmit, and do not delete a possibly still-running native operation to force progress. Native state and the root-private durable intent must be reconciled first. **If the failed submission is permanently unresolved:** inspect the original `pending_creates` metadata, native all-project guest list and `incus operation list --all-projects`; then, only with explicit Owner approval scoped to this project/instance/work/operation, call `managed_environment(operation="abandon_create",project,instance,work_id,operation_id,approval=true)`. The Agent rechecks both native APIs twice under the same local lifecycle lock and refuses abandonment on any native guest identity or outstanding background operation, unavailable/malformed native list, mismatched intent or missing approval. The all-project Incus inventory must be a non-null JSON array; **every** row must have a valid explicit native project and instance name. A row missing `project` is not assigned the legacy `default` fallback for a destructive/retirement absence decision. It cannot prove an external administrator will never create a guest later; do not confuse the absence proof with cross-provider atomicity. A successful abandon retains the **old operation ID as a retired tombstone**, releases the name and requires a **new** stable operation ID for the next attempt. This does not cancel any native Incus operations. The resulting identity must be verified in the native list/inspect result **and is durably bound as `native_uuid` in the confirmed local intent**. Copying its Agent ownership tags to a replacement with another native UUID does not grant the replacement the original create/lifecycle identity; confirmed project/name ownership is checked **independently** of current guest work/operation tags, and a changed/missing operation tag or changed work tag is rejected even if the guest retains the original UUID. Explicitly register new service credential references after a separately authorized replacement. Reuse the exact project/instance/UUID for `start`, `stop`, `exec`, `snapshot`, `snapshots`, `restore` and `delete`. `restore` and `delete` require fresh scope-specific owner approval. Mutations are serialized and re-inspected before issue, but separate privileged Incus actors can still cause a **non-atomic external name-replacement race**; do not claim conditional-by-UUID deletion. Keep unusual identity transitions and destructive outcomes marked uncertain for manual native reconciliation. Guest execution is native argv, without implicit shell interpretation; do not assume the VM guest agent is installed/ready. A snapshot's presence is not proof that restoring its filesystem has been tested.

### Native nonblocking snapshot and restore (Issue #221 — source candidate)

On supported Incus 6.0+ local UNIX-socket hosts, `managed_environment(operation="snapshot", project, instance, uuid, snapshot, operation_id)` and `operation="restore"` submit asynchronously through Incus's REST API. Use a stable, unique `operation_id` for each intended snapshot/restore, not a new ID on a timeout or chat reconnection. Restore still requires `approval=true` for the exact guest/snapshot and a stopped native guest. Before a destructive restore PUT, the Agent GETs the current native instance, verifies its original UUID/Agent ownership, preserves all writable configuration and uses native `ETag` / `If-Match` to reject concurrent configuration updates. The small root-private `managed-incus/native-operations.json` record (including non-secret authenticated-principal provenance) is fsynced **before** the native mutation. Results contain `native_operation` with the stable submission identity and, on acknowledgement, the native Incus operation UUID; `status="running"` means **accepted**, not completed. Identical `operation_id` replays return the stored state without a second mutation; a changed identity with the same ID fails. Until a native operation reaches a confirmed terminal state, other Agent mutations of that exact guest are refused.

First use `managed_environment(operation="operation_list", project)` to rediscover non-secret journal identities in a new conversation without relying on a previous chat, then use `managed_environment(operation="operation_status", operation_id, project, instance, uuid)` to inspect or reconcile after a client disconnect, Agent/Executor restart or a new authorized chat. The existing Executor periodically persists observed terminal outcomes while running and automatically resumes native-operation monitoring on Executor restart when a protected pending journal exists; fresh-chat operation_list/status provide additional manual reconciliation. There is no new daemon or always-on poller when no pending work exists. For backpressure, recovery admissions use a bounded FIFO queue with at most 32 concurrent observer goroutines and fair short observation turns, including durable records with a lost native acknowledgement; an observer timeout never changes an unproven operation into success or resubmits it. Incus 6.0.4 returns operation-list records grouped under `metadata.running`/other states, and its `project` field may be absent: lost-ack reattachment requires an unambiguous native operation carrying the exact instance and snapshot resource paths (including project) and the correct action, never a matching description or snapshot name alone. **Important native limitation (verified against real Incus 6.0.4):** a snapshot operation exposes both instance and snapshot resource paths, while a *restore* operation exposes only the instance path, omitting the snapshot name. If a restore HTTP 202 acknowledgement is lost and no independently binding evidence survives, it intentionally remains `outcome_uncertain` despite possible successful filesystem rollback. Inspect the exact native guest and restored data and follow approval-gated `resolve_uncertain` only after native operations are absent; do not claim automatically recoverable restore success from a mere resource/name match. `native_succeeded` means the exact Incus operation reported success and the same guest identity was revalidated; the caller **must still verify restored filesystem/application content**. A native operation that expires before the Agent observes authoritative completion stays `outcome_uncertain` even if a snapshot name is present. Failed native operations retain `native_failed`; transport errors do not imply safe retry. An uncertain operation blocks subsequent guest mutations until it is reconciled or explicitly retired. If no safe native terminal evidence can be recovered, `managed_environment(operation="resolve_uncertain", operation_id, project, instance, uuid, approval=true)` only after separate Owner approval and manual examination: it double-checks the **all-project** native operation list is complete and empty, and that the original managed guest/UUID still matches; it records `closed_unverified`, **not success**. Do not use `resolve_uncertain` while another project has native operations in progress, and do not reuse a closed operation ID. Native Incus, not this journal, is authoritative for data; no full VM disk backups or periodic daemon are introduced by the feature.

**Credential references are general Agent/Executor infrastructure, not Browser-only.** A record may optionally describe an HTTP(S), SSH, database or other network-service URI with a password, API key, textual private key, static code or another short lowercase kind label. For an offline license or host-local app secret, omit `endpoint` rather than inventing an HTTP URL. Registering non-secret metadata never authorizes an operation or returns credential material. Each consumer checks its own protocol, destination, principal and resource identity; Browser protected login accepts HTTP(S) only.

**For AI-generated credentials:** use `service_access(operation="register", project, guest, guest_uuid, service, endpoint, username)` first (omit guest fields for authorized external services). Then use `service_access(operation="generate", ref, kind="password"|"token", ttl_days=30)`. The root Executor generates and stores a random 256-bit value without returning it; the resulting `svc_...` ref survives eligible updates/restarts, subject to expiry/revocation and Agent instance identity. For a guest-bound credential, `managed_environment(operation="exec", credential_ref, project, instance, uuid, command=[...])` can deliver the value privately via stdin to an authorized provisioning command, suppressing guest output. Registration/generation by itself **does not configure the target**; verify actual account/service behavior separately. Never generate a fake third-party API key, OTP or keypair with the random-password generator.

**For a credential already issued by a service or held by an operator:** one root-local command can register an independent service and save its credential **atomically**, with no secret in chat, shell arguments, environment or tool output:

```bash
sudo ai-server-agent -config /etc/ai-server-agent/config.json service-access add --service "Example API" --endpoint "https://api.example.org/" --kind api_key --ttl-days 30
```

The command prompts once through `/dev/tty` with echo disabled and prints only a newly created non-secret `svc_...` reference. For a local activation code, omit the optional URI and use e.g. `service-access add --service "Local license" --kind access_code --ttl-days 7`. Existing registrations can be privately updated using `service-access enroll --ref ...`; do not duplicate them. For **multiline textual keys**, provide an existing root-owned 0400/0600 file in a trusted private root directory; contents (including final newlines) are preserved exactly:

```bash
sudo ai-server-agent -config /etc/ai-server-agent/config.json service-access add --service "Git remote" --endpoint "ssh://git.example.org:22" --kind private_key --secret-file /root/private/git-service.key --never
```

`--secret-file` rejects symlinks, hardlinks and untrusted paths. The operator manages the source file after verifying enrollment; no automatic deletion. Only UTF-8 textual material up to 4096 bytes is supported in this version; binary credentials require a typed import format. Use a real root terminal for existing secrets, not AI root-shell arguments. Short-lived MFA/OTP challenges are typically interactive steps and must not be treated as reusable keys. `kind` is descriptive metadata, not validation that a credential was issued by the target.

In later chats, call `service_access(operation="list"|"inspect")` to recover nonsecret target identity and reference, then the **appropriate supported, authorized consumer**. Credential values are never returned by MCP. Revocation, expiry, renew, cross-origin-change approval and service identity safeguards still apply.

**Private direct consumers (`service_use`, new compatible source only):** with a ready registered network target and `svc_...` reference, call `service_use(credential_ref="svc_...", method="GET", path="/api/status", auth_mode="bearer")` for HTTP(S), `service_use(credential_ref="svc_...", command="hostname")` for SSH, or `service_use(credential_ref="svc_...", query="SELECT 1")` for PostgreSQL/MySQL/MariaDB. Do not paste the credential value. HTTP uses the enrolled origin only; caller-selected path cannot change the destination host and redirects are reported rather than followed. `auth_mode` may be `bearer`, `basic`, or `header` (`auth_header` defaults to `X-API-Key`), with a useful default inferred from the descriptive credential `kind`, not a provider-specific allowlist. SSH uses native `/usr/bin/ssh` and an ephemeral private-key file or optional `/usr/bin/sshpass` for password/token; host-key verification remains strict. SQL uses optional native `/usr/bin/psql` or `/usr/bin/mysql`, sending SQL on stdin and credentials via protected process environment or a private, short-lived client option file. Missing native executables fail clearly. The adapter uses the registered host/port/database/user and verifies any guest-bound native UUID before use; it cannot use an offline-only credential reference without an endpoint. Returned/stored output scrubs **exact plaintext credential occurrences**, including across output chunks; a remote service may deliberately re-encode or transform a secret, so avoid requesting secret echo/debug dumps. Use an authorized disposable service for destructive SQL/API/SSH actions and separately verify the target accepted authentication; source fixtures alone are not installed acceptance.

```bash
sudo ai-server-agent -config /etc/ai-server-agent/config.json service-access enroll --ref svc_<actual-id> --kind password --username <user> --ttl-days 30
```

The existing `enroll` form prompts through `/dev/tty` with echo disabled and outputs **metadata only**. For app/guest provisioning, use a typed consumer and verify that the target actually accepted the credential. No no-TTY automation fallback accepts plaintext arguments. For provisioning, `managed_environment(operation="exec", credential_ref, project, instance, uuid, command=[...])` passes the protected credential as a single stdin line and suppresses guest stdout/stderr to avoid disclosure; select a guest provisioning command that explicitly reads the line and handles it safely. `service_access(operation="update", ref, endpoint, username)` changes metadata while retaining the secret and immutable native guest binding; a different destination origin requires explicit Owner approval. `renew`, `revoke`, `delete` and `cleanup` implement the lifecycle. Expired entries deny use even when no cleanup job has run. Renew before expiry or explicitly re-enroll after a wiped expired value.

For a new authenticated Browser test, create a **fresh isolated** `browser_session_open`, navigate to the registered service URL using `browser_session_flow`, inspect CSS selectors, then call `browser_session_credential_login` with the stored `credential_ref`, username/secret/submit selectors and an element or URL that changes **only after a successful login**. This fetches the credential inside the root Executor; no secret passes through the chat. Provide optional `login_error_selector` to identify an observed login rejection and `challenge_selector` for MFA/CAPTCHA; absent explicit evidence, failed login remains `credential_login_not_confirmed`. The protected login checks non-secret service reachability first; transport failure and HTTP 5xx have separate actionable codes. A failure or MFA challenge is not a successful login and cannot be bypassed with stored cookies. After successful protected login, use `browser_session_flow`, `browser_session_script`, screenshots, image continuation, snapshots, console/network evidence and traces **in that same isolated authenticated session**, subject to ordinary size, resource and principal gates. The registered **credential-entry page and form action** must remain on the enrolled origin; a `success_url` may explicitly name a distinct post-auth SSO/dashboard origin, verified by exact result origin and URL prefix after submission. A rendered third-party page or custom script may expose user-visible application data, including any credential the application itself echoes: keep evidence in authorized channels, never mistake a sanitized Agent transport for a claim that external page content is secret-free. Close an unverified or expired session and start fresh after correcting the cause. A new chat rediscovers host+guest+service; it must open its own Browser session.

**Acceptance status is evidence-specific.** Source/unit/CLI-fixture tests do not establish a real Linux VM with aaPanel/WordPress, an installed optional Incus daemon, or UI login from a fresh chat. Record those actual results in Issue #65 before claiming end-to-end acceptance.
