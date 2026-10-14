# Environment Purpose Profiles

AI Server Agent supports two **operational purpose labels** to help a connected AI choose the right authorized server. This is **not** a permission switch, a GitHub integration, a separate Agent build, or a different runtime/tool installation.

| Purpose | Prefer this server for |
| --- | --- |
| `development` | Ordinary Git/source development, builds, debugging and project management |
| `validation` | Installing candidates, running real E2E/integration tests, reproducing failures and gathering acceptance evidence |

All MCP and root/worker capabilities remain available with either purpose. A Validation server can run Git, build, debug or develop Agent itself when the operator explicitly requests that work. No GitHub credentials are installed, revoked or changed by selecting a purpose.

## First installation

A **fresh interactive** installer asks for Development (default) or Validation and explains each purpose before the normal connection wizard.

For **noninteractive first installation**, the installer accepts:

- `AI_SERVER_AGENT_ENVIRONMENT_PROFILE=development|validation` (default: `development`)
- `AI_SERVER_AGENT_SERVER_LABEL` (optional, printable, at most 80 characters and 256 UTF-8 bytes; no surrounding whitespace)

Provide these environment variables to the installer process itself; do **not** prefix only the `curl` process of a piped bootstrap. Follow the [published authenticated installer](../README.md) and any documented environment-passing behavior for your chosen installation path.

The existing Connection Mode (local/public/Cloudflare) is an independent setup choice. Do not infer the Profile from the hostname or network address.

## Post-install management

From an independent SSH/operator terminal:

```bash
sudo ai-server-agent-manage profile-show
sudo ai-server-agent-manage profile-set validation
sudo ai-server-agent-manage profile-label 'QA Server'
sudo ai-server-agent-manage profile-label-clear
```

The interactive `sudo ai-server-agent-manage` menu also includes **Environment purpose / server label**.

Changes are validated and root-lock serialized, then restart the **Agent service only** (the root Executor is not intentionally restarted); an active MCP connection may have to reconnect. Health and the effective environment manifest are checked. A failed activation restores the previous configuration when possible; unverified rollback retains protected recovery state and blocks further profile/runtime setting changes until reconciled. No Agent reinstall or source rebuild is required.

A new-baseline installer update/repair retains the selected purpose and label. Migration guarantees for older installed Agent generations are not implied.

## AI-client discovery

The authenticated `agent_environment` output includes a stable `instance_id`, `environment_profile`, optional `server_label`, and the existing capability manifest. MCP `initialize` includes concise, fixed role-specific instructions.

Some clients may not show or obey server-provided instructions; inspect the authenticated environment output to confirm the intended role. Instructions and labels do not enforce task permission or grant authority.

## Feature delivery

The owning acceptance work is [Issue #143](https://github.com/ach1992/ai-server-agent/issues/143). This guide describes source functionality under development; inspect the **installed release** and [GitHub Releases](https://github.com/ach1992/ai-server-agent/releases) before assuming the commands are shipped. The product-level decision is recorded in the Owner-approved Project Specification (documentation integration through [PR #144](https://github.com/ach1992/ai-server-agent/pull/144)).
