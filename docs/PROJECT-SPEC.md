# AI Server Agent — Project Specification

**Status:** Owner-approved durable product definition
**Authority:** This is the canonical product-level specification. See [Architecture](ARCHITECTURE.md) for technical boundaries and [Developer Runtime](DEVELOPER-RUNTIME.md) for capability design.

## 1. Product Definition

AI Server Agent is a **lightweight, extensible, AI-client-neutral remote execution and development environment** for authorized Linux hosts.

It gives AI clients practical access to capabilities normally absent or restricted in chat environments, so they can perform real software development, system operations, testing, debugging, diagnostics, validation, and recoverable engineering workflows.

The Agent supplies **capabilities and execution infrastructure**. It does not replace AI reasoning, project governance, Git/GitHub, human authorization, or the project's own specifications.

## 2. Mission

Reduce environmental and tooling barriers that prevent AI systems from completing reliable engineering work. An authorized AI should be able to inspect, modify, execute, test, debug, diagnose, and validate real systems with capabilities comparable to an engineer operating that same authorized environment.

**Design objective:** maximize practical AI capability per unit of permanent complexity, preserving simplicity, performance, maintainability, and room for justified future extensions.

## 3. Users and Supported Environments

The intended users are developers, technical operators, AI-assisted engineering teams, and owners of dedicated development or validation servers.

The current supported platform commitment is Ubuntu 22.04+ and Debian 11+, on amd64/x86_64 and arm64/aarch64, with systemd. The [architecture](ARCHITECTURE.md) and tested release/installer matrix own platform implementation details. Support for new platforms requires evidence and explicit acceptance, not an implicit promise.

Multiple projects may share an Agent/host. Their Git/worktree identities and recoverable runtime operations must remain distinguishable. Shared workspace labels and a common worker UID do **not** imply strong tenant isolation.

## 4. Required Outcomes

### 4.1 Host access and operations

Provide authorized normal and privileged command execution, file and process operations, service control, environment preparation, networking/system diagnostics, and access to relevant Linux tools. Preserve practical root-capable escape hatches when authorized; do not attempt to replace every shell use case with a separate narrow API.

### 4.2 Software development

Enable repository discovery and recovery, isolated worktree workflows, search, source editing, semantic code intelligence, debugging, builds, tests, terminal use, and ordinary Git/GitHub development flows. Where independently authorized tools and credentials are available, AI work should be able to progress from repository inspection and changes through version control, push, review/CI evidence, and a release-ready handoff without requiring the Agent to become a project manager or GitHub credential broker. Prefer standards and mature native tools. Add first-class structured Agent functionality where it materially improves correctness, semantic fidelity, state ownership, recoverability, or AI usability.

### 4.3 Real validation and diagnosis

Enable realistic application, service, browser, database, integration, fault-reproduction, performance, and recovery tests on authorized hosts. A Validation environment must retain sufficient tools to test AI Server Agent itself; its declared purpose must not make the host incapable of the job.

### 4.4 Recoverability and authoritative project truth

An AI/client must be able to identify server and capability state, determine the actual repository/worktree/HEAD and relevant recoverable operations, and continue after chat or transport loss within the guarantees of each execution mechanism.

Git/GitHub remain the sources of truth for code, project specifications, unresolved work, commits, PRs, reviews, CI, and releases. Agent-owned state is limited to protected operational state needed for reliable execution and recovery; it is not a second project-management database.

## 5. Client and Connection Independence

**Direct AI-client connection is a first-class standalone use case.** An intermediary such as MCP Gateway is an optional integration, never a prerequisite for using, developing, releasing, or managing the Agent.

MCP is the initial common protocol; the core is AI-provider/client neutral. Future provider/protocol adapters should reuse existing Agent capabilities and authorization semantics rather than introduce duplicate execution stacks. Provider compatibility claims require observed client-side evidence.

## 6. Authority and Security Without Artificial Disability

Authorized capability must remain broad: legitimate user-directed engineering work should not be arbitrarily prohibited under a generic claim of security.

Security comes from real authentication, authorization, explicit scope, trusted execution boundaries, bounded resource use, secret protection, operation-sensitive approval, lifecycle safety, and honest audit/diagnostics—not from removing useful capabilities.

Risk warnings and documentation inform operators but do not replace controls required for correctness, credentials, or integrity. Untrusted workspace/repository content must not silently execute through actions described as passive inspection. Privileged shell and privileged file access remain genuinely root-capable and are never advertised as a sandbox.

## 7. Resource Economy and Performance

Keep the persistent control-plane footprint small in CPU, RAM, disk, services, ports, dependencies, background work, and global host configuration. Preserve host capacity and ports 80/443 for user/AI workloads by default.

