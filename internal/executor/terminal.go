package executor

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/ach1992/ai-server-agent/internal/systemexec"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Terminal is a tmux Control Mode consumer of the executor-owned session
// broker, not a second process supervisor. All tmux commands travel over the
// same bounded authenticated executor/stdio path used by other consumers.
const (
	terminalMaxInput          = 4096
	terminalMaxOutput         = 32 << 10
	terminalMaxLine           = 128 << 10
	terminalCommandTimeout    = 3 * time.Second
	terminalMaxUnixSocketPath = 107 // Linux sockaddr_un.sun_path includes trailing NUL
)

type terminalChunk struct {
	seq  uint64
	pane string
	data []byte
}

type TerminalOutputEvent struct {
	Cursor uint64 `json:"cursor"`
	PaneID string `json:"pane_id"`
	Data   string `json:"data_base64"`
}

type tmuxTerminalState struct {
	root          bool
	epoch         string
	recovered     bool
	inReply       bool
	replyLines    []string
	lastReply     string
	actionMu      sync.Mutex // serializes control commands and their acknowledgements
	socketDir     string
	socket        string
	name          string
	pane          string
	columns       int
	rows          int
	pending       []byte
	chunks        []terminalChunk
	retained      int
	latest        uint64
	ack           uint64
	ackFailed     bool
	exited        bool
	protocolError string
}

// accept is called with the owning stdioSession.mu held, ensuring parsed output
// stays ordered even when stdout arrives in many arbitrarily split writes.
func (t *tmuxTerminalState) accept(p []byte) {
	if t.protocolError != "" {
		return
	}
	if len(t.pending)+len(p) > terminalMaxLine && !strings.Contains(string(p), "\n") {
		t.protocolError = "terminal_control_frame_too_large"
		return
	}
	t.pending = append(t.pending, p...)
	for {
		ix := -1
		for i, b := range t.pending {
			if b == '\n' {
				ix = i
				break
			}
		}
		if ix < 0 {
			break
		}
		if ix > terminalMaxLine {
			t.protocolError = "terminal_control_frame_too_large"
			return
		}
		line := string(t.pending[:ix])
		t.pending = t.pending[ix+1:]
		switch {
		case strings.HasPrefix(line, "%output "):
			fields := strings.SplitN(line, " ", 3)
			if len(fields) != 3 || !strings.HasPrefix(fields[1], "%") {
				t.protocolError = "terminal_invalid_output"
				return
			}
			if t.pane == "" {
				t.pane = fields[1]
			}
			// Each tmux pane has a stable %id, and additional panes are
			// legitimate. Preserve pane identity for the caller instead of
			// merging output silently or treating a split as a fatal error.
			if !validTmuxPaneID(fields[1]) {
				t.protocolError = "terminal_invalid_pane_id"
				return
			}
			decoded, err := decodeTmuxOutput(fields[2])
			if err != nil {
				t.protocolError = "terminal_invalid_output_encoding"
				return
			}
			for len(decoded) > 0 {
				n := len(decoded)
				if n > maxStdioEventBytes {
					n = maxStdioEventBytes
				}
				t.latest++
				t.chunks = append(t.chunks, terminalChunk{seq: t.latest, pane: fields[1], data: append([]byte(nil), decoded[:n]...)})
				t.retained += n
				decoded = decoded[n:]
				for t.retained > maxStdioOutputBytes || len(t.chunks) > maxStdioOutputEvents {
					t.retained -= len(t.chunks[0].data)
					t.chunks[0] = terminalChunk{}
					t.chunks = t.chunks[1:]
				}
			}
		case strings.HasPrefix(line, "%begin "):
			t.inReply = true
			t.replyLines = nil
		case strings.HasPrefix(line, "%end "):
			t.lastReply = strings.Join(t.replyLines, "\n")
			t.inReply = false
			t.ack++
			t.ackFailed = false
		case strings.HasPrefix(line, "%error "):
			t.inReply = false
			t.lastReply = ""
			t.ack++
			t.ackFailed = true
		case line == "%exit" || strings.HasPrefix(line, "%exit "):
			t.exited = true
		default:
			if t.inReply && len(line) <= 4096 && len(t.replyLines) < 8 {
				t.replyLines = append(t.replyLines, line)
			}
		}
	}
	if len(t.pending) > terminalMaxLine {
		t.protocolError = "terminal_control_frame_too_large"
	}
}

