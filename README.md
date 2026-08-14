# AI Server Agent

A small self-hosted MCP control plane that turns a **dedicated Ubuntu 22.04 LTS amd64 test server** into a machine ChatGPT can inspect, configure, test, and operate.

> **Safety scope:** use only on dedicated development/test servers, not production. The project intentionally exposes root-capable tools; keep snapshots/backups and treat the Agent as privileged infrastructure.

## v0.1 support scope

The stable v0.1 release supports **Ubuntu 22.04 LTS on amd64 only**. Ubuntu 24.04, Debian 11/12, arm64, and aaPanel compatibility are intentionally deferred until they receive release-grade validation. Development/source installer paths may remain technically usable on additional Ubuntu/Debian or arm64 environments, but they are not part of the v0.1 support claim and stable v0.1 artifacts are amd64-only.

## Design goals

- One-command installation.
- Release-grade support for Ubuntu 22.04 LTS (`amd64`) in v0.1.
- Minimal host dependencies and no web-stack ownership.
- Full root access when a task genuinely needs it.
- Unprivileged default command execution.
- Persistent background jobs that survive MCP disconnects.
- Optional isolated Playwright browser runtime.
- Machine-readable self-preservation manifest so AI knows what not to break.
- No dependency on nginx, Apache, PHP, MySQL, Docker, Node.js, Python, aaPanel, or ports 80/443.
- Optional native TLS on the Agent's dedicated high port for direct remote MCP deployments.

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
REF='main'
curl -fsSLG \
  -H 'Accept: application/vnd.github.raw+json' \
  --data-urlencode "ref=$REF" \
  'https://api.github.com/repos/ach1992/ai-server-agent/contents/install.sh' | \
  sudo AI_SERVER_AGENT_REF="$REF" bash
```

For source installs, `AI_SERVER_AGENT_REF` may be a branch, tag, or full commit SHA. The installer resolves mutable names to an immutable commit SHA before downloading the source archive. For the most reproducible validation, pass a full commit SHA.

The installer validates OS/architecture compatibility, installs only small download/build utilities, builds a static Go binary using a temporary verified Go toolchain, creates isolated service users/directories, starts the two services, and performs a health check. The temporary Go toolchain is removed automatically.

For stable releases, the same installer downloads prebuilt release artifacts instead of building on the server. v0.1 stable release artifacts are produced for Linux `amd64` only.

## Service isolation

```text
remote MCP client
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

The default MCP endpoint is bearer-authenticated `127.0.0.1:3210/mcp`. Loopback avoids web-stack collisions but is not treated as an authentication boundary: untrusted local project/browser code must not be able to call root-capable MCP tools. The installer creates a protected `/etc/ai-server-agent/mcp.authorization` header file for a trusted local client.

For a direct remote deployment, use `public` bind mode together with native TLS. Public mode refuses to start without both a certificate and private key, and still uses the Agent's configurable high port rather than reserving 80/443:

```bash
sudo AI_SERVER_AGENT_BIND_MODE=public \
  AI_SERVER_AGENT_PORT=3210 \
  AI_SERVER_AGENT_TLS_CERT_FILE=/etc/ai-server-agent/tls/origin.crt \
  AI_SERVER_AGENT_TLS_KEY_FILE=/etc/ai-server-agent/tls/origin.key \
  bash install.sh
```

The TLS files must already exist at absolute paths readable by the `aiagent` service identity. Keep bearer authentication enabled end-to-end. An external edge may map standard HTTPS to the Agent's origin port, but the Agent itself does not install or own a reverse proxy.

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

This preserves config/state/users/workspace and optional runtimes by default. To remove agent-owned config/state/logs/runtime and the `aiagent` service account/group too:

```bash
sudo ./uninstall.sh --purge
```

`/srv/ai-workspace` and the `aiworker` account/group are preserved even with `--purge` to reduce accidental project-data loss.

## Status

v0.1 release readiness is intentionally narrow: CI must pass on Ubuntu 22.04 amd64, one disposable Ubuntu 22.04 amd64 lifecycle run must prove normal uninstall/purge/reinstall behavior, stable amd64 artifacts and `SHA256SUMS` must verify, and the final release review/human release gate must pass. Broader OS/architecture and aaPanel compatibility claims are deferred from v0.1.
