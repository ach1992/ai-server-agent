package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in *real pinned engine* check with private temp profiles and a local
// test site. Never open or mutate the installed Agent's shared Browser profile.
func TestIsolatedManagedBrowserRealProfileSeparation(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("set AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME to verified pinned runtime")
	}
	node := filepath.Join(engine, "node", "bin", "node")
	if _, err := os.Stat(node); err != nil {
		t.Fatalf("verified pinned Node missing: %v", err)
	}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><html><body>
<button onclick="localStorage.setItem('isolated-key','persisted'); document.cookie='isolated-cookie=yes; path=/; max-age=3600'; document.querySelector('#status').textContent='stored'">Save</button>
<p id="status"></p>
<script>document.querySelector('#status').textContent=(localStorage.getItem('isolated-key') || 'empty') + '_' + (document.cookie.includes('isolated-cookie=yes') ? 'cookie' : 'noCookie')</script>
</body></html>`)
	}))
	defer web.Close()
	state := t.TempDir()
	shared := filepath.Join(state, "shared-profile")
	downloads := filepath.Join(state, "downloads")
	for _, dir := range []string{shared, downloads} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// These two directories are not ours to clean.
	foreign := filepath.Join(downloads, "arbitrary-project-data")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	sharedSentinel := filepath.Join(shared, "sentinel-do-not-delete")
	if err := os.WriteFile(sharedSentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}

	run := func(mode string, steps []FlowStep) {
		t.Helper()
		script, err := managedSessionRunner(engine, shared, downloads, false, mode)
		if err != nil {
			t.Fatal(err)
		}
		scriptFile := filepath.Join(state, "runner-"+mode+".mjs")
		if err := os.WriteFile(scriptFile, []byte(script), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, node, scriptFile)
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
		reaped := false
		defer func() {
			if !reaped {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		reader := bufio.NewReader(stdout)
		read := func() map[string]any {
			t.Helper()
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("browser frame unavailable: %v, stderr=%s", err, stderr.String())
			}
			if !strings.HasPrefix(line, "ASA_BROWSER_SESSION ") {
				t.Fatalf("untrusted Browser frame %q", line)
			}
			var value map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "ASA_BROWSER_SESSION ")), &value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		if first := read(); first["event"] != "ready" {
			t.Fatalf("Browser readiness failure: %+v stderr=%s", first, stderr.String())
		}
		input, _ := json.Marshal(map[string]any{
			"type": "flow", "nonce": strings.Repeat("a", 32), "steps": steps,
		})
		if _, err := stdin.Write(append(input, '\n')); err != nil {
			t.Fatal(err)
		}
		got := read()
		if got["event"] != "result" || got["nonce"] != strings.Repeat("a", 32) {
			t.Fatalf("Browser E2E protocol failed: %+v stderr=%s", got, stderr.String())
		}
		result, ok := got["result"].(map[string]any)
		if !ok || result["ok"] != true {
			t.Fatalf("real Browser state assertion failed: %+v stderr=%s", got, stderr.String())
		}
		if _, err := stdin.Write([]byte("{\"type\":\"close\"}\n")); err != nil {
			t.Fatal(err)
		}
		if err := stdin.Close(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("Browser controlled close failed: %v stderr=%s", err, stderr.String())
		}
		reaped = true
		entries, err := os.ReadDir(downloads)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "asa-profile-") {
				t.Fatalf("Browser private state retained after verified normal close: %s", entry.Name())
			}
		}
		if b, err := os.ReadFile(sharedSentinel); err != nil || string(b) != "preserve" {
			t.Fatalf("shared Browser profile unexpectedly altered: %q err=%v", b, err)
		}
		if _, err := os.Stat(foreign); err != nil {
			t.Fatal("unrelated Agent-owned directory removed")
		}
	}
	save := []FlowStep{
		{Action: "goto", URL: web.URL},
		{Action: "click", Role: "button", Name: "Save"},
		{Action: "assert_text", Selector: "#status", Expected: "stored"},
	}
	empty := []FlowStep{
		{Action: "goto", URL: web.URL},
		{Action: "assert_text", Selector: "#status", Expected: "empty_noCookie"},
	}
	existing := []FlowStep{
		{Action: "goto", URL: web.URL},
		{Action: "assert_text", Selector: "#status", Expected: "persisted_cookie"},
	}
	run("shared", save)
	run("isolated", empty)
	run("isolated", save)
	// The first isolated run's state must not survive to the next run.
	run("isolated", empty)
	// A verified prior shared Browser profile must still retain both stores.
	run("shared", existing)
	stale := filepath.Join(downloads, "asa-profile-ABC123")
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "orphan"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	run("isolated", empty)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("prior Agent-owned orphaned ephemeral profile not safely retired: %v", err)
	}
}
