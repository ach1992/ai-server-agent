#!/usr/bin/env python3
"""Bounded, opt-in installed Agent protected-login acceptance via its normal MCP tools.

This is a *local operator test client*, not a new executor or credential path.
Only run on an authorized disposable Test host, as root, against an existing
loopback Agent instance with already enrolled service-access references.

No target password, MCP bearer or authenticated-page evidence is accepted via
arguments or emitted in output. The locally configured bearer is read privately
from the Agent's root-owned file, and all sign-in bytes remain in the Agent's
existing root Executor -> isolated Browser path.

Example:
  sudo python3 tests/installed_browser_auth_smoke.py \
    --config /etc/asa65-agent/config.json --port 13265 \
    --workspace /srv/ai-workspace/ai-server-agent-issue65-candidate \
    --expected-instance asa_5ad156dc167f5cb892457a6fbd685b94 \
    --ref svc_EXAMPLE --profile wordpress
"""

from __future__ import annotations

import argparse
import http.client
import json
import os
from pathlib import Path
import re
import stat
import sys
import time
import urllib.parse

PROTOCOL = "2025-11-25"
SERVICE_REF = re.compile(r"svc_[0-9a-f]{32}\Z")
INSTANCE_ID = re.compile(r"asa_[0-9a-f]{32}\Z")
PROFILES = {
    "wordpress": {
        "username_selector": "#user_login",
        "secret_selector": "#user_pass",
        "submit_selector": "#wp-submit",
        "success_selector": "#wpadminbar",
        "login_error_selector": "#login_error",
        "checks": ({"action": "assert_text", "selector": "body", "expected": "Dashboard"},),
    },
    "aapanel": {
        "username_selector": "#username",
        "secret_selector": "#password",
        "submit_selector": ".login_btn",
        "success_selector": "#app",
        "challenge_selector": ".check_code:not(.hide)",
        "login_error_selector": ".error_tips:not(:empty)",
        "checks": (
            {"action": "assert_text", "selector": "#app", "expected": "Home"},
            {"action": "assert_text", "selector": "#app", "expected": "Files"},
            {"action": "assert_text", "selector": "#app", "expected": "CPU"},
        ),
    },
}


class SmokeError(Exception):
    """A stable, nonsensitive diagnostic."""


def require_regular_root_file(path: str, *, label: str) -> str:
    if not path or not os.path.isabs(path):
        raise SmokeError(label + "_path_invalid")
    try:
        info = os.lstat(path)
    except OSError as exc:
        raise SmokeError(label + "_unavailable") from None
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
        raise SmokeError(label + "_ownership_untrusted")
    parent = Path(path).parent
    while True:
        try:
            parent_info = os.lstat(parent)
        except OSError:
            raise SmokeError(label + "_ownership_untrusted") from None
        if (not stat.S_ISDIR(parent_info.st_mode) or parent_info.st_uid != 0 or
                parent_info.st_mode & 0o022):
            raise SmokeError(label + "_ownership_untrusted")
        if parent == parent.parent:
            break
        parent = parent.parent
    return path


def load_private_bearer(config_path: str, *, port: int, instance: str) -> str:
    require_regular_root_file(config_path, label="config")
    try:
        with open(config_path, encoding="utf-8") as stream:
            config = json.load(stream)
        if not isinstance(config, dict):
            raise ValueError("not an object")
        # Bind the private bearer to the *configured* local MCP listener,
        # not an arbitrary loopback port supplied in CLI arguments.
        if (config.get("listen_address") != f"127.0.0.1:{port}" or
                config.get("mcp_path") != "/mcp" or
                config.get("instance_id") != instance or
                config.get("auth_mode") != "bearer" or
                config.get("tls_cert_file") or config.get("tls_key_file")):
            raise SmokeError("local_agent_identity_mismatch")
        bearer_path = require_regular_root_file(config.get("bearer_token_file", ""), label="bearer")
        with open(bearer_path, encoding="utf-8") as stream:
            bearer = stream.read().strip()
    except (OSError, ValueError, TypeError):
        raise SmokeError("private_config_unavailable") from None
    if not bearer or len(bearer) > 4096 or "\n" in bearer or "\r" in bearer:
        raise SmokeError("bearer_format_invalid")
    return bearer


