package executor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Only diagnostically enrich an already-failing security preflight. This
// never authorizes shell launch or changes kernel state on either branch.
func TestShellDAPReadinessFailureClassification(t *testing.T) {
	cases := []struct {
		name    string
		runtime error
		ipc     error
		reason  string
	}{
		{"untrusted_runtime", errors.New("/private/identity-sensitive/path"), nil, "shell_dap_runtime_unavailable"},
		{"runtime_before_kernel", errors.New("runtime unavailable"), errShellDAPPtracePolicyInsufficient, "shell_dap_runtime_unavailable"},
		{"yama_disabled", nil, errShellDAPPtracePolicyInsufficient, "shell_dap_ipc_ptrace_policy_insufficient"},
		{"yama_wrapped", nil, errors.Join(errors.New("context"), errShellDAPPtracePolicyInsufficient), "shell_dap_ipc_ptrace_policy_insufficient"},
		{"nonroot", nil, errShellDAPRequiresRootExecutor, "shell_dap_ipc_requires_root_executor"},
		{"policy_unreadable", nil, errors.New("/proc/sys/kernel/yama/ptrace_scope: unexpected error"), "shell_dap_ipc_prerequisite_unavailable"},
		{"unknown_ipc_failure", nil, errors.New("unanticipated IPC condition"), "shell_dap_ipc_prerequisite_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, hint := shellDAPReadinessFailure(tc.runtime, tc.ipc)
			if code != tc.reason || hint == "" {
				t.Fatalf("unactionable wrong readiness classification: %q, %q", code, hint)
			}
			if strings.Contains(hint, "/private/") || strings.Contains(hint, "unanticipated IPC") ||
				strings.Contains(hint, "/proc/sys/kernel/yama/") {
				t.Fatalf("passive status exposed raw host diagnostics: %q", hint)
			}
			if tc.reason == "shell_dap_ipc_ptrace_policy_insufficient" &&
				(!strings.Contains(hint, "1..3") || !strings.Contains(hint, "operator approval") ||
					!strings.Contains(hint, "never changes")) {
				t.Fatalf("Yama guidance incorrectly allows auto-weakening policy: %q", hint)
			}
		})
	}
}

func TestShellDAPStatusReportsTruthfulPassiveReason(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	owner.Action, owner.Language = "debug_adapter_status", "shell"
	got := s.debugAction(t.Context(), owner)
	if !got.OK || got.OutputEncoding != "json" || got.BytesReturned != int64(len(got.Output)) {
		t.Fatalf("shell status metadata malformed: %+v", got)
	}
	var data struct {
		Adapter           string `json:"adapter"`
		Installed         bool   `json:"installed"`
		UnavailableReason string `json:"unavailable_reason"`
		OperatorHint      string `json:"operator_hint"`
	}
	if err := json.Unmarshal([]byte(got.Output), &data); err != nil {
		t.Fatal(err)
	}
	if data.Adapter != "shell/bash-trap" {
		t.Fatalf("wrong adapter: %+v", data)
	}
	if data.Installed {
		if got.Status != "installed_containment_unverified" || data.UnavailableReason != "" || data.OperatorHint != "" {
			t.Fatalf("ready adapter spuriously reported a failure: %+v data=%+v", got, data)
		}
	} else if got.Status != "unavailable" || data.UnavailableReason == "" || data.OperatorHint == "" {
		t.Fatalf("unavailable adapter hid prerequisites: %+v data=%+v", got, data)
	}
	s.sessions.mu.Lock()
	count := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if count != 0 {
		t.Fatalf("passive readiness query created %d debug sessions", count)
	}
}
