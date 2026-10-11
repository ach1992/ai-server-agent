# Developer Runtime Target Architecture

> Status: **accepted target architecture; main source slices integrated, full installed-client/Stable acceptance pending**.
>
> Current runtime behavior is documented by `README.md` and the current implementation. This document defines the accepted Developer Runtime target delivered by the relevant workstreams tracked under Issues #54-#66 and the cross-cutting data-delivery Issue #99. Phase A architecture/documentation is integrated and its execution boundary was owner-accepted on 2026-10-07; optional/deferred workstreams in that range are explicitly non-blocking below.

## 1. Outcome

AI Server Agent should give an authorized AI client a compact, high-leverage remote development environment on a real Linux development/test server while keeping Git/GitHub as durable project truth.

This entire target loop is a standalone Agent capability. MCP Gateway is an optional future upstream consumer of the same tools, not a source of required development features or a prerequisite for their implementation, direct acceptance or standalone release. Connector readiness must not freeze the Agent's additive tool evolution; prepare reusable bounded contracts and add further integration changes only when evidence requires them.

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

The initial Go read-only MCP operations (`code_definition`, `code_references`, `code_symbols`, `code_diagnostics`) require an explicit, resolved workspace, a workspace-relative `.go` source path and the exact `file_version` returned by `workspace_stat`/`workspace_read`. The executor obtains complete, bounded UTF-8 source through the existing sandboxed `aiworker` file helper and rechecks the file version after gopls responds; stale, binary or oversized source is rejected rather than silently analyzed as a truncated document. The reference adapter intentionally handles sources below 64 KiB (within its 128 KiB LSP frame budget), bounded result JSON and root-owned trusted, non-symlink system gopls installations at conventional paths, with parent directories checked for unsafe ownership and writability. The same trust validator governs the optional Delve and root-terminal tmux production executables and the capability manifest. Broader documents/providers, reuse and paginated semantic results remain explicit expansion points, not implied support. The integrated v1 cross-file location path uses a bounded first-query target discovery, exact worker-authority read snapshots and explicit gopls `didOpen` overlays for target files, followed by a confirming query and file-version rechecks. Every in-workspace location carries its own `file_version`; target edits or conflicting semantic results fail closed rather than lending the request source's version to a different file. External locations stay redacted, and this is not a multi-file atomic transaction or a claim that the output is a durable edit plan.

The sub-64 KiB Go-source adapter boundary is an **implementation-slice limit**, not the permanent product capability ceiling. It was raised after an actual 47,350-byte source in this repository failed the older 32,767-byte limit, without adding a second semantic backend or removing version checks. The 128 KiB frame and bounded result budgets remain enforced; some documents and unusually large/escaped LSP messages may still be refused explicitly. Issue #56/#59 acceptance should continue measuring larger real Go sources and prefer version-pinned bounded/streamed semantic input or a compatible adapter improvement when it materially restores useful capability; retain source-consistency and resource-safety invariants instead of silently truncating analysis or demanding a new generic indexing service.

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

Stdio-session process-group cleanup must remain tied to a **stable Linux process identity**, not a bare numeric PID/PGID after reaping. A pidfd by itself does **not** reserve a numeric PGID: the session broker now uses Linux 6.9+ group-scoped pidfd signalling where supported. Its older-kernel fallback for *direct child* adapters keeps that child waitable via `waitid(WNOWAIT)` until same-group descendants are reconciled, so the numeric group leader cannot be recycled before any fallback signal. For Delve's separate *non-child* debuggee, there is **no unsafe numeric fallback**: the production adapter and all its debuggees instead run inside a private root-owned cgroup v2 child of the executor service, with `cgroup.kill` (Linux 5.14+) as the final cleanup authority. DAP launch fails closed if the executor cannot verify and enforce that containment before transmitting the first target-producing `launch` request. Other capabilities remain available on older kernels. Generic command/PTY/LSP capabilities remain available on older supported kernels. Same-group descendants must be reconciled after ordinary adapter exit, not only after a timeout. When process identity or group-cleanup completion cannot be verified, fail closed and retain enough bounded runtime identity for explicit reconciliation rather than claiming clean shutdown. Linux pidfd support is required for that implemented stdio mode. This does not promise that stdio survives executor restart or that a descendant deliberately escaping its process group is stopped by process-group cleanup. The #66/#59 installed-runtime acceptance must validate these behaviors before claiming complete release support.

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

