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
	maxStdioSessions             = 8 // legacy/default shared session capacity
	maxConfigurableStdioSessions = 64
	maxStdioInputBytes           = 16 << 10
	maxStdioEventBytes           = 4 << 10
	maxStdioOutputBytes          = 64 << 10
	maxStdioOutputEvents         = 64
	maxStdioSessionAge           = time.Hour
	completedSessionRetain       = 5 * time.Minute
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
	mu                      sync.Mutex
	stopMu                  sync.Mutex
	id                      string
	kind                    string
	ownerID                 string
	ownerClass              string
	workspace               string
	pid                     int
	pidPin                  *os.File // The pidfd is a stable process identity; it does NOT reserve the numeric PGID after reaping.
	cmd                     *exec.Cmd
	stdin                   *os.File
	closed                  bool
	exited                  bool
	exitCode                int
	cleanupErr              error
	completed               time.Time
	lastSeq                 uint64
	events                  []stdioSessionEvent
	bytes                   int
	done                    chan struct{}
	expiry                  *time.Timer
	terminal                *tmuxTerminalState       // consumer-specific Control Mode state; broker owns its process
	terminalRecordPersisted bool                     // guarded by entry.mu; PENDING or ACTIVE file owns capacity
	dap                     *dapState                // DAP stream attaches to this same process identity and broker
	dapStdout               *dapStdoutQueue          // bounded independent stdio transport; only selected DAP adapter
	browser                 *browserStdioState       // managed Browser consumer; never a second process broker
	browserLease            *browserSessionAdmission // release only after verified process cleanup, before broker deletion
}

type stdioSessionBroker struct {
	mu       sync.Mutex
	sessions map[string]*stdioSession
	limit    int
}

func newStdioSessionBroker() *stdioSessionBroker {
	return newStdioSessionBrokerForSlots(maxStdioSessions)
}

func newStdioSessionBrokerForSlots(slots int) *stdioSessionBroker {
	if slots < 1 || slots > maxConfigurableStdioSessions {
		slots = maxStdioSessions
	}
	return &stdioSessionBroker{sessions: make(map[string]*stdioSession), limit: slots}
}

func (s *Server) stdioSessionCapacity() int {
	n := s.cfg.EffectiveRuntime().StdioSessionSlots
	if n < 1 || n > maxConfigurableStdioSessions {
		return maxStdioSessions
	}
	return n
}

// A scope whose startup could not be proven clean has NO Control Mode client
// yet. It remains a principal-bound reserved session until exact shutdown is
// verified. Normal failed-start defers must not discard this identity.
func preserveUncertainTerminalStart(entry *stdioSession, err error) bool {
	var unknown *terminalScopeCleanupUncertain
	if entry == nil || entry.terminal == nil || !errors.As(err, &unknown) {
		return false
	}
	entry.mu.Lock()
	entry.terminal.scopeUncertain = true
	entry.cleanupErr = err
	entry.mu.Unlock()
	return true
}

// Count terminal sessions without a verified durable record. An uncertain
// scope start is NOT a free slot; the caller holds terminalAdmissionMu.
func (b *stdioSessionBroker) unpersistedTerminalCount() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, entry := range b.sessions {
		entry.mu.Lock()
		pending := entry.terminal != nil && !entry.terminalRecordPersisted
		entry.mu.Unlock()
		if pending {
			count++
		}
	}
	return count
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

// All production admissions pass a protected terminal registry snapshot under
// Server.terminalAdmissionMu. Direct broker fixtures have no durable scopes.
func (b *stdioSessionBroker) reserve(session *stdioSession) error {
	return b.reserveAgainstRegistry(session, terminalReservations{})
}

func (b *stdioSessionBroker) pruneCompletedLocked() {
	now := time.Now()
	for id, entry := range b.sessions {
		entry.mu.Lock()
		// Browser session identity is also the ONLY reconciliation record
		// for the shared-profile lease. Only an explicit verified close
		// removes it; generic completed-session GC must never erase it.
		outdated := entry.kind != "browser" && entry.exited && entry.cleanupErr == nil && entry.pidPin == nil && now.Sub(entry.completed) >= completedSessionRetain
		if outdated && entry.expiry != nil {
			entry.expiry.Stop()
		}
		entry.mu.Unlock()
		if outdated {
			delete(b.sessions, id)
		}
	}
}

