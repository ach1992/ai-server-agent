package executor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// This test must never execute as part of ordinary CI. It launches a genuine
// root-owned transient systemd scope and drops the pane to aiworker. Only a
// separately authorized disposable host may opt in explicitly.
func TestApprovedWorkerTmuxScopeSurvivesBrokerStop(t *testing.T) {
	approved := os.Getenv("ASA_PTY_SCOPE_APPROVED_HOST")
	hostname, _ := os.Hostname()
	if approved == "" || approved != hostname || hostname != "CLY535343" || os.Geteuid() != 0 {
		t.Skip("real systemd scope test requires explicitly authorized disposable host and root")
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
	dir, err := os.MkdirTemp("/tmp", "asa-pty-scope-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
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
	project := filepath.Join(dir, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(project, uid, gid); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:       config.Config{WorkspaceDir: project, StateDir: state, WorkerUser: "aiworker"},
		workerUID: uint32(uid), workerGID: uint32(gid),
		sessions: newStdioSessionBroker(),
		audit:    audit.New(filepath.Join(state, "audit.jsonl")),
	}
	owner := Request{
		Action: "terminal_open", PrincipalID: "isolated-scope-test-owner", PrincipalClass: "direct",
		Workspace: project, Columns: 80, Rows: 24, RequestID: "isolated-systemd-scope",
	}
	var ownedGeneration string
	t.Cleanup(func() {
		if ownedGeneration != "" {
			_ = stopScopedTerminalBackend(ownedGeneration)
		}
	})
	opened := s.terminalAction(owner)
	if !opened.OK {
		// An uncertain startup may already have an isolated backend. The
		// returned session ID is a reconciliation locator, not success.
		if opened.SessionID != "" {
			owner.SessionID = opened.SessionID
			if pending, err := s.sessions.get(owner, opened.SessionID); err == nil {
				ownedGeneration = pending.terminal.name
				t.Logf("pending ack=%d pane=%q protocol=%q exited=%v", pending.terminal.ack, pending.terminal.pane, pending.terminal.protocolError, pending.exited)
			}
		}
		t.Fatalf("production worker scope launch failed: %+v", opened)
	}
	owner.SessionID, owner.SessionEpoch = opened.SessionID, opened.SessionEpoch
	e, err := s.sessions.get(owner, opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sock, generation := e.terminal.socket, e.terminal.name
	ownedGeneration = generation
	t.Cleanup(func() {
		_ = exec.Command("/usr/bin/tmux", "-S", sock, "kill-server").Run()
		_ = stopScopedTerminalBackend(generation)
	})
	if !e.terminal.scoped {
		t.Fatal("production tmux did not use the isolated systemd backend")
	}
	if err := verifyScopedTerminalBackend(generation); err != nil {
		t.Fatalf("scope lost cgroup or lifetime boundary: %v", err)
	}
	// Worker UID must NOT attach directly to another principal's root-owned
	// socket, even when it knows the opaque socket path.
	if err := exec.Command("/usr/bin/setpriv", "--reuid="+account.Uid, "--regid="+account.Gid,
		"--clear-groups", "/usr/bin/tmux", "-S", sock, "list-sessions").Run(); err == nil {
		t.Fatal("aiworker acquired root-owned tmux socket authority")
	}
	// Simulate loss of the Executor-owned Control Mode broker process.
	// Unlike the former single-cgroup implementation, the server remains
	// under PID1 ownership in a separate 1h-bounded scope.
	if err := s.sessions.removeAndStop(e); err != nil {
		t.Fatal(err)
	}
	if err := verifyScopedTerminalBackend(generation); err != nil {
		t.Fatalf("backend did not outlive Control Mode: %v", err)
	}
	s2 := &Server{
		cfg: s.cfg, workerUID: s.workerUID, workerGID: s.workerGID,
		sessions: newStdioSessionBroker(), audit: s.audit,
	}
	reconnect := owner
	reconnect.Action = "terminal_reconnect"
	foreign := reconnect
	foreign.PrincipalID = "another-authenticated-principal"
	if got := s2.terminalAction(foreign); got.OK || got.ErrorCode != "terminal_reconnect_unavailable" {
		t.Fatalf("reconnect crossed authenticated principal boundary: %+v", got)
	}
	wrongWorkspace := reconnect
	wrongWorkspace.Workspace = filepath.Join(dir, "project-other")
	if got := s2.terminalAction(wrongWorkspace); got.OK || got.ErrorCode != "terminal_reconnect_unavailable" {
		t.Fatalf("reconnect crossed workspace boundary: %+v", got)
	}
	recovered := s2.terminalAction(reconnect)
	if !recovered.OK || recovered.SessionID != owner.SessionID ||
		recovered.SessionEpoch == owner.SessionEpoch || !recovered.RetentionTruncated {
		t.Fatalf("recovery did not preserve trusted original backend: %+v", recovered)
	}
	stale := owner
	stale.Action = "terminal_read"
	if got := s2.terminalAction(stale); got.OK || got.ErrorCode != "terminal_epoch_changed" {
		t.Fatalf("old output epoch silently reused: %+v", got)
	}
	owner.SessionEpoch = recovered.SessionEpoch
	owner.Action = "terminal_write"
	owner.Content = "printf 'SCOPE_WORKER_UID:%s\\n' \"$(id -u)\"\n"
	if got := s2.terminalAction(owner); !got.OK {
		t.Fatalf("worker input after recovered backend failed: %+v", got)
	}
	owner.Action = "terminal_read"
	owner.Content = ""
	owner.Limit = 8192
	var seen strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res := s2.terminalAction(owner)
		if !res.OK {
			t.Fatalf("worker output after recovered backend failed: %+v", res)
		}
		owner.Cursor = res.NextCursor
		raw, err := base64.StdEncoding.DecodeString(res.Output)
		if err != nil {
			t.Fatal(err)
		}
		seen.Write(raw)
		if strings.Contains(seen.String(), fmt.Sprintf("SCOPE_WORKER_UID:%d", uid)) {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !strings.Contains(seen.String(), fmt.Sprintf("SCOPE_WORKER_UID:%d", uid)) {
		t.Fatalf("no worker-identity proof after control recovery; got %q", seen.String())
	}
	owner.Action = "terminal_close"
	if closed := s2.terminalAction(owner); !closed.OK {
		t.Fatalf("production terminal close did not clean scope: %+v", closed)
	}
	if err := verifyScopedTerminalBackend(generation); err == nil {
		t.Fatal("systemd scope remained active after terminal_close")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("private socket survived verified close: %v", err)
	}
	// An Executor crash loses in-memory expiry timers. Its next terminal
	// open must reconcile expired, trusted scope records instead of letting
	// eight expired records permanently consume all available session slots.
	newRequest := Request{
		Action: "terminal_open", PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass,
		Workspace: project, Columns: 80, Rows: 24, RequestID: "expired-managed-scope-reaper",
	}
	newSession := s2.terminalAction(newRequest)
	if !newSession.OK {
		t.Fatalf("new worker scope for stale-record proof: %+v", newSession)
	}
	newRequest.SessionID, newRequest.SessionEpoch = newSession.SessionID, newSession.SessionEpoch
	e2, err := s2.sessions.get(newRequest, newSession.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	name2 := e2.terminal.name
	t.Cleanup(func() { _ = stopScopedTerminalBackend(name2) })
	path := filepath.Join(state, "terminal-sessions", newSession.SessionID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oldRecord terminalRecord
	if err := json.Unmarshal(body, &oldRecord); err != nil {
		t.Fatal(err)
	}
	if !oldRecord.Scoped {
		t.Fatal("new record did not persist protected scope provenance")
	}
	oldRecord.ExpiresAt = time.Now().Add(-time.Minute)
	body, err = json.Marshal(oldRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	count, err := s2.terminalRecordCount()
	if err != nil || count != 0 {
		t.Fatalf("expired trusted scope record not pruned: count=%d err=%v", count, err)
	}
	if err := verifyScopedTerminalBackend(name2); err == nil {
		t.Fatal("expired terminal backend scope remained active")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired root-owned record not removed: %v", err)
	}
	if err := s2.sessions.removeAndStop(e2); err != nil {
		t.Fatalf("scoped control cleanup after expired server: %v", err)
	}
	// If PID1 expires the backend while its Executor process still lives,
	// explicit terminal_close must reconcile a now-dead Control Mode stream
	// using the authoritative stopped scope, rather than leak its record.
	gone := Request{Action: "terminal_open", PrincipalID: owner.PrincipalID,
		PrincipalClass: owner.PrincipalClass, Workspace: project,
		Columns: 80, Rows: 24, RequestID: "expired-backend-close"}
	goneOpened := s2.terminalAction(gone)
	if !goneOpened.OK {
		t.Fatalf("backend expiry cleanup fixture open: %+v", goneOpened)
	}
	gone.SessionID, gone.SessionEpoch = goneOpened.SessionID, goneOpened.SessionEpoch
	goneEntry, err := s2.sessions.get(gone, gone.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	goneName := goneEntry.terminal.name
	t.Cleanup(func() { _ = stopScopedTerminalBackend(goneName) })
	if err := stopScopedTerminalBackend(goneName); err != nil {
		t.Fatalf("could not simulate PID1 backend deadline: %v", err)
	}
	gone.Action = "terminal_close"
	if closed := s2.terminalAction(gone); !closed.OK {
		t.Fatalf("close of scope already ended by PID1 not reconciled: %+v", closed)
	}
	if _, err := os.Stat(filepath.Join(state, "terminal-sessions", gone.SessionID+".json")); !os.IsNotExist(err) {
		t.Fatalf("dead-backend terminal record survived verified close: %v", err)
	}
	t.Log("real scope, restart, principal isolation, stale-record GC, expired-backend close PASS")
}
