package executor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"golang.org/x/sys/unix"
)

func workspaceFixture(t *testing.T) (string, string, workspaceFileOperation) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	return root, repo, workspaceFileOperation{WorkspaceRoot: root, Workspace: repo, Path: "src/file.txt"}
}

func TestWorkerFileReadWritePreconditionsAndNonText(t *testing.T) {
	_, repo, op := workspaceFixture(t)
	op.Action = "workspace_write"
	op.MustNotExist = true
	op.Content = string([]byte{0xff})
	if invalid := writeWorkerWorkspaceFile(op); invalid.OK || invalid.ErrorCode != "invalid_content" {
		t.Fatalf("invalid UTF-8 write silently accepted: %+v", invalid)
	}
	op.Content = "hello"
	first := writeWorkerWorkspaceFile(op)
	if !first.OK || first.FileVersion == "" {
		t.Fatalf("create: %+v", first)
	}
	if again := writeWorkerWorkspaceFile(op); again.OK || again.ErrorCode != "file_exists" {
		t.Fatalf("duplicate create: %+v", again)
	}
	op.MustNotExist = false
	if noVersion := writeWorkerWorkspaceFile(op); noVersion.OK || noVersion.ErrorCode != "invalid_precondition" {
		t.Fatalf("missing precondition: %+v", noVersion)
	}
	op.FileVersion = first.FileVersion
	op.Content = "second"
	second := writeWorkerWorkspaceFile(op)
	if !second.OK || second.FileVersion == first.FileVersion {
		t.Fatalf("replace: %+v", second)
	}
	if stale := writeWorkerWorkspaceFile(op); stale.OK || stale.ErrorCode != "file_changed" {
		t.Fatalf("stale overwrite: %+v", stale)
	}
	op.Action = "workspace_read"
	op.Content = ""
	op.FileVersion = second.FileVersion
	read := readWorkerWorkspaceFile(op)
	if !read.OK || read.Output != "second" || read.OutputEncoding != "utf-8" {
		t.Fatalf("read: %+v", read)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "binary.dat"), []byte{0xff, 0, 0xfe}, 0644); err != nil {
		t.Fatal(err)
	}
	op.Path = "src/binary.dat"
	op.FileVersion = ""
	binary := readWorkerWorkspaceFile(op)
	if !binary.OK || binary.OutputEncoding != "base64" || binary.Output == "" {
		t.Fatalf("binary: %+v", binary)
	}
	op.Path = "src/file.txt"
	op.Offset = 2
	op.Limit = 2
	partial := readWorkerWorkspaceFile(op)
	if !partial.OK || partial.Output != "co" || !partial.Truncated || *partial.EOF {
		t.Fatalf("partial: %+v", partial)
	}
}

