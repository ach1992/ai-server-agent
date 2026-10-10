package executor

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// Opt-in, host-pinned, real PID1 backend with an injected failure AFTER
// successful scope verification but BEFORE publication of the ACTIVE record.
// The installed Agent/Executor services are never restarted or replaced.
func TestApprovedRealScopedPTYPreActiveExecutorCrashRecovery(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host != "CLY535343" || os.Geteuid() != 0 ||
		os.Getenv("ASA_PTY_SCOPE_APPROVED_HOST") != host {
		t.Skip("explicit approved disposable root test host required")
	}
	acct, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(acct.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(acct.Gid)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "asa-pty-pend-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
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
	createServer := func() *Server {
		return &Server{
			cfg:       config.Config{WorkspaceDir: project, StateDir: state, WorkerUser: "aiworker"},
			workerUID: uint32(uid), workerGID: uint32(gid),
			sessions: newStdioSessionBroker(), audit: audit.New(filepath.Join(state, "audit.jsonl")),
		}
	}
	s := createServer()
	var scopeName string
	s.terminalActivePublishTestHook = func(term *tmuxTerminalState) error {
		scopeName = term.name
		if err := verifyScopedTerminalBackend(scopeName); err != nil {
			return err
		}
		return errors.New("injected post-scope pre-ACTIVE publication failure")
	}
	req := Request{
		Action: "terminal_open", PrincipalID: "pending-live-owner", PrincipalClass: "direct",
		Workspace: project, Columns: 80, Rows: 24, RequestID: "preactive-real-scope",
	}
	result := s.terminalAction(req)
	if scopeName != "" {
		t.Cleanup(func() { _ = stopScopedTerminalBackend(scopeName) })
	}
	if result.OK || result.ErrorCode != "terminal_start_uncertain" ||
		!validTerminalID(result.SessionID) || result.SessionEpoch == "" {
		t.Fatalf("post-scope publication failure was lost: %+v", result)
	}
	req.SessionID, req.SessionEpoch = result.SessionID, result.SessionEpoch
	rec, err := s.readOwnedTerminalRecord(req, true)
	if err != nil || rec.Lifecycle != "pending" || rec.Name != scopeName {
		t.Fatalf("real scope did not retain durable PENDING: %+v %v", rec, err)
	}
	entry, err := s.sessions.get(req, req.SessionID)
	if err != nil || !entry.terminal.scopeUncertain {
		t.Fatalf("in-memory active access was not disabled: %v", err)
	}
	// Simulate a dead Executor's Control Mode client. Its separately scoped
	// tmux server MUST survive and remain owned by the root state record.
	if err := s.sessions.removeAndStop(entry); err != nil {
		t.Fatalf("could not retire isolated Control Mode client: %v", err)
	}
	if err := verifyScopedTerminalBackend(scopeName); err != nil {
		t.Fatalf("scope died with its client: %v", err)
	}
	restarted := createServer() // completely new broker, no prior sessions
	req.Action = "terminal_reconnect"
	if denial := restarted.terminalAction(req); denial.OK || denial.ErrorCode != "terminal_reconnect_unknown" {
		t.Fatalf("new Executor attached to incomplete scope: %+v", denial)
	}
	wrong := req
	wrong.PrincipalID = "not-the-owner"
	wrong.Action = "terminal_close"
	if denial := restarted.terminalAction(wrong); denial.OK || denial.ErrorCode != "session_not_found" {
		t.Fatalf("another principal closed pending scope: %+v", denial)
	}
	req.Action = "terminal_close"
	if closed := restarted.terminalAction(req); !closed.OK {
		t.Fatalf("fresh Executor could not stop exact PID1 scope: %+v", closed)
	}
	if count, err := restarted.terminalRecordCount(); err != nil || count != 0 {
		t.Fatalf("real scoped terminal capacity not freed: %d %v", count, err)
	}
	if err := verifyScopedTerminalBackend(scopeName); err == nil {
		t.Fatal("backend still active after verified pending close")
	}
}