Worker terminal HOME/config/cache is Agent-managed state separate from project worktrees; cwd identifies the selected project/worktree. The integrated implementation provisions `worker-home` under the Agent root-controlled state directory, owned by `aiworker` with mode `0700`, and sets HOME plus XDG config/cache/data paths for stateful worker sessions. New installs use it as the aiworker account home; known legacy account-home values are updated without moving or deleting workspace data, preserving existing one-shot/job execution contracts. Missing/untrusted worker HOME fails session startup closed.

The first-class terminal surface opens an explicit worker/root tmux session and returns an opaque `session_id` plus a per-attachment `session_epoch`. All subsequent reads/inputs/resize/interrupt/close calls must preserve the authenticated principal, original workspace, authority mode and epoch. Text input is encoded as literal tmux hex key events; output is binary-safe base64, with ordered cursors, retention gaps and per-pane event identities, never an unbounded terminal transcript. `terminal_reconnect` attaches to a verified surviving tmux pane after executor restart without creating a new shell; an epoch change explicitly invalidates previous output cursors. This runtime record is protected Agent state, not Git/project truth. The **tmux Control Mode socket and server are executor-root-owned for BOTH ordinary worker and privileged root terminals**, in distinct `worker-terminals` and `root-terminals` state namespaces with private permissions. An ordinary terminal's **pane shell** drops to `aiworker` before any user command through a trusted, explicit `setpriv`/environment/worker-shell vector; tmux itself never grants direct socket authority to the shared worker UID. This prevents another principal with normal aiworker process access from bypassing Agent principal, epoch and audit checks by connecting to a shared-UID socket. Only user-approved privileged use may request the real root pane; the root terminal socket remains executor-owned and root-only.

The external `tmux` package is optional, not bundled or silently installed. Without it the terminal tools report unavailability and existing one-shot/job tools continue working. A disconnected tmux backend may require explicit reconciliation, and root-TUI/soak/host-migration claims require the separate HIGH_ASSURANCE validation recorded in Issue #60 before stable release.
Production terminal recovery across an actual Executor service restart also requires trusted root-owned systemd-run/systemctl. The Executor retains systemd's **KillMode=control-group**; it does not permit global process orphaning. Each private tmux server and its worker/root panes run in a separate transient, root-owned scope with KillMode=control-group and a hard **one-hour RuntimeMaxSec** (matching the existing session lifetime). Only the authenticated Control Mode client runs under the Executor service. Scope identity derives from the unguessable tmux generation; when the session closes, the Executor stops that exact scope before deleting the authenticated recovery record and socket. On Executor restart, it reattaches only to the original verified pane/generation and discloses the output-epoch gap. Missing systemd-run, unverifiable scope policy, ambiguous cleanup and stale backend identity all fail closed. On a dead Executor's next terminal open, expired root-owned scoped recovery records are reaped only after the exact old scope's termination is verified; a live Executor can similarly close a backend already stopped by its systemd lifetime. Concurrent terminal opens serialize persistent-record reconciliation, capacity admission, scope startup, and identity persistence under one Executor lock: eight surviving disk records block the ninth scope before launch. Before systemd-run can create a terminal scope, the Executor atomically creates/fsyncs a private root-owned **PENDING** record bound to the original authenticated principal/class, exact workspace, root/worker authority, opaque session ID, epoch, generated scope/socket identity, and one-hour expiry. It counts toward the same eight-session capacity even after an Executor crash; incomplete creation cannot silently become an orphan or free a slot. After tmux pane/generation/Control Mode verification, the same record is atomically promoted/fsynced to **ACTIVE**, preserving the original scope expiry. Verified-clean startup failures delete/fsync PENDING; unknown launch, failed policy verification, failed Control Mode or pre-ACTIVE publication retain it until verified exact-scope shutdown. A new Executor can authenticate the original owner to explicitly close a PENDING scope without granting terminal input/output, reconnect, resize or interrupt. Concurrent pending close and expiry/admission reconciliation are serialized; untrusted records fail closed rather than being pruned speculatively. Deterministic fresh-Executor tests cover uncertain launch, policy verification, pre-ACTIVE failure, capacity, authority, expiry, safe close, no-clobber promotion and retry; separately gated disposable-host tests prove real systemd scope lifecycle. A bounded opt-in root-only test on a disposable development host exercises a **real separately owned systemd broker-unit restart**; normal CI never restarts host services just to execute tests. See Issue #60 for the outstanding installed-client and security review gates.


