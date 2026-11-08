package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	guest := managedGuestExecFailure(managedincus.Result{GuestExecAttempted: true, TimedOut: true, ExitCode: -1},
		errors.New("native operation failed, exit_code=-1: signal: killed"))
	if guest.OK || !guest.TimedOut || guest.ErrorCode != "native_guest_exec_failed" {
		t.Fatalf("timed-out native guest exec wrongly reported success: %+v", guest)
	}
}

// Native identity discovery happens before guest exec. Its own CLI failure
// must remain visible rather than being replaced by an empty guest Result.
func TestManagedGuestExecPreflightNativeListFailureKeepsNativeDiagnostic(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "incus-fake")
	execMarker := filepath.Join(dir, "guest-exec-invoked")
	code := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--force-local\" ]; then shift; fi\ncase \"$1\" in\n  query) printf '{\"api_version\":\"1.0\"}\\n';;\n  list)\n    printf 'native identity discovery refused password=\"never expose these words\"; inspect socket\\n' >&2\n    head -c 80000 /dev/zero | tr '\\000' x >&2\n    exit 47;;\n  exec) touch %q; exit 0;;\n  *) exit 44;;\nesac\n", execMarker)
	if err := os.WriteFile(script, []byte(code), 0700); err != nil {
		t.Fatal(err)
	}
	b := managedincus.Backend{Binary: script, StateDir: dir}
	result, err := b.Run(context.Background(), managedincus.Request{
		Operation: "exec", Project: "default", Instance: "guest-a",
		UUID:    "12345678-1234-1234-1234-123456789abc",
		Command: []string{"true"},
	}, false, "")
	var native *managedincus.NativeCommandError
	if err == nil || !errors.As(err, &native) || native.Stage != "list" ||
		result.GuestExecAttempted {
		t.Fatalf("preflight failure was reported as guest-exec dispatch: result=%+v error=%v", result, err)
	}
	resp := managedGuestExecFailure(result, err)
	if resp.OK || resp.ErrorCode != "native_command_failed" || resp.ExitCode != 47 ||
		!strings.Contains(resp.Error, "list failed") || !strings.Contains(resp.Output, "native identity discovery refused") ||
		strings.Contains(resp.Output, "never expose these words") || !resp.Truncated ||
		resp.BytesSeen <= 64<<10 || resp.BytesReturned != int64(len(resp.Output)) ||
		resp.OmittedBytes <= 0 {
		t.Fatalf("preflight native stderr/exit were overwritten: %+v", resp)
	}
	if _, statErr := os.Stat(execMarker); !os.IsNotExist(statErr) {
		t.Fatalf("guest exec unexpectedly dispatched: %v", statErr)
	}
}

func TestManagedOuterRecoveryReasonOverridesNestedNativeMarker(t *testing.T) {
	raw := "CLI binary disappeared after readiness; inspect native installation"
	native := &managedincus.NativeCommandError{
		Stage: "launch", ExitCode: -1, Diagnostic: raw,
		BytesSeen: int64(len(raw)), Cause: errors.New("incus_missing: executable vanished"),
	}
	cases := []struct {
		name, reason, want string
	}{
		{"submitted-intent", "create_outcome_uncertain: durable create intent remains reserved", "outcome_uncertain"},
		{"storage-preflight", "incus_storage_unavailable: native pool query failed", "dependency_unavailable"},
		{"post-launch-collision", "create_collision_after_launch: unexpected native identity", "identity_conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := managedErr(fmt.Errorf("%s: %w", tc.reason, native))
			if resp.OK || resp.ErrorCode != tc.want || resp.ReasonCode != tc.want ||
				resp.ExitCode != -1 || resp.Output != raw || resp.BytesSeen != int64(len(raw)) ||
				!strings.Contains(resp.Error, tc.reason) {
				t.Fatalf("subordinate incus_missing displaced authoritative recovery: %+v", resp)
			}
		})
	}
	direct := managedErr(native)
	if direct.ErrorCode != "resource_missing" || direct.Output != raw || direct.ExitCode != -1 {
		t.Fatalf("unwrapped native missing-resource error lost its existing classification: %+v", direct)
	}
}

func TestManagedJoinedNativeLaunchAndInspectionBothRemainActionable(t *testing.T) {
	launch := &managedincus.NativeCommandError{
		Stage: "launch", ExitCode: 45, Diagnostic: "launch native failure; inspect storage pool",
		BytesSeen: 100, OmittedBytes: 12, Truncated: true,
		Cause: errors.New("incus_missing: binary vanished"),
	}
	reconcile := &managedincus.NativeCommandError{
		Stage: "list", ExitCode: 46, Diagnostic: "native reconciliation failed; cannot read guest state",
		BytesSeen: 120, TimedOut: true,
		Cause: errors.New("exit status 46"),
	}
	err := fmt.Errorf("create_outcome_uncertain: native state could not be reconciled: %w", errors.Join(launch, reconcile))
	resp := managedErr(err)
	if resp.OK || resp.ErrorCode != "outcome_uncertain" ||
		resp.ExitCode != 45 || !resp.TimedOut || !resp.Truncated ||
		resp.BytesSeen != 220 || resp.OmittedBytes < 12 ||
		resp.BytesReturned != int64(len(resp.Output)) {
		t.Fatalf("native multi-stage response lost authoritative metadata: %+v", resp)
	}
	for _, snippet := range []string{
		"native_stage=\"launch\" exit_code=45", "native_stage=\"list\" exit_code=46",
		"launch native failure", "native reconciliation failed",
	} {
		if !strings.Contains(resp.Output, snippet) {
			t.Fatalf("native stage missing: %s: %+v", snippet, resp)
		}
	}
	// Both maximum windows may contain escaped control bytes. The framed
	// diagnostic must remain finite and include both recovery stages.
	launch.Diagnostic = strings.Repeat("\x00", 64<<10)
	reconcile.Diagnostic = strings.Repeat("\t", 64<<10)
	huge := managedErr(err)
	if len(huge.Output) > 64<<10 || !huge.Truncated || huge.OmittedBytes <= 12 ||
		!strings.Contains(huge.Output, "native_stage=\"launch\"") ||
		!strings.Contains(huge.Output, "native_stage=\"list\"") {
		t.Fatalf("combined native diagnostics exceeded budget or lost stages: %+v", huge)
	}
}
