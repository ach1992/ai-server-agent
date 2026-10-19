# Developer Runtime Target Architecture

**Authority:** [PROJECT-SPEC.md](PROJECT-SPEC.md) is the Owner-approved product-level north star. This document owns capability-specific technical design and defer/acceptance boundaries; [ARCHITECTURE.md](ARCHITECTURE.md) owns host, security and lifecycle architecture. Current integrated and released behavior must not be inferred from target architecture alone.

> **Status and evidence:** This document records accepted capability architecture, not a live release or installed-server status ledger. Verify implemented/integrated work against current source and the owning [GitHub Issues](https://github.com/ach1992/ai-server-agent/issues) and PRs; [Releases](https://github.com/ach1992/ai-server-agent/releases) own shipped versions. [Issue #59](https://github.com/ach1992/ai-server-agent/issues/59) records accepted practical workflow evidence and the decision not to run an exhaustive fault matrix, [#137](https://github.com/ach1992/ai-server-agent/issues/137) records accepted shared-runtime verification, [#99](https://github.com/ach1992/ai-server-agent/issues/99) owns conditional data/client delivery, and [#140](https://github.com/ach1992/ai-server-agent/issues/140) owns multi-language LSP/DAP. Phase A architecture was accepted; runtime integration and installed evidence remain independent.

## 1. Outcome

AI Server Agent should give an authorized AI client a compact, high-leverage remote development environment on a real Linux development/test server while keeping Git/GitHub as durable project truth.

This entire development loop is a standalone Agent capability exposed through its own MCP and Executor contracts. No unapproved intermediary is a prerequisite or an active delivery target. Preserve a client-neutral, bounded tool surface; add integration-specific work only after an explicit scope decision backed by a real consumer.

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

Go/gopls is the original Stable reference path. The first optional multi-language LSP slice, shipped starting with Stable v0.1.13, added verified PHP, JavaScript/TypeScript, Python, Bash/POSIX Shell, Rust, HTML, CSS, and PostgreSQL/MySQL/MariaDB SQL profiles wherever a trusted adapter is installed and the selected operation is supported. Advanced modes remain follow-up #149 and are not implied by language selection. The installed release and verified adapter readiness govern what actually works.

The initial Go read-only MCP operations (`code_definition`, `code_references`, `code_symbols`, `code_diagnostics`) require an explicit, resolved workspace, a workspace-relative `.go` source path and the exact `file_version` returned by `workspace_stat`/`workspace_read`. The executor obtains complete, bounded UTF-8 source through the existing sandboxed `aiworker` file helper and rechecks the file version after gopls responds; stale, binary or oversized source is rejected rather than silently analyzed as a truncated document. The reference adapter intentionally handles sources below 64 KiB (within its 128 KiB LSP frame budget), bounded result JSON and root-owned trusted, non-symlink system gopls installations at conventional paths, with parent directories checked for unsafe ownership and writability. The same trust validator governs the optional Delve and root-terminal tmux production executables and the capability manifest. Broader documents/providers, reuse and paginated semantic results remain explicit expansion points, not implied support. The integrated v1 cross-file location path uses a bounded first-query target discovery, exact worker-authority read snapshots and explicit gopls `didOpen` overlays for target files, followed by a confirming query and file-version rechecks. Every in-workspace location carries its own `file_version`; target edits or conflicting semantic results fail closed rather than lending the request source's version to a different file. External locations stay redacted, and this is not a multi-file atomic transaction or a claim that the output is a durable edit plan.

The sub-64 KiB Go-source adapter boundary is an **implementation-slice limit**, not the permanent product capability ceiling. It was raised after an actual 47,350-byte source in this repository failed the older 32,767-byte limit, without adding a second semantic backend or removing version checks. The 128 KiB frame and bounded result budgets remain enforced; some documents and unusually large/escaped LSP messages may still be refused explicitly. For real large-Go-source failures, use targeted evidence under Issue #56 and prefer version-pinned bounded/streamed semantic input or a compatible adapter improvement when it materially restores useful capability; retain source-consistency and resource-safety invariants instead of silently truncating analysis or demanding a new generic indexing service.

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

Stdio-session process-group cleanup must remain tied to a **stable Linux process identity**, not a bare numeric PID/PGID after reaping. A pidfd by itself does **not** reserve a numeric PGID: the session broker now uses Linux 6.9+ group-scoped pidfd signalling where supported. Its older-kernel fallback for *direct child* adapters keeps that child waitable via `waitid(WNOWAIT)` until same-group descendants are reconciled, so the numeric group leader cannot be recycled before any fallback signal. For Delve's separate *non-child* debuggee, there is **no unsafe numeric fallback**: the production adapter and all its debuggees instead run inside a private root-owned cgroup v2 child of the executor service, with `cgroup.kill` (Linux 5.14+) as the final cleanup authority. DAP launch fails closed if the executor cannot verify and enforce that containment before transmitting the first target-producing `launch` request. Other capabilities remain available on older kernels. Generic command/PTY/LSP capabilities remain available on older supported kernels. Same-group descendants must be reconciled after ordinary adapter exit, not only after a timeout. When process identity or group-cleanup completion cannot be verified, fail closed and retain enough bounded runtime identity for explicit reconciliation rather than claiming clean shutdown. Linux pidfd support is required for that implemented stdio mode. This does not promise that stdio survives executor restart or that a descendant deliberately escaping its process group is stopped by process-group cleanup. Meaningful changes to this containment boundary require focused installed-runtime evidence under #66 when the risk warrants it; do not infer exhaustive fault/soak proof from the bounded #59 acceptance.

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

The external `tmux` package is optional, not bundled or silently installed. Without it the terminal tools report unavailability and existing one-shot/job tools continue working. A disconnected tmux backend may require explicit reconciliation. The reference worker/root PTY shipped, but extended root-TUI, long-soak, failure and cross-host acceptance in Issue #60 remains separate; published source does not waive that evidence.
Production terminal recovery across an actual Executor service restart also requires trusted root-owned systemd-run/systemctl. The Executor retains systemd's **KillMode=control-group**; it does not permit global process orphaning. Each private tmux server and its worker/root panes run in a separate transient, root-owned scope with KillMode=control-group and a hard **one-hour RuntimeMaxSec** (matching the existing session lifetime). Only the authenticated Control Mode client runs under the Executor service. Scope identity derives from the unguessable tmux generation; when the session closes, the Executor stops that exact scope before deleting the authenticated recovery record and socket. Scope admission, LSP/DAP/Browser starts, and terminal reattachment use one protected capacity invariant: every surviving durable Terminal scope counts once against the configured shared `runtime.stdio_session_slots` (1–64), including across Executor restarts. The verified original owner may reattach and close the same scope even after downscaling below the number of surviving scopes; this consumes its existing reservation rather than admitting a new process slot. Already-attached terminal control clients are not counted twice. New sessions cannot bypass durable reservations, and unverifiable root-controlled registry state fails closed. These operations share the existing serialized Executor admission lock; Browser profile exclusivity remains independent. On Executor restart, it reattaches only to the original verified pane/generation and discloses the output-epoch gap. Missing systemd-run, unverifiable scope policy, ambiguous cleanup and stale backend identity all fail closed. On a dead Executor's next terminal open, expired root-owned scoped recovery records are reaped only after the exact old scope's termination is verified; a live Executor can similarly close a backend already stopped by its systemd lifetime. Concurrent terminal opens serialize persistent-record reconciliation, capacity admission, scope startup, and identity persistence under one Executor lock: a configured shared-session capacity (default eight, operator-selectable 1–64) governs how many surviving disk records block another scope before launch. Before systemd-run can create a terminal scope, the Executor atomically creates/fsyncs a private root-owned **PENDING** record bound to the original authenticated principal/class, exact workspace, root/worker authority, opaque session ID, epoch, generated scope/socket identity, and one-hour expiry. It counts toward the same configured shared-session capacity even after an Executor crash; incomplete creation cannot silently become an orphan or free a slot. After tmux pane/generation/Control Mode verification, the same record is atomically promoted/fsynced to **ACTIVE**, preserving the original scope expiry. Verified-clean startup failures delete/fsync PENDING; unknown launch, failed policy verification, failed Control Mode or pre-ACTIVE publication retain it until verified exact-scope shutdown. A new Executor can authenticate the original owner to explicitly close a PENDING scope without granting terminal input/output, reconnect, resize or interrupt. Concurrent pending close and expiry/admission reconciliation are serialized; untrusted records fail closed rather than being pruned speculatively. Deterministic fresh-Executor tests cover uncertain launch, policy verification, pre-ACTIVE failure, capacity, authority, expiry, safe close, no-clobber promotion and retry; separately gated disposable-host tests prove real systemd scope lifecycle. A bounded opt-in root-only test on a disposable development host exercises a **real separately owned systemd broker-unit restart**; normal CI never restarts host services just to execute tests. See Issue #60 for the outstanding installed-client and security review gates.


## 10. DAP debugging

DAP is Core v1 because debugging is expected to be a recurring workflow where structured runtime state materially outperforms parsing a human debugger console.

Go/Delve is the original Stable DAP reference path. The first optional multi-language slice, shipped starting with Stable v0.1.13, added only tested worker-owned DAP modes: PHP CLI, Python script/module/test, Node JavaScript and precompiled TypeScript source maps, Rust prebuilt LLDB, and local HTML/Chromium Browser. Trusted adapters and process containment are required; PHP HTTP Service Job debugging, Shell DAP and JavaScript extension-host DAP acceptance are owned by #149 and are not first-slice installed-runtime claims. HTML/CSS/SQL do not acquire generic DAP obligations.

**PHP HTTP Service Job DAP (#149 source extension; not a published first-slice capability).** `debug_launch(language=php, mode=http)` never launches or owns a web server. The caller must separately start an authorized worker `start_job(kind=service)` running one PHP 8.4 built-in HTTP server with the pinned trusted PHP/Xdebug runtime and workspace document root, on exactly `127.0.0.1:<unprivileged-port>`. Its original launch must predeclare a 32-character lowercase hexadecimal `xdebug.trigger_value` and the deterministic Unix DBGp endpoint `/tmp/.asa-php-http-<trigger>.sock`. The Executor binds this socket as root **only after** validating root-owned sticky `/tmp`; the socket stays root-owned with aiworker group-only connection permission, preventing worker pathname replacement. It binds the same authenticated principal/class and workspace to the protected started Service Job claim, exact systemd unit/InvocationID/ControlGroup/MainPID, pidfd-pinned original runner and PHP process, trusted PHP executable inode, exact PHP `-S` arguments, original loopback listener socket inode and versioned `.php` source. Knowing a job ID, trigger or worker UID alone never grants attach authority.

The existing root-cgroup-confined DAP adapter and original private Unix DBGp peer broker are independent of the Service Job cgroup. First set version-pinned breakpoints and `debug_configure`; then explicitly invoke the new side-effecting `debug_http_request`, which permits one bounded GET to the pinned localhost service and rejects redirects, other destinations and retries even after unknown completion. `debug_status` reports bounded events/metadata rather than exposing HTTP response bodies. Crucially, a Service's static `XDEBUG_TRIGGER`, PHP PID, and DBGp `fileuri` do **not** establish the identity of an individual HTTP GET. The Executor pins the original outbound localhost TCP socket before sending bytes and admits a DBGp peer only if the original PHP process owns the unique matching accepted kernel socket inode/4-tuple and no other concurrently accepted local request makes attribution ambiguous. On ambiguity, deny the debug peer; never attach an unrelated request. A terminal rejected DBGp peer also blocks later HTTP dispatch with its original typed provenance error, which remains inspectable via `debug_status`; never wait for the legitimate peer before sending the authorized request. A request whose dispatch outcome was already uncertain is not retried. `debug_stop` uses `disconnect(terminateDebuggee=false)` and cleans only debugger-owned adapter/proxy/client resources, then verifies the original service/PHP pidfd and listener still exist. Only a separately authorized `job_stop` may stop the service. No implicit PHP-FPM/Apache/production/remote attach, new arbitrary process broker, or hidden Service launch. Source support, real isolated root Service Job acceptance, independently reviewed integration, published release, installed runtime and connected-client acceptance remain **distinct evidence gates**.

The implemented Go/Delve reference path provides a first-class `debug_*` MCP loop with Delve `dap` in **worker-only `exec` mode**. `debug_launch` accepts an existing regular executable inside an exact resolved workspace/worktree and a matching `workspace_stat` `file_version`; the operator builds debug binaries explicitly through the normal worker command/job path. `debug_adapter_status` reports optional administrative Delve availability (`/usr/bin/dlv` or `/usr/local/bin/dlv`) without automatically installing it. Once launched, the client can set version-pinned source breakpoints, complete `debug_configure`, inspect threads/stacks/scopes/variables, continue/pause/step, inspect exception information when supported, evaluate explicitly authorized expressions and terminate/inspect status. Attach, elevated debugging and DAP reverse `runInTerminal` requests are **not supported by the initial reference path**; unusual workflows stay available through separately authorized terminal/CLI tools.

The existing executor-owned #66 broker owns Delve process identity, principal/workspace authorization, audit, session expiry and cleanup. The original adapter's live pidfd and the debuggee's own pidfd plus `/proc` UID, parent, executable and process-group provenance are verified for identity. Crucially, neither process event delivery nor process-group signalling is the **final DAP cleanup proof**: the root-owned cgroup membership is enforced before launch and `cgroup.kill` proves all contained processes are gone even when Delve dies before reporting a debuggee PID. This also covers descendants that change process groups but remain in their inherited cgroup; escaping a root-owned cgroup is outside worker authority. Delve dials *into* a dedicated private executor-owned Unix socket; no new public network port or parallel session broker is introduced. Its peer PID and UID must match the already-launched adapter. DAP events have bounded in-memory retention and dropped-event disclosure, messages/results are size-checked, and source paths outside the workspace are redacted rather than becoming file-read authority. Source `file_version` changes after breakpoint registration are rejected instead of returning potentially misleading results. A malformed/repeated DAP process identity poisons the session, and an unknown post-send DAP result blocks further action commands until stop/reconciliation. Event queues are acknowledged only after the final bounded response has been successfully constructed; rejected output does not silently drop events. Evaluate is **potentially side-effecting** and must never be silently treated as a read-only operation or blindly retried. Debug sessions/outputs remain runtime evidence, not durable GitHub state, and raw expressions/debuggee output are not audit content.

**Abnormal DAP recovery and status (source behavior).** After `debug_status`, the JSON output distinguishes protocol `state` from the broker's verified OS cleanup. `adapter_exited` is always present on a successful status; only when it is `true` are `adapter_exit_code` and `cleanup_verified` returned. A real `terminated` DAP event remains `terminated` after clean adapter exit; unexpected exit is `failed`; failed/unproven cleanup is `uncertain`. Passive status polling that encounters a complete-stream EOF while the broker has not yet proved cleanup reports provisional `uncertain` and blocks new actions, but never poisons a valid earlier terminal event as an unknown side-effecting command. On broker-verified exit it resolves to `terminated` or `failed`; a genuine post-send unknown control/evaluate result remains permanently `uncertain` even after clean process cleanup. Malformed or incomplete protocol frames still fail closed. After the adapter has been reaped, `debug_status` performs a bounded **inbound-only** drain of complete final DAP frames already buffered in its authenticated Unix socket, without sending any DAP request or reverse-request response. A broker-first exit cannot silently discard a buffered valid `terminated` event, and a trailing partial/malformed frame permanently poisons protocol integrity. If the per-poll frame/deadline budget ends before EOF, status reports provisional `uncertain` even when OS cleanup has been verified; a later `debug_status` resumes the bounded drain and resolves to `terminated` or `failed` only after its input is fully reconciled. A competing `debug_stop` rechecks session presence under the per-session action lock, so stale operations cannot recreate a retired session or falsely claim duplicate cleanup uncertainty. Neither a `stopped`/exception event nor absence of a DAP response by itself proves process termination. Once the adapter has exited, ordinary `debug_action` control operations reject with `debug_session_ended` or `debug_cleanup_uncertain` rather than sending an operation to a dead adapter. Query `debug_status` with the original authenticated workspace/session, then call `debug_stop`. When the broker already proved full cleanup, `debug_stop` retires the session and reports `cleanup_confirmed_adapter_already_exited` without attempting an impossible DAP disconnect. When a live broker session has already lost its passive DAP stream, stop skips the impossible `disconnect`, verifies broker cleanup and reports `cleanup_confirmed_adapter_stream_closed` (not graceful DAP acknowledgement). On unverified cleanup, `debug_stop` reports uncertainty **and preserves the original principal-bound session for reconciliation**; never blindly retry `evaluate` or relaunch on the assumption that the previous target died. A live-adapter `disconnect` that fails may still report `cleanup_confirmed_dap_disconnect_uncertain` after successful independent broker cleanup; a failure to verify broker cleanup is a distinct blocking state. No new privileged modes, process broker or escape hatch is introduced.

**DAP containment rationale:** a debuggee may escape the adapter's process group or outlive a crashed adapter, even before the adapter reports its PID. The Executor therefore establishes root-owned cgroup containment before launching the session and verifies the debuggee/descendant cleanup independently of DAP process events and protocol status. Do not infer cleanup from adapter exit or a `terminated` message alone. Representative real Delve/host fixtures previously exposed this failure class; focused regression and future adapter acceptance belong to `docs/TESTING.md` and Issues #57/#149 when the risk warrants it, not the closed #59 exhaustive test program.

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

The managed cross-call Browser uses `browser_session_open`, `browser_session_flow`, `browser_session_status`, `browser_session_close`, optional `browser_session_capture` and `browser_session_trace` within the **existing** executor broker and pinned Chromium engine. Authentication binds the opaque session ID to principal and exact worker workspace; knowing the ID alone grants no authority. One executor-owned, exclusive profile lease spans session lifetime and is not silently released by a worker crash or audit failure. A verified `browser_status` reports the lease; unknown cleanup fails closed. Snapshot element refs persist only within a valid bounded session; navigation, DOM changes, new snapshots or detach invalidate them. A known failed assertion is distinguishable from an unknown side-effect outcome; only the latter blocks further actions. An Executor restart does **not** promise Browser-session recovery, and `browser_e2e` refs stay one-call-only.

`browser_session_capture` is opt-in viewport JPEG evidence with an integrity-checked, fully decoded MIME-typed image where the client can actually receive image content; ordinary Base64 fallback is independently bounded and may require a smaller crop/quality. The implemented source guardrails include a maximum 32 KiB JPEG, fallback under 16 KiB, 1024×720 CSS-pixel viewport crop, no full-page/file store, and no synthetic CSS animation fast-forward or transition-event triggering. Overlarge capture fails without poisoning a valid session; malformed or interrupted frames have different failure semantics. Root and cross-principal paths remain forbidden.

`browser_session_trace` records one bounded managed Browser flow, retains at most one principal/workspace-bound in-memory ZIP (512 KiB cap), exposes SHA-256 metadata and integrity-checked 8 KiB ranged Base64 reads, and has a 30-second capture watchdog. Discard/replacement/verified close/expiry and controlled crash cleanup end retention; there is no unauthenticated URL, second artifact store or automatic retry of possibly mutating Browser actions. See `docs/TOOL-REFERENCE.md` for caller options and `docs/TESTING.md` for exact tests. Successful typed server output or a verified Base64 ZIP is **not** proof of native ChatGPT image/download rendering; #99 retains that conditional client-capability question.

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

Keep existing worker/root authority, bounded resource admission, per-principal authorization, workspace confinement, object-version checks, audit/retention and separate generated-artifact lifecycle. Current inline/file/JPEG/trace budgets describe implemented reference slices, **not permanent size restrictions on authorized useful work**. Prefer configurable or capability-appropriate bounded windows, continuation, version/integrity checks and evidence-driven ceilings when larger practical transfers are justified; do not turn resource governance into denial of an otherwise authorized capability. Prefer extending existing data/file paths, not adding a universal parser, object store, public download endpoint, alternative file writer or speculative upload framework. The Issue #99 worker-side staged upload has a real binary-safe consumer with explicit whole-object/chunk integrity, bounded admission, atomic publication and partial-completion handling. This does not imply native client attachment handling.

The current MCP implementation sends tool results through `structuredContent` and, when the serialized text fallback would exceed its administrator-configured `text_fallback_bytes` budget (32 KiB by default), omits raw payload bytes from that fallback. When a client does not show structured content, the fallback must still provide bounded continuation/retention metadata and an actionable smaller read size; it must not silently claim that the AI saw the payload. This alone does not solve typed-artifact presentation, client token limits or missing historical job-log bytes. Issue #99 retains conditional client-visible verification when a real direct MCP consumer exposes an unmet boundary; the broader #59 test program was closed after Owner acceptance of bounded practical evidence.

### Worker-side staged transfer (implemented; remaining #99 acceptance)

One direct-mode, client-neutral `workspace_transfer` tool accepts operations `begin`, `chunk`, `status`, `commit`, and `abort` under `aiworker` authority. Begin requires exact workspace and relative destination, a stable caller-generated `transfer_key` (16–128 ASCII letters/digits/underscore/hyphen), positive expected raw-byte size, full SHA-256 digest and mutually exclusive exact `file_version` or `must_not_exist`; identical begin retries return the existing transfer handle. Chunks carry up to 256 KiB decoded bytes as base64, exact raw-byte offset, and per-chunk SHA-256. Status provides durable `next_offset`, `prepared_bytes`, size and expiry. `commit` works through 4 MiB bounded preparation windows and returns `status=preparing` plus durable `prepared_bytes` until complete; the caller repeats commit with the same handle. The destination inode is first allocated **unnamed** (`O_TMPFILE`); its identity, initial SHA-256 state and source identity are durably journaled **before** linking its deterministic destination temp pathname. Crashes before journaling leave only an unnamed automatically reclaimed inode, while later cleanup requires exact durably pinned dev/ino identity, not merely worker Unix ownership. Unsupported O_TMPFILE/linking on a destination filesystem fails closed. The prepared byte offset and resumable Go SHA-256 hash state are then fsynced and journaled; uncheckpointed copy tails are truncated on resume. Each window checks the unchanged staged-source version and exact destination precondition. Once all windows finish, full SHA-256, final fsync and the original create/replace precondition are required before the atomic `renameat2`. No single helper timeout has to cover the whole object, and uncertain publication uses pinned inode identities, an original displaced-inode metadata digest and a durable **exchange_verified** checkpoint before cleanup. If a racing destination replacement is detected after RENAME_EXCHANGE, recovery verifies both path identities, rolls back the exchange to preserve the concurrent object and journals a terminal `conflicted` receipt; subsequent commit refuses `file_changed`, rather than republishing. The private `state_dir/worker-home/transfers` directory is owned by the unprivileged worker; two existing Landlock workspace boundaries plus the explicit staging subtree govern helper access. The ordinary worker shell is a shared Unix identity, not a separately isolated principal.

Only the resource knobs justified by this transfer are introduced: `runtime.transfer_staging_mib` default 512 (16–1048576), `runtime.transfer_active_sessions` default 8 (1–64), and `runtime.transfer_session_ttl_seconds` default 3600 (60–86400). They are operator-managed, validated and effective only on a compatible installed/restarted Agent/Executor, not globally enlarged request frames. The Executor schedules worker-only, separately Landlock-confined expiry cleanup at startup and every minute while running (also opportunistically on begin); terminal `status`/`abort`/`commit` calls retry unfinished data cleanup. Physical retained stage bytes remain quota-accounted even when the durable record has already become `aborted` or `committed`. The bounded 4096-entry receipt namespace and session-age policy prevent unbounded normal API-created state, with expiry automatically freeing capacity. Empty manifest-less interrupted `begin` directories retain the material-bound idempotency reservation until their TTL (based on the partial session directory creation time); independent GC does not erase a fresh reservation, identical retry safely recreates it, and different material is rejected until expiry. If the service is stopped or a file/workspace identity becomes unprovable, cleanup can be delayed and explicitly fails closed rather than deleting unrelated user data. Destination staging creates temporary disk demand in addition to the private staged source; free-space checks guard each bounded preparation window. Transfer receipt expiry is derived immediately before every begin/chunk/preparation/commit durable manifest checkpoint rather than a stale request-start timestamp. Source and installed Stable evidence remain distinct. Native client attachment presentation requires real client capability and targeted verification; the Owner did not require an additional exhaustive fault/resource matrix for this stage. Outgoing binary/text remains in version-pinned bounded `workspace_read` windows. Direct native ChatGPT attachments and typed artifact understanding remain conditional on actual client support and evidence under #99, not the completed bounded #59 program.

For reproducible official MCP SDK and isolated installed-binary transfer validation, including the separate runnable client, see [WORKSPACE-TRANSFER-ACCEPTANCE.md](WORKSPACE-TRANSFER-ACCEPTANCE.md). An isolated same-UID executable fixture is not equivalent to updating/restarting the protected installed services or proving native ChatGPT attachment rendering.

### Operator-managed runtime timeouts and model-text visibility (Issue #118)

The initial operator-tunable subset is stored in the existing protected `config.json` under `runtime` and takes effect after an Agent **and** Executor restart, not on live unverified hot reload. Defaults preserve the installed previous behavior if `runtime` is absent. Supported keys, units, min/max and current configured values are available from `ai-server-agent-manage runtime-show`. Administrators can use `runtime-set <key> <integer>` and `runtime-reset <key>`; the root-managed lifecycle lock, exact installed Go config validation, restricted owner/mode, staged replace and local post-restart health/rollback prevent silent invalid configuration changes. A failed recovery is explicitly reported rather than declared safe. Operators must run connection-affecting changes from a separate SSH/console session.

The previous initial knobs are `http_read_header_timeout_seconds` (default 10), `http_idle_timeout_seconds` (90), `command_timeout_seconds` (300), `workspace_file_timeout_seconds` (30), and `text_fallback_bytes` (32768). The Issue #99 transfer-specific knobs described above share this existing typed runtime configuration and lifecycle. HTTP header/keep-alive idleness is **not** an MCP credential lifetime, root policy override, durable job lifetime or managed terminal/browser session expiry. Longer commands should use `start_job`. The MCP text fallback limit is a **serialized visible-text budget** separate from `structuredContent` and the model's actual context; increasing it never proves model visibility of an arbitrary output. The per-call 8 MiB MCP/Executor framing, file write/read and browser artifact slices remain bounded resource guards; Issue #99 Rev 3 owns safe end-to-end large/binary transfer through chunking/retention rather than raising global input frames. Further knobs should be added only where user-visible demand and coherent cross-component bounds justify them.

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

**Environment purpose is not a capability or security profile:** The implemented `development` and `validation` labels help an AI choose the right authorized host, but both have the same core tool and worker/root capability surface. Their hints are independent of GitHub credential provisioning, installed language adapters, chosen workspace, connection method, and actual permission/approval checks. The authenticated `agent_environment` manifest and MCP instructions expose the selected purpose where the installed client supports them; do not create duplicate tool catalogs or per-role process/security engines. The operational role can be changed after installation, and an explicit authorized task may override the preference.

Technical capability and project/action authorization remain separate.

- ordinary development defaults to worker authority;
- root capability remains available where genuinely useful;
- security should gate/authenticate/audit/bound capability rather than delete useful authorized power;
- capability availability never grants repository/project/action authority;
- the runtime does not reproduce project governance inside every low-level tool.

## 15. Public API evolution

Published MCP tool names, input schemas, output fields and decision-relevant error semantics are compatibility contracts for clients of the supported MCP interface.

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

### Shared-host development and cold-chat recovery (Issue #137; bounded acceptance completed)

**Implemented first-install baseline:** The shared Agent design targets clean supported Linux installs, without speculative migration logic/tests for historical Agent claims, profiles or runtime state. [Issue #137](https://github.com/ach1992/ai-server-agent/issues/137) records the Owner-accepted bounded real-host and connected-client evidence. Shipped behavior still depends on the exact immutable Stable release and installed-client capability.

One Agent/Executor and shared systemd/toolchains/Playwright installation serve
multiple different projects. Different projects may have concurrent
Masters, but each individual project has only one active Master at a time;
successor chats recover from GitHub. The Agent is a bounded runtime
execution/discovery plane, not a project database or Master-election service.

| Planned minimal behavior | Reused boundary |
| --- | --- |
| Workspace-aware commands | Optional canonical existing worker CWD on `run_command` and `start_job`; default CWD remains the workspace root. All worker Command/Job/PTY/LSP/DAP paths reuse the **same private Agent-managed worker HOME/XDG cache** outside project worktrees (already present for sessions), not per-project HOME; root paths stay unchanged. Non-Git directories are valid; check Git identity only for Git-backed work and reject worker-only `workspace` on root jobs. |
| Recoverable Job records | One small protected record **for every new job**, even without caller `operation_id`; retain principal, CWD, kind, bounded status, a short required service label (optional for task) and new-baseline material idempotency; callers sharing a principal choose distinct project/task operation IDs. A verified `job_stop` persists protected `stopped` proof in the root-owned recovery claim (not a successful command exit); natural numeric exits stay `completed`, while unverified termination remains `unknown` and is not automatically replayed. Only a durable `claimed` record proves no systemd dispatch occurred and permits pre-launch reconstruction; `launching` is persisted **before** dispatch and must fail closed on unknown delivery across retries, restarts or staleness (a currently missing transient unit does not prove non-execution). Reconcile started/unit/numeric evidence without re-dispatching. Admission systemd inventory shares bounded command/pipe waits. |
| `runtime_list` | One bounded read-only snapshot for **task/service records and same-principal recoverable Terminal handles**; workspace/kind filters, active-first bounded results and honest unknown/truncation; no mutating reconciliation or raw systemd command descriptions in inventory. |
| Task versus Service | Use the **same existing `ai-job-*` systemd runner**, record/claim/log/stop path and lifecycle lock, with independently bounded admission and shared resource/disk safeguards. Services run in foreground; launch acknowledgement is not application readiness. Transient services need not restart on host reboot. |
| Isolated Browser state | Shared-profile default and one Browser admission remain. Opt-in temporary private cookies/storage with the **same pinned engine**; preserve the shared profile and verify cleanup. Use **existing `browser_status`** for readiness/busy rather than duplicating it in `runtime_list`. |

Workspace selection is operational correctness, **not** a filesystem sandbox
or tenant ACL. Multiple Masters may use the same trusted principal/worker
UID. Discover only handles permitted by the existing principal/workspace
boundary; do not expand root-terminal discovery beyond explicit privileged
authorization. Isolated browser storage does not isolate the tested app's
server-side data.

A successor chat recovers the owning Issue/PR from GitHub, verifies exact
remote/HEAD/dirty worktree, reads the bounded runtime snapshot and connects
only to a valid same-owner session before starting new side effects.
Terminal lifetime is still bounded to one hour. Unpushed work, expired PTYs
and lost-host Browser sessions cannot be reconstructed automatically.
Reuse the existing Go executor, session broker and Playwright runner;
**no replacement platform, extra daemon, new
queue/database, generic port broker, duplicate Browser or historical
compatibility matrix**.

Fresh installation is a **product target**, not authorization to erase
an occupied development host. Preserve unrelated live jobs/workspaces;
test on an authorized disposable clean host. New-baseline process-restart
recovery and finite resource lifetime remain necessary, but upgrading an
earlier installed Agent is **not** a completion gate. Bounded shared-runtime
verification was accepted in #137; the additional exhaustive #59 test program
was discontinued by the Owner, not passed. Terminal substrate remains under
#60/#66, Browser base under #63, runtime settings under #118, and conditional
typed attachment rendering under #99.

## 17. Implementation and delivery boundaries

The core Developer Runtime capabilities have been implemented in coherent slices. Future capability extensions should reuse the existing repository/workspace, broker, PTY, LSP/DAP, Browser and resource mechanisms rather than repeat the initial implementation plan. Sequence new work by a concrete consumer need, its security/compatibility boundary and the smallest representative real test.

Completed documentation/design gates and past PR sequences are **not** current prerequisites. Advanced PHP HTTP, Shell/extension-host debugging, SQL analysis and optional adapter acceptance are tracked by #149; #137 and #59 contain accepted bounded shared-runtime/workflow evidence and explicit verification limits; #99 retains conditional native client data/attachment delivery. Merged source, published Stable, installed host and actual client-visible capability require separate evidence.

## 18. Promotion rule for future capabilities

A mature CLI/tool should move into Layer 1 only when evidence shows that a dedicated Agent abstraction materially improves a recurring workflow through one or more of:

- correctness/concurrency protection;
- persistent structured state;
- materially better semantic fidelity;
- substantial reduction in tool-call/round-trip/error burden;
- lifecycle/recovery/resource behavior that direct CLI use cannot provide cleanly.

Do not promote capabilities merely because they are powerful, modern, or available.

## 19. Language-neutral adapter extensions and fresh-install acceptance

**Already agreed language coverage:** Go; PHP; JavaScript/TypeScript; Python; Bash/POSIX Shell; Rust; HTML/CSS; and SQL for PostgreSQL/MySQL/MariaDB. Do not reopen this product scope or conflate an unprovisioned optional adapter with a cancelled language. The first tested subset is delivered under completed #140; [#149](https://github.com/ach1992/ai-server-agent/issues/149) owns the agreed advanced/debugger/SQL follow-up and real adapter/arm64/client acceptance, including explicit Owner decisions where a safe debugger mode proves infeasible. No automatic installation of all adapters is implied.

**Design/delivery distinction:** The Owner selected protocol-first adapters and phased delivery. #140 owns the first independently reviewed Stable slice; #149 owns advanced modes and further adapter/client acceptance. A source candidate, PR checks, merged code, optional adapter readiness, installed MCP client and published Stable are separate facts. GitHub PRs, Issues and Releases own current status.

**Extension test:** For an adapter that already implements LSP/DAP, adding a language should normally be a bounded integration of a trusted tool, its narrow profile, and conformance fixtures. It must not require a new MCP family, file writer, process/session registry or security model. Genuine language-specific runtime differences may require a small typed launch/transport handler. Do not promise that every third-party adapter behaves identically.

| Boundary | Existing owner / extension rule |
| --- | --- |
| Public interaction | Reuse the four existing `code_*` names and the 17 existing `debug_*` names. Add only useful optional inputs (e.g. resolved language, SQL dialect, supported launch mode), preserving practical Go/default behavior; require a separately justified contract decision for a genuinely new tool. |
| Semantic protocol | One executor-private LSP client owns initialize/capability negotiation, JSON-RPC framing, request/notification demultiplexing, cancel/time bounds, position encoding, diagnostics and normalized result semantics. |
| Debug protocol | One executor-private DAP engine owns protocol sequencing, async events, typed responses and state transitions. Adapter-specific configuration, launch arguments, transport and debuggee identification stay outside the generic wire layer. |
| Runtime ownership | Reuse the existing #66 executor-owned session broker, worker identity, principal/class/workspace checks, resource admission, audit, process lifetime and verified cleanup. No second broker or public raw adapter proxy. |
| Files / mutations | Version-pin source reads and cross-file locations through #55; route structured WorkspaceEdit/rename/refactor plans through the existing safe version-precondition edit path. Neither a language adapter nor ast-grep becomes an Agent-owned second file writer. |
| Project vs Agent readiness | #64 `repository_environment` remains descriptive of **project-declared** toolchains and requirements; it is **not** the authority for Agent-managed LSP/DAP adapters. Report actual trusted Agent adapter readiness through existing `agent_environment`/selected `debug_adapter_status`-style status surfaces and reconcile with the selected workspace/language. Distinguish declared, installed, protocol-compatible, permitted, ready, degraded and unavailable; discovery never grants authority, installs tools or executes untrusted workspace code. |
| Binary/large evidence | Reuse #99 bounded metadata, versioned ranges, integrity and supported typed delivery. No independent language-specific artifact store or claim that base64 transport implies model visibility. |
| Web/UI/SQL behavior | Reuse Browser/Playwright for real DOM/CSS behavior and the existing command/job paths for explicit build/test. SQL analysis is offline by default; optional DB metadata access is separately approved, least-privilege and not generic query execution. |

**Offline SQL SELECT alias-to-column navigation:** The source-only SQL supplement also binds explicitly qualified column uses such as `a.id`, `t.account_id` or `schema.table.id` to real `CREATE/ALTER` column declarations across version-pinned files when one ordinary `SELECT ... FROM real_table [AS] alias [JOIN real_table [AS] alias (ON ... | USING (col [, ...]))]` has a provable table/alias scope (at most eight source tables). A declared table alias hides that table's unaliased name; duplicate/ambiguous aliases are not resolved. Only column tokens in supported expressions are navigation locations; unqualified `SELECT id`, projection aliases, guessed DB schema and implicit runtime casts do not become column definitions. CTEs, nested queries/subqueries, derived or table-valued sources, PostgreSQL positional table column-alias lists (`FROM accounts AS a (renamed_id, ...)`), `NATURAL`/`LATERAL`/`CROSS`, malformed or ambiguous `USING` lists, implicit comma joins, set operators, empty or incomplete `JOIN ... ON` predicates, unconsumed/unknown tokens between FROM/JOIN sources and unknown scope are conservatively excluded instead of fabricating cross-table column matches. At source/alias positions, PostgreSQL unquoted reserved and type/function-keyword tokens (including the `ONLY` inheritance modifier) are not physical table names, so the whole unsupported SELECT table/column graph is rejected; PostgreSQL quoted `"only"` and MySQL/MariaDB's legal unquoted `ONLY` relation remain navigable. A single validated SELECT scope now owns both table-level source occurrences and column occurrences: when the scope is unsupported, neither is emitted. The same dialect-aware physical relation-name guard is reused by DDL/DML, REFERENCES and index source emitters; bare DDL column names also receive context-sensitive keyword validation, while quoted identifiers and legal post-dot attributes such as `a.select` are preserved. The shared lexical tokenizer and runtime SQL execution boundaries are unchanged. For `JOIN ... USING (col [, ...])`, only a fully consumed bounded list of unique, dialect-legal bare or quoted single column names is proven (MySQL/MariaDB columns are deduplicated case-insensitively across quoted and unquoted ASCII names; ambiguous non-ASCII member equivalence fails closed; PostgreSQL preserves quoted case); the unqualified USING member has no uniquely attributable physical table, while `a.id`/`p.id` qualified column references remain source-backed. For explicit `JOIN ... ON`, only a bounded subset of complete predicates (ordinary identifier/numeric comparisons, `IS [NOT] NULL/TRUE/FALSE`, `LIKE`/`ILIKE`, boolean `AND`/`OR`/`NOT` and parenthesized groups) and valid `INNER`/`LEFT`/`RIGHT`/`FULL [OUTER]` modifiers can prove scope; malformed operator ordering, mixed join kinds, dialect-invalid bare SQL keywords at ON identifier positions and unsupported expression syntax yield no offline column graph. Legal boolean/NULL literals and quoted or post-dot keyword identifiers retain their respective expression roles. String-literal values are intentionally omitted by the shared offline SQL tokenizer, but their lexical presence is retained across every consumed FROM/JOIN source-name, qualifier, optional AS and alias token as well as inside `JOIN ... ON`; skipped strings between table-name tokens, before aliases, at clause boundaries or after a complete predicate invalidate the whole SELECT column graph. Qualified function-call names followed by `(` are not table-column references; real qualified columns elsewhere in the same SELECT remain eligible. The existing source-version, search-completeness, migration lifecycle/rename/drop, retention and bounded-result guards still apply. A qualified SELECT column reference must start with a dialect-legal bare ColId (or quoted name); PostgreSQL `ONLY.id`/`TABLE.id` cannot navigate even when an identically named quoted physical relation exists, but `a.only`/`a.select` and quoted leading names remain legal. CREATE VIEW/MATERIALIZED VIEW/TABLE AS SELECT and INSERT INTO ... SELECT physical FROM/JOIN dependencies are emitted only for a complete directly recognized SELECT body using that same proven source/ON-or-USING grammar; generic non-SELECT FROM/JOIN token scanning is forbidden. Direct INSERT VALUES/SELECT, UPDATE SET, DELETE [WHERE], and supported TRUNCATE targets now require complete bounded statement proof before either table or target-column occurrences are emitted; valid-but-unsupported richer DML is omitted instead of accepting a plausible header. The shared tokenizer counts adjacent elided SQL strings (saturated at two), so only one literal in an explicitly supported DEFAULT, VALUES, assignment, or SELECT projection context can be consumed; unconsumed constraint, predicate, index, suffix, and statement-boundary literals invalidate physical navigation. The shared whole-statement CREATE TABLE proof owns MySQL/MariaDB LIKE and ENGINE alternatives; independent no-FROM SELECT output aliases and ALTER MATERIALIZED VIEW lifecycle headers/actions must likewise consume every structural lexical gap before publishing physical identities or rename history. PostgreSQL datatype heads use reserved/type-function keyword classes in the correct grammar context, while index names and ALTER TABLE RENAME INDEX/KEY actions use dialect-specific rules. Supported VIEW relation renames and materialized-view population suffixes must be complete before publishing a source identity. For supported parenthesized CREATE TABLE declarations, the entire bounded column-list syntax (including bare column legality, physical REFERENCES names and supported constraint-key member identifiers) must pass a pre-emission gate; one invalid field invalidates the table, earlier valid-looking columns and related FK declarations atomically. Likewise, a supported multi-action ALTER TABLE with an illegal column identifier cannot publish earlier ADD/DROP/RENAME column occurrences; rename-bearing uncertainty retains migration-history fail-closed protection. A complete bounded grammar proof now gates each published CREATE TABLE column/constraint list, each ALTER TABLE action segment and each CREATE INDEX key/suffix before physical table or column navigation is emitted; unsupported prefixes, trailing keywords, mixed valid/invalid actions, and unknown rename-bearing history cannot silently produce partial evidence. The shared SELECT analysis also validates consumed qualified-name literal gaps and the supported projection/WHERE/GROUP/ORDER/LIMIT/OFFSET tail of both standalone and embedded SELECT statements; unconsumed double aliases or extra projection tokens cannot emit sources, while independent string projections and bounded qualified function calls retain their separate expression roles. Column definitions reject structural SQL keywords in the datatype position and incomplete DEFAULT expressions without suppressing supported signed/unsigned numeric or separate string DEFAULT literals. A CREATE AS query body that is not positively proven (including CTE, VALUES, TABLE, function-valued FROM or complex supported SQL expressions) never establishes an offline physical VIEW/TABLE declaration, even if the separate document-outline parser recognizes its syntax. Valid complex SQL remains valid SQL, but may lack source-backed navigation until its whole bounded grammar is supported. The check is intentionally a bounded supported subset, not full DDL semantic validation. This is not a general SQL parser, live database inspection or an assertion that migrations ran. SQLLens keeps responsibility for independently supported language-server methods and advanced unsupported expressions.

**Offline SQL relation history:** The bounded SQL source-navigation supplement is deliberately not a database/migration replayer. For ordinary `ALTER TABLE/VIEW ... RENAME TO` or `SET SCHEMA`, the previous qualified relation definition is invalidated; a moved name never inherits an invented `CREATE`. Only a subsequent explicit source-backed declaration in a provably ordered migration can restore a definition. Malformed moves and ambiguous migration ordering fail closed. Column-only renames do not invalidate their parent table/view. PostgreSQL `ALTER TABLE ONLY` applies to inheritance-scoped table actions such as column changes, not to relation-level `RENAME TO` or `SET SCHEMA`; malformed ONLY relation moves in token-history regressions exercise conservative behavior, not executable SQL grammar. PostgreSQL-only modifier syntax is rejected in MySQL/MariaDB instead of creating source-backed history; a valid unquoted MySQL/MariaDB relation named `ONLY` (or schema `ONLY`) is an identifier, not the PostgreSQL modifier. Supported complete comma-separated MySQL/MariaDB `ALTER TABLE` ADD/DROP COLUMN plus table `RENAME` clauses (optional `TO`, and MySQL's optional `AS`) invalidate the former relation definition in ordered offline history, regardless of clause order. Typed auxiliary clauses have deliberately bounded numeric-parameter validation: common `DECIMAL`/`NUMERIC` precision 1–65, scale 0–30 and no greater than precision, and conservative 1–255 character/binary lengths; other valid dialect-specific forms remain conservatively uncertain rather than fabricating history. Unquoted reserved identifiers in the bounded MySQL 8.4/MariaDB default-dialect CREATE/DROP/RENAME TABLE relation names, ALTER TABLE rename source/target and ADD/DROP auxiliary-column positions are rejected before any table lifecycle marker is emitted; complete RENAME TABLE name-pair lists are validated atomically; quoted reserved words and post-dot qualified identifiers are supported, and unquoted nonreserved ONLY remains legal. Keyword treatment is dialect/context-specific and does not attempt to model every SQL mode or future version. The MariaDB offline identifier subset conservatively rejects version-qualified reserved words across documented default-mode releases when no target server version is known, while retaining documented nonreserved DATABASE/WINDOW; bare SYSTEM immediately after MariaDB ALTER auxiliary ADD or DROP without COLUMN is rejected contextually (SYSTEM VERSIONING ambiguity); explicit ADD/DROP COLUMN system, CREATE-column and quoted uses remain legal. Unrecognized rename-bearing ALTER specifications explicitly make that SQL source graph unparseable instead of certifying an obsolete CREATE or inventing a successful rename. This remains a bounded offline subset, not full SQL grammar or proof that migrations ran.

**Minimal profile, not a plugin marketplace.** An internal static adapter profile may declare language IDs/file matches, dialect where relevant, trusted executable/version/source, supported protocol operations, safe initialization options, supported DAP launch modes, connection type, required runtime/toolchain, and capabilities that are actually verified. Ambiguous file/language selection fails explicitly; a repository must not supply arbitrary privileged launch commands or silently override the adapter executable. Profiles compose with common mechanisms; do not copy broker, audit, resource, filesystem, output or auth logic into every language implementation.

**Pin the full adapter execution chain.** Root-owned `node`, `python` or `php` alone does not validate loaded language-server packages, wheels, plugins, shared libraries, startup scripts or child binaries. Only operator-controlled, provenance/version/checksum-verified and platform-compatible launch chains may execute as Agent-owned adapters; workspace-local, worker-writable dependencies/config must not silently gain authority. Include licenses and Ubuntu/Debian amd64/arm64 artifacts in readiness checks. Optional adapters must not become mandatory Core dependencies.

**LSP correctness before adapter count.** The initial Go-only reference was constrained to `.go`, `gopls`, narrow framing and pull diagnostics. Optional profiles must support negotiated real server capabilities, UTF-16 positions unless negotiated otherwise, properly correlated asynchronous push `publishDiagnostics` as well as pull diagnostics, complete/bounded results, multi-file source-version binding, explicit unsupported operations, lost-event disclosure and fail-closed source drift. Client and server requests/notifications must not be silently dropped when relevant. Keep per-call launch initially; only add bounded session reuse/caching after representative measurements show a net benefit and no principal/workspace/cache cross-talk.

**Passive LSP must not mutate.** Versionless pushed diagnostics cannot acquire fabricated `file_version`; accept them only when correlated to a verifiably stable source snapshot, otherwise return uncertainty. Reject unsolicited `workspace/applyEdit`, automatic command/download or other execution/write requests during read-only `code_*` calls. Future user-authorized rename/refactor may translate an explicitly accepted, bounded, versioned WorkspaceEdit through #55; current multi-file preflight plus per-file atomic writes are **not** all-or-nothing transactions. Hover and rename are examples of LSP capabilities, not additional required public tools in #140.

**DAP correctness before adapter count.** The Delve reference path assumes a prebuilt Go executable, a private socket and Delve-specific debuggee provenance. A conforming DAP adapter may emit `initialized`, request breakpoint configuration, and defer its `launch` response until `configurationDone`; a sequential wait-for-launch-response implementation can deadlock. The shared engine must handle supported legal orderings, concurrent pending messages, adapter capabilities and explicit timeouts without turning on adapter-originated `runInTerminal` or unreviewed reverse requests. Script/module/test (Python), Node/TypeScript source maps, PHP CLI/request Xdebug and prebuilt Rust/LLDB need distinct, bounded launch/transport contracts. Preserve authenticated worker authority, private endpoints, pinned process identities, cgroup/descendant containment, unknown-after-send reporting and **verified cleanup even if the adapter exits before reporting a debuggee**. Delve's direct-child PID/executable assumptions are not a universal DAP contract.

**Different target owners require different verified stop semantics.** For a session-owned CLI target, the Executor must prove containment and cleanup of adapter, debuggee and descendants. For an existing *separately service-owned* test target (e.g. worker-controlled PHP HTTP service started by the existing Job/Service path), the DAP session does **not** inherit ownership of that service: bind the exact permitted request, service and private DBGp connection, detach/close/verify only its debugger resources on `debug_stop`, preserve the running service, and reserve service termination for separately authorized `job_stop`. Never `cgroup.kill` a cgroup owned by the HTTP service from DAP stop or claim an external process was reaped. Arbitrary remote, production or unverified PHP-FPM/Apache targets are not supported. Treat safe PHP HTTP request-mode feasibility as an explicit gate: unresolved provenance/detachment blocks acceptance rather than silently dropping the mode.

**Adapter-originated launch is not a back door.** DAP `runInTerminal` is unsupported by default. If an accepted adapter requires it, prefer verified self-launch; otherwise a future reviewed handler may admit only a typed user-approved worker target/argv/CWD/environment bound to the same session, audit, contained process ownership and cleanup. No arbitrary reverse-request shell, root execution, uncontained child process or automatic nested debug session. Declare unsupported capability until the safe path is implemented and proven.

**Trust is separate from protocol and from workspace identity.** A "read-only" LSP call must not execute untrusted project build scripts, Rust procedural macros, repository toolchain overrides, startup hooks, package-manager scripts or project-controlled language-server configuration. If safe semantic analysis is not possible, report an accurately degraded/unsupported capability until the action is explicitly trusted and confined. SQL must never open a database automatically or run statements including `EXPLAIN ANALYZE` under a read-only label. PHP HTTP/Xdebug and browser-extension debugging require separately accepted local/test authorization and transport boundaries; root/remote attach do not arrive through adapter capability discovery. A shared worker UID/workspace filter is **not** cross-tenant filesystem isolation.

**Shared HOME does not confer trust.** #137 deliberately shares one worker HOME/XDG among projects; never treat its worker-writable config or caches as operator adapter policy. Where a real tool requires cache scoping, use a bounded canonical workspace + adapter/version key without another HOME manager, and prove no cross-repository semantic-result or session-handle contamination. Workspace checks are logical authorization, not tenant filesystem confidentiality.

**Single-host resource economics.** One Agent/Executor and the existing job/session/browser infrastructure remain authoritative. Protocol messages need bounded per-consumer framing and backpressure; terminal output retention and lossless protocol response handling are different contracts. The existing finite broker admission budget is shared by LSP/DAP/Terminal/Browser; test heterogeneous multi-project capacity, early rejection and truthful busy/unknown outcomes rather than hiding starvation behind indefinite waits. Record CPU/RSS, process counts, time-to-first-result, dropped events and cleanup against concrete multi-project fixtures before adding worker pools, hot-session caches, parallel language servers or new knobs. Do not perform a blanket refactor of already-working Git, Browser, Jobs, Workspace or PTY tools to make them fit a speculative universal interface. Converge duplicate action dispatch or specialized transport code only when a testable real integration slice requires it.

**Fresh-install verification boundary (#137).** The Owner accepted representative first-install evidence on a supported real host and separate connected-client evidence; a single exhaustive all-capabilities/all-faults matrix was intentionally not required. This does not authorize wiping an occupied installation, relaxing Stable supply-chain checks or ignoring future versioned upgrades. Preserve authenticated MCP tool semantics, bounded runtime identity, reconnect and lifecycle behavior; revisit uncovered cases if a concrete risk or meaningful compatibility change justifies it. Immutable Releases and Operations own shipped Stable behavior; #137 preserves the accepted evidence and its limits.

**Forward acceptance for an extended language/debugging capability:**

1. Validate a narrow representative real adapter against the existing shared LSP/DAP protocols, worker/workspace identity, cancellation, private transport, bounded results and verified process cleanup. Respect async diagnostics and delayed DAP launch responses rather than assuming one request order fits all adapters.
2. For service-owned debugging (such as PHP HTTP), prove exact target/request provenance, safe detach that does not stop an independently owned service, and containment of only debugger-owned resources. If reverse-launch, Shell DAP or extension-host debugging cannot be proven safe, report it as unsupported pending a separate product decision.
3. Exercise independent project worktrees and representative failure/permission paths on disposable supported hosts. Add crash/soak/resource-pressure scenarios only where the specific change creates a material risk; keep source, published release, installed-host and actual connected-client evidence distinct.
4. Apply exact-candidate CI/Security, proportional independent review for material process/security changes, and appropriate integration/release/installed acceptance gates. #149 owns advanced capability implementation; #99 retains conditional native client artifact delivery, and completed #59 records the bounded end-to-end verification decision.

**What is deliberately excluded:** a public plugin API/marketplace, universal language runner, unconditional managed provisioning, second state broker, per-project Agent/database/UID, broad rewrite of working tools, and a guarantee that any future adapter can integrate without a small compatibility handler. Each new abstraction must demonstrate a real repeated failure or measurable integration/maintenance improvement.
