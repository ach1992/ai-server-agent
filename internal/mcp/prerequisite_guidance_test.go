package mcp

import (
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

func TestPrerequisiteGuidancePreservesFailureIdentity(t *testing.T) {
	for _, tc := range []struct{ code, class, useful string }{
		{"incus_unavailable", "dependency", "managed_environment(operation=status)"},
		{"search_engine_unavailable", "dependency", "rg for text"},
		{"search_engine_incompatible", "dependency", "ast-grep"},
		{"workspace_sandbox_unavailable", "security", "Landlock"},
		{"browser_containment_unavailable", "dependency", "browser_status"},
		{"terminal_runtime_unavailable", "terminal", "tmux"},
		{"worker_shell_unavailable", "terminal", "Bash/setpriv/env"},
		{"php_lsp_unavailable", "dependency", "language server"},
		{"gopls_unavailable", "dependency", "gopls"},
		{"service_client_unavailable", "dependency", "native client"},
		{"python_ty_unavailable", "dependency", "language server"},
		{"delve_unavailable", "dependency", "debug_adapter_status"},
		{"dap_containment_unavailable", "dependency", "Executor cgroup"},
	} {
		t.Run(tc.code+"/"+tc.class, func(t *testing.T) {
			in := executor.Response{OK: false, ErrorCode: tc.code, ReasonCode: tc.code, ErrorClass: tc.class,
				Error: "provider-native detail; password=SENSITIVE", RequestID: "req-123", Output: "native stderr"}
			out := annotatePrerequisiteFailure(in)
			if out.OperatorHint == "" || !strings.Contains(out.OperatorHint, tc.useful) ||
				out.PrerequisiteCode != tc.code || out.Error != in.Error || out.Output != in.Output ||
				out.ReasonCode != in.ReasonCode || out.RequestID != in.RequestID ||
				out.ErrorClass != in.ErrorClass {
				t.Fatalf("guidance lost failure identity: %+v", out)
			}
			if len(out.OperatorHint) > 280 || strings.Contains(out.OperatorHint, "SENSITIVE") {
				t.Fatalf("hint too verbose or copied private native data: %q", out.OperatorHint)
			}
			fallback := diagnosticTextFallback(out, 4096)
			for _, marker := range []string{"operator_hint=", "prerequisite_code=", "req-123", "error_code="} {
				if !strings.Contains(fallback, marker) {
					t.Fatalf("text-only recovery omitted %q: %s", marker, fallback)
				}
			}
		})
	}
}

func TestPrerequisiteGuidanceDoesNotMislabelOtherFailures(t *testing.T) {
	for _, in := range []executor.Response{
		{OK: true, ErrorClass: "dependency", ErrorCode: "incus_unavailable"},
		{OK: false, ErrorClass: "approval", ErrorCode: "approval_required"},
		{OK: false, ErrorClass: "validation", ErrorCode: "unsupported_language"},
		{OK: false, ErrorClass: "state", ErrorCode: "outcome_uncertain"},
		{OK: false, ErrorClass: "terminal", ErrorCode: "terminal_start_uncertain"},
		{OK: false, ErrorClass: "process", ErrorCode: "search_engine_unavailable"},
		{OK: false, ErrorClass: "dependency", ErrorCode: "code_capability_unsupported"},
		{OK: false, ErrorClass: "dependency", ErrorCode: "unrecognized_optional_dependency"},
		{OK: false, ErrorClass: "resource", ErrorCode: "resource_limit"},
		{OK: false, ErrorClass: "security", ErrorCode: "another_security_failure"},
	} {
		out := annotatePrerequisiteFailure(in)
		if out.OperatorHint != "" || out.PrerequisiteCode != "" {
			t.Fatalf("unknown or non-prerequisite failure misclassified: %+v", out)
		}
	}
	old := executor.Response{ErrorClass: "dependency", ErrorCode: "incus_unavailable", OperatorHint: "trusted specific instruction", PrerequisiteCode: "known"}
	if got := annotatePrerequisiteFailure(old); got.OperatorHint != old.OperatorHint || got.PrerequisiteCode != old.PrerequisiteCode {
		t.Fatalf("capability-specific hint overwritten: %+v", got)
	}
}