func TestWorkerFileContainmentAndSpecialFiles(t *testing.T) {
	root, repo, op := workspaceFixture(t)
	op.Action = "workspace_read"
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(repo, "outside-parent")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside.txt", "src/../../outside.txt", ".git/config", "sub/.git/config", "link.txt", "outside-parent/outside.txt", "/etc/passwd"} {
		op.Path = path
		if result := readWorkerWorkspaceFile(op); result.OK {
			t.Errorf("unsafe path %q returned success", path)
		}
	}
	op.Workspace = filepath.Dir(root)
	op.Path = "etc/passwd"
	if result := readWorkerWorkspaceFile(op); result.OK {
		t.Fatalf("outside workspace succeeded: %+v", result)
	}
	op.Workspace = repo
	op.Path = "src"
	if result := readWorkerWorkspaceFile(op); result.OK || result.ErrorCode != "unsupported_file_type" {
		t.Fatalf("directory read: %+v", result)
	}
	fifo := filepath.Join(repo, "named-pipe")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	op.Path = "named-pipe"
	done := make(chan Response, 1)
	go func() { done <- readWorkerWorkspaceFile(op) }()
	select {
	case result := <-done:
		if result.OK || result.ErrorCode != "unsupported_file_type" {
			t.Fatalf("FIFO: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("special FIFO blocked worker read")
	}
	// A symlink parent must be rejected for mutations as well.
	op.Action = "workspace_write"
	op.Path = "outside-parent/created.txt"
	op.MustNotExist = true
	op.Content = "evil"
	if result := writeWorkerWorkspaceFile(op); result.OK {
		t.Fatalf("symlink-parent write escaped: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside mutation exists: %v", err)
	}
}

func TestWorkspaceFileHelperRunsWithWorkerIdentityAndAudit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("ordinary worker bridge fixture runs without root; privileged service coverage belongs to high assurance CI")
	}
	requireWorkerLandlockV2(t)
	root, repo, op := workspaceFixture(t)
	binary := filepath.Join(t.TempDir(), "ai-server-agent")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v: %s", err, output)
	}
	loggerPath := filepath.Join(t.TempDir(), "audit.jsonl")
	logger := audit.New(loggerPath)
	server := &Server{cfg: config.Config{WorkspaceDir: root}, runs: newRunLimiterWith(1, 1), workerUID: uint32(os.Geteuid()), workerGID: uint32(os.Getegid()), audit: logger, workspaceHelperBinary: binary}
	req := Request{Action: "workspace_write", Workspace: repo, Path: op.Path, Content: "worker-created", MustNotExist: true, RequestID: "req-worker", PrincipalID: "direct-default"}
	write := server.workerWorkspaceFile(t.Context(), req)
	if !write.OK || write.FileVersion == "" {
		t.Fatalf("worker helper write: %+v", write)
	}
	stat, err := os.Stat(filepath.Join(repo, op.Path))
	if err != nil {
		t.Fatal(err)
	}
	sys := stat.Sys().(*syscall.Stat_t)
	if sys.Uid != uint32(os.Geteuid()) {
		t.Fatalf("helper did not create under worker identity: uid=%d", sys.Uid)
	}
	req.Action = "workspace_read"
	req.FileVersion = write.FileVersion
	req.Content = ""
	req.MustNotExist = false
	read := server.workerWorkspaceFile(t.Context(), req)
	if !read.OK || read.Output != "worker-created" {
		t.Fatalf("worker helper read: %+v", read)
	}
	auditData, err := os.ReadFile(loggerPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(auditData), "worker-created") {
		t.Fatal("raw source contents leaked to audit")
	}
	// Required intent audit failure must prevent the worker process from
	// creating a file, even though the worker identity could write it.
	server.auditWriteHook = func(phase string) error {
		if phase == "start" {
			return errors.New("forced intent failure")
		}
		return nil
	}
	blockedPath := "src/blocked.txt"
	blocked := server.workerWorkspaceFile(t.Context(), Request{Action: "workspace_write", Workspace: repo, Path: blockedPath, MustNotExist: true, Content: "must not exist"})
	if blocked.OK || blocked.ErrorCode != "audit_unavailable" {
		t.Fatalf("required intent failure did not block: %+v", blocked)
	}
	if _, err := os.Stat(filepath.Join(repo, blockedPath)); !os.IsNotExist(err) {
		t.Fatalf("worker mutation started without audit: %v", err)
	}
	server.auditWriteHook = nil

	// After an already-completed worker mutation, completion persistence
	// failure must retain the truthful result but close later admissions.
	server.auditBeforeCompletionHook = func() {
		if err := os.Remove(loggerPath); err != nil {
			t.Errorf("remove audit: %v", err)
			return
		}
		if err := os.Symlink(loggerPath+".invalid", loggerPath); err != nil {
			t.Errorf("replace audit: %v", err)
		}
	}
	degradedPath := "src/degraded.txt"
	degraded := server.workerWorkspaceFile(t.Context(), Request{Action: "workspace_write", Workspace: repo, Path: degradedPath, MustNotExist: true, Content: "committed-before-audit-failure"})
	if !degraded.OK || !degraded.AuditDegraded {
		t.Fatalf("worker completion failure hidden: %+v", degraded)
	}
	if got, err := os.ReadFile(filepath.Join(repo, degradedPath)); err != nil || string(got) != "committed-before-audit-failure" {
		t.Fatalf("host effect missing: %v %q", err, got)
	}
	if err := os.Remove(loggerPath); err != nil {
		t.Fatal(err)
	}
	server.auditBeforeCompletionHook = nil
	laterPath := "src/after-degradation.txt"
	later := server.workerWorkspaceFile(t.Context(), Request{Action: "workspace_write", Workspace: repo, Path: laterPath, MustNotExist: true, Content: "blocked"})
	if later.OK || later.ErrorCode != "audit_unavailable" {
		t.Fatalf("degraded worker admission passed: %+v", later)
	}
	if _, err := os.Stat(filepath.Join(repo, laterPath)); !os.IsNotExist(err) {
		t.Fatalf("worker mutation after degraded latch: %v", err)
	}
}
