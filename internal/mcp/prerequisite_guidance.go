package mcp

import (
	"strings"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

// Add actionable guidance only when the observed failure identifies a known
// optional prerequisite. Do not turn every dependency error into an install
// instruction: unsupported operations and unrelated tools may still work.
func annotatePrerequisiteFailure(resp executor.Response) executor.Response {
	if resp.OK || resp.OperatorHint != "" {
		return resp
	}
	terminalMissing := resp.ErrorClass == "terminal" &&
		(resp.ErrorCode == "terminal_runtime_unavailable" || resp.ErrorCode == "worker_shell_unavailable")
	landlockMissing := resp.ErrorClass == "security" && resp.ErrorCode == "workspace_sandbox_unavailable"
	if resp.ErrorClass != "dependency" && !terminalMissing && !landlockMissing {
		return resp
	}
	switch {
	case resp.ErrorCode == "incus_unavailable":
		resp.OperatorHint = "Native Incus access was not established for this request. Inspect managed_environment(operation=status) and the native diagnostic if needed."
	case resp.ErrorCode == "search_engine_unavailable":
		resp.OperatorHint = "The requested search engine could not be verified (rg for text, ast-grep for structural). Inspect the native error if this mode is needed."
	case resp.ErrorCode == "search_engine_incompatible":
		resp.OperatorHint = "The trusted ast-grep version is incompatible with this structural-search mode; inspect its version if the mode is needed."
	case resp.ErrorCode == "workspace_sandbox_unavailable":
		resp.OperatorHint = "Structured workspace helper could not establish Landlock isolation. Inspect kernel/container support if this capability is needed; no bypass occurred."
	case resp.ErrorCode == "browser_containment_unavailable":
		resp.OperatorHint = "Browser containment was not established. Inspect browser_status and Executor diagnostics if this capability is needed."
	case resp.ErrorCode == "service_client_unavailable":
		resp.OperatorHint = "The selected service protocol lacks a trusted native client. Inspect the original executable error to identify the missing component."
	case resp.ErrorCode == "gopls_unavailable":
		resp.OperatorHint = "The optional trusted gopls adapter is unavailable for this request. Inspect the native diagnostic if semantic analysis is needed."
	case resp.ErrorCode == "terminal_runtime_unavailable":
		resp.OperatorHint = "The persistent PTY runtime is unavailable. Inspect tmux/session diagnostics if this capability is needed."
	case resp.ErrorCode == "worker_shell_unavailable":
		resp.OperatorHint = "The worker shell launch chain is unavailable. Inspect the trusted Bash/setpriv/env components if relevant."
	case strings.HasSuffix(resp.ErrorCode, "_lsp_unavailable") ||
		resp.ErrorCode == "python_ty_unavailable" || resp.ErrorCode == "typescript7_unavailable":
		resp.OperatorHint = "The requested language server is unavailable. Inspect the trusted adapter and runtime if this semantic operation is needed."
	case resp.ErrorCode == "dap_containment_unavailable":
		resp.OperatorHint = "Debugger containment could not be established. Inspect Executor cgroup diagnostics if debugging is needed."
	case resp.ErrorCode == "delve_unavailable" || resp.ErrorCode == "shell_dap_runtime_unavailable" ||
		resp.ErrorCode == "shell_dap_ipc_unavailable" || strings.HasSuffix(resp.ErrorCode, "_dap_unavailable"):
		resp.OperatorHint = "An optional debugger component is unavailable. Inspect debug_adapter_status for the selected language if needed."
	default:
		// Unknown dependency errors, including unsupported semantic methods,
		// must not be mislabeled as a missing package or repair instruction.
		return resp
	}
	if resp.PrerequisiteCode == "" {
		resp.PrerequisiteCode = resp.ErrorCode
	}
	return resp
}
