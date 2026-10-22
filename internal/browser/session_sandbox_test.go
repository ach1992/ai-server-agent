package browser

import (
	"context"
	"strings"
	"testing"
)

// Source/API validation does not claim kernel enforcement. The root Executor
// performs the separate, actual cgroup-bound Chromium renderer attestation.
func TestIsolatedManagedChromiumSandboxSourceAndProfileGate(t *testing.T) {
	const engine = "/opt/browser"
	const profile = "/state/browser/profile"
	const downloads = "/state/browser/isolated"
	on, err := managedSessionRunnerWithSandbox(engine, profile, downloads, false, "isolated", true)
	if err != nil {
		t.Fatal(err)
	}
	off, err := managedSessionRunnerWithSandbox(engine, profile, downloads, false, "isolated", false)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := managedSessionRunner(engine, profile, downloads, false, "shared")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"const chromiumSandbox = true",
		"headless: true, ignoreHTTPSErrors: false, chromiumSandbox",
		"await page.evaluate(() => true)",
		"browser_sandbox_unavailable",
	} {
		if !strings.Contains(on, fragment) {
			t.Fatalf("requested sandbox runner missing %q", fragment)
		}
	}
	if strings.Contains(on, "--no-sandbox") && !strings.Contains(on, "// A requested OS sandbox is never silently retried") {
		t.Fatal("explicit sandbox runner unexpectedly enables --no-sandbox")
	}
	for name, src := range map[string]string{"off": off, "shared": shared} {
		if !strings.Contains(src, "const chromiumSandbox = false") {
			t.Fatalf("%s baseline Chromium policy changed", name)
		}
	}
	if _, err := managedSessionRunnerWithSandbox(engine, profile, downloads, false, "shared", true); err == nil {
		t.Fatal("shared Chromium sandbox request accepted")
	}
	m := testBrowserManager(t)
	for _, mode := range []string{"", "shared"} {
		result, err := m.SessionOpen(context.Background(), SessionOptions{Workspace: "/tmp", ProfileMode: mode, ChromiumSandbox: true})
		if err != nil || result.OK || result.ErrorCode != "browser_sandbox_requires_isolated_profile" {
			t.Fatalf("sandbox on shared mode %q did not fail before runtime admission: %+v %v", mode, result, err)
		}
	}
}