func decodeTmuxOutput(raw string) ([]byte, error) {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			out = append(out, raw[i])
			continue
		}
		if i+3 >= len(raw) {
			return nil, errors.New("incomplete escape")
		}
		// tmux uses exactly three octal digits for escaped bytes.
		n, err := strconv.ParseUint(raw[i+1:i+4], 8, 8)
		if err != nil {
			return nil, err
		}
		out = append(out, byte(n))
		i += 3
	}
	return out, nil
}

func validTmuxPaneID(id string) bool {
	if len(id) < 2 || len(id) > 16 || id[0] != '%' {
		return false
	}
	_, err := strconv.ParseUint(id[1:], 10, 64)
	return err == nil
}

func (s *Server) terminalAction(req Request) Response {
	if req.PrincipalID == "" || req.PrincipalClass == "" {
		return terminalError("unauthorized_terminal", errors.New("authenticated principal required"))
	}
	if req.Root && !req.Approval {
		return Response{Error: "approval_required", ReasonCode: "approval_required", ErrorCode: "approval_required", ErrorClass: "approval"}
	}
	if !req.Root && req.Approval {
		return terminalError("invalid_authority", errors.New("worker terminal cannot use root approval"))
	}
	switch req.Action {
	case "terminal_open":
		return s.terminalOpen(req)
	case "terminal_reconnect":
		return s.terminalReconnect(req)
	case "terminal_read":
		return s.terminalRead(req)
	case "terminal_write", "terminal_interrupt", "terminal_resize", "terminal_close":
		return s.terminalControl(req)
	default:
		return terminalError("invalid_action", errors.New("unsupported terminal operation"))
	}
}

func terminalError(code string, err error) Response {
	return Response{Error: err.Error(), ReasonCode: code, ErrorCode: code, ErrorClass: "terminal"}
}

