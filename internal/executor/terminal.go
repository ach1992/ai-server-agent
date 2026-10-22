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
	terminalInputChunk        = 4096     // each hex-encoded tmux command fits the 16 KiB broker frame
	terminalMaxInput          = 64 << 10 // 16 ordered chunks per MCP call
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
	root           bool
	epoch          string
	recovered      bool
	scoped         bool // production backend lives in its own bounded systemd scope
	recordVersion  int  // 1: legacy hard TTL; 2: separate renewable PID1 timer
	scopeUncertain bool // PID1 may own a backend but exact-stop verification failed
	inReply        bool
	replyLines     []string
	lastReply      string
	actionMu       sync.Mutex // serializes control commands and their acknowledgements
	socketDir      string
	socket         string
	name           string
	pane           string
	columns        int
	rows           int
	pending        []byte
	chunks         []terminalChunk
	retained       int
	latest         uint64
	ack            uint64
	ackFailed      bool
	readWaiter     bool // at most one bounded sleeping reader, never blocks writes
	exited         bool
	protocolError  string
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
	return s.withTerminalAdmission(func() Response {
		return s.terminalOpenAdmitted(req)
	})
}

// Holding one serialized admission through successful record persistence or
// verified cleanup is the reservation. Unknown failures stay counted via the
// broker's unpersisted terminal identities until exact cleanup is proven.
func (s *Server) withTerminalAdmission(open func() Response) Response {
	return s.withTerminalAdmissionStop(open, stopScopedTerminalBackend)
}

// Injected stop is used ONLY by pure deterministic admission tests: installed
// Executor always uses the verified PID1 scope stopper.
func (s *Server) withTerminalAdmissionStop(open func() Response, stop func(string) error) Response {
	s.terminalAdmissionMu.Lock()
	defer s.terminalAdmissionMu.Unlock()
	registry, admissionErr := s.terminalReservationsWithStop(stop)
	if admissionErr != nil {
		return terminalError("terminal_registry_unavailable", admissionErr)
	}
	if s.sessions == nil || s.sessions.occupiedAgainstRegistry(registry) >= s.stdioSessionCapacity() {
		return terminalError("terminal_resource_limit", errSessionBusy)
	}
	return open()
}