class LocalMCP:
    def __init__(self, *, port: int, bearer: str):
        if port < 1 or port > 65535:
            raise SmokeError("loopback_port_invalid")
        self.port = port
        self.bearer = bearer
        self.sequence = 0

    def rpc(self, method: str, params: dict, timeout: float = 30) -> dict:
        self.sequence += 1
        payload = json.dumps({
            "jsonrpc": "2.0", "id": self.sequence, "method": method,
            "params": params,
        }).encode("utf-8")
        if len(payload) > 8192:
            raise SmokeError("rpc_request_too_large")
        # The raw loopback HTTP client does not follow redirects or consult
        # proxy settings. The private bearer is never forwarded to another host.
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)
        try:
            conn.request("POST", "/mcp", body=payload, headers={
                "Authorization": "Bearer " + self.bearer,
                "Content-Type": "application/json",
                "Accept": "application/json, text/event-stream",
                "MCP-Protocol-Version": PROTOCOL,
            })
            response = conn.getresponse()
            if response.status != 200:
                raise SmokeError("rpc_http_" + str(response.status))
            if response.getheader("Content-Type", "").split(";")[0].strip() != "application/json":
                raise SmokeError("unexpected_rpc_content_type")
            raw = response.read(1024 * 1024 + 1)
        except (OSError, TimeoutError, http.client.HTTPException):
            raise SmokeError("rpc_transport_unavailable") from None
        finally:
            conn.close()
        if len(raw) > 1024 * 1024:
            raise SmokeError("rpc_response_too_large")
        try:
            message = json.loads(raw)
        except (ValueError, UnicodeError):
            raise SmokeError("rpc_response_invalid") from None
        if not isinstance(message, dict) or "error" in message:
            raise SmokeError("rpc_protocol_failure")
        result = message.get("result")
        if not isinstance(result, dict):
            raise SmokeError("rpc_result_missing")
        return result

    def initialize(self) -> None:
        result = self.rpc(
            "initialize",
            {"protocolVersion": PROTOCOL, "capabilities": {},
             "clientInfo": {"name": "asa-installed-protected-auth-acceptance", "version": "1"}},
        )
        if result.get("serverInfo", {}).get("name") != "ai-server-agent":
            raise SmokeError("unexpected_mcp_server")

    def call(self, tool: str, arguments: dict, *, timeout: float = 30) -> dict:
        result = self.rpc("tools/call", {"name": tool, "arguments": arguments}, timeout)
        structured = result.get("structuredContent")
        if not isinstance(structured, dict):
            raise SmokeError("tool_structured_result_missing")
        # browser_status is a passive RuntimeStatus object, unlike the
        # Executor-backed tools, which return {ok, error_code, ...}.
        if tool == "browser_status":
            if structured.get("inspection_complete") is not True:
                raise SmokeError("browser_status_incomplete")
            return structured
        if structured.get("ok") is not True:
            code = structured.get("error_code") or structured.get("reason_code") or "unknown"
            if not isinstance(code, str) or not re.fullmatch(r"[a-zA-Z0-9_]{1,80}", code):
                code = "unknown"
            raise SmokeError("tool_" + tool + "_" + code)
        return structured


def read_registered_service(client: LocalMCP, ref: str, instance: str) -> str:
    raw = client.call("service_access", {"operation": "inspect", "ref": ref})
    if raw.get("output_encoding") != "json" or not isinstance(raw.get("output"), str):
        raise SmokeError("reference_metadata_invalid")
    try:
        metadata = json.loads(raw["output"])
    except ValueError:
        raise SmokeError("reference_metadata_invalid") from None
    if not isinstance(metadata, dict) or metadata.get("id") != ref:
        raise SmokeError("reference_identity_mismatch")
    if metadata.get("host_id") != instance:
        raise SmokeError("reference_agent_mismatch")
    if metadata.get("status") != "ready":
        raise SmokeError("reference_not_ready")
    endpoint = metadata.get("endpoint")
    if not isinstance(endpoint, str):
        raise SmokeError("service_endpoint_invalid")
    parsed = urllib.parse.urlsplit(endpoint)
    if (parsed.scheme not in ("http", "https") or not parsed.hostname or
            parsed.username is not None or parsed.password is not None or
            parsed.query or parsed.fragment or len(endpoint) > 2048):
        raise SmokeError("service_endpoint_invalid")
    return endpoint


