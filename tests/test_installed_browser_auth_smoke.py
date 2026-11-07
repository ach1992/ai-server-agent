#!/usr/bin/env python3
"""Nonsensitive, pure fake-MCP checks for the installed protected-login smoke client."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

MODULE_PATH = Path(__file__).with_name("installed_browser_auth_smoke.py")
SPEC = importlib.util.spec_from_file_location("installed_browser_auth_smoke", MODULE_PATH)
auth = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(auth)

REF = "svc_" + "a" * 32
INSTANCE = "asa_" + "b" * 32


class FakeMCP:
    def __init__(self, *, error_tool=None, ref_status="ready", host=INSTANCE,
                 endpoint="http://127.0.0.1/login", close_error=False):
        self.error_tool = error_tool
        self.ref_status = ref_status
        self.host = host
        self.endpoint = endpoint
        self.close_error = close_error
        self.calls = []
        self.initialized = False

    def initialize(self):
        self.initialized = True

    def call(self, name, params, **_):
        self.calls.append((name, params))
        if name == self.error_tool:
            raise auth.SmokeError("fixture_failure")
        if name == "browser_status":
            return {"inspection_complete": True, "ready": True}
        if name == "service_access":
            return {"ok": True, "output_encoding": "json",
                    "output": json.dumps({"id": REF, "host_id": self.host,
                                          "status": self.ref_status, "endpoint": self.endpoint})}
        if name == "browser_session_open":
            return {"ok": True, "session_id": "fixture-session"}
        if name == "browser_session_credential_login":
            return {"ok": True, "status": "authenticated"}
        if name == "browser_session_close" and self.close_error:
            raise auth.SmokeError("fixture_close_failed")
        return {"ok": True}


class LocalLoginAcceptance(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)

    def run_fixture(self, client, profile="wordpress", ignore_https_errors=False):
        return auth.run_smoke(
            client, profile=profile, ref=REF, instance=INSTANCE,
            workspace=self.temp.name, ignore_https_errors=ignore_https_errors,
        )

    def test_browser_status_is_not_an_executor_ok_response(self):
        class PassiveResult(auth.LocalMCP):
            def rpc(self, method, params, timeout=30):
                return {"structuredContent": {"inspection_complete": True, "ready": True}}
        result = PassiveResult(port=12345, bearer="fixture").call("browser_status", {})
        self.assertTrue(result["ready"])

    def test_success_uses_normal_tools_and_always_closes(self):
        client = FakeMCP()
        result = self.run_fixture(client)
        self.assertTrue(result["ok"])
        self.assertTrue(result["browser_session_closed"])
        self.assertEqual(result["admin_assertions"], 1)
        self.assertTrue(client.initialized)
        self.assertEqual([name for name, _ in client.calls],
                         ["browser_status", "service_access", "browser_session_open",
                          "browser_session_flow", "browser_session_credential_login",
                          "browser_session_flow", "browser_session_close"])
        arguments = client.calls[4][1]
        self.assertEqual(arguments["credential_ref"], REF)
        self.assertEqual(arguments["secret_selector"], "#user_pass")
        self.assertNotIn("password", arguments)
        self.assertFalse(client.calls[2][1]["ignore_https_errors"])
        self.assertEqual(client.calls[3][1]["steps"][0]["url"], client.endpoint)
        self.assertEqual(client.calls[5][1]["steps"][0]["action"], "assert_text")
        self.assertEqual(client.calls[6][1]["session_id"], "fixture-session")

    def test_aapanel_only_uses_explicit_certificate_exception(self):
        client = FakeMCP(endpoint="https://127.0.0.1:4242/entrance")
        result = self.run_fixture(client, profile="aapanel", ignore_https_errors=True)
        self.assertEqual(result["admin_assertions"], 3)
        self.assertTrue(client.calls[2][1]["ignore_https_errors"])

    def test_failed_login_still_closes_without_replay(self):
        client = FakeMCP(error_tool="browser_session_credential_login")
        with self.assertRaisesRegex(auth.SmokeError, "fixture_failure"):
            self.run_fixture(client)
        self.assertEqual([name for name, _ in client.calls].count("browser_session_credential_login"), 1)
        self.assertEqual(client.calls[-1][0], "browser_session_close")

    def test_cleanup_failure_never_claims_success(self):
        client = FakeMCP(close_error=True)
        with self.assertRaisesRegex(auth.SmokeError, "browser_cleanup_unverified"):
            self.run_fixture(client)

    def test_invalid_reference_is_denied_before_browser_open(self):
        for client in (
            FakeMCP(ref_status="revoked"),
            FakeMCP(host="asa_" + "c"*32),
            FakeMCP(endpoint="http://attacker.invalid@127.0.0.1/login"),
            FakeMCP(endpoint="http://127.0.0.1/login?auth=something"),
        ):
            with self.subTest(client=client.__dict__):
                with self.assertRaises(auth.SmokeError):
                    self.run_fixture(client)
                self.assertNotIn("browser_session_open", [name for name, _ in client.calls])

    def test_insecure_https_override_needs_https(self):
        client = FakeMCP()
        with self.assertRaisesRegex(auth.SmokeError, "https_override_requires_https"):
            self.run_fixture(client, ignore_https_errors=True)

    def test_mcp_does_not_follow_redirect_with_private_bearer(self):
        class RedirectResponse:
            status = 302
            def getheader(self, name, default=None):
                return "https://unexpected.example/" if name == "Location" else default

        class FixedConnection:
            def __init__(self):
                self.request_count = 0
                self.closed = False
            def request(self, *args, **kwargs):
                self.request_count += 1
            def getresponse(self):
                return RedirectResponse()
            def close(self):
                self.closed = True

        connection = FixedConnection()
        with mock.patch.object(auth.http.client, "HTTPConnection", return_value=connection) as ctor:
            with self.assertRaisesRegex(auth.SmokeError, "rpc_http_302"):
                auth.LocalMCP(port=13265, bearer="LOCAL-TEST-ONLY").rpc("initialize", {})
        ctor.assert_called_once()
        self.assertEqual(ctor.call_args[0][0], "127.0.0.1")
        self.assertEqual(connection.request_count, 1)
        self.assertTrue(connection.closed)

    def test_private_bearer_bound_to_configured_listener_and_instance(self):
        token_path = Path(self.temp.name) / "test-token"
        token_path.write_text("SYNTHETIC-TEST-TOKEN-ONLY")
        config_path = Path(self.temp.name) / "test-config.json"
        config = {"listen_address": "127.0.0.1:13265", "mcp_path": "/mcp",
                  "instance_id": INSTANCE, "auth_mode": "bearer",
                  "bearer_token_file": str(token_path)}
        config_path.write_text(json.dumps(config))
        with mock.patch.object(auth, "require_regular_root_file", side_effect=lambda path, **_: path):
            self.assertEqual(auth.load_private_bearer(str(config_path), port=13265, instance=INSTANCE),
                             "SYNTHETIC-TEST-TOKEN-ONLY")
            for port, instance in ((13266, INSTANCE), (13265, "asa_" + "c" * 32)):
                with self.assertRaisesRegex(auth.SmokeError, "local_agent_identity_mismatch"):
                    auth.load_private_bearer(str(config_path), port=port, instance=instance)
            config["tls_cert_file"] = "/tmp/fake.crt"
            config_path.write_text(json.dumps(config))
            with self.assertRaisesRegex(auth.SmokeError, "local_agent_identity_mismatch"):
                auth.load_private_bearer(str(config_path), port=13265, instance=INSTANCE)

    def test_bogus_refs_fail_before_network_calls(self):
        client = FakeMCP()
        with self.assertRaisesRegex(auth.SmokeError, "reference_or_instance_format_invalid"):
            auth.run_smoke(client, profile="wordpress", ref="not-a-ref",
                           instance=INSTANCE, workspace=self.temp.name,
                           ignore_https_errors=False)
        self.assertEqual(client.calls, [])


if __name__ == "__main__":
    unittest.main()
