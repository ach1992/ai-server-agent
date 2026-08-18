# Connect ChatGPT

AI Server Agent supports two connection topologies while keeping bearer authentication enabled:

1. a remote public HTTPS MCP endpoint;
2. a private/local MCP endpoint carried through OpenAI Secure MCP Tunnel.

ChatGPT full MCP/developer-mode functionality is currently available for Business, Enterprise and Edu workspaces on ChatGPT web and is still evolving. UI labels, permissions and action-confirmation behavior can change, so treat the current OpenAI documentation as authoritative for the ChatGPT-side workflow.

Current OpenAI guidance:

- Developer mode and MCP apps: <https://help.openai.com/en/articles/12584461-developer-mode-and-mcp-apps-in-chatgpt>
- Apps in ChatGPT: <https://help.openai.com/en/articles/11487775-apps-in-chatgpt>

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

The protected Authorization value is revealed only after explicit confirmation in the terminal. Do not paste it into chat, issue comments, documentation, screenshots, shell history or source control.

## Create/test the custom MCP app in ChatGPT

Use the current ChatGPT developer-mode flow for your workspace. At a high level:

1. ensure Developer mode/custom MCP apps are enabled for the account that will configure the app;
2. open **Apps → Create** from the current workspace/user settings flow;
3. enter the remote MCP endpoint shown by `ai-server-agent-manage chatgpt-setup`, for example `https://mcp.example.com/mcp`;
4. choose the available bearer/access-token authentication option and provide the protected Agent credential only in the ChatGPT connection UI;
5. run **Scan Tools** and verify that the expected Agent tools are discovered;
6. create the app as a draft and test it before publishing;
7. publish only through the workspace controls appropriate to your plan and organization.

OpenAI currently notes that write/modify actions may require ChatGPT-side confirmation depending on app permissions and action context, and some especially risky actions can be blocked rather than offered for approval. Do not weaken Agent bearer authentication, tool metadata or server-side root guardrails to work around a client-side safety decision.

## Private MCP with Secure MCP Tunnel

The default Agent installation is bearer-authenticated and loopback-only at `127.0.0.1:3210`.

ChatGPT does not connect directly to a local/private MCP server. For a private network, on-premises server or development machine, use OpenAI Secure MCP Tunnel according to the current OpenAI instructions instead of exposing the loopback listener directly to the internet.

The Agent's local MCP endpoint remains bearer-authenticated. Use the protected Authorization value from the server only where the trusted tunnel/client setup requires it.

## Validation

Before treating the ChatGPT connection as ready, verify at least:

1. the app/tool scan succeeds;
2. a read-only call such as `agent_environment` returns the expected server identity;
3. an ordinary `run_command` executes as `aiworker` in `/srv/ai-workspace`;
4. authentication rejection is observed when the bearer credential is missing/invalid;
5. any action-capable/root behavior you intend to allow still observes both ChatGPT-side controls and the Agent's own approval/policy boundary.

Do not record a ChatGPT client limitation or one successful historical run as a permanent architecture guarantee. Re-validate client-side behavior against the current ChatGPT product when it matters for a release.
