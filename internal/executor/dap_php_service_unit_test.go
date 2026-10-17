package executor

import (
	"fmt"
	"strings"
	"testing"
)

func TestPHPHTTPServiceUnitParserRequiresStableRootManagerIdentity(t *testing.T) {
	const id = "1234567890123456789"
	const user = "aiworker"
	unit := "ai-job-" + id + ".service"
	good := []string{
		"Id=" + unit,
		"LoadState=loaded",
		"ActiveState=active",
		"SubState=running",
		"User=aiworker",
		"MainPID=24680",
		"ControlGroup=/system.slice/" + unit,
		"InvocationID=0123456789abcdef0123456789abcdef",
	}
	input := strings.Join(good, "\n") + "\n"
	proof, err := parsePHPHTTPServiceUnit(id, input, user)
	if err != nil || proof.id != id || proof.mainPID != 24680 ||
		proof.cgroup != "/system.slice/"+unit {
		t.Fatalf("root-owned service unit properties rejected: proof=%+v err=%v", proof, err)
	}
	changes := []struct{ name, from, to string }{
		{"wrong_id", "Id=" + unit, "Id=some-other.service"},
		{"old_invocation", "InvocationID=0123456789abcdef0123456789abcdef", "InvocationID="},
		{"invalid_invocation", "InvocationID=0123456789abcdef0123456789abcdef", "InvocationID=0123456789abcdef0123456789abcdeg"},
		{"wrong_user", "User=aiworker", "User=root"},
		{"root_active_but_exited", "SubState=running", "SubState=exited"},
		{"inactive", "ActiveState=active", "ActiveState=inactive"},
		{"not_loaded", "LoadState=loaded", "LoadState=not-found"},
		{"main_pid_zero", "MainPID=24680", "MainPID=0"},
		{"main_pid_non_numeric", "MainPID=24680", "MainPID=unavailable"},
		{"wrong_cgroup", "ControlGroup=/system.slice/" + unit, "ControlGroup=/system.slice/another.service"},
		{"nested_cgroup", "ControlGroup=/system.slice/" + unit, "ControlGroup=/system.slice/" + unit + "/attacker"},
		{"relative_cgroup", "ControlGroup=/system.slice/" + unit, "ControlGroup=../../another"},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parsePHPHTTPServiceUnit(id, strings.Replace(input, tc.from, tc.to, 1), user); err == nil {
				t.Fatal("unverified PHP HTTP service unit admitted")
			}
		})
	}
	for _, tc := range []struct {
		name, text, job, user string
	}{
		{"duplicate", input + "MainPID=123\n", id, user},
		{"missing_property", strings.Replace(input, "User=aiworker\n", "", 1), id, user},
		{"bad_job", input, "../etc", user},
		{"wrong_claim_id", input, "9876543210123456789", user},
		{"blank_user", input, id, ""},
		{"no_equal", input + "not-a-property\n", id, user},
		{"oversize", input + strings.Repeat("x", systemdOutputLimit), id, user},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parsePHPHTTPServiceUnit(tc.job, tc.text, tc.user); err == nil {
				t.Fatal(fmt.Sprintf("untrusted PHP HTTP unit observation accepted in %s", tc.name))
			}
		})
	}
}
