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
