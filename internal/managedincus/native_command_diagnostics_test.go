package managedincus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeCommandFailurePreservesBoundedRedactedDiagnostic(t *testing.T) {
	script := filepath.Join(t.TempDir(), "incus-fake")
	err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'native daemon connection refused password=private123\\n' >&2\nexit 47\n"), 0700)
	if err != nil {
		t.Fatal(err)
	}
	b := Backend{Binary: script}
	_, err = b.List(context.Background(), "default")
	var failure *NativeCommandError
	if !errors.As(err, &failure) || failure.ExitCode != 47 || failure.Stage != "list" ||
		!strings.Contains(failure.Diagnostic, "native daemon connection refused") ||
		!strings.Contains(failure.Diagnostic, "[REDACTED]") ||
		strings.Contains(failure.Diagnostic, "private123") ||
		strings.Contains(err.Error(), "native daemon connection refused") {
		t.Fatalf("native typed error lost evidence or mixed untrusted stderr into Error: %+v / %v", failure, err)
	}
}

func TestNativeCommandLargeFailureReportsTruncation(t *testing.T) {
	script := filepath.Join(t.TempDir(), "incus-fake")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'native failure:' >&2\nhead -c 90000 /dev/zero | tr '\\000' x >&2\nexit 49\n"), 0700); err != nil {
		t.Fatal(err)
	}
	b := Backend{Binary: script}
	_, err := b.List(context.Background(), "default")
	var failure *NativeCommandError
	if !errors.As(err, &failure) || failure.ExitCode != 49 || !failure.Truncated ||
		failure.BytesSeen <= maxOutput || len(failure.Diagnostic) != maxOutput {
		t.Fatalf("native oversized diagnostic metrics: %+v %v", failure, err)
	}
}

func TestReadyFailedNativeQueryPreservesEvidenceWithoutClaimingAvailability(t *testing.T) {
	script := filepath.Join(t.TempDir(), "incus-fake")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'cannot connect native socket password=private123\\n' >&2\nexit 47\n"), 0700); err != nil {
		t.Fatal(err)
	}
	b := Backend{Binary: script}
	ready := b.Ready(context.Background())
	if ready.Available || ready.Status != "unavailable" || ready.ExitCode != 47 ||
		!strings.HasPrefix(ready.Diagnostic, "incus_unready:") ||
		!strings.Contains(ready.Output, "cannot connect native socket") ||
		!strings.Contains(ready.Output, "[REDACTED]") ||
		strings.Contains(ready.Output, "private123") {
		t.Fatalf("native readiness error lost safe evidence or claimed availability: %+v", ready)
	}
}

// Repeated short assignments can become longer than the capture window only
// after masking. Returned bytes must remain bounded while BytesSeen remains
// the actual native stream count.
func TestNativeCommandRedactionExpansionDoesNotInflateRawBytesSeen(t *testing.T) {
	for _, repeats := range []int{8100, 8192, 9000} { // below, at and above native 64 KiB capture
		t.Run(fmt.Sprint(repeats), func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "incus-fake")
			body := fmt.Sprintf("#!/bin/sh\ni=0; while [ \"$i\" -lt %d ]; do printf 'token=a;'; i=$((i+1)); done >&2\nexit 49\n", repeats)
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			b := Backend{Binary: script}
			_, err := b.command(context.Background(), 5*time.Second, "list")
			var native *NativeCommandError
			if !errors.As(err, &native) || native.ExitCode != 49 ||
				native.BytesSeen != int64(8*repeats) || len(native.Diagnostic) != maxOutput ||
				!native.Truncated || native.OmittedBytes <= 0 ||
				strings.Contains(native.Diagnostic, "token=a;") {
				t.Fatalf("post-redaction diagnostic metrics or masking incorrect: %+v %v", native, err)
			}
		})
	}
}

func TestNativeCommandIncompleteQuotedCredentialAtCaptureBoundary(t *testing.T) {
	script := filepath.Join(t.TempDir(), "incus-fake")
	body := "#!/bin/sh\nprintf 'native failure password=\"secret phrase with multiple words ' >&2\nhead -c 71000 /dev/zero | tr '\\000' x >&2\nexit 49\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	b := Backend{Binary: script}
	_, err := b.command(context.Background(), 5*time.Second, "list")
	var native *NativeCommandError
	if !errors.As(err, &native) || native.BytesSeen <= maxOutput ||
		!native.Truncated || native.OmittedBytes <= 0 ||
		len(native.Diagnostic) > maxOutput ||
		strings.Contains(native.Diagnostic, "secret phrase") ||
		!strings.Contains(native.Diagnostic, "native failure") {
		t.Fatalf("truncated quoted secret leaked or native bytes were hidden: %+v %v", native, err)
	}
}

