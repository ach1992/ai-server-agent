package systemexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func selectionFixtureBytes(t *testing.T, dir, name, suffix string, want []byte) {
	t.Helper()
	got, err := readAdapterStateBytes(adapterStatePath(dir, name, suffix))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s: got %q want %q: %v", suffix, got, want, err)
	}
}

func putSelectionFixture(t *testing.T, dir, name, suffix string, raw []byte) {
	t.Helper()
	if err := putAdapterStateBytes(adapterStatePath(dir, name, suffix), raw, 0644); err != nil {
		t.Fatal(err)
	}
}

func freshProcessAdapterRecovery(t *testing.T, dir, name string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdapterSelectionFreshProcessRecovery$")
	cmd.Env = append(os.Environ(), "AGENT_ADAPTER_SELECTION_FIXTURE="+dir, "AGENT_ADAPTER_SELECTION_NAME="+name)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh-process recovery: %v\n%s", err, output)
	}
}

func TestAdapterSelectionFreshProcessRecovery(t *testing.T) {
	dir := os.Getenv("AGENT_ADAPTER_SELECTION_FIXTURE")
	if dir == "" {
		t.Skip("only invoked as a fresh-process lifecycle fixture")
	}
	if err := reconcileAdapterSelection(dir, os.Getenv("AGENT_ADAPTER_SELECTION_NAME")); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterSelectionLifecycleHistory(t *testing.T) {
	const name = "sql-lsp"
	a, b, c := []byte("profile A"), []byte("profile B"), []byte("profile C")
	t.Run("builtin-to-A-rollback-to-builtin", func(t *testing.T) {
		dir := t.TempDir()
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{AfterActive: a}); err != nil {
			t.Fatal(err)
		}
		selectionFixtureBytes(t, dir, name, "", a)
		selectionFixtureBytes(t, dir, name, ".previous", nil)
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{
			BeforeActive: a, SaveInactive: true, AfterInactive: a,
		}); err != nil {
			t.Fatal(err)
		}
		selectionFixtureBytes(t, dir, name, "", nil)
		selectionFixtureBytes(t, dir, name, ".previous", nil)
		selectionFixtureBytes(t, dir, name, ".inactive", a)
	})
	t.Run("A-to-B-rollback-to-A-and-retry", func(t *testing.T) {
		dir := t.TempDir()
		putSelectionFixture(t, dir, name, "", a)
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{
			BeforeActive: a, AfterActive: b, AfterPrevious: a,
		}); err != nil {
			t.Fatal(err)
		}
		selectionFixtureBytes(t, dir, name, ".previous", a)
		// Identical activation retry must not overwrite rollback history.
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{
			BeforeActive: b, AfterActive: b, AfterPrevious: c,
		}); err != nil {
			t.Fatal(err)
		}
		selectionFixtureBytes(t, dir, name, ".previous", a)
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{
			BeforeActive: b, AfterActive: a, AfterPrevious: b,
		}); err != nil {
			t.Fatal(err)
		}
		selectionFixtureBytes(t, dir, name, "", a)
		selectionFixtureBytes(t, dir, name, ".previous", b)
	})
	t.Run("deactivate-invalidates-stale-previous-history", func(t *testing.T) {
		dir := t.TempDir()
		putSelectionFixture(t, dir, name, "", a)
		putSelectionFixture(t, dir, name, ".previous", c)
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{
			BeforeActive: a, AfterActive: b, AfterPrevious: a,
		}); err != nil {
			t.Fatal(err)
		}
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{
			BeforeActive: b, SaveInactive: true, AfterInactive: b,
		}); err != nil {
			t.Fatal(err)
		}
		selectionFixtureBytes(t, dir, name, "", nil)
		selectionFixtureBytes(t, dir, name, ".previous", nil)
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{
			AfterActive: c,
		}); err != nil {
			t.Fatal(err)
		}
		selectionFixtureBytes(t, dir, name, ".previous", nil)
		if err := switchAdapterSelection(dir, name, adapterSelectionTransition{
			BeforeActive: c, SaveInactive: true, AfterInactive: c,
		}); err != nil {
			t.Fatal(err)
		}
		selectionFixtureBytes(t, dir, name, "", nil)
		selectionFixtureBytes(t, dir, name, ".previous", nil)
		selectionFixtureBytes(t, dir, name, ".inactive", c)
	})
}

