# Connect ChatGPT

This is the **ChatGPT/OpenAI client-specific integration guide**, not the Agent core architecture. The Agent's MCP/executor/policy/lifecycle core is client/vendor neutral; support for another client gets its own current compatibility evidence rather than inheriting ChatGPT assumptions.

AI Server Agent currently ships built-in lifecycle/setup support for a remote public HTTPS MCP endpoint while keeping bearer authentication enabled. Its bearer-authenticated loopback MCP endpoint can also be carried through OpenAI Secure MCP Tunnel when that tunnel is provisioned and operated separately with the official OpenAI tooling.

The Agent does **not** yet ship first-class Tunnel installation, service lifecycle, menu integration, status/repair, or credential management. That Agent-managed topology remains parked in Issue #35 until separately reactivated and implemented.

ChatGPT's custom-MCP/plugin UI, workspace controls and permission labels evolve independently from AI Server Agent. Do **not** treat a historical menu path, a "Developer mode" label, or the legacy Apps page as a durable protocol requirement.

Use the current official OpenAI guidance at connection time:

- Add custom MCP server: <https://developers.openai.com/api/docs/guides/custom-mcp-server>
- Official plugin connection walkthrough: <https://developers.openai.com/plugins/build/app-quickstart>
- Secure MCP Tunnel: <https://developers.openai.com/api/docs/guides/secure-mcp-tunnels>
- Plugins in ChatGPT and Codex: <https://help.openai.com/en/articles/20001256-plugins-in-chatgpt>
- Admin controls for plugins and apps: <https://help.openai.com/en/articles/11509118-admin-controls-security-and-compliance-for-plugins-and-apps>

Current OpenAI surfaces use **Plugins** as the primary discovery/management path. Some managed workspaces may still expose complementary or legacy **Apps** controls. Plan/workspace permissions and UI names can change; the Agent contract is the MCP endpoint + authentication/tool behavior, not a specific ChatGPT navigation sequence.

## Direct remote MCP

The guided Cloudflare path is the simplest supported direct-public configuration:

```text
ChatGPT
    |
    | HTTPS :443
    v
Cloudflare edge / public DNS
    |
    | HTTPS to the dedicated Agent origin port
    v
ai-server-agent :3210
(native TLS + bearer authentication)
```

The Agent does not install a web server/tunnel daemon or reserve ports 80/443. Public bind mode requires native TLS and does not support a plaintext public MCP endpoint.

Configure the server first:

```bash
sudo ai-server-agent-manage configure-cloudflare
```

or, when you already manage the public edge and certificate yourself:

```bash
sudo ai-server-agent-manage configure-manual-tls
```

Then show the current MCP URL/auth guidance:

```bash
sudo ai-server-agent-manage chatgpt-setup
```

Existing Authorization plaintext is not retained for later display. When a new direct credential is needed, run `sudo ai-server-agent-manage credential-rotate direct-default`; the replacement value is revealed once in the protected local terminal after verified cutover. Do not paste it into chat, issue comments, documentation, screenshots, shell history or source control. Provide it only to the trusted ChatGPT app-connection UI.

## Create and test the custom MCP server/plugin in ChatGPT

Use an eligible ChatGPT workspace/account with permission to create or configure custom MCP servers. Exact roles and workspace controls are client-side policy and must be checked against the current official OpenAI guidance above.

For the current ChatGPT web flow:

1. Open **Plugins** and use the **+** action to choose **Add custom MCP server**.
2. Enter a name and the connection details: **Server URL** for the Agent's built-in public HTTPS MCP endpoint, or **Tunnel** only if the official OpenAI Secure MCP Tunnel is already provisioned separately.
3. Configure authentication with the protected Agent bearer **only in the trusted connection form**; never paste it into chat, Issues, docs, screenshots or source control.
4. Review the server's tool/permission details and risk warning, acknowledge the risks, then choose **Create as a plugin**.
5. Install the resulting plugin in the intended personal/workspace context, subject to current administrative controls.
6. Open a **new supported conversation**, invoke the installed plugin with `@` or the available tool picker, and verify `agent_environment` and a harmless worker action against the expected host.
7. If the **installed** Agent tool schema changes, use **Refresh** on the existing plugin connection where supported and verify the new client-visible catalog in a fresh conversation. A source merge alone does not upgrade the Agent or refresh the client; recreate only if the platform requires it.
8. Enable wider workspace distribution only after actual end-to-end tests, permission review and applicable workspace-admin authorization.

Managed workspaces may expose plugin administration under **Admin/Workspace settings → Plugins**, while the underlying app/custom-MCP configuration may also remain visible through complementary or legacy **Apps** controls. Do not fail setup merely because an older `Apps → Create` or `Developer mode` label is absent; follow the current official product surface instead.

ChatGPT-side action permissions and confirmations are separate from Agent-side authorization. Client permissions may require confirmation or deny an action, but they must never be used as a reason to weaken Agent bearer authentication, tool safety metadata, root/file authority truth, or server-side policy/approval guardrails.

Use a normal supported ChatGPT conversation for full-MCP validation. Whether specialized ChatGPT modes can invoke custom MCP/plugin actions is client behavior and should be re-validated against current OpenAI documentation when release acceptance depends on it.

## Private MCP with Secure MCP Tunnel

The default Agent installation is bearer-authenticated and loopback-only at `127.0.0.1:3210`.

ChatGPT does not connect directly to a local/private MCP server. An operator may use OpenAI Secure MCP Tunnel according to the current OpenAI instructions to carry that local endpoint without exposing it directly to the internet. Today this is an **external integration**, not an Agent-managed connection mode: `ai-server-agent-manage` does not install/manage the Tunnel runtime, its service, Tunnel credentials, or Tunnel status/repair lifecycle. First-class Agent-managed Tunnel support is tracked by parked Issue #35.

The Agent's local MCP endpoint remains bearer-authenticated. Use a currently issued named Agent credential only where the trusted tunnel/client setup requires it, and follow current OpenAI guidance for protected header/secret handling.

## End-to-end validation

Before treating the ChatGPT connection as ready, verify at least:

1. the app/tool scan succeeds against the intended public/tunneled endpoint;
2. the expected Agent tools are discovered with their intended metadata;
3. a read-only call such as `agent_environment` returns the expected server identity;
4. an ordinary `run_command` executes as `aiworker` in `/srv/ai-workspace` using a harmless command;
5. authentication rejection is observed when the bearer credential is missing/invalid;
6. the normal ChatGPT conversation can select/invoke the app after connection;
7. the current ChatGPT confirmation UI behaves as expected for action-capable tools;
8. any action-capable/root behavior you intend to allow still observes the Agent's own approval/policy boundary regardless of ChatGPT-side permission settings;
9. reconnect/recreate behavior is understood before publishing if the MCP tool schema changes.

Do not record a ChatGPT client limitation or one successful historical run as a permanent architecture guarantee. Re-validate client-side behavior against the current ChatGPT product when it matters for a release or operational change.
