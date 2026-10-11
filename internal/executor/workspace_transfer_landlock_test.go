package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

func TestWorkspaceTransferActualWorkerHelperLandlock(t *testing.T) {
	required := os.Getenv("ASA_REQUIRE_TRANSFER_LANDLOCK") == "1"
	if os.Geteuid() == 0 {
		if required {
			t.Fatal("real worker Landlock proof must run unprivileged")
		}
		t.Skip("run worker helper acceptance unprivileged; root delegation is isolated CI")
	}
	if required {
		abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
		if errno != 0 || abi < 2 {
			t.Fatalf("required exact-candidate transfer proof lacks Landlock ABI v2: abi=%d errno=%v", abi, errno)
		}
	} else {
		requireWorkerLandlockV2(t)
	}
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
	full := make([]byte, (4<<20)+87)
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
	for i := 0; res.OK && res.Status == "preparing" && i < 128; i++ {
		res = server.workerWorkspaceFile(t.Context(), commit)
	}
	if !res.OK || res.Status != "committed" {
		t.Fatalf("worker real atomic commit: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(repo, "src/landlock.bin"))
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("worker content mismatch: %v", err)
	}
	// Verify actual periodic cleanup traverses the worker helper + Landlock
	// without relying on any subsequent client begin/commit call.
	waiting := base
	waiting.TransferKey = "landlock-expire-02"
	waiting.Path = "src/pending-expired.dat"
	waiting.ExpectedSize = 17
	waiting.SHA256 = hex.EncodeToString(sha256.New().Sum(nil))
	created := server.workerWorkspaceFile(t.Context(), waiting)
	if !created.OK {
		t.Fatalf("create expiry fixture: %+v", created)
	}
	waitingSession := filepath.Join(state, "worker-home", "transfers", created.TransferID)
	journal, err := transferReadRecord(waitingSession)
	if err != nil {
		t.Fatal(err)
	}
	journal.ExpiresUnix = time.Now().Add(-time.Second).Unix()
	if err = transferSaveRecord(waitingSession, journal); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { server.runTransferSweepLoop(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, e := os.Stat(waitingSession); os.IsNotExist(e) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not stop after cancellation")
	}
	if _, e := os.Stat(waitingSession); !os.IsNotExist(e) {
		t.Fatalf("real worker periodic sweep left expired staging: %v", e)
	}
}
