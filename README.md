# AI Server Agent

AI Server Agent connects an authorized AI client to a Linux server through **MCP**. It lets the client inspect and edit projects, run commands and tests, debug applications, work with Git repositories, and investigate the server and its services.

Regular project work runs as the unprivileged `aiworker` account. Explicitly authorized host administration can use the separate **root-capable** executor. This is intended for a server you control, **not** as a sandbox for mutually untrusted users.

The Agent exposes its MCP tools directly to compatible AI clients. ChatGPT has a dedicated connection guide.

## Requirements

- Ubuntu 22.04+ or Debian 11+ with `systemd`
- Linux `amd64`/`x86_64` or `arm64`/`aarch64`
- An authorized Linux host; production workloads require extra operator care because the Agent retains broad execution capabilities

Optional features such as browser automation and additional language debuggers may require separately installed tools. The Agent itself does not require Docker, a web server, a database, Node.js, Python, PHP, or control of ports 80/443. For Debian 11, arrange appropriate extended security maintenance or prefer Debian 12+.

## Install

Install the **latest published Stable**:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | bash
```

To install a specific published Stable version (example):

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | bash -s -- v0.1.17
```

The short bootstrap URL intentionally uses the project's **reviewed, mutable `main` branch**; the installed version comes from an **immutable Stable Release**. The release installer is SHA-256-verified before its privileged execution and verified again in protected root staging. On supported systems, the bootstrap may run `sudo apt-get` to install missing `jq` and CA-certificate prerequisites *before* release verification. Review the bootstrap before running it. **Run it as shown, not through `sudo bash`.** Both bootstrap commands are **fresh-install only**: they refuse an existing or partial Agent installation, even with an explicit version. For an installed Agent, use `sudo ai-server-agent-manage update`; see [Operations](docs/OPERATIONS.md) for credential-safe version rollback.

After installing, choose public HTTPS with guided Cloudflare, local/private operation, your own TLS certificate, or **configure later**. Deferring public connectivity does not make the core installation fail.

## Manage and connect

Open the interactive management console:

```bash
sudo ai-server-agent-manage
```

Useful commands:

```bash
sudo ai-server-agent-manage status
sudo ai-server-agent-manage runtime-show
sudo ai-server-agent-manage update
sudo ai-server-agent-manage repair
```

Use the [ChatGPT connection guide](docs/CONNECT_CHATGPT.md) to set up a custom MCP connection. The [operator guide](docs/OPERATIONS.md) covers authentication, connection modes, safe updates, service management, and uninstalling.

## What you can do

| Capability | Examples |
| --- | --- |
| Server operations | Run worker commands, request authorized root operations, inspect files and services |
| Projects and Git | Discover repositories/worktrees, search and edit source safely, use Git CLI |
| Longer tasks | Run persistent jobs and bounded interactive worker/root terminals; newer installed builds support longer pastes and optional output waiting |
| Testing and browsers | Run tests, managed Browser sessions, isolated profiles, bounded screenshots and private traces when supported by the installed build |
| Code intelligence | Use Go/gopls and optional language-aware analysis; supported SQL source navigation stays offline and never executes SQL |
| Debugging | Use Go/Delve and supported optional debuggers; check actual adapter readiness |
| File transfer | Transfer and inspect files in bounded, integrity-aware chunks |

Managed Browser concurrency, private trace, opt-in Chromium OS sandbox, image quality/region selection, long Terminal input/output wait, and additional SQL navigation are **version-dependent**; some are present in source before the next Stable publication. Features, optional adapters, and native file/image presentation vary with the **installed Agent version**, host tools, and AI client. Check the [latest published Stable](https://github.com/ach1992/ai-server-agent/releases/latest), the authenticated `agent_environment` result, and per-tool readiness; neither a merged PR nor publication upgrades an installed host or proves native client attachment rendering. See the [MCP tool reference](docs/TOOL-REFERENCE.md) for exact operations and limitations.

## Server purpose: Development, Validation or Production

A supported Agent can describe its intended use as **Development** (build/debug), **Validation** (test/acceptance) or **Production** (live workloads; inspect first, request fresh consent for each change). These are **AI guidance, not enforced permission tiers**; all authorized tools remain accessible. The Production choice is available starting with Stable `v0.1.15` (not in `v0.1.14`). See [Environment profiles](docs/ENVIRONMENT-PROFILES.md) for setup and the approval limitation.

## Security

Treat a root-authorized connection as access to the underlying server. The Agent uses bearer authentication, approval and protected execution boundaries, but it does not make arbitrary root shell commands harmless. Keep credentials private and use dedicated hosts and appropriate operator controls. [Security policy](SECURITY.md).

## More information

Find the [latest published release](https://github.com/ach1992/ai-server-agent/releases), [installation and troubleshooting guide](docs/OPERATIONS.md), [ChatGPT setup guide](docs/CONNECT_CHATGPT.md), and [tool reference](docs/TOOL-REFERENCE.md).

Licensed under [MIT](LICENSE).
