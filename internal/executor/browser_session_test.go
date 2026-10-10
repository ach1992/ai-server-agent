package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserSessionLeaseAndPrincipalBinding(t *testing.T) {
	if _, err := os.Stat("/opt/ai-server-agent/browser/node/bin/node"); err != nil {
		t.Skip("pinned test server Node unavailable")
	}
	s, owner, workspace := testStdioBroker(t)
	// The agent owns this body. The fixture exercises the real pinned Node,
	// executor-owned process/pidfd, bounded stdio transport and auth layer,
	// not an unauthenticated public client or a second Browser engine.
	script := `import { createInterface } from 'node:readline';
const reply = obj => process.stdout.write('ASA_BROWSER_SESSION ' + JSON.stringify(obj) + '\n');
reply({event:'ready'});
for await (const line of createInterface({input:process.stdin})) {
  const cmd = JSON.parse(line);
  if (cmd.type === 'close') break;
  reply({event:'result',nonce:cmd.nonce,result:{ok:true,executed:cmd.steps.length}});
}
process.exit(0);`
	owner.Content = script
	owner.Action = "browser_session_open"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK || !strings.HasPrefix(opened.SessionID, "stdio_") {
		t.Fatalf("Browser session open failed: %+v", opened)
	}
	id := opened.SessionID
	defer func() {
		_ = s.browserSessionClose(Request{PrincipalID: owner.PrincipalID, PrincipalClass: owner.PrincipalClass, Workspace: workspace, SessionID: id})
	}()
	if s.browserAdmission.acquireRun() {
		t.Fatal("one-shot Browser run admitted over live session")
	}
	forbidden := s.runContext(ctx, Request{Action: "run", AuditAction: "browser_run", Command: "echo SHOULD_NOT_RUN"})
	if forbidden.OK || forbidden.ErrorCode != "resource_limit" || strings.Contains(forbidden.Output, "SHOULD_NOT_RUN") {
		t.Fatalf("executor one-shot Browser bypassed session lease: %+v", forbidden)
	}
	other := owner
	other.PrincipalID = "attacker"
	other.SessionID = id
	other.Action = "browser_session_status"
	other.Content = ""
	if got := s.browserSessionAction(ctx, other); got.OK || got.ErrorCode != "browser_session_not_found" {
		t.Fatalf("cross-principal leaked session: %+v", got)
	}
	other.Action = "browser_session_close"
	if got := s.browserSessionAction(ctx, other); got.OK || got.ErrorCode != "browser_session_not_found" {
		t.Fatalf("cross-principal closed another session: %+v", got)
	}
	other = owner
	other.SessionID = id
	other.Workspace = filepath.Dir(workspace)
	other.Content = ""
	other.Action = "browser_session_status"
	if got := s.browserSessionAction(ctx, other); got.OK || got.ErrorCode != "browser_session_not_found" {
		t.Fatalf("cross-workspace leaked session: %+v", got)
	}
	action := owner
	action.SessionID = id
	action.Content = `[{"action":"snapshot"}]`
	action.Action = "browser_session_flow"
	performed := s.browserSessionAction(ctx, action)
	if !performed.OK || !strings.Contains(performed.Output, `"executed":1`) {
		t.Fatalf("cross-call protocol failure: %+v", performed)
	}
	action.Content = `[{"action":"assert_url"}]`
	if got := s.browserSessionAction(ctx, action); !got.OK {
		t.Fatalf("second call failed: %+v", got)
	}
	stat := action
	stat.Action = "browser_session_status"
	stat.Content = ""
	if got := s.browserSessionAction(ctx, stat); !got.OK || got.Status != "running" {
		t.Fatalf("status: %+v", got)
	}
	closed := stat
	closed.Action = "browser_session_close"
	if got := s.browserSessionAction(ctx, closed); !got.OK {
		t.Fatalf("close failed: %+v", got)
	}
	if !s.browserAdmission.acquireRun() {
		t.Fatal("Browser profile remained held after verified close")
	}
	s.browserAdmission.releaseRun()
	if got := s.browserSessionAction(ctx, stat); got.OK || got.ErrorCode != "browser_session_not_found" {
		t.Fatalf("closed identity remains addressable: %+v", got)
	}
}

func TestBrowserSessionAdmissionOwnership(t *testing.T) {
	var gate browserSessionAdmission
	if !gate.acquireRun() || gate.acquireRun() || gate.try("opening") {
		t.Fatal("one-shot profile admission not exclusive")
	}
	gate.release("wrong-holder")
	if gate.try("opening") {
		t.Fatal("wrong owner released profile")
	}
	gate.releaseRun()
	if !gate.try("opening") || !gate.transfer("opening", "stdio_123") {
		t.Fatal("open lease transfer failed")
	}
	if gate.matches("one-shot") || gate.acquireRun() || gate.transfer("opening", "stdio_456") {
		t.Fatal("profile lease aliased")
	}
	gate.release("stdio_123")
	if !gate.acquireRun() {
		t.Fatal("lease not reusable after release")
	}
}
