package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

// Intentionally opt-in and hDE01-only: a root-owned disposable test instance
// of the production Executor lives inside its actual systemd service cgroup.
// Never changes or restarts the installed Agent service or global profiles.
func approvedBrowserCgroupRootFixture(t *testing.T) (*Server, Request, string, string) {
	t.Helper()
	host, _ := os.Hostname()
	if host != "hDE01" || os.Geteuid() != 0 || os.Getenv("ASA_197_BROWSER_CGROUP_APPROVED_HOST") != "hDE01" {
		t.Skip("requires explicit root/disposable hDE01 Browser cgroup opt-in")
	}
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || strings.TrimSpace(string(cgroup)) != "0::/system.slice/ai-server-agent-executor.service" {
		t.Skip("requires the installed Executor's exact kernel cgroup, without modifying its service")
	}
	for _, file := range []string{"/opt/ai-server-agent/browser/node/bin/node", "/opt/ai-server-agent/browser/node_modules/playwright/index.mjs", "/opt/ai-server-agent/browser/browsers/chromium_headless_shell-1228/chrome-headless-shell-linux64/chrome-headless-shell"} {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("disposable fixture pinned runtime missing: %s: %v", file, err)
		}
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(worker.Uid)
	if err != nil || uid == 0 {
		t.Fatalf("worker UID invalid: %v", err)
	}
	gid, err := strconv.Atoi(worker.Gid)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/var/tmp", "asa-browser-owned-root-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Test failures could leave a still-live child. Do not delete the
		// corresponding private Browser files without whole-cgroup proof.
		if t.Failed() {
			t.Logf("retaining failed root fixture for exact operator reconciliation: %s", root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("disposable root cleanup: %v", err)
		}
	})
	sources := filepath.Join(root, "sources")
	ws := filepath.Join(sources, "workspace")
	state := filepath.Join(root, "state")
	home := filepath.Join(state, "worker-home")
	tmp := filepath.Join(state, "runtime", "browser", "tmp")
	for _, dir := range []string{sources, state, filepath.Dir(tmp)} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{ws, home, tmp} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{
		cfg:       config.Config{WorkspaceDir: sources, StateDir: state},
		workerUID: uint32(uid), workerGID: uint32(gid),
		sessions: newStdioSessionBroker(), runs: newRunLimiterWith(2, 1),
		guard: policy.New(nil), audit: audit.New(filepath.Join(state, "audit.jsonl")),
	}
	req := Request{Action: "browser_session_open", PrincipalID: "cgroup-proof-user", PrincipalClass: "direct", RequestID: "root-browser-cgroup-proof", Workspace: ws, BrowserProfileMode: "isolated"}
	return s, req, tmp, root
}

func rootBrowserPersistentScript(profile, marker string) string {
	return fmt.Sprintf(`import { chromium } from 'file:///opt/ai-server-agent/browser/node_modules/playwright/index.mjs';
import { spawn } from 'node:child_process';
import { writeFileSync } from 'node:fs';
const context = await chromium.launchPersistentContext(%q,{headless:true,
  executablePath:'/opt/ai-server-agent/browser/browsers/chromium_headless_shell-1228/chrome-headless-shell-linux64/chrome-headless-shell'});
await context.newPage();
// This disposable, independently grouped child deliberately survives abrupt
// Node death long enough to expose an incorrectly premature admission release.
const child=spawn('/usr/bin/setsid',['/bin/sleep','90'],{stdio:'ignore',detached:true});
writeFileSync(%q,String(child.pid)); child.unref();
process.stdout.write('ASA_BROWSER_SESSION '+JSON.stringify({event:'ready'})+'\n');
setInterval(()=>{},1000);`, profile, marker)
}

func browserFixtureWorkerPID(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 0 {
				t.Fatalf("disposable detached fixture PID invalid: %q %v", data, err)
			}
			return pid
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("disposable detached child marker missing %s", marker)
	return 0
}

