package browser

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Standalone aiworker probe: production-generated isolated Node runner +
// Playwright option. The ROOT Executor's aibrowser/cgroup attestation is a
// separate acceptance gate and cannot be claimed from this subprocess test.
func TestIsolatedChromiumSandboxPinnedWorkerCompatibility(t *testing.T) {
	if os.Getenv("ASA_BROWSER_SANDBOX_PINNED_WORKER_TEST") != "1" {
		t.Skip("explicit opt-in for pinned standalone worker compatibility")
	}
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Fatal("pinned Browser runtime required")
	}
	dir := t.TempDir()
	downloads := filepath.Join(dir, "downloads")
	if err := os.Mkdir(downloads, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := managedSessionRunnerWithSandbox(engine, filepath.Join(dir, "shared"), downloads, false, "isolated", true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "managed.mjs")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(engine, "node", "bin", "node"), path)
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"event":"ready"`) {
		t.Fatalf("sandboxed generated runner did not reach ready: %v %q stderr=%s", err, line, stderr.String())
	}
	_, err = fmt.Fprintf(stdin, "%s\n", `{"type":"flow","nonce":"0123456789abcdef0123456789abcdef","steps":[{"action":"snapshot"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(result, `"event":"result"`) {
		t.Fatalf("sandboxed managed FlowStep failed: %v %q stderr=%s", err, result, stderr.String())
	}
	_, err = fmt.Fprintf(stdin, "%s\n", `{"type":"close"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Chromium did not close cleanly: %v stderr=%s", err, stderr.String())
	}
	t.Log("PINNED_SANDBOXED_MANAGED_WORKER_PROBE_OK; NOT aibrowser/Root Executor attestation")
}
