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
