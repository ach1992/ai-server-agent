package executor

import (
	"errors"
	"strings"
	"testing"
)

func TestCodeInspectionFailurePreservesMeaningAndNativeEvidence(t *testing.T) {
	for _, tc := range []struct {
		native string
		code   string
		class  string
	}{
		{"lsp_capability_not_supported: references", "code_capability_unsupported", "validation"},
		{"code target unstable during semantic overlay", "code_inspection_failed", "runtime"},
		{"lsp transport closed", "code_inspection_failed", "runtime"},
	} {
		t.Run(tc.code+"/"+tc.native, func(t *testing.T) {
			resp := codeInspectionFailure(errors.New(tc.native))
			if resp.OK || resp.ErrorCode != tc.code || resp.ErrorClass != tc.class ||
				!strings.Contains(resp.Error, tc.native) {
				t.Fatalf("semantic unsupported was treated as a missing dependency or lost error evidence: %+v", resp)
			}
		})
	}
	got := codeInspectionFailure(errCodeTargetUnstable)
	if got.ErrorClass != "conflict" || got.ErrorCode != "code_target_changed" {
		t.Fatalf("unstable target lost its conflict classification: %+v", got)
	}
}
