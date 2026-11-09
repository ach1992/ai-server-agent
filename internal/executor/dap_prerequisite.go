package executor

// optionalDAPPrerequisite supplements an observed absence of an optional,
// trusted adapter. A successful status call is still not a launch test; do
// not convert an absent adapter into an unsupported language or provision it.
func optionalDAPPrerequisite(detail map[string]any, installed bool, language string) map[string]any {
	if installed {
		return detail
	}
	var reason, hint string
	switch language {
	case "javascript":
		reason, hint = "javascript_dap_adapter_not_verified", "Check the optional pinned complete js-debug runtime/Node chain on this host; after authorized provisioning, recheck debug_adapter_status(language=javascript). Browser-mode support is independent and must be verified separately."
	case "typescript":
		reason, hint = "typescript_dap_adapter_not_verified", "Check the optional pinned complete js-debug runtime/Node chain on this host and precompiled TypeScript source maps. After authorized provisioning, recheck debug_adapter_status(language=typescript)."
	case "python":
		reason, hint = "python_debugpy_adapter_not_verified", "Inspect the trusted Python interpreter and pinned debugpy adapter files on this host. Provision/repair only with authorization, then recheck debug_adapter_status(language=python)."
	case "php":
		reason, hint = "php_xdebug_adapter_not_verified", "Inspect PHP CLI, Xdebug and the trusted pinned DBGp-to-DAP adapter chain. Provision missing optional components with authorization, then recheck debug_adapter_status(language=php)."
	case "rust":
		reason, hint = "rust_lldb_adapter_not_verified", "Inspect the trusted pinned LLDB DAP executable and its native distribution. Repair optional dependencies with authorization, then recheck debug_adapter_status(language=rust)."
	case "go":
		reason, hint = "go_delve_adapter_not_verified", "Inspect the trusted Delve executable on this host and supported DAP mode. Provision it with authorization, then recheck debug_adapter_status(language=go)."
	default:
		return detail
	}
	detail["unavailable_reason"] = reason
	detail["operator_hint"] = hint
	return detail
}
