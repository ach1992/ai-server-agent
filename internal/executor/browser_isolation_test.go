package executor

import (
	"context"
	"testing"
	"time"
)

// A production-configured Executor may continue shared Browser work when
// the dedicated private account is unavailable, but never silently fall
// back to the worker UID for an isolated Browser session.
func TestIsolatedBrowserMissingPrivateIdentityFailsBeforeAdmission(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	s.cfg.AgentUser = "aiagent"
	s.browserUID, s.browserGID = 0, 0
	owner.BrowserProfileMode = "isolated"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	denied := s.browserSessionAction(ctx, owner)
	if denied.OK || denied.ErrorCode != "browser_isolation_unavailable" ||
		s.browserAdmission.reserved() {
		t.Fatalf("unprovisioned Browser identity admitted or leaked capacity: %+v", denied)
	}
	owner.BrowserProfileMode = "shared"
	allowed := s.browserSessionAction(ctx, owner)
	if !allowed.OK {
		t.Fatalf("shared Browser must remain independent of missing isolated account: %+v", allowed)
	}
	defer func() {
		_ = s.browserSessionClose(browserOwnerControl(owner, allowed.SessionID, "browser_session_close"))
	}()
}
