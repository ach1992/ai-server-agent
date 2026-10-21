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
	for _, dir := range []string{sources, state, filepath.Join(state, "runtime"), filepath.Dir(tmp)} {
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
const child=spawn('/bin/sleep',['90'],{stdio:'ignore',detached:true});
writeFileSync(%q,String(child.pid)); child.unref();
process.stdout.write('ASA_BROWSER_SESSION '+JSON.stringify({event:'ready'})+'\n');
process.stdin.setEncoding('utf8');
process.stdin.on('data',chunk=>{
  if(chunk.includes('"type":"close"')) void context.close().finally(()=>process.exit(0));
});
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
	// One allowed slot makes an early release observable; another separate
	// acceptance covers the normal 2-slot concurrent isolated policy.
	limits := s.cfg.EffectiveRuntime()
	limits.BrowserConcurrentSlots = 1
	s.cfg.Runtime = &limits
	profile := filepath.Join(tmp, "asa-profile-Roo123")
	marker := filepath.Join(req.Workspace, "detached.pid")
	req.Content = rootBrowserPersistentScript(profile, marker)
	blocked := make(chan struct{})
	allow := make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(allow) }) })
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
	// Install the deterministic barrier only AFTER the actual Playwright
	// ready handshake; a launch failure must still be free to clean up.
	group.beforeKillTestHook = func() error { close(blocked); <-allow; return nil }
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
	if allowed, _ := s.browserAcquire("unsafe-concurrent-isolated", "isolated"); allowed {
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
	s.browserKillTestHook = func() error { close(blocked); <-allow; return nil }
	marker := filepath.Join(req.Workspace, "oneshot.pid")
	profile := filepath.Join(tmp, "asa-profile-Sho123")
	js := fmt.Sprintf("const {spawn}=require('node:child_process');const {writeFileSync}=require('node:fs');const p=spawn('/bin/sleep',['90'],{detached:true,stdio:'ignore'});writeFileSync(%q,String(p.pid));p.unref();", marker)
	command := fmt.Sprintf("mkdir -m 0700 %q; /opt/ai-server-agent/browser/node/bin/node -e %q", profile, js)
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

// Two independent Playwright+Chromium groups must coexist, and closing one
// must not sweep the other process tree/profile. A shared profile may coexist
// with an isolated one but no second shared holder may overlap it.
func TestApprovedRootBrowserConcurrentCgroupsAndSharedContention(t *testing.T) {
	s, owner, tmp, _ := approvedBrowserCgroupRootFixture(t)
	first := owner
	first.BrowserProfileMode = "isolated"
	first.Content = rootBrowserPersistentScript(filepath.Join(tmp, "asa-profile-Iso111"), filepath.Join(first.Workspace, "a.pid"))
	second := owner
	second.PrincipalID = "independent-cgroup-owner"
	second.BrowserProfileMode = "isolated"
	second.Content = rootBrowserPersistentScript(filepath.Join(tmp, "asa-profile-Iso222"), filepath.Join(second.Workspace, "b.pid"))
	opened := func(req Request) (*stdioSession, string) {
		t.Helper()
		r := s.browserSessionAction(context.Background(), req)
		if !r.OK {
			t.Fatalf("actual Browser open: %+v", r)
		}
		entry, err := s.sessions.get(req, r.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if s.browserAdmission.matches(r.SessionID) {
				if got := s.browserSessionAction(context.Background(), browserOwnerControl(req, r.SessionID, "browser_session_close")); !got.OK {
					t.Errorf("test-owned session cleanup: %+v", got)
				}
			}
		})
		return entry, r.SessionID
	}
	one, id1 := opened(first)
	two, id2 := opened(second)
	if one.browserGroup == nil || two.browserGroup == nil || one.browserGroup.path == two.browserGroup.path {
		t.Fatal("two isolated contexts share a cgroup")
	}
	path1, path2 := one.browserGroup.path, two.browserGroup.path
	a := browserFixtureWorkerPID(t, filepath.Join(first.Workspace, "a.pid"))
	b := browserFixtureWorkerPID(t, filepath.Join(second.Workspace, "b.pid"))
	if !one.browserGroup.containsPID(a) || !two.browserGroup.containsPID(b) || one.browserGroup.containsPID(b) || two.browserGroup.containsPID(a) {
		t.Fatal("Chromium owned descendants not isolated across kernel groups")
	}
	if out := s.browserSessionAction(context.Background(), first); out.OK || out.ErrorCode != "resource_limit" {
		t.Fatalf("third isolated Browser bypassed two-slot cap: %+v", out)
	}
	if out := s.browserSessionAction(context.Background(), browserOwnerControl(first, id1, "browser_session_close")); !out.OK {
		t.Fatalf("first isolated close: %+v", out)
	}
	if _, err := os.Stat(path1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first cgroup not removed: %v", err)
	}
	if !two.browserGroup.containsPID(b) {
		t.Fatal("second live isolated Browser stopped with first")
	}
	if _, err := os.Stat(filepath.Join(tmp, "asa-profile-Iso222")); err != nil {
		t.Fatalf("second live profile disappeared: %v", err)
	}
	shared := owner
	shared.BrowserProfileMode = "shared"
	shared.Content = rootBrowserPersistentScript(filepath.Join(tmp, "asa-profile-Sha333"), filepath.Join(shared.Workspace, "shared.pid"))
	_, sharedID := opened(shared)
	if out := s.browserSessionAction(context.Background(), shared); out.OK || out.ErrorCode != "resource_limit" {
		t.Fatalf("second shared profile admitted: %+v", out)
	}
	if out := s.browserSessionAction(context.Background(), browserOwnerControl(shared, sharedID, "browser_session_close")); !out.OK {
		t.Fatalf("shared close: %+v", out)
	}
	if !two.browserGroup.containsPID(b) {
		t.Fatal("shared close damaged independent isolated Chrome")
	}
	if out := s.browserSessionAction(context.Background(), browserOwnerControl(second, id2, "browser_session_close")); !out.OK {
		t.Fatalf("second isolated close: %+v", out)
	}
	if _, err := os.Stat(path2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second isolated cgroup not removed: %v", err)
	}
	if s.browserAdmission.reserved() {
		t.Fatal("Browser reservations leaked after verified parallel closes")
	}
	t.Log("BROWSER_TWO_REAL_CHROMIUM_ROOT_CGROUPS_AND_SHARED_CONTENTION_PASS")
}

