package executor

import (
	"strings"
	"testing"
	"time"
)

func TestTerminalScopeUnitNamingAndValidation(t *testing.T) {
	const name = "asa_0123456789abcdef01234567"
	got, err := terminalScopeUnit(name)
	if err != nil || got != "asa-pty-0123456789abcdef01234567.scope" {
		t.Fatalf("unit=%q err=%v", got, err)
	}
	for _, unsafe := range []string{
		"", "asa_", "asa_0123456789abcdef0123456g",
		"asa_0123456789abcdef01234567.scope", "../asa_0123456789abcdef01234567",
		"asa_0123456789abcdef01234567;systemctl stop sshd",
		strings.Repeat("a", 256),
	} {
		if _, err := terminalScopeUnit(unsafe); err == nil {
			t.Fatalf("accepted invalid tmux generation: %q", unsafe)
		}
		if err := stopScopedTerminalBackend(unsafe); err == nil {
			t.Fatalf("accepted invalid systemd unit identity: %q", unsafe)
		}
	}
}

func TestTerminalLeaseUnitScopedToExactGeneration(t *testing.T) {
	name := "asa_0123456789abcdef01234567"
	timer, err := terminalLeaseTimerUnit(name)
	if err != nil || timer != "asa-pty-0123456789abcdef01234567-idle.timer" {
		t.Fatalf("incorrect root-only timer identity: %q %v", timer, err)
	}
	for _, candidate := range []string{"", "asa_123", "asa_0123456789abcdef01234567/../bad"} {
		if _, err := terminalLeaseTimerUnit(candidate); err == nil {
			t.Fatalf("unsafe generation %q", candidate)
		}
	}
}

func TestTerminalRecordVersionedLeaseGuards(t *testing.T) {
	opened := time.Now().UTC()
	old := terminalRecord{Version: 1, ExpiresAt: opened.Add(time.Hour)}
	if !validTerminalRecordVersion(old) {
		t.Fatal("valid one-hour legacy record rejected")
	}
	v2 := terminalRecord{Version: 2, Scoped: true, CreatedAt: opened, ExpiresAt: opened.Add(time.Hour)}
	if !validTerminalRecordVersion(v2) {
		t.Fatal("durable scoped lease rejected")
	}
	cases := []terminalRecord{
		{Version: 3}, {Version: 2, Scoped: false, CreatedAt: opened, ExpiresAt: opened.Add(time.Hour)},
		{Version: 2, Scoped: true, ExpiresAt: opened.Add(time.Hour)},
		{Version: 2, Scoped: true, CreatedAt: opened, ExpiresAt: opened.Add(-time.Second)},
		{Version: 2, Scoped: true, CreatedAt: opened, ExpiresAt: opened.Add(terminalScopeHardAge + time.Second)},
	}
	for _, rec := range cases {
		if validTerminalRecordVersion(rec) {
			t.Fatalf("unsafe record accepted: %+v", rec)
		}
	}
}

func TestTerminalReadLeaseFailureNeverUnlocksUnheldMutex(t *testing.T) {
	s, req, workspace := testStdioBroker(t)
	req.Action = "terminal_read"
	req.SessionID = "stdio_invalid-lease"
	req.SessionEpoch = "valid-epoch"
	term := &tmuxTerminalState{epoch: req.SessionEpoch, scoped: true, name: "INVALID"}
	e := &stdioSession{id: req.SessionID, kind: "terminal", ownerID: req.PrincipalID,
		ownerClass: req.PrincipalClass, workspace: workspace, terminal: term}
	if err := s.sessions.reserve(e); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.remove(e)
	got := s.terminalRead(req)
	if got.OK || got.ErrorCode != "terminal_lease_unverified" {
		t.Fatalf("renewal error not fail-closed: %+v", got)
	}
}
