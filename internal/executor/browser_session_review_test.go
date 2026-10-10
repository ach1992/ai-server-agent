package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
)

// The test-only Node executable override lets these lifecycle faults run in
// normal Go CI, without Chromium downloads or an installed root Agent. It is
// never controllable by public MCP input; production pins a root-owned Node.
func browserAuditFixture(t *testing.T) (*Server, Request, string) {
	t.Helper()
	s, owner, workspace := testStdioBroker(t)
	script := filepath.Join(t.TempDir(), "browser-fixture")
	body := `#!/bin/sh
printf 'ASA_BROWSER_SESSION {"event":"ready"}\n'
while IFS= read -r line; do
  case "$line" in
    *'"type":"close"'*) exit 0 ;;
    *) : ;;
  esac
done
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	s.browserNodeBinary = script
	s.audit = audit.New(filepath.Join(t.TempDir(), "audit.jsonl"))
	owner.Action = "browser_session_open"
	owner.Content = "console.log('bounded test runner source')"
	return s, owner, workspace
}
func browserOwnerControl(owner Request, id, action string) Request {
	owner.Action = action
	owner.SessionID = id
	owner.Content = ""
	return owner
}
func breakAuditCompletion(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".not-found", path); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserSessionCreateAuditDegradedRemainsRecoverable(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	path := filepath.Join(t.TempDir(), "unused")
	// Use the audit logger's actual path from this independent test state.
	// A fault after child spawn must not lose Browser type/reconciliation.
	s.audit = audit.New(path)
	sabotaged := false
	s.auditBeforeCompletionHook = func() {
		if !sabotaged {
			sabotaged = true
			breakAuditCompletion(t, path)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	got := s.browserSessionAction(ctx, owner)
	if got.OK || got.ErrorCode != "browser_session_start_uncertain" || got.SessionID == "" || !sabotaged {
		t.Fatalf("post-spawn audit failure not preserved: %+v", got)
	}
	stat := s.browserSessionAction(ctx, browserOwnerControl(owner, got.SessionID, "browser_session_status"))
	if !stat.OK || stat.Status != "uncertain" {
		t.Fatalf("degraded Browser ID was lost: %+v", stat)
	}
	if s.browserAdmission.acquireRun() {
		t.Fatal("lost-process risk admitted conflicting profile run")
	}
	attacker := browserOwnerControl(owner, got.SessionID, "browser_session_close")
	attacker.PrincipalID = "another-principal"
	if resp := s.browserSessionAction(ctx, attacker); resp.OK || resp.ErrorCode != "browser_session_not_found" {
		t.Fatalf("principal bypassed degraded session: %+v", resp)
	}
	// Reinitialize the fail-closed audit logger only in this synthetic test,
	// simulating a repaired audit path; never bypass authorization to close.
	s.auditBeforeCompletionHook = nil
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.audit = audit.New(path)
	closed := s.browserSessionAction(ctx, browserOwnerControl(owner, got.SessionID, "browser_session_close"))
	if !closed.OK || closed.Status != "closed" {
		t.Fatalf("cannot reconcile degraded create: %+v", closed)
	}
	if !s.browserAdmission.acquireRun() {
		t.Fatal("profile remains reserved after verified recovery")
	}
	s.browserAdmission.releaseRun()
	again := s.browserSessionAction(ctx, owner)
	if !again.OK {
		t.Fatalf("new session cannot start after audited recovery: %+v", again)
	}
	if stop := s.browserSessionAction(ctx, browserOwnerControl(owner, again.SessionID, "browser_session_close")); !stop.OK {
		t.Fatalf("replacement Browser close: %+v", stop)
	}
}

func TestBrowserSessionCloseAuditDegradedDoesNotStrandProfile(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("open failed: %+v", opened)
	}
	id := opened.SessionID
	path := filepath.Join(t.TempDir(), "audit-close")
	s.audit = audit.New(path)
	completeCount := 0
	s.auditBeforeCompletionHook = func() {
		completeCount++
		// close sends one audited graceful-shutdown input, then invokes
		// broker close. Break the *close completion* only, AFTER the child
		// was cleanly stopped/removed from the broker.
		if completeCount == 2 {
			breakAuditCompletion(t, path)
		}
	}
	closed := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close"))
	if closed.OK || closed.ErrorCode != "audit_degraded" || closed.Status != "closed_audit_degraded" || completeCount != 2 {
		t.Fatalf("physical close/audit failure not distinguished: %+v; writes=%d", closed, completeCount)
	}
	if !s.browserAdmission.acquireRun() {
		t.Fatal("audit completion held lease despite verified child death")
	}
	s.browserAdmission.releaseRun()
	if stat := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_status")); stat.OK || stat.ErrorCode != "browser_session_not_found" {
		t.Fatalf("cleanly removed session resurrected: %+v", stat)
	}
	s.auditBeforeCompletionHook = nil
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.audit = audit.New(path)
	again := s.browserSessionAction(ctx, owner)
	if !again.OK {
		t.Fatalf("recovered audit cannot reopen: %+v", again)
	}
	if resp := s.browserSessionAction(ctx, browserOwnerControl(owner, again.SessionID, "browser_session_close")); !resp.OK {
		t.Fatalf("recovered close: %+v", resp)
	}
}

func TestBrowserSessionExitedRetentionGCAndExpiry(t *testing.T) {
	s, owner, workspace := browserAuditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("open: %+v", opened)
	}
	id := opened.SessionID
	entry, err := s.browserSessionEntry(browserOwnerControl(owner, id, "browser_session_status"))
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	child := entry.cmd.Process
	entry.mu.Unlock()
	if err := child.Kill(); err != nil {
		t.Fatal(err)
	}
	waitForStdioExit(t, entry)
	entry.mu.Lock()
	entry.completed = time.Now().Add(-completedSessionRetain - time.Second)
	entry.mu.Unlock()
	otherID, err := s.workerStdioSession(Request{PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass, Workspace: workspace, RequestID: "gc-provocation"}, "lsp", workspace, "/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = s.sessions.close(Request{PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass, Workspace: workspace}, otherID)
	}()
	if _, err = s.browserSessionEntry(browserOwnerControl(owner, id, "browser_session_status")); err != nil {
		t.Fatalf("generic retention GC discarded leased Browser entry: %v", err)
	}
	if s.browserAdmission.acquireRun() {
		t.Fatal("abnormal exit prematurely released shared profile")
	}
	s.expireProcessSession(entry)
	if _, err = s.browserSessionEntry(browserOwnerControl(owner, id, "browser_session_status")); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("expiry did not remove terminated Browser entry: %v", err)
	}
	if !s.browserAdmission.acquireRun() {
		t.Fatal("expiry did not release verified Browser lease")
	}
	s.browserAdmission.releaseRun()
	replacement := s.browserSessionAction(ctx, owner)
	if !replacement.OK {
		t.Fatalf("new Browser open failed after GC+expiry: %+v", replacement)
	}
	if close := s.browserSessionAction(ctx, browserOwnerControl(owner, replacement.SessionID, "browser_session_close")); !close.OK {
		t.Fatalf("replacement close: %+v", close)
	}
}

func TestBrowserSessionCloseBeforeActionAuditFailureRetainsLease(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("open: %+v", opened)
	}
	id := opened.SessionID
	s.auditWriteHook = func(phase string) error {
		if phase == "start" {
			return errors.New("injected audit failure")
		}
		return nil
	}
	denied := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close"))
	if denied.OK || denied.ErrorCode != "browser_session_cleanup_unverified" {
		t.Fatalf("unverified close succeeded: %+v", denied)
	}
	if s.browserAdmission.acquireRun() {
		t.Fatal("unverified close released profile")
	}
	s.auditWriteHook = nil
	fixed := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close"))
	if !fixed.OK {
		t.Fatalf("audited retry cannot stop same session: %+v", fixed)
	}
	if !s.browserAdmission.acquireRun() {
		t.Fatal("post-retry profile remained reserved")
	}
	s.browserAdmission.releaseRun()
}

func TestBrowserSessionKnownFlowFailurePreservesSession(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	// This fixture returns a known failure with bounded details.
	script := `#!/bin/sh
printf 'ASA_BROWSER_SESSION {"event":"ready"}\n'
while IFS= read -r line; do
  case "$line" in
    *'"type":"close"'*) exit 0 ;;
  esac
  nonce=$(printf '%s' "$line" | sed -n 's/.*"nonce":"\([0-9a-f]*\)".*/\1/p')
  printf 'ASA_BROWSER_SESSION {"event":"result","nonce":"%s","result":{"ok":false,"failed_step":0,"results":[{"index":0,"action":"assert_text","ok":false,"error":"not_found"}]}}\n' "$nonce"
done
exit 0
`
	if err := os.WriteFile(s.browserNodeBinary, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("open: %+v", opened)
	}
	id := opened.SessionID
	defer func() { _ = s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")) }()
	request := browserOwnerControl(owner, id, "browser_session_flow")
	request.Content = `[{"action":"assert_text","selector":"#missing","expected":"hello"}]`
	first := s.browserSessionAction(ctx, request)
	if first.OK || first.ErrorCode != "browser_flow_failed" || first.Status != "failed" || first.SessionID != id || !strings.Contains(first.Output, `"failed_step":0`) {
		t.Fatalf("known failed Browser flow misclassified: %+v", first)
	}
	// A proved failure is not an unknown transport outcome. This second
	// deliberate action must be allowed (even if the fixture fails again).
	second := s.browserSessionAction(ctx, request)
	if second.ErrorCode != "browser_flow_failed" {
		t.Fatalf("known failure poisoned session: %+v", second)
	}
}
