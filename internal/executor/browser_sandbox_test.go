package executor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBrowserSandboxStatusParserAndEnforcement(t *testing.T) {
	status := []byte("Name:\tchrome\nUid:\t1000\t1000\t1000\t1000\nGid:\t1002\t1002\t1002\t1002\nSeccomp:\t2\nNoNewPrivs:\t1\n")
	for _, tc := range []struct {
		key      string
		index    int
		expected uint32
	}{
		{"Uid", 1, 1000}, {"Gid", 1, 1002}, {"Seccomp", 0, 2}, {"NoNewPrivs", 0, 1},
	} {
		got, err := sandboxStatusField(status, tc.key, tc.index)
		if err != nil || got != tc.expected {
			t.Fatalf("%s: %v value=%d", tc.key, err, got)
		}
	}
	for _, raw := range [][]byte{nil, []byte("Seccomp:\tbogus\n"), []byte("Seccomp:\n")} {
		if _, err := sandboxStatusField(raw, "Seccomp", 0); err == nil {
			t.Fatalf("malformed status accepted: %q", raw)
		}
	}
	parent := browserSandboxProcess{effectiveUID: 1000, effectiveGID: 1002, seccomp: 0, userNS: "user:[100]", pidNS: "pid:[200]"}
	child := browserSandboxProcess{effectiveUID: 1000, effectiveGID: 1002, seccomp: 2, noNewPrivs: 1, userNS: "user:[101]", pidNS: "pid:[201]"}
	if !child.confinedFrom(parent, 1000, 1002) {
		t.Fatal("realistic confined renderer not accepted")
	}
	for name, mutate := range map[string]func(*browserSandboxProcess){
		"wrong_uid":            func(p *browserSandboxProcess) { p.effectiveUID = 1001 },
		"wrong_gid":            func(p *browserSandboxProcess) { p.effectiveGID = 1001 },
		"no_seccomp":           func(p *browserSandboxProcess) { p.seccomp = 0 },
		"strict_seccomp_wrong": func(p *browserSandboxProcess) { p.seccomp = 1 },
		"no_nnp":               func(p *browserSandboxProcess) { p.noNewPrivs = 0 },
		"shared_userns":        func(p *browserSandboxProcess) { p.userNS = parent.userNS },
		"shared_pidns":         func(p *browserSandboxProcess) { p.pidNS = parent.pidNS },
		"missing_userns":       func(p *browserSandboxProcess) { p.userNS = "" },
	} {
		t.Run(name, func(t *testing.T) {
			got := child
			mutate(&got)
			if got.confinedFrom(parent, 1000, 1002) {
				t.Fatal("insufficient renderer sandbox attestation accepted")
			}
		})
	}
}

func TestChromiumSandboxProcessTitleAndPinnedBinary(t *testing.T) {
	for _, tc := range []struct {
		name, cmdline       string
		renderer, noSandbox bool
	}{
		{"separate_flags", "chrome-headless-shell\x00--type=renderer\x00--headless=old\x00", true, false},
		{"rewritten_argv0", "chrome-headless-shell --type=renderer --headless=old\x00/opt/chrome-headless-shell\x00", true, false},
		{"rewritten_with_no_sandbox", "chrome-headless-shell --type=renderer --no-sandbox --headless\x00", true, true},
		{"separate_no_sandbox", "chrome\x00--type=renderer\x00--no-sandbox=1\x00", true, true},
		{"false_prefix", "chrome-headless-shell --type=renderer-extra --no-sandboxish\x00", false, false},
		{"embedded_flag_in_unrelated_arg", "node\x00--input-type=module\x00const s='--type=renderer --no-sandbox'\x00", false, false},
		{"no_flags", "chrome-headless-shell\x00--headless=old\x00", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renderer, noSandbox := chromiumSandboxCommandFlags([]byte(tc.cmdline))
			if renderer != tc.renderer || noSandbox != tc.noSandbox {
				t.Fatalf("unexpected argv interpretation: renderer=%t noSandbox=%t", renderer, noSandbox)
			}
		})
	}
	for _, tc := range []struct {
		path    string
		trusted bool
	}{
		{"/opt/ai-server-agent/browser/browsers/chromium_headless_shell-1228/chrome-headless-shell-linux64/chrome-headless-shell", true},
		{"/opt/ai-server-agent/browser/browsers/chromium-1228/chrome-linux64/chrome", true},
		{"/tmp/chrome-headless-shell", false},
		{"/opt/ai-server-agent/browser/browsers/chromium_headless_shell-1228/other/chrome", false},
		{"/opt/ai-server-agent/browser/browsers/chromium_headless_shell-1228/chrome-headless-shell-linux64/chrome-headless-shell (deleted)", false},
		{"/opt/ai-server-agent/browser/node/bin/node", false},
	} {
		if got := pinnedChromiumExecutable(tc.path); got != tc.trusted {
			t.Errorf("pinned Chromium executable identity %q: got %t, want %t", tc.path, got, tc.trusted)
		}
	}
}

