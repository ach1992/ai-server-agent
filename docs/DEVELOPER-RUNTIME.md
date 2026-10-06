# Developer Runtime Target Architecture

> Status: **accepted target architecture; not yet fully shipped**.
>
> Current runtime behavior is documented by `README.md` and the current implementation. This document defines the accepted Developer Runtime target that implementation Issues #54-#66 will deliver after the Phase A owner gate.

## 1. Outcome

AI Server Agent should give an authorized AI client a compact, high-leverage remote development environment on a real Linux development/test server while keeping Git/GitHub as durable project truth.

The product is **not** trying to recreate every IDE feature or wrap every developer CLI. It should make the common development loop excellent, keep uncommon work possible through general Linux/CLI/root/PTY capabilities, and stay cheap to extend when a real new need appears.

Target loop:

```text
recover GitHub/project truth
  -> select/reuse the exact repository/worktree
  -> inspect text/structure/semantics
  -> edit safely
  -> build/test/lint
  -> debug
  -> browser/E2E validate
  -> inspect diff
  -> commit/push
  -> GitHub CI/review/release
```

## 2. Value-first complexity budget

**Value before completeness. Complexity must earn its place.**

The design target is approximately: common high-value work receives first-class support; uncommon work remains possible through general tools even when it does not receive the best dedicated UX. The informal “90% first-class / 10% possible” framing is a design heuristic, not a coverage SLA.

Rules:

- prefer the smallest mechanism that materially improves AI effectiveness;
- reuse mature CLI/protocol/tooling before building custom machinery;
- promote a dedicated Agent surface only when it adds meaningful correctness, state, reliability, safety, or recurring workflow efficiency;
- preserve cheap extension seams for credible future needs, but do not pre-build hypothetical backends/providers/frameworks;
- optional capabilities should impose near-zero runtime/maintenance cost when unused;
- do not maintain two implementations of the same capability without evidence that both create material value;
- measure before escalating infrastructure or architecture.

A proposed first-class feature should answer:

1. Why is direct shell/CLI/protocol reuse not good enough for the common workflow?
2. What observable improvement justifies the permanent implementation, testing, compatibility and maintenance cost?

## 3. Capability layers

### Layer 1 — first-class high-leverage capabilities

These receive a stable Agent-facing abstraction because structured integration materially improves common workflows:

- bounded commands and persistent jobs;
- repository/worktree/workspace identity and lifecycle;
- worker-authority safe workspace read/edit;
- bounded text search;
- structural code search backed by ast-grep;
- semantic code intelligence through LSP;
- persistent interactive worker/root terminal sessions;
- structured debugging through DAP;
- browser/E2E capability backed by Playwright.

### Layer 2 — mature native/project tools reused directly

Examples:

- Git CLI;
- `rg` / `git grep`;
- advanced ast-grep CLI/rule usage;
- Go and project build/test/lint tools;
- package managers;
- `mise`, `devenv`, `systemctl`, `curl`, and similar tooling.

Layer 2 means direct reuse is currently more valuable than a dedicated wrapper. A Layer-2 tool may also be the implementation behind a narrow Layer-1 capability; ast-grep is the structural-search engine while its advanced feature set stays direct CLI.

### Layer 3 — escape hatch / uncommon tooling

Specialized tools may be installed and used through worker/root shell or PTY without a dedicated Agent API. Examples can include profilers, tracing/network tools, Incus, specialized database tooling, custom compilers, or one-off diagnostics.

Layer 3 is not a backlog. Promote from it only when repeated evidence shows first-class support would materially improve the workflow.

## 4. Source of truth and state ownership

Git/GitHub remains authoritative for durable project state:

- source code and repository history;
- Issues/PRs/review/CI;
- canonical repository documentation/specifications;
- releases and durable project decisions.

Server-local development state is transient or reconstructable.

Keep these ownership classes distinct:

```text
project workspace/worktrees
worker HOME/config
tool/build caches
Agent runtime/session state
generated evidence/artifacts
audit/log state
Agent configuration/control state
```

Project worktrees must not become the dumping ground for worker HOME state, tool caches, generated traces, or control-plane secrets.

Exact filesystem paths are implementation details. The invariant is separation of ownership and lifecycle.

## 5. Repository/worktree identity versus Git CLI

Git itself remains the normal operation engine. Do not rebuild ordinary Git verbs as Agent tools merely for completeness.

Layer 1 owns the correctness problem that direct Git CLI does not solve by itself for a stateful AI runtime: **which repository/worktree/HEAD is every other capability operating on?**

