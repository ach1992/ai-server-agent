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
