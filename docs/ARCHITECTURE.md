# Architecture

## 1. Supported release target

Stable v0.1 releases target **Ubuntu 22.04 LTS amd64/x86_64 on dedicated development/test servers**.

The source installer also accepts Ubuntu 22.04+ and Debian 11+ on amd64/arm64 for development/source use. That broader path is not a stable-release support promise.

The control plane intentionally coexists with normal server software. It does not require or own nginx, Apache, Caddy, Docker, PHP, databases, Node.js, Python, `cloudflared`, or ports 80/443.

## 2. Process and trust boundaries

AI Server Agent is split into two long-running services:

- `ai-server-agent.service`: MCP/API process running as the unprivileged `aiagent` account.
- `ai-server-agent-executor.service`: root executor reachable through the Agent-owned Unix socket.

Ordinary shell work runs as `aiworker` in `/srv/ai-workspace`. Root shell work is explicit and uses the same policy/approval evaluation before execution.

The public MCP surface uses bearer authentication. Direct public mode also requires native TLS. The bearer-authenticated MCP control plane is one authorization domain: root and worker jobs are different execution modes, not different external principals.

### Root command environment

Root commands must not inherit worker-controlled ambient shell state. The executor uses:

- `HOME=/root`;
- working directory `/root`;
- a minimal explicit environment and fixed command `PATH`;
- `/bin/bash --noprofile --norc -c`;
- no inherited `BASH_ENV` or `ENV`.

Worker commands retain `/srv/ai-workspace` as HOME/CWD.

### Persistent jobs

Persistent jobs run as transient systemd units and survive MCP/ChatGPT reconnects. Job metadata lives under the root-controlled state container `/var/lib/ai-server-agent/jobs`.

Job log/status files are created with exclusive, no-follow semantics. Reads reject symlinks, non-regular files, unexpected owners, and world-writable files. Root-owned versus `aiworker`-owned job files record execution provenance and protect filesystem replacement; they are not a separate bearer-auth authorization partition.

## 3. Filesystem trust model

Important paths:

| Path | Purpose / trust |
| --- | --- |
| `/usr/local/bin/ai-server-agent` | installed agent binary |
| `/usr/local/sbin/ai-server-agent-manage` | supported privileged management entrypoint/wrapper |
| `/usr/local/lib/ai-server-agent` | installed management/update/uninstall implementation |
| `/etc/ai-server-agent` | root-controlled configuration |
| `/etc/ai-server-agent/control` | root-only install identity, Cloudflare journal/backup, internal control state |
| `/var/lib/ai-server-agent` | root-controlled state container |
| `/var/lib/ai-server-agent/runtime` | root-controlled runtime container |
| `/var/lib/ai-server-agent/jobs` | root-controlled persistent-job container |
| `/var/log/ai-server-agent` | Agent logs/audit data |
| `/srv/ai-workspace` | `aiworker` writable project workspace; preserved by purge |
| `/run/lock/ai-server-agent` | purge-safe root lifecycle lock namespace |

Root-consumed control state must not be replaceable by `aiworker`/`aiagent`. Install/migration paths reject or repair unsafe legacy container layouts and symlinks where the supported migration semantics allow it.

## 4. Privileged management lifecycle

There are two lock layers with different scopes.

### Global lifecycle serialization

The installed `/usr/local/sbin/ai-server-agent-manage` wrapper acquires:

`/run/lock/ai-server-agent/management.lock`

before entering `manage.sh`. Install, update, uninstall and purge use the same root-only lifecycle lock (or inherit its open descriptor when invoked through management). This namespace is outside Agent config/state so purge cannot remove the lock while a concurrent operation is active.

This is the authoritative cross-operation lifecycle lock for privileged state mutation.

### Cloudflare/internal management lock

`manage.sh` also uses a root-only lock under `/etc/ai-server-agent/control` around connection-management internals. It protects transaction-local management state, but it is not the purge-safe global lifecycle namespace.

New privileged state-mutating entrypoints must participate in the global lifecycle serialization model rather than relying only on the internal control lock.

## 5. Connection modes

### Local/private

The default local endpoint binds to loopback and remains bearer-authenticated. This is appropriate for private connectivity such as a separately managed secure MCP tunnel.

