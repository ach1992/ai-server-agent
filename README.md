# AI Server Agent

A small self-hosted MCP control plane that turns a **dedicated Ubuntu/Debian test server** into a machine ChatGPT can inspect, configure, test, and operate.

> **Pre-release:** do not use on production. The project intentionally exposes root-capable tools and must be validated on disposable/snapshot-backed servers first.

## Design goals

- One-command installation.
- Ubuntu 22.04+ and Debian 11+ (`amd64` / `arm64`).
- Minimal host dependencies and no web-stack ownership.
- Full root access when a task genuinely needs it.
- Unprivileged default command execution.
- Persistent background jobs that survive MCP disconnects.
- Optional isolated Playwright browser runtime.
- Machine-readable self-preservation manifest so AI knows what not to break.
- No dependency on nginx, Apache, PHP, MySQL, Docker, Node.js, Python, aaPanel, or ports 80/443.

## What ChatGPT gets

- `agent_environment` — control-plane manifest and preservation rules.
- `run_command` — arbitrary Bash as `aiworker`.
- `run_root_command` — arbitrary Bash as root, with connection/destruction guardrails.
- `start_job`, `job_status`, `job_output`, `job_stop` — persistent background work.
- `read_file`, `write_file` — host filesystem access through the privileged executor.
- `browser_setup` — optional private Node.js + Playwright runtime.
- `browser_run` — run Playwright JavaScript with `browser`, `context`, and `page` available.

This intentionally keeps the MCP surface small. Anything not covered by a structured tool can still be done through the shell.

## Install from the current development branch

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/feat/v0.1-agent/install.sh | sudo AI_SERVER_AGENT_REF=feat/v0.1-agent bash
```

The installer validates OS/architecture, installs only small download/build utilities, builds a static Go binary using a temporary verified Go toolchain, creates isolated service users/directories, starts the two services, and performs a health check. The temporary Go toolchain is removed automatically.

For stable releases, the same installer will download prebuilt release artifacts instead of building on the server.

## Service isolation

```text
ChatGPT / MCP tunnel
        |
        v
ai-server-agent.service       (aiagent, no root)
        |
        | Unix socket only
        v
ai-server-agent-executor      (root, no TCP listener)
        |
        +-- commands as aiworker
        +-- commands as root
        +-- persistent systemd jobs
```

The MCP endpoint defaults to `127.0.0.1:3210/mcp`. This avoids collisions with hosting software. Keep it private and use a secure MCP tunnel whenever possible.

## Self-preservation

The installed server contains:

```text
/var/lib/ai-server-agent/AI_ENVIRONMENT.json
```

It tells AI which paths, services, ports, and primitives keep the connection alive. The agent also exposes the same information through `agent_environment` and places a concise preservation rule in MCP discovery instructions.

The policy guard requires explicit approval for obvious actions that can cut connectivity or damage protected resources. Because arbitrary root shell is a deliberate feature, this is not a perfect sandbox. Use a server dedicated to AI and keep snapshots/backups.

## Browser automation

The browser is not a core dependency. On first use, `browser_setup` installs an isolated Node.js/Playwright/Chromium engine under:

```text
/opt/ai-server-agent/browser
```

The engine is root-owned and read-only to the normal project worker. Writable browser profiles and session data live separately under:

```text
/var/lib/ai-server-agent/runtime/browser
```

This leaves the system Node installation and web-server packages untouched. Removing the optional browser runtime disables browser tools but does not stop the MCP core. `browser_run` can execute Playwright code for E2E, UI, console, network, upload/download, authentication, and production-readiness checks.

## Uninstall

```bash
sudo ./uninstall.sh
```

This preserves config/state/users/workspace and optional runtimes by default. To remove agent-owned config/state/logs/runtime and the `aiagent` account too:

```bash
sudo ./uninstall.sh --purge
```

`/srv/ai-workspace` and the `aiworker` account are preserved even with `--purge` to reduce accidental project-data loss.

## Status

v0.1 is not release-ready until CI passes and clean-VM install/MCP/browser/root/recovery tests succeed on Ubuntu 22.04+, Ubuntu 24.04+, Debian 11+, and Debian 12+.
