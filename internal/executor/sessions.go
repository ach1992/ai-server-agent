package executor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// This broker is an executor-private building block for LSP/DAP. It is not a
// generic MCP process tool. Terminal/tmux control remains owned by #60.
const (
	maxStdioSessions       = 8
	maxStdioInputBytes     = 16 << 10
	maxStdioEventBytes     = 4 << 10
	maxStdioOutputBytes    = 64 << 10
	maxStdioOutputEvents   = 64
	maxStdioSessionAge     = time.Hour
	completedSessionRetain = 5 * time.Minute
)

var (
	errSessionNotFound = errors.New("session_not_found")
	errSessionBusy     = errors.New("session_resource_limit")
	errSessionStopped  = errors.New("session_stopped")
	errSessionCursor   = errors.New("session_invalid_cursor")
)

type stdioSessionEvent struct {
	Sequence uint64
	Stream   string
	Data     []byte
}

type stdioSessionRead struct {
	Events        []stdioSessionEvent
	Earliest      uint64
	Latest        uint64
	Truncated     bool
	Running       bool
	ExitCode      int
	BytesRetained int
}

type stdioSession struct {
	mu         sync.Mutex
	stopMu     sync.Mutex
	id         string
	kind       string
	ownerID    string
	ownerClass string
	workspace  string
	pid        int
	pidPin     *os.File // Linux pidfd pins the process-group leader PID across Wait/reaping.
	cmd        *exec.Cmd
	stdin      *os.File
	closed     bool
	exited     bool
	exitCode   int
	cleanupErr error
	completed  time.Time
	lastSeq    uint64
	events     []stdioSessionEvent
	bytes      int
	done       chan struct{}
	expiry     *time.Timer
	terminal   *tmuxTerminalState // consumer-specific Control Mode state; broker owns its process
	dap        *dapState          // DAP stream attaches to this same process identity and broker
}

type stdioSessionBroker struct {
	mu       sync.Mutex
	sessions map[string]*stdioSession
}

func newStdioSessionBroker() *stdioSessionBroker {
	return &stdioSessionBroker{sessions: make(map[string]*stdioSession)}
}

func (b *stdioSessionBroker) get(req Request, id string) (*stdioSession, error) {
	if b == nil || id == "" || req.PrincipalID == "" || req.PrincipalClass == "" {
		return nil, errSessionNotFound
	}
	b.mu.Lock()
	session := b.sessions[id]
	b.mu.Unlock()
	if session == nil || session.ownerID != req.PrincipalID || session.ownerClass != req.PrincipalClass || filepath.Clean(req.Workspace) != session.workspace || (session.terminal != nil && session.terminal.root != req.Root) || (session.terminal == nil && (req.Root || req.Approval)) || (session.terminal != nil && req.Approval != req.Root) {
		// Do not reveal whether another principal's opaque session exists.
		return nil, errSessionNotFound
	}
	return session, nil
}

func (b *stdioSessionBroker) reserve(session *stdioSession) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for id, entry := range b.sessions {
		entry.mu.Lock()
		outdated := entry.exited && entry.cleanupErr == nil && entry.pidPin == nil && now.Sub(entry.completed) >= completedSessionRetain
		if outdated && entry.expiry != nil {
			entry.expiry.Stop()
		}
		entry.mu.Unlock()
		if outdated {
			delete(b.sessions, id)
		}
	}
	if len(b.sessions) >= maxStdioSessions || b.sessions[session.id] != nil {
		return errSessionBusy
	}
	b.sessions[session.id] = session
	return nil
}

// workerStdioSession starts an explicitly selected language/debug adapter.
// Caller-facing capability handlers must validate their own allowed binaries
// and semantics. The executor never grants root execution through this path.
func (s *Server) workerStdioSession(req Request, kind, workspace, binary string, args ...string) (string, error) {
	if req.Root || req.Approval {
		return "", errors.New("worker stdio sessions cannot be privileged")
	}
	return s.workerProcessSession(req, kind, workspace, binary, nil, args...)
}

