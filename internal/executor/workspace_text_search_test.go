package executor

import (
	"encoding/json"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

func TestWorkerLandlockedTextSearchResultsAndBounds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("worker helper black-box test runs with ordinary worker authority")
	}
	if _, err := os.Stat("/usr/bin/rg"); err != nil {
		t.Skip("ripgrep not installed in optional development host")
	}
	root, repo, _ := workspaceFixture(t)
	if err := os.WriteFile(filepath.Join(repo, "src", "one.go"), []byte("package main\nhello_alpha(1)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "two.go"), []byte("package main\nhello_beta(2)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secrets.go")
	if err := os.WriteFile(outside, []byte("hello_OUTSIDE"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "src", "link.go")); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "ai-server-agent")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v %s", err, output)
	}
	server := &Server{cfg: config.Config{WorkspaceDir: root}, runs: newRunLimiterWith(2, 1), workerUID: uint32(os.Geteuid()), workerGID: uint32(os.Getegid()), audit: audit.New(filepath.Join(t.TempDir(), "audit.jsonl")), workspaceHelperBinary: binary}
	search := func(req Request) WorkspaceSearchResult {
		t.Helper()
		req.Action = "workspace_text_search"
		req.Workspace = repo
		resp := server.workerWorkspaceFile(t.Context(), req)
		if !resp.OK {
			t.Fatalf("worker ripgrep error: %+v", resp)
		}
		var out WorkspaceSearchResult
		if err := json.Unmarshal([]byte(resp.Output), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// The client may use a root-relative workspace selector; it must resolve
	// to the same authorized checkout irrespective of the helper's cwd.
	relative := server.workerWorkspaceFile(t.Context(), Request{Action: "workspace_text_search", Workspace: "repo", Pattern: "hello_"})
	if !relative.OK {
		t.Fatalf("relative workspace failed: %+v", relative)
	}
	found := search(Request{Pattern: "hello_"})
	if !found.Complete || found.Truncated || len(found.Matches) != 2 || found.Engine != "ripgrep" {
		t.Fatalf("unexpected search: %+v", found)
	}
	for _, m := range found.Matches {
		if strings.Contains(m.File, "link") || strings.Contains(m.Text, "OUTSIDE") || !strings.HasPrefix(m.File, "src/") {
			t.Fatalf("escaped or malformed result: %+v", m)
		}
	}
	one := search(Request{Pattern: "hello_", SearchLimit: 1})
	if one.Complete || !one.Truncated || len(one.Matches) != 1 || one.TruncationReason != "result_limit" {
		t.Fatalf("limit not enforced: %+v", one)
	}
	filtered := search(Request{Pattern: "hello_", Globs: []string{"!two.go"}})
	if len(filtered.Matches) != 1 || !strings.Contains(filtered.Matches[0].File, "one.go") {
		t.Fatalf("unexpected globs: %+v", filtered)
	}
	literal := search(Request{Pattern: "hello_.+", Literal: true})
	if !literal.Complete || len(literal.Matches) != 0 {
		t.Fatalf("fixed-string mode interpreted regex metacharacters: %+v", literal)
	}
	regex := search(Request{Pattern: "hello_.+"})
	if !regex.Complete || len(regex.Matches) != 2 {
		t.Fatalf("regex mode did not match expected content: %+v", regex)
	}
	invalidTimeout := server.workerWorkspaceFile(t.Context(), Request{Action: "workspace_text_search", Workspace: repo, Pattern: "hello_", TimeoutMS: int64(maxWorkspaceSearchTimeout/time.Millisecond) + 1})
	if invalidTimeout.OK || invalidTimeout.ErrorCode != "invalid_text_search" {
		t.Fatalf("excessive timeout accepted: %+v", invalidTimeout)
	}
	empty := search(Request{Pattern: "never_present_XXXXXXXXX"})
	if !empty.Complete || len(empty.Matches) != 0 {
		t.Fatalf("no-match should be complete: %+v", empty)
	}
	// JSON events for a giant matched source line are bounded while streaming;
	// the scanner must report a partial result, not buffer the full line.
	huge := filepath.Join(repo, "src", "huge.txt")
	if err := os.WriteFile(huge, []byte("giant_marker_"+strings.Repeat("a", maxWorkspaceSearchLineBytes)), 0644); err != nil {
		t.Fatal(err)
	}
	oversized := search(Request{Pattern: "giant_marker_"})
	if oversized.Complete || !oversized.Truncated || oversized.TruncationReason != "engine_event_too_large" {
		t.Fatalf("giant event unbounded or mislabeled: %+v", oversized)
	}
	// A malicious FIFO in the tree must not block bounded text search.
	if err := os.Remove(huge); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(repo, "src", "blocked.fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	noPipe := search(Request{Pattern: "nonexistent_pipe_marker"})
	if !noPipe.Complete || noPipe.TimedOut {
		t.Fatalf("search blocked by nonregular file: %+v", noPipe)
	}
	invalid := server.workerWorkspaceFile(t.Context(), Request{Action: "workspace_text_search", Workspace: repo, Pattern: "["})
	if invalid.OK {
		t.Fatalf("invalid regex was accepted: %+v", invalid)
	}
}
