package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	executables, ok := browserRuntimeExecutablePaths(engine, desired)
	if !ok {
		t.Fatalf("unsupported test platform %q", desired.Platform)
	}
	for _, executable := range executables {
		if err := os.MkdirAll(filepath.Dir(executable), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
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

func TestBrowserStatusRejectsMissingOrNonExecutableRuntimeExecutable(t *testing.T) {
	for _, tc := range []struct {
		name            string
		breakExecutable func(t *testing.T, path string)
	}{
		{
			name: "missing",
			breakExecutable: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "not-executable",
			breakExecutable: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		for executableIndex, executableName := range []string{"chromium", "headless-shell", "ffmpeg"} {
			t.Run(tc.name+"/"+executableName, func(t *testing.T) {
				m := testBrowserManager(t)
				writeReadyBrowserFixture(t, m)
				executables, ok := browserRuntimeExecutablePaths(m.engineDir(), desiredRuntimeManifest())
				if !ok || len(executables) != 3 {
					t.Fatalf("runtime executable paths = %v, ok=%v", executables, ok)
				}
				tc.breakExecutable(t, executables[executableIndex])
				status := m.Status(context.Background())
				if status.Ready || !strings.Contains(status.Reason, "executable") {
					t.Fatalf("broken executable status = %+v", status)
				}
			})
		}
	}
}

func TestBrowserSetupConvergesWhenRuntimeExecutableMissing(t *testing.T) {
	m := testBrowserManager(t)
	writeReadyBrowserFixture(t, m)
	executables, ok := browserRuntimeExecutablePaths(m.engineDir(), desiredRuntimeManifest())
	if !ok || len(executables) != 3 {
		t.Fatalf("runtime executable paths = %v, ok=%v", executables, ok)
	}
	if err := os.Remove(executables[0]); err != nil {
		t.Fatal(err)
	}
	if status := m.Status(context.Background()); status.Ready {
		t.Fatalf("runtime with missing Chromium executable unexpectedly ready: %+v", status)
	}

	m.token = "test-executor-token"
	socket := filepath.Join(t.TempDir(), "executor.sock")
	m.cfg.ExecutorSocket = socket
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	requestCh := make(chan executor.Request, 1)
	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		var req executor.Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			serverDone <- err
			return
		}
		requestCh <- req
		resp := executor.Response{
			Error:      "setup intentionally not executed by unit-test executor",
			ReasonCode: "command_failed",
			ErrorCode:  "command_failed",
			ErrorClass: "process",
			ExitCode:   2,
		}
		if err := json.NewEncoder(conn).Encode(resp); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	resp, err := m.Setup(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status == "already_current" {
		t.Fatalf("setup skipped convergence for runtime with missing executable: %+v", resp)
	}
	req := <-requestCh
	if req.Command == m.cleanupCommand() || !strings.Contains(req.Command, `stage=$(mktemp -d "$parent/.browser-stage.XXXXXX")`) {
		t.Fatalf("setup did not select convergence command after readiness failed")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
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

	maxMS := int64(maxBrowserRunTimeout / time.Millisecond)
	got, err = normalizeRunTimeout(maxMS)
	if err != nil || got != maxBrowserRunTimeout {
		t.Fatalf("maximum timeout = %v, %v", got, err)
	}

	for name, ms := range map[string]int64{
		"negative":               -1,
		"one-over-maximum":       maxMS + 1,
		"max-int64":              math.MaxInt64,
		"duration-overflow-edge": math.MaxInt64/int64(time.Millisecond) + 1,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeRunTimeout(ms); err == nil {
				t.Fatalf("timeout_ms=%d unexpectedly accepted", ms)
			}
		})
	}

	for _, ms := range []int64{1, 1234, maxMS} {
		got, err := normalizeRunTimeout(ms)
		if err != nil {
			t.Fatalf("timeout_ms=%d rejected: %v", ms, err)
		}
		if got <= 0 || got > maxBrowserRunTimeout {
			t.Fatalf("timeout_ms=%d normalized outside browser bounds: %v", ms, got)
		}
		if executorMS := int64(got / time.Millisecond); executorMS <= 0 || executorMS > maxMS {
			t.Fatalf("timeout_ms=%d produced executor TimeoutMS=%d", ms, executorMS)
		}
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
	if !strings.Contains(secure, "AI_SERVER_AGENT_BROWSER_PROFILE") ||
		!strings.Contains(secure, "AI_SERVER_AGENT_BROWSER_DOWNLOADS") ||
		!strings.Contains(secure, "cd \"$run_tmp\"") ||
		strings.Contains(secure, "launchPersistentContext('./profile'") {
		t.Fatalf("per-run cwd/persistent-profile contract missing: %s", secure)
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

func TestBrowserAlreadyCurrentSetupHonorsLifecycleLock(t *testing.T) {
	m := testBrowserManager(t)
	writeReadyBrowserFixture(t, m)

	lockPath := filepath.Join(t.TempDir(), "management.lock")
	if err := os.WriteFile(lockPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	m.lifecycleLockPath = lockPath
	m.lifecycleLockStat = fmt.Sprintf("%d:%d:600", os.Geteuid(), os.Getegid())
	m.token = "test-executor-token"

	socket := filepath.Join(t.TempDir(), "executor.sock")
	m.cfg.ExecutorSocket = socket
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		for range 2 {
			conn, err := ln.Accept()
			if err != nil {
				serverDone <- err
				return
			}
			var req executor.Request
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				_ = conn.Close()
				serverDone <- err
				return
			}
			cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-s")
			cmd.Stdin = strings.NewReader(req.Command)
			out, runErr := cmd.CombinedOutput()
			resp := executor.Response{OK: runErr == nil}
			if runErr != nil {
				resp.Error = strings.TrimSpace(string(out))
				resp.ReasonCode = "command_failed"
				resp.ErrorCode = "command_failed"
				resp.ErrorClass = "process"
				var exitErr *exec.ExitError
				if errors.As(runErr, &exitErr) {
					resp.ExitCode = exitErr.ExitCode()
				} else {
					resp.ExitCode = -1
				}
			}
			if err := json.NewEncoder(conn).Encode(resp); err != nil {
				_ = conn.Close()
				serverDone <- err
				return
			}
			if err := conn.Close(); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()

	parent := filepath.Dir(m.engineDir())
	stage := filepath.Join(parent, ".browser-stage.active")
	backup := filepath.Join(parent, ".browser-old.active")
	tmpArtifact := filepath.Join(m.dataDir(), "tmp", "active")
	npmCache := filepath.Join(m.engineDir(), ".npm-cache")
	for _, dir := range []string{stage, backup, tmpArtifact, npmCache} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}

	lockFile, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	resp, err := m.Setup(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.ErrorCode != "resource_limit" || resp.ErrorClass != "resource" || !resp.Retryable || resp.Status != "busy" {
		t.Fatalf("already-current setup with held lifecycle lock = %+v", resp)
	}
	for _, path := range []string{stage, backup, tmpArtifact, npmCache} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("already-current setup mutated %s while lifecycle lock was held: %v", path, statErr)
		}
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	resp, err = m.Setup(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Status != "already_current" {
		t.Fatalf("already-current setup after lifecycle lock release = %+v", resp)
	}
	for _, path := range []string{stage, backup, tmpArtifact, npmCache} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("already-current setup left %s after lock release: %v", path, statErr)
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
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
	captureDir := t.TempDir()
	capture := filepath.Join(captureDir, "runner.mjs")
	captureCWD := filepath.Join(captureDir, "cwd")
	profileSentinel := filepath.Join(m.dataDir(), "profile", "sentinel")
	if err := os.WriteFile(profileSentinel, []byte("durable"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeNode := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" > \"$CAPTURE_CWD\"\nprintf 'relative-artifact' > relative-artifact.txt\ncp -- \"$1\" \"$CAPTURE\"\n"
	if err := os.WriteFile(filepath.Join(m.engineDir(), "node", "bin", "node"), []byte(fakeNode), 0755); err != nil {
		t.Fatal(err)
	}

	script := `console.log("ephemeral-marker")`
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-s")
	cmd.Stdin = strings.NewReader(m.runCommand(script, false))
	cmd.Env = append(os.Environ(), "CAPTURE="+capture, "CAPTURE_CWD="+captureCWD)
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
	cwdBytes, err := os.ReadFile(captureCWD)
	if err != nil {
		t.Fatal(err)
	}
	cwd := strings.TrimSpace(string(cwdBytes))
	if !strings.HasPrefix(cwd, filepath.Join(m.dataDir(), "tmp", "run.")) {
		t.Fatalf("browser runner cwd = %q, want per-run temp directory", cwd)
	}
	if got, err := os.ReadFile(profileSentinel); err != nil || string(got) != "durable" {
		t.Fatalf("durable browser profile sentinel = %q err=%v", got, err)
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
	profileSentinel := filepath.Join(m.dataDir(), "profile", "sentinel")
	if err := os.WriteFile(profileSentinel, []byte("durable"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeNode := "#!/bin/sh\ntruncate -s 300M ./relative.bin\nexit 0\n"
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
	if got, readErr := os.ReadFile(profileSentinel); readErr != nil || string(got) != "durable" {
		t.Fatalf("durable browser profile sentinel after resource limit = %q err=%v", got, readErr)
	}
}

func browserSetupRecoveryBlock(t *testing.T, m *Manager) string {
	t.Helper()
	command, err := m.setupCommand()
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(command, `if [ ! -e "$engine" ]; then`)
	endMarker := `stage=$(mktemp -d "$parent/.browser-stage.XXXXXX")`
	end := strings.Index(command, endMarker)
	if start < 0 || end < 0 || start >= end {
		t.Fatalf("setup recovery block not found")
	}
	return command[start:end]
}

func TestBrowserSetupRecoveryRestoresSingleInterruptedBackup(t *testing.T) {
	m := testBrowserManager(t)
	parent := filepath.Dir(m.engineDir())
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(parent, ".browser-old.single")
	stage := filepath.Join(parent, ".browser-stage.stale")
	if err := os.MkdirAll(backup, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "sentinel"), []byte("known-good"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stage, 0755); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf("set -euo pipefail\nfail(){ printf '%%s\\n' \"$*\" >&2; exit 2; }\nparent=%s\nengine=%s\n%s",
		shellQuote(parent), shellQuote(m.engineDir()), browserSetupRecoveryBlock(t, m))
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-s")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recover interrupted browser setup: %v: %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(m.engineDir(), "sentinel"))
	if err != nil || string(got) != "known-good" {
		t.Fatalf("restored runtime = %q err=%v", got, err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("stale stage survived recovery: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(parent, ".browser-old.*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("stale browser backups remain: %v err=%v", matches, err)
	}
}

func TestBrowserSetupRecoveryFailsClosedOnAmbiguousBackups(t *testing.T) {
	m := testBrowserManager(t)
	parent := filepath.Dir(m.engineDir())
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".browser-old.a", ".browser-old.b"} {
		if err := os.MkdirAll(filepath.Join(parent, name), 0755); err != nil {
			t.Fatal(err)
		}
	}

	script := fmt.Sprintf("set -euo pipefail\nfail(){ printf '%%s\\n' \"$*\" >&2; exit 2; }\nparent=%s\nengine=%s\n%s",
		shellQuote(parent), shellQuote(m.engineDir()), browserSetupRecoveryBlock(t, m))
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("ambiguous backup recovery unexpectedly succeeded: %s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("ambiguous backup exit = %v; output=%s", err, out)
	}
	if !strings.Contains(string(out), "multiple browser runtime backups require operator reconciliation") {
		t.Fatalf("ambiguous-backup reason missing: %s", out)
	}
	if _, err := os.Stat(m.engineDir()); !os.IsNotExist(err) {
		t.Fatalf("ambiguous recovery created engine unexpectedly: %v", err)
	}
}
