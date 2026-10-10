#!/usr/bin/env python3
"""Opt-in Linux/systemd acceptance using a supplied candidate, never installed services.

Run as root on a dedicated test host after inspecting the candidate and this file:
  python3 tests/integrated_data_path.py /absolute/path/to/candidate
  python3 tests/integrated_data_path.py /absolute/path/to/candidate --terminal-only

The optional terminal-only mode exercises authenticated MCP + an isolated
worker tmux PTY/reconnect without creating systemd job units. Two separate
disposable named credentials exercise real cross-principal MCP authorization.
Opt-in fault probes use ASA_TEST_MCP_TERMINAL_FAULT=after_open or
ASA_TEST_MCP_TERMINAL_FAULT=cleanup_unverified with --terminal-only; both
MUST fail, and the second intentionally preserves an inspectable private
fixture for verified task-only cleanup. The default mode additionally
requires spare global ai-job capacity and never stops unrelated jobs.
Only task-owned temporary resources change.
This proves the isolated wire/process path, not installed-service acceptance,
ChatGPT rendering or Gateway.
"""

import base64
import hashlib
import json
import os
from pathlib import Path
import pwd
import secrets
import shlex
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def wait_for(check, timeout=15):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.1)
    raise TimeoutError("isolated acceptance condition did not become true")


def stop(process):
    if process is not None and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def running(response):
    return response.get("ok") and "ActiveState=active" in response.get("status", "").splitlines()


