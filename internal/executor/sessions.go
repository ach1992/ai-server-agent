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
	cmd        *exec.Cmd
	stdin      *os.File
	closed     bool
	exited     bool
	exitCode   int
	completed  time.Time
	lastSeq    uint64
	events     []stdioSessionEvent
	bytes      int
	done       chan struct{}
	expiry     *time.Timer
}

type stdioSessionBroker struct {
	mu       sync.Mutex
	sessions map[string]*stdioSession
}

func newStdioSessionBroker() *stdioSessionBroker {
	return &stdioSessionBroker{sessions: make(map[string]*stdioSession)}
}

func (b *stdioSessionBroker) get(req Request, id string) (*stdioSession, error) {
	if b == nil || id == "" || req.PrincipalID == "" || req.PrincipalClass == "" || req.Root || req.Approval {
		return nil, errSessionNotFound
	}
	b.mu.Lock()
	session := b.sessions[id]
	b.mu.Unlock()
	if session == nil || session.ownerID != req.PrincipalID || session.ownerClass != req.PrincipalClass || filepath.Clean(req.Workspace) != session.workspace {
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
		outdated := entry.exited && now.Sub(entry.completed) >= completedSessionRetain
		if outdated && entry.expiry != nil {
			entry.expiry.Stop()
		}
		entry.mu.Unlock()
		if outdated {
			delete(b.sessions, id)
		}
	}
	if len(b.sessions) >= maxStdioSessions {
		return errSessionBusy
	}
	b.sessions[session.id] = session
	return nil
}

// workerStdioSession starts an explicitly selected language/debug adapter.
// Caller-facing capability handlers must validate their own allowed binaries
// and semantics. The executor never grants root execution through this path.
func (s *Server) workerStdioSession(req Request, kind, workspace, binary string, args ...string) (id string, err error) {
	if s.sessions == nil || req.PrincipalID == "" || req.PrincipalClass == "" || req.Root || req.Approval {
		return "", errors.New("session requires authenticated worker authority")
	}
	if kind != "lsp" && kind != "dap" {
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
	if os.Geteuid() != 0 && uint32(os.Geteuid()) != s.workerUID {
		return "", errors.New("worker identity unavailable")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	entry := &stdioSession{
		id: "stdio_" + hex.EncodeToString(nonce[:]), kind: kind,
		ownerID: req.PrincipalID, ownerClass: req.PrincipalClass,
		workspace: cwd, done: make(chan struct{}), exitCode: -1,
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
	if blocked := s.beginActionAudit(req, "session_create", "worker", kind+"\x00"+cwd+"\x00"+binary, "developer_session"); blocked != nil {
		return "", fmt.Errorf("%s: %s", blocked.ReasonCode, blocked.Error)
	}
	start := time.Now()
	// The dedicated worker HOME/cache lives outside the selected worktree.
	cmd := exec.Command(binary, args...)
	cmd.Dir = cwd
	cmd.Env = sanitizedCommandEnv(s.cfg.WorkspaceDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = processGroupTerminateGrace
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}
	}
	stdin, pipeErr := cmd.StdinPipe()
	if pipeErr != nil {
		s.finishActionAudit(req, "session_create", "worker", kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{Error: pipeErr.Error()})
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
		s.finishActionAudit(req, "session_create", "worker", kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{Error: err.Error()})
		return "", err
	}
	entry.mu.Lock()
	entry.cmd = cmd
	entry.pid = cmd.Process.Pid
	entry.mu.Unlock()
	removeOnFailure = false
	go entry.wait()
	// A forgotten or disconnected session cannot run indefinitely.
	entry.mu.Lock()
	entry.expiry = time.AfterFunc(maxStdioSessionAge, func() { _ = s.sessions.removeAndStop(entry) })
	entry.mu.Unlock()
	audited := s.finishActionAudit(req, "session_create", "worker", kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{OK: true})
	if audited.AuditDegraded {
		// The session already started. Return its identity for reconciliation.
		return entry.id, errors.New("session_started_audit_degraded")
	}
	return entry.id, nil
}

func (e *stdioSession) wait() {
	err := e.cmd.Wait()
	// A child may exit while descendants keep stdout/stderr pipes open.
	// WaitDelay bounds that case; clean up the still-live process group.
	if errors.Is(err, exec.ErrWaitDelay) {
		_, _ = terminateProcessGroup(e.pid)
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
	_ = e.stdin.Close()
	close(e.done)
	e.mu.Unlock()
}

type sessionEventWriter struct {
	entry  *stdioSession
	stream string
}

func (w sessionEventWriter) Write(p []byte) (int, error) {
	total := len(p)
	w.entry.mu.Lock()
	defer w.entry.mu.Unlock()
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
	defer entry.stopMu.Unlock()
	entry.mu.Lock()
	entry.closed = true
	if entry.expiry != nil {
		entry.expiry.Stop()
	}
	pid := entry.pid
	exited := entry.exited
	if entry.stdin != nil {
		_ = entry.stdin.Close()
	}
	entry.mu.Unlock()
	if !exited {
		if pid <= 0 {
			return errors.New("session_stop_outcome_unknown: process identity unavailable")
		}
		if _, err := terminateProcessGroup(pid); err != nil {
			return fmt.Errorf("session_stop_outcome_unknown: %w", err)
		}
		select {
		case <-entry.done:
		case <-time.After(processGroupTerminateGrace + time.Second):
			return errors.New("session_stop_outcome_unknown: child wait not reconciled")
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