## 10. DAP debugging

DAP is Core v1 because debugging is expected to be a recurring workflow where structured runtime state materially outperforms parsing a human debugger console.

v1 reference path: Go + Delve/DAP.

The **integrated PR #98 reference implementation** adds a first-class `debug_*` MCP loop with Delve `dap` in **worker-only `exec` mode**. `debug_launch` accepts an existing regular executable inside an exact resolved workspace/worktree and a matching `workspace_stat` `file_version`; the operator builds debug binaries explicitly through the normal worker command/job path. `debug_adapter_status` reports optional administrative Delve availability (`/usr/bin/dlv` or `/usr/local/bin/dlv`) without automatically installing it. Once launched, the client can set version-pinned source breakpoints, complete `debug_configure`, inspect threads/stacks/scopes/variables, continue/pause/step, inspect exception information when supported, evaluate explicitly authorized expressions and terminate/inspect status. Attach, elevated debugging and DAP reverse `runInTerminal` requests are **not supported by the initial reference path**; unusual workflows stay available through separately authorized terminal/CLI tools.

The existing executor-owned #66 broker owns Delve process identity, principal/workspace authorization, audit, session expiry and cleanup. The original adapter's live pidfd and the debuggee's own pidfd plus `/proc` UID, parent, executable and process-group provenance are verified for identity. Crucially, neither process event delivery nor process-group signalling is the **final DAP cleanup proof**: the root-owned cgroup membership is enforced before launch and `cgroup.kill` proves all contained processes are gone even when Delve dies before reporting a debuggee PID. This also covers descendants that change process groups but remain in their inherited cgroup; escaping a root-owned cgroup is outside worker authority. Delve dials *into* a dedicated private executor-owned Unix socket; no new public network port or parallel session broker is introduced. Its peer PID and UID must match the already-launched adapter. DAP events have bounded in-memory retention and dropped-event disclosure, messages/results are size-checked, and source paths outside the workspace are redacted rather than becoming file-read authority. Source `file_version` changes after breakpoint registration are rejected instead of returning potentially misleading results. A malformed/repeated DAP process identity poisons the session, and an unknown post-send DAP result blocks further action commands until stop/reconciliation. Event queues are acknowledged only after the final bounded response has been successfully constructed; rejected output does not silently drop events. Evaluate is **potentially side-effecting** and must never be silently treated as a read-only operation or blindly retried. Debug sessions/outputs remain runtime evidence, not durable GitHub state, and raw expressions/debuggee output are not audit content.

Real *isolated non-production* Go/Delve integration, breakpoint, stack/variables/evaluation, explicit stop, expiry-driven cleanup and Delve SIGKILL crash cleanup have been exercised against the integrated implementation's earlier candidate and disposable-host fixtures. A crash probe first demonstrated that the debuggee can survive in its **own** process group after Delve exits. The broker now also creates an executor-owned cgroup before launch, independently of optional DAP process events. A host-gated, genuine Linux cgroup-v2 root test has verified that after a disposable adapter receives a launch instruction, spawns a **new process-group** child and is SIGKILLed before any process event, cgroup.kill removes the live child and verifies empty containment. The earlier Delve breakpoint/evaluate/TTL/SIGKILL acceptance tests remain separate worker-fixture evidence. A previously installed direct MCP client also passed a normal Go/Delve breakpoint/step/variables/exit workflow; that evidence does not prove updated-source exceptional-failure paths, every supported host policy or Stable delivery. Component cleanup tests and previous installed happy-path smoke are **partial evidence**, not independently validated current-release support: adapter/debuggee crash races, broader stepping/exception variants, exact-candidate installed failure/recovery checks, tool provisioning, any newly invalidated high-assurance gates and program-level #59 acceptance still require separate evidence. CI without Delve skips the explicitly optional real-adapter fixture; green generic CI does not imply the full debugger acceptance.

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

