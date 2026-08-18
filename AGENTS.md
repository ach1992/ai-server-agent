# AGENTS.md

## Project scope

AI Server Agent is a Go MCP control plane for dedicated development/test servers. The public MCP endpoint is bearer-authenticated, the agent process is unprivileged, and privileged host operations are delegated to a root executor behind explicit policy/approval guardrails.

Stable v0.1 releases support **Ubuntu 22.04 LTS on amd64/x86_64 only**. `install.sh` also has a broader source-install compatibility path for Ubuntu 22.04+ and Debian 11+ on amd64/arm64; that is development/source compatibility, not a stable-release support claim.

## Non-negotiable behavior

- Preserve `/srv/ai-workspace` and the `aiworker` account during purge.
- Preserve bearer authentication and native TLS for direct public mode.
- Preserve the intentional root executor capability and its approval guardrails.
- Do not add nginx, Apache, Caddy, `cloudflared`, Docker, Node.js, Python, PHP, databases, or hosting panels as core dependencies.
- Do not make the core control plane own ports 80/443.
- Cloudflare automation is hostname-scoped and must not mutate whole-zone SSL mode.
- Never persist Cloudflare API tokens or other user secrets in repository files, logs, managed state, or test fixtures.
- Stable install/update paths must resolve to immutable published releases and must never silently fall back to `main`.
- Initial stable installation must authenticate release `install.sh` bytes before privileged execution. The supported one-line path loads `scripts/install-stable.sh` from a published immutable release tag; do not restore a direct `releases/.../install.sh | sudo bash` path or a mutable branch bootstrap.

## Privileged lifecycle

Use the installed `/usr/local/sbin/ai-server-agent-manage` entrypoint for management. Privileged install/update/manage/uninstall/purge operations are serialized by the root-only lifecycle lock under `/run/lock/ai-server-agent`; Cloudflare transaction state and rollback material live under `/etc/ai-server-agent/control`.

Do not bypass lifecycle locking in new privileged state-mutating paths. Purge may remove Agent config/state, but it must not remove the `/run/lock` namespace while another management operation is active.

## Development and validation

Primary language/tooling:

- Go 1.26.x
- Bash
- systemd on the supported Linux target

Before proposing a substantive change, inspect the relevant production path and its tests. Use the narrowest discriminating checks first, then the applicable broader checks.

Typical local checks on a compatible development host:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh scripts/install-stable.sh tests/*.sh
```

High-risk changes to privileged execution, Cloudflare recovery, installer/updater trust, or release provenance require the corresponding High Assurance Security coverage. Static/grep contracts are secondary guardrails; do not treat them as substitutes for behavioral tests of the production path.

See `docs/ARCHITECTURE.md` for trust boundaries and `docs/TESTING.md` for the current validation model.

## Change discipline

- Make the smallest coherent root-cause change; avoid unrelated cleanup.
- Prefer deleting superseded machinery over layering another workaround when guarantees are preserved.
- Keep docs aligned with current behavior, not historical remediation.
- Do not edit published release identities.
- Do not use production VPS or live Cloudflare mutation as an ordinary diagnosis/test environment.
- Do not use independent HIGH_ASSURANCE review as iterative lint while a candidate is still moving. Complete the accepted implementation and behavioral validation, freeze an exact candidate, then use independent review as an integration/release gate when required. A review finding that changes the candidate invalidates that review identity; fix the root cause, revalidate, and refreeze before another independent gate review.
- Do not request, enable, or use GitHub Copilot pull-request review as project review evidence, including for independent HIGH_ASSURANCE review. Historical Copilot review results may explain past findings only; they do not satisfy a current review gate.
- When review independent from the Master is required, the Master must prepare a ready-to-paste `INDEPENDENT REVIEW CHAT` prompt following the `github-project-orchestrator` review handoff. The user relays that prompt to a separate fresh reviewer context, person, or review tool; the returned review must identify the exact candidate SHA, state `APPROVE` or `CHANGES_REQUIRED`, and provide evidence-backed findings for Master reconciliation.