// A definite cgroup.kill failure may NOT become success merely because the
// Node group has exited. Authenticated close must be able to retry the exact
// cgroup and safely re-open without losing the original lease/session ID.
func TestApprovedRootBrowserUncertainCleanupRetainsExactRetry(t *testing.T) {
	s, req, tmp, _ := approvedBrowserCgroupRootFixture(t)
	capacity := s.cfg.EffectiveRuntime()
	capacity.BrowserConcurrentSlots = 1
	s.cfg.Runtime = &capacity
	profile := filepath.Join(tmp, "asa-profile-Unc123")
	marker := filepath.Join(req.Workspace, "uncertain.pid")
	req.Content = rootBrowserPersistentScript(profile, marker)
	attempts := 0
	s.browserKillTestHook = func() error {
		attempts++
		if attempts == 1 {
			return errors.New("synthetic test-owned cgroup stop uncertainty")
		}
		return nil
	}
	opened := s.browserSessionAction(context.Background(), req)
	if !opened.OK {
		t.Fatalf("private cgroup failed to start: %+v", opened)
	}
	entry, err := s.sessions.get(req, opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s.browserAdmission.matches(opened.SessionID) {
			_ = s.browserSessionClose(browserOwnerControl(req, opened.SessionID, "browser_session_close"))
		}
	})
	markerPID := browserFixtureWorkerPID(t, marker)
	entry.mu.Lock()
	originalPID, group := entry.pid, entry.browserGroup
	entry.mu.Unlock()
	if err := syscall.Kill(originalPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForStdioExit(t, entry)
	if entry.cleanupErr == nil || attempts != 1 || !group.containsPID(markerPID) {
		t.Fatalf("failed cgroup stop falsely cleared Node child: attempts=%d err=%v", attempts, entry.cleanupErr)
	}
	if !s.browserAdmission.matches(opened.SessionID) {
		t.Fatal("uncertain cgroup stop lost authenticated reservation")
	}
	if out := s.browserSessionStatus(browserOwnerControl(req, opened.SessionID, "browser_session_status")); !out.OK || out.Status != "uncertain" {
		t.Fatalf("uncertain Browser status lost: %+v", out)
	}
	if newOne, _ := s.browserAcquire("unsafe-retry-bypass", "isolated"); newOne {
		t.Fatal("uncertain cgroup permitted a new Browser")
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("live profile deleted during uncertain cgroup stop: %v", err)
	}
	if out := s.browserSessionClose(browserOwnerControl(req, opened.SessionID, "browser_session_close")); !out.OK || attempts != 2 {
		t.Fatalf("exact authenticated cgroup retry failed: %+v attempts=%d", out, attempts)
	}
	if s.browserAdmission.reserved() {
		t.Fatal("slot not released after proven retry")
	}
	if _, err := os.Stat(group.path); group.path != "" && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup retained after verified retry: %v", err)
	}
	t.Log("BROWSER_UNCERTAIN_STOP_RETRY_IDENTITY_AND_PROFILE_PASS")
}

