package mcp

import (
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

func TestPrerequisiteGuidancePreservesFailureIdentity(t *testing.T) {
	for _, tc := range []struct{ code, text string }{
		{"incus_unavailable", "managed_environment"},
		{"search_engine_unavailable", "workspace_search"},
		{"search_engine_incompatible", "ast-grep"},
		{"workspace_sandbox_unavailable", "Landlock"},
		{"browser_containment_unavailable", "browser_status"},
		{"terminal_runtime_unavailable", "terminal_open"},
		{"worker_shell_unavailable", "setpriv/env/Bash"},
		{"php_lsp_unavailable", "code_*"},
		{"gopls_unavailable", "gopls"},
		{"service_client_unavailable", "service_use"},
		{"python_ty_unavailable", "code_*"},
		{"unrecognized_optional_dependency", "could not be verified"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			class := "dependency"
			if tc.code == "terminal_runtime_unavailable" || tc.code == "worker_shell_unavailable" {
				class = "terminal"
			}
			in := executor.Response{OK: false, ErrorCode: tc.code, ReasonCode: tc.code, ErrorClass: class,
				Error: "provider-native detail; password=SENSITIVE", RequestID: "req-123", Output: "native stderr"}
			out := annotatePrerequisiteFailure(in)
			if out.OperatorHint == "" || !strings.Contains(out.OperatorHint, tc.text) ||
				out.PrerequisiteCode != tc.code || out.Error != in.Error || out.Output != in.Output ||
				out.ReasonCode != in.ReasonCode || out.RequestID != in.RequestID {
				t.Fatalf("guidance lost failure identity: %+v", out)
			}
			if strings.Contains(out.OperatorHint, "SENSITIVE") {
				t.Fatal("native data was copied into the operator hint")
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
	} {
		out := annotatePrerequisiteFailure(in)
		if out.OperatorHint != "" || out.PrerequisiteCode != "" {
			t.Fatalf("non-dependency was mislabeled: %+v", out)
		}
	}
	old := executor.Response{ErrorClass: "dependency", ErrorCode: "incus_unavailable", OperatorHint: "trusted specific instruction", PrerequisiteCode: "known"}
	if got := annotatePrerequisiteFailure(old); got.OperatorHint != old.OperatorHint || got.PrerequisiteCode != old.PrerequisiteCode {
		t.Fatalf("capability-specific hint overwritten: %+v", got)
	}
}
