# Connect ChatGPT Business

ChatGPT Business supports custom MCP apps in Developer Mode. AI Server Agent defaults to `127.0.0.1:3210` so the privileged control plane is not exposed directly to the Internet. MCP requests still require a bearer token on loopback; this prevents arbitrary local project/browser code from calling root-capable tools directly.

Recommended topology:

```text
ChatGPT Business -> Secure MCP Tunnel -> bearer-authenticated 127.0.0.1:3210/mcp
```

1. Install AI Server Agent in `local` bind mode.
2. Confirm `curl http://127.0.0.1:3210/healthz` succeeds on the server. Health remains unauthenticated for local service checks.
3. Configure OpenAI Secure MCP Tunnel for the server/private network according to the current OpenAI documentation.
4. Configure the tunnel-client MCP binding for `http://127.0.0.1:3210/mcp` and inject the protected local Authorization header. Current tunnel-client configuration supports static MCP headers whose values are read from a file. The installer creates `/etc/ai-server-agent/mcp.authorization`, containing the complete `Bearer ...` header value and readable only by root/`aiagent`.

Example tunnel-client YAML fragment:

```yaml
mcp:
  server_urls:
    - channel: main
      url: http://127.0.0.1:3210/mcp
  extra_headers:
    Authorization: file:/etc/ai-server-agent/mcp.authorization
```

Run tunnel-client under a service identity that can read that protected file; do not copy the token into shell history or checked-in configuration.

5. In ChatGPT Business, enable Developer Mode, choose the configured Tunnel connection, and create the custom MCP app.
6. Scan tools and keep the app in draft while validating root, filesystem, job, browser, and approval behavior.
7. Do not publish the app workspace-wide until clean-server testing is complete.

Public bind mode exists for advanced deployments behind an existing secure tunnel or TLS gateway. Bearer authentication remains enabled, but plain public HTTP is not considered safe. The installer intentionally does not add a reverse proxy because doing so would violate the non-interference contract and can conflict with hosting panels such as aaPanel.
