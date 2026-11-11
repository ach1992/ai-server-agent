#!/usr/bin/env python3
"""Bounded, unprivileged LSP/DAP handshake for operator-verified adapters.

This is an initial protocol smoke, not authenticated MCP or debugger-launch proof.
Verify the root-owned candidate profile with `optional-adapter verify` first.
"""

import argparse
import json
import os
from pathlib import Path
import select
import shutil
import subprocess
import sys
import tempfile
import time


def adapter_command(name, profile, python):
    entries = profile["entrypoints"]
    node = entries.get("node")
    if name == "bash-lsp":
        return [node, entries["cli"], "start"], "shellscript", "sh", "#!/usr/bin/env bash\ngreet() { echo hi; }\ngreet\n"
    if name == "web-lsp-html":
        return [node, entries["html"], "--stdio"], "html", "html", "<!DOCTYPE html><html><body><main><h1>Greetings</h1></main></body></html>"
    if name == "web-lsp-css":
        return [node, entries["css"], "--stdio"], "css", "css", "main { color: blue; }"
    if name == "sql-lsp":
        return [node, entries["cli"], "--stdio"], "sql-postgres", "sql", "CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT);\n"
    if name == "python-dap":
        bootstrap = "import runpy,sys;sys.path.insert(0," + json.dumps(entries["modules"]) + ");runpy.run_module('debugpy.adapter',run_name='__main__')"
        return [python, "-I", "-Xfrozen_modules=off", "-c", bootstrap], None, None, None
    raise ValueError("unknown adapter")


def send(proc, message):
    data = json.dumps(message, separators=(",", ":")).encode("utf-8")
    if len(data) > 1 << 20:
        raise RuntimeError("oversized smoke request")
    proc.stdin.write(f"Content-Length: {len(data)}\r\n\r\n".encode("ascii") + data)
    proc.stdin.flush()


def await_response(proc, response_id, timeout):
    output = bytearray()
    diagnostics = bytearray()
    recent = []
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"adapter exited {proc.returncode}: {diagnostics[-800:].decode(errors='replace')}")
        ready, _, _ = select.select([proc.stdout, proc.stderr], [], [], max(0, deadline - time.monotonic()))
        for stream in ready:
            try:
                chunk = os.read(stream.fileno(), 16384)
            except BlockingIOError:
                chunk = b""
            if stream is proc.stdout:
                output.extend(chunk)
                if len(output) > 1 << 20:
                    raise RuntimeError("unbounded protocol output")
            else:
                diagnostics.extend(chunk[: max(0, 8192 - len(diagnostics))])
        while b"\r\n\r\n" in output:
            split = output.index(b"\r\n\r\n")
            header = output[:split].decode("ascii", errors="replace")
            lengths = [int(line.split(":", 1)[1].strip()) for line in header.split("\r\n") if line.lower().startswith("content-length:")]
            if len(lengths) != 1 or lengths[0] < 0 or lengths[0] > 1 << 20:
                raise RuntimeError("invalid adapter message length")
            end = split + 4 + lengths[0]
            if len(output) < end:
                break
            message = json.loads(output[split + 4 : end])
            del output[:end]
            recent.append(message)
            recent = recent[-3:]
            if message.get("id", message.get("request_seq")) == response_id:
                return message
    raise TimeoutError(f"adapter response timed out; recent={recent}; stderr={diagnostics[-800:].decode(errors='replace')}")


def run(name, profile_path, python, timeout):
    profile = json.loads(Path(profile_path).read_text(encoding="utf-8"))
    command, language, ext, content = adapter_command(name, profile, python)
    home = tempfile.mkdtemp(prefix="ach-adapter-smoke-", dir="/var/tmp")
    proc = None
    try:
        env = {
            "PATH": "/usr/bin:/bin",
            "HOME": home,
            "NODE_ENV": "production",
            "SQLLENS_NO_PLUGINS": "1",
            "SQLLENS_USER_CONFIG": "/dev/null",
            "PYTHONNOUSERSITE": "1",
        }
        proc = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=home, env=env)
        os.set_blocking(proc.stdout.fileno(), False)
        os.set_blocking(proc.stderr.fileno(), False)
        if language is None:
            send(proc, {"seq": 1, "type": "request", "command": "initialize", "arguments": {"adapterID": "python", "linesStartAt1": True, "columnsStartAt1": True, "pathFormat": "path"}})
            response = await_response(proc, 1, timeout)
            if response.get("success") is not True:
                raise RuntimeError(f"DAP initialize failed: {response}")
            print(f"{name}: DAP initialize=PASS")
            send(proc, {"seq": 2, "type": "request", "command": "disconnect", "arguments": {"terminateDebuggee": False}})
            return

        send(proc, {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"processId": None, "rootUri": None, "capabilities": {"textDocument": {"documentSymbol": {"hierarchicalDocumentSymbolSupport": True}}}, "workspaceFolders": None}})
        if "error" in await_response(proc, 1, timeout):
            raise RuntimeError("LSP initialize failed")
        send(proc, {"jsonrpc": "2.0", "method": "initialized", "params": {}})
        document = Path(home) / ("optional-smoke." + ext)
        document.write_text(content, encoding="utf-8")
        send(proc, {"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": {"textDocument": {"uri": document.as_uri(), "languageId": language, "version": 1, "text": content}}})
        send(proc, {"jsonrpc": "2.0", "id": 2, "method": "textDocument/documentSymbol", "params": {"textDocument": {"uri": document.as_uri()}}})
        symbols = await_response(proc, 2, timeout)
        if "error" in symbols or not isinstance(symbols.get("result"), list):
            raise RuntimeError(f"LSP symbols failed: {symbols}")
        print(f"{name}: LSP initialize=PASS symbols={len(symbols['result'])}")
        send(proc, {"jsonrpc": "2.0", "id": 3, "method": "shutdown", "params": {}})
    finally:
        if proc is not None:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait(timeout=2)
            for stream in (proc.stdin, proc.stdout, proc.stderr):
                stream.close()
        shutil.rmtree(home)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("adapter", choices=("bash-lsp", "web-lsp-html", "web-lsp-css", "sql-lsp", "python-dap"))
    parser.add_argument("profile", help="absolute root-owned candidate JSON (run optional-adapter verify first)")
    parser.add_argument("--python", default=sys.executable, help="trusted interpreter for debugpy")
    parser.add_argument("--timeout", type=float, default=18, help="per-request handshake deadline in seconds")
    opts = parser.parse_args()
    if opts.timeout <= 0:
        parser.error("--timeout must be positive")
    run(opts.adapter, opts.profile, opts.python, opts.timeout)
