package executor

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

// FULL authenticated terminal_open and terminal_close path with deterministic
// systemd fault injection. Does NOT launch real scope or restart installed
// services; root gating preserves the actual production authority checks.
func TestApprovedR1ScopeUncertainFullOpenRetainsBrokerIdentity(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host != "CLY535343" || os.Geteuid() != 0 ||
		os.Getenv("ASA_PTY_SCOPE_APPROVED_HOST") != host {
		t.Skip("explicit disposable root environment required")
	}
	account, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		runFailure bool
	}{
		{name: "systemd_run_may_have_created_scope", runFailure: true},
		{name: "systemd_scope_verification_unavailable", runFailure: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "asa-pty-r1-fault-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(dir, "state")
			if err := os.Mkdir(state, 0711); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(state, "worker-home")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(home, uid, gid); err != nil {
				t.Fatal(err)
			}
			project := filepath.Join(dir, "project")
			if err := os.Mkdir(project, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(project, uid, gid); err != nil {
				t.Fatal(err)
			}
			s := &Server{
				cfg:       config.Config{WorkspaceDir: project, StateDir: state, WorkerUser: "aiworker"},
				workerUID: uint32(uid), workerGID: uint32(gid),
				sessions: newStdioSessionBroker(), audit: audit.New(filepath.Join(state, "audit.jsonl")),
			}
			var exactName string
			s.terminalScopeStartTestHook = func(_ Request, _ string, term *tmuxTerminalState, _ []string) error {
				exactName = term.name
				return startTerminalScopeLifecycle(term.name, func() error {
					if tc.runFailure {
						return errors.New("deterministic may-have-created scope")
					}
					return nil
				}, func() error {
					if tc.runFailure {
						t.Error("policy verifier called after failed systemd-run")
					}
					return errors.New("deterministic scope verification failure")
				}, func(name string) error {
					if name != term.name {
						t.Errorf("cleanup identity mismatch %s != %s", name, term.name)
					}
					return errors.New("simulated cleanup verification failure")
				})
			}
			req := Request{
				Action: "terminal_open", PrincipalID: "r1-owner",
				PrincipalClass: "direct", Workspace: project,
				Columns: 80, Rows: 24, RequestID: "r1-fault-full-pipeline",
			}
			opened := s.terminalAction(req)
			if opened.OK || opened.ErrorCode != "terminal_start_uncertain" ||
				!validTerminalID(opened.SessionID) || opened.SessionEpoch == "" {
				t.Fatalf("uncertain initial scope became clean failure: %+v", opened)
			}
			req.SessionID, req.SessionEpoch = opened.SessionID, opened.SessionEpoch
			entry, err := s.sessions.get(req, opened.SessionID)
			if err != nil || entry.terminal.name != exactName ||
				!entry.terminal.scopeUncertain || s.sessions.unpersistedTerminalCount() != 0 {
				t.Fatalf("lost real broker reservation: entry=%+v err=%v", entry, err)
			}
			wrong := req
			wrong.PrincipalID = "foreign-owner"
			wrong.Action = "terminal_close"
			if got := s.terminalAction(wrong); got.OK || got.ErrorCode != "session_not_found" {
				t.Fatalf("other principal closed unknown scope: %+v", got)
			}
			req.Action = "terminal_reconnect"
			if got := s.terminalAction(req); got.OK || got.ErrorCode != "terminal_reconnect_unknown" ||
				got.SessionID != req.SessionID {
				t.Fatalf("unverified startup incorrectly reconciled by reconnect: %+v", got)
			}
			req.Action = "terminal_write"
			req.Content = "id\n"
			if got := s.terminalAction(req); got.OK || got.ErrorCode != "terminal_start_uncertain" {
				t.Fatalf("command allowed before verified cleanup: %+v", got)
			}
			stopCalls := 0
			s.terminalScopeStopTestHook = func(name string) error {
				stopCalls++
				if name != exactName {
					t.Errorf("stop targeted another scope: %q", name)
				}
				if stopCalls == 1 {
					return errors.New("scope shutdown still not proven")
				}
				return nil
			}
			req.Action = "terminal_close"
			req.Content = ""
			first := s.terminalAction(req)
			if first.OK || first.ErrorCode != "terminal_control_unknown" || first.SessionID != req.SessionID {
				t.Fatalf("failed stop erased reconciliation ID: %+v", first)
			}
			if _, err := s.sessions.get(req, req.SessionID); err != nil ||
				s.sessions.unpersistedTerminalCount() != 0 {
				t.Fatalf("failed stop freed scope ownership: %v", err)
			}
			if _, err := s.readOwnedTerminalRecord(req, true); err != nil {
				t.Fatalf("failed stop lost durable PENDING record: %v", err)
			}
			if closed := s.terminalAction(req); !closed.OK || stopCalls != 2 {
				t.Fatalf("repaired cleanup did not release exact scope: %+v stops=%d", closed, stopCalls)
			}
			if _, err := s.sessions.get(req, req.SessionID); !errors.Is(err, errSessionNotFound) ||
				s.sessions.unpersistedTerminalCount() != 0 {
				t.Fatalf("verified scope stop failed to release session: %v", err)
			}
		})
	}
}
