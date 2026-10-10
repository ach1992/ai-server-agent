package executor

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// Actual bounded systemd backends, not merely synthetic recovery records.
// Opt-in root/hostname gate: NEVER restarts an installed Agent service.
func TestApprovedPersistentPTYCapacityAfterExecutorRestart(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host != "CLY535343" || os.Geteuid() != 0 ||
		os.Getenv("ASA_PTY_SCOPE_APPROVED_HOST") != host {
		t.Skip("requires approved disposable root test host")
	}
	u, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "asa-pty-capacity-proof-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	state := filepath.Join(root, "state")
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
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(project, uid, gid); err != nil {
		t.Fatal(err)
	}
	srv := func() *Server {
		return &Server{
			cfg:       config.Config{WorkspaceDir: project, StateDir: state, WorkerUser: "aiworker"},
			workerUID: uint32(uid), workerGID: uint32(gid),
			sessions: newStdioSessionBroker(),
			audit:    audit.New(filepath.Join(state, "audit.jsonl")),
		}
	}
	s := srv()
	owner := Request{Action: "terminal_open", PrincipalID: "capacity-proof-principal",
		PrincipalClass: "direct", Workspace: project, Columns: 80, Rows: 24, RequestID: "capacity-proof"}
	type active struct {
		id, epoch, name string
	}
	saved := make([]active, 0, maxStdioSessions+2)
	t.Cleanup(func() {
		// An assertion failure must not leave any test-owned root scopes.
		for _, v := range saved {
			if validTmuxSessionName(v.name) {
				_ = stopScopedTerminalBackend(v.name)
			}
		}
	})
	for i := 0; i < maxStdioSessions; i++ {
		req := owner
		req.RequestID = "capacity-open-" + strconv.Itoa(i)
		o := s.terminalAction(req)
		if !o.OK {
			t.Fatalf("could not establish %d real scoped terminals: %+v", i, o)
		}
		req.SessionID, req.SessionEpoch = o.SessionID, o.SessionEpoch
		e, err := s.sessions.get(req, o.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, active{id: o.SessionID, epoch: o.SessionEpoch, name: e.terminal.name})
		if err := verifyScopedTerminalBackend(e.terminal.name); err != nil {
			t.Fatalf("scope %d not verified: %v", i, err)
		}
	}
	// Simulated Executor restart: all eight Control Mode clients die but
	// eight real PID1-managed scopes and their durable records survive.
	for _, v := range saved {
		req := owner
		req.SessionID, req.SessionEpoch = v.id, v.epoch
		e, err := s.sessions.get(req, v.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.sessions.removeAndStop(e); err != nil {
			t.Fatalf("Control Mode detach before simulated restart: %v", err)
		}
	}
	restarted := srv() // new broker has NO in-memory knowledge of old sessions
	reject := restarted.terminalAction(owner)
	if reject.OK || reject.ErrorCode != "terminal_resource_limit" || reject.SessionID != "" {
		t.Fatalf("post-restart ninth scope was created before admission: %+v", reject)
	}
	for _, v := range saved {
		if err := verifyScopedTerminalBackend(v.name); err != nil {
			t.Fatalf("capacity rejection touched a surviving backend: %v", err)
		}
	}
	if count, err := restarted.terminalRecordCount(); err != nil || count != maxStdioSessions {
		t.Fatalf("recovered eight scopes not charged against capacity: count=%d err=%v", count, err)
	}
	// Release exactly one old session via authenticated reconnect+close.
	previous := saved[0]
	rec := owner
	rec.Action = "terminal_reconnect"
	rec.SessionID, rec.SessionEpoch = previous.id, previous.epoch
	reconnected := restarted.terminalAction(rec)
	if !reconnected.OK {
		t.Fatalf("old terminal could not be reclaimed: %+v", reconnected)
	}
	rec.Action, rec.SessionEpoch = "terminal_close", reconnected.SessionEpoch
	if closed := restarted.terminalAction(rec); !closed.OK {
		t.Fatalf("verified old backend close failed: %+v", closed)
	}
	var wg sync.WaitGroup
	results := make(chan Response, 2)
	for j := 0; j < 2; j++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := owner
			req.RequestID = "concurrent-capacity-" + strconv.Itoa(i)
			results <- restarted.terminalAction(req)
		}(j)
	}
	wg.Wait()
	close(results)
	pass, denied := 0, 0
	for result := range results {
		if result.OK {
			pass++
			owner.SessionID, owner.SessionEpoch = result.SessionID, result.SessionEpoch
			e, err := restarted.sessions.get(owner, result.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			saved = append(saved, active{id: result.SessionID, epoch: result.SessionEpoch, name: e.terminal.name})
		} else if result.ErrorCode == "terminal_resource_limit" && result.SessionID == "" {
			denied++
		} else {
			t.Fatalf("concurrent terminal admission not bounded: %+v", result)
		}
	}
	if pass != 1 || denied != 1 {
		t.Fatalf("real concurrent terminal scope over-admission: passed=%d rejected=%d", pass, denied)
	}
	if count, err := restarted.terminalRecordCount(); err != nil || count != maxStdioSessions {
		t.Fatalf("expected exactly eight durable scopes after concurrent opens: count=%d err=%v", count, err)
	}
	for _, v := range saved[1:] {
		if err := verifyScopedTerminalBackend(v.name); err != nil {
			t.Fatalf("unrelated original real scope was stopped: %v", err)
		}
	}
	// Test cleanup is by exact original generation, not by global
	// KillMode changes, broad process matching, or installed service restart.
	t.Log("real eight-scope post-restart capacity and concurrent 7-to-8 admission PASS")
}
