package executor

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
	"time"
)

func TestTmuxControlModeDecoderAndBounds(t *testing.T) {
	b, err := decodeTmuxOutput(`test\015\012\033[31m`)
	if err != nil || !bytes.Equal(b, []byte("test\r\n\x1b[31m")) {
		t.Fatalf("decode %q: %v", b, err)
	}
	if _, err := decodeTmuxOutput(`broken\8ab`); err == nil {
		t.Fatal("accepted malformed escape")
	}
	state := &tmuxTerminalState{}
	state.accept([]byte("%output %1 ab\\0"))
	state.accept([]byte("15\\012\n%begin 1 2 3\n%end 1 2 3\n"))
	if state.ack != 1 || state.pane != "%1" || len(state.chunks) != 1 || string(state.chunks[0].data) != "ab\r\n" {
		t.Fatalf("fragmented protocol was not parsed: %+v", state)
	}
	state.accept([]byte("%output %2 changed\n"))
	if state.protocolError != "" || len(state.chunks) != 2 || state.chunks[1].pane != "%2" {
		t.Fatalf("multi-pane output was not tagged: %+v", state)
	}
	state.accept([]byte("%output %x bad\n"))
	if state.protocolError != "terminal_invalid_pane_id" {
		t.Fatalf("invalid pane accepted: %+v", state)
	}
}

func TestWorkerTerminalRealTmuxCrossCall(t *testing.T) {
	bin := os.Getenv("ASA_TEST_TMUX_BIN")
	if bin == "" {
		t.Skip("isolated tmux binary not supplied; production does not require optional tmux")
	}
	s, owner, workspace := testStdioBroker(t)
	s.terminalBinary = bin
	owner.Action = "terminal_open"
	owner.Columns = 80
	owner.Rows = 24
	opened := s.terminalAction(owner)
	if !opened.OK {
		t.Fatalf("tmux open failed: %+v", opened)
	}
	id := opened.SessionID
	owner.SessionEpoch = opened.SessionEpoch
	e, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeReq := owner
		closeReq.Action = "terminal_close"
		closeReq.SessionID = id
		_ = s.terminalAction(closeReq)
		cmd := exec.Command(bin, "-S", e.terminal.socket, "kill-server")
		_ = cmd.Run() // fixture-only cleanup if the test failed before the normal close
	})
	readReq := owner
	readReq.Action = "terminal_read"
	readReq.SessionID = id
	readReq.Limit = 8192
	read := func(cursor uint64) (Response, []byte) {
		t.Helper()
		readReq.Cursor = cursor
		r := s.terminalAction(readReq)
		if !r.OK {
			t.Fatalf("terminal read: %+v", r)
		}
		b, err := base64.StdEncoding.DecodeString(r.Output)
		if err != nil {
			t.Fatal(err)
		}
		return r, b
	}
	current, _ := read(0)
	send := func(input string) {
		t.Helper()
		req := owner
		req.Action = "terminal_write"
		req.SessionID = id
		req.Content = input
		r := s.terminalAction(req)
		if !r.OK {
			t.Fatalf("send %q: %+v", input, r)
		}
	}
	await := func(marker string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		var collected []byte
		for time.Now().Before(deadline) {
			var output []byte
			current, output = read(current.NextCursor)
			collected = append(collected, output...)
			if bytes.Contains(collected, []byte(marker)) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("missing %q in bounded incremental output %q", marker, collected)
	}
	// Literal input is passed into the same real shell across multiple calls.
	send("read -r demo; printf 'REAL_VALUE:%s\\n' \"$demo\"\n")
	send("worker42\n")
	await("REAL_VALUE:worker42")
	other := readReq
	other.PrincipalID = "different-principal"
	if r := s.terminalAction(other); r.OK || r.ErrorCode != "session_not_found" {
		t.Fatalf("cross-principal access: %+v", r)
	}
	wrong := owner
	wrong.Action = "terminal_write"
	wrong.SessionID = id
	wrong.Workspace = workspace + "-wrong"
	wrong.Content = "echo UNAUTHORIZED\n"
	if r := s.terminalAction(wrong); r.OK {
		t.Fatalf("workspace mismatch allowed: %+v", r)
	}
	resize := owner
	resize.Action = "terminal_resize"
	resize.SessionID = id
	resize.Columns = 55
	resize.Rows = 13
	if r := s.terminalAction(resize); !r.OK {
		t.Fatalf("resize: %+v", r)
	}
	send("stty size\n")
	await("13 55")
	send("sleep 25\n")
	interrupt := owner
	interrupt.Action = "terminal_interrupt"
	interrupt.SessionID = id
	if r := s.terminalAction(interrupt); !r.OK {
		t.Fatalf("ctrl-c: %+v", r)
	}
	send("printf 'AFTER_INTERRUPT\\n'\n")
	await("AFTER_INTERRUPT")
	send("printf '\\033[31mCOLOR\\033[0m\\n'\n")
	await("COLOR")
	closeReq := owner
	closeReq.Action = "terminal_close"
	closeReq.SessionID = id
	if r := s.terminalAction(closeReq); !r.OK {
		t.Fatalf("close: %+v", r)
	}
	if _, err := s.sessions.get(owner, id); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("closed session accessible: %v", err)
	}
	if _, err := os.Stat(e.terminal.socket); !os.IsNotExist(err) {
		t.Fatalf("tmux server socket remains: %v", err)
	}
}

