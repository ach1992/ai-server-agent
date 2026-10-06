# AGENTS.md

## Project scope

AI Server Agent is a Go MCP control plane for dedicated development/test servers. The public MCP endpoint is bearer-authenticated, the agent process is unprivileged, and privileged host operations are delegated to a root executor behind explicit policy/approval guardrails.

The supported stable platform policy is **Ubuntu 22.04+ and Debian 11+ on amd64/x86_64 and arm64/aarch64**, with systemd on dedicated development/test servers; current `main` follows the same policy. Keep distro minimums aligned between the standalone stable bootstrap and installer, keep stable release architectures in `scripts/release-arches.txt`, and extend compatibility tests when platform support changes rather than scattering new platform conditionals across release workflows. Current release identity belongs to GitHub Releases and the README stable-install guidance rather than this durable agent policy.

## Non-negotiable behavior

- Preserve `/srv/ai-workspace` and the `aiworker` account during purge.
- Preserve bearer authentication and native TLS for direct public mode.
- Keep the Agent core AI-client/vendor neutral: ChatGPT/OpenAI-specific setup, credentials, tunnels and UI behavior belong at explicit integration boundaries and must not become executor/policy/lifecycle/security assumptions. MCP is the current primary common protocol, not an OpenAI-only contract.
- Preserve the intentional root executor capability and its approval guardrails.
- Treat arbitrary root shell as genuinely root-capable: pattern/path approval checks are defense-in-depth, not a complete sandbox or exhaustive shell-effect classifier. Do not weaken this truth in tool descriptions, Gateway integration, review, or future permission UX.
- Treat current `read_file` / `write_file` as privileged root-host-file capabilities: read can expose root-readable secrets and write can produce root-equivalent effects. Approval/path heuristics are additive guardrails, not a general sensitive-file sandbox.
- Do not add nginx, Apache, Caddy, `cloudflared`, Docker, Node.js, Python, PHP, databases, or hosting panels as core dependencies.
- Do not make the core control plane own ports 80/443.
- Cloudflare automation is hostname-scoped and must not mutate whole-zone SSL mode.
- Never persist Cloudflare API tokens or other user secrets in repository files, logs, managed state, or test fixtures.
- Stable install/update paths must resolve to immutable published releases and must never silently fall back to `main`.
- Initial stable installation must authenticate release `install.sh` bytes before privileged execution. The supported one-line path loads `scripts/install-stable.sh` from a published immutable release tag; do not restore a direct `releases/.../install.sh | sudo bash` path or a mutable branch bootstrap.
- Keep external GitHub Actions references pinned to verified full-length commit SHAs; a nearby version comment may document the intended upstream major. Verify a replacement SHA belongs to the expected upstream action repository before updating it.

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
go test -race -vet=off ./...
bash -n install.sh update.sh uninstall.sh manage.sh scripts/build-release.sh scripts/install-stable.sh scripts/ci-change-scope.sh tests/*.sh
```

High-risk changes to privileged execution, Cloudflare recovery, installer/updater trust, or release provenance require the corresponding High Assurance Security coverage. Static/grep contracts are secondary guardrails; do not treat them as substitutes for behavioral tests of the production path.

See `docs/ARCHITECTURE.md` for trust boundaries and `docs/TESTING.md` for the current validation model.

## Change discipline

- Make the smallest coherent root-cause change; avoid unrelated cleanup.
- When changing command/file/job/browser execution, do not add or preserve avoidable unbounded buffering, whole-file reads for bounded APIs, silent truncation, lossy binary/text conversion, or ever-growing logs as a convenience. Keep ordinary work low-overhead, use explicit bounded metadata, and route expected long/high-output work through persistent jobs.
- Prefer deleting superseded machinery over layering another workaround when guarantees are preserved.
- Keep docs aligned with current behavior, not historical remediation.
- Do not edit published release identities.
- Do not use production VPS or live Cloudflare mutation as an ordinary diagnosis/test environment.
- Do not use independent HIGH_ASSURANCE review as iterative lint while a candidate is still moving. Complete the accepted implementation and behavioral validation, freeze an exact candidate, then use independent review as an integration/release gate when required. A review finding that changes the candidate invalidates that review identity; fix the root cause, revalidate, and refreeze before another independent gate review.
- Do not request, enable, or use GitHub Copilot pull-request review as project review evidence, including for independent HIGH_ASSURANCE review. Historical Copilot review results may explain past findings only; they do not satisfy a current review gate.
- When review independent from the Master is required, the Master must prepare a ready-to-paste `INDEPENDENT REVIEW CHAT` prompt following the `github-project-orchestrator` review handoff. The user relays that prompt to a separate fresh reviewer context, person, or review tool; the returned review must identify the exact candidate SHA, state `APPROVE` or `CHANGES_REQUIRED`, and provide evidence-backed findings for Master reconciliation.

## Developer Runtime value-first rules

When working on Issues #52-#66, read `docs/DEVELOPER-RUNTIME.md` before designing or implementing a new capability.

The governing optimization is **maximum practical AI-development leverage per unit of total complexity**.

- Do not wrap a mature CLI merely to increase MCP tool count.
- Promote a capability to first-class only when structured integration materially improves recurring correctness, state, reliability, semantic fidelity or AI efficiency.
- Preserve general worker/root shell and PTY escape hatches for uncommon work instead of implementing dedicated APIs for every tool.
- Keep Git CLI as the ordinary Git operation path; first-class repository logic owns identity/worktree/lifecycle correctness.
- Keep the code-inspection ladder distinct: text search -> structural ast-grep search -> LSP semantics.
- Keep one structured mutation path: LSP WorkspaceEdit and structural rewrite plans apply through the safe workspace edit/precondition layer.
- DAP and Playwright-backed Browser are Core-v1 capabilities, but their public surfaces stay focused on common high-value workflows rather than protocol/API completeness.
- Keep #66 consumer-driven and internal; do not grow it into a public generic process/provider framework.
- Keep project worktrees separate from worker HOME/config/cache, runtime/session state, generated artifacts and audit/log state.
- Do not implement Ctags, managed Incus, multiple terminal backends, generic tool managers/environment-provider frameworks, custom indexes or similar deferred capabilities without new evidence satisfying their promotion rule.
- Keep README truthful about shipped behavior; target architecture belongs in `docs/DEVELOPER-RUNTIME.md` until implementation is real.

For a proposed abstraction or dependency, ask: **what existing complexity or recurring failure does this remove, and is that payoff larger than its permanent maintenance/test/compatibility cost?**