func pendingAdapterSelectionFixture(t *testing.T, dir, name string, tx adapterSelectionTransition) {
	t.Helper()
	tx.Name = name
	raw, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := putAdapterStateBytes(adapterStatePath(dir, name, ".pending"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterSelectionCrashRecoveryAcrossFreshProcesses(t *testing.T) {
	const name = "python-dap"
	a, b, c, unexpected := []byte("valid A"), []byte("valid B"), []byte("valid C"), []byte("unknown candidate")
	t.Run("prepared-but-not-committed-preserves-old-history", func(t *testing.T) {
		dir := t.TempDir()
		putSelectionFixture(t, dir, name, "", a)
		putSelectionFixture(t, dir, name, ".previous", b)
		pendingAdapterSelectionFixture(t, dir, name, adapterSelectionTransition{BeforeActive: a, AfterActive: c, AfterPrevious: a})
		// A crash here must NOT destroy B, since C never became active.
		selectionFixtureBytes(t, dir, name, ".previous", b)
		freshProcessAdapterRecovery(t, dir, name)
		selectionFixtureBytes(t, dir, name, "", a)
		selectionFixtureBytes(t, dir, name, ".previous", b)
		selectionFixtureBytes(t, dir, name, ".pending", nil)
	})
	t.Run("committed-but-before-previous-update", func(t *testing.T) {
		dir := t.TempDir()
		putSelectionFixture(t, dir, name, "", a)
		putSelectionFixture(t, dir, name, ".previous", b)
		pendingAdapterSelectionFixture(t, dir, name, adapterSelectionTransition{BeforeActive: a, AfterActive: c, AfterPrevious: a})
		putSelectionFixture(t, dir, name, "", c)
		freshProcessAdapterRecovery(t, dir, name)
		selectionFixtureBytes(t, dir, name, "", c)
		selectionFixtureBytes(t, dir, name, ".previous", a)
		selectionFixtureBytes(t, dir, name, ".pending", nil)
	})
	t.Run("committed-and-previous-updated-but-not-finalized", func(t *testing.T) {
		dir := t.TempDir()
		putSelectionFixture(t, dir, name, "", a)
		putSelectionFixture(t, dir, name, ".previous", b)
		pendingAdapterSelectionFixture(t, dir, name, adapterSelectionTransition{BeforeActive: a, AfterActive: c, AfterPrevious: a})
		putSelectionFixture(t, dir, name, "", c)
		putSelectionFixture(t, dir, name, ".previous", a)
		freshProcessAdapterRecovery(t, dir, name)
		selectionFixtureBytes(t, dir, name, "", c)
		selectionFixtureBytes(t, dir, name, ".previous", a)
		selectionFixtureBytes(t, dir, name, ".pending", nil)
	})
	t.Run("committed-deactivation-before-history-cleanup", func(t *testing.T) {
		dir := t.TempDir()
		putSelectionFixture(t, dir, name, "", a)
		putSelectionFixture(t, dir, name, ".previous", b)
		pendingAdapterSelectionFixture(t, dir, name, adapterSelectionTransition{
			BeforeActive: a, SaveInactive: true, AfterInactive: a,
		})
		putSelectionFixture(t, dir, name, "", nil)
		freshProcessAdapterRecovery(t, dir, name)
		selectionFixtureBytes(t, dir, name, "", nil)
		selectionFixtureBytes(t, dir, name, ".previous", nil)
		selectionFixtureBytes(t, dir, name, ".inactive", a)
	})
	t.Run("unexpected-active-does-not-silently-fall-back", func(t *testing.T) {
		dir := t.TempDir()
		putSelectionFixture(t, dir, name, "", a)
		putSelectionFixture(t, dir, name, ".previous", b)
		pendingAdapterSelectionFixture(t, dir, name, adapterSelectionTransition{BeforeActive: a, AfterActive: c, AfterPrevious: a})
		putSelectionFixture(t, dir, name, "", unexpected)
		if err := reconcileAdapterSelection(dir, name); err == nil || !strings.Contains(err.Error(), "manual reconciliation") {
			t.Fatalf("unexpected active accepted: %v", err)
		}
		selectionFixtureBytes(t, dir, name, "", unexpected)
		selectionFixtureBytes(t, dir, name, ".previous", b)
		if _, err := os.Stat(adapterStatePath(dir, name, ".pending")); err != nil {
			t.Fatal(fmt.Errorf("unresolved recovery must remain visible: %w", err))
		}
	})
	t.Run("malformed-intent-does-not-change-selection", func(t *testing.T) {
		dir := t.TempDir()
		putSelectionFixture(t, dir, name, "", a)
		putSelectionFixture(t, dir, name, ".previous", b)
		putSelectionFixture(t, dir, name, ".pending", []byte("{bad JSON"))
		if err := reconcileAdapterSelection(dir, name); err == nil {
			t.Fatal("malformed pending transition accepted")
		}
		selectionFixtureBytes(t, dir, name, "", a)
		selectionFixtureBytes(t, dir, name, ".previous", b)
	})
}