def run_smoke(client: LocalMCP, *, profile: str, ref: str,
              instance: str, workspace: str, ignore_https_errors: bool) -> dict:
    if profile not in PROFILES:
        raise SmokeError("unknown_profile")
    if not SERVICE_REF.fullmatch(ref) or not INSTANCE_ID.fullmatch(instance):
        raise SmokeError("reference_or_instance_format_invalid")
    if not os.path.isabs(workspace) or not os.path.isdir(workspace):
        raise SmokeError("workspace_unavailable")
    client.initialize()
    status = client.call("browser_status", {})
    if not status.get("ready"):
        raise SmokeError("browser_not_ready")
    endpoint = read_registered_service(client, ref, instance)
    if ignore_https_errors and urllib.parse.urlsplit(endpoint).scheme != "https":
        raise SmokeError("https_override_requires_https")
    rules = PROFILES[profile]
    opened = client.call("browser_session_open", {
        "workspace": workspace, "profile_mode": "isolated",
        "ignore_https_errors": ignore_https_errors,
    }, timeout=60)
    sid = opened.get("session_id")
    if not isinstance(sid, str) or not sid:
        raise SmokeError("browser_session_identity_missing")
    login_started = None
    failed = None
    result = None
    cleanup_failure = None
    try:
        client.call("browser_session_flow", {
            "workspace": workspace, "session_id": sid,
            "steps": [{"action": "goto", "url": endpoint}],
        }, timeout=45)
        login_started = time.monotonic()
        credentials = {name: value for name, value in rules.items() if name != "checks"}
        credentials.update({
            "workspace": workspace, "session_id": sid, "credential_ref": ref,
        })
        outcome = client.call("browser_session_credential_login", credentials, timeout=55)
        duration_ms = round((time.monotonic() - login_started) * 1000)
        if outcome.get("status") != "authenticated":
            raise SmokeError("login_not_confirmed")
        # Bounded text assertions only. Never request screenshots, snapshots,
        # raw network/console events or traces for credential-derived sessions.
        client.call("browser_session_flow", {
            "workspace": workspace, "session_id": sid,
            "steps": list(rules["checks"]),
        }, timeout=45)
        result = {
            "ok": True, "profile": profile,
            "status": "authenticated", "admin_assertions": len(rules["checks"]),
            "duration_ms": duration_ms, "instance_confirmed": True,
        }
    except SmokeError as exc:
        failed = exc
    finally:
        try:
            client.call("browser_session_close", {
                "workspace": workspace, "session_id": sid,
            }, timeout=35)
        except SmokeError as exc:
            cleanup_failure = exc
    if cleanup_failure is not None:
        # Never claim success when an isolated Chromium session may persist.
        raise SmokeError("browser_cleanup_unverified") from cleanup_failure
    if failed is not None:
        raise failed
    assert result is not None
    result["browser_session_closed"] = True
    return result


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, help="Root-owned local Agent config file")
    parser.add_argument("--port", required=True, type=int, help="Existing local MCP HTTP port")
    parser.add_argument("--workspace", required=True, help="Exact existing Browser workspace/worktree")
    parser.add_argument("--ref", required=True, help="Opaque registered local service reference")
    parser.add_argument("--expected-instance", required=True, help="Exact local Agent instance ID")
    parser.add_argument("--profile", required=True, choices=sorted(PROFILES))
    parser.add_argument("--ignore-https-errors", action="store_true",
                        help="Explicit local test exception for a known private HTTPS certificate")
    args = parser.parse_args(argv)
    if os.geteuid() != 0:
        print(json.dumps({"ok": False, "reason_code": "root_operator_required"}))
        return 2
    try:
        # Never execute the helper as root from a mutable worker worktree.
        # Explicitly stage the reviewed bytes under a root-owned directory.
        require_regular_root_file(__file__, label="client_script")
        bearer = load_private_bearer(args.config, port=args.port,
                                     instance=args.expected_instance)
        client = LocalMCP(port=args.port, bearer=bearer)
        result = run_smoke(
            client, profile=args.profile, ref=args.ref, instance=args.expected_instance,
            workspace=args.workspace, ignore_https_errors=args.ignore_https_errors,
        )
        print(json.dumps(result, sort_keys=True))
        return 0
    except SmokeError as exc:
        print(json.dumps({"ok": False, "reason_code": str(exc)}, sort_keys=True))
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
