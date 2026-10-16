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

For a release **that includes this feature**, put environment variables on the **`bash` side** of the permanent Stable bootstrap pipe (not only on `curl`). The bootstrap passes only these three non-secret inputs through `sudo` to the integrity-verified release installer; it does not forward arbitrary environment variables:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | \
  AI_SERVER_AGENT_NONINTERACTIVE=1 \
  AI_SERVER_AGENT_ENVIRONMENT_PROFILE=validation \
  AI_SERVER_AGENT_SERVER_LABEL='QA Server' bash
```

The short, fixed bootstrap URL intentionally reads **reviewed mutable `main` source**; the **installation target** is the latest published **immutable Stable**, with the release installer verified before privileged execution. Never run mutable `main/install.sh` as root or install an unreleased binary as though it were Stable. Profiles require Stable `v0.1.13` or later (`v0.1.14` also supports them); an explicit release can also be chosen by passing its tag as a `bash` argument to the same bootstrap URL. For direct source installation, pass the same settings to the privileged `install.sh` process.

The existing Connection Mode (local/public/Cloudflare) is an independent setup choice. Do not infer the Profile from the hostname or network address.

## Post-install management

From an independent SSH/operator terminal:

```bash
sudo ai-server-agent-manage profile-show
sudo ai-server-agent-manage profile-set validation
sudo ai-server-agent-manage profile-label 'QA Server'
sudo ai-server-agent-manage profile-label-clear
```

The interactive `sudo ai-server-agent-manage` menu also includes **Server purpose / label**.

Changes are validated and root-lock serialized, then restart the **Agent service only** (the root Executor is not intentionally restarted); an active MCP connection may have to reconnect. Health and the effective environment manifest are checked. A failed activation restores the previous configuration when possible; unverified rollback retains protected recovery state and blocks further profile/runtime setting changes until reconciled. No Agent reinstall or source rebuild is required.

A new-baseline installer update/repair retains the selected purpose and label. Migration guarantees for older installed Agent generations are not implied.

## AI-client discovery

The authenticated `agent_environment` output includes a stable `instance_id`, `environment_profile`, optional `server_label`, and the existing capability manifest. MCP `initialize` includes concise, fixed role-specific instructions.

Some clients may not show or obey server-provided instructions; inspect the authenticated environment output to confirm the intended role. Instructions and labels do not enforce task permission or grant authority.

## Feature delivery

Environment profiles first shipped in immutable Stable [`v0.1.13`](https://github.com/ach1992/ai-server-agent/releases/tag/v0.1.13). Completed [Issue #143](https://github.com/ach1992/ai-server-agent/issues/143) retains the original source and installed-client acceptance evidence; new fresh-host/program acceptance belongs to #137/#59. Inspect the **actual installed release** and [GitHub Releases](https://github.com/ach1992/ai-server-agent/releases) before assuming these commands are available. The durable product-level decision is in the Owner-approved [Project Specification](PROJECT-SPEC.md#10-environment-purpose-profiles).
