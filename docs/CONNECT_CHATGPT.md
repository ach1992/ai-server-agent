# Connect ChatGPT

AI Server Agent supports two connection topologies while keeping bearer authentication enabled:

1. a remote public HTTPS MCP endpoint;
2. a private/local MCP endpoint carried through OpenAI Secure MCP Tunnel.

ChatGPT full MCP/developer-mode functionality, including write/modify actions, is currently rolling out in beta for Business, Enterprise and Edu workspaces on ChatGPT web. UI labels, permissions and confirmation behavior can change, so treat current OpenAI documentation as authoritative for the ChatGPT-side workflow rather than treating this file as a permanent UI contract.

Current OpenAI guidance:

- Developer mode and MCP apps: <https://help.openai.com/en/articles/12584461>
- Apps in ChatGPT: <https://help.openai.com/en/articles/11487775-connectors-in>

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

## Create and test the custom MCP app in ChatGPT Business

For the current Business workspace flow on ChatGPT web:

1. use an **Admin/Owner** account; Business members cannot enable developer mode or deploy a custom MCP app;
2. enable Developer mode from the current workspace/user settings flow. OpenAI currently exposes it under **Workspace Settings → Permissions & Roles → Connected Data** and also when creating a custom app from **Workspace Settings → Apps → Create**;
3. open **Workspace Settings → Apps → Create**;
4. provide the remote MCP endpoint shown by `ai-server-agent-manage chatgpt-setup`, for example `https://mcp.example.com/mcp`;
5. choose the authentication mechanism offered by the current UI and provide the protected Agent bearer credential only in that trusted connection UI;
6. click **Scan Tools** and wait for the scan to complete;
7. create the app and verify it appears as a draft;
8. open a new normal ChatGPT conversation, select the draft app from the tools/app picker or refer to it in the prompt, and exercise the discovered Agent tools;
9. publish only after end-to-end validation. Business publishing is an Admin/Owner action through the workspace Apps controls.

For Business, current OpenAI behavior does not allow updating a published custom app's tools/metadata in place; recreate and republish if the published app definition must change. Treat this as a client/workspace behavior, not an Agent protocol guarantee.

ChatGPT app permissions control when the client asks before using app actions. Write/modify operations can require confirmation based on permissions and context, and especially risky actions can be blocked rather than presented for approval. Do not weaken Agent bearer authentication, tool safety metadata or server-side root guardrails to work around a ChatGPT-side confirmation/block.

For this project's full-MCP validation, use a normal ChatGPT web conversation. Current OpenAI guidance says agent mode does not use custom apps, while deep research can use custom apps only for read/fetch actions.

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
