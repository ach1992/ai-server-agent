package executor

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// Execute only on an isolated, explicitly opted-in CI runner after the Agent
// fixture installer creates aiworker. Never run this as a production health
// check: it tests a fresh temporary workspace and no live Agent sockets.
func TestWorkerWorkspaceRootDelegation(t *testing.T) {
	if os.Getenv("ASA_WORKSPACE_ROOT_ACCEPTANCE") != "1" {
		t.Skip("isolated CI opt-in required")
	}
	if os.Geteuid() != 0 {
		t.Fatal("root-to-worker acceptance must start as root")
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("ASA_WORKSPACE_TEST_BINARY")
	if binary == "" {
		t.Fatal("isolated test must identify its built Agent binary")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	// Go's t.TempDir creates a private outer test directory as well. Both
	// directories are test-owned; permit only traversal for the worker probe.
	for _, path := range []string{filepath.Dir(root), root} {
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(root, "repo")
	src := filepath.Join(repo, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{repo, src} {
		if err := os.Chown(path, int(uid), int(gid)); err != nil {
			t.Fatal(err)
		}
	}
	gitDir := filepath.Join(repo, ".git")
	if err := os.Mkdir(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(gitDir, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("private workspace metadata"), 0644); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg: config.Config{WorkspaceDir: root}, workerUID: uint32(uid), workerGID: uint32(gid),
		runs: newRunLimiterWith(2, 1), audit: audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
		workspaceHelperBinary: binary,
	}
	create := server.workerWorkspaceFile(t.Context(), Request{
		Action: "workspace_write", Workspace: repo, Path: "src/created.txt",
		Content: "proof of worker identity", MustNotExist: true,
		RequestID: "root-worker-test", PrincipalID: "direct-default",
	})
	if !create.OK || create.FileVersion == "" {
		t.Fatalf("root->worker file helper rejected: %+v", create)
	}
	stat, err := os.Stat(filepath.Join(src, "created.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if stat.Sys().(*syscall.Stat_t).Uid != uint32(uid) {
		t.Fatal("worker-created file was not owned by aiworker")
	}
	read := server.workerWorkspaceFile(t.Context(), Request{
		Action: "workspace_read", Workspace: repo, Path: "src/created.txt", FileVersion: create.FileVersion,
	})
	if !read.OK || read.Output != "proof of worker identity" {
		t.Fatalf("worker read failed: %+v", read)
	}
	escaped := server.workerWorkspaceFile(t.Context(), Request{
		Action: "workspace_write", Workspace: repo, Path: "../unauthorized.txt",
		Content: "escaped", MustNotExist: true,
	})
	if escaped.OK {
		t.Fatalf("worker path escape accepted: %+v", escaped)
	}
	if _, err := os.Stat(filepath.Join(root, "unauthorized.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside workspace was modified: %v", err)
	}
	forbidden := server.workerWorkspaceFile(t.Context(), Request{
		Action: "workspace_read", Workspace: filepath.Join(repo, ".git"), Path: "config",
	})
	if forbidden.OK {
		t.Fatalf("Git admin workspace unexpectedly accepted: %+v", forbidden)
	}
}
