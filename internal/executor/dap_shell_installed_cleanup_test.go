package executor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These are non-root, nonintrusive regressions of the SAME cleanup gate used
// by the opt-in installed Executor test. They touch only disposable test
// fixtures and never connect to an actual Executor or change a host policy.
func TestShellDAPInstalledFixtureCleanupFailureBoundaries(t *testing.T) {
	tests := []struct {
		name         string
		dispatched   bool
		launchReply  Response
		verifiedStop bool
		recoveryOK   bool
		wantRetained bool
		wantStops    int
	}{
		{
			name:       "failed launch with SessionID but cleanup remains uncertain",
			dispatched: true, launchReply: Response{OK: false, SessionID: "failed-launch-session"},
			wantRetained: true, wantStops: 1,
		},
		{
			name:         "transport failure after dispatch without SessionID",
			dispatched:   true, // the transport returned no recoverable response
			wantRetained: true, wantStops: 0,
		},
		{
			name:       "verified debug_stop after successful launch",
			dispatched: true, launchReply: Response{OK: true, SessionID: "stopped-session"},
			verifiedStop: true, wantRetained: false, wantStops: 0,
		},
		{
			name:       "failed launch with SessionID and verified broker recovery",
			dispatched: true, launchReply: Response{OK: false, SessionID: "recoverable-session"},
			recoveryOK: true, wantRetained: false, wantStops: 1,
		},
		{
			name:         "pre-dispatch failure may remove fixture",
			wantRetained: false, wantStops: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workspace, err := os.MkdirTemp(t.TempDir(), "shell-dap-owned-")
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(workspace, "original-source.sh")
			if err := os.WriteFile(marker, []byte("#!/bin/bash\n"), 0600); err != nil {
				t.Fatal(err)
			}
			fixture := shellDAPCIFixtureGuard{workspace: workspace}
			// Mimic the installed path's order: mark uncertain BEFORE issuing
			// the launch, and capture SessionID irrespective of response.OK.
			fixture.launchDispatched = tc.dispatched
			fixture.sessionID = tc.launchReply.SessionID
			fixture.cleanupVerified = tc.verifiedStop
			stopCalls := 0
			retained, err := fixture.finish(func(id string) bool {
				stopCalls++
				if id != tc.launchReply.SessionID || id == "" {
					t.Fatalf("cleanup targeted wrong session: got=%q want=%q", id, tc.launchReply.SessionID)
				}
				return tc.recoveryOK
			})
			if err != nil {
				t.Fatalf("test-owned fixture cleanup returned error: %v", err)
			}
			if stopCalls != tc.wantStops || retained != tc.wantRetained {
				t.Fatalf("cleanup state: retained=%t stops=%d; want retained=%t stops=%d",
					retained, stopCalls, tc.wantRetained, tc.wantStops)
			}
			_, statErr := os.Stat(marker)
			if tc.wantRetained {
				if statErr != nil {
					t.Fatalf("unknown process cleanup erased source fixture: %v", statErr)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("verified/pre-dispatch cleanup did not remove fixture: %v", statErr)
			}
		})
	}
}
