# Connect ChatGPT Business

ChatGPT Business supports custom MCP apps in Developer Mode. AI Server Agent supports two connection topologies while keeping bearer authentication enabled.

## Direct remote MCP

For the primary public deployment path, expose the Agent through a public HTTPS hostname while keeping the Agent on its dedicated high port:

```text
ChatGPT Business
        |
        | HTTPS :443
        v
public edge / DNS provider
        |
        | HTTPS to dedicated origin port
        v
ai-server-agent :3210
(native TLS + bearer authentication)
```

The Agent does not install nginx, Caddy, Apache, `cloudflared`, or any other web/tunnel daemon, and it does not reserve ports 80/443. Public bind mode requires native TLS and refuses to start with plaintext HTTP.

Prepare a certificate and private key at absolute paths readable by the `aiagent` service, then install or reconfigure with:

```bash
sudo AI_SERVER_AGENT_BIND_MODE=public \
  AI_SERVER_AGENT_PORT=3210 \
  AI_SERVER_AGENT_TLS_CERT_FILE=/etc/ai-server-agent/tls/origin.crt \
  AI_SERVER_AGENT_TLS_KEY_FILE=/etc/ai-server-agent/tls/origin.key \
  bash install.sh
```

Keep encryption enabled from the public edge all the way to the Agent and keep the bearer token private. The installer never prints the public-mode token; it remains in `/etc/ai-server-agent/mcp.token` with restricted permissions.

In ChatGPT Business:

1. Enable Developer Mode for the workspace admin/owner account that will create the app.
2. Go to Workspace Settings → Apps → Create.
3. Enter the public MCP endpoint, for example `https://mcp.example.com/mcp`.
4. Select `Access token / API key` authentication.
5. Select the `Bearer` header scheme and enter the Agent bearer token in the ChatGPT connection UI. Do not copy the token into documentation, logs, issue comments, or source control.
6. Scan Tools and verify that a non-empty action list is discovered.
7. Publish/connect the app only when the intended workspace release gates permit it.

### Validated ChatGPT Business flow

The v0.1 validation on 2026-08-14 used the flow above successfully with the published `AI Server Agent Dev` custom app:

- `Access token / API key` with `Bearer` connected successfully.
- ChatGPT discovered the Agent actions.
- `agent_environment` executed successfully through the real ChatGPT connection.
- `run_command` executed successfully as `aiworker` in `/srv/ai-workspace`.
- The protected `read_file` approval flow was exercised end-to-end: `approval=false` returned `approval_required`, and after explicit user confirmation `approval=true` succeeded. The bearer token itself was not exposed.

### Current ChatGPT safety limitation

During the same validation, ChatGPT blocked direct `run_root_command` and `browser_run` calls with an OpenAI safety check before either call reached the MCP server. Do not claim those real ChatGPT calls succeeded and do not repeatedly retry identical blocked calls without new platform evidence.

Treat this as a current ChatGPT client/platform limitation, not an Agent regression. Server-side root approval behavior and browser capability remain covered by direct MCP, CI, and VPS validation. Do not weaken bearer authentication, tool annotations, or server guardrails to work around the client-side block.

## Private MCP with Secure MCP Tunnel

The default installation remains bearer-authenticated `127.0.0.1:3210/mcp`. Loopback prevents network exposure but is not an authentication boundary, so MCP requests still require the token.

For private/on-premises deployments, use OpenAI Secure MCP Tunnel according to the current OpenAI documentation. The installer creates `/etc/ai-server-agent/mcp.authorization`, containing the complete `Bearer ...` header value for a trusted local tunnel client. Keep that file readable only by the service identity that needs it and never copy the token into source control or shell history.

## Validation before publishing

For the real ChatGPT path, validate authentication, action discovery, a read-only call such as `agent_environment`, `run_command`, and one protected-resource approval flow. If ChatGPT blocks broader action-capable tools upstream before MCP, record that limitation accurately and rely on direct server-side evidence for those capabilities instead of weakening the Agent security model.