First-class repository/worktree behavior should be limited to value such as:

- discover/reuse the intended repository;
- prove remote, branch, HEAD, dirty/conflict and worktree identity;
- safely create/remove task worktrees;
- bind terminal/edit/LSP/DAP/browser work to the intended checkout;
- reconcile remote-write outcomes to the exact local commit where needed.

Ordinary `git status`, `git diff`, `git log`, `git fetch`, `git commit`, `git push`, and similar operations remain normal Git CLI unless a future structured operation demonstrably prevents an important ambiguity or data-loss class.

## 6. Code inspection ladder

Use the lowest layer that answers the question correctly:

```text
text search       -> rg / git grep
structural search -> ast-grep
semantic meaning  -> LSP
```

### Text search

Use for literal/regex occurrence discovery. Results are workspace-scoped, bounded and machine-readable.

### Structural search

Structural search is first-class because it covers a recurring gap that text search and LSP do not naturally own: AST/syntax-shape queries independent of formatting.

Selected engine: **ast-grep**.

The first-class surface stays narrow: workspace, language, structural pattern, path/glob filters, result limit, and bounded matches with file/range/matched text plus useful captures.

Do not expose the entire ast-grep CLI/rule engine through MCP. Advanced rules/codemods remain available through direct CLI.

Ctags is not a Core-v1 managed dependency. Reconsider only from a concrete non-LSP symbol-inventory need.

### Semantic code intelligence

LSP owns diagnostics, symbols, definitions, references, hover, rename and related language-aware semantics.

v1 reference path: Go with gopls. Additional language servers are added only from real consumer need.

## 7. One structured mutation path

Structured tools must not each become independent file writers.

```text
AI edit
LSP WorkspaceEdit
ast-grep replacement plan
        |
        v
safe workspace edit/apply path
        |
        v
filesystem + Git-visible change
```

The shared edit path owns:

- workspace containment;
- optimistic file/version preconditions;
- bounded edits;
- atomic per-file replacement where practical;
- multi-file preflight;
- explicit partial-outcome reporting;
- no silent Git reset/clean/stash rollback.

Direct shell editing remains an escape hatch, but first-class structured mutations converge on the safe workspace path.

## 8. Stateful session substrate

PTY, LSP and DAP share a concrete need for long-lived subprocess/session mechanics. Issue #66 owns the smallest internal executor-side substrate that prevents three separate privilege/process brokers.

It is **not** a public generic process framework.

Internal mechanics may cover only the transports needed by accepted consumers:

- bounded managed stdio, such as gopls;
- private local Unix-socket streams when a selected adapter requires/supports them, such as a Delve DAP path;
- tmux Control Mode for terminal sessions.

No public TCP listener is required merely to normalize internal transports.

Session identity is opaque runtime identity, not authority by itself. Session operations remain bound to authenticated principal/authorization context plus decision-relevant workspace/backend/generation state.

Stateful protocols can emit asynchronous events between MCP calls. Preserve bounded ordered sequence/cursor state where needed rather than exposing unbounded raw protocol streams.

## 9. Interactive terminal

Interactive terminal is first-class because prompts, REPLs, TUIs, debugger consoles and TTY-sensitive tools cannot be modeled reliably as one-shot commands.

Public Agent terminal semantics stay backend-neutral.

v1 backend: **tmux Control Mode**.

- worker PTY runs with worker authority;
- root PTY is a real root-capable session after its exact session authorization boundary;
- opening root PTY does not waive higher-level project/production/destructive-action governance;
- terminal output/input/history is bounded and secret-safe;
- Zellij remains the first reconsider candidate only under the explicit evidence/soak-test triggers recorded in Issue #60;
- native PTY is an evidence-triggered fallback if tmux cannot satisfy a concrete acceptance requirement.

Worker terminal HOME/config/cache is Agent-managed state separate from project worktrees; cwd identifies the selected project/worktree.

## 10. DAP debugging

DAP is Core v1 because debugging is expected to be a recurring workflow where structured runtime state materially outperforms parsing a human debugger console.

v1 reference path: Go + Delve/DAP.

Keep the first surface focused on the common loop:

- launch/attach where supported;
- breakpoints;
- continue/pause/step;
- stack/threads/scopes/variables;
- evaluate with explicit side-effect classification;
- stop/disconnect;
- bounded stopped/continued/terminated/output state.

Adapter-specific uncommon operations remain available through debugger CLI/PTY instead of becoming first-class API solely for completeness.

## 11. Browser and E2E validation