func (s *Server) workerProcessSession(req Request, kind, workspace, binary string, terminal *tmuxTerminalState, args ...string) (string, error) {
	if req.Root || req.Approval {
		return "", errors.New("worker process cannot be privileged")
	}
	return s.startProcessSession(req, kind, workspace, binary, terminal, args...)
}

func (s *Server) rootTmuxProcessSession(req Request, workspace, binary string, terminal *tmuxTerminalState, args ...string) (string, error) {
	if !req.Root || !req.Approval || terminal == nil || !terminal.root || os.Geteuid() != 0 {
		return "", errors.New("root_terminal_authorization_required")
	}
	return s.startProcessSession(req, "terminal", workspace, binary, terminal, args...)
}

func (s *Server) startProcessSession(req Request, kind, workspace, binary string, terminal *tmuxTerminalState, args ...string) (string, error) {
	return s.startProcessSessionWithID(req, kind, workspace, binary, terminal, "", args...)
}

func (s *Server) startProcessSessionWithID(req Request, kind, workspace, binary string, terminal *tmuxTerminalState, priorID string, args ...string) (id string, err error) {
	if s.sessions == nil || req.PrincipalID == "" || req.PrincipalClass == "" || req.Root != (terminal != nil && terminal.root) || req.Approval != req.Root {
		return "", errors.New("session requires authenticated worker authority")
	}
	if kind != "lsp" && kind != "dap" && (kind != "terminal" || terminal == nil) {
		return "", errors.New("unsupported session kind")
	}
	if !filepath.IsAbs(binary) || len(binary) > 4096 || len(args) > 64 {
		return "", errors.New("invalid session executable")
	}
	for _, arg := range args {
		if len(arg) > 4096 {
			return "", errors.New("session argument too large")
		}
	}
	cwd, err := s.workspacePath(workspace, true)
	if err != nil {
		return "", err
	}
	if filepath.Clean(req.Workspace) != cwd {
		return "", errors.New("session workspace does not match request identity")
	}
	if req.Root && os.Geteuid() != 0 {
		return "", errors.New("root executor unavailable")
	}
	if !req.Root && os.Geteuid() != 0 && uint32(os.Geteuid()) != s.workerUID {
		return "", errors.New("worker identity unavailable")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	if priorID != "" {
		if !validTerminalID(priorID) || terminal == nil {
			return "", errors.New("invalid recovered session id")
		}
	} else {
		priorID = "stdio_" + hex.EncodeToString(nonce[:])
	}
	entry := &stdioSession{
		id: priorID, kind: kind,
		ownerID: req.PrincipalID, ownerClass: req.PrincipalClass,
		workspace: cwd, done: make(chan struct{}), exitCode: -1, terminal: terminal,
	}
	if err := s.sessions.reserve(entry); err != nil {
		return "", err
	}
	removeOnFailure := true
	defer func() {
		if removeOnFailure {
			s.sessions.remove(entry)
		}
	}()
	if blocked := s.beginActionAudit(req, "session_create", auditMode(req.Root), kind+"\x00"+cwd+"\x00"+binary, "developer_session"); blocked != nil {
		return "", fmt.Errorf("%s: %s", blocked.ReasonCode, blocked.Error)
	}
	start := time.Now()
	// The dedicated worker HOME/cache lives outside the selected worktree.
	cmd := exec.Command(binary, args...)
	cmd.Dir = cwd
	home := s.cfg.WorkspaceDir
	if req.Root {
		home = "/root"
	}
	cmd.Env = sanitizedCommandEnv(home)
	if terminal != nil {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = processGroupTerminateGrace
	if os.Geteuid() == 0 {
		if req.Root {
			cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{0}}
		} else {
			cmd.SysProcAttr.Credential = &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}
		}
	}
	stdin, pipeErr := cmd.StdinPipe()
	if pipeErr != nil {
		s.finishActionAudit(req, "session_create", auditMode(req.Root), kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{Error: pipeErr.Error()})
		return "", pipeErr
	}
	f, ok := stdin.(*os.File)
	if !ok {
		_ = stdin.Close()
		return "", errors.New("session stdin is not a pollable pipe")
	}
	entry.stdin = f
	cmd.Stdout = sessionEventWriter{entry: entry, stream: "stdout"}
	cmd.Stderr = sessionEventWriter{entry: entry, stream: "stderr"}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		s.finishActionAudit(req, "session_create", auditMode(req.Root), kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{Error: err.Error()})
		return "", err
	}
	// The process is not reaped until cmd.Wait; pin its PID now so a
	// later process-group signal can never target an unrelated, recycled
	// PID after the leader exits. Supported Linux targets have pidfd.
	pidfd, pinErr := unix.PidfdOpen(cmd.Process.Pid, 0)
	if pinErr != nil {
		_ = stdin.Close()
		_, stopErr := terminateProcessGroup(cmd.Process.Pid)
		_ = cmd.Wait()
		failure := fmt.Errorf("session_pid_identity_unavailable: %w", pinErr)
		if stopErr != nil {
			failure = fmt.Errorf("%w; session_stop_outcome_unknown: %v", failure, stopErr)
		}
		s.finishActionAudit(req, "session_create", auditMode(req.Root), kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{Error: failure.Error()})
		return "", failure
	}
	entry.mu.Lock()
	entry.cmd = cmd
	entry.pid = cmd.Process.Pid
	entry.pidPin = os.NewFile(uintptr(pidfd), "session-pidfd")
	entry.mu.Unlock()
	removeOnFailure = false
	go entry.wait()
	// A forgotten or disconnected session cannot run indefinitely.
	entry.mu.Lock()
	entry.expiry = time.AfterFunc(maxStdioSessionAge, func() { s.expireProcessSession(entry) })
	entry.mu.Unlock()
	audited := s.finishActionAudit(req, "session_create", auditMode(req.Root), kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{OK: true})
	if audited.AuditDegraded {
		// The session already started. Return its identity for reconciliation.
		return entry.id, errors.New("session_started_audit_degraded")
	}
	return entry.id, nil
}

