# AI Server Agent

**AI Server Agent** gives an authorized AI client a practical Linux development and testing environment through a lightweight, bearer-authenticated MCP control plane. It exposes real shell, file, job, Git/worktree, Browser, code-inspection, and debugging capabilities without turning the server into a heavyweight IDE or hosting panel.

Ordinary engineering work uses the unprivileged `aiworker` account. Privileged host actions are available through a separate root executor with approval and self-preservation safeguards. This is a **root-capable** dedicated-server product, not a sandbox for untrusted tenants.

Direct MCP access is a first-class standalone mode; MCP Gateway is an **optional future integration**, not a runtime dependency. ChatGPT is the first documented/validated AI client, but the core is deliberately client/vendor neutral.

For the agreed product vision, boundaries, and non-goals, see **[Project Specification](docs/PROJECT-SPEC.md)**. Current implementation is not the same as future accepted work.

## Supported hosts

- Ubuntu **22.04+** or Debian **11+**, with `systemd`
- Linux **amd64/x86_64** or **arm64/aarch64**
- A dedicated server the owner authorizes for AI-controlled development or validation

Debian 11 has limited upstream security maintenance; prefer Debian 12+ unless an appropriate extended-support arrangement is in place.

The Agent itself does **not** require a web server, database, Docker, a hosting panel, Node.js, Python, PHP, or a permanent claim on ports 80/443. Optional capabilities may require separately installed tools. See [platform and dependency details](docs/OPERATIONS.md#platform-support).

## Install

To install the **latest published Stable**, use this permanent, version-free command:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | bash
```

To install a **specific Stable release**, for example `v0.1.14`:

```bash
curl -fsSL https://raw.githubusercontent.com/ach1992/ai-server-agent/main/scripts/install-stable.sh | bash -s -- v0.1.14
```

The **command URL stays the same** for future releases. It loads the small bootstrap from the repository's maintained `main` branch; that source is **mutable** and must remain protected and reviewed, unlike a SHA-pinned bootstrap. By default the bootstrap asks GitHub Releases for the latest published immutable Stable, or checks the explicit release tag when supplied. If `jq` is missing, the bootstrap may first use `sudo apt-get` on supported Debian/Ubuntu systems to install `jq` and CA certificates from configured system repositories. **That prerequisite step can run before release verification.** The downloaded release `install.sh` is SHA-256-verified **before its separate privileged execution**, and its protected root-stage copy is rechecked. Run the bootstrap with `bash`, **not `sudo bash`**. Never pipe the raw latest-release installer directly into root.

For an exact-version install, troubleshooting, advanced or source installations and supply-chain verification, use the [installation guide](docs/OPERATIONS.md#install-the-latest-stable-release).

A successful core installation is separate from public connection setup. The first-run wizard provides Cloudflare-hostname HTTPS, local/private, existing certificate/TLS, or configure later. The core can remain healthy when public connection configuration is deferred.

## Configure and use

Open the server management menu:

```bash
sudo ai-server-agent-manage
```

Common commands:

```bash
sudo ai-server-agent-manage status
sudo ai-server-agent-manage runtime-show
sudo ai-server-agent-manage update
sudo ai-server-agent-manage repair
```

For credentials, Cloudflare, local/private mode, manual TLS, safe uninstall, purge and configurable runtime budgets, see [Operations](docs/OPERATIONS.md#management-commands). To connect ChatGPT, see [Connect ChatGPT](docs/CONNECT_CHATGPT.md). Other clients must be validated against their actual MCP/transport and tool-presentation support.

## Main capabilities

| Area | What the Agent provides |
| --- | --- |
| Host execution | Worker/root commands, privileged file operations, service/process diagnostics |
| Workspace and Git | Repository/worktree identity, versioned worker file/search/edit/transfer, normal Git CLI |
| Long-running work | Persistent jobs with status and ranged output; optional interactive Terminal |
| Browser and testing | Optional Playwright/Chromium, structured E2E steps, controlled Browser sessions |
| Code intelligence | Go/gopls plus optional, separately validated PHP, JS/TS, Python, Shell, Rust, HTML/CSS and PostgreSQL/MySQL/MariaDB SQL source analysis; not every method or runtime is available on every host |
| Debugging | Go/Delve and optional contained PHP CLI, Rust prebuilt, Python, Node/precompiled TypeScript and local HTML Browser DAP; never imply PHP HTTP-service, Shell DAP, extension-host or remote attach support |
| Environment | Authenticated Agent/environment identity, available tools, host footprint and warnings |

The approved language scope covers **Go; PHP; JavaScript/TypeScript; Python; Bash/POSIX Shell; Rust; HTML/CSS; and PostgreSQL/MySQL/MariaDB SQL**. The first optional LSP/DAP slice was completed in [#140](https://github.com/ach1992/ai-server-agent/issues/140); the accepted advanced modes and adapter/client acceptance remain in [#149](https://github.com/ach1992/ai-server-agent/issues/149). An optional adapter missing from a host is a provisioning/readiness gap, not cancellation of that language's accepted scope.

Not every integrated feature or optional adapter is ready on every OS/kernel/client. Use `agent_environment`, the applicable tool status and actual installed version rather than inferring availability from this table.

Read the [Tool Reference](docs/TOOL-REFERENCE.md) for tools, limits, examples, and important caveats. [Developer Runtime](docs/DEVELOPER-RUNTIME.md) documents the accepted future composition and unresolved delivery gates.

## Development and Validation server roles

Immutable Stable `v0.1.13` introduced two **Environment Purpose Profiles** (verify the installed version):

- **Development:** preferred for source/Git changes, builds, debugging and routine project development.
- **Validation:** preferred for candidate installation, real E2E/integration tests, reproduction and acceptance evidence.

The profiles label and guide compatible clients; **they are not access-control tiers or separate Agent binaries**. Both preserve the same MCP/root/worker tools. GitHub authentication and optional tool installation remain separate operator decisions.

Use a supporting installed release (`v0.1.13` or later); publishing new source does not upgrade an existing Agent. Purpose profiles are independent of `local/public` connection modes. See [Profile setup, management, and client discovery](docs/ENVIRONMENT-PROFILES.md), the [Project Specification](docs/PROJECT-SPEC.md#10-environment-purpose-profiles), and [Issue #143](https://github.com/ach1992/ai-server-agent/issues/143) for installed/client acceptance.

## Security and resource use

An authenticated principal with root-shell authority is effectively root-capable. Tool annotations and AI instructions do not constrain what authorized shell commands can do. Authentication, protected control-plane resources, approval, verified dependencies, bounded outputs and truthful failure reporting remain important; broad usable capabilities are deliberately preserved.

Resource budgets, timeouts and concurrency are bounded to protect host reliability. Some are configurable without source changes; other fixed implementation limits and remaining tests are tracked in [Issue #118](https://github.com/ach1992/ai-server-agent/issues/118). See [Operations](docs/OPERATIONS.md#security-model).

## Project documentation

- **[PROJECT-SPEC.md](docs/PROJECT-SPEC.md):** canonical purpose, scope, success criteria and durable principles
- **[ARCHITECTURE.md](docs/ARCHITECTURE.md):** trust boundaries, host resources, runtime architecture, install/update contracts
- **[DEVELOPER-RUNTIME.md](docs/DEVELOPER-RUNTIME.md):** detailed accepted capability design and future architecture
- **[ENVIRONMENT-PROFILES.md](docs/ENVIRONMENT-PROFILES.md):** Development/Validation purpose, install options, management and client metadata (requires an installed supporting release)
- **[TESTING.md](docs/TESTING.md):** validation strategy and evidence expectations
- **[OPERATIONS.md](docs/OPERATIONS.md):** full install, connectivity, management, updates and repair instructions
- **[TOOL-REFERENCE.md](docs/TOOL-REFERENCE.md):** MCP tool and feature details
- **[CONNECT_CHATGPT.md](docs/CONNECT_CHATGPT.md):** current client-specific connection help
- **[AGENTS.md](AGENTS.md):** engineering/contributor instructions

GitHub [Issues](https://github.com/ach1992/ai-server-agent/issues), [Pull Requests](https://github.com/ach1992/ai-server-agent/pulls), [Actions](https://github.com/ach1992/ai-server-agent/actions), and [Releases](https://github.com/ach1992/ai-server-agent/releases) own live work, reviews, CI, and published-delivery status. Historical installed validation and remaining client limitations are tracked primarily in [Issue #59](https://github.com/ach1992/ai-server-agent/issues/59), not in this README.

## License

See [LICENSE](LICENSE).
