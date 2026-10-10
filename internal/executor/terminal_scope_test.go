package executor

import (
	"strings"
	"testing"
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