func (e *stdioSession) wait() {
	err := e.cmd.Wait()

	// Cmd.Wait reaps the leader; child processes may still occupy its
	// process group even when they closed stdout/stderr. The open pidfd
	// prevents reuse of the group ID between reaping and cleanup.
	// Serialize group signals with close/expiry, then mark completion
	// before releasing the pin.
	e.stopMu.Lock()
	e.mu.Lock()
	pid, pin := e.pid, e.pidPin
	e.mu.Unlock()
	var cleanupErr error
	if pid <= 0 || pin == nil {
		cleanupErr = errors.New("session_pid_identity_unavailable")
	} else {
		_, cleanupErr = terminateProcessGroup(pid)
	}
	// Delve's debuggee is NOT in the adapter's process group. Its separate,
	// verified pidfd-pinned process group must be stopped after any adapter
	// exit, including SIGKILL/crash, not only after explicit DAP disconnect.
	e.mu.Lock()
	d := e.dap
	e.mu.Unlock()
	if d != nil {
		d.mu.Lock()
		cleanupErr = errors.Join(cleanupErr, d.stopPinnedDebuggee())
		d.mu.Unlock()
	}
	e.mu.Lock()
	e.exited = true
	e.completed = time.Now()
	e.exitCode = 0
	if err != nil {
		e.exitCode = -1
		if exit, ok := err.(*exec.ExitError); ok {
			e.exitCode = exit.ExitCode()
		}
	}
	e.cleanupErr = cleanupErr
	_ = e.stdin.Close()
	if cleanupErr == nil && e.pidPin != nil {
		_ = e.pidPin.Close()
		e.pidPin = nil
	}
	close(e.done)
	e.mu.Unlock()
	e.stopMu.Unlock()
}

