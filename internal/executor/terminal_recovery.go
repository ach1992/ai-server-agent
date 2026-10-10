package executor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A root/executor-owned runtime record is enough to reattach tmux after an
// executor restart. It is deliberately NOT a persistent session broker: only
// tmux survives restart; stdio LSP/DAP processes keep their existing semantics.
// These records are not Git/project truth and never contain shell input/output.
type terminalRecord struct {
	Version    int       `json:"version"`
	ID         string    `json:"id"`
	OwnerID    string    `json:"owner_id"`
	OwnerClass string    `json:"owner_class"`
	Workspace  string    `json:"workspace"`
	Root       bool      `json:"root"`
	Scoped     bool      `json:"scoped,omitempty"`
	Lifecycle  string    `json:"lifecycle,omitempty"` // pending before PID1 launch, active after pane verification
	Epoch      string    `json:"epoch,omitempty"`     // only pending; binds exact close across Executor restart
	SocketDir  string    `json:"socket_dir"`
	Name       string    `json:"name"`
	Pane       string    `json:"pane"`
	Columns    int       `json:"columns"`
	Rows       int       `json:"rows"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func validTerminalID(id string) bool {
	if len(id) != 38 || !strings.HasPrefix(id, "stdio_") {
		return false
	}
	_, err := hex.DecodeString(id[6:])
	return err == nil
}
func validTerminalEpoch(epoch string) bool {
	if len(epoch) != 24 {
		return false
	}
	_, err := hex.DecodeString(epoch)
	return err == nil
}

func validTmuxSessionName(name string) bool {
	if len(name) != 28 || !strings.HasPrefix(name, "asa_") {
		return false
	}
	_, err := hex.DecodeString(name[4:])
	return err == nil
}
func (s *Server) terminalRecordsDir() (string, error) {
	if !filepath.IsAbs(s.cfg.StateDir) || s.cfg.StateDir == "/" {
		return "", errors.New("trusted absolute Agent state directory required")
	}
	if err := trustedDir(s.cfg.StateDir); err != nil {
		return "", err
	}
	dir := filepath.Join(s.cfg.StateDir, "terminal-sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := trustedDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}
func (s *Server) terminalRecordPath(id string) (string, error) {
	if !validTerminalID(id) {
		return "", errSessionNotFound
	}
	dir, err := s.terminalRecordsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

var errTerminalPending = errors.New("terminal_start_uncertain")

// Retired scoped servers must not permanently consume the limited terminal
// registry after an Executor crash. Never trust the filename alone: inspect
// the private root-owned record without following symlinks. Only records
// explicitly tagged as new managed scopes and already expired may be reaped.
func (s *Server) terminalRecordCount() (int, error) {
	return s.terminalRecordCountWithStop(stopScopedTerminalBackend)
}

func (s *Server) terminalRecordCountWithStop(stop func(string) error) (int, error) {
	dir, err := s.terminalRecordsDir()
	if err != nil {
		return 0, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	count := 0
	now := time.Now()
	for _, entry := range files {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validTerminalID(id) {
			count++ // ambiguous root-only state must never be deleted on guesswork
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, openErr := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if openErr != nil {
			return 0, fmt.Errorf("terminal_record_cleanup_unverified: %w", openErr)
		}
		info, statErr := f.Stat()
		var rec terminalRecord
		var parseErr error
		if statErr == nil {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 ||
				st.Uid != uint32(os.Geteuid()) || info.Size() > 4096 {
				parseErr = errors.New("terminal_record_untrusted")
			} else {
				var body []byte
				body, parseErr = io.ReadAll(io.LimitReader(f, 4097))
				if parseErr == nil {
					parseErr = json.Unmarshal(body, &rec)
				}
			}
		} else {
			parseErr = statErr
		}
		closeErr := f.Close()
		if closeErr != nil {
			return 0, fmt.Errorf("terminal_record_close_unverified: %w", closeErr)
		}
		if parseErr != nil || rec.Version != 1 || rec.ID != id ||
			!rec.Scoped || !validTmuxSessionName(rec.Name) ||
			(rec.Lifecycle != "" && rec.Lifecycle != "active" && rec.Lifecycle != "pending") ||
			(rec.Lifecycle == "pending" && (!validTerminalEpoch(rec.Epoch) || !validTerminalPendingSocket(rec, s.cfg.StateDir))) ||
			rec.ExpiresAt.IsZero() || !now.After(rec.ExpiresAt) {
			count++
			continue
		}
		// A dead Executor cannot fire its in-memory expiry timer. PID1's
		// one-hour limit still owns the backend. Stop only the exact, random
		// scoped unit from this trusted expired record, then fsync deletion.
		if err := stop(rec.Name); err != nil {
			return 0, fmt.Errorf("expired terminal backend not cleaned: %w", err)
		}
		if err := s.deleteTerminalRecord(rec.ID); err != nil {
			return 0, fmt.Errorf("expired terminal record not deleted: %w", err)
		}
	}
	return count, nil
}

// A pending reservation is linked and directory-fsynced before systemd-run
// can create a scope. The returned linked flag tells the caller whether a
// failed sync requires deletion/reconciliation; never delete a collided ID.
func (s *Server) persistPendingTerminalRecord(e *stdioSession) (bool, error) {
	e.mu.Lock()
	t := e.terminal
	rec := terminalRecord{Version: 1, ID: e.id, OwnerID: e.ownerID, OwnerClass: e.ownerClass,
		Workspace: e.workspace, Root: t.root, Scoped: true, Lifecycle: "pending",
		Epoch: t.epoch, SocketDir: t.socketDir, Name: t.name, Columns: t.columns,
		Rows: t.rows, ExpiresAt: time.Now().Add(maxStdioSessionAge)}
	e.mu.Unlock()
	if !validTerminalPendingSocket(rec, s.cfg.StateDir) || !validTerminalEpoch(rec.Epoch) {
		return false, errors.New("terminal_pending_identity_invalid")
	}
	return s.writeTerminalRecord(rec, false)
}

// The same durable file transitions from PENDING to ACTIVE by a single atomic
// rename. A crash sees one complete lifecycle state, never a missing record.
// Do not reset expiry: PID1's hard scope TTL began at initial launch.
func (s *Server) persistTerminalRecord(req Request, e *stdioSession) error {
	e.mu.Lock()
	t := e.terminal
	rec := terminalRecord{Version: 1, ID: e.id, OwnerID: e.ownerID, OwnerClass: e.ownerClass,
		Workspace: e.workspace, Root: t.root, SocketDir: t.socketDir, Name: t.name,
		Pane: t.pane, Scoped: t.scoped, Lifecycle: "active", Columns: t.columns,
		Rows: t.rows, ExpiresAt: time.Now().Add(maxStdioSessionAge)}
	e.mu.Unlock()
	if rec.Scoped {
		// During terminal_open the public Request has not received its
		// generated SessionID yet. Recovery records are keyed to the
		// broker-assigned opaque ID, not to req.SessionID.
		owner := req
		owner.SessionID = rec.ID
		owner.Workspace = rec.Workspace
		old, err := s.readOwnedTerminalRecord(owner, true)
		if err != nil || old.ID != rec.ID || old.Name != rec.Name || old.SocketDir != rec.SocketDir ||
			old.Root != rec.Root || old.Columns != rec.Columns || old.Rows != rec.Rows {
			return errors.New("terminal_pending_identity_changed")
		}
		rec.ExpiresAt = old.ExpiresAt
	}
	_, err := s.writeTerminalRecord(rec, rec.Scoped)
	return err
}

// This atomic-file writer retains the prior no-clobber rule for initial
// creation. Replacement is allowed ONLY for a previously verified matching
// pending reservation under the private executor-owned namespace.
func (s *Server) writeTerminalRecord(rec terminalRecord, promote bool) (linked bool, err error) {
	path, err := s.terminalRecordPath(rec.ID)
	if err != nil {
		return false, err
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	if len(body) > 4096 {
		return false, errors.New("terminal_record_too_large")
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".terminal-pending-")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	if promote {
		if err = os.Rename(tmp, path); err != nil {
			return false, err
		}
	} else {
		if err = os.Link(tmp, path); err != nil {
			return false, err
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		return true, err
	}
	defer d.Close()
	return true, d.Sync()
}

// Pending scope identity may have no tmux socket or pane yet. Restrict
// reconciliation to the original authenticated owner and trusted namespace;
// absence of a socket must never prevent exact PID1 scope shutdown.
func validTerminalPendingSocket(rec terminalRecord, stateDir string) bool {
	if !rec.Scoped || !validTmuxSessionName(rec.Name) || !filepath.IsAbs(rec.Workspace) ||
		rec.Workspace != filepath.Clean(rec.Workspace) || rec.OwnerID == "" || rec.OwnerClass == "" ||
		rec.ExpiresAt.IsZero() || rec.Columns < 20 || rec.Columns > 240 || rec.Rows < 5 || rec.Rows > 80 {
		return false
	}
	base := filepath.Join(stateDir, "worker-terminals")
	if rec.Root {
		base = filepath.Join(stateDir, "root-terminals")
	}
	return filepath.IsAbs(rec.SocketDir) && rec.SocketDir != base &&
		withinPath(base, rec.SocketDir) && strings.HasPrefix(filepath.Base(rec.SocketDir), ".asa-tmux-") &&
		len(filepath.Join(rec.SocketDir, "socket")) <= terminalMaxUnixSocketPath
}

func (s *Server) readTerminalRecord(req Request) (terminalRecord, error) {
	return s.readOwnedTerminalRecord(req, false)
}

// pendingOnly permits an authenticated owner to reconcile an incomplete
// creation after a brand-new Executor has lost its volatile broker.
func (s *Server) readOwnedTerminalRecord(req Request, pendingOnly bool) (terminalRecord, error) {
	var rec terminalRecord
	path, err := s.terminalRecordPath(req.SessionID)
	if err != nil {
		return rec, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return rec, errSessionNotFound
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return rec, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) || info.Size() > 4096 {
		return rec, errors.New("terminal_record_untrusted")
	}
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return rec, err
	}
	if err = json.Unmarshal(body, &rec); err != nil {
		return rec, errors.New("terminal_record_invalid")
	}
	if rec.Version != 1 || rec.ID != req.SessionID || rec.OwnerID != req.PrincipalID || rec.OwnerClass != req.PrincipalClass || rec.Workspace != filepath.Clean(req.Workspace) || rec.Root != req.Root || req.Approval != req.Root || !validTmuxSessionName(rec.Name) || !validTerminalID(rec.ID) ||
		(rec.Lifecycle != "" && rec.Lifecycle != "active" && rec.Lifecycle != "pending") {
		return terminalRecord{}, errSessionNotFound
	}
	if pendingOnly {
		if rec.Lifecycle != "pending" || !validTerminalEpoch(rec.Epoch) ||
			!validTerminalPendingSocket(rec, s.cfg.StateDir) {
			return terminalRecord{}, errSessionNotFound
		}
		return rec, nil
	}
	if rec.Lifecycle == "pending" {
		return rec, errTerminalPending
	}
	if rec.Pane == "" || rec.Columns < 20 || rec.Columns > 240 || rec.Rows < 5 || rec.Rows > 80 {
		return terminalRecord{}, errSessionNotFound
	}
	// Production worker terminals are executor-root-controlled just like
	// root terminals; only an explicit in-process fixture keeps the former
	// worker-owned socket layout. Never trust a legacy worker-owned socket.
	fixtureWorkerBackend := !rec.Root && s.terminalBinary != ""
	base := filepath.Join(s.cfg.StateDir, "worker-terminals")
	if rec.Root {
		base = filepath.Join(s.cfg.StateDir, "root-terminals")
	} else if fixtureWorkerBackend {
		base = s.cfg.WorkspaceDir
	}
	if !filepath.IsAbs(rec.SocketDir) || !withinPath(base, rec.SocketDir) || !strings.HasPrefix(filepath.Base(rec.SocketDir), ".asa-tmux-") || rec.SocketDir == base {
		return terminalRecord{}, errors.New("terminal_socket_identity_invalid")
	}
	info, err = os.Lstat(rec.SocketDir)
	if err != nil {
		return rec, errors.New("terminal_runtime_missing")
	}
	st, ok = info.Sys().(*syscall.Stat_t)
	expectedUID := uint32(0)
	if fixtureWorkerBackend {
		expectedUID = s.workerUID
	}
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || st.Uid != expectedUID {
		return rec, errors.New("terminal_runtime_untrusted")
	}
	sock := filepath.Join(rec.SocketDir, "socket")
	if len(sock) > terminalMaxUnixSocketPath {
		return rec, errors.New("terminal_socket_path_too_long")
	}
	info, err = os.Lstat(sock)
	if err != nil {
		return rec, errors.New("terminal_backend_missing")
	}
	st, ok = info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || st.Uid != expectedUID {
		return rec, errors.New("terminal_backend_untrusted")
	}
	if time.Now().After(rec.ExpiresAt) {
		return rec, errors.New("terminal_session_expired")
	}
	return rec, nil
}
func (s *Server) deleteTerminalRecord(id string) error {
	path, err := s.terminalRecordPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func terminalEpoch() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// pendingTerminalAction is a deliberately narrow recovery capability: the
// original owner can close a scope after the Executor process (and its
// volatile broker) has died. No socket, pane or Control Mode action is ever
// exposed while the on-disk lifecycle remains PENDING.
func (s *Server) pendingTerminalAction(req Request) (Response, bool) {
	// Serializing exact close with expired-record admission/GC prevents a
	// concurrent open from acting on or deleting the same pending scope.
	if req.Action == "terminal_close" {
		s.terminalAdmissionMu.Lock()
		defer s.terminalAdmissionMu.Unlock()
	}
	rec, err := s.readOwnedTerminalRecord(req, true)
	if err != nil {
		return Response{}, false
	}
	if req.SessionEpoch != rec.Epoch {
		return Response{SessionID: rec.ID, SessionEpoch: rec.Epoch,
			Error: "terminal pending epoch mismatch", ErrorCode: "terminal_epoch_changed", ErrorClass: "state"}, true
	}
	if req.Action != "terminal_close" {
		code := "terminal_start_uncertain"
		if req.Action == "terminal_reconnect" {
			code = "terminal_reconnect_unknown"
		}
		return Response{SessionID: rec.ID, SessionEpoch: rec.Epoch,
			Error:     "terminal creation remains pending; only exact close is permitted",
			ErrorCode: code, ErrorClass: "state"}, true
	}
	if blocked := s.beginActionAudit(req, req.Action, auditMode(req.Root), rec.ID, "developer_terminal"); blocked != nil {
		return *blocked, true
	}
	started := time.Now()
	stop := stopScopedTerminalBackend
	if s.terminalScopeStopTestHook != nil {
		stop = s.terminalScopeStopTestHook
	}
	err = stop(rec.Name)
	if err == nil {
		err = s.deleteTerminalRecord(rec.ID)
	}
	if err == nil {
		removeVerifiedPendingSocketDir(rec.SocketDir)
	}
	result := Response{OK: err == nil, SessionID: rec.ID, SessionEpoch: rec.Epoch}
	if err != nil {
		result.Error = "terminal pending cleanup unverified: " + err.Error()
		result.ErrorCode = "terminal_control_unknown"
		result.ErrorClass = "state"
	}
	return s.finishActionAudit(req, req.Action, auditMode(req.Root), rec.ID, "developer_terminal", started, result), true
}

// Best-effort runtime cleanup must never follow a replaced/symlinked
// directory or unlink a non-socket target. Root-only persisted identity is
// sufficient to STOP the scope even when its local socket is missing.
func removeVerifiedPendingSocketDir(dir string) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return
	}
	socket := filepath.Join(dir, "socket")
	si, err := os.Lstat(socket)
	if err == nil {
		sockStat, ok := si.Sys().(*syscall.Stat_t)
		if !ok || si.Mode()&os.ModeSocket == 0 || sockStat.Uid != uint32(os.Geteuid()) {
			return
		}
		if err := os.Remove(socket); err != nil {
			return
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return
	}
	_ = os.Remove(dir) // never recurse through unexpected contents
}

// Reattach a verified tmux backend without creating a new shell/pane. An old
// output epoch cannot silently refer to a new in-memory bounded event stream.
func (s *Server) terminalReconnect(req Request) Response {
	if !validTerminalID(req.SessionID) {
		return terminalError("session_not_found", errSessionNotFound)
	}
	if current, err := s.sessions.get(req, req.SessionID); err == nil {
		current.mu.Lock()
		if current.terminal != nil && current.terminal.scopeUncertain {
			epoch := current.terminal.epoch
			current.mu.Unlock()
			return Response{SessionID: current.id, SessionEpoch: epoch, Error: "scope cleanup is unverified; close/reconcile original session first", ErrorCode: "terminal_reconnect_unknown", ErrorClass: "state"}
		}
		live := !current.exited && !current.closed && !current.terminal.exited
		epoch := current.terminal.epoch
		current.mu.Unlock()
		if live {
			return Response{OK: true, Status: "already_connected", SessionID: current.id, SessionEpoch: epoch}
		}
		if err := s.sessions.removeAndStop(current); err != nil {
			return terminalError("terminal_disconnect_uncertain", err)
		}
	}
	rec, err := s.readTerminalRecord(req)
	if errors.Is(err, errTerminalPending) {
		if pending, ok := s.pendingTerminalAction(req); ok {
			return pending
		}
		return terminalError("terminal_reconnect_unknown", errTerminalPending)
	}
	if err != nil {
		return terminalError("terminal_reconnect_unavailable", err)
	}
	epoch, err := terminalEpoch()
	if err != nil {
		return terminalError("terminal_epoch_unavailable", err)
	}
	t := &tmuxTerminalState{root: rec.Root, socketDir: rec.SocketDir, socket: filepath.Join(rec.SocketDir, "socket"),
		name: rec.Name, pane: rec.Pane, columns: rec.Columns, rows: rec.Rows, epoch: epoch, recovered: true,
		scoped: s.terminalBinary == ""}
	binary, err := s.resolveTmuxBinary()
	if err != nil {
		return terminalError("tmux_untrusted", err)
	}
	id, err := s.startProcessSessionWithID(req, "terminal", rec.Workspace, binary, t, rec.ID,
		"-f", "/dev/null", "-S", t.socket, "-C", "attach-session", "-t", rec.Name)
	if err != nil {
		if id != "" {
			return Response{SessionID: id, SessionEpoch: epoch, Error: "terminal reconnect outcome uncertain: " + err.Error(), ErrorCode: "terminal_reconnect_unknown", ErrorClass: "state"}
		}
		return terminalError("terminal_reconnect_failed", err)
	}
	e, err := s.sessions.get(req, id)
	if err != nil {
		return terminalError("terminal_reconnect_unknown", err)
	}
	deadline := time.Now().Add(terminalCommandTimeout)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		ready := t.ack > 0 && !e.exited && t.protocolError == ""
		e.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// tmux may attach to an idle shell without emitting a new %output.
	// Verify pane identity via its structured Control Mode command reply.
	paneErr := s.terminalCommand(req, e, "list-panes -t "+rec.Name+" -F '#{pane_id} #{window_width} #{window_height}'")
	e.mu.Lock()
	paneReply := strings.TrimSpace(t.lastReply)
	found := false
	for _, line := range strings.Split(paneReply, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != rec.Pane {
			continue
		}
		w, err1 := strconv.Atoi(fields[1])
		h, err2 := strconv.Atoi(fields[2])
		if err1 == nil && err2 == nil && w >= 20 && w <= 240 && h >= 5 && h <= 80 {
			t.columns = w
			t.rows = h
			found = true
		}
	}
	match := paneErr == nil && found && t.pane == rec.Pane && !e.exited && t.protocolError == ""
	e.mu.Unlock()
	if !match {
		_ = s.sessions.removeAndStop(e)
		return terminalError("terminal_backend_identity_changed", errors.New("backend pane is absent, stale, or replaced"))
	}
	if err := s.verifyTerminalGeneration(req, e, rec.Name); err != nil {
		_ = s.sessions.removeAndStop(e)
		return terminalError("terminal_backend_identity_changed", err)
	}
	e.mu.Lock()
	if e.expiry != nil {
		e.expiry.Stop()
	}
	remaining := time.Until(rec.ExpiresAt)
	if remaining < time.Second {
		remaining = time.Second
	}
	e.expiry = time.AfterFunc(remaining, func() { s.expireProcessSession(e) })
	e.mu.Unlock()
	return Response{OK: true, Status: "recovered_previous_output_unavailable", SessionID: rec.ID, SessionEpoch: epoch,
		Running: boolPtr(true), RetentionTruncated: true, Rows: t.rows, Columns: t.columns}
}

func (s *Server) verifyTerminalGeneration(req Request, e *stdioSession, name string) error {
	if err := s.terminalCommand(req, e, "show-options -qv -t "+name+" @asa_generation"); err != nil {
		return err
	}
	e.mu.Lock()
	reply := strings.TrimSpace(e.terminal.lastReply)
	e.mu.Unlock()
	if reply != name {
		return fmt.Errorf("tmux generation mismatch")
	}
	return nil
}
