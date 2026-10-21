package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserProfileModeValidationAndOneShotIsolation(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		valid       bool
	}{
		{"", "shared", true}, {"shared", "shared", true}, {"isolated", "isolated", true},
		{"invalid", "", false}, {"ISOLATED", "", false}, {"shared;rm", "", false},
	} {
		got, err := normalizeProfileMode(tc.input)
		if tc.valid && (err != nil || got != tc.want) {
			t.Fatalf("mode %q=%q err=%v", tc.input, got, err)
		}
		if !tc.valid && err == nil {
			t.Fatalf("accepted invalid mode %q", tc.input)
		}
	}
	m := testBrowserManager(t)
	isolated := m.runCommand("console.log('isolated')", false, "isolated")
	shared := m.runCommand("console.log('shared')", false)
	for _, sample := range []struct{ name, script, mode string }{
		{"isolated", isolated, "isolated"}, {"shared", shared, "shared"},
	} {
		if !strings.Contains(sample.script, "profile_mode='"+sample.mode+"'") {
			t.Fatalf("%s profile selection absent", sample.name)
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(sample.script)
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s shell syntax: %v %s", sample.name, err, data)
		}
	}
	if !strings.Contains(isolated, `profile="$run_tmp/isolated-profile"`) ||
		!strings.Contains(isolated, `if [ "$profile_mode" = shared ]; then cleanup_cache; fi`) ||
		!strings.Contains(isolated, `rm -rf -- "$run_tmp"`) {
		t.Fatal("private profile isolation/cleanup not tied to the bounded disposable run")
	}
	resp, err := m.Run(context.Background(), RunOptions{ProfileMode: "untrusted"})
	if err != nil || resp.OK || resp.ErrorCode != "invalid_browser_profile_mode" {
		t.Fatalf("invalid mode did not fail before browser runtime: %+v %v", resp, err)
	}
	session, err := m.SessionOpen(context.Background(), SessionOptions{ProfileMode: "untrusted"})
	if err != nil || session.OK || session.ErrorCode != "invalid_browser_profile_mode" {
		t.Fatalf("invalid session mode did not fail before admission: %+v %v", session, err)
	}
}

func TestManagedBrowserIsolatedSessionRunnerCleanupContract(t *testing.T) {
	script, err := managedSessionRunner("/opt/runtime", "/var/agent/browser/profile", "/var/agent/browser/tmp", false, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range []string{
		`const profileMode = "isolated"`,
		`mkdtempSync(join(downloads, 'asa-profile-'))`,
		`isolatedProfile || profile`,
		`const executorOwnsScratch = process.env.ASA_BROWSER_RECLAIM_BY_EXECUTOR === '1'`,
		`ASA_BROWSER_SESSION_PROFILE`,
		`ASA_BROWSER_SESSION_TRACE`,
		`browser_trace_identity_unavailable`,
		`cleanupProfile();`,
		`mkdirSync(isolatedProfile, {mode:0o700})`,
		`/^asa-profile-[A-Za-z0-9]{6}$/`,
		`bytes > 256 * 1024 * 1024`,
		`seen > 8192`,
		`!diskSafe() || !privateProfileSafe()`,
	} {
		if !strings.Contains(script, piece) {
			t.Fatalf("isolated session missing %q", piece)
		}
	}
	shared, err := managedSessionRunner("/opt/runtime", "/var/agent/browser/profile", "/var/agent/browser/tmp", false)
	if err != nil || !strings.Contains(shared, `const profileMode = "shared"`) {
		t.Fatalf("shared default changed: %v", err)
	}
}

// This opt-in test exports the literal production-generated shell wrapper for
// the separate Executor root-cgroup acceptance. No public runtime API or
// cross-package production dependency is needed for the combined proof.
func TestExportApprovedRootProductionBrowserWrapper(t *testing.T) {
	host, _ := os.Hostname()
	root := os.Getenv("ASA_BROWSER_ROOT_PRODUCTION_TEST_ROOT")
	if host != "hDE01" || os.Geteuid() != 0 ||
		os.Getenv("ASA_197_BROWSER_CGROUP_APPROVED_HOST") != host ||
		!strings.HasPrefix(root, "/var/tmp/asa-browser-owned-root-") ||
		filepath.Clean(root) != root {
		t.Skip("requires an approved disposable hDE01 root fixture")
	}
	workspace := filepath.Join(root, "sources", "workspace")
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("disposable fixture workspace is not a real directory")
	}
	marker := filepath.Join(workspace, "production-oneshot.pid")
	source := "const { spawn } = await import('node:child_process');\n" +
		"const { writeFileSync } = await import('node:fs');\n" +
		"const child = spawn('/bin/sleep', ['90'], {stdio:'ignore', detached:true});\n" +
		fmt.Sprintf("writeFileSync(%q, String(child.pid)); child.unref(); process.exit(7);\n", marker)
	manager := &Manager{
		enginePath: "/opt/ai-server-agent/browser",
		dataPath:   filepath.Join(root, "state", "runtime", "browser"),
	}
	output := filepath.Join(workspace, "production-oneshot.sh")
	if err := os.WriteFile(output, []byte(manager.runCommand(source, false, "isolated")), 0600); err != nil {
		t.Fatal(err)
	}
}