func TestNativeDeadlineCarriesTimeoutAndReadinessEvidence(t *testing.T) {
	script := filepath.Join(t.TempDir(), "incus-fake")
	body := "#!/bin/sh\nprintf 'native timeout source; token=DO_NOT_EXPOSE\\n' >&2\nwhile :; do :; done\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	b := Backend{Binary: script}
	_, err := b.command(context.Background(), 50*time.Millisecond, "list")
	var native *NativeCommandError
	if !errors.As(err, &native) || !native.TimedOut ||
		!strings.Contains(native.Diagnostic, "native timeout source") ||
		strings.Contains(native.Diagnostic, "DO_NOT_EXPOSE") {
		t.Fatalf("deadline failure lost timeout and safe native evidence: %+v %v", native, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ready := b.Ready(ctx)
	if ready.Available || !ready.TimedOut ||
		!strings.Contains(ready.Output, "native timeout source") ||
		ready.ExitCode == 0 {
		t.Fatalf("readiness deadline incorrectly claimed success or lost native evidence: %+v", ready)
	}
}

func TestUncertainCreateAndPoolPreflightPreserveNativeEvidence(t *testing.T) {
	b := fixture(t)
	mode := filepath.Join(b.StateDir, "native.json.mode")
	if err := os.WriteFile(mode, []byte("uncertain_launch_diagnostic"), 0600); err != nil {
		t.Fatal(err)
	}
	req := createRequest()
	result, err := b.Run(context.Background(), req, false, "")
	var native *NativeCommandError
	if !errors.As(err, &native) || !strings.Contains(err.Error(), "create_outcome_uncertain") ||
		!strings.Contains(native.Diagnostic, "launch denied by native daemon") ||
		strings.Contains(native.Diagnostic, "LAUNCHSECRET") || native.ExitCode != 45 ||
		result.Instance != nil {
		t.Fatalf("uncertain native launch lost evidence or claimed completion: %+v %+v %v", result, native, err)
	}
	intents, err := b.loadIntents()
	if err != nil || len(intents.Intents) != 1 || intents.Intents[0].OperationID != req.OperationID || intents.Intents[0].State != "submitted" {
		t.Fatalf("native intent was not durably reserved: %+v %v", intents, err)
	}
	if _, retryErr := b.Run(context.Background(), req, false, ""); retryErr == nil || !strings.Contains(retryErr.Error(), "create_outcome_uncertain") {
		t.Fatalf("duplicate create resubmission not blocked: %v", retryErr)
	}
	guests, err := b.List(context.Background(), req.Project)
	if err != nil || len(guests) != 0 {
		t.Fatalf("uncertain create generated duplicate native guest: %+v %v", guests, err)
	}
	if err := os.WriteFile(mode, []byte("pool_unavailable_diagnostic"), 0600); err != nil {
		t.Fatal(err)
	}
	poolErr := b.checkNativePool(context.Background(), "default", 1)
	native = nil
	if !errors.As(poolErr, &native) ||
		!strings.Contains(poolErr.Error(), "incus_storage_unavailable") ||
		native.ExitCode != 43 ||
		!strings.Contains(native.Diagnostic, "selected pool storage driver unavailable") ||
		strings.Contains(native.Diagnostic, "POOLSECRET") {
		t.Fatalf("storage preflight discarded safe native evidence: %+v %v", native, poolErr)
	}
}

func TestCreateLaunchAndReconciliationFailuresPreserveBothStagesAndIntent(t *testing.T) {
	b := fixture(t)
	modePath := filepath.Join(b.StateDir, "native.json.mode")
	if err := os.WriteFile(modePath, []byte("launch_and_inspect_fail"), 0600); err != nil {
		t.Fatal(err)
	}
	req := createRequest()
	result, err := b.Run(context.Background(), req, false, "")
	if err == nil || !strings.Contains(err.Error(), "create_outcome_uncertain") ||
		result.Instance != nil {
		t.Fatalf("unreconciled create falsely reported success: %+v %v", result, err)
	}
	var nativeLaunch *NativeCommandError
	if !errors.As(err, &nativeLaunch) || nativeLaunch.Stage != "launch" ||
		nativeLaunch.ExitCode != 45 || !strings.Contains(nativeLaunch.Diagnostic, "native launch stage failed") ||
		strings.Contains(nativeLaunch.Diagnostic, "LAUNCH_SECRET") {
		t.Fatalf("launch failure evidence missing/unsafe: %+v %v", nativeLaunch, err)
	}
	joined := errors.Unwrap(err)
	branch, ok := joined.(interface{ Unwrap() []error })
	if !ok || len(branch.Unwrap()) != 2 {
		t.Fatalf("both launch/reconciliation errors not preserved in chain: %T %v", joined, err)
	}
	reconcile, ok := branch.Unwrap()[1].(*NativeCommandError)
	if !ok || reconcile.Stage != "list" || reconcile.ExitCode != 46 ||
		!strings.Contains(reconcile.Diagnostic, "native reconciliation list failed") ||
		strings.Contains(reconcile.Diagnostic, "RECONCILE_SECRET") {
		t.Fatalf("reconciliation evidence missing/unsafe: %+v", reconcile)
	}
	journal, loadErr := b.loadIntents()
	if loadErr != nil || len(journal.Intents) != 1 ||
		journal.Intents[0].State != "submitted" || journal.Intents[0].OperationID != req.OperationID {
		t.Fatalf("unverified create did not reserve original intent: %+v %v", journal, loadErr)
	}
	if err := os.Remove(modePath); err != nil {
		t.Fatal(err)
	}
	if _, retryErr := b.Run(context.Background(), req, false, ""); retryErr == nil ||
		!strings.Contains(retryErr.Error(), "create_outcome_uncertain") {
		t.Fatalf("reconnect repeated uncertain create: %v", retryErr)
	}
	list, listErr := b.List(context.Background(), req.Project)
	if listErr != nil || len(list) != 0 {
		t.Fatalf("reconnect launched a duplicate guest: %+v %v", list, listErr)
	}
}

func TestPostLaunchIdentityCollisionPreservesNativeLaunchFailure(t *testing.T) {
	b := fixture(t)
	if err := os.WriteFile(filepath.Join(b.StateDir, "native.json.mode"), []byte("launch_then_identity_collision"), 0600); err != nil {
		t.Fatal(err)
	}
	req := createRequest()
	result, err := b.Run(context.Background(), req, false, "")
	var native *NativeCommandError
	if err == nil || !strings.Contains(err.Error(), "create_collision_after_launch") ||
		!errors.As(err, &native) || native.Stage != "launch" || native.ExitCode != 45 ||
		!strings.Contains(native.Diagnostic, "native launch collision failure") ||
		strings.Contains(native.Diagnostic, "LAUNCH_SECRET") || result.Instance != nil {
		t.Fatalf("post-launch identity failure discarded the launch stage: %+v %+v %v", result, native, err)
	}
	journal, loadErr := b.loadIntents()
	if loadErr != nil || len(journal.Intents) != 1 ||
		journal.Intents[0].State != "submitted" || journal.Intents[0].OperationID != req.OperationID {
		t.Fatalf("collision silently confirmed or dropped durable create intent: %+v %v", journal, loadErr)
	}
	guests, listErr := b.List(context.Background(), req.Project)
	if listErr != nil || len(guests) != 1 || guests[0].Managed {
		t.Fatalf("collision was not identified as foreign guest: %+v %v", guests, listErr)
	}
	if _, retryErr := b.Run(context.Background(), req, false, ""); retryErr == nil ||
		!strings.Contains(retryErr.Error(), "instance_collision") {
		t.Fatalf("collision retry was not refused: %v", retryErr)
	}
	again, listErr := b.List(context.Background(), req.Project)
	if listErr != nil || len(again) != 1 {
		t.Fatalf("collision retry changed native guest inventory: %+v %v", again, listErr)
	}
}

func TestGuestExecSecondIdentityPreflightNativeFailureKeepsEvidence(t *testing.T) {
	b := fixture(t)
	created, err := b.Run(context.Background(), createRequest(), false, "")
	if err != nil || created.Instance == nil {
		t.Fatalf("fixture guest create failed: %+v %v", created, err)
	}
	mode := filepath.Join(b.StateDir, "native.json.mode")
	b.beforeMutation = func(Request) {
		if err := os.WriteFile(mode, []byte("list_unavailable"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.Run(context.Background(), Request{
		Operation: "exec", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID,
		Command: []string{"true"},
	}, false, "")
	var native *NativeCommandError
	if err == nil || !strings.Contains(err.Error(), "native_identity_preflight_unavailable") ||
		!errors.As(err, &native) || native.Stage != "list" || native.ExitCode != 48 ||
		got.GuestExecAttempted {
		t.Fatalf("second pre-dispatch native error was lost or guest command claimed: %+v %+v %v", got, native, err)
	}
}
