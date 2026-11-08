package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

func helperFailureServer(t *testing.T) (*Server, string) {
	t.Helper()
	root, repo, _ := workspaceFixture(t)
	return &Server{
		cfg:  config.Config{WorkspaceDir: root},
		runs: newRunLimiterWith(2, 1), workerUID: uint32(os.Geteuid()), workerGID: uint32(os.Getegid()),
		audit: audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
	}, repo
}

func TestWorkspaceHelperPreSpawnFailureIsDefiniteForReadAndWrite(t *testing.T) {
	s, repo := helperFailureServer(t)
	for _, path := range []string{filepath.Join(t.TempDir(), "does-not-exist"), filepath.Join(t.TempDir(), "not-executable")} {
		if strings.HasSuffix(path, "not-executable") {
			if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		s.workspaceHelperBinary = path
		for _, action := range []string{"workspace_stat", "workspace_write"} {
			req := Request{Action: action, Workspace: repo, Path: "src/new.txt"}
			if action == "workspace_write" {
				req.MustNotExist = true
				req.Content = "not-written"
			}
			resp := s.workerWorkspaceFile(context.Background(), req)
			if resp.OK || resp.ErrorCode != "workspace_helper_start_failed" ||
				resp.Status != "helper_not_started" || resp.ErrorClass != "process" ||
				!resp.Retryable || resp.ExitCode != -1 || strings.Contains(resp.Error, "may have taken effect") ||
				!strings.Contains(resp.Error, "did not start") {
				t.Fatalf("%s %s: %+v", path, action, resp)
			}
			if _, err := os.Stat(filepath.Join(repo, "src/new.txt")); !os.IsNotExist(err) {
				t.Fatalf("helper did not start but mutation observed: %v", err)
			}
		}
	}
}

func TestWorkspaceHelperPostSpawnOutcomeDiffersForReadAndWrite(t *testing.T) {
	s, repo := helperFailureServer(t)
	helper := filepath.Join(t.TempDir(), "broken-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho 'helper provider failed token=NEVER_LEAK' >&2\nexit 47\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.workspaceHelperBinary = helper
	for _, action := range []string{"workspace_stat", "workspace_write"} {
		req := Request{Action: action, Workspace: repo, Path: "src/not-created"}
		if action == "workspace_write" {
			req.MustNotExist = true
			req.Content = "private-content"
		}
		got := s.workerWorkspaceFile(context.Background(), req)
		expected := "workspace_helper_failed"
		if action == "workspace_write" {
			expected = "unknown_completion"
		}
		if got.OK || got.ErrorCode != expected || got.ExitCode != 47 ||
			!strings.Contains(got.Output, "helper provider failed") ||
			strings.Contains(got.Output, "NEVER_LEAK") {
			t.Fatalf("%s: %+v", action, got)
		}
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho '{invalid-json'\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if stat := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: repo, Path: "src/file"}); stat.ErrorCode != "workspace_helper_failed" {
		t.Fatalf("invalid JSON read-only classification: %+v", stat)
	}
	if write := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_write", Workspace: repo, Path: "src/file", MustNotExist: true, Content: "x"}); write.ErrorCode != "unknown_completion" {
		t.Fatalf("invalid JSON after spawned writer classification: %+v", write)
	}
}

// Read-only audit remains advisory, but suppressing its error would incorrectly
// imply the diagnostic record was durably captured.
func TestWorkspaceHelperReadOnlyAuditFailureIsVisibleWithoutReclassifyingRead(t *testing.T) {
	s, repo := helperFailureServer(t)
	helper := filepath.Join(t.TempDir(), "successful-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho '{\"ok\":true,\"status\":\"metadata_only\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.workspaceHelperBinary = helper
	blockedParent := filepath.Join(t.TempDir(), "blocked-audit-parent")
	if err := os.WriteFile(blockedParent, []byte("regular file, not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	s.audit = audit.New(filepath.Join(blockedParent, "audit.jsonl"))
	got := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: repo, Path: "src/file"})
	if !got.OK || !got.AuditDegraded || !strings.Contains(got.AuditError, "read succeeded") ||
		!strings.Contains(got.AuditError, "blocked-audit-parent") {
		t.Fatalf("read-only audit failure was suppressed or misclassified: %+v", got)
	}
}
