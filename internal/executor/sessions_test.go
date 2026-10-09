package executor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
)

func testStdioBroker(t *testing.T) (*Server, Request, string) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "worktree")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:       config.Config{WorkspaceDir: root, StateDir: filepath.Join(root, "state")},
		workerUID: uint32(os.Geteuid()), workerGID: uint32(os.Getegid()),
		sessions: newStdioSessionBroker(),
	}
	return s, Request{PrincipalID: "direct-default", PrincipalClass: "direct", RequestID: "test-session", Workspace: workspace}, workspace
}

func waitForStdioExit(t *testing.T, e *stdioSession) {
	t.Helper()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not exit")
	}
}

func TestWorkerStdioSessionLifecycleAndPrincipalIsolation(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	id, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer s.sessions.close(owner, id)
	if !strings.HasPrefix(id, "stdio_") || strings.Contains(id, "/") {
		t.Fatalf("unexpected session identity %q", id)
	}
	other := owner
	other.PrincipalID = "mcp-gateway"
	if err := s.sessions.write(other, id, []byte("secret")); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("different principal unexpectedly wrote to session: %v", err)
	}
	if _, err := s.sessions.read(other, id, 0, 4096); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("different principal unexpectedly read session: %v", err)
	}
	if err := s.sessions.close(other, id); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("different principal unexpectedly closed session: %v", err)
	}
	if err := s.workerStdioWrite(owner, id, []byte("hello-session\n")); err != nil {
		t.Fatal(err)
	}
	var got stdioSessionRead
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err = s.sessions.read(owner, id, 0, maxStdioOutputBytes)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Events) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got.Events) != 1 || string(got.Events[0].Data) != "hello-session\n" || got.Truncated || !got.Running {
		t.Fatalf("unexpected read: %+v", got)
	}
	after := got.Events[0].Sequence
	if next, err := s.sessions.read(owner, id, after, 4096); err != nil || len(next.Events) != 0 {
		t.Fatalf("cursor repeated events: %+v, %v", next, err)
	}
	if err := s.workerStdioClose(owner, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sessions.read(owner, id, 0, 4096); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("closed session remained addressable: %v", err)
	}
}

func TestWorkerStdioSessionExitAndBoundedOutput(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	id, err := s.workerStdioSession(owner, "dap", workspace, "/bin/sh", "-c", "head -c 262144 /dev/zero; exit 23")
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := s.sessions.get(owner, id)
	defer s.sessions.close(owner, id)
	waitForStdioExit(t, entry)
	read, err := s.sessions.read(owner, id, 0, maxStdioOutputBytes)
	if err != nil {
		t.Fatal(err)
	}
	if read.Running || read.ExitCode != 23 || !read.Truncated || read.Earliest < 2 || read.Latest < read.Earliest {
		t.Fatalf("bad exited/truncated state: %+v", read)
	}
	if read.BytesRetained > maxStdioOutputBytes || len(read.Events) > maxStdioOutputEvents {
		t.Fatalf("unbounded session output: %+v", read)
	}
	var last uint64
	for _, event := range read.Events {
		if event.Sequence <= last || len(event.Data) > maxStdioEventBytes {
			t.Fatalf("out of order or unbounded event: %+v", event)
		}
		last = event.Sequence
	}
	if err := s.sessions.write(owner, id, []byte("after-exit")); !errors.Is(err, errSessionStopped) {
		t.Fatalf("write to exited process: %v", err)
	}
}

func TestWorkerStdioSessionCapacityAndIdentity(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	var ids []string
	for i := 0; i < maxStdioSessions; i++ {
		id, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/cat")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	defer func() {
		for _, id := range ids {
			_ = s.sessions.close(owner, id)
		}
	}()
	if _, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/cat"); !errors.Is(err, errSessionBusy) {
		t.Fatalf("capacity should fail without queuing, got %v", err)
	}
	if err := s.sessions.close(owner, ids[0]); err != nil {
		t.Fatal(err)
	}
	id, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/cat")
	if err != nil {
		t.Fatalf("closed capacity slot not reclaimed: %v", err)
	}
	ids = append(ids, id)
	if id == ids[0] {
		t.Fatal("new session reused stale opaque identity")
	}
	invalid := owner
	invalid.Root = true
	if _, err := s.workerStdioSession(invalid, "dap", workspace, "/bin/cat"); err == nil {
		t.Fatal("unapproved root creation accepted")
	}
	if _, err := s.workerStdioSession(owner, "generic-process", workspace, "/bin/cat"); err == nil {
		t.Fatal("generic process kind accepted")
	}
	if _, err := s.workerStdioSession(owner, "lsp", "../outside", "/bin/cat"); err == nil {
		t.Fatal("workspace escape accepted")
	}
}