func TestApprovedRootBrowserDetachedChromiumCrashRetainsAdmission(t *testing.T) {
	s, req, tmp, _ := approvedBrowserCgroupRootFixture(t)
	profile := filepath.Join(tmp, "asa-profile-Roo123")
	marker := filepath.Join(req.Workspace, "detached.pid")
	req.Content = rootBrowserPersistentScript(profile, marker)
	blocked := make(chan struct{})
	allow := make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(allow) }) })
	s.browserContainmentBeforeKillTestHook = func() error { close(blocked); <-allow; return nil }
	opened := s.browserSessionAction(context.Background(), req)
	if !opened.OK {
		t.Fatalf("root-contained Playwright open: %+v", opened)
	}
	id := opened.SessionID
	entry, err := s.sessions.get(req, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unblock.Do(func() { close(allow) })
		if s.browserAdmission.matches(id) {
			if e := s.workerStdioClose(req, id); e != nil {
				t.Errorf("test-owned Browser cgroup close uncertain: %v", e)
			}
		}
	})
	entry.mu.Lock()
	parentPID, pin, group := entry.pid, entry.pidPin, entry.browserGroup
	entry.mu.Unlock()
	if pin == nil || group == nil || !group.verified || !group.containsPID(parentPID) || !strings.Contains(group.path, "/ai-server-agent-executor.service/asa-browser-") {
		t.Fatalf("original Node not atomically contained: pid=%d, group=%+v", parentPID, group)
	}
	childPID := browserFixtureWorkerPID(t, marker)
	if !group.containsPID(childPID) {
		t.Fatal("independently grouped child is not in original private cgroup")
	}
	if pg, err := syscall.Getpgid(childPID); err != nil || pg != childPID {
		t.Fatalf("child is not a separate process-group leader: %d %v", pg, err)
	}
	data, err := os.ReadFile(filepath.Join(group.path, "cgroup.procs"))
	if err != nil || len(strings.Fields(string(data))) < 3 {
		t.Fatalf("no real Chromium descendants plus separate child: %s %v", data, err)
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("live Playwright profile unavailable: %v", err)
	}
	ownedCgroup := group.path
	if err := syscall.Kill(parentPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill exact disposable Node process: %v", err)
	}
	select {
	case <-blocked:
	case <-time.After(6 * time.Second):
		t.Fatal("Node cleanup failed to reach pre-cgroup-kill proof barrier")
	}
	// Required assertion while independent Chrome/test descendant is STILL
	// ALIVE, not after waiting for eventual Playwright shutdown.
	if !group.containsPID(childPID) {
		t.Fatal("detached child disappeared before the critical pre-release assertion")
	}
	if busy, err := group.populated(); err != nil || !busy {
		t.Fatalf("live child not covered by cgroup: %t %v", busy, err)
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("profile deleted while detached process is alive: %v", err)
	}
	if allowed, _ := s.browserAcquire("unsafe-concurrent-shared", "shared"); allowed {
		t.Fatal("profile capacity freed before detached Chromium stopped")
	}
	closeResult := make(chan Response, 1)
	go func() {
		closeResult <- s.browserSessionAction(context.Background(), browserOwnerControl(req, id, "browser_session_close"))
	}()
	select {
	case out := <-closeResult:
		t.Fatalf("close returned without whole-cgroup stop: %+v", out)
	case <-time.After(175 * time.Millisecond):
	}
	if !group.containsPID(childPID) {
		t.Fatal("detached child died before cgroup stop release")
	}
	unblock.Do(func() { close(allow) })
	select {
	case out := <-closeResult:
		if !out.OK || out.Status != "closed" {
			t.Fatalf("exact Browser cleanup failed: %+v", out)
		}
	case <-time.After(9 * time.Second):
		t.Fatal("verified whole-cgroup cleanup did not resolve close")
	}
	if _, err := os.Stat(ownedCgroup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified private cgroup was not removed: %v", err)
	}
	if s.browserAdmission.reserved() {
		t.Fatal("Browser reservation remained after kernel verified cgroup removal")
	}
	// The next first admission may reclaim this *now proven dead* profile.
	if granted, err := s.browserAcquire("safe-after-reconciliation", "shared"); err != nil || !granted {
		t.Fatalf("capacity not safely reusable: %t %v", granted, err)
	}
	s.browserAdmission.release("safe-after-reconciliation")
	if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead profile not reclaimed after proven stop: %v", err)
	}
	t.Logf("BROWSER_DETACHED_CHROMIUM_NODE_CRASH_PROOF_PASS Node=%d detached=%d; profile retained while child alive, cgroup.kill verified before admission reuse", parentPID, childPID)
}

func TestApprovedRootBrowserOneShotDetachedChildAfterNodeExit(t *testing.T) {
	s, req, tmp, _ := approvedBrowserCgroupRootFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
	defer cancel()
	blocked := make(chan struct{})
	allow := make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(allow) }) })
	s.browserContainmentBeforeKillTestHook = func() error { close(blocked); <-allow; return nil }
	marker := filepath.Join(req.Workspace, "oneshot.pid")
	profile := filepath.Join(tmp, "asa-profile-Sho123")
	command := fmt.Sprintf("mkdir -m 0700 %q; /usr/bin/setsid /bin/sleep 90 </dev/null >/dev/null 2>&1 & echo $! > %q; exit 0", profile, marker)
	one := req
	one.Action = "run"
	one.AuditAction = "browser_run"
	one.Command = command
	one.BrowserProfileMode = "shared"
	one.Content = ""
	result := make(chan Response, 1)
	go func() { result <- s.runContext(ctx, one) }()
	select {
	case <-blocked:
	case <-time.After(7 * time.Second):
		t.Fatal("one-shot did not reach owned-cgroup stop hook")
	}
	pid := browserFixtureWorkerPID(t, marker)
	if pg, err := syscall.Getpgid(pid); err != nil || pg != pid {
		t.Fatalf("one-shot descendant not independently grouped: %d %v", pg, err)
	}
	if !s.browserAdmission.reserved() {
		t.Fatal("one-shot reservation already released with child alive")
	}
	if granted, _ := s.browserAcquire("unsafe-oneshot-overlap", "shared"); granted {
		t.Fatal("competing shared Browser admitted with detached child still alive")
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("one-shot active profile removed: %v", err)
	}
	select {
	case <-result:
		t.Fatal("one-shot acknowledged completion before cgroup proof")
	case <-time.After(130 * time.Millisecond):
	}
	unblock.Do(func() { close(allow) })
	select {
	case out := <-result:
		if out.OK || out.ReasonCode != "background_process" {
			t.Fatalf("detached one-shot process did not report background work while verifying cleanup: %+v", out)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("one-shot cgroup cleanup never verified")
	}
	if s.browserAdmission.reserved() {
		t.Fatal("one-shot slot remained reserved after proven stop")
	}
	if granted, err := s.browserAcquire("after-oneshot", "shared"); err != nil || !granted {
		t.Fatalf("one-shot release failed: %t %v", granted, err)
	}
	s.browserAdmission.release("after-oneshot")
	if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead one-shot profile was not safely reclaimed: %v", err)
	}
	t.Log("BROWSER_ONESHOT_DETACHED_GROUP_PROOF_PASS")
}