The main source also provides `browser_e2e`: a compact one-call ordered Browser workflow using the **same** managed `browser_run` process, profile and admission/budget boundary, with accessible snapshots, semantic role/CSS interactions, assertions, and bounded console/network events. A complete and stable snapshot can provide bounded, exact-element `eN` refs usable by later click/fill/assert steps in **that same call**. Issuance is conservative: it binds document/root mutation identity across DOM preflight and two matching accessibility snapshots, uses role-filtered accessible elements with exact order/header agreement (including ARIA ownership reordering), verifies that each final accessibility role/index resolves to the **same DOM node** as its staged ElementHandle (also rejecting transient CSSOM accessibility swaps that revert), omits raw DOM hint metadata, and fails closed when generation, identity, uniqueness or visibility cannot be proven. Dynamic or shadow scopes may return `refs_unavailable` instead of ambiguous handles. Refs fail when invalid, detached or navigated away from, and are never durable handles. It adds no CLI daemon, browser engine or second profile. This **one-call tool** does not provide across-calls refs, managed session lifecycle or retrievable binary traces; those require the separate `browser_session_*` controls or #99 binary-delivery work and have independent acceptance gates. The existing `browser_run` free-form script remains available for advanced cases.

The source-level managed cross-call Browser path is exposed through `browser_session_open`, `browser_session_flow`, `browser_session_status`, `browser_session_close` and the opt-in `browser_session_capture`. Its process is owned by the existing executor stdio/pidfd broker, scoped to the authenticated principal and exact worker workspace, and shares the same pinned Chromium engine and persistent profile with `browser_run`/`browser_e2e` under an executor-side exclusive admission lease. A session returns an opaque ID (not an authorization token), and a verified snapshot returns exact ElementHandle `eN` refs usable on a subsequent call. Navigations, new snapshots and detached/inaccessible elements invalidate old refs; numeric ref IDs are never reused during the bounded session. The worker has a one-hour maximum lifetime and a bounded number of calls, input and output. Its Browser subprocess stops on close/expiry; uncertain action or cleanup outcomes fail closed. The Browser profile's executor-owned lease is retained with its addressable broker identity across abnormal worker exit and audit failures; generic broker garbage collection cannot erase that state before verified cleanup. A verified close releases the lease even when the close-completion audit itself becomes degraded (reported separately). `browser_status` probes this authoritative lease, reporting reserved while a session is open and failing closed as unavailable if executor admission cannot be checked. A known failed assertion/action is reported as top-level failure with the bounded result and session ID; only an unknown outcome poisons further actions. No arbitrary JS or independent profile is exposed through these new methods. A disconnected client may reconnect while the *same executor* retains the session; executor restart does not promise survival. Existing `browser_e2e` refs remain strictly one-call only. The optional screenshot consumer `browser_session_capture` uses the existing authenticated principal-bound worker and broker, returns a viewport-sized JPEG up to **32 KiB raw** through bounded in-memory frames, with SHA-256/size validation before any result can be marked successful. A client with image support receives one typed MCP `image/jpeg` item only after full JPEG decoding succeeds (not just header/size/SHA checks), and short metadata with the digest; raw base64 is omitted from structured metadata in this mode. For clients without visible image content, the explicit `representation=base64` mode exposes the image only when its raw size is at most **16 KiB** to stay inside the 32 KiB text fallback; otherwise it reports `too_large`, with controls to reduce quality and width. Overlarge screenshot errors do not poison the session; lost, malformed or partial capture frames do. Screenshot capture allows in-flight CSS animations to continue naturally; it never fast-forwards finite transitions or intentionally triggers their `transitionend` handlers. Captures are opt-in, bounded to a **left-edge crop of the currently scrolled viewport** no wider than 1024 CSS pixels and no taller than 720 CSS pixels (they do not resize or provide a full-page screenshot), do not write named screenshot artifact files or audit payloads, and do not confer root or cross-principal access. This is NOT a general screenshot/artifact storage/retrieval service. A separate opt-in source-level `browser_session_trace` action from #99 records exactly one bounded managed Browser flow with Playwright tracing, retains at most one 512 KiB ZIP in its principal/workspace-bound private session worker memory, and returns ZIP metadata plus a pinned SHA256. An explicit `read` returns at most 8 KiB raw bytes per base64 window with checked per-window integrity, offset, next offset and EOF; `discard`, replacement, verified close, expiry or worker exit ends retention. Normal recording finalization removes temporary ZIP files, and a later exclusively admitted session reclaims this feature's own abandoned trace directories after a crash. There is no public URL, second object store or generic MIME conversion service. The 30-second recording watchdog bounds the deliberate capture window; oversized traces fail explicitly after flow execution and must not cause automatic action replay. This first consumer is NOT a general-purpose large artifact API: files above 512 KiB, lower-context complete ZIP ingestion, broader non-text types, crash/restart retention guarantees and confirmed installed ChatGPT client behavior remain #99/#59 acceptance. Source/pinned-runtime proof and earlier direct-client Browser session/ref/JPEG-base64/ZIP acceptance cover different identities and behaviors; #59/#63 still require current-build lifecycle/rollback evidence, model-visible typed content validation and final release gates before declaring full acceptance.

