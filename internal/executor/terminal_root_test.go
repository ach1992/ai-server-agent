package executor

import (
	"bytes"
	"encoding/base64"
	"errors"
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

// This is an explicitly gated, NON-PRODUCTION, non-service test. It must not
// run as a side effect of CI or regular go test ./.... It runs only when an
// operator has authorized real root PTY effects on the specified dev host.
func TestApprovedRootTmuxRealNonProduction(t *testing.T) {
	approvedHost := os.Getenv("ASA_ROOT_TMUX_APPROVED_HOST")
	if approvedHost == "" || os.Geteuid() != 0 {
		t.Skip("requires an explicit approved development hostname and root test execution")
	}
	hostname, err := os.Hostname()
	if err != nil || hostname != approvedHost {
		t.Fatalf("root PTY proof denied on host %q; expected approved host mismatch: %v", hostname, err)
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("/tmp", "asa-root-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(base, "worktree")
	if err := os.Mkdir(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(worktree, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "private-state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Config{WorkspaceDir: base, StateDir: state},
		sessions: newStdioSessionBroker(), workerUID: uint32(uid), workerGID: uint32(gid),
		audit: audit.New(filepath.Join(state, "audit.jsonl"))}
	req := Request{Action: "terminal_open", PrincipalID: "root-pty-proof-only", PrincipalClass: "direct",
		Workspace: worktree, Root: true, Columns: 80, Rows: 24, RequestID: "req-scoped-root-test",
		ApprovalID: "approval-scoped-nonproduction-test"}
	if got := s.terminalAction(req); got.OK || got.ErrorCode != "approval_required" {
		t.Fatalf("root opened without approval: %+v", got)
	}
	req.Approval = true
	open := s.terminalAction(req)
	if !open.OK {
		if pending, err := s.sessions.get(req, open.SessionID); err == nil {
			pending.mu.Lock()
			t.Logf("root open diagnostics: exited=%t code=%d closed=%t ack=%d pane=%q protocol=%q tmux-exit=%t stderr=%v", pending.exited, pending.exitCode, pending.closed, pending.terminal.ack, pending.terminal.pane, pending.terminal.protocolError, pending.terminal.exited, func() []string {
				var lines []string
				for _, v := range pending.events {
					if v.Stream == "stderr" {
						lines = append(lines, string(v.Data))
					}
				}
				return lines
			}())
			pending.mu.Unlock()
		} else {
			t.Logf("root open no pending session: %v", err)
		}
		t.Fatalf("real root tmux open: %+v", open)
	}
	req.SessionID = open.SessionID
	req.SessionEpoch = open.SessionEpoch
	e, err := s.sessions.get(req, open.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sock := e.terminal.socket
	t.Cleanup(func() {
		if e.terminal != nil {
			kill := exec.Command("/usr/bin/tmux", "-S", sock, "kill-server")
			_ = kill.Run() // only this test-owned private socket
		}
	})
	if !strings.HasPrefix(sock, state+string(filepath.Separator)) {
		t.Fatalf("root tmux socket escaped private state: %s", sock)
	}
	for _, path := range []string{filepath.Dir(sock), sock} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			t.Fatalf("root socket state not owned by root: %s", path)
		}
		if path == filepath.Dir(sock) && info.Mode().Perm() != 0700 {
			t.Fatalf("root socket directory not private: %s %s", path, info.Mode())
		}
	}
	// Test as ACTUAL aiworker, not merely checking mode bits under root.
	denied := exec.Command("/usr/bin/su", "-s", "/bin/sh", "aiworker", "-c", "test -S "+sock)
	if err := denied.Run(); err == nil {
		t.Fatal("aiworker could traverse root-only tmux socket")
	}
	workerRead := req
	workerRead.Action = "terminal_read"
	workerRead.Root = false
	workerRead.Approval = false
	if got := s.terminalAction(workerRead); got.OK || got.ErrorCode != "session_not_found" {
		t.Fatalf("worker stole root session: %+v", got)
	}
	intruder := req
	intruder.Action = "terminal_read"
	intruder.PrincipalID = "different-principal"
	if got := s.terminalAction(intruder); got.OK || got.ErrorCode != "session_not_found" {
		t.Fatalf("other principal read root session: %+v", got)
	}
	cursor := uint64(0)
	read := func() (string, error) {
		req.Action = "terminal_read"
		req.Cursor = cursor
		req.Limit = 16384
		r := s.terminalAction(req)
		if !r.OK {
			return "", fmt.Errorf("read: %+v", r)
		}
		cursor = r.NextCursor
		b, err := base64.StdEncoding.DecodeString(r.Output)
		return string(b), err
	}
	send := func(content string) {
		t.Helper()
		req.Action = "terminal_write"
		req.Content = content
		got := s.terminalAction(req)
		if !got.OK {
			t.Fatalf("write: %+v", got)
		}
	}
	await := func(marker string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		var collected strings.Builder
		for time.Now().Before(deadline) {
			b, err := read()
			if err != nil {
				t.Fatal(err)
			}
			collected.WriteString(b)
			if strings.Contains(collected.String(), marker) {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("root PTY output %q not seen in bounded stream %q", marker, collected.String())
	}
	_, _ = read()
	send("printf 'ROOT_ID:'; id -u\n")
	await("ROOT_ID:0")
	send("read -r proof; printf 'ROOT_PROMPT:%s\\n' \"$proof\"\n")
	send("root-prompt-evidence\n")
	await("ROOT_PROMPT:root-prompt-evidence")
	resize := req
	resize.Action = "terminal_resize"
	resize.Columns = 67
	resize.Rows = 19
	if got := s.terminalAction(resize); !got.OK {
		t.Fatalf("root resize: %+v", got)
	}
	send("stty size\n")
	await("19 67")
	send("sleep 19\n")
	interrupt := req
	interrupt.Action = "terminal_interrupt"
	if got := s.terminalAction(interrupt); !got.OK {
		t.Fatalf("root interrupt: %+v", got)
	}
	send("printf 'ROOT_AFTER_INTERRUPT\\n'\n")
	await("ROOT_AFTER_INTERRUPT")
	send("printf '\\033[?1049hROOT_ALT_SCREEN\\033[?1049l\\n'\n")
	await("ROOT_ALT_SCREEN")
	// Exercise an interactive REPL rather than one-shot shell command emulation.
	send("python3 -q\n")
	// Use markers assembled by execution, not strings appearing verbatim
	// in echoed input. Otherwise a pending Python REPL can falsely satisfy
	// await() before the shell has actually resumed after exit().
	send("print('ROOT_PY_' + 'REPL')\n")
	await("ROOT_PY_REPL")
	send("exit()\n")
	send("printf 'ROOT_%s\\n' 'REPL_EXITED'\n")
	await("ROOT_REPL_EXITED")

	// Restart the executor-owned Control Mode connection only; do NOT
	// restart, replace or modify installed Agent system services.
	if err := s.sessions.removeAndStop(e); err != nil {
		t.Fatalf("control-mode detach: %v", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("root tmux backend did not survive detach: %v", err)
	}
	s2 := &Server{cfg: s.cfg, sessions: newStdioSessionBroker(), workerUID: s.workerUID,
		workerGID: s.workerGID, audit: s.audit}
	recon := req
	recon.Action = "terminal_reconnect"
	recovered := s2.terminalAction(recon)
	if !recovered.OK || recovered.SessionID != open.SessionID || recovered.SessionEpoch == open.SessionEpoch ||
		recovered.Columns != 67 || recovered.Rows != 19 || !recovered.RetentionTruncated {
		t.Fatalf("root restart reconciliation failed: %+v", recovered)
	}
	stale := req
	stale.Action = "terminal_read"
	if got := s2.terminalAction(stale); got.OK || got.ErrorCode != "terminal_epoch_changed" {
		t.Fatalf("old root epoch reused after reconnect: %+v", got)
	}
	req.SessionEpoch = recovered.SessionEpoch
	req.Action = "terminal_write"
	req.Content = "printf 'ROOT_AFTER_RECONNECT:'; id -u\n"
	if got := s2.terminalAction(req); !got.OK {
		t.Fatalf("post-reconnect write: %+v", got)
	}
	req.Action = "terminal_read"
	req.Content = ""
	req.Cursor = 0
	deadline := time.Now().Add(3 * time.Second)
	found := false
	var observed []byte
	for time.Now().Before(deadline) {
		r := s2.terminalAction(req)
		if !r.OK {
			t.Fatalf("post-reconnect read: %+v", r)
		}
		req.Cursor = r.NextCursor
		output, err := base64.StdEncoding.DecodeString(r.Output)
		if err != nil {
			t.Fatal(err)
		}
		observed = append(observed, output...)
		if len(observed) > 65536 {
			observed = observed[len(observed)-65536:]
		}
		if bytes.Contains(observed, []byte("ROOT_AFTER_RECONNECT:0")) {
			found = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !found {
		t.Fatalf("root command after tmux recovery not observed; bounded output=%q", observed)
	}
	// Simulate a user-created, detached SECOND tmux session: close must
	// kill the complete private server, not only the original shell.
	extra := exec.Command("/usr/bin/tmux", "-S", sock, "new-session", "-d", "-s", "extra-root-proof")
	if out, err := extra.CombinedOutput(); err != nil {
		t.Fatalf("root extra session: %v %q", err, out)
	}
	if out, err := exec.Command("/usr/bin/tmux", "-S", sock, "set-option", "-g", "exit-empty", "off").CombinedOutput(); err != nil {
		t.Fatalf("exit-empty preparation: %v %q", err, out)
	}
	req.Action = "terminal_close"
	closed := s2.terminalAction(req)
	if !closed.OK {
		t.Fatalf("root terminal cleanup uncertain: %+v", closed)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root server socket survives verified close: %v", err)
	}
	recordPath := filepath.Join(state, "terminal-sessions", open.SessionID+".json")
	if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root terminal identity record not removed: %v", err)
	}
	auditData, err := os.ReadFile(filepath.Join(state, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(auditData, []byte(`"mode":"root"`)) || !bytes.Contains(auditData, []byte("terminal_close")) {
		t.Fatal("missing root mode or close correlation from audit")
	}
	if bytes.Contains(auditData, []byte("root-prompt-evidence")) || bytes.Contains(auditData, []byte("ROOT_AFTER_RECONNECT")) {
		t.Fatal("raw root terminal input leaked into audit")
	}
	t.Logf("approved real-root PTY proof passed: owner uid=0, aiworker denied, repo=%q, socket cleanup proven", worktree)
}
