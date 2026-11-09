package executor

import (
	"strings"
	"testing"
)

func TestOptionalDAPReadinessGivesPerAdapterRecoveryWithoutFalseSupport(t *testing.T) {
	for _, tc := range []struct{ language, reason, tool string }{
		{"javascript", "javascript_dap_adapter_not_verified", "js-debug/Node"},
		{"typescript", "typescript_dap_adapter_not_verified", "js-debug/Node"},
		{"python", "python_debugpy_adapter_not_verified", "Python/debugpy"},
		{"php", "php_xdebug_adapter_not_verified", "PHP/Xdebug"},
		{"rust", "rust_lldb_adapter_not_verified", "Rust/LLDB-DAP"},
		{"go", "go_delve_adapter_not_verified", "Go/Delve"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			detail := optionalDAPPrerequisite(map[string]any{"installed": false}, false, tc.language)
			if detail["unavailable_reason"] != tc.reason ||
				!strings.Contains(detail["operator_hint"].(string), "not verified") ||
				!strings.Contains(detail["operator_hint"].(string), tc.tool) ||
				len(detail["operator_hint"].(string)) > 155 {
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
