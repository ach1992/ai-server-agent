package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Explicit hDE01-only proof: the production-generated managed Browser session
// runs under a distinct OS UID, creates a real Chromium trace, and retains
// its ZIP behind an OS boundary against an otherwise authorized aiworker
// command. The only source instrumentation is one detached /bin/sleep child
// for a deterministic NORMAL-close kernel teardown barrier. No installed
// service, global profile, account or config is modified.
func TestApprovedRootBrowserPrivateTraceNormalCloseAndWorkerDenial(t *testing.T) {
	s, owner, _, fixtureRoot := approvedBrowserCgroupRootFixture(t)
	unprivileged, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("disposable isolated account fixture unavailable")
	}
	uid, err := strconv.Atoi(unprivileged.Uid)
	if err != nil || uid == 0 || uint32(uid) == s.workerUID {
		t.Fatal("disposable Browser identity must differ from worker")
	}
	gid, err := strconv.Atoi(unprivileged.Gid)
	if err != nil || gid == 0 || uint32(gid) == s.workerGID {
		t.Fatal("disposable Browser group must differ from worker")
	}
	s.browserUID, s.browserGID = uint32(uid), uint32(gid)
	s.cfg.AgentUser = "aiagent" // activate the real production readiness guard
	s.cfg.WorkerUser = "aiworker"
	agentAccount, err := user.Lookup("aiagent")
	if err != nil {
		t.Fatal(err)
	}
	agentUID, err := strconv.ParseUint(agentAccount.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	agentGID, err := strconv.ParseUint(agentAccount.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	s.agentUID, s.agentGID = uint32(agentUID), uint32(agentGID)
	private := s.isolatedBrowserScratchDir()
	home := filepath.Join(s.cfg.StateDir, "browser-home")
	for _, dir := range []string{home, private} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.isolatedBrowserHome(); err != nil {
		t.Fatalf("isolated Browser directory provenance: %v", err)
	}
	capacity := s.cfg.EffectiveRuntime()
	capacity.BrowserConcurrentSlots = 1
	s.cfg.Runtime = &capacity
	owner.PrincipalID = "trace-owner-A"
	owner.BrowserProfileMode = "isolated"

	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo = filepath.Clean(filepath.Join(repo, "..", ".."))
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatalf("source root unavailable: %v", err)
	}
	exportCtx, exportCancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer exportCancel()
	export := exec.CommandContext(exportCtx, "go", "test", "./internal/browser",
		"-run", "^TestExportApprovedRootManagedBrowserTraceRunner$", "-count=1", "-v")
	export.Dir = repo
	export.Env = append(os.Environ(),
		"ASA_BROWSER_ROOT_PRODUCTION_TEST_ROOT="+fixtureRoot,
		"ASA_197_BROWSER_CGROUP_APPROVED_HOST=hDE01", "TMPDIR=/var/tmp")
	if output, err := export.CombinedOutput(); err != nil ||
		!bytes.Contains(output, []byte("APPROVED_BROWSER_PRODUCTION_MANAGED_SOURCE_EXPORTED")) {
		t.Fatalf("production managed Browser source export: %v; %s", err, output)
	}
	runner, err := os.ReadFile(filepath.Join(owner.Workspace, "production-managed.mjs"))
	if err != nil || len(runner) == 0 || len(runner) > browserSessionMaxScript {
		t.Fatalf("generated Browser source invalid size=%d err=%v", len(runner), err)
	}
	owner.Content = string(runner)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	open := s.browserSessionAction(ctx, owner)
	if !open.OK {
		t.Fatalf("root-owned real isolated Chromium start: %+v", open)
	}
	id := open.SessionID
	var release sync.Once
	releaseBarrier := make(chan struct{})
	t.Cleanup(func() {
		release.Do(func() { close(releaseBarrier) })
		if s.browserAdmission.matches(id) {
			closed := s.browserSessionAction(context.Background(),
				browserOwnerControl(owner, id, "browser_session_close"))
			if !closed.OK {
				t.Errorf("test-owned Browser cleanup uncertain: %+v", closed)
			}
		}
	})
	entry, err := s.browserSessionEntry(browserOwnerControl(owner, id, "browser_session_status"))
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	childPID, group := entry.pid, entry.browserGroup
	traceName, profileName := entry.browserTraceScratchName, entry.browserScratchName
	privateMode := entry.browserPrivate
	entry.mu.Unlock()
	if !privateMode || group == nil || !group.verified || !group.containsPID(childPID) {
		t.Fatalf("private managed Node not inside root-owned cgroup: pid=%d, private=%t", childPID, privateMode)
	}
	proc, err := os.Stat(fmt.Sprintf("/proc/%d", childPID))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := proc.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != s.browserUID {
		t.Fatal("managed Node did not drop privileges to isolated Browser UID")
	}
	originalGroupPath := group.path
	marker := filepath.Join(private, "detached.pid")
	descendant := browserFixtureWorkerPID(t, marker)
	if !group.containsPID(descendant) {
		t.Fatal("detached test descendant escaped original kernel cgroup")
	}

	record := browserOwnerControl(owner, id, "browser_session_trace")
	record.Content = "{\"operation\":\"record\",\"steps\":[{\"action\":\"snapshot\"}]}"
	result := s.browserSessionAction(ctx, record)
	if !result.OK || result.Status != "recorded" || result.FileVersion == "" {
		t.Fatalf("real root Browser trace record: %+v", result)
	}
	traceDir := filepath.Join(private, traceName)
	tracePath := filepath.Join(traceDir, "trace.zip")
	info, err := os.Lstat(tracePath)
	if err != nil || info.Size() < 4 {
		t.Fatalf("actual retained trace ZIP absent: %v %v", info, err)
	}
	if ownerInfo, ok := info.Sys().(*syscall.Stat_t); !ok || ownerInfo.Uid != s.browserUID {
		t.Fatal("retained trace bytes are not owned by isolated Browser UID")
	}
	read := browserOwnerControl(owner, id, "browser_session_trace")
	read.Content = fmt.Sprintf("{\"operation\":\"read\",\"file_version\":%q,\"offset\":0}", result.FileVersion)
	window := s.browserSessionAction(ctx, read)
	raw, err := base64.StdEncoding.Strict().DecodeString(window.Output)
	if err != nil || !window.OK || len(raw) < 4 || !bytes.Equal(raw[:2], []byte{'P', 'K'}) {
		t.Fatalf("owner could not retrieve authentic bounded ZIP window: %+v err=%v", window, err)
	}
	intruder := read
	intruder.PrincipalID = "other-authenticated-principal-B"
	if denied := s.browserSessionAction(ctx, intruder); denied.OK ||
		denied.ErrorCode != "browser_session_not_found" || denied.Output != "" {
		t.Fatalf("foreign principal accessed Browser trace API: %+v", denied)
	}
	// Both the named ZIP and the original Node's open FDs must be unavailable
	// to B's otherwise authorized worker shell, despite knowledge of paths/PID.
	command := fmt.Sprintf("test ! -r %q && test ! -x %q && test ! -r /proc/%d/environ",
		tracePath, private, childPID)
	workerB := Request{Action: "run", PrincipalID: intruder.PrincipalID,
		PrincipalClass: owner.PrincipalClass, Workspace: owner.Workspace, Command: command}
	shell := s.runContext(ctx, workerB)
	if !shell.OK || shell.ExitCode != 0 || shell.Output != "" {
		t.Fatalf("worker shell isolation failed: %+v", shell)
	}

	entered := make(chan struct{})
	group.beforeKillTestHook = func() error {
		close(entered)
		<-releaseBarrier
		return nil
	}
	closeDone := make(chan Response, 1)
	go func() {
		closeDone <- s.browserSessionAction(ctx,
			browserOwnerControl(owner, id, "browser_session_close"))
	}()
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("managed Browser normal close never reached pre-cgroup-kill barrier")
	}
	if !group.containsPID(descendant) {
		t.Fatal("detached descendant died before normal-close stop proof")
	}
	if busy, err := group.populated(); err != nil || !busy {
		t.Fatalf("private cgroup was not populated at barrier: %t %v", busy, err)
	}
	if _, err := os.Stat(tracePath); err != nil {
		t.Fatalf("trace reclaimed before whole-cgroup cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(private, profileName)); err != nil {
		t.Fatalf("isolated profile prematurely reclaimed: %v", err)
	}
	if !s.browserAdmission.matches(id) {
		t.Fatal("Browser session admission was released before cgroup stop")
	}
	if granted, _ := s.browserAcquire("unsafe-overlap", "isolated"); granted {
		t.Fatal("another isolated Browser admitted while detached child alive")
	}
	select {
	case closed := <-closeDone:
		t.Fatalf("normal close acknowledged success before descendant cleanup: %+v", closed)
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(func() { close(releaseBarrier) })
	select {
	case closed := <-closeDone:
		if !closed.OK || closed.Status != "closed" {
			t.Fatalf("normal close after cgroup proof failed: %+v", closed)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("normal Browser close never reconciled cgroup and scratch")
	}
	if _, err := os.Stat(originalGroupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact private cgroup still exists after proof: %v", err)
	}
	for _, name := range []string{traceName, profileName} {
		if _, err := os.Lstat(filepath.Join(private, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private retained scratch %s not removed: %v", name, err)
		}
	}
	if s.browserAdmission.matches(id) || s.browserAdmission.reserved() {
		t.Fatal("Browser admission retained after proven cleanup")
	}
	t.Log("PRIVATE_BROWSER_REAL_CHROMIUM_TRACE_A_VS_B_WORKER_DENIAL_AND_NORMAL_CGROUP_CLOSE_PASS")
}