// Each protected terminal record costs one slot, even if its Control Mode
// client was lost on restart. Reconnected clients with that ID must not be
// counted twice. Nonterminal and unpersisted terminal sessions each cost one.
func (b *stdioSessionBroker) occupiedLocked(registry terminalReservations) int {
	occupied := registry.count()
	for id := range b.sessions {
		if _, persisted := registry.ids[id]; !persisted {
			occupied++
		}
	}
	return occupied
}

func (b *stdioSessionBroker) occupiedAgainstRegistry(registry terminalReservations) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneCompletedLocked()
	return b.occupiedLocked(registry)
}

func (b *stdioSessionBroker) reserveAgainstRegistry(session *stdioSession, registry terminalReservations) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneCompletedLocked()
	if b.sessions[session.id] != nil {
		return errSessionBusy
	}
	if session.terminal != nil && session.terminal.recovered {
		// Must already own a persistent slot. Caller must additionally have
		// proven the original principal, workspace, authority and backend.
		if _, ok := registry.ids[session.id]; !ok {
			return errSessionNotFound
		}
	} else if b.occupiedLocked(registry) >= b.limit {
		return errSessionBusy
	}
	b.sessions[session.id] = session
	return nil
}

// Shared admission has exactly one lock ordering:
// terminalAdmissionMu -> trusted registry scanner -> broker.mu -> entry.mu.
// terminal_open and terminal_reconnect already hold terminalAdmissionMu
// throughout scope creation or authenticated recovery; other session kinds
// acquire it here for the same protected resource budget.
func (s *Server) reserveProcessSession(entry *stdioSession) error {
	if entry.terminal == nil {
		s.terminalAdmissionMu.Lock()
		defer s.terminalAdmissionMu.Unlock()
	}
	registry, err := s.terminalReservationsWithStop(stopScopedTerminalBackend)
	if err != nil {
		return fmt.Errorf("terminal_registry_unavailable: %w", err)
	}
	return s.sessions.reserveAgainstRegistry(entry, registry)
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

// Python DAP must start inside the exact executor-owned private cgroup
// atomically, before any trusted adapter startup code can spawn descendants.
// No other broker consumer or caller may select a cgroup or execution mode.
func (s *Server) workerContainedDAPSession(req Request, workspace, binary string, group *dapCgroup, args ...string) (string, error) {
	if req.Root || req.Approval || group == nil || os.Geteuid() != 0 {
		return "", errors.New("contained DAP requires an unprivileged request on root executor")
	}
	return s.startProcessSessionWithOptions(req, "dap", workspace, binary, nil, "", group, true, args...)
}

// JS adapters listen on a private Unix socket instead of stdio. The same
// original process/session broker owns the verified private cgroup; this
// launches no root debuggee and creates no additional process broker.
func (s *Server) workerContainedSocketDAPSession(req Request, workspace, binary string, group *dapCgroup, args ...string) (string, error) {
	if req.Root || req.Approval || group == nil || os.Geteuid() != 0 {
		return "", errors.New("socket DAP requires root executor-owned worker containment")
	}
	return s.startProcessSessionWithOptions(req, "dap", workspace, binary, nil, "", group, false, args...)
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

// Stateful developer subprocesses must never use the project workspace as
// HOME. The installer provisions this directory under the root-controlled
// Agent state parent, with only the worker UID owning its contents.
func (s *Server) workerSessionHome() (string, error) {
	if !filepath.IsAbs(s.cfg.StateDir) || filepath.Clean(s.cfg.StateDir) != s.cfg.StateDir || s.cfg.StateDir == "/" {
		return "", errors.New("worker home requires a trusted absolute Agent state directory")
	}
	home := filepath.Join(s.cfg.StateDir, "worker-home")
	if withinPath(s.cfg.WorkspaceDir, home) {
		return "", errors.New("worker home must not be within the project workspace")
	}
	if err := trustedDir(s.cfg.StateDir); err != nil {
		return "", fmt.Errorf("worker home parent untrusted: %w", err)
	}
	info, err := os.Lstat(home)
	if err != nil {
		return "", fmt.Errorf("worker home not provisioned: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || st.Uid != s.workerUID || st.Gid != s.workerGID {
		return "", errors.New("worker home is not an owned private directory")
	}
	return home, nil
}

func sanitizedSessionEnv(home string) []string {
	return append(sanitizedCommandEnv(home),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
	)
}

func (s *Server) startProcessSession(req Request, kind, workspace, binary string, terminal *tmuxTerminalState, args ...string) (string, error) {
	return s.startProcessSessionWithID(req, kind, workspace, binary, terminal, "", args...)
}

func (s *Server) startProcessSessionWithID(req Request, kind, workspace, binary string, terminal *tmuxTerminalState, priorID string, args ...string) (id string, err error) {
	return s.startProcessSessionWithOptions(req, kind, workspace, binary, terminal, priorID, nil, false, args...)
}

func (s *Server) startProcessSessionWithOptions(req Request, kind, workspace, binary string, terminal *tmuxTerminalState, priorID string, containment *dapCgroup, directDAP bool, args ...string) (id string, err error) {
	if directDAP && (kind != "dap" || terminal != nil || priorID != "" || req.Root || req.Approval) {
		return "", errors.New("private DAP stdio is available only to fresh worker-owned DAP sessions")
	}
	if containment != nil && (kind != "dap" || terminal != nil || priorID != "" || req.Root || req.Approval || os.Geteuid() != 0) {
		return "", errors.New("DAP cgroup is only valid for worker-only fresh debug sessions")
	}
	if s.sessions == nil || req.PrincipalID == "" || req.PrincipalClass == "" || req.Root != (terminal != nil && terminal.root) || req.Approval != req.Root {
		return "", errors.New("session requires authenticated worker authority")
	}
	if kind != "lsp" && kind != "dap" && kind != "browser" && (kind != "terminal" || terminal == nil) {
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
		terminalRecordPersisted: terminal != nil && terminal.recovered,
		id:                      priorID, kind: kind,
		ownerID: req.PrincipalID, ownerClass: req.PrincipalClass,
		workspace: cwd, done: make(chan struct{}), exitCode: -1, terminal: terminal,
	}
	if kind == "browser" {
		// Attach before broker reservation, process spawn, and completion
		// audit. Even a post-spawn audit failure must leave the Browser
		// entry addressable for status/close/expiry reconciliation.
		entry.browser = &browserStdioState{}
		entry.browserLease = &s.browserAdmission
	}
	if err := s.reserveProcessSession(entry); err != nil {
		return "", err
	}
	removeOnFailure := true
	defer func() {
		if removeOnFailure {
			s.sessions.remove(entry)
		}
	}()
	// An externally owned scope must have an fsynced pending reservation
	// BEFORE systemd-run. Every later startup failure must reconcile that
	// record; no in-memory-only uncertain backend is permitted.
	launchedScope, pendingLinked := false, false
	defer func() {
		if !removeOnFailure || !pendingLinked {
			return
		}
		var cleanupErr error
		if launchedScope {
			stop := stopScopedTerminalBackend
			if s.terminalScopeStopTestHook != nil {
				stop = s.terminalScopeStopTestHook
			}
			cleanupErr = stop(terminal.name)
		}
		if cleanupErr == nil {
			cleanupErr = s.deleteTerminalRecord(entry.id)
		}
		if cleanupErr != nil {
			// Until PID1 has verified the stop AND deletion is durable,
			// retain the opaque ID and fail closed on all terminal I/O.
			removeOnFailure = false
			err = errors.Join(err, fmt.Errorf("terminal_scope_reconciliation_unverified: %w", cleanupErr))
			entry.mu.Lock()
			terminal.scopeUncertain = true
			entry.cleanupErr = err
			entry.mu.Unlock()
			id = entry.id
		}
	}()
	if blocked := s.beginActionAudit(req, "session_create", auditMode(req.Root), kind+"\x00"+cwd+"\x00"+binary, "developer_session"); blocked != nil {
		return "", fmt.Errorf("%s: %s", blocked.ReasonCode, blocked.Error)
	}
	start := time.Now()
	// The control-plane tmux process is privileged only to protect its
	// socket. The pane command itself must drop to the requested worker UID.
	rootControlledWorkerTmux := terminal != nil && !terminal.root && s.terminalBinary == ""
	if rootControlledWorkerTmux && os.Geteuid() != 0 {
		return "", errors.New("worker tmux requires root executor-owned backend")
	}
	if terminal != nil && s.terminalBinary == "" && !terminal.recovered {
		linked, reservationErr := s.persistPendingTerminalRecord(entry)
		pendingLinked = linked
		if linked {
			entry.mu.Lock()
			entry.terminalRecordPersisted = true
			entry.mu.Unlock()
		}
		if reservationErr != nil {
			return "", fmt.Errorf("terminal_pending_reservation_unverified: %w", reservationErr)
		}
		launch := s.startScopedTerminalBackend
		if s.terminalScopeStartTestHook != nil {
			launch = s.terminalScopeStartTestHook
		}
		if scopeErr := launch(req, binary, terminal, args); scopeErr != nil {
			// A may-exist privileged scope with unverified cleanup must never
			// turn into a normal failed start. Preserve the already reserved
			// broker identity; its SessionID is the explicit close/retry locator.
			if preserveUncertainTerminalStart(entry, scopeErr) {
				removeOnFailure = false
				id = entry.id
			}
			s.finishActionAudit(req, "session_create", auditMode(req.Root), kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{Error: scopeErr.Error()})
			return id, scopeErr
		}
		launchedScope = true
		args = []string{"-f", "/dev/null", "-S", terminal.socket, "-C", "attach-session", "-t", terminal.name}
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = cwd
	home := "/root"
	if !req.Root {
		home, err = s.workerSessionHome()
		if err != nil {
			return "", err
		}
	}
	cmd.Env = sanitizedSessionEnv(home)
	if rootControlledWorkerTmux {
		// Do not supply worker-writable HOME/XDG paths to the root tmux server.
		cmd.Env = sanitizedSessionEnv("/root")
	}
	if terminal != nil {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if containment != nil {
		if containment.path == "" || containment.verified {
			return "", errors.New("DAP private cgroup not fresh")
		}
		fd, openErr := unix.Open(containment.path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
		if openErr != nil {
			return "", fmt.Errorf("DAP cgroup fd unavailable: %w", openErr)
		}
		defer unix.Close(fd)
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = fd
	}
	cmd.WaitDelay = processGroupTerminateGrace
	if os.Geteuid() == 0 {
		if req.Root || rootControlledWorkerTmux {
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
	if directDAP {
		entry.dapStdout = newDAPStdoutQueue()
		cmd.Stdout = entry.dapStdout
	} else {
		cmd.Stdout = sessionEventWriter{entry: entry, stream: "stdout"}
	}
	cmd.Stderr = sessionEventWriter{entry: entry, stream: "stderr"}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		s.finishActionAudit(req, "session_create", auditMode(req.Root), kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{Error: err.Error()})
		return "", err
	}
	if containment != nil {
		if membershipErr := containment.verifySpawnMembership(cmd.Process.Pid); membershipErr != nil {
			_ = stdin.Close()
			_, stopErr := terminateProcessGroup(cmd.Process.Pid)
			_ = cmd.Wait()
			failure := errors.Join(membershipErr, stopErr)
			s.finishActionAudit(req, "session_create", "worker", kind+"\x00"+cwd+"\x00"+binary, "developer_session", start, Response{Error: failure.Error()})
			return "", failure
		}
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
	// WNOWAIT is essential: retain this DIRECT child's exit status (and
	// therefore numeric PID reservation) while reconciling same-group
	// descendants. Reaping first and using kill(-oldPGID) is unsafe.
	e.mu.Lock()
	pid, pin := e.pid, e.pidPin
	e.mu.Unlock()
	var witnessErr error
	if pid <= 0 || pin == nil {
		witnessErr = errors.New("session_pid_identity_unavailable")
	} else {
		witnessErr = unix.Waitid(unix.P_PIDFD, int(pin.Fd()), nil, unix.WEXITED|unix.WNOWAIT, nil)
	}
	if e.dapStdout != nil {
		e.dapStdout.adapterExited()
	}
	e.stopMu.Lock()
	var cleanupErr error
	if pid > 0 && pin != nil {
		_, cleanupErr = terminatePinnedGroup(pid, pin, witnessErr == nil)
	} else {
		cleanupErr = errors.New("session_pid_identity_unavailable")
	}
	cleanupErr = errors.Join(witnessErr, cleanupErr)
	e.mu.Lock()
	d := e.dap
	e.mu.Unlock()
	if d != nil {
		if d.phpProxy != nil {
			d.phpProxy.shutdown()
		}
		d.mu.Lock()
		cleanupErr = errors.Join(cleanupErr, d.stopPinnedDebuggee())
		if d.adapterPin != nil {
			_ = d.adapterPin.Close()
			d.adapterPin = nil
		}
		d.mu.Unlock()
	}
	// Only after the direct child's process group has been reconciled may
	// os/exec reap the child. The direct-child fallback is no longer valid
	// after this point; later retries must use pidfd-only group signalling.
	err := e.cmd.Wait()
	if e.dapStdout != nil {
		e.dapStdout.finish()
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
		if entry.dap.phpProxy != nil {
			entry.dap.phpProxy.shutdown()
		}
		entry.dap.mu.Lock()
		if entry.dap.conn != nil {
			_ = entry.dap.conn.Close()
		}
		if entry.dap.parentConn != nil {
			_ = entry.dap.parentConn.Close()
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
		if _, err := terminatePinnedGroup(pid, pin, !exited); err != nil {
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
	// A rejected later DAP process identity is a permanent lack of cleanup
	// proof, even if the original pinned target has already been stopped.
	if entry.dap != nil {
		entry.dap.mu.Lock()
		pinError := entry.dap.pinError
		targetStillPinned := entry.dap.debuggeePin != nil
		entry.dap.mu.Unlock()
		// A successful verified cgroup.kill reconciles all descendants,
		// including identities never delivered in DAP events.
		entry.dap.mu.Lock()
		proven := entry.dap.containmentProven
		entry.dap.mu.Unlock()
		if !proven && (pinError != "" || targetStillPinned) {
			return errors.New("session_stop_outcome_unknown: DAP target cleanup not proven")
		}
	}
	// DAP's private socket is transport runtime state only, not a second
	// process owner. Clean it only after pidfd-pinned broker cleanup succeeds.
	if entry.dap != nil {
		if entry.dap.phpProxy != nil {
			if err := entry.dap.phpProxy.removeAfterStop(); err != nil {
				return fmt.Errorf("php_dap_proxy_cleanup_uncertain: %w", err)
			}
		} else {
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
	}
	// Chromium can recreate its user-data directory while still running.
	// Never remove a Browser DAP profile before the original adapter AND all
	// descendants have been proven dead by this session's cgroup.kill.
	// The root-owned generated profile is only cleanup state, not a second
	// process authority; retain the broker ID on any unverified removal.
	if entry.dap != nil && entry.dap.browserProfileDir != "" {
		entry.dap.mu.Lock()
		proven := entry.dap.containmentProven
		entry.dap.mu.Unlock()
		if !proven {
			return errors.New("javascript_browser_profile_cleanup_before_verified_cgroup")
		}
		if err := removeBrowserDAPProfile(entry.dap.browserProfileDir); err != nil {
			return fmt.Errorf("javascript_browser_profile_cleanup_uncertain: %w", err)
		}
	}
	// Browser has one shared-profile lease. Do not remove the last broker
	// reconciliation identity until the verified process is dead AND the
	// exact lease has been released; an audit-completion failure happens
	// *later* and must not cause an unrecoverable profile lock.
	if entry.browserLease != nil && !entry.browserLease.release(entry.id) {
		return errors.New("browser_profile_lease_reconciliation_failed")
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

// cleanlyRemoved is intentionally keyed to the exact previously authorized
// pointer, not only a reusable session ID or optimistic close response.
// Only the broker's verified process-group reconciliation may prove the
// Browser profile safe to release after a completion-audit failure.
func (b *stdioSessionBroker) cleanlyRemoved(e *stdioSession) bool {
	if b == nil || e == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions[e.id] != nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed && e.exited && e.cleanupErr == nil && e.pidPin == nil
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
