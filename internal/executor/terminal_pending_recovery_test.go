package executor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
)

func pendingRecoveryFixture(t *testing.T, privileged bool) (*Server, Request, *stdioSession) {
	t.Helper()
	s, req, _ := testStdioBroker(t)
	// The Unix domain socket address has a strict Linux 107-byte limit.
	// An intentionally short private test state path still exercises all
	// the production namespace checks, without touching installed state.
	state, err := os.MkdirTemp("/tmp", "asa-pending-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	s.cfg.StateDir = state
	s.audit = audit.New(filepath.Join(state, "audit.jsonl"))
	req.Root, req.Approval = privileged, privileged
	req.SessionID = "stdio_1234567890abcdef1234567890abcdef"
	req.SessionEpoch = "0123456789abcdef01234567"
	req.Action = "terminal_open"
	folder := "worker-terminals"
	if privileged {
		folder = "root-terminals"
	}
	socketDir := filepath.Join(state, folder, ".asa-tmux-pending")
	term := &tmuxTerminalState{
		root: privileged, scoped: true, name: "asa_aabbccddeeff001122334455",
		socketDir: socketDir, socket: filepath.Join(socketDir, "socket"),
		epoch: req.SessionEpoch, columns: 80, rows: 24,
	}
	entry := &stdioSession{
		id: req.SessionID, kind: "terminal", ownerID: req.PrincipalID,
		ownerClass: req.PrincipalClass, workspace: req.Workspace,
		terminal: term, done: make(chan struct{}),
	}
	if err := s.sessions.reserve(entry); err != nil {
		t.Fatal(err)
	}
	linked, err := s.persistPendingTerminalRecord(entry)
	if err != nil || !linked {
		t.Fatalf("PENDING was not durably created: %v (%t)", err, linked)
	}
	entry.terminalRecordPersisted = true
	return s, req, entry
}

func TestPendingScopeCrashRecoveryFreshServer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		runUnknown bool
		privileged bool
	}{
		{name: "systemd_run_may_have_created", runUnknown: true},
		{name: "policy_verification_failed", runUnknown: false},
		{name: "root_authorization_after_restart", runUnknown: true, privileged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, req, entry := pendingRecoveryFixture(t, tc.privileged)
			name := entry.terminal.name
			invoked, verified := 0, 0
			lifecycleErr := startTerminalScopeLifecycle(name, func() error {
				invoked++
				if _, err := original.readOwnedTerminalRecord(req, true); err != nil {
					t.Fatalf("external scope launch preceded durable ownership: %v", err)
				}
				if tc.runUnknown {
					return errors.New("launch outcome unknown")
				}
				return nil
			}, func() error {
				verified++
				return errors.New("PID1 scope policy unverifiable")
			}, func(unit string) error {
				if unit != name {
					t.Fatalf("stop targeted another scope: %s", unit)
				}
				return errors.New("initial exact stop unverifiable")
			})
			if !preserveUncertainTerminalStart(entry, lifecycleErr) || invoked != 1 ||
				verified != map[bool]int{true: 0, false: 1}[tc.runUnknown] {
				t.Fatalf("wrong pending setup: run=%d verify=%d err=%v", invoked, verified, lifecycleErr)
			}
			// Simulate FULL Executor/Broker loss: no old process, map or
			// in-memory capacity can participate in any of these assertions.
			restarted := &Server{
				cfg: original.cfg, workerUID: original.workerUID, workerGID: original.workerGID,
				sessions: newStdioSessionBroker(), audit: original.audit,
			}
			for n := 1; n < maxStdioSessions; n++ {
				admissionRecordForTest(t, restarted, n, time.Now().Add(time.Hour))
			}
			if count, err := restarted.terminalRecordCountWithStop(func(string) error {
				return errors.New("unexpected expiration")
			}); err != nil || count != maxStdioSessions {
				t.Fatalf("pending slot lost on restart: %d (%v)", count, err)
			}
			req.Action = "terminal_open"
			req.Columns, req.Rows = 80, 24
			req.SessionID, req.SessionEpoch = "", ""
			if got := restarted.terminalAction(req); got.ErrorCode != "terminal_resource_limit" {
				t.Fatalf("restart allowed ninth scope: %+v", got)
			}
			req.SessionID = entry.id
			req.SessionEpoch = entry.terminal.epoch
			// The recovered pending owner cannot acquire any terminal I/O.
			for _, action := range []string{"terminal_read", "terminal_write", "terminal_interrupt", "terminal_resize", "terminal_reconnect"} {
				req.Action = action
				req.Content = "not allowed\n"
				req.Columns, req.Rows = 80, 24
				got := restarted.terminalAction(req)
				if got.OK || got.SessionID != entry.id ||
					(got.ErrorCode != "terminal_start_uncertain" && got.ErrorCode != "terminal_reconnect_unknown") {
					t.Fatalf("restarted pending allowed %s: %+v", action, got)
				}
			}
			req.Action, req.Content = "terminal_close", ""
			for _, change := range []func(*Request){
				func(r *Request) { r.PrincipalID = "foreign" },
				func(r *Request) { r.PrincipalClass = "other" },
				func(r *Request) { r.Workspace = filepath.Dir(r.Workspace) },
				func(r *Request) { r.Root = !r.Root; r.Approval = r.Root },
			} {
				foreign := req
				change(&foreign)
				if got := restarted.terminalAction(foreign); got.OK || got.ErrorCode != "session_not_found" {
					t.Fatalf("foreign authority discovered/closed pending scope: %+v", got)
				}
			}
			stale := req
			stale.SessionEpoch = "abcdef0123456789abcdef01"
			if got := restarted.terminalAction(stale); got.OK || got.ErrorCode != "terminal_epoch_changed" {
				t.Fatalf("stale pending epoch was accepted: %+v", got)
			}
			attempts := 0
			restarted.terminalScopeStopTestHook = func(unit string) error {
				attempts++
				if unit != name {
					t.Fatalf("wrong scope unit: %s", unit)
				}
				if attempts == 1 {
					return errors.New("PID1 stop not yet proven")
				}
				return nil
			}
			if got := restarted.terminalAction(req); got.OK || got.ErrorCode != "terminal_control_unknown" {
				t.Fatalf("failed stop incorrectly released pending: %+v", got)
			}
			if _, err := restarted.readOwnedTerminalRecord(req, true); err != nil {
				t.Fatalf("failed stop lost on-disk owner: %v", err)
			}
			if got := restarted.terminalAction(req); !got.OK || attempts != 2 {
				t.Fatalf("retry did not reconcile exact old scope: %+v, stops=%d", got, attempts)
			}
			if _, err := restarted.readOwnedTerminalRecord(req, true); !errors.Is(err, errSessionNotFound) {
				t.Fatalf("cleaned pending still exists: %v", err)
			}
			count, err := restarted.terminalRecordCountWithStop(func(string) error { return errors.New("unexpected stop") })
			if err != nil || count != maxStdioSessions-1 {
				t.Fatalf("capacity was not restored after safe stop: %d (%v)", count, err)
			}
			launched := 0
			got := restarted.withTerminalAdmissionStop(func() Response {
				launched++
				return Response{OK: true}
			}, func(string) error { return errors.New("unexpected expire") })
			if !got.OK || launched != 1 {
				t.Fatalf("released slot was not reusable: %+v", got)
			}
		})
	}
}