The pinned Playwright package currently exposes its own compact CLI and can prove snapshot/ref workflows without a second runtime or npm dependency. Bare CLI defaults to system Google Chrome; select the Agent-pinned Chromium binary explicitly. Direct worker CLI sessions are an **acceptance/consumer probe**, not automatically an Agent-managed first-class Browser operation: their detached daemon, admission, profile lifecycle, disk retention and model-visible binary artifacts must meet the existing #42/#63/#99 contracts before promotion. Avoid committing CLI-generated state into worktrees; do not silently assume CLI sessions and browser_run share browser state.

## 12. Environment and toolchain reuse

Developer Runtime discovers and reuses repository intent; it does not invent a universal environment format.

Possible repository-owned mechanisms include native language manifests/lockfiles, mise, devenv, Dev Containers or Dagger when the repository already declares them.

Core v1 may use direct host execution as the only implemented execution context.

Preserve an internal extension seam so a future **real** second execution context can be added without rewriting command/terminal/LSP/DAP semantics, but do not expose a generic public provider framework or execution-context parameter before a second concrete context exists.

Agent-managed capability tooling and repository-owned dependencies remain separate ownership classes.

### Managed external tooling

When a first-class Agent capability needs an external tool such as tmux, gopls, Delve, ast-grep or Playwright:

- first detect/reuse a compatible repository/host-provided tool when that preserves the accepted contract;
- when the Agent manages installation, use an explicit supported source and record decision-relevant tool identity/version;
- pin versions when compatibility/reproducibility requires it and verify integrity/provenance where the upstream distribution supports it;
- check supported OS/architecture before activation;
- give Agent-managed installs explicit update and cleanup ownership;
- account for license/notice/redistribution obligations when binaries or packages are bundled/redistributed;
- never make an unverified floating `curl | root-shell` installer the durable managed-tool contract;
- keep capability-tool versions out of product repositories unless the repository itself owns that dependency.

Do not build a generic tool-manager framework in advance. Share acquisition/version/provenance mechanics only after concrete managed tools demonstrate stable repeated behavior worth extracting.

### Capability discovery

A fresh AI client should be able to learn which optional Developer Runtime capabilities are available without repeated blind trial-and-error.

Prefer extending existing Agent environment/status surfaces instead of creating a heavyweight capability registry. Expose only bounded decision-relevant facts such as:

- capability available/unavailable;
- relevant tool/adapter version;
- setup required/degraded/ready state;
- supported mode/capability summary when needed for tool selection.

Capability discovery is descriptive; availability never grants authority.

### Environment and secret propagation

Developer subprocesses receive only the environment appropriate to their execution identity/workspace.

- root/control-plane bearer credentials and protected Agent secrets are not ambient worker/terminal/LSP/DAP/browser environment;
- project/runtime secrets, when legitimately required, use an explicit protected mechanism rather than being written into Git, ordinary audit records, or tool results;
- worker HOME/config/cache remains separate from project worktrees;
- secret values are not returned merely to make environment status discoverable.

### Generated evidence and artifacts

Browser traces/screenshots, debug logs/output, profiles, test artifacts and similar generated evidence are runtime artifacts unless intentionally promoted into project truth.

- store them outside project worktrees by default;
- use protected permissions appropriate to their potentially sensitive content;
- bound size/retention and cleanup ownership;
- retrieve large/binary artifacts through a bounded artifact/file path rather than embedding unbounded base64/text in MCP responses;
- do not copy raw artifacts into audit logs;
- a local artifact path is runtime identity, not durable GitHub project identity.

### AI-facing bounded data delivery

Source-byte limits, encoded MCP response limits, and the AI client's usable context are three *different* budgets. The Agent must not assume that a successful large tool response means the model can see or interpret it. Issue #99 owns implementation and client-facing acceptance of this concern; #42's completed resource-governance contract remains authoritative for existing bounds.

