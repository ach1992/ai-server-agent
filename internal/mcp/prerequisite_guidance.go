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
		resp.OperatorHint = "If Incus is needed, check managed_environment(status), then local incus query /1.0 or incus info with authorized access. Inspect daemon/socket errors; other tools may work."
	case resp.ErrorCode == "search_engine_unavailable":
		resp.OperatorHint = "Check which optional search engine this mode needs (rg or ast-grep). Use authorized run_command/git grep for suitable text searches; install only if necessary."
	case resp.ErrorCode == "search_engine_incompatible":
		resp.OperatorHint = "The trusted ast-grep version does not match this structural mode. Check its version or use text search when suitable; update only if needed."
	case resp.ErrorCode == "workspace_sandbox_unavailable":
		resp.OperatorHint = "Structured workspace helper needs Landlock. If necessary, inspect kernel/container support; authorized run_command is an alternative with different isolation, not a sandbox bypass."
	case resp.ErrorCode == "browser_containment_unavailable":
		resp.OperatorHint = "Browser containment is unavailable. Check browser_status and Executor cgroup details if UI automation is needed, or use another suitable method."
	case resp.ErrorCode == "service_client_unavailable":
		resp.OperatorHint = "The selected service protocol needs a trusted native CLI (ssh/sshpass/psql/mysql). Inspect the original error, or use another authorized API/client; provision only if needed."
	case resp.ErrorCode == "gopls_unavailable":
		resp.OperatorHint = "Go LSP unavailable. Use source search when sufficient; check trusted gopls and Go version only if semantic analysis is needed."
	case resp.ErrorCode == "terminal_runtime_unavailable":
		resp.OperatorHint = "Persistent PTY unavailable. run_command/start_job may suffice; if interaction is needed, inspect tmux/session state before repair or retry."
	case resp.ErrorCode == "worker_shell_unavailable":
		resp.OperatorHint = "Worker shell runtime unavailable. Inspect trusted Bash/setpriv/env if required; other file/code tools can still work."
	case strings.HasSuffix(resp.ErrorCode, "_lsp_unavailable") ||
		resp.ErrorCode == "python_ty_unavailable" || resp.ErrorCode == "typescript7_unavailable":
		resp.OperatorHint = "Requested language server unavailable. Source search may suffice; otherwise inspect its trusted adapter/runtime and recheck after any authorized repair."
	case resp.ErrorCode == "dap_containment_unavailable":
		resp.OperatorHint = "Debugger containment is unavailable. Inspect Executor cgroup support if debugging is needed; use source inspection or other authorized diagnostics instead."
	case resp.ErrorCode == "delve_unavailable" || resp.ErrorCode == "shell_dap_runtime_unavailable" ||
		resp.ErrorCode == "shell_dap_ipc_unavailable" || strings.HasSuffix(resp.ErrorCode, "_dap_unavailable"):
		resp.OperatorHint = "Optional debugger component unavailable. Inspect debug_adapter_status for the requested language; use source inspection if debugging is unnecessary."
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
