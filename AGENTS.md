# Agent Engineering Rules

This repository builds a privileged MCP control plane. Changes must preserve the following invariants:

1. The core agent must not depend on or reserve nginx, Apache, PHP, MySQL, Docker, Node.js, Python, aaPanel, or ports 80/443.
2. Runtime host dependencies must stay minimal and non-conflicting. Optional capabilities belong under `/var/lib/ai-server-agent/runtime` when practical.
3. The public MCP process must remain unprivileged. Root execution belongs behind the local executor socket.
4. `AI_ENVIRONMENT.json` and `agent_environment` must accurately describe control-plane dependencies and protected resources.
5. Host-wide or destructive actions must remain auditable and connectivity-risk actions must require explicit approval.
6. Long-running jobs must survive ChatGPT/MCP disconnects.
7. Support Ubuntu 22.04+ and Debian 11+ on amd64 and arm64.
8. Never commit credentials, generated tokens, browser profiles, job output, or server-specific state.

Validation before merge: `gofmt`, `go vet ./...`, `go test -race ./...`, static build, and `bash -n` on shell scripts. Clean-VM installation tests are required before a stable release.