| Need | Preferred presentation |
| --- | --- |
| Small, focused text or structured facts | Compact inline content with source identity, type/encoding, and completeness metadata. |
| Medium or searchable content | Prefer metadata-first workspace_stat on readable worker files, then a deliberately small workspace_read window with matching file_version or scoped search/selection. File size is not MIME detection or content interpretation. |
| Large files or generated evidence | Keep data at the source; return bounded metadata/preview and use safe, authorized ranged access or an expiring artifact handle only where a concrete consumer justifies one. |
| Binary, images, PDF, archives, audio or other formats | Represent the type accurately; use protocol-native typed content or a resource/optional extraction mechanism only when supported and authorized. Transporting base64 bytes is not equivalent to AI understanding a document or image. |
| Streaming logs, PTY/LSP/DAP and browser events | Bounded incremental output with explicit cursor/sequence, retention gaps, truncation and reconnection semantics; never imply dropped bytes can be replayed. |

Keep existing worker/root authority, bounded resource admission, per-principal authorization, workspace confinement, object-version checks, audit/retention and separate generated-artifact lifecycle. Current inline/file/JPEG/trace budgets describe implemented reference slices, **not permanent size restrictions on authorized useful work**. Prefer configurable or capability-appropriate bounded windows, continuation, version/integrity checks and evidence-driven ceilings when larger practical transfers are justified; do not turn resource governance into denial of an otherwise authorized capability. Prefer extending existing data/file paths, not adding a universal parser, object store, public download endpoint, alternative file writer or speculative upload framework. Any later binary-safe resumable upload must have a real consumer plus explicit integrity, admission, atomic publication and partial-completion controls.

The current MCP implementation sends tool results through `structuredContent` and, when the serialized text fallback would exceed its administrator-configured `text_fallback_bytes` budget (32 KiB by default), omits raw payload bytes from that fallback. When a client does not show structured content, the fallback must still provide bounded continuation/retention metadata and an actionable smaller read size; it must not silently claim that the AI saw the payload. This alone does not solve typed-artifact presentation, client token limits or missing historical job-log bytes. Issue #99 and the final integrated #59 acceptance must validate those boundaries using real direct MCP clients.

### Operator-managed runtime timeouts and model-text visibility (Issue #118)

The initial operator-tunable subset is stored in the existing protected `config.json` under `runtime` and takes effect after an Agent **and** Executor restart, not on live unverified hot reload. Defaults preserve the installed previous behavior if `runtime` is absent. Supported keys, units, min/max and current configured values are available from `ai-server-agent-manage runtime-show`. Administrators can use `runtime-set <key> <integer>` and `runtime-reset <key>`; the root-managed lifecycle lock, exact installed Go config validation, restricted owner/mode, staged replace and local post-restart health/rollback prevent silent invalid configuration changes. A failed recovery is explicitly reported rather than declared safe. Operators must run connection-affecting changes from a separate SSH/console session.

Initial knobs are `http_read_header_timeout_seconds` (default 10), `http_idle_timeout_seconds` (90), `command_timeout_seconds` (300), `workspace_file_timeout_seconds` (30), and `text_fallback_bytes` (32768). HTTP header/keep-alive idleness is **not** an MCP credential lifetime, root policy override, durable job lifetime or managed terminal/browser session expiry. Longer commands should use `start_job`. The MCP text fallback limit is a **serialized visible-text budget** separate from `structuredContent` and the model's actual context; increasing it never proves model visibility of an arbitrary output. The per-call 8 MiB MCP/Executor framing, file write/read and browser artifact slices remain bounded resource guards; Issue #99 Rev 3 owns safe end-to-end large/binary transfer through chunking/retention rather than raising global input frames. Further knobs should be added only where user-visible demand and coherent cross-component bounds justify them.

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

Phase A documentation is integrated and the owner execution-boundary gate was accepted on 2026-10-07. Runtime work now follows the current dependency contracts and value-first sequencing in Issue #52 rather than waiting on another Phase A gate.

Activate bounded dependency-aware slices rather than everything at once:

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
- #66 — internal stateful-session substrate;
- #99 — AI-facing bounded data delivery and typed-artifact acceptance.

After this document is integrated, use it for stable architecture and use Issues for active work/dependencies. Do not reconstruct these decisions from chat history.
