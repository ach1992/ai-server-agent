package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
)

// This opt-in acceptance uses real Chromium and the production executor in a
// separate, test-owned process. It never runs setup or changes installed Agent
// state. Its reconstructed fixture manifest proves execution compatibility;
// installer integrity/convergence is a separate acceptance obligation.
func TestBrowserPinnedRuntimeAcceptance(t *testing.T) {
	runtimeDir := os.Getenv("AI_SERVER_AGENT_BROWSER_ACCEPTANCE_RUNTIME")
	if runtimeDir == "" {
		t.Skip("set AI_SERVER_AGENT_BROWSER_ACCEPTANCE_RUNTIME to a verified pinned runtime")
	}
	if os.Geteuid() != 0 {
		t.Fatal("isolated production executor acceptance requires root to drop to the worker user")
	}
	worker := os.Getenv("AI_SERVER_AGENT_BROWSER_ACCEPTANCE_WORKER")
	if worker == "" {
		worker = "aiworker"
	}
	u, err := user.Lookup(worker)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil || uid == 0 {
		t.Fatal("browser acceptance worker must be a valid non-root user")
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir, err = filepath.Abs(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(runtimeDir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("runtime source must be a real directory")
	}
	// Ensure the fixture depends on the exact locked package identities rather
	// than trusting the runtime's possibly historical root dependency range.
	var installedLock, desiredLock struct {
		Packages map[string]struct {
			Version   string `json:"version"`
			Resolved  string `json:"resolved"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	lockBytes, err := os.ReadFile(filepath.Join(runtimeDir, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lockBytes, &installedLock); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(browserPackageLockJSON), &desiredLock); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node_modules/playwright", "node_modules/playwright-core"} {
		got, ok := installedLock.Packages[name]
		if !ok || got != desiredLock.Packages[name] {
			t.Fatalf("runtime lock identity mismatch for %s", name)
		}
	}

	root, err := os.MkdirTemp("/tmp", "asa42-browser-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	for _, path := range []string{data, filepath.Join(data, "profile"), filepath.Join(data, "tmp"), filepath.Join(root, "workspace")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	engine := filepath.Join(root, "engine")
	if err := os.Mkdir(engine, 0755); err != nil {
		t.Fatal(err)
	}
	// The privileged test runner may inherit a restrictive umask. The worker
	// must still traverse the root-owned fixture engine, as in production.
	if err := os.Chmod(engine, 0755); err != nil {
		t.Fatal(err)
	}
	// The immutable installed runtime remains read-only. Only the test-owned
	// engine directory, generated runner, profile, cache and temp can be written.
	for _, name := range []string{"node", "node_modules", "browsers"} {
		if err := os.Symlink(filepath.Join(runtimeDir, name), filepath.Join(engine, name)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(desiredRuntimeManifest())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engine, "runtime-manifest.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("runtime compatibility fixture: %s", manifest)
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), browserStatusTimeout)
	probe := exec.CommandContext(probeCtx, filepath.Join(engine, "node", "bin", "node"), "--version")
	probe.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{uint32(gid)}}}
	probe.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	probeOutput, probeErr := probe.Output()
	cancelProbe()
	if probeErr != nil || strings.TrimSpace(string(probeOutput)) != browserNodeVersion {
		t.Fatalf("worker runtime readiness probe failed: err=%v output=%q", probeErr, probeOutput)
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(tokenBytes)
	cfg := config.Default()
	cfg.WorkerUser, cfg.AgentUser = worker, worker
	cfg.WorkspaceDir = filepath.Join(root, "workspace")
	cfg.StateDir, cfg.LogDir = filepath.Join(root, "state"), filepath.Join(root, "logs")
	cfg.ExecutorSocket = filepath.Join(root, "executor.sock")
	cfg.ExecutorToken = filepath.Join(root, "executor.token")
	if err := os.WriteFile(cfg.ExecutorToken, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestBrowserAcceptanceExecutorHelper$")
	helper.Env = append(os.Environ(), "AI_SERVER_AGENT_BROWSER_ACCEPTANCE_HELPER="+configPath)
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	helper.Stdout, helper.Stderr = os.Stderr, os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- helper.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = helper.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", cfg.ExecutorSocket, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("isolated executor did not become ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	m := &Manager{cfg: cfg, token: token, enginePath: engine, dataPath: data}
	status := m.Status(context.Background())
	if !status.Ready || !status.SharedProfile || status.InstalledState != status.Desired {
		t.Fatalf("runtime fixture is not ready: %+v", status)
	}
	response := func(opts RunOptions) executor.Response {
		t.Helper()
		resp, err := m.Run(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("run: ok=%t error_code=%s exit=%d timed_out=%t bytes_seen=%d bytes_returned=%d truncated=%t duration_ms=%d",
			resp.OK, resp.ErrorCode, resp.ExitCode, resp.TimedOut, resp.BytesSeen, resp.BytesReturned, resp.Truncated, resp.DurationMS)
		return resp
	}
	assertClean := func() {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(data, "tmp"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("disposable run artifacts remain: %v err=%v", entries, err)
		}
		if _, err := os.Stat(filepath.Join(data, "profile", "Default", "Local Storage")); err != nil {
			t.Fatalf("persistent profile local storage was lost: %v", err)
		}
	}
	httpFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "<title>Browser acceptance</title>")
	}))
	defer httpFixture.Close()
	tlsFixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "<title>Scoped TLS fixture</title>")
	}))
	defer tlsFixture.Close()
	urlJSON, _ := json.Marshal(httpFixture.URL)
	tlsURLJSON, _ := json.Marshal(tlsFixture.URL)

	t.Run("profile_and_artifact_cleanup", func(t *testing.T) {
		resp := response(RunOptions{Script: fmt.Sprintf(`await page.goto(%s); await page.evaluate(() => localStorage.setItem('acceptance-shared', 'persisted')); await context.addCookies([{name:'acceptance', value:'persisted', url:%s, expires: Math.floor(Date.now()/1000)+3600}]); await page.screenshot({path:'relative.png'}); console.log('PROFILE_WRITTEN');`, urlJSON, urlJSON), TimeoutMS: 15000})
		if !resp.OK || strings.TrimSpace(resp.Output) != "PROFILE_WRITTEN" || resp.Truncated || resp.OutputEncoding != "utf-8" {
			t.Fatalf("profile write: %+v", resp)
		}
		assertClean()
		// A second caller shares the same Agent-wide state, without receiving a
		// fabricated per-client profile or a copied storage snapshot.
		otherCaller := &Manager{cfg: cfg, token: token, enginePath: engine, dataPath: data}
		resp, err := otherCaller.Run(context.Background(), RunOptions{Script: fmt.Sprintf(`await page.goto(%s); const storage = await page.evaluate(() => localStorage.getItem('acceptance-shared')); const cookies = await context.cookies(); if (storage !== 'persisted' || !cookies.some(x => x.name === 'acceptance' && x.value === 'persisted')) throw new Error('profile did not persist'); console.log('PROFILE_SHARED');`, urlJSON), TimeoutMS: 15000})
		if err != nil || !resp.OK || strings.TrimSpace(resp.Output) != "PROFILE_SHARED" {
			t.Fatalf("second caller profile read: err=%v response=%+v", err, resp)
		}
		assertClean()
	})

	t.Run("tls_default_and_scoped_exception", func(t *testing.T) {
		script := fmt.Sprintf(`await page.goto(%s); console.log(await page.title());`, tlsURLJSON)
		for _, ignore := range []bool{false, true, false} {
			resp := response(RunOptions{Script: script, IgnoreHTTPSErrors: ignore, TimeoutMS: 15000})
			if ignore {
				if !resp.OK || !strings.Contains(resp.Output, "Scoped TLS fixture") {
					t.Fatalf("request-scoped TLS exception failed: %+v", resp)
				}
			} else if resp.OK || !strings.Contains(resp.Output, "ERR_CERT_AUTHORITY_INVALID") {
				t.Fatalf("default TLS did not reject untrusted fixture: %+v", resp)
			}
			assertClean()
		}
	})

	t.Run("timeout_busy_and_next_run_recovery", func(t *testing.T) {
		marker := filepath.Join(cfg.WorkspaceDir, "hung-started")
		markerJSON, _ := json.Marshal(marker)
		var resp executor.Response
		var runErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, runErr = m.Run(context.Background(), RunOptions{Script: fmt.Sprintf(`const fs = await import('node:fs/promises'); await fs.writeFile(%s, 'started'); console.log('HUNG_STARTED'); await new Promise(() => {});`, markerJSON), TimeoutMS: 2500})
		}()
		t.Cleanup(wg.Wait)
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("hung browser did not reach its script before the timeout")
			}
			time.Sleep(10 * time.Millisecond)
		}
		busy := response(RunOptions{Script: `console.log('must not run');`, TimeoutMS: 1000})
		if busy.OK || busy.ErrorCode != "resource_limit" || busy.Status != "busy" || !busy.Retryable {
			t.Fatalf("concurrent browser call was not refused: %+v", busy)
		}
		wg.Wait()
		t.Logf("timeout response: error_code=%s timed_out=%t duration_ms=%d output=%q", resp.ErrorCode, resp.TimedOut, resp.DurationMS, resp.Output)
		if runErr != nil || resp.OK || !resp.TimedOut || !strings.Contains(resp.Output, "HUNG_STARTED") || (resp.ErrorCode != "timeout" && resp.ErrorCode != "unknown_completion") {
			t.Fatalf("bounded browser timeout: err=%v response=%+v", runErr, resp)
		}
		if resp.DurationMS > 8000 {
			t.Fatalf("browser timeout recovery exceeded its bounded allowance: %dms", resp.DurationMS)
		}
		recovered := response(RunOptions{Script: `console.log('RECOVERED');`, TimeoutMS: 15000})
		if !recovered.OK || strings.TrimSpace(recovered.Output) != "RECOVERED" {
			t.Fatalf("browser did not recover after timeout: %+v", recovered)
		}
		assertClean()
	})

	t.Run("bounded_noisy_output", func(t *testing.T) {
		resp := response(RunOptions{Script: `await new Promise((resolve, reject) => process.stdout.write(Buffer.concat([Buffer.from('OUTPUT_HEAD\n'), Buffer.alloc(2*1024*1024, 120), Buffer.from('\nOUTPUT_TAIL\n')]), err => err ? reject(err) : resolve()));`, TimeoutMS: 15000})
		if !resp.OK || !resp.Truncated || resp.OutputEncoding != "utf-8" || resp.BytesSeen <= 2<<20 || resp.BytesReturned > 1<<20 || resp.BytesSeen-resp.BytesReturned != resp.OmittedBytes || !strings.HasPrefix(resp.Output, "OUTPUT_HEAD\n") || !strings.HasSuffix(resp.Output, "\nOUTPUT_TAIL\n") {
			t.Fatalf("browser noisy output framing: ok=%t code=%s seen=%d returned=%d omitted=%d truncated=%t", resp.OK, resp.ErrorCode, resp.BytesSeen, resp.BytesReturned, resp.OmittedBytes, resp.Truncated)
		}
		assertClean()
	})

	t.Run("disposable_apparent_size_refusal", func(t *testing.T) {
		// Sparse truncation crosses the apparent-size guard with negligible
		// physical disk use, entirely inside the disposable run directory.
		resp := response(RunOptions{Script: `const fs = await import('node:fs/promises'); const f = await fs.open('./oversized.bin', 'w'); await f.truncate(300*1024*1024); await f.close(); await new Promise(() => {});`, TimeoutMS: 15000})
		if resp.OK || resp.ErrorCode != "resource_limit" || resp.Status != "resource_limited" || !strings.Contains(resp.Output, "disposable browser run data exceeded its bound") {
			t.Fatalf("browser disposable-size guard: %+v", resp)
		}
		assertClean()
	})
}

func TestBrowserAcceptanceExecutorHelper(t *testing.T) {
	configPath := os.Getenv("AI_SERVER_AGENT_BROWSER_ACCEPTANCE_HELPER")
	if configPath == "" {
		t.Skip("helper is started only by the opt-in browser runtime acceptance")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(cfg.ExecutorToken)
	if err != nil {
		t.Fatal(err)
	}
	s, err := executor.NewServer(cfg, string(token))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(); err != nil {
		t.Fatal(err)
	}
}
