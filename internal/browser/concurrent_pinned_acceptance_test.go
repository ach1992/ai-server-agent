package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Linux MemAvailable is a point-in-time host headroom proxy, not an
// attribution of all Chromium shared memory. The acceptance records both
// baseline and two-browser readings on an authorized small test host.
func browserAvailableMemoryKiB() (int64, error) {
	content, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			break
		}
		return strconv.ParseInt(fields[1], 10, 64)
	}
	return 0, fmt.Errorf("MemAvailable is not inspectable")
}

// Real Chromium concurrency and storage-isolation acceptance. Explicit
// disposable-host opt-in; never uses the installed Agent's shared profile.
// It exercises the Executor-owned cleanup contract (no per-Node global sweep)
// with both browsers alive at once, not merely two mocked broker handles.
func TestApprovedConcurrentRealIsolatedChromiumProfiles(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	expectedHost := os.Getenv("ASA_197_BROWSER_CONCURRENT_APPROVED_HOST")
	hostname, _ := os.Hostname()
	if engine == "" || expectedHost == "" || expectedHost != hostname {
		t.Skip("requires pinned runtime and exact disposable test-host opt-in")
	}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><body><button onclick="localStorage.setItem('owner','alpha');document.cookie='alpha=1;path=/';document.querySelector('#state').textContent='alpha'">Save</button><p id="state"></p><script>document.querySelector('#state').textContent=(localStorage.getItem('owner') || 'empty')+'_'+(document.cookie.includes('alpha=1') ? 'cookie':'none')</script></body></html>`)
	}))
	defer web.Close()
	work := t.TempDir()
	downloads := filepath.Join(work, "tmp")
	shared := filepath.Join(work, "shared-do-not-touch")
	for _, dir := range []string{downloads, shared} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, "sentinel"), []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := managedSessionRunner(engine, shared, downloads, false, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(work, "source.mjs")
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	type worker struct {
		cmd    *exec.Cmd
		in     io.WriteCloser
		out    *bufio.Reader
		done   bool
		stderr strings.Builder
	}
	instances := make([]*worker, 0, 2)
	closeWorker := func(w *worker) {
		if w == nil || w.done {
			return
		}
		_, _ = io.WriteString(w.in, "{\"type\":\"close\"}\n")
		_ = w.in.Close()
		_ = w.cmd.Wait()
		w.done = true
	}
	defer func() {
		for _, w := range instances {
			closeWorker(w)
		}
	}()
	launch := func() *worker {
		t.Helper()
		cmd := exec.CommandContext(ctx, filepath.Join(engine, "node/bin/node"), script)
		cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"), "ASA_BROWSER_RECLAIM_BY_EXECUTOR=1", "ASA_BROWSER_SESSION_PROFILE="+fmt.Sprintf("asa-profile-%032x", len(instances)+1),
			"ASA_BROWSER_SESSION_TRACE="+fmt.Sprintf("asa-trace-%032x", len(instances)+1))
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		w := &worker{cmd: cmd, in: in, out: bufio.NewReader(out)}
		cmd.Stderr = &w.stderr
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		instances = append(instances, w)
		line, err := w.out.ReadString('\n')
		if err != nil || !strings.Contains(line, `"event":"ready"`) {
			t.Fatalf("real isolated Chromium failed to start: %q, %v, stderr=%s", line, err, w.stderr.String())
		}
		return w
	}
	send := func(w *worker, nonce string, steps []FlowStep) {
		t.Helper()
		command, err := json.Marshal(map[string]any{"type": "flow", "nonce": nonce, "steps": steps})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.in.Write(append(command, '\n')); err != nil {
			t.Fatal(err)
		}
		frame, err := w.out.ReadString('\n')
		if err != nil {
			t.Fatalf("flow response read: %v, stderr=%s", err, w.stderr.String())
		}
		var envelope struct {
			Event  string `json:"event"`
			Nonce  string `json:"nonce"`
			Result struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			} `json:"result"`
		}
		if !strings.HasPrefix(frame, "ASA_BROWSER_SESSION ") || json.Unmarshal([]byte(strings.TrimPrefix(frame, "ASA_BROWSER_SESSION ")), &envelope) != nil || envelope.Event != "result" || envelope.Nonce != nonce || !envelope.Result.OK {
			t.Fatalf("isolated flow mismatch: %s stderr=%s", frame, w.stderr.String())
		}
	}
	baselineKiB, err := browserAvailableMemoryKiB()
	if err != nil {
		t.Fatalf("inspect disposable host memory: %v", err)
	}
	first := launch()
	second := launch()
	activeKiB, err := browserAvailableMemoryKiB()
	if err != nil {
		t.Fatalf("inspect active Chromium memory: %v", err)
	}
	t.Logf("CHROMIUM_2_CPU_4GIB_MEASUREMENT cpus=%d available_baseline_mib=%d available_2_live_mib=%d delta_mib=%d", runtime.NumCPU(), baselineKiB/1024, activeKiB/1024, (baselineKiB-activeKiB)/1024)
	if activeKiB < 512*1024 {
		t.Fatalf("two Chrome contexts left less than 512 MiB host headroom: %d KiB", activeKiB)
	}
	profileCount := 0
	if entries, err := os.ReadDir(downloads); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "asa-profile-") {
				profileCount++
			}
		}
	} else {
		t.Fatal(err)
	}
	if profileCount != 2 {
		t.Fatalf("expected 2 concurrent private Chromium profiles, got %d", profileCount)
	}
	send(first, strings.Repeat("a", 32), []FlowStep{{Action: "goto", URL: web.URL}, {Action: "click", Role: "button", Name: "Save"}, {Action: "assert_text", Selector: "#state", Expected: "alpha"}})
	send(second, strings.Repeat("b", 32), []FlowStep{{Action: "goto", URL: web.URL}, {Action: "assert_text", Selector: "#state", Expected: "empty_none"}})
	send(first, strings.Repeat("c", 32), []FlowStep{{Action: "goto", URL: web.URL}, {Action: "assert_text", Selector: "#state", Expected: "alpha_cookie"}})
	closeWorker(first)
	if entries, err := os.ReadDir(downloads); err != nil {
		t.Fatal(err)
	} else {
		still := 0
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "asa-profile-") {
				still++
			}
		}
		if still != 2 {
			t.Fatalf("standalone worker must retain both Executor-owned profiles until cgroup proof, got %d", still)
		}
	}
	closeWorker(second)
	retained := 0
	if entries, err := os.ReadDir(downloads); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "asa-profile-") {
				retained++
			}
		}
	}
	if retained != 2 {
		t.Fatalf("Executor-owned isolated profiles reclaimed before cgroup proof: retained=%d", retained)
	}
	if b, err := os.ReadFile(filepath.Join(shared, "sentinel")); err != nil || string(b) != "untouched" {
		t.Fatalf("test shared profile changed: %q %v", b, err)
	}
	t.Logf("REAL_CHROMIUM_CONCURRENT_ISOLATION_PASS host=%s profiles=2; both sessions maintained independent localStorage/cookies; worker retained profiles for Executor cleanup", hostname)
}