### Manual public TLS

The operator supplies an existing certificate/key pair. The manager validates hostname/key pairing, installs them under `/etc/ai-server-agent/tls`, switches the Agent to public/native TLS, restarts services, and restores the previous local TLS/config state if activation fails.

### Guided Cloudflare

The Cloudflare path manages only the selected MCP hostname. It may create/reconcile:

- a proxied `A` DNS record;
- a hostname-scoped Origin Rule for the Agent port;
- a hostname-scoped Configuration Rule setting SSL to `strict`;
- a Cloudflare Origin CA certificate.

It does **not** change whole-zone SSL mode.

Existing external DNS/rules are not silently adopted or overwritten. The manager reuses a matching external proxied DNS record without taking ownership, and otherwise fails closed on conflicts or ambiguous ownership.

## 6. Cloudflare transaction and recovery model

Cloudflare configuration is a root-only durable transaction. The journal is:

`/etc/ai-server-agent/control/cloudflare-transaction.json`

and the durable pre-local-mutation rollback snapshot is:

`/etc/ai-server-agent/control/cloudflare-transaction-backup/`

The current journal schema is version 3.

### State machine

Normal commit path:

`prepared -> applying -> committing -> committed`

Recovery/rollback path:

`prepared | applying | unproven committing -> rolling_back -> rolled_back`

The journal validator enforces semantic relationships, not only JSON types:

- pending kind is bound to the correct Cloudflare ruleset phase;
- pending zone/hostname must match the transaction identity;
- every pending create kind carries the kind-specific durable ownership/representation evidence needed by its recovery path;
- pending creates cannot survive into local `applying`, `committing`, or `committed` state;
- commit identity must exactly match the transaction/resource identity;
- `rolled_back` is terminal only when pending evidence, commit intent, rollback-owned resources, and certificate ownership are empty;
- malformed, contradictory, unsafe-owner/mode, or unknown-schema journals fail closed and remain untouched.

### Durable local rollback snapshot

Before the first remote create, the manager creates the root-only local backup and records `backup_ready=true`. Local config/TLS/managed-state mutation does not begin until remote resources are reconciled and checkpointed.

If the process dies after local mutation but before a proven commit, a fresh process restores the durable local snapshot before entering `rolling_back`.

### Commit identity

Before replacing trusted managed state, the manager durably records the exact intended hostname, port, Cloudflare IDs, ownership flags, and fingerprints and moves to `committing`.

A fresh process may finalize `committing`/`committed` without rollback only when current trusted `managed.json` exactly matches that durable commit identity. Otherwise it restores local state and rolls back transaction-created remote resources.

### Remote ownership and concurrent drift

For confirmed Agent-owned DNS/rules, the manager stores canonical full-resource fingerprints and immediately re-reads the current resource before destructive cleanup. If representation changed, automatic deletion fails closed.

For response-lost POSTs, discovery identity alone is insufficient. Before the POST, the journal stores the pending create kind and transaction identity plus the exact request-controlled semantic fingerprint. DNS/rule creates also carry an unpredictable Agent marker/nonce used to discover only the Agent-created candidate.

Origin CA uses the newly generated CSR as its unpredictable discovery value. The durable Origin CA fingerprint covers `csr`, the hostname set, `request_type`, and `requested_validity`. Recovery first discovers a unique candidate by CSR, then GETs that exact certificate ID, recomputes the complete semantic fingerprint, and revokes only when it still equals the durable pre-POST intent. Ambiguity, representation mismatch, or exact-ID read failure preserves the pending recovery state and fails closed.

DNS/rule recovery likewise re-reads the exact discovered resource by ID, recomputes the semantic fingerprint, and deletes only if the current representation still matches the durable pre-POST representation. Competing lookalikes, multiple matches, missing representation proof, or drift remain unresolved rather than being destructively guessed.

Ruleset recovery deletes only the exact Agent rule, never the shared Ruleset container.

The current design deliberately avoids unconditional in-place mutation of previously recorded Cloudflare DNS/rules when a compare-and-swap guarantee is unavailable; drift is surfaced for explicit resolution instead.

## 7. Stable install/update trust

Stable and source channels are distinct.

### Stable bootstrap and first install