func TestWorkerTerminalRejectsOversizeAndUnauthenticated(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	owner.Action = "terminal_open"
	owner.Columns = 80
	owner.Rows = 24
	owner.PrincipalID = ""
	if r := s.terminalAction(owner); r.OK {
		t.Fatal("unauthenticated open succeeded")
	}
	owner.PrincipalID = "a"
	owner.Root = true
	owner.Approval = true
	if r := s.terminalAction(owner); r.OK || !strings.Contains(r.ErrorCode, "root_executor_unavailable") {
		t.Fatalf("root must fail closed until authorized implementation: %+v", r)
	}
}

func TestWorkerTmuxRealExecutorRestartReconnect(t *testing.T) {
	bin := os.Getenv("ASA_TEST_TMUX_BIN")
	if bin == "" {
		t.Skip("isolated optional tmux fixture not supplied")
	}
	s, owner, workspace := testStdioBroker(t)
	s.terminalBinary = bin
	owner.Action = "terminal_open"
	owner.Columns = 80
	owner.Rows = 24
	opened := s.terminalAction(owner)
	if !opened.OK {
		t.Fatalf("open: %+v", opened)
	}
	e, err := s.sessions.get(owner, opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	resize := owner
	resize.Action = "terminal_resize"
	resize.SessionID = opened.SessionID
	resize.SessionEpoch = opened.SessionEpoch
	resize.Columns = 55
	resize.Rows = 13
	if r := s.terminalAction(resize); !r.OK {
		t.Fatalf("pre-restart resize: %+v", r)
	}
	// This ends only the executor-owned Control Mode client. The authentic
	// tmux pane/server should remain, as it would during executor restart.
	socket := e.terminal.socket
	t.Cleanup(func() {
		cmd := exec.Command(bin, "-S", socket, "kill-server")
		_ = cmd.Run()
	})
	if err := s.sessions.removeAndStop(e); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("tmux backend did not survive client loss: %v", err)
	}
	s2 := &Server{cfg: s.cfg, workerUID: s.workerUID, workerGID: s.workerGID,
		sessions: newStdioSessionBroker(), terminalBinary: bin}
	reconnect := owner
	reconnect.Action = "terminal_reconnect"
	reconnect.SessionID = opened.SessionID
	result := s2.terminalAction(reconnect)
	if !result.OK {
		t.Fatalf("recover: %+v", result)
	}
	if result.SessionID != opened.SessionID || result.SessionEpoch == opened.SessionEpoch || !result.RetentionTruncated || result.Columns != 55 || result.Rows != 13 {
		t.Fatalf("stale epoch or gap not disclosed: %+v", result)
	}
	invalid := owner
	invalid.Action = "terminal_read"
	invalid.SessionID = result.SessionID
	invalid.SessionEpoch = opened.SessionEpoch
	if r := s2.terminalAction(invalid); r.OK || r.ErrorCode != "terminal_epoch_changed" {
		t.Fatalf("stale epoch silently reused: %+v", r)
	}
	other := reconnect
	other.PrincipalID = "other"
	if r := s2.terminalAction(other); r.OK {
		t.Fatalf("different principal reattached: %+v", r)
	}
	wrong := reconnect
	wrong.Workspace = workspace + "-outside"
	if r := s2.terminalAction(wrong); r.OK {
		t.Fatalf("different workspace reattached: %+v", r)
	}
	write := owner
	write.Action = "terminal_write"
	write.SessionID = result.SessionID
	write.SessionEpoch = result.SessionEpoch
	write.Content = "printf 'RECOVERED42\\n'\n"
	if r := s2.terminalAction(write); !r.OK {
		t.Fatalf("post-restart write: %+v", r)
	}
	read := owner
	read.Action = "terminal_read"
	read.SessionID = result.SessionID
	read.SessionEpoch = result.SessionEpoch
	deadline := time.Now().Add(3 * time.Second)
	var found bool
	var cursor uint64
	for time.Now().Before(deadline) {
		read.Cursor = cursor
		got := s2.terminalAction(read)
		if !got.OK {
			t.Fatalf("post-restart read: %+v", got)
		}
		cursor = got.NextCursor
		data, err := base64.StdEncoding.DecodeString(got.Output)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("RECOVERED42")) {
			found = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !found {
		t.Fatal("no real post-restart output")
	}
	closeReq := owner
	closeReq.Action = "terminal_close"
	closeReq.SessionID = result.SessionID
	closeReq.SessionEpoch = result.SessionEpoch
	if got := s2.terminalAction(closeReq); !got.OK {
		t.Fatalf("post-restart close: %+v", got)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("tmux socket survived close: %v", err)
	}
}

// Optional genuine tmux Control Mode evidence for the source-only shared
// capacity invariant. Two original backends survive Executor reconstruction;
// downscaling to one must not prevent either original owner from reattaching
// and closing, nor permit a third process to take that occupied budget.
func TestWorkerTmuxRecoveredTerminalsDownscaleIndependentCapacity(t *testing.T) {
	bin := os.Getenv("ASA_TEST_TMUX_BIN")
	if bin == "" {
		t.Skip("isolated optional real-tmux fixture not supplied")
	}
	s, owner, _ := testStdioBroker(t)
	// The real tmux fixture needs a short sockaddr_un path; do not weaken
	// production socket length or symlink ownership validation for tests.
	workspace, err := os.MkdirTemp("/tmp", "asa-r1-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	s.cfg.WorkspaceDir = workspace
	owner.Workspace = workspace
	runtime := config.DefaultRuntimeSettings()
	runtime.StdioSessionSlots = 2
	s.cfg.Runtime = &runtime
	s.sessions = newStdioSessionBrokerForSlots(2)
	s.terminalBinary = bin
	owner.Action, owner.Columns, owner.Rows = "terminal_open", 80, 24
	type surviving struct{ id, epoch, socket string }
	saved := make([]surviving, 0, 2)
	for i := 0; i < 2; i++ {
		got := s.terminalAction(owner)
		if !got.OK {
			t.Fatalf("original scoped terminal %d: %+v", i, got)
		}
		request := owner
		request.SessionID = got.SessionID
		e, err := s.sessions.get(request, request.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		socket := e.terminal.socket
		t.Cleanup(func() { _ = exec.Command(bin, "-S", socket, "kill-server").Run() })
		saved = append(saved, surviving{id: got.SessionID, epoch: got.SessionEpoch, socket: socket})
		if err := s.sessions.removeAndStop(e); err != nil {
			t.Fatalf("detach original control client: %v", err)
		}
	}
	runtime.StdioSessionSlots = 1
	fresh := &Server{cfg: s.cfg, workerUID: s.workerUID, workerGID: s.workerGID,
		sessions: newStdioSessionBrokerForSlots(1), terminalBinary: bin}
	if _, err := fresh.workerStdioSession(owner, "lsp", workspace, "/bin/cat"); !errors.Is(err, errSessionBusy) {
		t.Fatalf("downscaled broker admitted new worker despite original terminals: %v", err)
	}
	for i, original := range saved {
		reconnect := owner
		reconnect.Action, reconnect.SessionID = "terminal_reconnect", original.id
		badOwner := reconnect
		badOwner.PrincipalID = "not-the-original-owner"
		if got := fresh.terminalAction(badOwner); got.OK {
			t.Fatalf("foreign owner recovered original terminal %d: %+v", i, got)
		}
		badWorkspace := reconnect
		badWorkspace.Workspace = workspace + "-wrong"
		if got := fresh.terminalAction(badWorkspace); got.OK {
			t.Fatalf("workspace mismatch recovered original terminal %d: %+v", i, got)
		}
		badRoot := reconnect
		badRoot.Root, badRoot.Approval = true, true
		if got := fresh.terminalAction(badRoot); got.OK {
			t.Fatalf("wrong root authority recovered original terminal %d: %+v", i, got)
		}
		got := fresh.terminalAction(reconnect)
		if !got.OK || got.SessionID != original.id || got.SessionEpoch == original.epoch ||
			!got.RetentionTruncated {
			t.Fatalf("reconnection %d failed after downscale: %+v", i, got)
		}
		oldEpoch := reconnect
		oldEpoch.Action, oldEpoch.SessionEpoch = "terminal_close", original.epoch
		if denied := fresh.terminalAction(oldEpoch); denied.OK || denied.ErrorCode != "terminal_epoch_changed" {
			t.Fatalf("original output epoch incorrectly authorized close: %+v", denied)
		}
		// Leave both reconnected clients admitted simultaneously even though
		// the NEW admission limit is now 1. Both occupy old durable records.
	}
	if _, err := fresh.workerStdioSession(owner, "dap", workspace, "/bin/cat"); !errors.Is(err, errSessionBusy) {
		t.Fatalf("fresh DAP admitted while two old terminal clients remain: %v", err)
	}
	for _, original := range saved {
		closing := owner
		closing.Action, closing.SessionID = "terminal_reconnect", original.id
		current := fresh.terminalAction(closing)
		if !current.OK || current.Status != "already_connected" {
			t.Fatalf("reconnected original identity disappeared: %+v", current)
		}
		closing.Action, closing.SessionEpoch = "terminal_close", current.SessionEpoch
		if closed := fresh.terminalAction(closing); !closed.OK {
			t.Fatalf("cannot close original terminal after downscale: %+v", closed)
		}
		if _, err := os.Stat(original.socket); !os.IsNotExist(err) {
			t.Fatalf("verified close left original socket: %v", err)
		}
	}
	lsp, err := fresh.workerStdioSession(owner, "lsp", workspace, "/bin/cat")
	if err != nil {
		t.Fatalf("capacity unavailable after both verified closes: %v", err)
	}
	_ = fresh.sessions.close(owner, lsp)
}

func TestWorkerTmuxRealOutputFloodIsBounded(t *testing.T) {
	bin := os.Getenv("ASA_TEST_TMUX_BIN")
	if bin == "" {
		t.Skip("isolated optional tmux fixture not supplied")
	}
	s, owner, _ := testStdioBroker(t)
	s.terminalBinary = bin
	owner.Action = "terminal_open"
	owner.Columns = 80
	owner.Rows = 24
	opened := s.terminalAction(owner)
	if !opened.OK {
		t.Fatalf("open: %+v", opened)
	}
	owner.SessionEpoch = opened.SessionEpoch
	owner.SessionID = opened.SessionID
	t.Cleanup(func() { owner.Action = "terminal_close"; _ = s.terminalAction(owner) })
	owner.Action = "terminal_write"
	owner.Content = "head -c 200000 /dev/zero | tr '\\000' Q; printf '\\nEND_OF_FLOOD\\n'\n"
	if r := s.terminalAction(owner); !r.OK {
		t.Fatalf("send: %+v", r)
	}
	e, _ := s.sessions.get(owner, opened.SessionID)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		latest := e.terminal.latest
		count := len(e.terminal.chunks)
		size := e.terminal.retained
		failed := e.terminal.protocolError
		e.mu.Unlock()
		if failed != "" {
			t.Fatalf("malformed control flood: %s", failed)
		}
		if latest > 100 {
			if size > maxStdioOutputBytes || count > maxStdioOutputEvents {
				t.Fatalf("unbounded retained output %d bytes / %d events", size, count)
			}
			owner.Action = "terminal_read"
			owner.Cursor = 0
			owner.Limit = 8192
			got := s.terminalAction(owner)
			if !got.OK || !got.RetentionTruncated || got.BytesReturned > 8192 {
				t.Fatalf("flood retention not bounded/disclosed: %+v", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no genuine flood observed")
}

func TestRootTerminalIdentityCannotBeUsedAsWorker(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	id := "stdio_1234567890abcdef1234567890abcdef"
	e := &stdioSession{id: id, kind: "terminal", ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass,
		workspace: workspace, terminal: &tmuxTerminalState{root: true}}
	if err := s.sessions.reserve(e); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.remove(e)
	if _, err := s.sessions.get(owner, id); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("worker opened root terminal: %v", err)
	}
	owner.Root = true
	if _, err := s.sessions.get(owner, id); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("root session without approval: %v", err)
	}
	owner.Approval = true
	if _, err := s.sessions.get(owner, id); err != nil {
		t.Fatal(err)
	}
	owner.PrincipalClass = "other"
	if _, err := s.sessions.get(owner, id); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("principal class isolation bypass: %v", err)
	}
}

func TestWorkerTmuxMultiPaneAndAlternateScreen(t *testing.T) {
	bin := os.Getenv("ASA_TEST_TMUX_BIN")
	if bin == "" {
		t.Skip("optional isolated tmux fixture not supplied")
	}
	s, owner, _ := testStdioBroker(t)
	s.terminalBinary = bin
	owner.Action = "terminal_open"
	owner.Columns = 80
	owner.Rows = 24
	opened := s.terminalAction(owner)
	if !opened.OK {
		t.Fatalf("open: %+v", opened)
	}
	owner.SessionID = opened.SessionID
	owner.SessionEpoch = opened.SessionEpoch
	e, _ := s.sessions.get(owner, opened.SessionID)
	t.Cleanup(func() { owner.Action = "terminal_close"; _ = s.terminalAction(owner) })
	outputCmd := exec.Command(bin, "-S", e.terminal.socket, "split-window", "-t", e.terminal.name, "-d", "sh -c 'printf SIDE_PANE_PROOF; sleep 3'")
	if out, err := outputCmd.CombinedOutput(); err != nil {
		t.Fatalf("second pane: %v: %s", err, out)
	}
	owner.Action = "terminal_write"
	owner.Content = "printf '\\033[?1049hALT_SCREEN_PROOF\\033[?1049l\\n'\n"
	if r := s.terminalAction(owner); !r.OK {
		t.Fatalf("terminal write: %+v", r)
	}
	reader := owner
	reader.Action = "terminal_read"
	deadline := time.Now().Add(4 * time.Second)
	var side, primary, alt bool
	var cursor uint64
	paneIDs := map[string]bool{}
	for time.Now().Before(deadline) {
		reader.Cursor = cursor
		result := s.terminalAction(reader)
		if !result.OK {
			t.Fatalf("multi-pane read: %+v", result)
		}
		cursor = result.NextCursor
		for _, evt := range result.TerminalEvents {
			paneIDs[evt.PaneID] = true
			bytes, err := base64.StdEncoding.DecodeString(evt.Data)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(bytes), "SIDE_PANE_PROOF") {
				side = true
			}
			if strings.Contains(string(bytes), "ALT_SCREEN_PROOF") {
				primary = true
			}
			if bytesContainTerminalAltScreen(bytes) {
				alt = true
			}
		}
		if side && primary && alt && len(paneIDs) > 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("multi-pane/alternate-screen evidence incomplete: side=%t primary=%t alt=%t panes=%v", side, primary, alt, paneIDs)
}

func bytesContainTerminalAltScreen(data []byte) bool {
	return bytes.Contains(data, []byte("\x1b[?1049h")) || bytes.Contains(data, []byte("\x1b[?1049l"))
}

func TestWorkerTmuxCloseKillsAllPrivateServerSessions(t *testing.T) {
	bin := os.Getenv("ASA_TEST_TMUX_BIN")
	if bin == "" {
		t.Skip("optional isolated tmux fixture not supplied")
	}
	s, owner, _ := testStdioBroker(t)
	s.terminalBinary = bin
	owner.Action = "terminal_open"
	owner.Columns = 80
	owner.Rows = 24
	opened := s.terminalAction(owner)
	if !opened.OK {
		t.Fatalf("open: %+v", opened)
	}
	owner.SessionID = opened.SessionID
	owner.SessionEpoch = opened.SessionEpoch
	entry, _ := s.sessions.get(owner, opened.SessionID)
	socket := entry.terminal.socket
	extra := exec.Command(bin, "-S", socket, "new-session", "-d", "-s", "extra")
	if out, err := extra.CombinedOutput(); err != nil {
		t.Fatalf("add detached session: %v %s", err, out)
	}
	// Even if exit-empty has been disabled, no detached root/worker server may
	// outlive a successful terminal_close. The entire private server must die.
	set := exec.Command(bin, "-S", socket, "set-option", "-g", "exit-empty", "off")
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("exit-empty fixture: %v %s", err, out)
	}
	owner.Action = "terminal_close"
	result := s.terminalAction(owner)
	if !result.OK {
		t.Fatalf("close private server: %+v", result)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("private server socket survived successful close: %v", err)
	}
	// Only original Agent session may have controlled the namespace; no
	// orphan "extra" session may be silently retained outside it.
	if out, err := exec.Command(bin, "-S", socket, "list-sessions").CombinedOutput(); err == nil {
		t.Fatalf("orphan private session still running: %s", out)
	}
}

func TestWorkerTerminalUnixSocketPathFailsBeforeSpawn(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	s.terminalBinary = "/usr/bin/tmux" // Only the isolated non-root fixture uses worker-owned sockets.
	root := filepath.Join(s.cfg.WorkspaceDir, strings.Repeat("long-namespace-", 8))
	workspace := filepath.Join(root, "worktree")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	s.cfg.WorkspaceDir = root
	owner.Workspace = workspace
	owner.Action = "terminal_open"
	owner.Columns = 80
	owner.Rows = 24
	result := s.terminalAction(owner)
	if result.OK || result.ErrorCode != "terminal_socket_path_too_long" || result.SessionID != "" {
		t.Fatalf("unusable long socket path should fail before broker spawn: %+v", result)
	}
	s.sessions.mu.Lock()
	n := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if n != 0 {
		t.Fatalf("unstarted long-socket session leaked into broker: %d", n)
	}
}
