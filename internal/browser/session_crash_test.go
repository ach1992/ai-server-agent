package browser

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Direct SIGKILL is the actual abrupt Node crash to test. Playwright can
// intercept SIGTERM and retain Node's process while stdin remains open;
// production close uses an explicit protocol frame followed by broker-pinned
// process-group cleanup, not SIGTERM as a completion acknowledgement.
func TestManagedBrowserSessionCrashCleanupPinnedRuntime(t *testing.T) {
	testManagedBrowserCrashSignal(t, syscall.SIGKILL)
}

func testManagedBrowserCrashSignal(t *testing.T, signal syscall.Signal) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("pinned runtime")
	}
	work := t.TempDir()
	profile := filepath.Join(work, "unique-profile")
	downloads := filepath.Join(work, "downloads")
	_ = os.Mkdir(downloads, 0700)
	src, err := managedSessionRunner(engine, profile, downloads, false)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(work, "session.mjs")
	if err = os.WriteFile(file, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(engine, "node/bin/node"), file)
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
	// Keep the managed Node input open until deliberately killing it:
	// EOF would otherwise terminate a session without a real crash.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(pipe)
	got, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ready") {
		t.Fatalf("unexpected startup: %s", got)
	}
	ids := func() []int {
		var pids []int
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err == nil && strings.Contains(string(b), profile) && strings.Contains(string(b), "chrome") && !strings.Contains(string(b), "session.mjs") {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	before := ids()
	if len(before) == 0 {
		t.Fatal("test did not observe launched isolated Chromium")
	}
	t.Logf("isolated chromium processes before Node crash: %v", before)
	if err := cmd.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	waitStarted := time.Now()
	waitErr := cmd.Wait()
	t.Logf("injected Node %s wait=%s result=%v ctx=%v", signal, time.Since(waitStarted), waitErr, ctx.Err())
	if ctx.Err() != nil {
		t.Fatalf("Node did not exit promptly after %s, timeout killed the process instead", signal)
	}
	deadline := time.Now().Add(3 * time.Second)
	after := ids()
	for len(after) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		after = ids()
	}
	for _, pid := range after {
		if syscall.Kill(pid, 0) == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if len(after) != 0 {
		t.Errorf("orphaned Chromium after Node %s (cleaned isolated fixture PIDs): %v", signal, after)
	}
}

// A real SIGKILL must not leave Chromium running. After verified termination,
// the next isolated session may reclaim only test-owned temporary profiles.
func TestIsolatedBrowserProfileReclaimedAfterSIGKILLPinnedRuntime(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("verified pinned browser runtime required")
	}
	work := t.TempDir()
	shared := filepath.Join(work, "shared-profile")
	downloads := filepath.Join(work, "disposable")
	for _, path := range []string{shared, downloads} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sharedSentinel := filepath.Join(shared, "shared-sentinel")
	if err := os.WriteFile(sharedSentinel, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(downloads, "foreign-data")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := managedSessionRunner(engine, shared, downloads, false, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(work, "isolated-crash.mjs")
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	start := func() (*exec.Cmd, io.WriteCloser) {
		t.Helper()
		cmd := exec.CommandContext(ctx, filepath.Join(engine, "node/bin/node"), script)
		cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr != nil || !strings.Contains(ready, "ready") {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("isolated Chromium startup: %q, %v", ready, readErr)
		}
		return cmd, stdin
	}
	profiles := func() []string {
		t.Helper()
		entries, err := os.ReadDir(downloads)
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "asa-profile-") {
				found = append(found, filepath.Join(downloads, entry.Name()))
			}
		}
		return found
	}
	chromiumPIDs := func(profile string) []int {
		var pids []int
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err == nil && strings.Contains(string(cmdline), profile) &&
				strings.Contains(string(cmdline), "chrome") {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	cmd, input := start()
	crashed := false
	defer func() {
		if !crashed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	active := profiles()
	if len(active) != 1 {
		t.Fatalf("expected exactly one live isolated profile, got %v", active)
	}
	if ids := chromiumPIDs(active[0]); len(ids) == 0 {
		t.Fatal("isolated Chromium was not running before the injected SIGKILL")
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	_ = input.Close()
	crashed = true
	deadline := time.Now().Add(3 * time.Second)
	remaining := chromiumPIDs(active[0])
	for len(remaining) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		remaining = chromiumPIDs(active[0])
	}
	if len(remaining) > 0 {
		for _, pid := range remaining {
			_ = syscall.Kill(pid, syscall.SIGKILL) // only PIDs tied to this test-owned profile
		}
		t.Fatalf("Chromium orphaned after SIGKILL; test-owned PIDs terminated: %v", remaining)
	}
	// The next admission owns the reclamation (no new background monitor).
	next, nextInput := start()
	closed := false
	defer func() {
		if !closed {
			_ = next.Process.Kill()
			_ = next.Wait()
		}
	}()
	if _, err := os.Stat(active[0]); !os.IsNotExist(err) {
		t.Fatalf("prior isolated profile was not reclaimed on verified next admission: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("unrelated directory touched by profile cleanup: %v", err)
	}
	if data, err := os.ReadFile(sharedSentinel); err != nil || string(data) != "preserved" {
		t.Fatalf("shared profile was changed: data=%q err=%v", data, err)
	}
	if _, err := nextInput.Write([]byte("{\"type\":\"close\"}\n")); err != nil {
		t.Fatal(err)
	}
	_ = nextInput.Close()
	if err := next.Wait(); err != nil {
		t.Fatalf("successor isolated Browser failed controlled close: %v", err)
	}
	closed = true
	if remaining := profiles(); len(remaining) != 0 {
		t.Fatalf("isolated profile remains after verified close: %v", remaining)
	}
}
