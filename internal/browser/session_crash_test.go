package browser

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Pinned Chromium crash acceptance: Node termination must not orphan Chrome.
func TestManagedBrowserSessionCrashCleanupPinnedRuntime(t *testing.T) {
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
	pipe, _ := cmd.StdoutPipe()
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
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
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
		t.Errorf("orphaned Chromium after Node SIGKILL (cleaned isolated fixture PIDs): %v", after)
	}
}