// Context timeout cancels the original one-shot Node process group. Its
// detached child remains in the same private cgroup until the Executor's
// explicit whole-cgroup stop proof, so cancellation cannot prematurely
// authorize another shared-profile run.
func TestApprovedRootBrowserOneShotTimeoutDetachedChild(t *testing.T) {
	s, req, tmp, _ := approvedBrowserCgroupRootFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	blocked := make(chan struct{})
	allow := make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(allow) }) })
	s.browserKillTestHook = func() error { close(blocked); <-allow; return nil }
	marker := filepath.Join(req.Workspace, "timeout.pid")
	profile := filepath.Join(tmp, "asa-profile-Tmo123")
	js := fmt.Sprintf("const {spawn}=require('node:child_process');const {writeFileSync}=require('node:fs');const p=spawn('/bin/sleep',['90'],{detached:true,stdio:'ignore'});writeFileSync(%q,String(p.pid));p.unref();setInterval(()=>{},1000);", marker)
	one := req
	one.Action = "run"
	one.AuditAction = "browser_run"
	one.Command = fmt.Sprintf("mkdir -m 0700 %q; /opt/ai-server-agent/browser/node/bin/node -e %q", profile, js)
	one.BrowserProfileMode = "shared"
	one.Content = ""
	done := make(chan Response, 1)
	go func() { done <- s.runContext(ctx, one) }()
	select {
	case <-blocked:
	case <-time.After(7 * time.Second):
		t.Fatal("canceled Browser did not reach cgroup teardown barrier")
	}
	pid := browserFixtureWorkerPID(t, marker)
	if pg, err := syscall.Getpgid(pid); err != nil || pg != pid {
		t.Fatalf("detached child unexpectedly not in separate PGID: %d %v", pg, err)
	}
	if !s.browserAdmission.reserved() {
		t.Fatal("timeout freed the admission slot before detached process stop")
	}
	if overlap, _ := s.browserAcquire("timeout-unsafe-shared", "shared"); overlap {
		t.Fatal("shared profile was reopened while timed-out Chrome child survived")
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("timeout profile removed before cgroup proof: %v", err)
	}
	select {
	case got := <-done:
		t.Fatalf("canceled Browser acknowledged cleanup while detached child still alive: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	unblock.Do(func() { close(allow) })
	select {
	case got := <-done:
		if got.OK || !got.TimedOut || got.ReasonCode != "timeout" {
			t.Fatalf("cgroup cleanup changed timeout semantics: %+v", got)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("canceled Browser did not reconcile cgroup")
	}
	if s.browserAdmission.reserved() {
		t.Fatal("timeout retained slot after cgroup was proven empty")
	}
	if allowed, err := s.browserAcquire("timeout-safe-next", "shared"); err != nil || !allowed {
		t.Fatalf("next Browser denied after verified timeout: %t %v", allowed, err)
	}
	s.browserAdmission.release("timeout-safe-next")
	if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped timeout profile not reclaimed: %v", err)
	}
	t.Log("BROWSER_ONESHOT_TIMEOUT_DETACHED_CGROUP_CLEANUP_PASS")
}
