# AI Server Agent

AI Server Agent is a small self-hosted MCP control plane for a **dedicated Ubuntu 22.04 LTS amd64 development/test server**. It gives ChatGPT a controlled way to inspect the host, run normal commands as an unprivileged worker, perform guarded privileged operations, run persistent jobs, access files, and optionally automate a browser.

> **Security scope:** AI Server Agent is root-capable infrastructure. Use it on a dedicated development/test server, keep snapshots/backups, and treat its bearer credential as a privileged secret. The v0.1 line is not a production-support claim.

## Supported stable scope

The v0.1 stable release supports:

- Ubuntu 22.04 LTS
- amd64 / x86-64
- systemd
- dedicated development/test servers

Ubuntu 24.04, Debian, arm64, hosting panels, and production workloads are outside the current stable support claim until they receive release-grade validation.

## Install the latest stable release

Run:

```bash
curl -fsSL https://github.com/ach1992/ai-server-agent/releases/latest/download/install.sh | sudo bash
```

GitHub's `releases/latest` path resolves a published full release rather than a draft or prerelease. The `install.sh` asset is generated for one exact release tag and pins both the version and ref to that tag. It never falls back to mutable `main`.

The pinned installer then downloads the matching Linux amd64 release archive and `SHA256SUMS` and verifies the archive with `sha256sum` before extracting or installing it.

### Install an exact version

Use the release-specific asset URL:

```bash
curl -fsSL https://github.com/ach1992/ai-server-agent/releases/download/v0.1.2/install.sh | sudo bash
```

This installs `v0.1.2` specifically. Change the version in the URL only when you intentionally want another published stable release.

If you prefer to inspect the installer before executing it, download that same release asset to a local file, review it, and run it with `sudo bash`.

## What the installer changes

The stable installer:

- checks for Ubuntu 22.04 LTS, amd64, and systemd;
- installs the small setup dependency set `ca-certificates`, `curl`, `jq`, `openssl`, `tar`, and `xz-utils`;
- creates the service identity `aiagent` when needed;
- creates the worker identity `aiworker` and `/srv/ai-workspace` when needed;
- installs the Agent binary, management command, updater, and uninstaller;
- creates protected config/state/token files under `/etc/ai-server-agent` and `/var/lib/ai-server-agent`;
- installs and starts `ai-server-agent.service` and `ai-server-agent-executor.service`;
- verifies both services and local health before starting the optional connection wizard.

It does **not** install or reserve nginx, Apache, PHP, MySQL, Docker, Node.js, Python, aaPanel, or ports 80/443 as part of the MCP core.

Main installed paths:

```text
/usr/local/bin/ai-server-agent
/usr/local/sbin/ai-server-agent-manage
/usr/local/lib/ai-server-agent/
/etc/ai-server-agent/
/var/lib/ai-server-agent/
/var/log/ai-server-agent/
/srv/ai-workspace/
```

## First-run wizard

After the core is installed and local health passes, a new interactive installation offers:

```text
1) Cloudflare domain - guided HTTPS/domain setup
2) Local/private - loopback-only for Secure MCP Tunnel
3) Existing certificate - advanced/manual TLS
0) Configure later - keep the healthy local Agent
```

Choosing `0`, cancelling, or encountering a connection-setup error does not turn a successful core installation into a false installation failure. The Agent remains in its last verified mode and the installer shows how to resume:

```bash
sudo ai-server-agent-manage
```

## Guided Cloudflare setup

Choose **Cloudflare domain** when the MCP hostname is proxied by Cloudflare and you want direct public HTTPS without adding a local web proxy.

The wizard asks for:

- the MCP hostname, for example `mcp.example.com`;
- a scoped Cloudflare API token;
- the server public IPv4 only if automatic detection cannot determine it.

The Cloudflare token prompt is hidden. The token is used only during that management command and is not written to Agent state.

### Create the Cloudflare API token

Create a custom API token in Cloudflare and restrict its resource scope to:

```text
Include -> Specific zone -> the zone containing the MCP hostname
```

Grant exactly these zone permissions:

```text
Zone > Zone > Read
Zone > DNS > Edit
Zone > SSL and Certificates > Edit
Zone > Origin Rules > Edit
Zone > Config Rules > Edit
```

