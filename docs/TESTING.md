# Release Test Plan

A stable release must not be published until the applicable release matrix and end-to-end connectivity checks have passed on clean, snapshot-backed servers. Pre-release implementation may merge to `main` after current CI, code/security review, and the primary validated platform pass; keep Issue #1 open for remaining stable-release gates.

## Supported server matrix

Run the core installation and MCP tests on:

- Ubuntu 22.04 LTS
- Ubuntu 24.04 LTS or newer supported Ubuntu
- Debian 11
- Debian 12 or newer supported Debian
- amd64; additionally validate arm64 before claiming arm64 release support

## 1. Fresh installation

On a disposable server, fetch the installer through the GitHub Contents API so slash-containing branch refs are handled correctly:

```bash
REF='feat/v0.1-agent'
curl -fsSLG \
  -H 'Accept: application/vnd.github.raw+json' \
  --data-urlencode "ref=$REF" \
  'https://api.github.com/repos/ach1992/ai-server-agent/contents/install.sh' | \
  sudo AI_SERVER_AGENT_REF="$REF" bash
```

For the first validation choose `local` bind mode and the default port `3210`.

Verify:

```bash
sudo systemctl is-active ai-server-agent.service
sudo systemctl is-active ai-server-agent-executor.service
curl -fsS http://127.0.0.1:3210/healthz
sudo ss -lntp | grep ':3210'
sudo cat /var/lib/ai-server-agent/AI_ENVIRONMENT.json
```

Acceptance:

- both services are `active`;
- health returns `{"status":"ok","service":"ai-server-agent"}`;
- port 3210 is loopback-only in local mode;
- ports 80 and 443 are untouched;
- the manifest lists the control-plane services, paths, socket, endpoint and preservation rules;
- nginx, Apache, Docker, PHP, database servers, Node.js, Python, tmux and hosting panels were not installed by the core installer.

## 2. ChatGPT Business MCP connection

Connect the local `/mcp` endpoint through the current OpenAI-supported private/remote MCP connectivity path. For a private/on-premises server, current OpenAI guidance supports Secure MCP Tunnel. In ChatGPT Business Developer Mode, create the custom MCP app and scan its tools.

Acceptance:

- the official MCP handshake succeeds;
- all expected tools are discoverable;
- `agent_environment` is advertised read-only;
- root and browser action tools are advertised as potentially destructive/action-capable;
- reconnecting the chat/app does not require reinstalling the server agent.

## 3. Normal and root execution

From ChatGPT:

```text
run_command: id -un
```

Expected: `aiworker`.

Then:

```text
run_root_command: id -u
```

Expected: `0`.

Also verify normal commands cannot write a root-owned test file while `run_root_command` can create and remove it.

## 4. Connection/destruction guard

Ask ChatGPT to attempt these **without approval=true**:

```text
reboot
ufw reset
iptables -P INPUT DROP
systemctl stop ai-server-agent.service
rm -rf /etc/ai-server-agent
```

Acceptance: each returns `approval_required` and none executes.

Do not approve these commands during the smoke test. The test proves the guard blocks the first attempt; it does not require damaging the VM.

## 5. Persistent jobs and reconnect

Start a harmless delayed job through `start_job`, for example:

```bash
sleep 45; echo persistent-job-ok
```

Record the returned job id. Disconnect/reconnect the MCP client or restart only `ai-server-agent.service` from the server console/SSH, then query `job_status` and `job_output`.

Acceptance: the job continues independently and its final output contains `persistent-job-ok`.

## 6. Optional browser

Call `browser_setup`. Confirm the installation action when prompted. Verify:

```bash
sudo test -x /opt/ai-server-agent/browser/node/bin/node
sudo stat -c '%U:%G %a' /opt/ai-server-agent/browser
sudo test -d /var/lib/ai-server-agent/runtime/browser
```

Use `browser_run` to navigate to a harmless test URL, read its title, inspect console/network state and persist a cookie or local-storage value across two browser calls.

Acceptance:

- browser automation works;
- engine files are root-owned and not writable by `aiworker`;
- persistent profile/session data survives separate MCP calls;
- installing the browser does not change the MCP listen address or ports 80/443.

## 7. Host-software non-interference

This is a core product requirement.

Install a normal web stack component such as nginx using `run_root_command`. After installation and restart of nginx, re-run MCP health and tool discovery.

Acceptance: nginx may own ports 80/443 while AI Server Agent remains healthy on its dedicated endpoint.

For aaPanel validation, use a fresh snapshot/VM and follow the panel's supported installation requirements. Before and after installation, verify the MCP services, endpoint, executor socket and manifest. If aaPanel modifies networking/firewall rules, those changes must not make the MCP path unreachable without explicit user approval.

## 8. Update

For reproducible validation, update from an immutable commit SHA:

```bash
sudo AI_SERVER_AGENT_REF='<40-character-commit-sha>' ./update.sh
```

Branch and tag refs are also accepted; the updater resolves them to a commit SHA before fetching the installer and source archive.

Acceptance:

- the requested ref is reported as a resolved immutable commit;
- existing bind mode and port are preserved;
- both services are restarted onto the newly installed binary;
- health succeeds after restart;
- existing workspace, tokens, job history and browser profile are preserved unless a migration explicitly documents otherwise.

## 9. Uninstall and purge

First test normal uninstall. It must remove services/binary while preserving config, state, optional runtimes, users and workspace.

Then reinstall and test:

```bash
sudo AI_SERVER_AGENT_YES=1 ./uninstall.sh --purge
```

Acceptance:

- agent services, binary, config, state, logs, `/opt/ai-server-agent` optional runtime and `aiagent` are removed;
- `/srv/ai-workspace` and `aiworker` remain intentionally preserved;
- unrelated host software and ports are untouched.

## Merge gate

A pre-release implementation may merge to `main` once the current head passes CI and final code/security review, and the primary validated platform has passed the core install/MCP/root/job/browser/update/non-interference flows. Merge does not imply stable-release support for untested OS/architecture/panel combinations.

## Stable release gate

Do not create a stable version tag/release until the supported-server matrix, arm64 coverage for any arm64 support claim, aaPanel compatibility claim (if retained), and end-to-end ChatGPT connection are validated. Any failure is fixed and the affected test is rerun before stable release.
