package executor

import (
	"errors"
	"fmt"
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

func TestManagedNativeWrappedErrorsPreserveRecoveryClassificationAndTimeout(t *testing.T) {
	raw := "native daemon unavailable"
	native := &managedincus.NativeCommandError{
		Stage: "launch", ExitCode: 45, Diagnostic: raw,
		BytesSeen: int64(len(raw)), OmittedBytes: 4, Truncated: true,
		TimedOut: true, Cause: errors.New("signal: killed"),
	}
	for _, tc := range []struct {
		name, prefix, want string
	}{
		{"create", "create_outcome_uncertain: submission remains durably reserved", "outcome_uncertain"},
		{"storage", "incus_storage_unavailable: pool resources could not be read", "dependency_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := managedErr(fmt.Errorf("%s: %w", tc.prefix, native))
			if got.OK || got.ErrorCode != tc.want || got.ReasonCode != tc.want ||
				!got.TimedOut || got.ExitCode != 45 || got.BytesSeen != int64(len(raw)) ||
				got.BytesReturned != int64(len(raw)) || got.OmittedBytes != 4 || !got.Truncated ||
				got.Output != raw {
				t.Fatalf("wrapped native diagnostic displaced high-level recovery semantics: %+v", got)
			}
		})
	}
	ready := managedUnavailableResult(managedincus.Result{
		Diagnostic: "incus_unready: investigate daemon", ExitCode: -1,
		TimedOut: true, Output: raw, OutputBytesSeen: int64(len(raw)),
		OutputTruncated: true, OutputOmittedBytes: 12,
	})
	if ready.OK || ready.ErrorCode != "incus_unavailable" ||
		!ready.TimedOut || ready.Status != "native_preflight_unavailable" ||
		ready.BytesSeen != int64(len(raw)) || ready.OmittedBytes != 12 ||
		!ready.Truncated || ready.Output != raw {
		t.Fatalf("failed preflight lost timeout/native evidence: %+v", ready)
	}
}

func TestManagedStatusAndGuestExecExposeNativeTimeoutWithoutFalseSuccess(t *testing.T) {
	status := managedJSON(managedincus.Result{Status: "unavailable", Available: false, TimedOut: true})
	if !status.OK || !status.TimedOut || !strings.Contains(status.Output, "unavailable") {
		t.Fatalf("native status observation lost deadline metadata: %+v", status)
	}
	guest := managedGuestExecFailure(managedincus.Result{TimedOut: true, ExitCode: -1},
		errors.New("native operation failed, exit_code=-1: signal: killed"))
	if guest.OK || !guest.TimedOut || guest.ErrorCode != "native_guest_exec_failed" {
		t.Fatalf("timed-out native guest exec wrongly reported success: %+v", guest)
	}
}
