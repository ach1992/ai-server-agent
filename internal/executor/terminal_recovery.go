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
func (s *Server) terminalRecordCount() (int, error) {
	dir, err := s.terminalRecordsDir()
	if err != nil {
		return 0, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".json") {
			count++
		}
	}
	return count, nil
}
func (s *Server) persistTerminalRecord(req Request, e *stdioSession) error {
	e.mu.Lock()
	t := e.terminal
	rec := terminalRecord{Version: 1, ID: e.id, OwnerID: e.ownerID, OwnerClass: e.ownerClass,
		Workspace: e.workspace, Root: t.root, SocketDir: t.socketDir, Name: t.name,
		Pane: t.pane, Columns: t.columns, Rows: t.rows, ExpiresAt: time.Now().Add(maxStdioSessionAge)}
	e.mu.Unlock()
	path, err := s.terminalRecordPath(e.id)
	if err != nil {
		return err
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".terminal-pending-")
	if err != nil {
		return err
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
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Link is no-clobber: a reused/stale opaque ID must never overwrite an
	// older runtime record, and a worker cannot write this directory.
	if err = os.Link(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *Server) readTerminalRecord(req Request) (terminalRecord, error) {
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
	if rec.Version != 1 || rec.ID != req.SessionID || rec.OwnerID != req.PrincipalID || rec.OwnerClass != req.PrincipalClass || rec.Workspace != filepath.Clean(req.Workspace) || rec.Root != req.Root || req.Approval != req.Root || !validTmuxSessionName(rec.Name) || !validTerminalID(rec.ID) || rec.Pane == "" || rec.Columns < 20 || rec.Columns > 240 || rec.Rows < 5 || rec.Rows > 80 {
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

// Reattach a verified tmux backend without creating a new shell/pane. An old
// output epoch cannot silently refer to a new in-memory bounded event stream.
func (s *Server) terminalReconnect(req Request) Response {
	if !validTerminalID(req.SessionID) {
		return terminalError("session_not_found", errSessionNotFound)
	}
	if current, err := s.sessions.get(req, req.SessionID); err == nil {
		current.mu.Lock()
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
	if err != nil {
		return terminalError("terminal_reconnect_unavailable", err)
	}
	epoch, err := terminalEpoch()
	if err != nil {
		return terminalError("terminal_epoch_unavailable", err)
	}
	t := &tmuxTerminalState{root: rec.Root, socketDir: rec.SocketDir, socket: filepath.Join(rec.SocketDir, "socket"),
		name: rec.Name, pane: rec.Pane, columns: rec.Columns, rows: rec.Rows, epoch: epoch, recovered: true}
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