type sessionEventWriter struct {
	entry  *stdioSession
	stream string
}

func (w sessionEventWriter) Write(p []byte) (int, error) {
	total := len(p)
	w.entry.mu.Lock()
	defer w.entry.mu.Unlock()
	if w.entry.terminal != nil && w.stream == "stdout" {
		w.entry.terminal.accept(p)
		return total, nil
	}
	for len(p) > 0 {
		n := len(p)
		if n > maxStdioEventBytes {
			n = maxStdioEventBytes
		}
		w.entry.lastSeq++
		chunk := append([]byte(nil), p[:n]...)
		w.entry.events = append(w.entry.events, stdioSessionEvent{Sequence: w.entry.lastSeq, Stream: w.stream, Data: chunk})
		w.entry.bytes += n
		for w.entry.bytes > maxStdioOutputBytes || len(w.entry.events) > maxStdioOutputEvents {
			w.entry.bytes -= len(w.entry.events[0].Data)
			w.entry.events[0] = stdioSessionEvent{}
			w.entry.events = w.entry.events[1:]
		}
		p = p[n:]
	}
	return total, nil
}

func (b *stdioSessionBroker) read(req Request, id string, after uint64, byteLimit int) (stdioSessionRead, error) {
	e, err := b.get(req, id)
	if err != nil {
		return stdioSessionRead{}, err
	}
	if byteLimit < maxStdioEventBytes || byteLimit > maxStdioOutputBytes {
		return stdioSessionRead{}, errors.New("invalid session read limit")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if after > e.lastSeq {
		// A caller with a stale or malformed future cursor must not receive
		// a misleading empty result indefinitely and silently lose events.
		return stdioSessionRead{}, fmt.Errorf("%w: latest=%d", errSessionCursor, e.lastSeq)
	}
	out := stdioSessionRead{Latest: e.lastSeq, Earliest: e.lastSeq + 1, Running: !e.exited && !e.closed, ExitCode: e.exitCode, BytesRetained: e.bytes}
	if len(e.events) != 0 {
		out.Earliest = e.events[0].Sequence
	}
	out.Truncated = after < out.Earliest-1
	remaining := byteLimit
	for _, event := range e.events {
		if event.Sequence <= after {
			continue
		}
		if len(event.Data) > remaining {
			break // Never skip an earlier event to deliver a later one.
		}
		out.Events = append(out.Events, stdioSessionEvent{Sequence: event.Sequence, Stream: event.Stream, Data: append([]byte(nil), event.Data...)})
		remaining -= len(event.Data)
	}
	return out, nil
}

func (b *stdioSessionBroker) write(req Request, id string, p []byte) error {
	e, err := b.get(req, id)
	if err != nil {
		return err
	}
	if len(p) == 0 || len(p) > maxStdioInputBytes {
		return errors.New("invalid session write limit")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.exited {
		return errSessionStopped
	}
	if err := e.stdin.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	n, err := e.stdin.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func (b *stdioSessionBroker) remove(entry *stdioSession) {
	b.mu.Lock()
	if b.sessions[entry.id] == entry {
		delete(b.sessions, entry.id)
	}
	b.mu.Unlock()
}

func (b *stdioSessionBroker) removeAndStop(entry *stdioSession) error {
	entry.stopMu.Lock()
	entry.mu.Lock()
	entry.closed = true
	if entry.dap != nil {
		entry.dap.mu.Lock()
		if entry.dap.conn != nil {
			_ = entry.dap.conn.Close()
		}
		entry.dap.mu.Unlock()
	}
	if entry.expiry != nil {
		entry.expiry.Stop()
	}
	pid, pin := entry.pid, entry.pidPin
	exited, cleanupErr := entry.exited, entry.cleanupErr
	if entry.stdin != nil {
		_ = entry.stdin.Close()
	}
	entry.mu.Unlock()

	// The leader may have exited and been reaped. Never signal a naked
	// process-group number; the pidfd must still pin that identity.
	if !exited || cleanupErr != nil {
		if pid <= 0 || pin == nil {
			entry.stopMu.Unlock()
			return errors.New("session_stop_outcome_unknown: process identity unavailable")
		}
		if _, err := terminateProcessGroup(pid); err != nil {
			entry.stopMu.Unlock()
			return fmt.Errorf("session_stop_outcome_unknown: %w", err)
		}
		if exited {
			// Retry the pinned debuggee cleanup as part of reconciliation;
			// a prior failure is never turned into clean termination merely
			// because the Delve adapter's group has disappeared.
			if entry.dap != nil {
				entry.dap.mu.Lock()
				targetErr := entry.dap.stopPinnedDebuggee()
				entry.dap.mu.Unlock()
				if targetErr != nil {
					entry.stopMu.Unlock()
					return fmt.Errorf("session_stop_outcome_unknown: %w", targetErr)
				}
			}
			entry.mu.Lock()
			entry.cleanupErr = nil
			_ = entry.pidPin.Close()
			entry.pidPin = nil
			entry.mu.Unlock()
		}
	}
	entry.stopMu.Unlock()

	if !exited {
		select {
		case <-entry.done:
		case <-time.After(processGroupTerminateGrace + time.Second):
			return errors.New("session_stop_outcome_unknown: child wait not reconciled")
		}
	}
	entry.mu.Lock()
	finalErr := entry.cleanupErr
	entry.mu.Unlock()
	if finalErr != nil {
		// Keep this session addressable and its PID pinned; deletion would
		// turn an uncertain cleanup into an unauditable orphan.
		return fmt.Errorf("session_stop_outcome_unknown: %w", finalErr)
	}
	// DAP's private socket is transport runtime state only, not a second
	// process owner. Clean it only after pidfd-pinned broker cleanup succeeds.
	if entry.dap != nil {
		dir := entry.dap.socketDir
		if dir != "" {
			socket := filepath.Join(dir, "dap.sock")
			if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("dap_socket_cleanup_uncertain: %w", err)
			}
			if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("dap_runtime_cleanup_uncertain: %w", err)
			}
		}
	}
	b.remove(entry)
	return nil
}

func (b *stdioSessionBroker) close(req Request, id string) error {
	e, err := b.get(req, id)
	if err != nil {
		return err
	}
	return b.removeAndStop(e)
}

// Owning capability handlers must use these audit-gated entry points, never
// the broker's raw write/close methods. Raw input/output is not audit content.
func (s *Server) workerStdioWrite(req Request, id string, data []byte) error {
	if _, err := s.sessions.get(req, id); err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxStdioInputBytes {
		return errors.New("invalid session write limit")
	}
	if blocked := s.beginActionAudit(req, "session_write", "worker", id, "developer_session"); blocked != nil {
		return fmt.Errorf("%s: %s", blocked.ReasonCode, blocked.Error)
	}
	start := time.Now()
	err := s.sessions.write(req, id, data)
	result := Response{OK: err == nil}
	if err != nil {
		result.Error = err.Error()
	}
	result = s.finishActionAudit(req, "session_write", "worker", id, "developer_session", start, result)
	if result.AuditDegraded {
		return errors.New("session_write_outcome_audit_degraded: inspect session before retry")
	}
	return err
}

func (s *Server) workerStdioClose(req Request, id string) error {
	if _, err := s.sessions.get(req, id); err != nil {
		return err
	}
	if blocked := s.beginActionAudit(req, "session_close", "worker", id, "developer_session"); blocked != nil {
		return fmt.Errorf("%s: %s", blocked.ReasonCode, blocked.Error)
	}
	start := time.Now()
	err := s.sessions.close(req, id)
	result := Response{OK: err == nil}
	if err != nil {
		result.Error = err.Error()
	}
	result = s.finishActionAudit(req, "session_close", "worker", id, "developer_session", start, result)
	if result.AuditDegraded {
		return errors.New("session_close_outcome_audit_degraded: inspect session before retry")
	}
	return err
}
