package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Deterministic, unprivileged unit coverage for the two HIGH_ASSURANCE
// findings. Real scope/systemd restart regression remains separately gated.
func admissionRecordForTest(t *testing.T, s *Server, n int, expires time.Time) terminalRecord {
	t.Helper()
	dir, err := s.terminalRecordsDir()
	if err != nil {
		t.Fatal(err)
	}
	rec := terminalRecord{
		Version: 1, ID: fmt.Sprintf("stdio_%032x", n),
		Name: fmt.Sprintf("asa_%024x", n), Scoped: true,
		OwnerID: "existing-owner", OwnerClass: "direct", Workspace: s.cfg.WorkspaceDir,
		ExpiresAt: expires,
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rec.ID+".json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestReviewR1UncertainScopeStartupKeepsExactIdentityUntilVerifiedStop(t *testing.T) {
	for _, tc := range []struct {
		name               string
		launchFailed       bool
		verificationFailed bool
	}{
		{name: "run_may_have_created_backend", launchFailed: true},
		{name: "scope_policy_verification_failed", verificationFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, owner, _ := testStdioBroker(t)
			shortState, err := os.MkdirTemp("/tmp", "asa-r1-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(shortState) })
			s.cfg.StateDir = shortState
			owner.Action = "terminal_open"
			owner.SessionID = "stdio_0123456789abcdef0123456789abcdef"
			owner.SessionEpoch = "0123456789abcdef01234567"
			socketDir := filepath.Join(s.cfg.StateDir, "worker-terminals", ".asa-tmux-review")
			term := &tmuxTerminalState{
				name: "asa_0123456789abcdef01234567", epoch: owner.SessionEpoch,
				socketDir: socketDir, socket: filepath.Join(socketDir, "socket"),
				scoped: true, columns: 80, rows: 24,
			}
			entry := &stdioSession{
				id: owner.SessionID, kind: "terminal", ownerID: owner.PrincipalID,
				ownerClass: owner.PrincipalClass, workspace: owner.Workspace,
				terminal: term, done: make(chan struct{}),
			}
			if err := s.sessions.reserve(entry); err != nil {
				t.Fatal(err)
			}
			if linked, err := s.persistPendingTerminalRecord(entry); err != nil || !linked {
				t.Fatalf("durable PENDING must precede scope launch: linked=%t err=%v", linked, err)
			}
			entry.terminalRecordPersisted = true
			launches, verifies, stops := 0, 0, 0
			lifecycleErr := startTerminalScopeLifecycle(term.name, func() error {
				launches++
				if tc.launchFailed {
					return errors.New("injected systemd-run may-have-created")
				}
				return nil
			}, func() error {
				verifies++
				if tc.verificationFailed {
					return errors.New("injected policy check unavailable")
				}
				return nil
			}, func(name string) error {
				stops++
				if name != term.name {
					t.Fatalf("wrong scope identity %q", name)
				}
				return errors.New("injected exact-scope stop not verifiable")
			})
			var unknown *terminalScopeCleanupUncertain
			if !errors.As(lifecycleErr, &unknown) ||
				!preserveUncertainTerminalStart(entry, lifecycleErr) {
				t.Fatalf("failure was normalized into clean startup: %v", lifecycleErr)
			}
			if launches != 1 || stops != 1 || (tc.launchFailed && verifies != 0) ||
				(tc.verificationFailed && verifies != 1) {
				t.Fatalf("incorrect two-stage fault path: run=%d verify=%d stop=%d", launches, verifies, stops)
			}
			if got, err := s.sessions.get(owner, entry.id); err != nil || got != entry {
				t.Fatalf("lost principal-bound reconciliation ID: %v", err)
			}
			if count, err := s.terminalRecordCountWithStop(func(string) error { return errors.New("unexpected stop") }); err != nil || count != 1 {
				t.Fatalf("durable PENDING did not occupy terminal capacity: %d (%v)", count, err)
			}
			if s.sessions.unpersistedTerminalCount() != 0 {
				t.Fatal("PENDING counted twice in disk and broker capacity")
			}
			wrong := owner
			wrong.PrincipalID = "another-principal"
			if _, err := s.sessions.get(wrong, entry.id); !errors.Is(err, errSessionNotFound) {
				t.Fatalf("other principal stole uncertain scope: %v", err)
			}
			owner.Action = "terminal_reconnect"
			if got := s.terminalAction(owner); got.OK || got.ErrorCode != "terminal_reconnect_unknown" ||
				got.SessionID != entry.id {
				t.Fatalf("uncertain scope treated as safe reconnect: %+v", got)
			}
			owner.Action = "terminal_read"
			if got := s.terminalAction(owner); got.OK || got.ErrorCode != "terminal_start_uncertain" ||
				got.SessionID != entry.id {
				t.Fatalf("uncertain scope treated as live output: %+v", got)
			}
			stopAttempts := 0
			stop := func(name string) error {
				stopAttempts++
				if name != term.name {
					return errors.New("wrong exact scope")
				}
				if stopAttempts == 1 {
					return errors.New("still cannot verify exact scope stop")
				}
				return nil
			}
			if err := s.reconcileUnstartedTerminal(entry, stop); err == nil {
				t.Fatal("unverified cleanup released the broker identity")
			}
			if _, err := s.sessions.get(owner, entry.id); err != nil {
				t.Fatalf("failed cleanup lost identity: %v", err)
			}
			if err := s.reconcileUnstartedTerminal(entry, stop); err != nil {
				t.Fatalf("verified repair did not release exact scope: %v", err)
			}
			if _, err := s.sessions.get(owner, entry.id); !errors.Is(err, errSessionNotFound) ||
				s.sessions.unpersistedTerminalCount() != 0 {
				t.Fatalf("verified cleanup failed to release bounded slot: %v", err)
			}
		})
	}
}

func TestReviewR2FullRecoveredCapacityRejectsBeforeLaunch(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	records := make([]terminalRecord, 0, maxStdioSessions)
	for n := 1; n <= maxStdioSessions; n++ {
		records = append(records, admissionRecordForTest(t, s, n, time.Now().Add(time.Hour)))
	}
	owner.Action, owner.Columns, owner.Rows = "terminal_open", 80, 24
	// The real terminalOpen must fail *before* it attempts a root-controlled
	// socket, tmux, or service/scope launch on this unprivileged CI fixture.
	r := s.terminalAction(owner)
	if r.OK || r.ErrorCode != "terminal_resource_limit" {
		t.Fatalf("full recovered registry failed to block before launch: %+v", r)
	}
	for _, rec := range records {
		if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "terminal-sessions", rec.ID+".json")); err != nil {
			t.Fatalf("original recovered state mutated: %v", err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "terminal-sessions")); len(entries) != maxStdioSessions {
		t.Fatalf("original recovered identities were not preserved: %d", len(entries))
	}
}

func TestReviewR2ConcurrentAdmissionSerializedAtBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expired bool
	}{
		{name: "seven_live_records"},
		{name: "seven_live_one_expired", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := testStdioBroker(t)
			for n := 1; n <= maxStdioSessions-1; n++ {
				admissionRecordForTest(t, s, n, time.Now().Add(time.Hour))
			}
			var expiredName string
			if tc.expired {
				expired := admissionRecordForTest(t, s, maxStdioSessions+1, time.Now().Add(-time.Second))
				expiredName = expired.Name
			}
			var entered atomic.Int32
			var stopCount atomic.Int32
			stop := func(name string) error {
				if name != expiredName || expiredName == "" {
					return fmt.Errorf("unexpected cleanup target %q", name)
				}
				stopCount.Add(1)
				return nil // deterministic stand-in for verified systemd stop
			}
			var wg sync.WaitGroup
			ch := make(chan Response, 2)
			for j := 0; j < 2; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					res := s.withTerminalAdmissionStop(func() Response {
						entered.Add(1)
						// This callback represents the ENTIRE scope launch
						// and persisted identity under admission lock.
						n := maxStdioSessions + 3
						if tc.expired {
							n++
						}
						id := fmt.Sprintf("stdio_%032x", n)
						record := terminalRecord{
							Version: 1, ID: id, Name: fmt.Sprintf("asa_%024x", n),
							Scoped: true, ExpiresAt: time.Now().Add(time.Hour),
						}
						payload, err := json.Marshal(record)
						if err != nil {
							return Response{Error: err.Error()}
						}
						if err := os.WriteFile(filepath.Join(s.cfg.StateDir, "terminal-sessions", id+".json"), payload, 0600); err != nil {
							return Response{Error: err.Error()}
						}
						time.Sleep(15 * time.Millisecond) // force interleaving pressure
						return Response{OK: true, Status: "running"}
					}, stop)
					ch <- res
				}()
			}
			wg.Wait()
			close(ch)
			pass, rejected := 0, 0
			for res := range ch {
				if res.OK {
					pass++
				} else if res.ErrorCode == "terminal_resource_limit" {
					rejected++
				} else {
					t.Fatalf("unexpected admission result: %+v", res)
				}
			}
			if pass != 1 || rejected != 1 || entered.Load() != 1 {
				t.Fatalf("overadmitted: pass=%d rejected=%d launched=%d", pass, rejected, entered.Load())
			}
			if count, err := s.terminalRecordCountWithStop(stop); err != nil || count != maxStdioSessions {
				t.Fatalf("registry not bounded after concurrent requests: count=%d err=%v", count, err)
			}
			if tc.expired && stopCount.Load() != 1 {
				t.Fatalf("expired exact scope was not reclaimed exactly once: %d", stopCount.Load())
			}
		})
	}
}

func TestReviewR2UnrecordedUncertainSessionCountsAgainstPersistentSlots(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	for n := 1; n < maxStdioSessions; n++ {
		admissionRecordForTest(t, s, n, time.Now().Add(time.Hour))
	}
	entry := &stdioSession{
		id: "stdio_ffffffffffffffffffffffffffffffff", kind: "terminal",
		ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass,
		workspace: owner.Workspace, terminal: &tmuxTerminalState{
			name: "asa_ffffffffffffffffffffffff", scopeUncertain: true,
		},
	}
	if err := s.sessions.reserve(entry); err != nil {
		t.Fatal(err)
	}
	calls := 0
	res := s.withTerminalAdmissionStop(func() Response {
		calls++
		return Response{OK: true}
	}, func(string) error { return errors.New("unexpected cleanup") })
	if res.OK || res.ErrorCode != "terminal_resource_limit" || calls != 0 {
		t.Fatalf("uncertain scope incorrectly considered free capacity: %+v, launches=%d", res, calls)
	}
}
