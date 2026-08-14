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

1. Enable Developer Mode for your admin/owner account.
2. Go to Workspace Settings → Apps → Create.
3. Enter the public MCP endpoint, for example `https://mcp.example.com/mcp`.
4. Select the authentication mechanism supported by the current ChatGPT UI and compatible with the server.
5. Click Scan Tools and keep the app in draft while validating tools and approval behavior.

AI Server Agent currently enforces bearer authentication. If the current ChatGPT custom-app UI does not offer a compatible static bearer/API-token mechanism, do not disable authentication; add a supported OAuth flow before publishing the app.

## Private MCP with Secure MCP Tunnel

The default installation remains bearer-authenticated `127.0.0.1:3210/mcp`. Loopback prevents network exposure but is not an authentication boundary, so MCP requests still require the token.

For private/on-premises deployments, use OpenAI Secure MCP Tunnel according to the current OpenAI documentation. The installer creates `/etc/ai-server-agent/mcp.authorization`, containing the complete `Bearer ...` header value for a trusted local tunnel client. Keep that file readable only by the service identity that needs it and never copy the token into source control or shell history.

## Validation before publishing

Keep the ChatGPT app private/draft until all release gates are complete. At minimum, scan tools and validate a read-only call, `run_command`, one explicitly confirmed root action, and browser capability through the real ChatGPT connection.
