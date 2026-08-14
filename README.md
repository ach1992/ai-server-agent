# AI Server Agent

AI Server Agent is a small self-hosted MCP control plane for a dedicated Linux server. It gives an MCP client a controlled way to inspect the host, run normal shell commands as an unprivileged worker, perform approved privileged operations, run persistent jobs, access files, and optionally automate a browser.

> **Security scope:** AI Server Agent is root-capable infrastructure. Use it on dedicated development/test servers, keep backups or snapshots, and treat its bearer credential as a privileged secret. The current stable release is not intended for production systems.

## Current stable release

**v0.1.0**

Officially supported:

- Ubuntu 22.04 LTS
- amd64 / x86-64
- systemd
- dedicated development/test servers

The stable v0.1.0 release does not claim release-grade support for Ubuntu 24.04, Debian, arm64, aaPanel, or production workloads.

## Features

- MCP endpoint with mandatory bearer authentication.
- Unprivileged command execution as `aiworker`.
- Root-capable executor isolated behind a local Unix socket.
- Explicit approval guardrails for protected or connectivity-sensitive actions.
- Persistent background jobs that survive MCP reconnects.
- Host file read/write operations through the privileged executor.
- Optional isolated Playwright browser runtime.
- Machine-readable environment and preservation manifest.
- Local loopback mode for private connectivity.
- Native TLS public mode on a dedicated high port.
- No ownership of ports 80/443 and no required nginx, Apache, Docker, database, PHP, Node.js, Python, or hosting-panel stack.

## Install

Install the current stable release directly from the GitHub Release artifact. The installer verifies the downloaded archive against `SHA256SUMS` before installing it.

```bash
VERSION='v0.1.0'
curl -fsSLG \
  -H 'Accept: application/vnd.github.raw+json' \
  --data-urlencode "ref=$VERSION" \
  'https://api.github.com/repos/ach1992/ai-server-agent/contents/install.sh' | \
  sudo env AI_SERVER_AGENT_VERSION="$VERSION" AI_SERVER_AGENT_REF="$VERSION" bash
```

The interactive installer asks for the bind mode and MCP port. For the safest default, choose:

```text
Bind mode: local
MCP port: 3210
```

After installation, the main components are:

```text
/usr/local/bin/ai-server-agent
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

## Verify the installation

```bash
systemctl is-active ai-server-agent.service
systemctl is-active ai-server-agent-executor.service
curl -fsS http://127.0.0.1:3210/healthz
```

A healthy local installation should have both services active and return a successful health response from `127.0.0.1:3210`.

## MCP endpoint and authentication

Local mode listens on:

```text
http://127.0.0.1:3210/mcp
```

Bearer authentication remains mandatory even on loopback. The installer creates the protected authorization header file:

```text
/etc/ai-server-agent/mcp.authorization
```

Configure the trusted MCP client to send that value as the HTTP `Authorization` header. Do not expose the bearer token in logs, screenshots, shell history, or source control.

## Public HTTPS mode

For direct remote MCP access, use `public` mode with a certificate and private key already present on the server. Public mode refuses to run without native TLS.

Example:

```bash
VERSION='v0.1.0'
curl -fsSLG \
  -H 'Accept: application/vnd.github.raw+json' \
  --data-urlencode "ref=$VERSION" \
  'https://api.github.com/repos/ach1992/ai-server-agent/contents/install.sh' | \
  sudo env \
    AI_SERVER_AGENT_VERSION="$VERSION" \
    AI_SERVER_AGENT_REF="$VERSION" \
    AI_SERVER_AGENT_NONINTERACTIVE='1' \
    AI_SERVER_AGENT_BIND_MODE='public' \
    AI_SERVER_AGENT_PORT='3210' \
    AI_SERVER_AGENT_TLS_CERT_FILE='/etc/ai-server-agent/tls/origin.crt' \
    AI_SERVER_AGENT_TLS_KEY_FILE='/etc/ai-server-agent/tls/origin.key' \
    bash
