package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editPtr(s string) *string { return &s }

func TestWorkspaceEditBatchPreflightAndPartialOutcomes(t *testing.T) {
	root, repo, _ := workspaceFixture(t)
	a := filepath.Join(repo, "src", "a.txt")
	b := filepath.Join(repo, "src", "b.txt")
	if err := os.WriteFile(a, []byte("alpha one"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("beta two"), 0644); err != nil {
		t.Fatal(err)
	}
	version := func(path string) string {
		r := readWorkerWorkspaceFile(workspaceFileOperation{WorkspaceRoot: root, Workspace: repo, Path: "src/" + filepath.Base(path)})
		if !r.OK {
			t.Fatalf("read version %q: %+v", path, r)
		}
		return r.FileVersion
	}
	makeOp := func() workspaceFileOperation {
		return workspaceFileOperation{
			Action: "workspace_apply_edits", WorkspaceRoot: root, Workspace: repo,
			Edits: []WorkspaceFileEdit{
				{Path: "src/a.txt", FileVersion: version(a), Replacements: []WorkspaceReplace{{Old: "one", New: "ONE"}}},
				{Path: "src/b.txt", FileVersion: version(b), Replacements: []WorkspaceReplace{{Old: "two", New: "TWO"}}},
			},
		}
	}
	mismatch := makeOp()
	mismatch.Edits[1].Replacements[0].Old = "missing"
	bad := applyWorkerWorkspaceEdits(mismatch)
	if bad.OK || bad.ErrorCode != "workspace_preflight_failed" {
		t.Fatalf("preflight incorrectly accepted: %+v", bad)
	}
	outcome, err := decodeWorkspaceEditResult(bad)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Applied != 0 || outcome.Files[0].Status != "not_attempted" || outcome.Files[1].ErrorCode != "patch_context_mismatch" {
		t.Fatalf("preflight outcome incorrect: %+v", outcome)
	}
	before, _ := os.ReadFile(a)
	if string(before) != "alpha one" {
		t.Fatal("first file mutated before full batch preflight")
	}

	op := makeOp()
	success := applyWorkerWorkspaceEdits(op)
	if !success.OK || success.Status != "complete" {
		t.Fatalf("batch success: %+v", success)
	}
	complete, err := decodeWorkspaceEditResult(success)
	if err != nil {
		t.Fatal(err)
	}
	if !complete.Complete || complete.Applied != 2 || complete.Files[0].FileVersion == "" || complete.Files[1].FileVersion == "" {
		t.Fatalf("batch success outcome: %+v", complete)
	}
	first, _ := os.ReadFile(a)
	second, _ := os.ReadFile(b)
	if string(first) != "alpha ONE" || string(second) != "beta TWO" {
		t.Fatalf("wrong edits: %q %q", first, second)
	}
	stale := applyWorkerWorkspaceEdits(op)
	if stale.OK || stale.ErrorCode != "workspace_preflight_failed" {
		t.Fatalf("stale edit accepted: %+v", stale)
	}

	// Fresh versions; the second file changes after full preflight but before
	// its commit. The first change must be reported, not rolled back.
	partialOp := makeOp()
	partialOp.Edits[0].Replacements[0] = WorkspaceReplace{Old: "ONE", New: "one"}
	partialOp.Edits[1].Replacements[0] = WorkspaceReplace{Old: "TWO", New: "two"}
	partial := applyWorkerWorkspaceEditsWithHook(partialOp, func(i int) {
		if i == 1 {
			if err := os.WriteFile(b, []byte("external edit"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	})
	if partial.OK || partial.ErrorCode != "partial_workspace_edit" || partial.Status != "partial" {
		t.Fatalf("failed to report partial commit: %+v", partial)
	}
	outcome, err = decodeWorkspaceEditResult(partial)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Complete || !outcome.Partial || outcome.Applied != 1 || outcome.Files[0].Status != "applied" || outcome.Files[1].Status != "failed" || outcome.Files[1].ErrorCode != "file_changed" {
		t.Fatalf("partial outcomes wrong: %+v", outcome)
	}
	first, _ = os.ReadFile(a)
	second, _ = os.ReadFile(b)
	if string(first) != "alpha one" || string(second) != "external edit" {
		t.Fatalf("unsafe partial results: %q %q", first, second)
	}
}

func TestWorkspaceEditBatchRejectsDuplicatesOversizeAndExternalPaths(t *testing.T) {
	root, repo, _ := workspaceFixture(t)
	original := filepath.Join(repo, "src", "a.txt")
	if err := os.WriteFile(original, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	version := readWorkerWorkspaceFile(workspaceFileOperation{WorkspaceRoot: root, Workspace: repo, Path: "src/a.txt"}).FileVersion
	input := WorkspaceFileEdit{Path: "src/a.txt", FileVersion: version, Content: editPtr("modified")}
	dup := applyWorkerWorkspaceEdits(workspaceFileOperation{WorkspaceRoot: root, Workspace: repo, Edits: []WorkspaceFileEdit{input, input}})
	if dup.OK || dup.ErrorCode != "workspace_preflight_failed" {
		t.Fatalf("duplicate edits accepted: %+v", dup)
	}
	oversized := applyWorkerWorkspaceEdits(workspaceFileOperation{WorkspaceRoot: root, Workspace: repo, Edits: []WorkspaceFileEdit{{Path: "src/a.txt", FileVersion: version, Content: editPtr(strings.Repeat("x", maxFileWriteBytes+1))}}})
	if oversized.OK {
		t.Fatal("oversized batch accepted")
	}
	escape := applyWorkerWorkspaceEdits(workspaceFileOperation{WorkspaceRoot: root, Workspace: repo, Edits: []WorkspaceFileEdit{input, {Path: "../../escaped.txt", MustNotExist: true, Content: editPtr("escaped")}}})
	if escape.OK {
		t.Fatalf("unsafe path batch accepted: %+v", escape)
	}
	result, err := os.ReadFile(original)
	if err != nil || string(result) != "original" {
		t.Fatalf("preflight rejection changed file: %v %q", err, result)
	}
}

// A create can lose a race after the batch-wide preflight. Never overwrite
// the other writer or claim that the entire batch rolled back; a retry must
// reconcile the already-applied edit before planning new changes.
func TestWorkspaceEditBatchConcurrentCreateReconciliation(t *testing.T) {
	root, repo, _ := workspaceFixture(t)
	firstPath := filepath.Join(repo, "src", "a.txt")
	newPath := filepath.Join(repo, "src", "new.txt")
	if err := os.WriteFile(firstPath, []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	initial := readWorkerWorkspaceFile(workspaceFileOperation{
		WorkspaceRoot: root, Workspace: repo, Path: "src/a.txt",
	})
	if !initial.OK || initial.FileVersion == "" {
		t.Fatalf("initial version unavailable: %+v", initial)
	}
	batch := workspaceFileOperation{
		Action: "workspace_apply_edits", WorkspaceRoot: root, Workspace: repo,
		Edits: []WorkspaceFileEdit{
			{Path: "src/a.txt", FileVersion: initial.FileVersion, Replacements: []WorkspaceReplace{{Old: "before", New: "after"}}},
			{Path: "src/new.txt", MustNotExist: true, Content: editPtr("our new file")},
		},
	}
	// A pre-existing destination is detected before the first write.
	if err := os.WriteFile(newPath, []byte("pre-existing"), 0600); err != nil {
		t.Fatal(err)
	}
	preflight := applyWorkerWorkspaceEdits(batch)
	if preflight.OK || preflight.ErrorCode != "workspace_preflight_failed" {
		t.Fatalf("existing destination accepted: %+v", preflight)
	}
	if got, err := os.ReadFile(firstPath); err != nil || string(got) != "before" {
		t.Fatalf("preflight unexpectedly changed first file: %v %q", err, got)
	}
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}

	// Race: the destination appears after preflight, just before commit 2.
	raced := applyWorkerWorkspaceEditsWithHook(batch, func(i int) {
		if i == 1 {
			if err := os.WriteFile(newPath, []byte("concurrent writer"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if raced.OK || raced.Status != "partial" || raced.ErrorCode != "partial_workspace_edit" {
		t.Fatalf("create race not truthfully reported: %+v", raced)
	}
	outcome, err := decodeWorkspaceEditResult(raced)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Partial || outcome.Applied != 1 || len(outcome.Files) != 2 ||
		outcome.Files[0].Status != "applied" || outcome.Files[0].FileVersion == "" ||
		outcome.Files[1].Status != "failed" || outcome.Files[1].ErrorCode != "file_exists" {
		t.Fatalf("incorrect per-file race outcome: %+v", outcome)
	}
	for path, want := range map[string]string{firstPath: "after", newPath: "concurrent writer"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("race clobbered %s: %v %q", path, err, got)
		}
	}
	// Replaying a stale batch must not undo the applied change or overwrite
	// the concurrent writer; fresh versions are required to resume safely.
	retry := applyWorkerWorkspaceEdits(batch)
	if retry.OK || retry.ErrorCode != "workspace_preflight_failed" {
		t.Fatalf("stale retry unexpectedly accepted: %+v", retry)
	}
	for path, want := range map[string]string{firstPath: "after", newPath: "concurrent writer"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("stale retry clobbered %s: %v %q", path, err, got)
		}
	}
}