func TestPreActiveScopeCrashAndAtomicPromotion(t *testing.T) {
	t.Run("verified_scope_but_control_publication_failed", func(t *testing.T) {
		s, req, e := pendingRecoveryFixture(t, false)
		// PID1 launch/verification succeeds, but before a verified pane
		// can be published the Executor crashes. No ACTIVE record exists.
		if err := startTerminalScopeLifecycle(e.terminal.name,
			func() error { return nil }, func() error { return nil },
			func(string) error { return errors.New("not expected") }); err != nil {
			t.Fatal(err)
		}
		reboot := &Server{cfg: s.cfg, sessions: newStdioSessionBroker(), audit: s.audit}
		rec, err := reboot.readOwnedTerminalRecord(req, true)
		if err != nil || rec.Lifecycle != "pending" {
			t.Fatalf("pre-ACTIVE scope lost: %+v %v", rec, err)
		}
		reboot.terminalScopeStopTestHook = func(unit string) error {
			if unit != rec.Name {
				t.Fatalf("wrong scope: %s", unit)
			}
			return nil
		}
		req.Action = "terminal_close"
		if result := reboot.terminalAction(req); !result.OK {
			t.Fatalf("pre-ACTIVE scope cannot be cleaned after restart: %+v", result)
		}
	})
	t.Run("verified_pane_promotes_exact_reservation", func(t *testing.T) {
		s, req, e := pendingRecoveryFixture(t, false)
		old, err := s.readOwnedTerminalRecord(req, true)
		if err != nil {
			t.Fatal(err)
		}
		e.terminal.pane = "%0"
		if err := s.persistTerminalRecord(req, e); err != nil {
			t.Fatal(err)
		}
		path, err := s.terminalRecordPath(req.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var current terminalRecord
		if err := json.Unmarshal(body, &current); err != nil {
			t.Fatal(err)
		}
		if current.Lifecycle != "active" || current.Pane != "%0" ||
			current.Name != old.Name || !current.ExpiresAt.Equal(old.ExpiresAt) ||
			current.OwnerID != old.OwnerID || current.SocketDir != old.SocketDir {
			t.Fatalf("promotion changed ownership or expiry: %+v", current)
		}
		if _, err := s.readOwnedTerminalRecord(req, true); !errors.Is(err, errSessionNotFound) {
			t.Fatalf("ACTIVE incorrectly reentered pending: %v", err)
		}
		if err := s.persistTerminalRecord(req, e); err == nil {
			t.Fatal("second promotion clobbered existing ACTIVE record")
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil || len(entries) != 1 {
			t.Fatalf("non-atomic state or temp leak: %d, %v", len(entries), err)
		}
	})
}

// The expiry reaper never frees an uncertain scope merely because its one-
// hour deadline elapsed. Both open admission and explicitly authorized close
// must preserve the durable owner until PID1 has verified the exact stop.
func TestPendingScopeExpiryAndConcurrentAdmission(t *testing.T) {
	s, req, entry := pendingRecoveryFixture(t, false)
	path, err := s.terminalRecordPath(req.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec terminalRecord
	if err = json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	rec.ExpiresAt = time.Now().Add(-time.Minute)
	expired, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, expired, 0600); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	s.terminalScopeStopTestHook = func(name string) error {
		attempts++
		if name != entry.terminal.name {
			t.Fatalf("wrong exact scope %q", name)
		}
		if attempts == 1 {
			return errors.New("PID1 stop unverified")
		}
		return nil
	}
	// An expired uncertain reservation must block new admission until its
	// exact scope can be stopped and the protected file fsynced deleted.
	got := s.withTerminalAdmissionStop(func() Response {
		return Response{OK: true}
	}, s.terminalScopeStopTestHook)
	if got.OK || got.ErrorCode != "terminal_registry_unavailable" || attempts != 1 {
		t.Fatalf("expired unknown stop was falsely reclaimed: %+v, calls=%d", got, attempts)
	}
	if _, err = s.readOwnedTerminalRecord(req, true); err != nil {
		t.Fatalf("expired PENDING lost without safe cleanup: %v", err)
	}
	// An ordinary synchronized admission can reap it after verified stop.
	got = s.withTerminalAdmissionStop(func() Response {
		return Response{OK: true}
	}, s.terminalScopeStopTestHook)
	if !got.OK || attempts != 2 {
		t.Fatalf("verified expired cleanup did not restore capacity: %+v, calls=%d", got, attempts)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired record still exists or stat failed: %v", err)
	}
}

func TestPendingCloseVsAdmissionSerialization(t *testing.T) {
	s, req, entry := pendingRecoveryFixture(t, false)
	// A real uncertain launch has already marked the in-memory terminal as
	// cleanup-only. Do not exercise ordinary Control Mode on a test fixture
	// that deliberately has no live tmux client.
	entry.mu.Lock()
	entry.terminal.scopeUncertain = true
	entry.mu.Unlock()
	req.Action = "terminal_close"
	started := make(chan struct{})
	release := make(chan struct{})
	s.terminalScopeStopTestHook = func(name string) error {
		if name != entry.terminal.name {
			t.Errorf("wrong scope: %s", name)
		}
		close(started)
		<-release
		return nil
	}
	stopResult := make(chan Response, 1)
	go func() { stopResult <- s.terminalAction(req) }()
	<-started
	admitted := make(chan Response, 1)
	go func() {
		admitted <- s.withTerminalAdmissionStop(func() Response { return Response{OK: true} },
			func(string) error { return errors.New("unexpected expiry") })
	}()
	close(release)
	if result := <-stopResult; !result.OK {
		t.Fatalf("authorized exact close failed: %+v", result)
	}
	if result := <-admitted; !result.OK {
		t.Fatalf("admission remained blocked after verified close: %+v", result)
	}
	if count, err := s.terminalRecordCountWithStop(func(string) error { return errors.New("unexpected cleanup") }); err != nil || count != 0 {
		t.Fatalf("pending close and admission raced: %d, %v", count, err)
	}
}

// A root-controlled directory may be substituted by a symlink after PID1
// terminates. Never follow it or unlink any arbitrary file named socket.
func TestPendingScopeCleanupRejectsSocketSymlink(t *testing.T) {
	s, req, e := pendingRecoveryFixture(t, false)
	dir := e.terminal.socketDir
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	marker := filepath.Join(external, "socket")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, dir); err != nil {
		t.Fatal(err)
	}
	reboot := &Server{cfg: s.cfg, sessions: newStdioSessionBroker(), audit: s.audit}
	reboot.terminalScopeStopTestHook = func(name string) error {
		if name != e.terminal.name {
			t.Errorf("wrong scope %q", name)
		}
		return nil
	}
	req.Action = "terminal_close"
	if result := reboot.terminalAction(req); !result.OK {
		t.Fatalf("scope close failed: %+v", result)
	}
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "preserve" {
		t.Fatalf("followed unexpected symlink: data=%q err=%v", b, err)
	}
}