func TestBrowserSandboxOptInStartupRefusalAndCleanRelease(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	owner.BrowserSandbox = true
	// Even with no dedicated identity, the shared profile must reject
	// the opt-in before reserving a process or touching the runtime.
	shared := s.browserSessionAction(ctx, owner)
	if shared.OK || shared.ErrorCode != "browser_sandbox_requires_isolated_profile" ||
		s.browserAdmission.reserved() {
		t.Fatalf("shared sandbox opt-in bypassed its mode gate: %+v", shared)
	}
	owner.BrowserProfileMode = "isolated"
	script := `#!/bin/sh
printf 'ASA_BROWSER_SESSION {"event":"startup_error","reason":"browser_sandbox_unavailable"}\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0 ;; esac
done
`
	if err := os.WriteFile(s.browserNodeBinary, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	refused := s.browserSessionAction(ctx, owner)
	if refused.OK || refused.ErrorCode != "browser_sandbox_unavailable" ||
		refused.SessionID != "" || refused.Status == "cleanup_unverified" ||
		!strings.Contains(refused.Error, "no unsandboxed retry") {
		t.Fatalf("explicit sandbox refusal or verified cleanup missing: %+v", refused)
	}
	if s.browserAdmission.reserved() {
		t.Fatal("verified failed launch retained capacity")
	}
	// A dishonest or incompatible runner reporting ready, but without
	// real root-owned Chromium/cgroup proof, also fails closed.
	ready := `#!/bin/sh
printf 'ASA_BROWSER_SESSION {"event":"ready"}\n'
while IFS= read -r line; do
  case "$line" in *'"type":"close"'*) exit 0 ;; esac
done
`
	if err := os.WriteFile(s.browserNodeBinary, []byte(ready), 0755); err != nil {
		t.Fatal(err)
	}
	unverified := s.browserSessionAction(ctx, owner)
	if unverified.OK || unverified.ErrorCode != "browser_sandbox_unverified" ||
		unverified.SessionID != "" || s.browserAdmission.reserved() {
		t.Fatalf("fake ready bypassed root kernel attestation or leaked admission: %+v", unverified)
	}
	owner.BrowserSandbox = false
	sharedOpen := owner
	sharedOpen.BrowserProfileMode = "shared"
	accepted := s.browserSessionAction(ctx, sharedOpen)
	if !accepted.OK || strings.Contains(accepted.Output, "chromium_sandbox") {
		t.Fatalf("existing unsandboxed shared Browser changed: %+v", accepted)
	}
	if closed := s.browserSessionAction(ctx, browserOwnerControl(sharedOpen, accepted.SessionID, "browser_session_close")); !closed.OK {
		t.Fatalf("shared Browser verified close failed: %+v", closed)
	}
	if s.browserAdmission.reserved() {
		t.Fatal("shared Browser admission not reclaimed")
	}
}
