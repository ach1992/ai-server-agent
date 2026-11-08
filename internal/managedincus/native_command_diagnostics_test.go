package managedincus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
