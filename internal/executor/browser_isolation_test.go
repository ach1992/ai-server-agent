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

// The new identity may have safe-looking names, HOME, group membership and
// private files while still sharing the numeric UID or GID that grants access
// to the protected aiagent Executor credential and UNIX socket.
func TestIsolatedBrowserAllProtectedNumericIdentityAliasesFailClosed(t *testing.T) {
	const workerUID, workerGID, agentUID, agentGID uint32 = 984, 983, 982, 981
	cases := []struct {
		name       string
		browserUID uint32
		browserGID uint32
		ok         bool
	}{
		{"independent three-account identity", 980, 979, true},
		{"browser UID aliases aiworker", workerUID, 979, false},
		{"browser GID aliases aiworker", 980, workerGID, false},
		{"browser UID aliases aiagent", agentUID, 979, false},
		{"browser GID aliases aiagent", 980, agentGID, false},
		{"browser has zero UID", 0, 979, false},
		{"browser has zero GID", 980, 0, false},
		{"worker has zero UID", 980, 979, false},
		{"worker has zero GID", 980, 979, false},
		{"agent has zero UID", 980, 979, false},
		{"agent has zero GID", 980, 979, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wu, wg, au, ag := workerUID, workerGID, agentUID, agentGID
			switch tc.name {
			case "worker has zero UID":
				wu = 0
			case "worker has zero GID":
				wg = 0
			case "agent has zero UID":
				au = 0
			case "agent has zero GID":
				ag = 0
			}
			if got := distinctIsolatedBrowserIdentity(tc.browserUID, tc.browserGID, wu, wg, au, ag); got != tc.ok {
				t.Fatalf("identity equality gate: got=%t expected=%t", got, tc.ok)
			}
			if !tc.ok {
				s := &Server{
					browserUID: tc.browserUID, browserGID: tc.browserGID,
					workerUID: wu, workerGID: wg, agentUID: au, agentGID: ag,
				}
				if err := s.verifyIsolatedBrowserIdentities(); err == nil {
					t.Fatal("unsafe Browser numeric alias unexpectedly permitted")
				}
			}
		})
	}
}
