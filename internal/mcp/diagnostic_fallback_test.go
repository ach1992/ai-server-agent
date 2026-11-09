package mcp

import (
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/ach1992/ai-server-agent/internal/secretoutput"
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

func TestDiagnosticTextFallbackPrioritizesCompleteRecoveryEnvelope(t *testing.T) {
	next, eof, fileSize := int64(9223372036854775807), true, int64(987654321)
	escaped := strings.Repeat("\x00\t", 2000)
	resp := executor.Response{
		OK: false, Status: escaped, ErrorCode: escaped, ReasonCode: escaped,
		ErrorClass: escaped, RequestID: escaped, OperationID: escaped,
		Error: escaped, AuditError: escaped, AuditDegraded: true,
		JobID: "job-stable-recovery-id", FileVersion: "sha256:valid-file-recovery-version",
		FileSize: &fileSize, RequestedOffset: 214, NextOffset: &next, EOF: &eof,
		AvailableFromOffset: 12, CurrentEnd: next, RetentionTruncated: true,
		OutputEncoding: "base64", Output: escaped,
	}
	result, structured, err := responseResultWithLimit(resp, 4096)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Content[0].(*mcpsdk.TextContent).Text
	for _, want := range []string{
		"diagnostic_truncated=true", "job_id=\"job-stable-recovery-id\"",
		"file_version=\"sha256:valid-file-recovery-version\"",
		"file_size=987654321", "requested_offset=214",
		"next_offset=9223372036854775807", "eof=true",
		"available_from_offset=12", "current_end=9223372036854775807",
		"retention_truncated=true", "limit<=2048", "structuredContent",
		"retryable=false", "audit_degraded=true",
		"status=", "error_code=", "reason_code=", "error_class=",
		"request_id=", "operation_id=", "error=", "audit_error=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("recovery-critical fallback fragment lost: %s: %q", want, got)
		}
	}
	if len(got) > 4096 || strings.Contains(got, "\x00") {
		t.Fatalf("fallback exceeded exact text-byte budget: %d", len(got))
	}
	if structured.Output != resp.Output || structured.ErrorCode != resp.ErrorCode ||
		structured.FileVersion != resp.FileVersion || structured.RequestID != resp.RequestID {
		t.Fatal("priority fallback mutated structured content")
	}
}

func TestRepositoryGitFailureMCPEnvelopeDoesNotExposeAuthorization(t *testing.T) {
	native := "git status exited 47: fatal: unable to read index; Authorization: Basic PRIVATE-BASE64; connection refused"
	safe, _ := secretoutput.BoundedDiagnosticText(native, 4096)
	for _, fallback := range []bool{false, true} {
		resp := executor.Response{
			OK: false, ErrorCode: "git_failed", ReasonCode: "git_failed",
			ErrorClass: "process", ExitCode: 47, Error: safe,
		}
		if fallback {
			resp.Output = strings.Repeat("X", 16384)
		}
		result, structured, err := responseResultWithLimit(resp, 4096)
		if err != nil {
			t.Fatal(err)
		}
		text := result.Content[0].(*mcpsdk.TextContent).Text
		if strings.Contains(text, "PRIVATE-BASE64") || strings.Contains(structured.Error, "PRIVATE-BASE64") ||
			!strings.Contains(text, "git_failed") || !strings.Contains(text, "exit_code") ||
			!strings.Contains(structured.Error, "Authorization: [REDACTED]") ||
			!strings.Contains(structured.Error, "connection refused") {
			t.Fatalf("MCP repository diagnostic redaction/exit failed: fallback=%t text=%q structured=%+v", fallback, text, structured)
		}
	}
}