The release `install.sh` cannot authenticate itself before it executes, so it is deliberately **not** the stable trust root. Initial stable installation begins with `scripts/install-stable.sh` loaded from a Git tag already bound to a published immutable GitHub Release. For the v0.1.2 generation, that durable bootstrap source identity is `v0.1.2` itself once the release has been published immutable.

GitHub immutable releases lock their associated tag against movement/deletion while the release exists, and a deleted immutable release does not permit reuse of the same tag name. The bootstrap source therefore does not depend on preservation of a feature branch or a particular merge strategy.

The immutable-tag bootstrap runs before release `install.sh` bytes receive root execution. It:

1. resolves an explicit stable tag or GitHub's latest release metadata;
2. requires the release to be non-draft, non-prerelease and immutable;
3. requires exactly one uploaded `install.sh` asset at the exact tag-scoped release URL;
4. requires the GitHub release asset `sha256:` digest;
5. downloads that release installer without privileged execution and verifies its bytes against the digest;
6. crosses the privilege boundary through a fixed bootstrap helper that copies the candidate into a root-owned `0700` staging directory;
7. recomputes the same expected release-asset digest on the root-controlled copy and fails closed on any mismatch;
8. executes only that re-verified root-controlled copy.

The original invoking-user-writable pathname is therefore never trusted for root execution after the privilege boundary. Replacing that pathname between the unprivileged hash and `sudo` can at most cause the privileged re-verification to fail; it cannot make different bytes pass into the installer execution path.

The release-scoped `install.sh` is generated with its version/ref pinned to the release tag. Once authenticated by the bootstrap, it rejects `AI_SERVER_AGENT_BINARY` overrides, downloads the release archive plus `SHA256SUMS`, and continues only after archive checksum verification.

A direct `releases/.../install.sh | sudo bash` command is intentionally not a supported stable trust path because it would execute the asset before authenticating that same asset. A mutable branch is likewise not an acceptable stable bootstrap source.

Stable v0.1 artifacts are amd64-only.

### Stable update

Trusted install identity is strict root-only JSON under `/etc/ai-server-agent/control/install-state.json`. Missing, malformed, contradictory, or unsafe identity fails closed; the updater does not guess `main`.

The already-installed trusted updater applies the same release identity/digest invariant without needing the external bootstrap. For the stable channel it:

1. resolves an explicit tag or the latest published immutable release;
2. requires the release to be non-draft, non-prerelease and immutable;
3. requires exactly one uploaded `install.sh` asset at the exact tag-scoped release URL;
4. requires the GitHub release asset `sha256:` digest;
5. downloads `install.sh` and verifies its bytes against that digest before execution;
6. lets the release-scoped installer verify the archive with `SHA256SUMS`.

Source updates resolve the selected source ref to a commit SHA and remain a separate development/source mechanism.

## 8. Release provenance

Release publication is manual-dispatch and exact-SHA based. The workflow requires:

- an exact validated `main` SHA;
- successful `main` push CI and High Assurance Security for that SHA;
- promotion of the exact CI-produced release artifact rather than rebuilding the release payload;
- an absent release/tag before create-only tag creation;
- operator confirmation that repository release immutability is enabled;
- operator confirmation that the release-tag ruleset protects the release tag pattern from update/delete and has no bypass actor capable of moving/deleting the tag;
- exact tag creation at the validated SHA before `gh release create --verify-tag`;
- post-publication verification that the release is immutable, the tag still resolves to the validated SHA, and release/asset attestations verify.

The workflow intentionally does **not** treat a missing `bypass_actors` field from its `GITHUB_TOKEN` ruleset response as proof that no bypass actors exist. That property is an explicit operator gate because the workflow credential cannot authoritatively establish it.

## 9. Preservation and removal

Normal uninstall removes executable services/tooling while preserving configuration, Agent state, users, optional runtime data and workspace for reinstall/repair.

Purge removes Agent-owned config/state/log/runtime and the `aiagent` identity, but intentionally preserves:

- `/srv/ai-workspace`;
- `aiworker` and its group.

Cloudflare resources are not silently deleted by purge. Recorded Cloudflare resources must be cleaned through the ownership-aware Cloudflare cleanup path when desired.
