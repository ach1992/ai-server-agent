# Reproducible workspace transfer acceptance

This runbook covers Issue #99's **worker-only, resumable binary transfer and
versioned egress**. It is an acceptance procedure, not a new MCP tool, upload
endpoint, privileged file writer, or native ChatGPT attachment integration.

## Choose the evidence boundary

| Mode | What executes | What it establishes |
| --- | --- | --- |
| Source SDK | Current Go Agent handler plus a separately built real Executor/worker helper | MCP SDK contracts, configured client fallback, transfer integrity and source regressions |
| Installed executable (opt-in) | Two separately launched processes of the **exact supplied installed binary** on disposable local-only config/state/workspace, with official Go MCP SDK client | Real compiled Agent + Executor + worker helper binary, authenticated client catalog, resumed transfer across deliberate Executor process death, bounded roundtrip, negative behavior and CPU/RSS/resource observations |
| Protected service, connected ChatGPT | Separate operator-gated work against the *already configured* Agent/Executor services and the live plugin catalog | Must be independently demonstrated; **neither automated mode below proves it** |

Both automated modes use a **same-UID unprivileged** temporary fixture.
They do **not** validate the production root-owned Executor vs aiworker privilege
boundary, installed configuration/credential-store values, service restart,
Cloudflare forwarding, server upgrade/rollback, Stable release, or direct
typed/binary attachment rendering in ChatGPT.

## Run from a clean repository

Requirements: supported Linux/Go toolchain, sufficient disk headroom for a
33 MiB upload plus staging and preparation, functioning Linux Landlock, and an
available copy of the Agent binary. No production token, root shell, plugin
change, or service restart is required.

For the ordinary source SDK acceptance:

```bash
ASA_REQUIRE_TRANSFER_LANDLOCK=1 go test -count=1 -timeout=7m -v \
  -run '^TestDirectMCPTransferSourceAcceptance$' ./internal/mcp
```

For **actual installed executable** acceptance, select a binary with known
provenance, separately establish its expected SHA256 (from a trusted release
or verified installed-artifact record) and run:

```bash
ASA_REQUIRE_TRANSFER_LANDLOCK=1 \
ASA_TRANSFER_ACCEPTANCE_BINARY=/usr/local/bin/ai-server-agent \
ASA_TRANSFER_ACCEPTANCE_SHA256=<verified-64-character-sha256> \
  go test -count=1 -timeout=7m -v \
  -run '^TestDirectMCPTransferInstalledAcceptance$' ./internal/mcp
```

The installed-mode harness uses the current source credential schema for
two unrelated direct MCP principals (`direct/default` and `direct/secondary`).
Use a binary compatible with this source revision; for an older released
binary, use the acceptance harness from its matching release rather than
claiming a failure caused by mixing versions.

The test explicitly rejects a non-absolute or mismatched binary. Its
temporary processes bind an OS-assigned **127.0.0.1** port, not the live
service address. It generates **two unrelated, temporary authenticated
principals**, stores their verifier-only credential records in private test
state, and keeps all uploaded artifacts inside a disposable test workspace.
No privileged configuration, existing service, public endpoint, or connector
catalog is changed. Temporary test resources are cleaned on exit.

Do not substitute the running public server URL or its protected token into
this test. Remote protected-service acceptance must use a separately
approved, principal-scoped client plan and verified rollback.

## Working authenticated client (no mock service)

The independently runnable MCP SDK consumer is at
[`examples/workspace-transfer-client/main.go`](../examples/workspace-transfer-client/main.go).
From a checkout of the repository, with an **already authorized** direct MCP
endpoint and workspace, execute (replace the placeholders with authorized
real values):

```bash
read -rs ASA_MCP_TOKEN
export ASA_MCP_TOKEN
go run ./examples/workspace-transfer-client \
  --endpoint 'https://YOUR-AUTHORIZED-AGENT.example/mcp' \
  --workspace '/srv/ai-workspace/YOUR-WORKSPACE' \
  --path 'binary/demo.bin' \
  --input '/LOCAL-PATH/demo.bin' \
  --output '/LOCAL-PATH/demo-returned.bin' \
  --key 'upload-demo-20261012-user-chosen-stable-key' \
  --call-timeout 2m
unset ASA_MCP_TOKEN
```

