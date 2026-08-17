# AI Server Agent

AI Server Agent is a small self-hosted MCP control plane for a **dedicated Ubuntu 22.04 LTS amd64 development/test server**. It gives ChatGPT a controlled way to inspect the host, run normal shell commands as an unprivileged worker, perform guarded privileged operations, run persistent jobs, access files, and optionally automate a browser.

> **Security scope:** AI Server Agent is root-capable infrastructure. Use it on a dedicated development/test server, keep snapshots/backups, and treat its bearer credential as a privileged secret. v0.1 is not a production-support claim.

## The simple path

The intended experience is:

1. Run one pinned stable installer command.
2. Answer a short setup wizard.
3. Let the installer configure and verify the server.
4. When it says **Server setup complete**, go to ChatGPT and create the MCP app.

You do not need to manually generate TLS keys/CSRs, hand-edit Agent JSON, remember update commands, or rebuild the setup when the domain/certificate changes.

## Current stable release

**v0.1.1**

Supported stable scope:

- Ubuntu 22.04 LTS
- amd64 / x86-64
- systemd
- dedicated development/test servers

Ubuntu 24.04, Debian, arm64, aaPanel, and production workloads are intentionally outside the stable v0.1 support claim until they receive release-grade validation.

## Install

Run the stable installer from the matching release tag:

```bash
VERSION='v0.1.1'
curl -fsSLG \
  -H 'Accept: application/vnd.github.raw+json' \
  --data-urlencode "ref=$VERSION" \
  'https://api.github.com/repos/ach1992/ai-server-agent/contents/install.sh' | \
  sudo env AI_SERVER_AGENT_VERSION="$VERSION" AI_SERVER_AGENT_REF="$VERSION" bash
```

The release archive is verified against `SHA256SUMS` before installation.

### First-run wizard

For a new interactive install you will see a short choice:

```text
1) Cloudflare domain - guided HTTPS/domain setup
2) Local/private - loopback-only for Secure MCP Tunnel
3) Existing certificate - advanced/manual TLS
```

For the normal direct HTTPS setup, choose **Cloudflare domain**.

The wizard asks for:

- the public MCP hostname, for example `mcp.example.com`;
- a Cloudflare API token, entered with hidden input and not stored by the Agent;
- the server public IPv4 only if automatic detection cannot determine it.

The guided Cloudflare path then:

- finds the matching Cloudflare zone;
- checks the zone SSL mode;
- generates a fresh host-local private key and CSR;
- requests a Cloudflare Origin CA certificate for that CSR;
- verifies certificate, key, and hostname consistency;
- creates or safely reconciles the proxied DNS record;
- creates or safely reconciles a hostname-scoped Origin Rule that sends Cloudflare traffic to the Agent's high port;
- enables native TLS/public mode on the Agent;
- verifies local health, public HTTPS health, unauthenticated MCP rejection, and authenticated MCP initialize;
- rolls the Agent configuration back if the public path cannot be verified.

The Cloudflare token should be scoped to the target zone and be able to list/read the zone, manage the hostname's DNS record, manage Origin Rules, read the zone SSL mode, and create/revoke Origin CA certificates. If the zone is not already **Full (strict)**, the wizard explains the impact and asks before attempting a zone-wide SSL mode change. It never silently changes that setting.

If you do not use Cloudflare, choose **Existing certificate** and provide an already-issued certificate/key. The Agent still validates them before switching to public mode.

## After install: one management command

Use:

```bash
sudo ai-server-agent-manage
```

The menu is intentionally small:

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

You can rerun the menu whenever the domain, TLS certificate, connection mode, or Agent version needs to change.

## Change the domain later

Run:

```bash
sudo ai-server-agent-manage
```

Choose **Configure or change domain / connection** and then **Cloudflare domain**. Enter the new hostname and a fresh scoped Cloudflare token when prompted.

The manager prepares and verifies the new hostname first. Only after the new public MCP path works does it offer to remove the old Agent-managed DNS rule/certificate resources. It identifies those resources by the IDs it recorded when it created them; it does not blindly replace unrelated Cloudflare state.

## Rotate the TLS certificate later

Run the management menu and choose:

```text
Rotate / renew Cloudflare TLS certificate
```

A new private key, CSR, and Origin CA certificate are generated. The private key is never printed. The new path is verified before the previous certificate is offered for revocation.

For manually managed certificates, choose **Configure or change domain / connection -> Existing certificate** and provide the new PEM paths.

## ChatGPT setup

Choose **ChatGPT setup** from the management menu after the public verification succeeds. It shows the MCP URL and the current final steps for ChatGPT Business without printing the bearer credential by default.

Typical MCP URL:

```text
https://mcp.example.com/mcp
```

Current ChatGPT Business flow on web is generally:

1. Enable Developer mode if your workspace requires it.
2. Open **Workspace Settings -> Apps -> Create**.
3. Enter the MCP URL shown by `ai-server-agent-manage`.
4. Select the authentication option available for the workspace and provide the protected bearer Authorization value only when required.
5. Scan tools and create the app.
6. Test `agent_environment`, then a safe `run_command`.

The protected Authorization header is stored at:

```text
/etc/ai-server-agent/mcp.authorization
```

The manager can reveal it only after an explicit confirmation. Do not paste it into normal chat messages, logs, screenshots, tickets, or source control.

