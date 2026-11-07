package executor

import "testing"

// Parsing only; never create or mutate systemd cgroups during ordinary tests.
func TestExecutorCgroupIdentityRequiresConfiguredUnit(t *testing.T) {
	for _, tc := range []struct {
		name, cg, configured, expected string
		ok                             bool
	}{
		{"canonical", "0::/system.slice/ai-server-agent-executor.service\n",
			"ai-server-agent-executor.service", "/system.slice/ai-server-agent-executor.service", true},
		{"isolated installation", "0::/system.slice/asa65-issue65-executor.service\n",
			"asa65-issue65-executor.service", "/system.slice/asa65-issue65-executor.service", true},
		{"mismatch denied", "0::/system.slice/asa65-issue65-executor.service\n",
			"ai-server-agent-executor.service", "", false},
		{"wrong unit type", "0::/system.slice/asa65-issue65-agent.service\n",
			"asa65-issue65-executor.service", "", false},
		{"nested wrong parent", "0::/system.slice/asa65-issue65-executor.service/child\n",
			"asa65-issue65-executor.service", "", false},
		{"traversal", "0::/system.slice/other.service/../asa65-issue65-executor.service\n",
			"asa65-issue65-executor.service", "", false},
		{"nested foreign service", "0::/system.slice/other.service/asa65-issue65-executor.service\n",
			"asa65-issue65-executor.service", "", false},
		{"nonstandard slice", "0::/user.slice/asa65-issue65-executor.service\n",
			"asa65-issue65-executor.service", "", false},
		{"empty expected", "0::/system.slice/ai-server-agent-executor.service\n", "", "", false},
		{"no unified cgroup", "3:devices:/system.slice/ai-server-agent-executor.service\n",
			"ai-server-agent-executor.service", "", false},
		{"unrelated cgroup", "0::/user.slice/session-11.scope\n",
			"ai-server-agent-executor.service", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel, err := executorCgroupRelative([]byte(tc.cg), tc.configured)
			if (err == nil) != tc.ok || rel != tc.expected {
				t.Fatalf("unexpected cgroup evidence: rel=%q err=%v", rel, err)
			}
		})
	}
}
