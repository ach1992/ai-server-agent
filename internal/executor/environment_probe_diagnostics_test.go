package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentGitPreservesNativeExitAndStderr(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{"README.md": "example"})
	_, code, err := s.environmentGit(context.Background(), repo, "rev-parse", "--verify", "refs/heads/no-such-branch")
	if code == 0 || err == nil ||
		!strings.Contains(err.Error(), "git rev-parse exited") ||
		!strings.Contains(err.Error(), "stderr=") {
		t.Fatalf("Git native failure lacks its stage/stderr/code: code=%d err=%v", code, err)
	}
}

func TestEnvironmentWorkerCatFailureRetainsOSDiagnostic(t *testing.T) {
	s, root := environmentTestServer(t)
	path := filepath.Join(root, "missing-declaration.toml")
	_, err := s.readEnvironmentFileAsWorker(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "cat") ||
		!strings.Contains(err.Error(), "missing-declaration.toml") ||
		!strings.Contains(err.Error(), "No such file") {
		t.Fatalf("missing declaration lost native cat stderr: %v", err)
	}
}

func TestEnvironmentOptionalMissingRemainsNonfatalAndUnreadableIsUseful(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, _ := initEnvironmentFixture(t, root, map[string]string{"mise.toml": "[tools]\nnode = \"22\"\n"})
	if err := os.Remove(filepath.Join(repo, "mise.toml")); err != nil {
		t.Fatal(err)
	}
	first := s.repositoryEnvironmentContext(context.Background(), Request{Path: repo})
	if !first.OK {
		t.Fatalf("optional missing declaration should be nonfatal: %+v", first)
	}
	if os.Geteuid() == 0 {
		return // A root-UID fixture can still read a chmod-000 source.
	}
	if err := os.WriteFile(filepath.Join(repo, "mise.toml"), []byte("[tools]\nnode = \"22\"\n"), 0000); err != nil {
		t.Fatal(err)
	}
	second := s.repositoryEnvironmentContext(context.Background(), Request{Path: repo})
	if !second.OK {
		t.Fatalf("unreadable declaration should remain a warning: %+v", second)
	}
	var summary RepositoryEnvironmentSummary
	if err := json.Unmarshal([]byte(second.Output), &summary); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(summary.Warnings, " ")
	if !strings.Contains(joined, "mise.toml could not be read with worker authority") ||
		!strings.Contains(joined, "Permission denied") {
		t.Fatalf("missing worker OS diagnostic in warning: %+v", summary.Warnings)
	}
}

func TestEnvironmentDeclarationDurabilityStatesAndNativeGitFailure(t *testing.T) {
	s, root := environmentTestServer(t)
	repo, head := initEnvironmentFixture(t, root, map[string]string{
		"go.mod": "module example.invalid/diagnostics\n\ngo 1.26.0\n",
	})
	probe := func(path string, ctx context.Context) (*environmentBuilder, EnvironmentDeclaration) {
		t.Helper()
		b := &environmentBuilder{server: s, root: repo}
		if _, ok := b.readDeclaration(ctx, path, "go"); !ok || len(b.declarations) != 1 {
			t.Fatalf("declaration not observed: %+v", b)
		}
		return b, b.declarations[0]
	}
	b, d := probe("go.mod", context.Background())
	if !d.Tracked || !d.MatchesHead || d.DurabilityStatus != "matches_head" || len(b.warnings) != 0 {
		t.Fatalf("unchanged Git declaration status wrong: %+v %+v", d, b.warnings)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.invalid/changed\n\ngo 1.26.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	b, d = probe("go.mod", context.Background())
	if !d.Tracked || d.MatchesHead || d.DurabilityStatus != "differs_from_head" ||
		!strings.Contains(strings.Join(b.warnings, " "), "differs from HEAD") {
		t.Fatalf("known modified declaration status wrong: %+v %+v", d, b.warnings)
	}
	if err := os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte("go 1.26.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	b, d = probe(".tool-versions", context.Background())
	if d.Tracked || d.MatchesHead || d.DurabilityStatus != "untracked" ||
		!strings.Contains(strings.Join(b.warnings, " "), "not Git-tracked") {
		t.Fatalf("untracked declaration misclassified: %+v %+v", d, b.warnings)
	}
	// The index can still enumerate tracked paths, but diff HEAD must fail
	// with a genuine native Git error once the fixture HEAD object is absent.
	// This changes only the disposable fixture repository, never a live worktree.
	object := filepath.Join(repo, ".git", "objects", head[:2], head[2:])
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	b, d = probe("go.mod", context.Background())
	warnings := strings.Join(b.warnings, " ")
	if !d.Tracked || d.MatchesHead || d.DurabilityStatus != "unknown" ||
		!strings.Contains(warnings, "Git HEAD comparison failed") ||
		!strings.Contains(warnings, "git diff exited") ||
		!strings.Contains(warnings, "stderr=") ||
		strings.Contains(warnings, "differs from HEAD") {
		t.Fatalf("failed diff claimed a verified content change: %+v %v", d, b.warnings)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	b = &environmentBuilder{server: s, root: repo}
	tracked, matched, state := b.declarationDurability(canceled, "go.mod")
	if tracked || matched || state != "unknown" ||
		!strings.Contains(strings.Join(b.warnings, " "), "Git tracking probe failed") ||
		strings.Contains(strings.Join(b.warnings, " "), "not Git-tracked") {
		t.Fatalf("unable to start Git was misreported as untracked: tracked=%t matched=%t status=%s warnings=%v", tracked, matched, state, b.warnings)
	}
}