def main(candidate, *, terminal_only=False):
    require(os.geteuid() == 0, "requires root on a dedicated non-production test host")
    candidate = Path(candidate).resolve(strict=True)
    require(candidate.is_file() and os.access(candidate, os.X_OK), "candidate must be executable")
    require(candidate != Path("/usr/local/bin/ai-server-agent"), "supply an explicitly built candidate")
    worker = pwd.getpwnam("aiworker")
    evidence = []
    jobs = set()
    processes = []
    tmp = tempfile.mkdtemp(prefix="asa42-acceptance-")
    safe_to_remove = False
    terminal_cleanup_unverified = False
    terminal_mode_success = False
    fault = os.environ.get("ASA_TEST_MCP_TERMINAL_FAULT", "")
    require(fault in ("", "after_open", "cleanup_unverified"),
            "unknown fixture-only terminal fault mode")
    require(terminal_only or not fault,
            "terminal fault simulation requires explicit --terminal-only mode")
    try:
        root = Path(tmp)
        root.chmod(0o755)
        binary = root / "candidate"
        shutil.copyfile(candidate, binary)
        binary.chmod(0o755)
        binary_hash = hashlib.sha256(binary.read_bytes()).hexdigest()
        workspace = root / "workspace"
        workspace.mkdir(mode=0o750)
        os.chown(workspace, worker.pw_uid, worker.pw_gid)
        state, logs = root / "state", root / "logs"
        state.mkdir(mode=0o711)
        state.chmod(0o711)
        logs.mkdir(mode=0o750)
        os.chown(state, 0, 0)
        os.chown(logs, 0, worker.pw_gid)
        # The fixture server runs as aiworker. Its private credentials are unrelated
        # to the installed Agent and are never printed or retained after cleanup.
        token = secrets.token_hex(32)
        other_token = secrets.token_hex(32) if terminal_only else None
        for name in ("bearer", "executor-token"):
            path = root / name
            path.write_text(token)
            path.chmod(0o640)
            os.chown(path, 0, worker.pw_gid)
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        cfg = {
            "listen_address": f"127.0.0.1:{port}", "mcp_path": "/mcp",
            "health_path": "/healthz", "auth_mode": "bearer",
            "bearer_token_file": str(root / "bearer"),
            "executor_token_file": str(root / "executor-token"),
            "executor_socket": str(root / "executor.sock"),
            "state_dir": str(state), "log_dir": str(logs),
            "workspace_dir": str(workspace), "worker_user": "aiworker",
            "agent_user": "aiworker",
        }
        if terminal_only:
            # Exercise the production named-principal credential boundary,
            # without touching any installed credential store or recording
            # either raw bearer in fixture files.
            store_path = root / "mcp-credentials.json"
            now = "2026-10-10T00:00:00Z"
            store = {"version": 1, "credentials": [
                {"principal": {"id": "direct-default", "class": "direct", "name": "direct/default"},
                 "verifier_algorithm": "sha256-v1",
                 "verifier": hashlib.sha256(token.encode("ascii")).hexdigest(),
                 "created_at": now, "enabled": True},
                {"principal": {"id": "mcp-gateway", "class": "gateway", "name": "mcp-gateway"},
                 "verifier_algorithm": "sha256-v1",
                 "verifier": hashlib.sha256(other_token.encode("ascii")).hexdigest(),
                 "created_at": now, "enabled": True},
            ]}
            store_fd = os.open(store_path, os.O_WRONLY | os.O_CREAT |
                               os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(store_fd, "w", encoding="utf-8") as store_file:
                json.dump(store, store_file)
            os.chown(store_path, 0, worker.pw_gid)
            store_path.chmod(0o640)
            cfg.pop("bearer_token_file")
            cfg["credential_store_file"] = str(store_path)
        config = root / "config.json"
        config.write_text(json.dumps(cfg))
        config.chmod(0o644)
        # The production executor fails closed without the canonical keyed
        # Audit prerequisite. Reproduce the install.sh format in this
        # disposable, root-owned fixture; never use the host Agent key.
        key_path = root / "audit-fingerprint.key"
        key_fd = os.open(key_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(key_fd, "w", encoding="ascii") as key_file:
            key_file.write("v1:" + secrets.token_hex(32) + "\n")
        key_info = key_path.stat()
        require(key_info.st_uid == 0 and key_info.st_gid == 0 and
                key_info.st_mode & 0o777 == 0o600 and key_info.st_size == 68,
                "fixture-only Audit key does not meet installer trust requirements")
        worker_home = state / "worker-home"
        worker_home.mkdir(mode=0o700)
        os.chown(worker_home, worker.pw_uid, worker.pw_gid)
        # The state containers retain the same root-owned trust boundary as a
        # real installation; only the existing manifest is worker-writable.
        (state / "jobs").mkdir(mode=0o711)
        (state / "jobs").chmod(0o711)
        manifest = state / "AI_ENVIRONMENT.json"
        manifest.touch(mode=0o640)
        os.chown(manifest, worker.pw_uid, worker.pw_gid)
        server_log = (root / "process.log").open("wb")

        def launch(mode):
            process = subprocess.Popen(
                [str(binary), "-config", str(config), mode],
                stdout=server_log, stderr=server_log,
                user=worker.pw_uid if mode == "serve" else 0,
                group=worker.pw_gid if mode == "serve" else 0,
                extra_groups=[] if mode == "serve" else [0],
            )
            processes.append(process)
            return process

        executor = launch("executor")
        server = None
        sequence = 0

        def rpc(method, params=None, *, bearer_token=None):
            nonlocal sequence
            sequence += 1
            body = json.dumps({"jsonrpc": "2.0", "id": sequence,
                               "method": method, "params": params or {}}).encode()
            request = urllib.request.Request(
                f"http://127.0.0.1:{port}/mcp", data=body,
                headers={"Authorization": "Bearer " + (token if bearer_token is None else bearer_token),
                         "Content-Type": "application/json",
                         "Accept": "application/json, text/event-stream",
                         "MCP-Protocol-Version": "2025-11-25"},
            )
            with urllib.request.urlopen(request, timeout=20) as response:
                raw = response.read(8 * 1024 * 1024 + 1)
            require(len(raw) <= 8 * 1024 * 1024, "response exceeds bounded decoder budget")
            value = json.loads(raw)
            require("error" not in value, "JSON-RPC returned an error")
            return value["result"], len(raw)

        def call(name, arguments, *, bearer_token=None):
            result, wire_bytes = rpc("tools/call", {"name": name, "arguments": arguments},
                                     bearer_token=bearer_token)
            structured = result.get("structuredContent")
            require(isinstance(structured, dict), f"{name} lacks structuredContent")
            if structured.get("job_id"):
                jobs.add(structured["job_id"])
            text_bytes = sum(len(item.get("text", "").encode()) for item in result.get("content", []))
            if len(structured.get("output", "").encode()) > 32768:
                require(text_bytes < 1024, "large output duplicated in text fallback")
            return structured, wire_bytes, text_bytes

        def healthy():
            if server.poll() is not None:
                raise RuntimeError("isolated MCP server exited; inspect fixture log")
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1) as res:
                    return res.status == 200
            except (OSError, urllib.error.URLError):
                return False

        try:
            wait_for(lambda: (root / "executor.sock").is_socket())
            server = launch("serve")
            wait_for(healthy)
            init, _ = rpc("initialize", {"protocolVersion": "2025-11-25", "capabilities": {},
                                         "clientInfo": {"name": "isolated-acceptance", "version": "1"}})
            require(init.get("serverInfo", {}).get("name") == "ai-server-agent", "wrong endpoint")
            tools, _ = rpc("tools/list")
            require(any(tool["name"] == "browser_status" for tool in tools["tools"]), "candidate tools missing")
            for size in (1024, 32768, 65536, 1048576, 2097152):
                command = f"printf 'BEGIN'; head -c {size} /dev/zero | tr '\\000' x; printf 'END'"
                response, wire_bytes, text_bytes = call("run_command", {"command": command})
                require(response["ok"] and response["exit_code"] == 0, "command failed")
                require(response["bytes_seen"] == size + 8, "raw-byte accounting changed")
                require(response["bytes_returned"] == min(size + 8, 1048576), "inline output bound changed")
                require(response["output"].startswith("BEGIN") and response["output"].endswith("END"), "head/tail diagnostics lost")
                require(response["truncated"] == (size + 8 > 1048576), "truncation not explicit")
                evidence.append({"sample": "direct_command", "bytes_seen": size + 8,
                                 "wire_bytes": wire_bytes, "text_bytes": text_bytes,
                                 "duration_ms": response["duration_ms"]})
            response, _, _ = call("run_command", {"command": "printf '\\377\\376A'"})
            require(response["output_encoding"] == "base64" and base64.b64decode(response["output"]) == b"\xff\xfeA", "binary output corrupted")
            data = workspace / "binary"
            data.write_bytes(b"\xff\xfe" + b"A" * 131072)
            first, _, _ = call("read_file", {"path": str(data), "limit": 32768})
            require(first["ok"] and first["output_encoding"] == "base64", "binary ranged read failed")
            second, _, _ = call("read_file", {"path": str(data), "offset": first["next_offset"],
                                             "limit": 32768, "file_version": first["file_version"]})
            require(second["ok"] and second["next_offset"] == 65536, "file continuation failed")
            data.write_bytes(b"changed")
            changed, _, _ = call("read_file", {"path": str(data), "file_version": first["file_version"]})
            require(changed["error_code"] == "file_changed", "file generation conflict missing")
            evidence.append({"sample": "binary_and_ranged_file", "result": "PASS"})

            if terminal_only:
                # Exercise two distinct authenticated principals on the same
                # loopback MCP endpoint. The second is a named fixture-only
                # gateway identity, not an installed Gateway integration.
                assert other_token is not None
                other_init, _ = rpc(
                    "initialize",
                    {"protocolVersion": "2025-11-25", "capabilities": {},
                     "clientInfo": {"name": "isolated-other-principal", "version": "1"}},
                    bearer_token=other_token)
                require(other_init.get("serverInfo", {}).get("name") == "ai-server-agent",
                        "second credential did not authenticate")
                other_tools, _ = rpc("tools/list", bearer_token=other_token)
                require(len(other_tools.get("tools", [])) >= 1, "other principal cannot list tools")

                names = {entry["name"] for entry in tools["tools"]}
                needed = {"terminal_open", "terminal_write", "terminal_read",
                          "terminal_resize", "terminal_interrupt", "terminal_close",
                          "terminal_reconnect", "workspace_stat", "code_definition",
                          "debug_adapter_status"}
                require(not (needed - names), "missing public MCP capabilities: " +
                        repr(sorted(needed - names)))
                evidence.append({"sample": "authenticated_mcp_tools_list",
                                 "result": "PASS", "named_tools": len(names)})
                source = workspace / "main.go"
                source.write_text("package main\nfunc main() {}\n")
                source.chmod(0o644)
                worker_stat, _, _ = call("workspace_stat", {
                    "workspace": str(workspace), "path": "main.go"})
                require(worker_stat.get("ok") and worker_stat.get("file_version"),
                        "real worker workspace_stat failed: " +
                        str(worker_stat.get("error_code")))
                evidence.append({"sample": "public_worker_workspace_stat", "result": "PASS"})

                def must(name, arguments):
                    result, _, _ = call(name, arguments)
                    require(result.get("ok") is True,
                            name + " failed: " + str(result.get("error_code")) +
                            "/" + str(result.get("error_class")))
                    return result

                worker_identity = {"workspace": str(workspace)}
                worker_runtime = state / "worker-terminals"
                session_id = None
                epoch = None
                terminal_closed = False

                def owned_terminal_dirs():
                    # This state tree was created fresh for this exact fixture.
                    # Trust only root-owned private directories, not symlinks or
                    # arbitrary entries. Unknown state is a failed cleanup proof.
                    if not worker_runtime.exists() and not worker_runtime.is_symlink():
                        return []
                    parent_info = os.lstat(worker_runtime)
                    require(stat.S_ISDIR(parent_info.st_mode) and
                            parent_info.st_uid == 0 and
                            stat.S_IMODE(parent_info.st_mode) == 0o700,
                            "fixture worker terminal parent is untrusted")
                    owned = []
                    for child in worker_runtime.iterdir():
                        info = os.lstat(child)
                        require(child.name.startswith(".asa-tmux-") and
                                stat.S_ISDIR(info.st_mode) and
                                info.st_uid == 0 and
                                stat.S_IMODE(info.st_mode) == 0o700,
                                "unexpected fixture terminal runtime entry")
                        owned.append(child)
                    return owned

                def cleanup_owned_terminal_backends():
                    # Even terminal_open with OK=false can start a real tmux
                    # backend. Scan the ACTUAL runtime directory, independent
                    # of whether the response supplied a usable session ID.
                    for directory in owned_terminal_dirs():
                        sock = directory / "socket"  # real production basename
                        if os.path.lexists(sock):
                            info = os.lstat(sock)
                            require(stat.S_ISSOCK(info.st_mode) and info.st_uid == 0,
                                    "fixture tmux socket identity is untrusted")
                            subprocess.run(["/usr/bin/tmux", "-S", str(sock), "kill-server"],
                                           stdout=subprocess.DEVNULL,
                                           stderr=subprocess.DEVNULL, timeout=4,
                                           check=False)
                            for _ in range(30):
                                if not os.path.lexists(sock):
                                    break
                                time.sleep(.05)
                        require(not os.path.lexists(sock),
                                "fixture tmux backend still has its real socket")
                        directory.rmdir()  # only if empty; never delete unknown data
                    require(not owned_terminal_dirs(),
                            "fixture tmux runtime not fully retired")

                def await_output(marker, session_epoch, timeout=8.0):
                    cursor = 0
                    collected = bytearray()
                    expires = time.monotonic() + timeout
                    while time.monotonic() < expires:
                        result = must("terminal_read", dict(
                            worker_identity, session_id=session_id,
                            session_epoch=session_epoch, cursor=cursor, limit=8192))
                        cursor = result.get("next_cursor", cursor)
                        collected.extend(base64.b64decode(result.get("output", "")))
                        if marker.encode() in collected:
                            return
                        if len(collected) > 65536:
                            collected = collected[-65536:]
                        time.sleep(.04)
                    raise AssertionError("worker terminal marker missing: " + marker)

                def send(inp, session_epoch):
                    return must("terminal_write", dict(
                        worker_identity, session_id=session_id,
                        session_epoch=session_epoch, content=inp))

                try:
                    require(not owned_terminal_dirs(),
                            "fresh fixture unexpectedly has prior terminal backends")
                    # Capture a partially successful/uncertain open response
                    # BEFORE asserting OK, so broker/direct cleanup remains
                    # possible when terminal_start_uncertain returns identity.
                    opened, _, _ = call("terminal_open", dict(
                        worker_identity, columns=80, rows=24))
                    session_id = opened.get("session_id")
                    epoch = opened.get("session_epoch")
                    if fault == "after_open" and opened.get("ok"):
                        # Deterministic failure after actual backend creation,
                        # before accepting a successful open; must NOT PASS.
                        opened = dict(opened, ok=False,
                                      error_code="terminal_start_uncertain")
                    require(opened.get("ok") and session_id and epoch,
                            "terminal_open failed or uncertain: " +
                            str(opened.get("error_code")))

                    owner = dict(worker_identity, session_id=session_id,
                                 session_epoch=epoch)
                    for name, args in (
                        ("terminal_read", dict(owner, limit=8192)),
                        ("terminal_write", dict(owner, content="printf BAD\n")),
                        ("terminal_reconnect", dict(owner)),
                        ("terminal_close", dict(owner)),
                    ):
                        denied, _, _ = call(name, args, bearer_token=other_token)
                        # A reconnect also checks the durable record, so
                        # that path maps the SAME non-disclosing sentinel to
                        # terminal_reconnect_unavailable. Never accept a
                        # successful call or a leaked session/epoch identity.
                        code = denied.get("error_code")
                        concealed_reconnect = (
                            name == "terminal_reconnect" and
                            code == "terminal_reconnect_unavailable" and
                            denied.get("error") == "session_not_found")
                        require(denied.get("ok") is not True and
                                (code == "session_not_found" or concealed_reconnect) and
                                not denied.get("session_id") and
                                not denied.get("session_epoch"),
                                name + ": other principal was not denied without disclosure")
                    # The owner must retain control after every attempted
                    # cross-principal operation, including denied close.
                    send("printf 'OWNER_%s\n' 'SURVIVED'\n", epoch)
                    await_output("OWNER_SURVIVED", epoch)
                    evidence.append({"sample": "authenticated_mcp_cross_principal_terminal_denials",
                                     "result": "PASS", "principal_count": 2,
                                     "denied_actions": 4})

                    stale, _, _ = call("terminal_read", dict(
                        owner, session_epoch="stale-invalid-epoch"))
                    require(stale.get("error_code") == "terminal_epoch_changed",
                            "stale epoch not rejected")
                    send("printf 'UID:%s\\n' \"$(id -u)\"\n", epoch)
                    await_output("UID:" + str(worker.pw_uid), epoch)
                    send("printf 'HOME:%s\\n' \"$HOME\"\n", epoch)
                    await_output("HOME:" + str(worker_home), epoch)
                    must("terminal_resize", dict(
                        owner, session_epoch=epoch, columns=93, rows=31))
                    send("printf 'SLEEP_%s\\n' 'STARTED'; sleep 15\n", epoch)
                    await_output("SLEEP_STARTED", epoch)
                    must("terminal_interrupt", dict(owner, session_epoch=epoch))
                    send("printf 'AFTER_%s\\n' 'INTERRUPT'\n", epoch)
                    await_output("AFTER_INTERRUPT", epoch)

                    # Restart ONLY the fixture-owned executor, preserving the
                    # MCP server and a live tmux backend.
                    stop(executor)
                    executor = launch("executor")

                    def executor_reconnected():
                        if executor.poll() is not None:
                            raise RuntimeError("fixture executor unexpectedly exited")
                        try:
                            with socket.socket(socket.AF_UNIX) as c:
                                c.connect(str(root / "executor.sock"))
                            return True
                        except OSError:
                            return False

                    wait_for(executor_reconnected)
                    intruder_reconnect, _, _ = call("terminal_reconnect", dict(
                        worker_identity, session_id=session_id),
                        bearer_token=other_token)
                    require(intruder_reconnect.get("ok") is not True and
                            intruder_reconnect.get("error_code") ==
                                "terminal_reconnect_unavailable" and
                            intruder_reconnect.get("error") == "session_not_found" and
                            not intruder_reconnect.get("session_id") and
                            not intruder_reconnect.get("session_epoch"),
                            "other principal gained recovered session")
                    recovered = must("terminal_reconnect", dict(
                        worker_identity, session_id=session_id))
                    new_epoch = recovered.get("session_epoch")
                    require(new_epoch and new_epoch != epoch,
                            "reconnect did not fence previous epoch")
                    stale, _, _ = call("terminal_write", dict(
                        owner, session_epoch=epoch, content="true\n"))
                    require(stale.get("error_code") == "terminal_epoch_changed",
                            "old write epoch not fenced")
                    epoch = new_epoch
                    send("printf 'POST_%s\\n' 'RECONNECT'\n", epoch)
                    await_output("POST_RECONNECT", epoch)
                    if fault == "cleanup_unverified":
                        # Introduce one task-owned, non-secret unexpected file
                        # in the REAL tmux socket directory. Broker close
                        # stops tmux, but its nonrecursive rmdir cannot delete
                        # this marker; our verifier must detect/retain it.
                        directories = owned_terminal_dirs()
                        require(len(directories) == 1,
                                "cleanup-fault fixture has unexpected backend count")
                        (directories[0] / ".fixture-unknown-cleanup-entry").write_text(
                            "disposable cleanup-proof marker\n")
                    must("terminal_close", dict(
                        owner, session_epoch=epoch))
                    terminal_closed = True
                finally:
                    # Always enter cleanup, including when terminal_open had
                    # no usable result, or when opening was uncertain.
                    if session_id and epoch and not terminal_closed:
                        try:
                            call("terminal_close", dict(
                                worker_identity, session_id=session_id,
                                session_epoch=epoch))
                        except Exception:
                            pass  # verify/terminate exact local backend below
                    try:
                        cleanup_owned_terminal_backends()
                    except Exception:
                        terminal_cleanup_unverified = True
                    # Any unknown socket-directory entries keep the fixture
                    # for explicit investigation rather than false PASS.

                require(not terminal_cleanup_unverified,
                        "fixture terminal cleanup unverified; preserve evidence")
                evidence.append({"sample": "public_worker_terminal_open_write_read_resize_interrupt_reconnect_close",
                                 "result": "PASS", "isolated": True,
                                 "actual_socket_name": "socket",
                                 "verified_runtime_dirs": 0})
                dbg, _, _ = call("debug_adapter_status", {})
                require(dbg.get("ok"), "public debugger capability status unavailable")
                adapter = json.loads(dbg.get("output", "{}"))
                require(isinstance(adapter, dict) and
                        isinstance(adapter.get("installed"), bool),
                        "debug_adapter_status lacks installed boolean")
                evidence.append({"sample": "public_debug_adapter_status",
                                 "result": "PASS",
                                 "trusted_system_delve_installed": adapter["installed"]})
                terminal_mode_success = True
                return

            release = workspace / "release"
            command = f"head -c 9437184 /dev/zero | tr '\\000' J; while [ ! -f {shlex.quote(str(release))} ]; do sleep 0.1; done; printf END"
            arguments = {"command": command, "operation_id": "acceptance-primary"}
            existing = subprocess.check_output(["systemctl", "list-units", "--state=active,activating",
                                                "--plain", "--no-legend", "ai-job-*.service"], text=True)
            existing_count = len(existing.splitlines())
            require(existing_count < 4, "host job capacity is already occupied; preserve unrelated jobs")
            primary, _, _ = call("start_job", arguments)
            require(primary["ok"], "real systemd job launch failed: " + primary.get("error_code", ""))
            job_id = primary["job_id"]
            replay, _, _ = call("start_job", arguments)
            require(replay["ok"] and replay["job_id"] == job_id and replay["idempotent_replay"], "job replay duplicated work")
            conflict, _, _ = call("start_job", {"command": "printf DIFFERENT", "operation_id": "acceptance-primary"})
            require(conflict["error_code"] == "idempotency_conflict", "material replay conflict missing")
            wait_for(lambda: call("job_output", {"job_id": job_id, "limit": 16})[0].get("current_end", 0) >= 9437184)
            for index in range(3 - existing_count):
                held, _, _ = call("start_job", {"command": f"while [ ! -f {shlex.quote(str(release))} ]; do sleep 0.1; done",
                                                "operation_id": f"acceptance-held-{index}", "root": index == 0})
                require(held["ok"], "held systemd job failed: " + held.get("error_code", ""))
                wait_for(lambda: running(call("job_status", {"job_id": held["job_id"]})[0]))
            busy, _, _ = call("start_job", {"command": "true", "operation_id": "acceptance-over-capacity"})
            require(busy["error_code"] == "resource_limit" and busy.get("status") == "busy",
                    "job capacity not structured: " + busy.get("error_code", "") + "/" + busy.get("status", ""))
            retained, _, _ = call("job_output", {"job_id": job_id, "offset": 0, "limit": 65536})
            require(retained["ok"] and retained["retention_truncated"] and retained["available_from_offset"] > 0, "retention metadata missing")
            require((state / "jobs" / (job_id + ".log")).stat().st_size <= 8388608 + 32, "physical log exceeded bound")
            metadata = subprocess.check_output(["systemctl", "show", "ai-job-" + job_id + ".service", "-p", "ExecStart", "-p", "Description"], text=True)
            require(command not in metadata and "9437184" not in metadata, "Agent duplicated command into systemd metadata")
            stop(server)
            stop(executor)
            executor = launch("executor")
            def executor_ready():
                if executor.poll() is not None:
                    raise RuntimeError("isolated executor exited")
                try:
                    with socket.socket(socket.AF_UNIX) as conn:
                        conn.connect(str(root / "executor.sock"))
                    return True
                except OSError:
                    return False
            wait_for(executor_ready)
            server = launch("serve")
            wait_for(healthy)
            survived, _, _ = call("job_status", {"job_id": job_id})
            require(running(survived), "job did not survive fixture control-plane restart")
            replay, _, _ = call("start_job", arguments)
            require(replay["job_id"] == job_id and replay["idempotent_replay"], "restarted executor lost job identity")
            release.touch()
            wait_for(lambda: call("job_status", {"job_id": job_id})[0].get("status") == "completed")
            terminal, _, _ = call("job_status", {"job_id": job_id})
            require(terminal["ok"] and terminal["exit_code"] == 0, "job did not complete")
            tail, _, _ = call("job_output", {"job_id": job_id, "offset": retained["next_offset"], "limit": 1048576})
            require(tail["ok"] and tail["next_offset"] > retained["next_offset"], "retained continuation did not progress")
            evidence.append({"sample": "real_systemd_jobs", "active_limit": 4,
                             "physical_log_limit": 8388640, "available_from_offset": retained["available_from_offset"],
                             "unrelated_active_jobs_preserved": existing_count,
                             "restart_replay": "PASS", "capacity_refusal": "PASS"})
        except Exception:
            # Bounded, fixture-only diagnostics; no command bodies or credentials.
            print(json.dumps({"partial_evidence": evidence, "fixture": root.name}))
            for path in (state / "jobs").glob("*.status"):
                print("fixture_job_status", path.stem, path.read_text()[:64])
            for path in (state / "jobs").glob("*.log"):
                with path.open("rb") as stream:
                    raw = stream.read(512)
                print("fixture_job_log_prefix", path.stem, repr(raw))
            raise
        finally:
            # Exact task-created unit names only. Never stop installed Agent services.
            cleanup_errors = []
            for job_id in jobs:
                try:
                    require(job_id.isdigit(), "unexpected job identity; refusing unsafe cleanup")
                    unit = "ai-job-" + job_id + ".service"
                    subprocess.run(["systemctl", "stop", unit], stdout=subprocess.DEVNULL,
                                   stderr=subprocess.DEVNULL, timeout=15)
                    active = subprocess.run(["systemctl", "is-active", "--quiet", unit]).returncode == 0
                    require(not active, "task-created job could not be stopped")
                except Exception as error:
                    cleanup_errors.append(str(error))
            for process in reversed(processes):
                try:
                    stop(process)
                except Exception as error:
                    cleanup_errors.append(str(error))
            server_log.close()
            safe_to_remove = not cleanup_errors and not terminal_cleanup_unverified
            require(safe_to_remove, "cleanup incomplete; fixture preserved at " + tmp)
        print(json.dumps({"result": "PASS", "candidate_sha256": binary_hash, "evidence": evidence}, indent=2))
    finally:
        if safe_to_remove:
            shutil.rmtree(tmp)
            if terminal_mode_success:
                print(json.dumps({"result": "PASS", "mode": "isolated_terminal",
                                  "candidate_sha256": binary_hash, "evidence": evidence}, indent=2))
        else:
            print("fixture retained for safe recovery:", tmp, file=sys.stderr)


if __name__ == "__main__":
    require(len(sys.argv) in (2, 3) and
            (len(sys.argv) == 2 or sys.argv[2] == "--terminal-only"),
            "usage: integrated_data_path.py /absolute/path/to/candidate [--terminal-only]")
    main(sys.argv[1], terminal_only=len(sys.argv) == 3)