## Status and repair

From the menu, **Status / health** reports:

- installed version and channel (`stable` or `source`);
- Agent/executor service state;
- local health;
- current bind mode and port;
- current managed domain/provider;
- MCP endpoint.

**Repair / restart** first restarts and verifies the current services. If local health is still broken it falls back to the channel-aware reinstall/update path.

## Updating

Use the menu and choose **Update Agent**, or run:

```bash
sudo ai-server-agent-manage update
```

Update behavior is channel-aware:

- a **stable** install updates only through a concrete stable GitHub Release tag and passes both the version and matching ref to the installer;
- a **source** install remains a source install and resolves its configured source ref to an immutable commit before installation.

A stable install does **not** silently drift to `main`.

For automation you may pin a stable update explicitly:

```bash
sudo env AI_SERVER_AGENT_VERSION='v0.1.1' ai-server-agent-manage update
```

## Noninteractive / automation inputs

Interactive setup is the normal user path. CI or automation can use environment variables instead:

- `AI_SERVER_AGENT_NONINTERACTIVE=1`
- `AI_SERVER_AGENT_SETUP_MODE=local|cloudflare|manual|keep`
- `AI_SERVER_AGENT_PORT=3210`
- `AI_SERVER_AGENT_HOSTNAME=mcp.example.com`
- `AI_SERVER_AGENT_PUBLIC_IPV4=203.0.113.10`
- `AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE=/secure/path/token`
- `AI_SERVER_AGENT_TLS_CERT_FILE=/absolute/path/cert.pem`
- `AI_SERVER_AGENT_TLS_KEY_FILE=/absolute/path/key.pem`

Prefer `AI_SERVER_AGENT_CLOUDFLARE_API_TOKEN_FILE` over putting an API token directly in an environment variable. The token file should be root-readable only and is not copied into Agent state.

## Installed components

```text
/usr/local/bin/ai-server-agent
/usr/local/sbin/ai-server-agent-manage
/usr/local/lib/ai-server-agent/
/etc/ai-server-agent/
/var/lib/ai-server-agent/
/var/log/ai-server-agent/
/srv/ai-workspace/
```

System services:

```text
ai-server-agent.service
ai-server-agent-executor.service
```

The network-facing Agent runs as the unprivileged `aiagent` identity. Root operations are delegated to the separate executor over a local Unix socket. Normal project shell work runs as `aiworker` under `/srv/ai-workspace`.

## MCP authentication

Bearer authentication is mandatory in both local and public modes. Loopback is not treated as an authentication boundary: untrusted local project/browser code must not be able to call root-capable MCP tools directly.

The bearer token itself is kept under `/etc/ai-server-agent/` with restrictive permissions and is not printed during install, update, health checks, or Cloudflare setup.

## Public networking

AI Server Agent does not reserve ports 80/443 and does not install nginx, Apache, a database, Docker, PHP, Node.js, Python, or a hosting panel as part of the MCP core.

The guided Cloudflare setup creates a proxied hostname and a hostname-scoped Origin Rule so standard public HTTPS can reach the Agent's dedicated high origin port while the Agent keeps native TLS and bearer authentication end-to-end.

## MCP tools

The Agent exposes a deliberately small tool surface:

- `agent_environment` — environment and preservation information.
- `run_command` — shell command execution as `aiworker`.
- `run_root_command` — privileged shell execution with approval controls.
- `start_job`, `job_status`, `job_output`, `job_stop` — persistent background jobs.
- `read_file`, `write_file` — host filesystem operations.
- `browser_setup` — installs the optional isolated browser runtime.
- `browser_run` — Playwright browser automation.

The exact tools are discoverable through MCP.

## Workspace and preservation

Normal project work lives under:

```text
/srv/ai-workspace
```

The Agent writes its machine-readable self-preservation manifest to:

```text
/var/lib/ai-server-agent/AI_ENVIRONMENT.json
```

The manifest tells automation which services, paths, and connection primitives must be preserved during host-wide work.

## Safe uninstall and purge

From the management menu:

- **Safe uninstall** removes Agent services, binary, management command, and installed helper scripts while preserving config/state/logs/users/workspace so reinstall/recovery is straightforward.
- **Purge** removes Agent-owned config/state/logs/runtime and the `aiagent` service identity as well.

Even purge intentionally preserves:

```text
/srv/ai-workspace
aiworker user/group
```

because removing the control plane must not silently delete user project data.

If the Agent created Cloudflare resources and you want those removed too, first switch away from the active Cloudflare connection and choose **Remove recorded Cloudflare resources**. That action asks for a fresh Cloudflare token because provider credentials are not retained.

## Security notes

- Keep the MCP bearer credential private.
- Never share the executor token or TLS private key.
- Use Full (strict) with Cloudflare Origin CA.
- Keep server snapshots/backups before host-wide changes.
- Treat Cloudflare/API credentials as temporary setup credentials; do not persist them in Agent config.
- The manager refuses ambiguous DNS replacement and asks before connectivity-sensitive or destructive actions.
- Approval guardrails are not a complete sandbox; privileged shell access is an intentional capability.
- Keep v0.1 on a dedicated development/test server.

## License

AI Server Agent is released under the [MIT License](LICENSE).