func (s *Server) terminalOpenAdmitted(req Request) Response {
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
		bash := systemexec.First("/usr/bin/bash", "/bin/bash")
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
		// Until ACTIVE is fsynced, allow ONLY explicit scope reconciliation.
		if id != "" {
			if e, getErr := s.sessions.get(req, id); getErr == nil {
				e.mu.Lock()
				t.scopeUncertain = true
				e.cleanupErr = err
				e.mu.Unlock()
			}
			return Response{SessionID: id, SessionEpoch: epoch, Error: "terminal creation outcome requires reconciliation: " + err.Error(), ReasonCode: "terminal_start_uncertain", ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
		}
		// A verified-clean production scope may still leave its Unix socket
		// filename behind. Remove only a trusted private socket directory;
		// never follow links or recursively delete an unknown path.
		if !fixtureWorkerBackend {
			removeVerifiedPendingSocketDir(dir)
		}
		_ = os.Remove(dir)
		return terminalError("terminal_start_failed", err)
	}
	e, err := s.sessions.get(req, id)
	if err != nil {
		return Response{SessionID: id, SessionEpoch: epoch, Error: "terminal session startup outcome uncertain: " + err.Error(), ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
	}
	// All paths returning before ACTIVE publication must fail closed.
	pendingFailure := func(message string, cause error) Response {
		e.mu.Lock()
		t.scopeUncertain = true
		e.cleanupErr = cause
		e.mu.Unlock()
		return Response{SessionID: id, SessionEpoch: epoch, Error: message + ": " + cause.Error(),
			ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
	}
	deadline := time.Now().Add(terminalCommandTimeout)
	panePrimed := !t.scoped
	for time.Now().Before(deadline) {
		// Attaching to an already-detached tmux server may have no new pane
		// output yet. After its initial Control Mode acknowledgement, obtain
		// the original pane identity through an explicit structured reply
		// instead of requiring a prompt or a magic keystroke.
		if !panePrimed {
			e.mu.Lock()
			canQuery := t.ack > 0 && t.protocolError == "" && !e.exited
			e.mu.Unlock()
			if canQuery {
				panePrimed = true
				paneErr := s.terminalCommand(req, e, "list-panes -t "+t.name+" -F '#{pane_id}'")
				e.mu.Lock()
				pane := strings.TrimSpace(t.lastReply)
				if paneErr != nil || !validTmuxPaneID(pane) || (t.pane != "" && t.pane != pane) {
					t.protocolError = "terminal_pane_identity_unverified"
				} else {
					t.pane = pane
				}
				e.mu.Unlock()
			}
		}
		e.mu.Lock()
		ready := t.ack > 0 && t.pane != "" && t.protocolError == "" && !e.exited
		failure := t.protocolError
		exited := e.exited
		e.mu.Unlock()
		if ready {
			if err := s.terminalCommand(req, e, "set-option -t "+t.name+" @asa_generation "+t.name); err != nil {
				return pendingFailure("tmux generation setup uncertain", err)
			}
			if s.terminalActivePublishTestHook != nil {
				if hookErr := s.terminalActivePublishTestHook(t); hookErr != nil {
					return pendingFailure("terminal ACTIVE publication interrupted", hookErr)
				}
			}
			if err = s.persistTerminalRecord(req, e); err != nil {
				return pendingFailure("terminal registry persistence unverified", err)
			}
			e.mu.Lock()
			e.terminalRecordPersisted = true
			e.mu.Unlock()
			return Response{OK: true, Status: "running", SessionID: id, SessionEpoch: epoch, Running: boolPtr(true), Columns: t.columns, Rows: t.rows}
		}
		if failure != "" || exited {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// We cannot claim a usable terminal until its control stream identifies
	// the original pane. Keep its ID on uncertain launch for reconciliation.
	return pendingFailure("terminal started but initial pane/ack not verified", errors.New("initial pane identity missing"))
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
		if pending, ok := s.pendingTerminalAction(req); ok {
			return pending
		}
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
	// Zero preserves legacy immediate reads, including against old Agents.
	// Limit the optional wait to a finite single-call budget: this is NOT a
	// durable subscription, unbounded queue, or long-lived broker lease.
	if req.WaitMS < 0 || req.WaitMS > 2000 {
		return terminalError("invalid_wait", errors.New("terminal read wait_ms must be 0..2000 milliseconds"))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.terminal
	if t.scopeUncertain {
		return Response{SessionID: e.id, SessionEpoch: t.epoch, Error: "terminal lifecycle remains pending; explicit close required", ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
	}
	if t.protocolError != "" {
		return terminalError(t.protocolError, errors.New("tmux control protocol became unusable"))
	}
	if req.SessionEpoch != t.epoch {
		return Response{Error: "terminal output epoch changed; resume from cursor zero with returned session_epoch", ReasonCode: "terminal_epoch_changed", ErrorCode: "terminal_epoch_changed", ErrorClass: "state", SessionID: e.id, SessionEpoch: t.epoch, RetentionTruncated: t.recovered}
	}
	// An invalid/future cursor is not successful session activity; reject
	// before a potentially expensive root systemd lease verification/renewal.
	if req.Cursor > t.latest {
		return terminalError("invalid_cursor", fmt.Errorf("future cursor; latest=%d", t.latest))
	}
	e.mu.Unlock()
	renewalErr := s.renewTerminalLease(req, e)
	e.mu.Lock() // defer above must remain paired even when renewal fails
	if renewalErr != nil {
		return Response{SessionID: e.id, SessionEpoch: t.epoch, Error: "Terminal idle lease renewal unverified: " + renewalErr.Error(), ErrorCode: "terminal_lease_unverified", ErrorClass: "state"}
	}
	if t.scopeUncertain {
		return Response{SessionID: e.id, SessionEpoch: t.epoch, Error: "scope creation remains unverified; explicit close/reconciliation required", ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
	}
	// A terminal can emit a prompt or a REPL result shortly after the
	// consumer reads its cursor. An OPTIONAL finite wait avoids busy polling,
	// but it must never hold e.mu or t.actionMu while waiting: the writer,
	// parser, and close/reconnect lifecycle must continue independently.
	//
	// Use a single waiter per session to bound outstanding idle work. Unlike
	// a session-wide consumption cursor, each caller owns its own cursor, so
	// an ordinary read remains available while another client is waiting.
	if req.WaitMS > 0 && req.Cursor == t.latest && !t.recovered &&
		!e.exited && !e.closed && !t.exited && t.protocolError == "" {
		if t.readWaiter {
			return Response{SessionID: e.id, SessionEpoch: t.epoch,
				Error:      "one bounded terminal output waiter already active",
				ReasonCode: "terminal_read_busy", ErrorCode: "terminal_read_busy",
				ErrorClass: "resource", Status: "busy", Retryable: true}
		}
		t.readWaiter = true
		defer func() { t.readWaiter = false }() // e.mu locked before all returns
		deadline := time.Now().Add(time.Duration(req.WaitMS) * time.Millisecond)
		for req.Cursor == t.latest && !e.exited && !e.closed && !t.exited &&
			!t.scopeUncertain && t.protocolError == "" && t.epoch == req.SessionEpoch {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			if remaining > 25*time.Millisecond {
				remaining = 25 * time.Millisecond
			}
			e.mu.Unlock()
			time.Sleep(remaining)
			e.mu.Lock()
		}
	}
	// Concurrent producer errors, lifecycle changes, and reconnects during
	// the unlocked interval must never be turned into successful output.
	if t.scopeUncertain {
		return Response{SessionID: e.id, SessionEpoch: t.epoch, Error: "terminal lifecycle remains pending; explicit close required", ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
	}
	if t.protocolError != "" {
		return terminalError(t.protocolError, errors.New("tmux control protocol became unusable"))
	}
	if req.SessionEpoch != t.epoch {
		return Response{Error: "terminal output epoch changed; resume from cursor zero with returned session_epoch",
			ReasonCode: "terminal_epoch_changed", ErrorCode: "terminal_epoch_changed",
			ErrorClass: "state", SessionID: e.id, SessionEpoch: t.epoch, RetentionTruncated: t.recovered}
	}
	earliest := t.latest + 1
	if len(t.chunks) > 0 {
		earliest = t.chunks[0].seq
	}
	result := Response{OK: true, Status: "running", SessionID: e.id, SessionEpoch: t.epoch, Earliest: earliest, Latest: t.latest, NextCursor: req.Cursor, Running: boolPtr(!e.exited && !t.exited && !e.closed), Rows: t.rows, Columns: t.columns, OutputEncoding: "base64", ReadWaitSupported: req.WaitMS > 0}
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

// An unknown startup may leave a privileged backend but no Control Mode
// process. Only exact-scope verified shutdown can release its owner identity.
func (s *Server) reconcileUnstartedTerminal(e *stdioSession, stop func(string) error) error {
	e.stopMu.Lock()
	e.mu.Lock()
	if e.terminal == nil || !e.terminal.scopeUncertain {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return errors.New("terminal_scope_reconciliation_state_invalid")
	}
	name := e.terminal.name
	hasClient := e.cmd != nil
	if !hasClient && (e.pid != 0 || e.pidPin != nil) {
		e.mu.Unlock()
		e.stopMu.Unlock()
		return errors.New("terminal_scope_reconciliation_state_invalid")
	}
	e.mu.Unlock()
	if err := stop(name); err != nil {
		e.stopMu.Unlock()
		return fmt.Errorf("terminal_scope_cleanup_unverified: %w", err)
	}
	e.stopMu.Unlock()
	if hasClient {
		if err := s.sessions.removeAndStop(e); err != nil {
			return fmt.Errorf("terminal_control_client_cleanup_unverified: %w", err)
		}
	} else {
		e.mu.Lock()
		e.closed = true
		e.cleanupErr = nil
		if e.stdin != nil {
			_ = e.stdin.Close()
		}
		e.completed = time.Now()
		e.mu.Unlock()
	}
	// Scope is verified stopped and client safely retired. Only now may a
	// durable reservation be deleted and the capacity returned to admission.
	if err := s.deleteTerminalRecord(e.id); err != nil {
		return fmt.Errorf("terminal_pending_deletion_unverified: %w", err)
	}
	s.sessions.remove(e)
	return nil
}

func (s *Server) terminalCloseUnstartedScope(req Request, e *stdioSession) Response {
	s.terminalAdmissionMu.Lock()
	defer s.terminalAdmissionMu.Unlock()
	if blocked := s.beginActionAudit(req, req.Action, auditMode(req.Root), e.id, "developer_terminal"); blocked != nil {
		return *blocked
	}
	started := time.Now()
	stop := stopScopedTerminalBackend
	if s.terminalScopeStopTestHook != nil {
		stop = s.terminalScopeStopTestHook
	}
	err := s.reconcileUnstartedTerminal(e, stop)
	if err == nil {
		if s.terminalBinary == "" {
			removeVerifiedPendingSocketDir(e.terminal.socketDir)
		} else {
			_ = os.Remove(e.terminal.socket)
			_ = os.Remove(e.terminal.socketDir) // fixture only
		}
	}
	result := Response{OK: err == nil, SessionID: e.id, SessionEpoch: e.terminal.epoch}
	if err != nil {
		result.Error = err.Error()
		result.ErrorCode = "terminal_control_unknown"
		result.ErrorClass = "state"
	}
	return s.finishActionAudit(req, req.Action, auditMode(req.Root), e.id, "developer_terminal", started, result)
}

func (s *Server) terminalControl(req Request) Response {
	e, err := s.sessions.get(req, req.SessionID)
	if err != nil {
		if pending, ok := s.pendingTerminalAction(req); ok {
			return pending
		}
		if req.Action == "terminal_close" {
			return s.closeDetachedActiveTerminal(req)
		}
		return terminalError("session_not_found", errSessionNotFound)
	}
	if e.kind != "terminal" || e.terminal == nil {
		return terminalError("session_not_found", errSessionNotFound)
	}
	t := e.terminal
	if req.SessionEpoch != t.epoch {
		return Response{Error: "terminal control epoch changed; inspect terminal state before retry", ErrorCode: "terminal_epoch_changed", ErrorClass: "state", SessionID: e.id, SessionEpoch: t.epoch}
	}
	if t.scopeUncertain {
		if req.Action != "terminal_close" {
			return Response{SessionID: e.id, SessionEpoch: t.epoch, Error: "scope creation is unverified; explicitly close/reconcile before other terminal operations", ErrorCode: "terminal_start_uncertain", ErrorClass: "state"}
		}
		return s.terminalCloseUnstartedScope(req, e)
	}
	// Closing a durable scope is serialized with authenticated lease renewal,
	// expiry and registry GC. Expiry already owns this admission lock and calls
	// terminalControlEntry directly to avoid a lock cycle.
	if req.Action == "terminal_close" {
		s.terminalAdmissionMu.Lock()
		defer s.terminalAdmissionMu.Unlock()
	}
	return s.terminalControlEntry(req, e)
}

// Caller owns terminalAdmissionMu for a scoped close, otherwise the action
// renews its validated idle lease under that lock before issuing tmux I/O.
// terminalPasteCommands encodes a bounded literal paste as a sequence of
// independently framed tmux send-keys commands. The chunks are applied under
// one actionMu lock; this does not make PTY input transactional on failure.
func terminalPasteCommands(name string, data []byte) ([]string, error) {
	if len(data) == 0 || len(data) > terminalMaxInput {
		return nil, fmt.Errorf("terminal input must be 1..%d bytes", terminalMaxInput)
	}
	commands := make([]string, 0, (len(data)+terminalInputChunk-1)/terminalInputChunk)
	const hexDigits = "0123456789abcdef"
	for len(data) > 0 {
		n := len(data)
		if n > terminalInputChunk {
			n = terminalInputChunk
		}
		var b strings.Builder
		b.Grow(len(name) + 20 + 3*n)
		b.WriteString("send-keys -t ")
		b.WriteString(name)
		b.WriteString(":0.0 -H")
		// Exact hex tokens prevent quotes, newlines and control bytes from
		// becoming tmux command syntax. Preserve byte order between chunks.
		for _, v := range data[:n] {
			b.WriteByte(' ')
			b.WriteByte(hexDigits[v>>4])
			b.WriteByte(hexDigits[v&15])
		}
		command := b.String()
		if len(command)+1 > maxStdioInputBytes {
			return nil, errors.New("terminal input chunk exceeds broker frame")
		}
		commands = append(commands, command)
		data = data[n:]
	}
	return commands, nil
}

func (s *Server) terminalControlEntry(req Request, e *stdioSession) Response {
	t := e.terminal
	var command string
	var commands []string
	switch req.Action {
	case "terminal_write":
		var inputErr error
		commands, inputErr = terminalPasteCommands(t.name, []byte(req.Content))
		if inputErr != nil {
			return terminalError("invalid_input", inputErr)
		}
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
	if req.Action != "terminal_close" {
		if err := s.renewTerminalLease(req, e); err != nil {
			return s.finishActionAudit(req, req.Action, auditMode(req.Root), e.id, "developer_terminal", started,
				Response{SessionID: e.id, SessionEpoch: t.epoch, Error: "Terminal idle lease renewal unverified: " + err.Error(), ErrorCode: "terminal_lease_unverified", ErrorClass: "state"})
		}
	}
	if len(commands) == 0 {
		commands = []string{command}
	}
	err := s.terminalCommands(req, e, commands)
	if req.Action == "terminal_close" && t.scoped {
		// At the PID1 idle deadline (or finite scope hard fallback),
		// the backend may exit before Go can send kill-server. A verified stop of
		// the exact session scope is sufficient to reconcile that close;
		// never abandon its root-only record and capacity after expiry.
		if scopeErr := stopScopedTerminalBackend(t.name); scopeErr != nil {
			err = errors.Join(err, scopeErr)
		} else {
			err = nil
		}
	}
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

// One logical terminal write may require multiple bounded tmux commands.
// Keep the Control Mode action lock across the entire sequence: a concurrent
// write/interrupt/close must never interleave between paste fragments.
func (s *Server) terminalCommand(req Request, e *stdioSession, command string) error {
	return s.terminalCommands(req, e, []string{command})
}

func (s *Server) terminalCommands(req Request, e *stdioSession, commands []string) error {
	if len(commands) == 0 {
		return errors.New("empty terminal command")
	}
	// Validate the complete request before writing any fragment.
	for _, command := range commands {
		if len(command)+1 > maxStdioInputBytes {
			return errors.New("terminal_command_too_large")
		}
	}
	t := e.terminal
	t.actionMu.Lock()
	defer t.actionMu.Unlock()
	for i, command := range commands {
		if err := s.terminalCommandLocked(req, e, command); err != nil {
			if len(commands) > 1 {
				// A tmux acknowledgement proves the command was accepted,
				// not that the foreground program consumed the input.
				// Never replay an ambiguous suffix automatically.
				return fmt.Errorf("terminal paste incomplete/uncertain after %d of %d tmux chunks acknowledged; inspect terminal state before retry: %w", i, len(commands), err)
			}
			return err
		}
	}
	return nil
}

// Called only with actionMu held, for a single bounded tmux control command.
func (s *Server) terminalCommandLocked(req Request, e *stdioSession, command string) error {
	t := e.terminal
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
	if entry.terminal != nil {
		s.terminalAdmissionMu.Lock()
		if entry.terminalLeaseStillValid(time.Now()) {
			s.terminalAdmissionMu.Unlock()
			return
		}
		entry.mu.Lock()
		uncertain := entry.terminal.scopeUncertain
		root, epoch := entry.terminal.root, entry.terminal.epoch
		entry.mu.Unlock()
		req := Request{Action: "terminal_close", PrincipalID: entry.ownerID, PrincipalClass: entry.ownerClass,
			Workspace: entry.workspace, SessionID: entry.id, Root: root, Approval: root, SessionEpoch: epoch}
		_ = ensureRequestCorrelation(&req)
		if uncertain {
			// The pending-close recovery path already serializes with admission.
			s.terminalAdmissionMu.Unlock()
			_ = s.terminalControl(req)
			return
		}
		result := s.terminalControlEntry(req, entry)
		s.terminalAdmissionMu.Unlock()
		if !result.OK {
			entry.mu.Lock()
			entry.cleanupErr = errors.New("tmux backend stop not verified")
			entry.mu.Unlock()
		}
		return
	}
	if entry.nonterminalIdleLeaseStillValid(time.Now()) {
		return
	}
	if entry.browser != nil {
		req := Request{Action: "browser_session_close", PrincipalID: entry.ownerID, PrincipalClass: entry.ownerClass, Workspace: entry.workspace, SessionID: entry.id}
		_ = ensureRequestCorrelation(&req)
		result := s.browserSessionAction(context.Background(), req)
		if !result.OK && (result.ErrorCode == "resource_limit" || result.ErrorCode == "browser_session_cleanup_unverified") {
			// Expiry may race a bounded in-flight action or a temporary audit
			// failure. Do not drop the singleton profile lease or silently
			// abandon this session: retry the verified close on the SAME ID.
			time.AfterFunc(5*time.Second, func() { s.expireProcessSession(entry) })
		}
		return
	}
	if entry.dap != nil {
		req := Request{Action: "debug_stop", PrincipalID: entry.ownerID, PrincipalClass: entry.ownerClass, Workspace: entry.workspace, SessionID: entry.id}
		_ = ensureRequestCorrelation(&req)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = s.debugStop(ctx, req)
		return
	}
	_ = s.sessions.removeAndStop(entry)
}