These permissions are used for zone lookup, proxied DNS, Cloudflare Origin CA certificate issuance, the origin-port rule, and the hostname-scoped SSL Configuration Rule respectively.

**Enter the token only in the hidden terminal prompt. Do not paste it into ChatGPT, normal chat messages, tickets, logs, screenshots, or source control.**

For noninteractive automation, prefer a root-readable token file and `AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE` instead of placing the token directly in an environment variable.

### What the Cloudflare path manages

For the selected MCP hostname only, the manager:

1. finds the matching Cloudflare zone;
2. generates a fresh private key locally and creates a CSR for the MCP hostname;
3. requests and validates a Cloudflare Origin CA certificate for that CSR;
4. creates or safely reconciles a proxied `A` record;
5. creates or safely reconciles a hostname-scoped Origin Rule that routes Cloudflare to the Agent's high port;
6. creates or safely reconciles a hostname-scoped Configuration Rule in phase `http_config_settings` with SSL mode `strict`;
7. enables native TLS/public mode on the Agent;
8. verifies public HTTPS health, unauthenticated MCP rejection (`401`), and authenticated MCP `initialize`.

The manager **does not change the whole-zone SSL mode**. A zone may remain `Flexible` for unrelated hostnames while the MCP hostname alone is forced to `strict` by its Configuration Rule.

Cloudflare Origin CA private keys are generated locally and are never sent to Cloudflare or printed. Only the CSR is sent for signing.

### Ownership and existing Cloudflare resources

The manager is deliberately conservative:

- matching external DNS can be reused without taking ownership;
- conflicting or ambiguous external DNS is not silently overwritten;
- Agent rules have deterministic refs and their Cloudflare IDs are recorded in `/etc/ai-server-agent/managed.json` after successful setup;
- an unrecorded rule that collides with an Agent ref is not silently adopted or overwritten;
- cleanup deletes only DNS recorded as Agent-owned and rule/certificate IDs recorded by the Agent;
- unrelated Configuration Rules, Origin Rules, and DNS records are left alone.

If public verification fails, the previous local Agent configuration is restored and newly created Cloudflare resources are rolled back where it is safe to do so.

## Existing certificate / manual TLS

If you do not use the guided Cloudflare path, choose **Existing certificate** and provide an already-issued PEM certificate and private key. The manager validates hostname coverage and key/certificate consistency before switching the Agent to public mode.

## Management

Run this at any time:

```bash
sudo ai-server-agent-manage
```

The menu provides:

```text
1) Status / health
2) ChatGPT setup
3) Configure or change domain / connection
4) Rotate / renew Cloudflare TLS certificate
5) Update Agent
6) Repair / restart
7) Safe uninstall (preserve data)
8) Purge Agent-owned server data
9) Remove recorded Cloudflare resources
0) Exit
```

### Status

`Status / health` reports the installed version/channel, ref, Agent/executor service state, local health, bind mode, port, current setup provider/domain, and MCP endpoint.

You can also run:

```bash
sudo ai-server-agent-manage status
```

### Resume an incomplete first run

If the installer says the core is healthy but connection setup is incomplete, run:

```bash
sudo ai-server-agent-manage
```

Then choose **Configure or change domain / connection**. You do not need to reinstall the core first.

### Rotate Cloudflare TLS

Choose **Rotate / renew Cloudflare TLS certificate**. A fresh local private key, CSR, and Origin CA certificate are created and verified. The previous certificate is offered for revocation only after the new public path verifies successfully.

### Update

Use **Update Agent** or:

```bash
sudo ai-server-agent-manage update
```

A stable installation resolves the latest published stable GitHub Release and invokes the installer with the matching stable version/tag. It does not drift to `main`.

To request an exact stable update:

```bash
sudo env AI_SERVER_AGENT_VERSION='v0.1.2' ai-server-agent-manage update
```

Source-channel development installs remain source installs and resolve their configured source ref to an immutable commit before installation.

### Repair

`Repair / restart` restarts and validates the current services. If local health is still broken, it invokes the channel-aware reinstall/update path.

## ChatGPT setup

