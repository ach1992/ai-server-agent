package mcp

import (
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDiagnosticTextFallbackRetainsFailureAndAuditWithMinimumBudget(t *testing.T) {
	for _, auditDegraded := range []bool{false, true} {
		resp := executor.Response{
			OK: auditDegraded, AuditDegraded: auditDegraded,
			Status: "native_failure", ErrorCode: "native_command_failed",
			ReasonCode: "native_command_failed", ErrorClass: "process",
			Retryable: false, TimedOut: true, ExitCode: 47,
			RequestID: "req-live-trace", OperationID: "operation-reconcile",
			Error:      "incus list failed at native CLI: socket missing, password=TOPSECRET; inspect daemon status",
			AuditError: "completion audit failed token=HIDDEN; check audit storage",
			Output:     strings.Repeat("\x00", 16<<10),
		}
		result, structured, err := responseResultWithLimit(resp, 4096)
		if err != nil {
			t.Fatal(err)
		}
		text := result.Content[0].(*mcpsdk.TextContent).Text
		for _, fragment := range []string{
			"ok=", "audit_degraded=", "native_command_failed", "request_id=\"req-live-trace\"",
			"operation_id=\"operation-reconcile\"", "retryable=false", "timed_out=true",
			"exit_code=47", "socket missing", "structuredContent",
		} {
			if !strings.Contains(text, fragment) {
				t.Errorf("degraded=%t: text omitted %q: %s", auditDegraded, fragment, text)
			}
		}
		if auditDegraded && !strings.Contains(text, "audit_error=") {
			t.Errorf("success with degraded audit failed to warn: %s", text)
		}
		if len(text) > 4096 || strings.Contains(text, "TOPSECRET") || strings.Contains(text, "HIDDEN") || strings.Contains(text, "\x00") {
			t.Errorf("fallback overflowed or leaked: %q", text)
		}
		if structured.Error != resp.Error || structured.AuditError != resp.AuditError ||
			structured.RequestID != resp.RequestID || structured.Output != resp.Output {
			t.Fatal("structured response was modified by the fallback")
		}
		if result.IsError == auditDegraded {
			t.Fatal("MCP IsError must track native OK, independently of degraded audit")
		}
	}
}

func TestDiagnosticTextFallbackExtremeMetadataStaysBounded(t *testing.T) {
	next, eof := int64(17), false
	resp := executor.Response{OK: false, ErrorCode: "request_failed", ReasonCode: "native_failed",
		Error: strings.Repeat("\x00", 3000), AuditDegraded: true, AuditError: strings.Repeat("\x00", 3000),
		RequestID: "req-critical", OperationID: "op-critical",
		FileVersion: strings.Repeat("\x00", 3000), JobID: "1234",
		NextOffset: &next, EOF: &eof,
		OutputEncoding: "base64", Output: strings.Repeat("\x00", 8192)}
	result, _, err := responseResultWithLimit(resp, 4096)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcpsdk.TextContent).Text
	if len(text) > 4096 || !strings.Contains(text, "diagnostic_truncated=true") ||
		!strings.Contains(text, "req-critical") || !strings.Contains(text, "audit_degraded=true") ||
		!strings.Contains(text, "job_id=") || !strings.Contains(text, "next_offset=17") ||
		!strings.Contains(text, "limit<=2048") {
		t.Fatalf("minimum fallback lost metadata/budget: len=%d %q", len(text), text)
	}
}
