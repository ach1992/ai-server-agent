package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/managedincus"
)

func TestManagedNativeFailureUsesSeparateDiagnosticChannel(t *testing.T) {
	native := &managedincus.NativeCommandError{
		Stage: "list", ExitCode: 47, Cause: errors.New("exit status 47"),
		Diagnostic: "native transport failed; recovery: inspect socket",
		BytesSeen:  int64(len("native transport failed; recovery: inspect socket")),
	}
	resp := managedErr(native)
	if resp.OK || resp.ReasonCode != "native_command_failed" || resp.ErrorClass != "process" ||
		resp.ExitCode != 47 || resp.Output != native.Diagnostic ||
		!strings.Contains(resp.Error, "list failed") || strings.Contains(resp.Error, "native transport failed") {
		t.Fatalf("native CLI diagnostic affected error classification or disappeared: %+v", resp)
	}
}