After public verification succeeds, choose **ChatGPT setup**. The manager shows the public MCP URL, for example:

```text
https://mcp.example.com/mcp
```

The protected bearer Authorization value is **not displayed by default**. The manager reveals it only after an explicit confirmation.

The protected Authorization header file is:

```text
/etc/ai-server-agent/mcp.authorization
```

In ChatGPT Business, create/configure the MCP app with the MCP URL and the workspace's available bearer/authentication option, scan the tools, then test a safe `agent_environment` call followed by a safe `run_command`.

Do not paste the Authorization header into ordinary chat messages, logs, screenshots, tickets, or source control.

## Noninteractive automation inputs

Interactive setup is the normal user path. Automation can use:

- `AI_SERVER_AGENT_NONINTERACTIVE=1`
- `AI_SERVER_AGENT_SETUP_MODE=local|cloudflare|manual|keep`
- `AI_SERVER_AGENT_PORT=3210`
- `AI_SERVER_AGENT_HOSTNAME=mcp.example.com`
- `AI_SERVER_AGENT_PUBLIC_IPV4=203.0.113.10`
- `AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE=/secure/path/token`
- `AI_SERVER_AGENT_TLS_CERT_FILE=/absolute/path/cert.pem`
- `AI_SERVER_AGENT_TLS_KEY_FILE=/absolute/path/key.pem`

The token file should be root-readable only. It is read for the Cloudflare operation and is not copied into Agent state.

## Authentication and privilege boundary

Bearer authentication is mandatory in both local and public modes. Loopback is network isolation, not an authentication boundary.

The public MCP process runs as unprivileged `aiagent`. Root-capable execution stays behind the local executor socket. Normal project shell work runs as `aiworker` under `/srv/ai-workspace`.

The MCP safety metadata and approval guardrails remain part of the privileged-operation boundary.

## Public networking

The guided Cloudflare path keeps native TLS on the Agent's dedicated high port. Cloudflare reaches that port through a hostname-scoped Origin Rule, and a separate hostname-scoped Configuration Rule forces only the MCP hostname to Full (strict) behavior.

No whole-zone SSL mutation is required for the normal path, so unrelated proxied hostnames in the same zone keep their existing SSL behavior.

## MCP tools

The Agent exposes a deliberately small tool surface:

- `agent_environment` — environment and preservation information;
- `run_command` — shell execution as `aiworker`;
- `run_root_command` — privileged shell execution with approval controls;
- `start_job`, `job_status`, `job_output`, `job_stop` — persistent jobs;
- `read_file`, `write_file` — host filesystem operations;
- `browser_setup` — optional isolated browser runtime installation;
- `browser_run` — Playwright browser automation.

The exact current tools are discoverable through MCP.

## Workspace and preservation

Normal project work lives at:

```text
/srv/ai-workspace
```

The Agent writes its machine-readable self-preservation manifest to:

```text
/var/lib/ai-server-agent/AI_ENVIRONMENT.json
```

### Safe uninstall

Safe uninstall removes the services, binary, management command, and installed helper scripts while preserving config/state/logs/users/workspace for straightforward recovery.

### Purge

Purge removes Agent-owned config/state/log/runtime and the `aiagent` service identity.

Even purge intentionally preserves:

```text
/srv/ai-workspace
aiworker user/group
```

Removing the control plane must not silently delete user project data.

Cloudflare resources are separate. If you want the Agent-recorded Cloudflare resources removed, switch away from the active Cloudflare connection first and choose **Remove recorded Cloudflare resources**. The manager asks for a fresh scoped token because provider credentials are not retained.

## Security notes

- Keep the MCP bearer credential private.
- Never share the executor token, Cloudflare token, or TLS private key.
- Keep snapshots/backups before host-wide work.
- Keep Cloudflare/API credentials temporary and least-privileged.
- Do not silently adopt, overwrite, or delete external DNS/rules.
- Public direct mode requires native TLS and bearer authentication.
- Approval guardrails are not a complete sandbox; privileged shell access is an intentional capability.
- Keep the v0.1 stable line on a dedicated development/test server.

## License

AI Server Agent is released under the [MIT License](LICENSE).
