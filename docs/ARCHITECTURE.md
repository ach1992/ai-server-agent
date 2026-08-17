# Architecture

AI Server Agent is intentionally host-neutral. The control plane is a single static Go binary used in two systemd services:

- `ai-server-agent.service`: unprivileged MCP HTTP process (`aiagent`).
- `ai-server-agent-executor.service`: root-only local executor with no TCP listener.

The services communicate through `/run/ai-server-agent/executor.sock` using a random local token. Normal project commands are dropped to `aiworker`; root commands run only when the root tool is selected.

Root-trusted control metadata is isolated under `/etc/ai-server-agent/control` (`root:root`, non-writable by `aiagent`/`aiworker`). Install identity is strict JSON and is never shell-sourced. The top-level `/var/lib/ai-server-agent` hierarchy plus its `jobs` and `runtime` container entries are root-owned so unprivileged identities cannot replace paths later consumed by the privileged executor; writable worker/browser files exist only beneath root-controlled directory entries. Privileged job-file reads reject symlinks/non-regular files, and legacy browser-data symlinks are removed without being followed during upgrade.

## Non-interference contract

The core does **not** install or require nginx, Apache, PHP, MySQL/MariaDB, Docker, Node.js, Python, Redis, aaPanel, or any hosting panel. It does not bind ports 80 or 443. Projects and AI workflows may install, replace, configure, or remove those components without taking down the MCP service.

The only required host primitives after installation are systemd, `/bin/bash`, and standard Linux process/filesystem facilities. Installation uses `curl`, `tar`, and `xz` only to obtain/build the static binary.

## Self-preservation manifest

At startup the agent writes `/var/lib/ai-server-agent/AI_ENVIRONMENT.json`. The same data is available through the `agent_environment` MCP tool. It records:

- agent services and binary/config/state paths;
- the active MCP bind address;
- executor socket;
- required host primitives;
- optional runtime components;
- rules for avoiding accidental disconnection.

The manifest is also summarized in MCP server instructions, so an AI client receives the preservation contract during discovery.

## Optional browser runtime

Browser automation is optional. `browser_setup` creates a root-owned private Node.js + Playwright + Chromium engine under `/opt/ai-server-agent/browser`. The normal `aiworker` cannot modify that engine. Persistent browser profiles and writable session data live separately under `/var/lib/ai-server-agent/runtime/browser`.

The browser capability does not use the system Node installation, does not run a public web service, and does not take ownership of a web port. It may install shared OS libraries required by Chromium only after explicit approval. Removing the browser runtime disables browser automation without affecting the MCP core.

## Persistent work

`start_job` creates a transient systemd unit (`ai-job-*`) and redirects output to the state directory. Jobs are independent of the MCP request and survive ChatGPT disconnections and MCP service restarts.

Interactive terminal software such as `tmux` is deliberately optional rather than a core dependency. AI can install it through the root tool if an interactive long-lived TTY is required.

## Root safety boundary

Arbitrary root shell is intentionally supported because real installation and diagnostic workflows may require it. The policy engine flags common destructive operations and changes that reference protected agent resources. This is a guardrail against accidents, not a security sandbox: a deliberately obfuscated root command can bypass string classification. Dedicated test servers, snapshots, backups, and ChatGPT action confirmations remain required operational controls.