The endpoint must use HTTPS (or loopback-only HTTP for local testing);
cross-host HTTP redirects are disabled to protect the token. The token goes
through the environment and authenticated MCP transport, not the command-line
arguments. The upload requires an authorized existing workspace and a new
server file (`must_not_exist`), verifies each 256 KiB chunk and the complete
SHA256, continues a partial transfer from server `status`, then retrieves
the committed exact version in <=128 KiB windows and SHA256-verifies the
reassembled bytes. It writes a private temporary local file, fsyncs it and
publishes it only when fully verified. The destination output must not
already exist (no overwrite).

If the connection is interrupted or response lost, rerun the **same** command
with the exact same `--key`, original file and server path to reconcile the
durable offset; do not mint a new key after an uncertain commit. On failure,
the client exits nonzero and preserves the server session for safe retry or
the operator's explicit abort/TTL cleanup. Stable idempotency keys are
**public session labels**, not bearer credentials, and must obey the
Agent's 16..128 ASCII token rules. Each call has a configurable timeout;
large transfers repeat bounded calls rather than raising request frames.

The installed executable acceptance suite exercises this example as a
**separate CLI process** after preloading a first chunk in another SDK
session, and checks successful resumption, exact binary output, stable
version and safe refusal to overwrite the verified local result. Direct
ChatGPT tool catalog refresh is a separate plugin-registration issue if
the installed MCP SDK lists `workspace_transfer` while ChatGPT does not;
neither the example nor SDK `structuredContent` supplies a native ChatGPT
downloadable attachment.

## Exact user journey and failure evidence

The same SDK client verifies a 32 KiB UTF-8 file, a 5 MiB + 43-byte binary and
a 33 MiB + 13-byte binary. Binary samples contain embedded NUL/non-UTF-8
bytes, not a repeated ASCII string. Each case:

1. Discovers real MCP tools (in installed mode), begins a size/SHA256-pinned,
   destination-conditional transfer, and retries the identical begin.
2. Transfers sequential <=256 KiB base64 chunks with exact per-chunk SHA256.
   Replays an accepted chunk and verifies that progress does not duplicate.
3. Closes/reconnects the MCP client after a partial upload, calls status and
   resumes at the exact durable next offset. Installed mode additionally
   **kills and restarts its own isolated Executor** during the 5 MiB case.
4. Confirms no destination is visible during staging/preparation. Repeats
   bounded commit calls until an atomic committed receipt and exact
   `file_version` exist.
5. Pins the version, reads a small text-only-visible preview, retrieves the
   whole content in bounded 128 KiB windows, reconstructs actual raw bytes
   according to `output_encoding` and verifies the source/destination SHA256.
6. Replays commit without replacing or republishing the finished file.

Installed mode also rejects an incorrect bearer, access to another
principal's transfer, reuse of a handle from a different workspace, wrong
chunk offsets, invalid base64, wrong chunk and full-file hashes, escaped
paths/symlink parents, staging quota violation, stale `file_version` and
concurrent destination creation. It checks that failed completion never
exposes partial destination bytes and supports abort.

The existing `internal/executor/workspace_transfer_*_test.go` tests remain
authoritative for process-kill checkpoints, Landlock worker authority,
rename-exchange recovery, cleanup/GC/TTL and quota state transitions. Do not
treat this SDK test as a replacement for those lower-level crash regressions.

## Interpret the measurements correctly

The verbose test logs source SHA256, resulting file version, chunk count,
commit rounds, read windows, elapsed wall time, separate upload time, and
the number of actual **HTTP entity-body bytes** read/written by the official
SDK transport. It also reports approximately sampled Agent/Executor
subprocess RSS high-water marks at 50 ms resolution. HTTP body bytes are
**not** the same as TCP/TLS/IP bytes; process RSS does not include the worker
helper, kernel cache, or the Go test client. SDK `TextContent`/structured
JSON visibility is **not** proof that the ChatGPT model received a native
downloadable file.

An installed-mode PASS supports only that isolated executable and its
configuration. Record the exact Git HEAD/tree, binary SHA256, Linux kernel,
runtime limits, test command, output and failure cases next to Issue #99.
For a real installed service, additionally verify Agent+Executor binary
and settings identities, true root/worker separation, live client catalog,
authorized credential principal/workspace, rollback, service restart and
actual client rendering. Issue #99 owns remaining conditional native client file/media presentation.
Issue #59 is closed by Owner decision: its discontinued exhaustive test program
must not be reopened solely for additional matrix coverage. Revisit a specific
transfer gap only when a real consumer or material regression justifies
bounded validation.
