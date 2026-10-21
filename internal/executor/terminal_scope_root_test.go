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

// For the 197 session-lifecycle acceptance, a test-owned host with the
// explicit per-host opt-in may exercise isolated tmux/systemd fixtures.
// This grants no permission to restart any installed Agent service.
func approvedPTYDisposableFixtureHost(host string) bool {
	if host == "" || os.Geteuid() != 0 || os.Getenv("ASA_PTY_SCOPE_APPROVED_HOST") != host {
		return false
	}
	return host == "CLY535343" || (host == "hDE01" && os.Getenv("ASA137_E2E_HOST") == host) ||
		os.Getenv("ASA_197_TERMINAL_DISPOSABLE_HOST") == host
}

// This test must never execute as part of ordinary CI. It launches a genuine
// root-owned transient systemd scope and drops the pane to aiworker. Only a
// separately authorized disposable host may opt in explicitly.
// The hDE01 path additionally requires the Issue #137 E2E environment gate;
// it neither broadens the historical CLY535343 gate nor affects installed services.
func TestApprovedWorkerTmuxScopeSurvivesBrokerStop(t *testing.T) {
	hostname, _ := os.Hostname()
	if !approvedPTYDisposableFixtureHost(hostname) {
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
	if err := os.Chmod(state, 0711); err != nil {
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
	if err := verifyScopedTerminalBackendForVersion(generation, 2); err != nil {
		t.Fatalf("scope lost cgroup or lifetime boundary: %v", err)
	}
	if err := verifyTerminalLeaseTimer(generation); err != nil {
		t.Fatalf("independent PID1 idle timer missing: %v", err)
	}
	// Move a test-owned durable lease near expiry to exercise the real public
	// terminal_read renewal, including persisted deadline and restarted PID1 timer.
	// This never reduces the actual one-hour timer of a live Agent session.
	old, err := s.readTerminalRecord(owner)
	if err != nil || old.Version != 2 {
		t.Fatalf("new scope lacks versioned lease: %+v %v", old, err)
	}
	old.ExpiresAt = time.Now().Add(20 * time.Second)
	if _, err := s.writeTerminalRecord(old, true); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.terminalLeaseExpiresAt = old.ExpiresAt
	e.mu.Unlock()
	readLease := owner
	readLease.Action, readLease.Limit = "terminal_read", 8192
	if response := s.terminalAction(readLease); !response.OK {
		t.Fatalf("authenticated read failed to renew near-expiry lease: %+v", response)
	}
	renewed, err := s.readTerminalRecord(owner)
	if err != nil || !renewed.ExpiresAt.After(time.Now().Add(58*time.Minute)) || !renewed.CreatedAt.Equal(old.CreatedAt) {
		t.Fatalf("renewal did not persist the same session with extended idle deadline: %+v %v", renewed, err)
	}
	if err := verifyTerminalLeaseTimer(generation); err != nil {
		t.Fatalf("renewed PID1 timer failed verification: %v", err)
	}
	// Worker UID must NOT attach directly to another principal's root-owned
	// socket, even when it knows the opaque socket path.
	if err := exec.Command("/usr/bin/setpriv", "--reuid="+account.Uid, "--regid="+account.Gid,
		"--clear-groups", "/usr/bin/tmux", "-S", sock, "list-sessions").Run(); err == nil {
		t.Fatal("aiworker acquired root-owned tmux socket authority")
	}
	// Simulate loss of the Executor-owned Control Mode broker process.
	// Unlike the former single-cgroup implementation, the server remains
	// under PID1 ownership in a separately hard-capped scope with a
	// renewable one-hour inactivity timer.
	if err := s.sessions.removeAndStop(e); err != nil {
		t.Fatal(err)
	}
	if err := verifyScopedTerminalBackendForVersion(generation, 2); err != nil {
		t.Fatalf("backend did not outlive Control Mode: %v", err)
	}
	if err := verifyTerminalLeaseTimer(generation); err != nil {
		t.Fatalf("PID1 timer did not survive lost Executor client: %v", err)
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
	expectedEnv := fmt.Sprintf("SCOPE_WORKER_UID:%d SCOPE_HOME:%s SCOPE_XDG:%s SCOPE_CWD:%s", uid, home, filepath.Join(home, ".config"), project)
	owner.Content = "printf 'SCOPE_WORKER_UID:%s SCOPE_HOME:%s SCOPE_XDG:%s SCOPE_CWD:%s\\n' \"$(id -u)\" \"$HOME\" \"$XDG_CONFIG_HOME\" \"$PWD\"\n"
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
		if strings.Contains(seen.String(), expectedEnv) {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !strings.Contains(seen.String(), expectedEnv) {
		t.Fatalf("no worker-identity proof after control recovery; got %q", seen.String())
	}
	owner.Action = "terminal_close"
	if closed := s2.terminalAction(owner); !closed.OK {
		t.Fatalf("production terminal close did not clean scope: %+v", closed)
	}
	if err := verifyScopedTerminalBackendForVersion(generation, 2); err == nil {
		t.Fatal("systemd scope remained active after terminal_close")
	}
	if err := verifyTerminalLeaseTimer(generation); err == nil {
		t.Fatal("PID1 idle timer survived verified close")
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
	// v2 persistence binds expiry to an immutable creation timestamp.
	// Simulate an aged but internally coherent record, not a forged lease
	// whose expiry predates its creation and must remain fail-closed.
	oldRecord.CreatedAt = oldRecord.ExpiresAt.Add(-maxStdioSessionAge)
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
	if err := verifyScopedTerminalBackendForVersion(name2, 2); err == nil {
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
	// Losing the independently managed timer while the scope still lives
	// must fail the next authenticated I/O and reconnect closed, including if
	// the in-memory record still claims almost one hour of idle capacity.
	if err := stopTerminalLeaseTimer(goneName); err != nil {
		t.Fatalf("could not simulate disappeared PID1 idle timer: %v", err)
	}
	gone.Action = "terminal_read"
	if blocked := s2.terminalAction(gone); blocked.OK || blocked.ErrorCode != "terminal_lease_unverified" {
		t.Fatalf("missing PID1 fallback was masked by cached lease: %+v", blocked)
	}
	gone.Action = "terminal_reconnect"
	if blocked := s2.terminalAction(gone); blocked.OK || blocked.ErrorCode != "terminal_lease_unverified" {
		t.Fatalf("reconnect hid missing PID1 timer: %+v", blocked)
	}
	// A completely new Executor must refuse to reconnect to the missing
	// timer, but the original authenticated owner can explicitly close the
	// exact detached ACTIVE scope without ever attaching to a dead timer.
	if err := s2.sessions.removeAndStop(goneEntry); err != nil {
		t.Fatal(err)
	}
	fresh := &Server{cfg: s.cfg, workerUID: s.workerUID, workerGID: s.workerGID,
		sessions: newStdioSessionBroker(), audit: s.audit}
	if blocked := fresh.terminalAction(gone); blocked.OK || blocked.ErrorCode != "terminal_lease_unverified" {
		t.Fatalf("new Executor reconnected without PID1 timer: %+v", blocked)
	}
	gone.Action = "terminal_close"
	if closed := fresh.terminalAction(gone); !closed.OK {
		t.Fatalf("original owner could not clean detached timerless ACTIVE scope: %+v", closed)
	}
	if err := verifyScopedTerminalBackendForVersion(goneName, 2); err == nil {
		t.Fatal("detached terminal scope remained after verified owner close")
	}
	if _, err := os.Stat(filepath.Join(state, "terminal-sessions", gone.SessionID+".json")); !os.IsNotExist(err) {
		t.Fatalf("detached ACTIVE record survived verified close: %v", err)
	}
	// Separately retain the original scenario: PID1 can expire the backend
	// while the old in-process Control Mode connection is still present.
	expired := Request{Action: "terminal_open", PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass,
		Workspace: project, Columns: 80, Rows: 24, RequestID: "expired-backend-live-client"}
	openedExpired := fresh.terminalAction(expired)
	if !openedExpired.OK {
		t.Fatalf("expired backend fixture open: %+v", openedExpired)
	}
	expired.SessionID, expired.SessionEpoch = openedExpired.SessionID, openedExpired.SessionEpoch
	expiredEntry, err := fresh.sessions.get(expired, expired.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	expiredName := expiredEntry.terminal.name
	t.Cleanup(func() { _ = stopScopedTerminalBackend(expiredName) })
	if err := stopScopedTerminalBackend(expiredName); err != nil {
		t.Fatalf("could not simulate PID1 backend deadline: %v", err)
	}
	expired.Action = "terminal_close"
	if closed := fresh.terminalAction(expired); !closed.OK {
		t.Fatalf("close of already-ended PID1 scope not reconciled: %+v", closed)
	}
	if _, err := os.Stat(filepath.Join(state, "terminal-sessions", expired.SessionID+".json")); !os.IsNotExist(err) {
		t.Fatalf("dead-backend record survived verified close: %v", err)
	}
	t.Log("real scope, service reattach, principal isolation, missing-timer detached close, stale GC, expired-backend close PASS")
}
