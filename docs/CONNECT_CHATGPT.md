# Connect ChatGPT

This is the **ChatGPT/OpenAI client-specific integration guide**, not the Agent core architecture. The Agent's MCP/executor/policy/lifecycle core is client/vendor neutral; support for another client gets its own current compatibility evidence rather than inheriting ChatGPT assumptions.

AI Server Agent supports two connection topologies while keeping bearer authentication enabled:

1. a remote public HTTPS MCP endpoint;
2. a private/local MCP endpoint carried through OpenAI Secure MCP Tunnel.

ChatGPT's custom-MCP/plugin UI, workspace controls and permission labels evolve independently from AI Server Agent. Do **not** treat a historical menu path, a "Developer mode" label, or the legacy Apps page as a durable protocol requirement.

Use the current official OpenAI guidance at connection time:

- Create custom MCP server: <https://developers.openai.com/api/docs/guides/custom-mcp-server>
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

The protected Authorization value is revealed only after explicit confirmation in the terminal. Do not paste it into chat, issue comments, documentation, screenshots, shell history or source control. Provide it only to the trusted ChatGPT app-connection UI when configuring the MCP app.

## Create and test the custom MCP server/plugin in ChatGPT

Use an eligible ChatGPT workspace/account with permission to create or configure custom MCP servers. Exact roles and workspace controls are client-side policy and must be checked against the current official OpenAI guidance above.

For the current ChatGPT web flow:

1. open the **Plugins** surface (for example the Plugins entry/directory available in the current ChatGPT UI);
2. choose the current **Create custom MCP server** action (or the equivalent current label);
3. choose the connection type:
   - **Server URL** for the Agent's public HTTPS MCP endpoint; or
   - **Tunnel** when using OpenAI Secure MCP Tunnel for a private/local Agent;
4. provide the endpoint/tunnel identity shown by the supported Agent/OpenAI setup flow;
5. configure authentication using the protected Agent bearer credential only in the trusted ChatGPT connection UI; never paste that credential into chat, Issues, docs, screenshots or source control;
6. let ChatGPT discover/validate the MCP tools using the current UI and review the exposed read/write/action surface;
7. create/save the custom MCP configuration and verify it is available to the intended test/admin context;
8. exercise the Agent from a normal supported ChatGPT conversation using the current plugin/tool picker or invocation path;
9. publish/enable it for broader workspace use only after end-to-end validation and the current workspace-admin review requirements are satisfied.

Managed workspaces may expose plugin administration under **Admin/Workspace settings → Plugins**, while the underlying app/custom-MCP configuration may also remain visible through complementary or legacy **Apps** controls. Do not fail setup merely because an older `Apps → Create` or `Developer mode` label is absent; follow the current official product surface instead.

If the Agent tool schema changes after publication, follow the current ChatGPT refresh/update/recreate behavior documented by OpenAI. Do not assume that a historical in-place-update limitation or menu sequence is permanent.

ChatGPT-side action permissions and confirmations are separate from Agent-side authorization. Client permissions may require confirmation or deny an action, but they must never be used as a reason to weaken Agent bearer authentication, tool safety metadata, root/file authority truth, or server-side policy/approval guardrails.

Use a normal supported ChatGPT conversation for full-MCP validation. Whether specialized ChatGPT modes can invoke custom MCP/plugin actions is client behavior and should be re-validated against current OpenAI documentation when release acceptance depends on it.

## Private MCP with Secure MCP Tunnel

The default Agent installation is bearer-authenticated and loopback-only at `127.0.0.1:3210`.

ChatGPT does not connect directly to a local/private MCP server. For a private network, on-premises server or development machine, use OpenAI Secure MCP Tunnel according to the current OpenAI instructions instead of exposing the loopback listener directly to the internet.

The Agent's local MCP endpoint remains bearer-authenticated. Use the protected Authorization value from the server only where the trusted tunnel/client setup requires it.

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
