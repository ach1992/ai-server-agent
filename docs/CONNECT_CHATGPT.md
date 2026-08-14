# Connect ChatGPT Business

ChatGPT Business supports custom MCP apps in Developer Mode. AI Server Agent defaults to `127.0.0.1:3210` so the privileged control plane is not exposed directly to the Internet.

Recommended topology:

```text
ChatGPT Business -> Secure MCP Tunnel -> 127.0.0.1:3210/mcp
```

1. Install AI Server Agent in `local` bind mode.
2. Confirm `curl http://127.0.0.1:3210/healthz` succeeds on the server.
3. Configure OpenAI Secure MCP Tunnel for the server/private network according to the current OpenAI documentation.
4. In ChatGPT Business, enable Developer Mode and create a custom MCP app pointing the tunnel to `/mcp`.
5. Scan tools and keep the app in draft while validating root, filesystem, job, browser, and approval behavior.
6. Do not publish the app workspace-wide until clean-server testing is complete.

Public bind mode exists for advanced deployments behind an existing secure tunnel or TLS gateway. It intentionally does not install a reverse proxy because doing so would violate the non-interference contract and can conflict with hosting panels such as aaPanel.
