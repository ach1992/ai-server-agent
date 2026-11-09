package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/managedincus"
)

func TestNativeUEFIFirmwareFailureProvidesRecoveryWithoutRetryingCreate(t *testing.T) {
	raw := "Unable to run feature checks during QEMU initialization: Unable to locate a UEFI firmware [REDACTED]"
	native := &managedincus.NativeCommandError{
		Stage: "launch", ExitCode: 45, Diagnostic: raw, BytesSeen: int64(len(raw)),
		Cause: errors.New("exit status 45"),
	}
	got := managedErr(fmt.Errorf("create_outcome_uncertain: native create submitted and no guest visible: %w", native))
	if got.OK || got.ErrorCode != "outcome_uncertain" || got.ErrorClass == "dependency" ||
		strings.Contains(got.OperatorHint, "firmware was not found at common paths") ||
		got.PrerequisiteCode != "uefi_firmware_not_located" ||
		!strings.Contains(got.OperatorHint, "ovmf") ||
		!strings.Contains(got.OperatorHint, "pending_creates") ||
		strings.Contains(got.OperatorHint, "operation_status") ||
		got.ExitCode != 45 || got.Output != raw || got.BytesSeen != int64(len(raw)) {
		t.Fatalf("UEFI dependency was mistaken for permanent unsupported/known-safe retry: %+v", got)
	}
	other := &managedincus.NativeCommandError{
		Stage: "launch", ExitCode: 44, Diagnostic: "QEMU: device initialization failed for another reason",
		BytesSeen: 57, Cause: errors.New("exit status 44"),
	}
	// A firmware failure can also occur during start of an existing VM.
	// The hint must not invent a create operation or a new operation_id.
	startFailure := managedErr(native)
	if startFailure.ErrorCode != "native_command_failed" ||
		!strings.Contains(startFailure.OperatorHint, "For start, inspect the existing guest") ||
		strings.Contains(startFailure.OperatorHint, "operation_status") ||
		strings.Contains(startFailure.OperatorHint, "VM creation") ||
		startFailure.ExitCode != 45 || startFailure.Output != raw {
		t.Fatalf("firmware hint assumed a create-only failure: %+v", startFailure)
	}
	unknown := managedErr(fmt.Errorf("create_outcome_uncertain: native create cannot be reconciled: %w", other))
	if unknown.PrerequisiteCode != "" || unknown.OperatorHint != "" ||
		unknown.ErrorCode != "outcome_uncertain" {
		t.Fatalf("unrelated native failure misdiagnosed as UEFI: %+v", unknown)
	}
}

func TestUncertainNativeOperationIDRemainsAvailableForRecovery(t *testing.T) {
	request := managedincus.Request{Operation: "create", OperationID: "issue245-create-recovery"}
	uncertain := withUncertainNativeOperationID(
		managedErr(errors.New("create_outcome_uncertain: submitted but guest not yet observed")), request)
	if uncertain.ErrorCode != "outcome_uncertain" ||
		uncertain.OperationID != request.OperationID ||
		uncertain.OperatorHint != "" {
		t.Fatalf("uncertain native operation lost its submitted identity: %+v", uncertain)
	}

	// A failed preflight must not be mistaken for a submitted operation.
	preflight := withUncertainNativeOperationID(
		managedErr(errors.New("incus_storage_unavailable: preflight failed")), request)
	if preflight.OperationID != "" || preflight.ErrorCode != "dependency_unavailable" {
		t.Fatalf("preflight acquired misleading submitted-operation identity: %+v", preflight)
	}
	previous := withUncertainNativeOperationID(
		Response{ErrorCode: "outcome_uncertain", OperationID: "existing-id"}, request)
	if previous.OperationID != "existing-id" {
		t.Fatalf("existing correlation ID overwritten: %+v", previous)
	}
}

func TestNativeIncusReadinessDistinguishesMissingBinaryAndDaemon(t *testing.T) {
	for _, tc := range []struct{ diagnostic, want string }{
		{"incus_missing: no trusted Incus binary", "incus_cli_not_detected"},
		{"incus_unready: daemon connection failed", "incus_daemon_unready"},
		{"unexpected native readiness error", "incus_preflight_unverified"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			resp := managedUnavailableResult(managedincus.Result{Diagnostic: tc.diagnostic, ExitCode: 47,
				Output: "native error", OutputBytesSeen: 12})
			if resp.OK || resp.PrerequisiteCode != tc.want || resp.ErrorCode != "incus_unavailable" ||
				resp.ErrorClass != "dependency" || resp.Output != "native error" || resp.ExitCode != 47 ||
				!strings.Contains(resp.OperatorHint, "managed_environment") || strings.Contains(resp.OperatorHint, "incus admin status") {
				t.Fatalf("native missing/daemon readiness diagnostics wrong: %+v", resp)
			}
		})
	}
}
