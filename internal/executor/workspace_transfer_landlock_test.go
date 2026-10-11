package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

func TestWorkspaceTransferActualWorkerHelperLandlock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run worker helper acceptance unprivileged; root delegation is isolated CI")
	}
	requireWorkerLandlockV2(t)
	root, repo, _ := workspaceFixture(t)
	state := filepath.Join(t.TempDir(), "state")
	home := filepath.Join(state, "worker-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "ai-server-agent")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/ai-server-agent")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v: %s", err, out)
	}
	server := &Server{cfg: config.Config{WorkspaceDir: root, StateDir: state},
		runs: newRunLimiterWith(2, 1), workerUID: uint32(os.Geteuid()), workerGID: uint32(os.Getegid()),
		workspaceHelperBinary: binary, audit: audit.New(filepath.Join(t.TempDir(), "audit.jsonl"))}
	full := make([]byte, (1<<20)+87)
	for i := range full {
		full[i] = byte(37*i + 13)
	}
	sum := sha256.Sum256(full)
	base := Request{Action: "workspace_transfer", Workspace: repo, Path: "src/landlock.bin",
		PrincipalID: "direct-default", PrincipalClass: "direct",
		TransferOp: "begin", TransferKey: "worker-acceptance-01", ExpectedSize: int64(len(full)),
		SHA256: hex.EncodeToString(sum[:]), MustNotExist: true}
	begin := server.workerWorkspaceFile(t.Context(), base)
	if !begin.OK {
		t.Fatalf("worker sandbox begin failed: %+v", begin)
	}
	if _, err := os.Stat(filepath.Join(repo, "src/landlock.bin")); !os.IsNotExist(err) {
		t.Fatal("partial target visible")
	}
	next := int64(0)
	for next < int64(len(full)) {
		n := 128 << 10
		if int64(n) > int64(len(full))-next {
			n = int(int64(len(full)) - next)
		}
		data := full[next : next+int64(n)]
		digest := sha256.Sum256(data)
		req := base
		req.TransferOp = "chunk"
		req.TransferKey = ""
		req.TransferID = begin.TransferID
		req.ExpectedSize = 0
		req.SHA256 = ""
		req.MustNotExist = false
		req.Offset = next
		req.ChunkBase64 = base64.StdEncoding.EncodeToString(data)
		req.ChunkSHA256 = hex.EncodeToString(digest[:])
		got := server.workerWorkspaceFile(t.Context(), req)
		if !got.OK || got.TransferredBytes != next+int64(n) {
			t.Fatalf("worker real chunk at %d: %+v", next, got)
		}
		next += int64(n)
	}
	commit := base
	commit.TransferOp = "commit"
	commit.TransferKey = ""
	commit.TransferID = begin.TransferID
	commit.ExpectedSize = 0
	commit.SHA256 = ""
	commit.MustNotExist = false
	res := server.workerWorkspaceFile(t.Context(), commit)
	if !res.OK || res.Status != "committed" {
		t.Fatalf("worker real atomic commit: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(repo, "src/landlock.bin"))
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("worker content mismatch: %v", err)
	}
}