func (s *Server) terminalOpen(req Request) Response {
	if req.Workspace == "" {
		return terminalError("invalid_workspace", errors.New("explicit workspace is required"))
	}
	if req.Columns < 20 || req.Columns > 240 || req.Rows < 5 || req.Rows > 80 {
		return terminalError("invalid_dimensions", errors.New("terminal dimensions must be 20..240 columns and 5..80 rows"))
	}
	cwd, err := s.workspacePath(req.Workspace, true)
	if err != nil {
		return terminalError("invalid_workspace", err)
	}
	if cwd != filepath.Clean(req.Workspace) {
		return terminalError("invalid_workspace", errors.New("workspace identity mismatch"))
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return terminalError("session_random_unavailable", err)
	}
	name := "asa_" + hex.EncodeToString(nonce[:])
	// Worker shells share one UID across authenticated principals. A socket
	// owned by aiworker is therefore NOT a principal boundary: arbitrary
	// worker code could attach to another principal's tmux server. Production
	// runs tmux Control Mode as root in an executor-only state namespace and
	// explicitly drops the pane process to aiworker. The old worker-owned
	// backend is permissible only for the in-process non-root test fixture.
	fixtureWorkerBackend := !req.Root && s.terminalBinary != ""
	runtimeRoot := s.cfg.WorkspaceDir
	if !fixtureWorkerBackend {
		if os.Geteuid() != 0 {
			if req.Root {
				return terminalError("root_executor_unavailable", errors.New("root executor is required"))
			}
			return terminalError("terminal_executor_unavailable", errors.New("private tmux control requires root executor"))
		}
		if err := trustedDir(s.cfg.StateDir); err != nil {
			return terminalError("terminal_state_untrusted", err)
		}
		folder := "worker-terminals"
		if req.Root {
			folder = "root-terminals"
		}
		runtimeRoot = filepath.Join(s.cfg.StateDir, folder)
		if err := os.MkdirAll(runtimeRoot, 0700); err != nil {
			return terminalError("terminal_runtime_unavailable", err)
		}
		if err := trustedDir(runtimeRoot); err != nil {
			return terminalError("terminal_runtime_untrusted", err)
		}
	}
	dir, err := os.MkdirTemp(runtimeRoot, ".asa-tmux-")
	if err != nil {
		return terminalError("terminal_runtime_unavailable", err)
	}
	if err := os.Chmod(dir, 0700); err == nil && fixtureWorkerBackend && os.Geteuid() == 0 {
		err = os.Chown(dir, int(s.workerUID), int(s.workerGID))
	}
	if err != nil {
		_ = os.Remove(dir)
		return terminalError("terminal_runtime_unavailable", err)
	}
	socket := filepath.Join(dir, "socket")
	if len(socket) > terminalMaxUnixSocketPath {
		_ = os.Remove(dir)
		return terminalError("terminal_socket_path_too_long", fmt.Errorf("private tmux socket path exceeds Linux %d-byte bound", terminalMaxUnixSocketPath))
	}
	epoch, err := terminalEpoch()
	if err != nil {
		_ = os.Remove(dir)
		return terminalError("terminal_epoch_unavailable", err)
	}
	t := &tmuxTerminalState{root: req.Root, socketDir: dir, socket: socket, name: name, columns: req.Columns, rows: req.Rows, epoch: epoch}
	binary, err := s.resolveTmuxBinary()
	if err != nil {
		_ = os.Remove(dir)
		return terminalError("tmux_untrusted", err)
	}
	args := []string{"-f", "/dev/null", "-S", socket, "-C", "new-session", "-s", name,
		"-c", cwd, "-x", strconv.Itoa(req.Columns), "-y", strconv.Itoa(req.Rows)}
	if !req.Root && !fixtureWorkerBackend {
		// tmux accepts a command+arguments vector after new-session options:
		// no shell interpolation, worker-controlled config, or root pane shell.
		setpriv := systemexec.First("/usr/bin/setpriv")
		env := systemexec.First("/usr/bin/env")
		bash := systemexec.First("/usr/bin/bash")
		if setpriv == "" || env == "" || bash == "" {
			_ = os.Remove(dir)
			return terminalError("worker_shell_unavailable", errors.New("trusted setpriv/env/bash required for isolated worker pane"))
		}
		home, homeErr := s.workerSessionHome()
		if homeErr != nil {
			_ = os.Remove(dir)
			return terminalError("worker_home_unavailable", homeErr)
		}
		// The tmux server keeps root-only socket authority, while this
		// executable drops UID/GID before starting the interactive pane.
		args = append(args, setpriv,
			"--reuid="+strconv.FormatUint(uint64(s.workerUID), 10),
			"--regid="+strconv.FormatUint(uint64(s.workerGID), 10),
			"--clear-groups", env, "-i",
			"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
			"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
			"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
			"PATH="+safeCommandPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
			"TERM=screen-256color", "USER="+s.cfg.WorkerUser, "LOGNAME="+s.cfg.WorkerUser,
			"AI_SERVER_AGENT=1", "SHELL="+bash, bash, "--noprofile", "--norc", "-i")
	}
	var id string
	if req.Root {
		id, err = s.rootTmuxProcessSession(req, cwd, binary, t, args...)
	} else {
		id, err = s.workerProcessSession(req, "terminal", cwd, binary, t, args...)
	}
	if err != nil {
		// A process can be running even if post-start audit completion fails.
		// Preserve its ID and socket namespace; never claim a clean failure.
		if id != "" {
			return Response{SessionID: id, SessionEpoch: epoch, Error: "terminal creation outcome requires reconciliation: " + err.Error(), ReasonCode: "terminal_start_uncertain", ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
		}
		_ = os.Remove(dir)
		return terminalError("terminal_start_failed", err)
	}
	e, err := s.sessions.get(req, id)
	if err != nil {
		return Response{SessionID: id, SessionEpoch: epoch, Error: "terminal session startup outcome uncertain: " + err.Error(), ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
	}
	deadline := time.Now().Add(terminalCommandTimeout)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		ready := t.ack > 0 && t.pane != "" && t.protocolError == "" && !e.exited
		failure := t.protocolError
		exited := e.exited
		e.mu.Unlock()
		if ready {
			if err := s.terminalCommand(req, e, "set-option -t "+t.name+" @asa_generation "+t.name); err != nil {
				return Response{SessionID: id, SessionEpoch: epoch, Error: "tmux generation setup uncertain: " + err.Error(), ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
			}
			count, err := s.terminalRecordCount()
			if err != nil || count >= maxStdioSessions {
				return Response{SessionID: id, SessionEpoch: epoch, Error: "terminal registry unavailable or full", ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
			}
			if err = s.persistTerminalRecord(req, e); err != nil {
				return Response{SessionID: id, SessionEpoch: epoch, Error: "terminal registry persistence unverified: " + err.Error(), ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
			}
			return Response{OK: true, Status: "running", SessionID: id, SessionEpoch: epoch, Running: boolPtr(true), Columns: t.columns, Rows: t.rows}
		}
		if failure != "" || exited {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// We cannot claim a usable terminal until its control stream identifies
	// the original pane. Keep its ID on uncertain launch for reconciliation.
	return Response{Error: "terminal started but initial pane/ack not verified", ReasonCode: "terminal_start_uncertain", ErrorCode: "terminal_start_uncertain", ErrorClass: "state", SessionID: id, SessionEpoch: epoch}
}

// The production binary trust rule must be identical for initial open and
// recovered sessions: root reconnect must never bypass the trusted path.
func (s *Server) resolveTmuxBinary() (string, error) {
	if s.terminalBinary != "" {
		return s.terminalBinary, nil
	} // internal fixture override
	if binary := systemexec.First("/usr/bin/tmux"); binary != "" {
		return binary, nil
	}
	return "", errors.New("optional tmux must be root-owned, executable, non-symlink and protected from non-root writes")
}

func (s *Server) terminalRead(req Request) Response {
	e, err := s.sessions.get(req, req.SessionID)
	if err != nil {
		return terminalError("session_not_found", err)
	}
	if e.kind != "terminal" || e.terminal == nil {
		return terminalError("session_not_found", errSessionNotFound)
	}
	limit := req.Limit
	if limit == 0 {
		limit = 8192
	}
	if limit < maxStdioEventBytes || limit > terminalMaxOutput {
		return terminalError("invalid_limit", errors.New("terminal read limit must be 4096..32768 bytes"))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.terminal
	if t.protocolError != "" {
		return terminalError(t.protocolError, errors.New("tmux control protocol became unusable"))
	}
	if req.SessionEpoch != t.epoch {
		return Response{Error: "terminal output epoch changed; resume from cursor zero with returned session_epoch", ReasonCode: "terminal_epoch_changed", ErrorCode: "terminal_epoch_changed", ErrorClass: "state", SessionID: e.id, SessionEpoch: t.epoch, RetentionTruncated: t.recovered}
	}
	if req.Cursor > t.latest {
		return terminalError("invalid_cursor", fmt.Errorf("future cursor; latest=%d", t.latest))
	}
	earliest := t.latest + 1
	if len(t.chunks) > 0 {
		earliest = t.chunks[0].seq
	}
	result := Response{OK: true, Status: "running", SessionID: e.id, SessionEpoch: t.epoch, Earliest: earliest, Latest: t.latest, NextCursor: req.Cursor, Running: boolPtr(!e.exited && !t.exited && !e.closed), Rows: t.rows, Columns: t.columns, OutputEncoding: "base64"}
	result.Truncated = req.Cursor < earliest-1
	output := make([]byte, 0, limit)
	for _, chunk := range t.chunks {
		if chunk.seq <= req.Cursor {
			continue
		}
		if len(chunk.data)+len(output) > limit {
			break
		}
		output = append(output, chunk.data...)
		result.TerminalEvents = append(result.TerminalEvents, TerminalOutputEvent{
			Cursor: chunk.seq, PaneID: chunk.pane, Data: base64.StdEncoding.EncodeToString(chunk.data),
		})
		result.NextCursor = chunk.seq
	}
	result.Output = base64.StdEncoding.EncodeToString(output)
	result.BytesReturned = int64(len(output))
	result.BytesSeen = int64(len(output))
	result.RetentionTruncated = result.Truncated || t.recovered
	if !*result.Running {
		result.Status = "disconnected"
	}
	return result
}

func (s *Server) terminalControl(req Request) Response {
	e, err := s.sessions.get(req, req.SessionID)
	if err != nil || e.kind != "terminal" || e.terminal == nil {
		return terminalError("session_not_found", errSessionNotFound)
	}
	t := e.terminal
	if req.SessionEpoch != t.epoch {
		return Response{Error: "terminal control epoch changed; inspect terminal state before retry", ErrorCode: "terminal_epoch_changed", ErrorClass: "state", SessionID: e.id, SessionEpoch: t.epoch}
	}
	var command string
	switch req.Action {
	case "terminal_write":
		data := []byte(req.Content)
		if len(data) == 0 || len(data) > terminalMaxInput {
			return terminalError("invalid_input", errors.New("terminal input must be 1..4096 bytes"))
		}
		// Hex keys prevent untrusted terminal input from ever becoming tmux
		// command-language syntax, even for quotes/newlines/control bytes.
		var b strings.Builder
		b.WriteString("send-keys -t ")
		b.WriteString(t.name)
		b.WriteString(":0.0 -H")
		for _, v := range data {
			fmt.Fprintf(&b, " %02x", v)
		}
		command = b.String()
	case "terminal_interrupt":
		command = "send-keys -t " + t.name + ":0.0 C-c"
	case "terminal_resize":
		if req.Columns < 20 || req.Columns > 240 || req.Rows < 5 || req.Rows > 80 {
			return terminalError("invalid_dimensions", errors.New("invalid resize dimensions"))
		}
		command = fmt.Sprintf("resize-window -t %s -x %d -y %d", t.name, req.Columns, req.Rows)
	case "terminal_close":
		// This Agent owns a dedicated tmux server per terminal socket. Kill
		// every server-side session, including extra sessions a shell may
		// have created, instead of leaving privileged detached children.
		command = "kill-server"
	}
	if blocked := s.beginActionAudit(req, req.Action, auditMode(req.Root), e.id, "developer_terminal"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	err = s.terminalCommand(req, e, command)
	if err == nil && req.Action == "terminal_resize" {
		e.mu.Lock()
		t.columns = req.Columns
		t.rows = req.Rows
		e.mu.Unlock()
	}
	if err == nil && req.Action == "terminal_close" {
		err = s.sessions.removeAndStop(e)
		if err == nil {
			err = s.deleteTerminalRecord(e.id)
		}
		if err == nil {
			_ = os.Remove(t.socket)
			_ = os.Remove(t.socketDir) // leave unexpected contents untouched
		}
	}
	result := Response{OK: err == nil, SessionID: e.id}
	if err != nil {
		result.Error = err.Error()
		result.ErrorCode = "terminal_control_unknown"
		result.ErrorClass = "state"
	}
	return s.finishActionAudit(req, req.Action, auditMode(req.Root), e.id, "developer_terminal", started, result)
}

func (s *Server) terminalCommand(req Request, e *stdioSession, command string) error {
	t := e.terminal
	t.actionMu.Lock()
	defer t.actionMu.Unlock()
	e.mu.Lock()
	if e.exited || e.closed || t.exited || t.protocolError != "" {
		e.mu.Unlock()
		return errSessionStopped
	}
	before := t.ack
	e.mu.Unlock()
	if len(command)+1 > maxStdioInputBytes {
		return errors.New("terminal_command_too_large")
	}
	if err := s.sessions.write(req, e.id, []byte(command+"\n")); err != nil {
		return err
	}
	deadline := time.Now().Add(terminalCommandTimeout)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		completed := t.ack > before
		failed := t.ackFailed
		stopped := e.exited || t.protocolError != ""
		e.mu.Unlock()
		if completed {
			if failed {
				return errors.New("tmux rejected terminal command")
			}
			return nil
		}
		if stopped {
			return errors.New("terminal_control_disconnected_before_ack")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("terminal_control_ack_unknown: inspect state before retry")
}

// The existing broker timer owns expiry; terminal expiry must terminate the
// actual tmux session, not merely its Control Mode client process.
func (s *Server) expireProcessSession(entry *stdioSession) {
	if entry.dap != nil {
		req := Request{Action: "debug_stop", PrincipalID: entry.ownerID, PrincipalClass: entry.ownerClass, Workspace: entry.workspace, SessionID: entry.id}
		_ = ensureRequestCorrelation(&req)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = s.debugStop(ctx, req)
		return
	}
	if entry.terminal == nil {
		_ = s.sessions.removeAndStop(entry)
		return
	}
	req := Request{Action: "terminal_close", PrincipalID: entry.ownerID, PrincipalClass: entry.ownerClass,
		Workspace: entry.workspace, SessionID: entry.id, Root: entry.terminal.root, Approval: entry.terminal.root, SessionEpoch: entry.terminal.epoch}
	_ = ensureRequestCorrelation(&req)
	result := s.terminalControl(req)
	if !result.OK {
		// Keep the identity visible. An unverified detached backend is not a
		// successful stop and must not be silently deleted on retention GC.
		entry.mu.Lock()
		entry.cleanupErr = errors.New("tmux backend stop not verified")
		entry.mu.Unlock()
	}
}