Browser/E2E capability is Core v1 because UI behavior, forms/navigation, console/network failures, screenshots and end-to-end validation are recurring development needs.

**Browser is the capability; Playwright is the selected implementation technology.**

Use the lowest-overhead Playwright path that gives the AI useful structured session/snapshot/ref state. Preserve `browser_run` as the arbitrary-script/advanced escape hatch.

Do not create separate browser engine/profile ownership for CLI versus scripted paths. Browser binaries, profile data, traces/screenshots and generated evidence stay outside project worktrees with bounded lifecycle and protected permissions.

## 12. Environment and toolchain reuse

Developer Runtime discovers and reuses repository intent; it does not invent a universal environment format.

Possible repository-owned mechanisms include native language manifests/lockfiles, mise, devenv, Dev Containers or Dagger when the repository already declares them.

Core v1 may use direct host execution as the only implemented execution context.

Preserve an internal extension seam so a future **real** second execution context can be added without rewriting command/terminal/LSP/DAP semantics, but do not expose a generic public provider framework or execution-context parameter before a second concrete context exists.

Agent-managed capability tooling and repository-owned dependencies remain separate ownership classes.

## 13. Optional and deferred capabilities

Not Core-v1 requirements:

- managed Incus integration — direct root/CLI use is sufficient until repeated evidence justifies integration;
- code-server — optional human visual UI only;
- Ctags — deferred fallback;
- multiple terminal backends;
- custom Tree-sitter/code-index services;
- generic tool manager/package-manager framework;
- generic environment-provider framework;
- mandatory telemetry/Redis/queue/microservice infrastructure.

These may be revisited from real usage without changing the fundamental architecture.

## 14. Capability-complete, policy-gated

Technical capability and project/action authorization remain separate.

- ordinary development defaults to worker authority;
- root capability remains available where genuinely useful;
- security should gate/authenticate/audit/bound capability rather than delete useful authorized power;
- capability availability never grants repository/project/action authority;
- the runtime does not reproduce project governance inside every low-level tool.

## 15. Public API evolution

Published MCP tool names, input schemas, output fields and decision-relevant error semantics are compatibility contracts for direct clients and integrations such as MCP Gateway.

Prefer additive-compatible changes. Material rename/removal/semantic breaks require an explicit deprecation/compatibility or breaking-version boundary.

Do not add independent schema versions to every object by default; keep versioning proportional.

## 16. Recovery

Total development-server loss must not require chat history.

A fresh supported server should be able to:

```text
install Agent
-> authenticate client
-> recover Issue/PR/repository intent from GitHub
-> clone/fetch repository
-> reconstruct required worktree
-> discover/reinstall required project/runtime tools
-> validate locally
-> continue from GitHub evidence
```

Uncommitted local work is not durable project truth. The workflow should create coherent commits/pushes often enough that useful work is recoverable without forcing noisy commits after every edit.

## 17. Implementation sequencing

Phase A documentation integrates first and receives the owner execution-boundary gate before runtime activation.

Then activate bounded dependency-aware slices rather than everything at once:

- repository/worktree identity/lifecycle;
- worker-safe workspace editing and text/structural search;
- shared stateful session substrate;
- PTY and LSP;
- DAP;
- Browser/Playwright;
- lightweight environment/toolchain discovery;
- final integrated acceptance.

Optional/deferred work is not a blocker.

## 18. Promotion rule for future capabilities

A mature CLI/tool should move into Layer 1 only when evidence shows that a dedicated Agent abstraction materially improves a recurring workflow through one or more of:

- correctness/concurrency protection;
- persistent structured state;
- materially better semantic fidelity;
- substantial reduction in tool-call/round-trip/error burden;
- lifecycle/recovery/resource behavior that direct CLI use cannot provide cleanly.

Do not promote capabilities merely because they are powerful, modern, or available.

## 19. Authoritative work items

- #52 — Developer Runtime program and sequencing;
- #53 — Phase A canonical architecture/docs;
- #54 — repository/worktree lifecycle;
- #55 — safe worker workspace operations;
- #56 — LSP;
- #57 — DAP;
- #59 — integrated acceptance;
- #60 — PTY / tmux Control Mode;
- #61 — completed technology research and final owner reconciliation;
- #62 — structural search / ast-grep;
- #63 — Browser / Playwright;
- #64 — environment/toolchain discovery;
- #65 — optional evidence-triggered Incus;
- #66 — internal stateful-session substrate.

After this document is integrated, use it for stable architecture and use Issues for active work/dependencies. Do not reconstruct these decisions from chat history.