# Security Policy

AI Server Agent intentionally exposes powerful host-control tools. Treat the MCP endpoint as equivalent to privileged administrative access.

- Use a dedicated disposable or snapshot-backed server whenever possible.
- Prefer a private bind plus a secure MCP tunnel; do not expose plain HTTP publicly.
- Never publish MCP or executor tokens.
- Do not connect the agent to a production server until you have validated its policy and recovery behavior for your environment.
- The root executor has no TCP listener; it is reachable only through a local Unix socket.
- The network-facing MCP process runs as an unprivileged service account.
- `run_root_command` is intentionally broad. The connection guard catches common destructive/self-disconnecting actions, but no string-based policy can make arbitrary root shell execution perfectly safe.

Report vulnerabilities privately through GitHub security advisories when available. Do not post working exploits or credentials in public issues.
