package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
)

func TestBrowserEnginePermissionsAllowWorkerReadExecuteWithoutWrite(t *testing.T) {
	engine := filepath.Join(t.TempDir(), "engine")
	nodeDir := filepath.Join(engine, "node")
	binDir := filepath.Join(nodeDir, "bin")
	nodePath := filepath.Join(binDir, "node")
	packagePath := filepath.Join(engine, "package.json")

	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nodeDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodePath, []byte("node"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packagePath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/bash", "-lc", browserEnginePermissionsCommand)
	cmd.Env = append(os.Environ(), "engine="+engine)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("normalize browser engine permissions: %v: %s", err, out)
	}

	assertPerm := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s permissions = %04o, want %04o", path, got, want)
		}
	}

	assertPerm(nodeDir, 0755)
	assertPerm(nodePath, 0755)
	assertPerm(packagePath, 0644)
}

func testBrowserManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = filepath.Join(root, "state")
	cfg.WorkerUser = os.Getenv("USER")
	if cfg.WorkerUser == "" {
		cfg.WorkerUser = "nobody"
	}
	return &Manager{
		cfg:        cfg,
		enginePath: filepath.Join(root, "engine"),
		dataPath:   filepath.Join(root, "data"),
	}
}

func writeReadyBrowserFixture(t *testing.T, m *Manager) {
	t.Helper()
	engine := m.engineDir()
	desired := desiredRuntimeManifest()
	for _, dir := range []string{m.dataDir(), filepath.Join(m.dataDir(), "profile"), filepath.Join(m.dataDir(), "tmp")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{
		filepath.Join(engine, "node", "bin"),
		filepath.Join(engine, "node_modules", "playwright"),
		filepath.Join(engine, "node_modules", "playwright-core"),
		filepath.Join(engine, "browsers", "chromium-"+desired.ChromiumRevision),
		filepath.Join(engine, "browsers", "chromium_headless_shell-"+desired.ChromiumRevision),
		filepath.Join(engine, "browsers", "ffmpeg-"+desired.FFmpegRevision),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engine, "runtime-manifest.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	node := "#!/bin/sh\n[ \"$1\" = --version ] && { printf '%s\\n' '" + desired.NodeVersion + "'; exit 0; }\nexit 2\n"
	if err := os.WriteFile(filepath.Join(engine, "node", "bin", "node"), []byte(node), 0755); err != nil {
		t.Fatal(err)
	}
	pkg, _ := json.Marshal(map[string]string{"version": desired.PlaywrightVersion})
	if err := os.WriteFile(filepath.Join(engine, "node_modules", "playwright", "package.json"), pkg, 0644); err != nil {
		t.Fatal(err)
	}
	registry, _ := json.Marshal(map[string]any{"browsers": []map[string]any{
		{"name": "chromium", "revision": desired.ChromiumRevision, "browserVersion": desired.ChromiumVersion},
		{"name": "ffmpeg", "revision": desired.FFmpegRevision},
	}})
	if err := os.WriteFile(filepath.Join(engine, "node_modules", "playwright-core", "browsers.json"), registry, 0644); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		filepath.Join(engine, "browsers", "chromium-"+desired.ChromiumRevision, "INSTALLATION_COMPLETE"),
		filepath.Join(engine, "browsers", "chromium_headless_shell-"+desired.ChromiumRevision, "INSTALLATION_COMPLETE"),
		filepath.Join(engine, "browsers", "ffmpeg-"+desired.FFmpegRevision, "INSTALLATION_COMPLETE"),
	} {
		if err := os.WriteFile(marker, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBrowserStatusVerifiesPinnedRuntime(t *testing.T) {
	m := testBrowserManager(t)
	writeReadyBrowserFixture(t, m)
	status := m.Status(context.Background())
	if !status.InspectionComplete || !status.Installed || !status.Ready || status.Busy || !status.SharedProfile || status.Reason != "" {
		t.Fatalf("ready status = %+v", status)
	}
	if status.Desired != desiredRuntimeManifest() || status.InstalledState != status.Desired {
		t.Fatalf("runtime manifests differ: %+v", status)
	}
	if status.ProfileDir != filepath.Join(m.dataDir(), "profile") {
		t.Fatalf("profile path = %q", status.ProfileDir)
	}
}

func TestBrowserStatusRejectsStaleManifest(t *testing.T) {
	m := testBrowserManager(t)
	writeReadyBrowserFixture(t, m)
	stale := desiredRuntimeManifest()
	stale.PlaywrightVersion = "0.0.0"
	b, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(m.engineDir(), "runtime-manifest.json"), b, 0644); err != nil {
		t.Fatal(err)
	}
	status := m.Status(context.Background())
	if !status.Installed || status.Ready || !strings.Contains(status.Reason, "manifest") {
		t.Fatalf("stale status = %+v", status)
	}
}

func TestBrowserStatusReportsBusyWithoutQueueing(t *testing.T) {
	m := testBrowserManager(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	status := m.Status(context.Background())
	if !status.Busy || status.InspectionComplete {
		t.Fatalf("busy status = %+v", status)
	}
}

func TestBrowserRunTimeoutBounds(t *testing.T) {
	got, err := normalizeRunTimeout(0)
	if err != nil || got != defaultBrowserRunTimeout {
		t.Fatalf("default timeout = %v, %v", got, err)
	}
	got, err = normalizeRunTimeout(1234)
	if err != nil || got != 1234*time.Millisecond {
		t.Fatalf("explicit timeout = %v, %v", got, err)
	}
	if _, err := normalizeRunTimeout(-1); err == nil {
		t.Fatal("negative timeout accepted")
	}
	if _, err := normalizeRunTimeout(int64(maxBrowserRunTimeout/time.Millisecond) + 1); err == nil {
		t.Fatal("oversized timeout accepted")
	}
}

func TestBrowserRunInputBoundRejectsBeforeRuntime(t *testing.T) {
	m := testBrowserManager(t)
	resp, err := m.Run(context.Background(), RunOptions{Script: strings.Repeat("x", maxBrowserScriptBytes+1)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.ErrorCode != "input_too_large" || resp.ErrorClass != "validation" {
		t.Fatalf("oversized script response = %+v", resp)
	}
}

func TestBrowserSetupRequiresApprovalBeforeInspection(t *testing.T) {
	m := testBrowserManager(t)
	resp, err := m.Setup(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.ErrorCode != "approval_required" || resp.ErrorClass != "approval" {
		t.Fatalf("setup approval response = %+v", resp)
	}
}

func TestBrowserRunCommandTLSAndHandoffContract(t *testing.T) {
	m := testBrowserManager(t)
	script := `console.log("secret-script-marker")`
	secure := m.runCommand(script, false)
	if strings.Contains(secure, script) {
		t.Fatal("raw browser script was copied into executor command text")
	}
	if !strings.Contains(secure, "ignore_https_errors=false") || !strings.Contains(secure, "ignoreHTTPSErrors: $ignore_https_errors") {
		t.Fatalf("secure TLS default missing: %s", secure)
	}
	if !strings.Contains(secure, "base64 -d > \"$body\"") || !strings.Contains(secure, "cat \"$body\"") {
		t.Fatalf("bounded temporary-file handoff missing: %s", secure)
	}
	if !strings.Contains(secure, "--disk-cache-size=67108864") || !strings.Contains(secure, "cleanup_cache") {
		t.Fatalf("disposable cache bound/cleanup missing: %s", secure)
	}

	exception := m.runCommand(script, true)
	if !strings.Contains(exception, "ignore_https_errors=true") {
		t.Fatalf("explicit TLS exception missing: %s", exception)
	}
}

func TestBrowserSetupCommandPinsAndConvergesRuntime(t *testing.T) {
	m := testBrowserManager(t)
	cmd, err := m.setupCommand()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		browserNodeVersion,
		browserNodeSHA256X64,
		browserNodeSHA256ARM64,
		browserPlaywrightVersion,
		browserChromiumRevision,
		browserChromiumVersion,
		desiredRuntimeManifest().ChromiumTreeSHA256,
		desiredRuntimeManifest().HeadlessShellTreeSHA256,
		desiredRuntimeManifest().FFmpegTreeSHA256,
		"npm ci --ignore-scripts --no-audit --no-fund",
		"package-lock.json",
		"runtime-manifest.json",
		"browser build metadata verification failed",
		".browser-stage.",
		".browser-old.",
		"/run/lock/ai-server-agent/management.lock",
		"flock -n 9",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("setup command missing %q", want)
		}
	}
	if strings.Contains(cmd, "playwright@") || strings.Contains(cmd, "npm install --package-lock-only") {
		t.Fatalf("setup command contains floating/regenerated dependency resolution: %s", cmd)
	}
}

func TestBrowserStatusRejectsUnsafeDataPath(t *testing.T) {
	m := testBrowserManager(t)
	writeReadyBrowserFixture(t, m)
	tmp := filepath.Join(m.dataDir(), "tmp")
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), tmp); err != nil {
		t.Fatal(err)
	}
	status := m.Status(context.Background())
	if status.Ready || !strings.Contains(status.Reason, "temp path") {
		t.Fatalf("unsafe temp status = %+v", status)
	}
}

func TestBrowserDiskPressureMapsToResourceLimit(t *testing.T) {
	resp := translateBrowserExecutorResponse(executor.Response{
		Error:      "exit status 73",
		ReasonCode: "command_failed",
		ErrorCode:  "command_failed",
		ErrorClass: "process",
		ExitCode:   browserDiskPressureExit,
	}, "browser execution")
	if resp.OK || resp.ErrorCode != "resource_limit" || resp.ErrorClass != "resource" || !resp.Retryable || resp.Status != "resource_limited" {
		t.Fatalf("disk-pressure response = %+v", resp)
	}
}

func TestBrowserCommandsCarryDiskReserveAndStayBelowExecutorInputBound(t *testing.T) {
	m := testBrowserManager(t)
	setup, err := m.setupCommand()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		strconv.FormatInt(browserSetupMinFreeBytes/1024, 10),
		"engine filesystem free space is below safety reserve",
		"exit " + strconv.Itoa(browserDiskPressureExit),
	} {
		if !strings.Contains(setup, want) {
			t.Fatalf("setup disk contract missing %q", want)
		}
	}
	if len(setup) >= 256<<10 {
		t.Fatalf("setup executor command is %d bytes, must remain below 256 KiB executor input limit", len(setup))
	}

	run := m.runCommand(strings.Repeat("x", maxBrowserScriptBytes), false)
	for _, want := range []string{
		strconv.FormatInt(browserRunMinFreeBytes/1024, 10),
		strconv.FormatInt(browserRunMaxDisposableBytes/1024, 10),
		"du -sk --apparent-size",
		"state filesystem safety reserve reached",
		"disposable browser run data exceeded its bound",
		"exit " + strconv.Itoa(browserDiskPressureExit),
	} {
		if !strings.Contains(run, want) {
			t.Fatalf("run disk contract missing %q", want)
		}
	}
	if len(run) >= 256<<10 {
		t.Fatalf("maximum browser executor command is %d bytes, must remain below 256 KiB executor input limit", len(run))
	}
}

func TestEmbeddedBrowserLockPinsPlaywrightIntegrity(t *testing.T) {
	var lock struct {
		Packages map[string]struct {
			Version   string `json:"version"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	if err := json.Unmarshal([]byte(browserPackageLockJSON), &lock); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"node_modules/playwright", "node_modules/playwright-core"} {
		entry, ok := lock.Packages[path]
		if !ok || entry.Version != browserPlaywrightVersion || entry.Integrity == "" {
			t.Fatalf("%s lock entry = %+v, ok=%v", path, entry, ok)
		}
	}
	if strings.Contains(browserPackageJSON, "^"+browserPlaywrightVersion) {
		t.Fatalf("embedded package.json floats Playwright: %s", browserPackageJSON)
	}
}

func TestBrowserLifecycleBusyMapsToResourceLimit(t *testing.T) {
	resp := translateBrowserExecutorResponse(executor.Response{
		Error:      "exit status 74",
		ReasonCode: "command_failed",
		ErrorCode:  "command_failed",
		ErrorClass: "process",
		ExitCode:   browserLifecycleBusyExit,
	}, "browser setup")
	if resp.OK || resp.ErrorCode != "resource_limit" || resp.ErrorClass != "resource" || !resp.Retryable || resp.Status != "busy" {
		t.Fatalf("lifecycle-busy response = %+v", resp)
	}
}

func TestBrowserGeneratedCommandsHaveValidShellSyntax(t *testing.T) {
	m := testBrowserManager(t)
	setup, err := m.setupCommand()
	if err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]string{
		"setup":   setup,
		"cleanup": m.cleanupCommand(),
		"run":     m.runCommand(`console.log("ok")`, false),
	} {
		cmd := exec.Command("/bin/bash", "-n")
		cmd.Stdin = strings.NewReader(command)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s command syntax: %v: %s\n%s", name, err, out, command)
		}
	}
}

func TestBrowserRunCommandUsesEphemeralRunnerAndCleansIt(t *testing.T) {
	m := testBrowserManager(t)
	for _, dir := range []string{
		filepath.Join(m.engineDir(), "node", "bin"),
		filepath.Join(m.dataDir(), "profile"),
		filepath.Join(m.dataDir(), "tmp"),
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	capture := filepath.Join(t.TempDir(), "runner.mjs")
	fakeNode := "#!/bin/sh\ncp -- \"$1\" \"$CAPTURE\"\n"
	if err := os.WriteFile(filepath.Join(m.engineDir(), "node", "bin", "node"), []byte(fakeNode), 0755); err != nil {
		t.Fatal(err)
	}

	script := `console.log("ephemeral-marker")`
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-s")
	cmd.Stdin = strings.NewReader(m.runCommand(script, false))
	cmd.Env = append(os.Environ(), "CAPTURE="+capture)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run generated browser command: %v: %s", err, out)
	}
	runner, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	text := string(runner)
	if !strings.Contains(text, script) || !strings.Contains(text, "ignoreHTTPSErrors: false") {
		t.Fatalf("runner did not carry expected script/TLS default: %s", text)
	}
	entries, err := os.ReadDir(filepath.Join(m.dataDir(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("browser temp directory was not cleaned: %+v", entries)
	}
}

func TestBrowserRunCommandStopsOversizedDisposableData(t *testing.T) {
	m := testBrowserManager(t)
	for _, dir := range []string{
		filepath.Join(m.engineDir(), "node", "bin"),
		filepath.Join(m.dataDir(), "profile"),
		filepath.Join(m.dataDir(), "tmp"),
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fakeNode := "#!/bin/sh\ntruncate -s 300M \"$(dirname \"$1\")/oversized.bin\"\nexec sleep 5\n"
	if err := os.WriteFile(filepath.Join(m.engineDir(), "node", "bin", "node"), []byte(fakeNode), 0755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-s")
	cmd.Stdin = strings.NewReader(m.runCommand(`console.log("unused")`, false))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("oversized disposable data unexpectedly succeeded: %s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != browserDiskPressureExit {
		t.Fatalf("oversized disposable data exit = %v, want %d; output=%s", err, browserDiskPressureExit, out)
	}
	if !strings.Contains(string(out), "disposable browser run data exceeded its bound") {
		t.Fatalf("resource-limit output missing: %s", out)
	}
	entries, readErr := os.ReadDir(filepath.Join(m.dataDir(), "tmp"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("browser temp directory was not cleaned after limit: %+v", entries)
	}
}
