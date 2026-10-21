package executor

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// Opt-in, real wall-clock >1h acceptance: no shortened timers, simulated
// clock, installed Agent restart or platform-wide manipulation. A disposable
// tmux scope/timer proves the same pane is accessible past its initial TTL.
// Run explicitly on a designated disposable root test host with
// go test -timeout=80m -run ^TestApprovedPTYRealHourLease$.
func TestApprovedPTYRealHourLease(t *testing.T) {
	host, _ := os.Hostname()
	if !approvedPTYDisposableFixtureHost(host) || os.Getenv("ASA_197_REAL_HOUR_LEASE") != "1" {
		t.Skip("requires separately opted-in 64-minute disposable PID1 test")
	}
	account, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("/tmp", "asa197-hour-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "state")
	if err := os.Mkdir(state, 0711); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(state, "worker-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, uid, gid); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "project")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(workspace, uid, gid); err != nil {
		t.Fatal(err)
	}
	makeServer := func() *Server {
		return &Server{
			cfg:       config.Config{WorkspaceDir: workspace, StateDir: state, WorkerUser: "aiworker"},
			workerUID: uint32(uid), workerGID: uint32(gid),
			sessions: newStdioSessionBroker(), audit: audit.New(filepath.Join(state, "audit.jsonl")),
		}
	}
	s := makeServer()
	req := Request{Action: "terminal_open", Workspace: workspace, Columns: 80, Rows: 24,
		PrincipalID: "hour-fixture-owner", PrincipalClass: "direct", RequestID: "real-hour-lease"}
	opened := s.terminalAction(req)
	if !opened.OK {
		t.Fatalf("open long fixture: %+v", opened)
	}
	req.SessionID, req.SessionEpoch = opened.SessionID, opened.SessionEpoch
	e, err := s.sessions.get(req, req.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	generation, pane := e.terminal.name, e.terminal.pane
	t.Cleanup(func() {
		if err := stopScopedTerminalBackend(generation); err != nil {
			t.Errorf("exact fixture cleanup unverified: %v", err)
		}
	})
	started := time.Now()
	first, err := s.readTerminalRecord(req)
	if err != nil || first.Version != 2 {
		t.Fatalf("missing durable v2 lease: %+v %v", first, err)
	}
	verify := func(phase string, broker *Server) terminalRecord {
		t.Helper()
		if err := verifyScopedTerminalBackendForVersion(generation, 2); err != nil {
			t.Fatalf("%s scope: %v", phase, err)
		}
		if err := verifyTerminalLeaseTimer(generation); err != nil {
			t.Fatalf("%s timer: %v", phase, err)
		}
		rec, err := broker.readTerminalRecord(req)
		if err != nil || rec.CreatedAt != first.CreatedAt || rec.Name != generation || rec.Pane != pane || !rec.ExpiresAt.After(time.Now()) {
			t.Fatalf("%s lease identity/expiry: %+v %v", phase, rec, err)
		}
		return rec
	}
	// At minute 31, an authenticated read must extend the real 1h timer.
	time.Sleep(time.Until(started.Add(31 * time.Minute)))
	req.Action = "terminal_read"
	response := s.terminalAction(req)
	if !response.OK {
		t.Fatalf("31m authenticated activity: %+v", response)
	}
	middle := verify("31m", s)
	if !middle.ExpiresAt.After(first.ExpiresAt) {
		t.Fatal("idle lease failed to extend beyond original one-hour boundary")
	}
	// Prove that the same PID1 scope/pane really survives after elapsed >1h.
	time.Sleep(time.Until(started.Add(64 * time.Minute)))
	if r := s.terminalAction(req); !r.OK {
		t.Fatalf("64m activity: %+v", r)
	}
	_ = verify("64m", s)
	if err := s.sessions.removeAndStop(e); err != nil {
		t.Fatalf("detach Control Mode: %v", err)
	}
	if err := verifyScopedTerminalBackendForVersion(generation, 2); err != nil {
		t.Fatalf("original pane lost on broker detach: %v", err)
	}
	previousEpoch := req.SessionEpoch
	recovered := makeServer()
	req.Action = "terminal_reconnect"
	latest := recovered.terminalAction(req)
	if !latest.OK || latest.SessionID != req.SessionID || latest.SessionEpoch == previousEpoch || !latest.RetentionTruncated {
		t.Fatalf("auth recovery after >1h did not preserve same pane and rotate epoch: %+v", latest)
	}
	req.SessionEpoch = latest.SessionEpoch
	_ = verify("64m post-restart", recovered)
	req.Action = "terminal_close"
	if closed := recovered.terminalAction(req); !closed.OK {
		t.Fatalf("real 64m scope close: %+v", closed)
	}
	if err := verifyScopedTerminalBackendForVersion(generation, 2); err == nil {
		t.Fatal("scope survived verified close")
	}
	if err := verifyTerminalLeaseTimer(generation); err == nil {
		t.Fatal("independent timer survived verified close")
	}
	t.Logf("REAL_64_MINUTE_LEASE_PASS elapsed=%s generation=%s", time.Since(started), generation)
}