```

The certificate and key paths must be absolute and readable by the Agent service. Bearer authentication is still required in public mode.

AI Server Agent does not install a reverse proxy and does not reserve ports 80 or 443. An external proxy or edge service may forward HTTPS traffic to the Agent's dedicated port when needed.

## Architecture

```text
MCP client
    |
    v
ai-server-agent.service
(aiagent, unprivileged network service)
    |
    | local Unix socket
    v
ai-server-agent-executor.service
(root, no TCP listener)
    |
    +-- normal commands as aiworker
    +-- approved privileged commands as root
    +-- persistent jobs
```

The network-facing service is not root. Privileged host operations are delegated to the local executor over a Unix socket.

## MCP tools

The Agent exposes a deliberately small tool surface:

- `agent_environment` — environment and preservation information.
- `run_command` — shell command execution as `aiworker`.
- `run_root_command` — privileged shell execution with approval controls.
- `start_job`, `job_status`, `job_output`, `job_stop` — persistent background jobs.
- `read_file`, `write_file` — host filesystem operations.
- `browser_setup` — installs the optional isolated browser runtime.
- `browser_run` — Playwright browser automation.

The exact tools available to a client can be discovered through the MCP connection.

## Workspace and preservation

Normal project work is performed under:

```text
/srv/ai-workspace
```

The Agent writes a machine-readable environment manifest to:

```text
/var/lib/ai-server-agent/AI_ENVIRONMENT.json
```

The manifest records important services, paths, endpoints, and preservation rules so automation can avoid breaking the control plane or unrelated host software.

## Browser automation

Browser support is optional. `browser_setup` installs an isolated runtime under:

```text
/opt/ai-server-agent/browser
```

Writable browser profile/session data is stored separately under:

```text
/var/lib/ai-server-agent/runtime/browser
```

The optional browser runtime is not required for the MCP core service.

## Updating

For exact stable-release immutability, rerun the stable installer with the version pinned explicitly:

```bash
VERSION='v0.1.0'
curl -fsSLG \
  -H 'Accept: application/vnd.github.raw+json' \
  --data-urlencode "ref=$VERSION" \
  'https://api.github.com/repos/ach1992/ai-server-agent/contents/install.sh' | \
  sudo env AI_SERVER_AGENT_VERSION="$VERSION" AI_SERVER_AGENT_REF="$VERSION" bash
```

Keep both `AI_SERVER_AGENT_VERSION` and `AI_SERVER_AGENT_REF` pinned when you need to remain on an exact stable release.

## Uninstall

Remove the Agent services and binary while preserving configuration, state, users, optional runtime data, and workspace:

```bash
VERSION='v0.1.0'
curl -fsSLG \
  -H 'Accept: application/vnd.github.raw+json' \
  --data-urlencode "ref=$VERSION" \
  'https://api.github.com/repos/ach1992/ai-server-agent/contents/uninstall.sh' | \
  sudo env AI_SERVER_AGENT_YES='1' bash
```

To remove Agent-owned configuration, state, logs, optional runtime data, and the `aiagent` service account/group as well:

```bash
VERSION='v0.1.0'
curl -fsSLG \
  -H 'Accept: application/vnd.github.raw+json' \
  --data-urlencode "ref=$VERSION" \
  'https://api.github.com/repos/ach1992/ai-server-agent/contents/uninstall.sh' | \
  sudo env AI_SERVER_AGENT_YES='1' bash -s -- --purge
```

`/srv/ai-workspace` and the `aiworker` account/group are intentionally preserved even with `--purge` to reduce the risk of accidental project-data loss.

## Security notes

- Keep the MCP bearer credential private.
- Use TLS for any public-mode deployment.
- Keep server snapshots or backups before host-wide changes.
- Do not treat approval guardrails as a complete sandbox; privileged shell access is an intentional capability.
- Keep the Agent on a dedicated server and avoid using the current stable release for production workloads.

## License

AI Server Agent is released under the [MIT License](LICENSE).
