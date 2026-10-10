package executor

import (
	"encoding/base64"
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
	t.Log("real root-owned transient scope and aiworker pane, reconnect, cleanup PASS")
}
