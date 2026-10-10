package executor

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// Explicit, root-authorized, disposable development-host evidence only.
// Normal CI and unprivileged go test cannot activate this test.
func TestApprovedWorkerTmuxPrincipalSocketIsolation(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host != "CLY581741" || os.Getenv("ASA_WORKER_TMUX_ISOLATION_HOST") != host || os.Geteuid() != 0 {
		t.Skip("requires approved dedicated development host and explicit root opt-in")
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
	base, err := os.MkdirTemp("/tmp", "asa-worker-private-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "project")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(workspace, uid, gid); err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "asa-worker-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
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
	s := &Server{
		cfg:       config.Config{WorkspaceDir: base, StateDir: state},
		workerUID: uint32(uid), workerGID: uint32(gid),
		sessions: newStdioSessionBroker(),
		audit:    audit.New(filepath.Join(state, "audit.jsonl")),
	}
	owner := Request{Action: "terminal_open", PrincipalID: "principal-A", PrincipalClass: "direct", Workspace: workspace, Columns: 80, Rows: 24, RequestID: "worker-isolation-test"}
	opened := s.terminalAction(owner)
	if !opened.OK {
		t.Fatalf("worker terminal could not start in executor-private namespace: %+v", opened)
	}
	owner.SessionID, owner.SessionEpoch = opened.SessionID, opened.SessionEpoch
	e, err := s.sessions.get(owner, opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	socket := e.terminal.socket
	t.Cleanup(func() {
		_ = exec.Command("/usr/bin/tmux", "-S", socket, "kill-server").Run()
	})
	for _, path := range []string{e.terminal.socketDir, socket} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			t.Fatalf("backend authority is not root-owned: %s", path)
		}
		if path == e.terminal.socketDir && info.Mode().Perm() != 0700 {
			t.Fatalf("private namespace not 0700: %s", path)
		}
	}
	// Same-UID worker with no Agent principal may discover the workspace,
	// but cannot enumerate or invoke tmux Control Mode on A's backend.
	worker := func(args ...string) *exec.Cmd {
		prefix := []string{"--reuid=" + strconv.Itoa(uid), "--regid=" + strconv.Itoa(gid), "--clear-groups"}
		return exec.Command("/usr/bin/setpriv", append(prefix, args...)...)
	}
	if err := worker("/usr/bin/tmux", "-S", socket, "list-sessions").Run(); err == nil {
		t.Fatal("ordinary aiworker unexpectedly connected directly to another principal's tmux server")
	}
	if err := worker("/usr/bin/ls", "-la", filepath.Dir(socket)).Run(); err == nil {
		t.Fatal("ordinary aiworker unexpectedly listed root-only tmux namespace")
	}
	other := owner
	other.PrincipalID = "principal-B"
	other.Action = "terminal_read"
	if got := s.terminalAction(other); got.OK || got.ErrorCode != "session_not_found" {
		t.Fatalf("principal B stole session through Agent broker: %+v", got)
	}
	cursor := uint64(0)
	await := func(agent *Server, req Request, marker string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		var collected bytes.Buffer
		for time.Now().Before(deadline) {
			read := req
			read.Action = "terminal_read"
			read.Limit = 16384
			read.Cursor = cursor
			result := agent.terminalAction(read)
			if !result.OK {
				t.Fatalf("read: %+v", result)
			}
			cursor = result.NextCursor
			payload, err := base64.StdEncoding.DecodeString(result.Output)
			if err != nil {
				t.Fatal(err)
			}
			collected.Write(payload)
			if bytes.Contains(collected.Bytes(), []byte(marker)) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("missing %q from worker terminal: %q", marker, collected.String())
	}
	write := func(agent *Server, req Request, input string) {
		t.Helper()
		req.Action = "terminal_write"
		req.Content = input
		if result := agent.terminalAction(req); !result.OK {
			t.Fatalf("write: %+v", result)
		}
	}
	write(s, owner, `printf 'WORKER_ID=%s\n' "$(id -u)"`+"\n")
	await(s, owner, fmt.Sprintf("WORKER_ID=%d", uid))
	write(s, owner, `printf 'HOME_CHECK=%s\n' "$HOME"`+"\n")
	await(s, owner, "HOME_CHECK="+home)
	write(s, owner, `printf 'XDG_CHECK=%s\n' "$XDG_CACHE_HOME"`+"\n")
	await(s, owner, "XDG_CHECK="+filepath.Join(home, ".cache"))
	write(s, owner, `printf 'TERM_CHECK=%s\n' "$TERM"`+"\n")
	await(s, owner, "TERM_CHECK=screen-256color")
	if err := s.sessions.removeAndStop(e); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("tmux server did not survive Control Mode detach: %v", err)
	}
	recoveredAgent := &Server{cfg: s.cfg, workerUID: s.workerUID, workerGID: s.workerGID,
		sessions: newStdioSessionBroker(), audit: s.audit}
	reconReq := owner
	reconReq.Action = "terminal_reconnect"
	recon := recoveredAgent.terminalAction(reconReq)
	if !recon.OK || recon.SessionID != owner.SessionID || recon.SessionEpoch == owner.SessionEpoch {
		t.Fatalf("secure backend reconnect failed: %+v", recon)
	}
	owner.SessionEpoch = recon.SessionEpoch
	cursor = 0
	if err := worker("/usr/bin/tmux", "-S", socket, "capture-pane", "-p").Run(); err == nil {
		t.Fatal("worker could read another principal's terminal after reconnect")
	}
	write(recoveredAgent, owner, `printf 'RECONNECTED_AS_WORKER\n'`+"\n")
	await(recoveredAgent, owner, "RECONNECTED_AS_WORKER")
	if err := worker("/usr/bin/tmux", "-S", socket, "kill-server").Run(); err == nil {
		t.Fatal("worker could kill another principal's terminal")
	}
	owner.Action = "terminal_close"
	if closed := recoveredAgent.terminalAction(owner); !closed.OK {
		t.Fatalf("terminal close: %+v", closed)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("private backend socket remained after close: %v", err)
	}
	// Root and worker namespaces must be disjoint even on a shared state root.
	if !strings.HasPrefix(socket, filepath.Join(state, "worker-terminals")+string(filepath.Separator)) {
		t.Fatalf("worker socket entered wrong backend namespace: %s", socket)
	}
}