Operational concurrency, timeouts, retention, and resource budgets should have suitable defaults and be operator-adjustable or resource-aware where justified. Avoid arbitrary, fixed product-level ceilings unrelated to technical constraints. Finite protocol, security, and system-integrity bounds may remain mandatory. Provide useful capacity errors rather than silent queuing, unbounded memory growth, or false success.

Optimize from representative evidence instead of speculative complexity.

## 8. Simplicity and Future Extensibility

Prefer a small number of reusable primitives for execution, state ownership, worker/root boundaries, terminal/jobs, file operations, audit, and protocol integration.

Use mature external CLIs, language servers, debuggers, and browser tools directly when suitable. Specialized runtimes should generally be optional and independently discoverable. Add a narrowly scoped extension point only when an actual capability needs it. Do not build a generic framework, daemon fleet, plugin marketplace, scheduler, or resource orchestrator in anticipation of unknown future work.

## 9. Installation, Update, and Diagnosis

Installation, configuration, ordinary maintenance, troubleshooting, safe update, and repair must be understandable and reproducible, with verifiable release identities.

**Current development baseline:** new feature acceptance targets a clean first installation. Migration from *older previously installed Agent generations* is not a requirement for the active first-install workstreams. This is not permission to modify an occupied host or discard unrelated state.

**Forward requirement:** from the newly established baseline onward, supported updates must preserve relevant user configuration, credentials, workspace data, and declared operational contracts according to the applicable release policy. Do not pre-build a speculative multi-version migration framework.

Errors should identify the failed operation, likely cause, and actionable recovery or diagnostic path without exposing secrets. Agent operations must preserve unrelated services, files, listening ports, and network configuration except where the owner explicitly authorizes the change.

## 10. Environment Purpose Profiles

An instance may declare an operational **purpose**:

- **Development**: preferred host for Git/source editing, builds, debugging, and normal project development.
- **Validation**: preferred host for installing and testing candidates, realistic integration/E2E checks, issue reproduction, and gathering acceptance evidence.

A profile is **descriptive guidance, not an access-control tier, binary variant, or separate installation**. Both profiles preserve the same core MCP/root/worker capabilities and may install the same optional tools. GitHub credentials, optional tools, and server authorization are independent of profile choice.

The selected purpose and a non-secret optional server label should be discoverable through authenticated environment metadata and concise MCP instructions where clients support them. An operator can change a profile without reinstalling. A client may choose another use of a host when explicitly authorized; instructions alone do not enforce policy.

This is a **durable product requirement**, independent of whether a particular installation or client has already passed acceptance. GitHub Issues/PRs and Releases own the implementation, validation and delivery evidence; this specification does not maintain a running status ledger.

## 11. Non-Goals

AI Server Agent is not an IDE, general-purpose hosting panel, AI reasoning engine, project-management database, GitHub replacement, mandatory multi-server fleet controller, universal workflow scheduler, or broad AI framework.

It does not require every programming language, database, browser, container runtime, or toolchain to be installed with the core. It does not promise unlimited host capacity, absolute security from malicious authorized shell commands, or identical tool presentation by every AI client.

## 12. Success and Completion Criteria

The product meets its intended outcome when a supported AI client can reliably authenticate, identify the host and available capabilities, perform substantial authorized development or validation workflows, inspect actionable evidence, and recover relevant work without reliance on chat history.

Installation, updates, and failure recovery must be reproducible and diagnosable; host capability must not be unnecessarily obstructed by the Agent itself. Operational concurrency and retained state must be understandable and manageable for the supported host environment.

Representative success must be demonstrated through an authorized direct-client development workflow, a realistic Validation-host test and diagnosis workflow, and recovery of relevant in-progress work after a client/chat interruption, using exact repository/host identity. Specific workflow fixtures, negative cases and release gates belong to [Testing](TESTING.md) and their owning Issues, not to this product-level document.

A source implementation or green unit CI is not automatically an installed/client-verified release. Each shipped capability must satisfy the corresponding actual protocol, operating-system, security, integration, error, resource, recovery, and delivery acceptance required by its work item.

## 13. Specification Governance

This document alone is the **canonical product-level intent**. Changing its mission, boundaries, durable operating principles, supported-environment commitments, or completion definition requires explicit Owner approval.

[Architecture](ARCHITECTURE.md) owns technical design; [Developer Runtime](DEVELOPER-RUNTIME.md) owns detailed capability design; [Testing](TESTING.md) owns validation strategy; [AGENTS.md](../AGENTS.md) owns coding-agent conventions; [README.md](../README.md) owns user-facing introduction, installation, navigation, and usage.

Issues/PRs/checks/releases own active work, evidence, and delivery. Do **not** duplicate live status, chat handoffs, versions of optional adapters, secrets, or historical acceptance logs here.
