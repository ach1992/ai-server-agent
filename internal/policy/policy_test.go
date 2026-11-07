package policy

import "testing"

func TestConnectionGuard(t *testing.T) {
	g := New([]string{"/etc/ai-server-agent", "127.0.0.1:3210"})
	cases := []struct {
		cmd      string
		approval bool
	}{
		{"apt-get install nginx", false},
		{"systemctl restart nginx", false},
		{"systemctl stop ai-server-agent.service", true},
		{"systemctl restart ai-server-agent.service", true},
		{"rm -rf /etc/ai-server-agent", true},
		{"rm -r -f /tmp/disposable", true},
		{"rm -f -r /tmp/disposable", true},
		{"rm --recursive --force /tmp/disposable", true},
		{"rm --force --recursive /tmp/disposable", true},
		{"rm -r /tmp/disposable", false},
		{"rm -f /tmp/disposable", false},
		{"ufw reset", true},
		{"ufw enable", true},
		{"iptables -P INPUT DROP", true},
		{"nft flush ruleset", true},
		{"ip route flush table main", true},
		{"ip link set eth0 down", true},
		{"systemctl restart systemd-networkd", true},
		{"apt-get purge systemd", true},
		{"apt remove bash", true},
		{"reboot", true},
	}
	for _, tc := range cases {
		if got := g.Evaluate(tc.cmd, true); got.RequiresApproval != tc.approval {
			t.Fatalf("%q approval=%v want %v (%+v)", tc.cmd, got.RequiresApproval, tc.approval, got)
		}
	}
}

// Print-only commands must not demand destructive-operation approval merely
// because the *text* mentions a destructive command or protected resource.
func TestRootLiteralOutputAvoidsSpuriousApproval(t *testing.T) {
	g := New([]string{"/etc/ai-server-agent"})
	harmless := []string{
		"printf '%s' 'rm -rf /tmp/disposable'",
		"printf '%s\\n' 'DROP TABLE accounts'",
		"echo \"systemctl restart ai-server-agent.service\"",
		"echo '/etc/ai-server-agent/mcp-credentials.json'",
		"printf '%s' 'nft flush ruleset'",
	}
	for _, cmd := range harmless {
		if got := g.Evaluate(cmd, true); !got.Allowed || got.RequiresApproval {
			t.Errorf("literal text wrongly denied: cmd=%q decision=%+v", cmd, got)
		}
	}
	unsafe := []string{
		"printf '%s' 'rm -rf /' | bash",
		"echo 'rm -rf /' ; rm -rf /",
		"printf '%s' \"$(rm -rf /)\"",
		"printf '%s' 'rm -rf /' > /etc/ai-server-agent/config.json",
		"bash -c 'rm -rf /tmp/disposable'",
		"echo 'systemctl stop ai-server-agent' && systemctl stop ai-server-agent",
	}
	for _, cmd := range unsafe {
		if got := g.Evaluate(cmd, true); !got.Allowed || !got.RequiresApproval {
			t.Errorf("shell execution bypassed approval: cmd=%q decision=%+v", cmd, got)
		}
	}
}