func TestWorkerStdioSessionSecretsAndInputBounds(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	t.Setenv("UNTRUSTED_EXECUTOR_SECRET", "must-not-be-inherited")
	id, err := s.workerStdioSession(owner, "lsp", workspace, "/usr/bin/env")
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := s.sessions.get(owner, id)
	defer s.sessions.close(owner, id)
	waitForStdioExit(t, entry)
	read, err := s.sessions.read(owner, id, 0, maxStdioOutputBytes)
	if err != nil {
		t.Fatal(err)
	}
	var content []byte
	for _, event := range read.Events {
		content = append(content, event.Data...)
	}
	if bytes.Contains(content, []byte("UNTRUSTED_EXECUTOR_SECRET")) || !bytes.Contains(content, []byte("HOME="+s.cfg.WorkspaceDir)) {
		t.Fatalf("session inherited unsafe env or wrong HOME: %q", content)
	}
	id2, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer s.sessions.close(owner, id2)
	if err := s.sessions.write(owner, id2, make([]byte, maxStdioInputBytes+1)); err == nil {
		t.Fatal("oversize input accepted")
	}
	if _, err := s.sessions.read(owner, id2, 0, maxStdioOutputBytes+1); err == nil {
		t.Fatal("oversize read accepted")
	}
}

func TestWorkerStdioSessionAuditBeforeSpawn(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	s.audit = audit.New(filepath.Join(t.TempDir(), "audit.jsonl"))
	s.auditWriteHook = func(phase string) error { return errors.New("disk full") }
	if _, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/cat"); err == nil {
		t.Fatal("required pre-action audit failure permitted subprocess spawn")
	}
	s.sessions.mu.Lock()
	count := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if count != 0 {
		t.Fatalf("failed creation leaked session reservation: %d", count)
	}
}

