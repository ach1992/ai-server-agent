package mcp

import (
	"strings"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

// annotatePrerequisiteFailure is a conservative, presentation-boundary
// supplement for already classified dependency errors. It never turns
// unsupported/authorization/uncertain outcomes into "just install a package".
// Capability-specific status tools remain the owner of real runtime checks.
func annotatePrerequisiteFailure(resp executor.Response) executor.Response {
	if resp.OK || resp.OperatorHint != "" {
		return resp
	}
	// Terminal has a legacy error class even for a known missing optional
	// runtime. Do not relabel its other process, security or unknown states.
	terminalPrerequisite := resp.ErrorClass == "terminal" &&
		(resp.ErrorCode == "terminal_runtime_unavailable" || resp.ErrorCode == "worker_shell_unavailable")
	if resp.ErrorClass != "dependency" && !terminalPrerequisite {
		return resp
	}
	switch {
	case resp.ErrorCode == "incus_unavailable":
		resp.OperatorHint = "Check managed_environment(operation=status), then native Incus daemon/socket initialization (incus admin status) on this host; fix only with host authorization and recheck. Other Agent tools may still work."
	case resp.ErrorCode == "search_engine_unavailable":
		resp.OperatorHint = "Inspect the trusted search engine required by this mode (rg for text, ast-grep for structural), its installed version and executable trust. Repair/provision the optional engine on the authorized host, then repeat a read-only workspace_search. run_command remains an alternative for ordinary search."
	case resp.ErrorCode == "search_engine_incompatible":
		resp.OperatorHint = "Inspect the exact trusted structural-search engine version and the minimum required by this Agent. Provision a compatible ast-grep without replacing unrelated system binaries, then retry workspace_search(mode=structural)."
	case resp.ErrorCode == "workspace_sandbox_unavailable":
		resp.OperatorHint = "The kernel's structured-workspace Landlock prerequisites are unavailable. Inspect host kernel Landlock ABI and container/security configuration. Use authorized run_command as a separate capability if appropriate; any host/kernel changes require approval."
	case resp.ErrorCode == "browser_containment_unavailable":
		resp.OperatorHint = "Inspect browser_status, the pinned optional Browser runtime and Executor containment/cgroup readiness; use approved browser_setup only when repair is actually needed. Retry browser_status before opening a session."
	case resp.ErrorCode == "service_client_unavailable":
		resp.OperatorHint = "An optional native service client was not found. Inspect the original error for ssh, sshpass, psql, mysql or mariadb; inspect the trusted executable under /usr/bin, provision the needed package only with authorization, verify its presence, then retry the authorized service_use. Other service protocols remain available."
	case resp.ErrorCode == "gopls_unavailable":
		resp.OperatorHint = "The optional trusted gopls language server is not installed or verified. Inspect its trusted executable and Go toolchain, provision only with authorization, then recheck with a bounded code_* request and matching file_version; text search remains available."
	case resp.ErrorCode == "terminal_runtime_unavailable":
		resp.OperatorHint = "The optional Terminal runtime could not be prepared. Inspect trusted tmux, the private socket/scope and host resource diagnostics. Repair only with host authorization; reconcile any uncertain session before a new terminal_open. run_command remains available independently."
	case resp.ErrorCode == "worker_shell_unavailable":
		resp.OperatorHint = "The trusted worker shell chain (setpriv/env/Bash) is unavailable. Inspect the host executables and ownership; repair with authorization, then retry an isolated terminal_open only after reconciling any uncertain session."
	case strings.HasSuffix(resp.ErrorCode, "_lsp_unavailable") ||
		resp.ErrorCode == "python_ty_unavailable" || resp.ErrorCode == "typescript7_unavailable":
		resp.OperatorHint = "Optional pinned language intelligence is not ready for this language. Inspect its trusted adapter and transitive runtime resources on this host, repair only with authorization, and recheck with a bounded code_* request and exact file_version. Source search/run_command may remain usable."
	default:
		// A dependency failure is not proof of a permanently unsupported
		// tool. Do not guess a package/repair from a generic error.
		resp.OperatorHint = "An optional prerequisite could not be verified. Inspect the original error_code and bounded native diagnostics, use the relevant passive status check or authorized host diagnostics, then recheck after repair; do not repeat uncertain stateful operations."
	}
	if resp.PrerequisiteCode == "" {
		resp.PrerequisiteCode = resp.ErrorCode
	}
	return resp
}
