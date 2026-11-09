package executor

// optionalDAPPrerequisite supplements an observed absence of an optional,
// trusted adapter. A successful status call is still not a launch test; do
// not convert an absent adapter into an unsupported language or provision it.
func optionalDAPPrerequisite(detail map[string]any, installed bool, language string) map[string]any {
	if installed {
		return detail
	}
	var reason, component string
	switch language {
	case "javascript", "typescript":
		reason, component = language+"_dap_adapter_not_verified", "js-debug/Node"
	case "python":
		reason, component = "python_debugpy_adapter_not_verified", "Python/debugpy"
	case "php":
		reason, component = "php_xdebug_adapter_not_verified", "PHP/Xdebug/DBGp-to-DAP"
	case "rust":
		reason, component = "rust_lldb_adapter_not_verified", "Rust/LLDB-DAP"
	case "go":
		reason, component = "go_delve_adapter_not_verified", "Go/Delve"
	default:
		return detail
	}
	detail["unavailable_reason"] = reason
	detail["operator_hint"] = "Optional " + component + " launch chain was not verified; inspect its trusted components if needed."
	return detail
}
