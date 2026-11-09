package executor

import (
	"strings"
	"testing"
)

func TestOptionalDAPReadinessGivesPerAdapterRecoveryWithoutFalseSupport(t *testing.T) {
	for _, tc := range []struct{ language, reason, tool string }{
		{"javascript", "javascript_dap_adapter_not_verified", "debug_adapter_status(language=javascript)"},
		{"typescript", "typescript_dap_adapter_not_verified", "debug_adapter_status(language=typescript)"},
		{"python", "python_debugpy_adapter_not_verified", "debug_adapter_status(language=python)"},
		{"php", "php_xdebug_adapter_not_verified", "debug_adapter_status(language=php)"},
		{"rust", "rust_lldb_adapter_not_verified", "debug_adapter_status(language=rust)"},
		{"go", "go_delve_adapter_not_verified", "debug_adapter_status(language=go)"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			detail := optionalDAPPrerequisite(map[string]any{"installed": false}, false, tc.language)
			if detail["unavailable_reason"] != tc.reason || !strings.Contains(detail["operator_hint"].(string), tc.tool) ||
				!strings.Contains(detail["operator_hint"].(string), "authoriz") {
				t.Fatalf("missing adapter lacks actionable recovery: %+v", detail)
			}
			available := optionalDAPPrerequisite(map[string]any{"installed": true}, true, tc.language)
			if available["unavailable_reason"] != nil || available["operator_hint"] != nil {
				t.Fatalf("installed adapter reported missing prerequisite: %+v", available)
			}
		})
	}
	unsupported := optionalDAPPrerequisite(map[string]any{"installed": false}, false, "unsupported-mode")
	if len(unsupported) != 1 {
		t.Fatalf("unsupported debugger language was labeled as an installable adapter: %+v", unsupported)
	}
}
