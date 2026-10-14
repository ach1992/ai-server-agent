package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func isolatedStdioDAPTest(t *testing.T, binary string, args ...string) (*Server, Request, *stdioSession, *stdioDAPConn) {
	t.Helper()
	s, owner, workspace := testStdioBroker(t)
	id, err := s.startProcessSessionWithOptions(owner, "dap", workspace, binary, nil, "", nil, true, args...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.workerStdioClose(owner, id) })
	entry, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if entry.dapStdout == nil {
		t.Fatal("requested bounded direct DAP transport unavailable")
	}
	owner.SessionID = id
	return s, owner, entry, newStdioDAPConn(entry.dapStdout, entry.stdin)
}

func TestStdioDAPConnUsesAuthenticatedOriginalBroker(t *testing.T) {
	s, owner, _, c := isolatedStdioDAPTest(t, "/bin/cat")
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("p", maxStdioInputBytes+20) + "\n")
	n, err := c.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("split worker write: bytes=%d err=%v", n, err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("bounded direct stdout: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatal("broker DAP transport corrupted bytes")
	}
	other := owner
	other.PrincipalID = "unauthorized"
	if _, err := s.sessions.get(other, owner.SessionID); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("cross-principal DAP pipe identity disclosed: %v", err)
	}
	other = owner
	other.Workspace = s.cfg.WorkspaceDir
	if _, err := s.sessions.get(other, owner.SessionID); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("cross-workspace DAP pipe identity disclosed: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("stale")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed DAP transport accepted write: %v", err)
	}
}

func TestStdioDAPConnRejectsRetainedFrameLoss(t *testing.T) {
	_, _, e, c := isolatedStdioDAPTest(t, "/bin/cat")
	if _, err := e.dapStdout.Write([]byte(strings.Repeat("x", dapStdioQueueLimit+1))); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "output_lost") {
		t.Fatalf("DAP stdout overflow was silently truncated: %v", err)
	}
}

func TestStdioDAPConnDeadlineUpdateAndAbruptExit(t *testing.T) {
	_, _, e, c := isolatedStdioDAPTest(t, "/bin/sh", "-c", "exit 7")
	done := make(chan error, 1)
	go func() {
		if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			done <- err
			return
		}
		_, err := c.Read(make([]byte, 32))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("exited adapter caused deadlock or misleading success: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DAP stdout reader remained blocked after adapter process exit")
	}
	select {
	case <-e.done:
	case <-time.After(3 * time.Second):
		t.Fatal("broker could not complete wait/cleanup after stdio DAP exit")
	}
}

func TestStdioDAPConnNoBrokerExitDeadlockWhileDAPCallHoldsLock(t *testing.T) {
	_, owner, e, c := isolatedStdioDAPTest(t, "/bin/sh", "-c", "sleep 0.15; exit 17")
	d := &dapState{conn: c, workspace: owner.Workspace, adapter: "python/debugpy", stage: "starting"}
	e.mu.Lock()
	e.dap = d
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	complete := make(chan error, 1)
	go func() { _, err := d.call(ctx, "initialize", map[string]any{"adapterID": "debugpy"}); complete <- err }()
	select {
	case err := <-complete:
		if err == nil {
			t.Fatal("non-DAP adapter unexpectedly returned success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DAP call blocked waiting for broker's cleanup lock")
	}
	select {
	case <-e.done:
	case <-time.After(3 * time.Second):
		t.Fatal("broker did not finish cleanup while DAP held protocol mutex")
	}
}

func TestRealDebugpyStdioOverExistingBroker(t *testing.T) {
	pkg := os.Getenv("AGENT_DEBUGPY_PROOF_PACKAGE")
	if pkg == "" {
		t.Skip("isolated and pinned real debugpy package fixture not provided")
	}
	if _, err := os.Stat(filepath.Join(pkg, "debugpy", "adapter", "__main__.py")); err != nil {
		t.Fatalf("missing real optional debugpy adapter: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal(err)
	}
	code := "import runpy,sys;sys.path.insert(0," + strconv.Quote(pkg) + ");runpy.run_module('debugpy.adapter',run_name='__main__')"
	_, owner, _, c := isolatedStdioDAPTest(t, "/usr/bin/python3", "-I", "-Xfrozen_modules=off", "-c", code)
	d := &dapState{conn: c, workspace: owner.Workspace, adapter: "python/debugpy", stage: "starting"}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	out, err := d.call(ctx, "initialize", map[string]any{
		"adapterID": "debugpy", "clientID": "ai-server-agent", "pathFormat": "path",
		"linesStartAt1": true, "columnsStartAt1": true, "supportsRunInTerminalRequest": false,
	})
	if err != nil || !strings.Contains(string(out), "supportsConfigurationDoneRequest") {
		t.Fatalf("real debugpy initialized via bounded worker DAP stream: %v %s", err, out)
	}
	t.Log("verified Python debugpy 1.8.22 initialized through original pidfd-owned broker without cross-lock dependency")
}