func TestWorkerStdioSessionWriteAndCloseRequireAudit(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	s.audit = audit.New(filepath.Join(t.TempDir(), "audit.jsonl"))
	id, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer s.sessions.close(owner, id)
	s.auditWriteHook = func(string) error { return errors.New("simulated audit failure") }
	if err := s.workerStdioWrite(owner, id, []byte("should-not-arrive\n")); err == nil {
		t.Fatal("input was allowed without durable pre-action audit")
	}
	if err := s.workerStdioClose(owner, id); err == nil {
		t.Fatal("control operation was allowed without durable pre-action audit")
	}
	s.auditWriteHook = nil
	if err := s.workerStdioWrite(owner, id, []byte("allowed\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		read, err := s.sessions.read(owner, id, 0, maxStdioOutputBytes)
		if err != nil {
			t.Fatal(err)
		}
		if len(read.Events) > 0 {
			if len(read.Events) != 1 || string(read.Events[0].Data) != "allowed\n" {
				t.Fatalf("rejected input reached subprocess: %+v", read)
			}
			found = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !found {
		t.Fatal("audited input never appeared in subprocess output")
	}
	if err := s.workerStdioClose(owner, id); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerStdioSessionFailedStopRetainsIdentityForReconciliation(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	entry := &stdioSession{id: "stdio_incomplete", ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass, workspace: workspace, done: make(chan struct{}), exitCode: -1}
	if err := s.sessions.reserve(entry); err != nil {
		t.Fatal(err)
	}
	if err := s.sessions.close(owner, entry.id); err == nil || !strings.Contains(err.Error(), "outcome_unknown") {
		t.Fatalf("unverified stop was hidden: %v", err)
	}
	if _, err := s.sessions.get(owner, entry.id); err != nil {
		t.Fatalf("unverified backend lost its recoverable identity: %v", err)
	}
	entry.mu.Lock()
	entry.exited = true
	entry.completed = time.Now()
	entry.mu.Unlock()
	if err := s.sessions.close(owner, entry.id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sessions.get(owner, entry.id); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("verified cleanup did not release session: %v", err)
	}
}

func TestWorkerStdioSessionReadCursorNeverSkipsEarlierEvent(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	entry := &stdioSession{id: "stdio_cursor", ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass, workspace: workspace}
	if err := s.sessions.reserve(entry); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.remove(entry)
	writer := sessionEventWriter{entry: entry, stream: "stdout"}
	if _, err := writer.Write(make([]byte, maxStdioEventBytes*2+100)); err != nil {
		t.Fatal(err)
	}
	first, err := s.sessions.read(owner, entry.id, 0, maxStdioEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 || first.Events[0].Sequence != 1 {
		t.Fatalf("first cursor read invalid: %+v", first)
	}
	second, err := s.sessions.read(owner, entry.id, 1, maxStdioEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].Sequence != 2 {
		t.Fatalf("event skipped by read limit: %+v", second)
	}
	if _, err := s.sessions.read(owner, entry.id, first.Latest+1, maxStdioEventBytes); !errors.Is(err, errSessionCursor) {
		t.Fatalf("future cursor must fail rather than appear caught up: %v", err)
	}
}

// A language server can exit while helpers it launched are still running in
// its process group and have closed all pipes. Wait() alone is not cleanup.
func TestWorkerStdioSessionExitStopsSameGroupDescendants(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	marker := filepath.Join(workspace, "orphan-wrote-file")
	command := "(sleep 0.75; printf orphan > " + shellQuote(marker) + ") >/dev/null 2>&1 & exit 0"
	id, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/sh", "-c", command)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.sessions.get(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.sessions.close(owner, id)
	waitForStdioExit(t, entry)
	entry.mu.Lock()
	cleanupErr, pidPinned := entry.cleanupErr, entry.pidPin != nil
	entry.mu.Unlock()
	if cleanupErr != nil || pidPinned {
		t.Fatalf("normal process exit did not release pinned group identity: err=%v pinned=%t", cleanupErr, pidPinned)
	}
	// Any orphaned same-group helper would write the marker after exit.
	time.Sleep(900 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("same-group descendant survived parent exit: %v", err)
	}
}

// Unknown cleanup cannot be turned into a recovered/expired session merely
// to free admission capacity; the pinned process identity must be retained
// until reconciliation succeeds.
func TestStdioSessionFailedGroupCleanupSurvivesRetentionPrune(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	entry := &stdioSession{
		id: "stdio_uncertain", ownerID: owner.PrincipalID,
		ownerClass: owner.PrincipalClass, workspace: workspace,
		exited: true, completed: time.Now().Add(-2 * completedSessionRetain),
		cleanupErr: errors.New("group cleanup not verified"),
	}
	if err := s.sessions.reserve(entry); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.remove(entry)
	second := &stdioSession{id: "stdio_new", ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass, workspace: workspace}
	if err := s.sessions.reserve(second); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.remove(second)
	if _, err := s.sessions.get(owner, entry.id); err != nil {
		t.Fatalf("expired but unreconciled session was lost: %v", err)
	}
	if err := s.sessions.close(owner, entry.id); err == nil || !strings.Contains(err.Error(), "outcome_unknown") {
		t.Fatalf("uncertain cleanup was reported as complete: %v", err)
	}
	if _, err := s.sessions.get(owner, entry.id); err != nil {
		t.Fatalf("unknown-outcome session identity was removed: %v", err)
	}
}

func TestStdioSessionIDDoesNotSurviveBrokerRestart(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	id, err := s.workerStdioSession(owner, "lsp", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer s.sessions.close(owner, id)
	// In-memory stdio sessions are not durable; a new executor generation
	// must never silently map a previous ID to any different process.
	restarted := newStdioSessionBroker()
	if _, err := restarted.read(owner, id, 0, maxStdioEventBytes); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("restarted broker accepted stale session identity: %v", err)
	}
}
