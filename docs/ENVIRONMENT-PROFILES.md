# Environment Purpose Profiles

AI Server Agent supports three **operational purpose labels** to help a connected AI recognize the intended server use. These are **advisory**, not a permission switch, a separate Agent build, or different tool installations.

| Purpose | Prefer this server for |
| --- | --- |
| `development` | Code and project development, builds and debugging |
| `validation` | Testing candidates, reproducing failures and collecting acceptance evidence |
| `production` | Live services/data: prefer passive read-only inspection; ask for fresh, action-specific user consent before **each** state-changing operation, without relying on previous consent |

All three profiles expose the same authorized MCP and root/worker capabilities. Production's caution is **AI guidance, not an enforced read-only mode or per-operation approval check**: the Agent cannot independently verify fresh human consent, and an AI client may not display or follow the instructions. Existing authentication, policy and root approval behavior remain unchanged. For guaranteed enforcement, a separately designed authorization boundary would be required. No GitHub credentials are installed, revoked or changed by selecting a purpose.

## First installation

A **fresh interactive** installer asks for Development (default), Validation or Production and describes each before the connection wizard. Production also displays the advisory-only limitation.

For **noninteractive first installation**, the installer accepts:

- `AI_SERVER_AGENT_ENVIRONMENT_PROFILE=development|validation|production` (default: `development`)
- `AI_SERVER_AGENT_SERVER_LABEL` (optional, printable, at most 80 characters and 256 UTF-8 bytes; no surrounding whitespace)

For a release **that includes this feature**, put environment variables on the **`bash` side** of the permanent Stable bootstrap pipe (not only on `curl`). The bootstrap passes only these three non-secret inputs through `sudo` to the integrity-verified release installer; it does not forward arbitrary environment variables:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | \
  AI_SERVER_AGENT_NONINTERACTIVE=1 \
  AI_SERVER_AGENT_ENVIRONMENT_PROFILE=validation \
  AI_SERVER_AGENT_SERVER_LABEL='QA Server' bash
```

The short, fixed bootstrap URL intentionally reads **reviewed mutable `main` source**; the **installation target** is the latest published **immutable Stable**, with the release installer verified before privileged execution. Never run mutable `main/install.sh` as root or install an unreleased binary as though it were Stable. Development and Validation require Stable `v0.1.13` or later. **Production was added in immutable Stable `v0.1.15`** and is not available in `v0.1.14`: verify the actual installed release before selecting it. An explicit published release can be chosen by passing its tag as a `bash` argument to the same bootstrap URL. For direct source installation, pass the same settings to the privileged `install.sh` process.

The existing Connection Mode (local/public/Cloudflare) is an independent setup choice. Do not infer the Profile from the hostname or network address.

## Post-install management

From an independent SSH/operator terminal:

```bash
sudo ai-server-agent-manage profile-show
sudo ai-server-agent-manage profile-set validation
sudo ai-server-agent-manage profile-set production  # only on a version supporting it
sudo ai-server-agent-manage profile-label 'QA Server'
sudo ai-server-agent-manage profile-label-clear
```

The interactive `sudo ai-server-agent-manage` menu also includes **Server purpose / label**.

Changes are validated and root-lock serialized, then restart the **Agent service only** (the root Executor is not intentionally restarted); an active MCP connection may have to reconnect. Health and the effective environment manifest are checked. A failed activation restores the previous configuration when possible; unverified rollback retains protected recovery state and blocks further profile/runtime setting changes until reconciled. No Agent reinstall or source rebuild is required.

A new-baseline installer update/repair retains the selected purpose and label. An older release without Production support (including `v0.1.14`) cannot load `environment_profile=production`; its target-binary compatibility preflight must refuse that configuration. Independently, the protected Stable updater no longer supports legacy `v0.1.14` as a downgrade target because that release lacks the privileged lifecycle/startup trust contract. A future **eligible** Stable downgrade must still prove target config/profile compatibility; first switch to Development or Validation if the eligible target lacks Production. The Agent must not silently convert an unsupported profile. Migration guarantees for older installed Agent generations are not implied.

## AI-client discovery

The authenticated `agent_environment` output includes a stable `instance_id`, `environment_profile`, optional `server_label`, and the existing capability manifest. MCP `initialize` includes concise, fixed role-specific instructions.

Production tells a cooperative AI client to inspect first and ask for a **new, specific human authorization before every state-changing action** (including worker commands, file changes, jobs and Browser interactions). No prior confirmation should be reused. This is not an Agent-enforced gate: clients may not expose or obey the advice, and an `approval=true` argument alone does not prove consent. Use existing server/client authorization controls when actual restrictions are required.

## Feature delivery

The original Development/Validation profiles shipped in immutable Stable [`v0.1.13`](https://github.com/ach1992/ai-server-agent/releases/tag/v0.1.13). The Production option was added in immutable Stable [`v0.1.15`](https://github.com/ach1992/ai-server-agent/releases/tag/v0.1.15); it is not part of `v0.1.14`. Completed [Issue #143](https://github.com/ach1992/ai-server-agent/issues/143) retains the original source and installed-client evidence; completed #137/#59 record bounded fresh-host/program acceptance and its limits. Inspect the **actual installed release** and [GitHub Releases](https://github.com/ach1992/ai-server-agent/releases) before assuming these commands are available. The durable product-level decision is in the Owner-approved [Project Specification](PROJECT-SPEC.md#10-environment-purpose-profiles).
