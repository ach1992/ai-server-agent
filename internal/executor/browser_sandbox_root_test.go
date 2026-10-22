package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Separately authorized disposable hDE01 acceptance: production-generated
// sandboxed Chromium under the REAL installed dedicated aibrowser identity.
// Requires a dedicated test-ready account and an execution context inside
// the original root Executor service cgroup. Never creates/modifies accounts,
// installed services, host namespace controls or global Browser profiles.
func TestApprovedRootBrowserSandboxEnforcedUnderAibrowser(t *testing.T) {
	host, _ := os.Hostname()
	if host != "hDE01" || os.Geteuid() != 0 ||
		os.Getenv("ASA_203_BROWSER_SANDBOX_APPROVED_HOST") != "hDE01" {
		t.Skip("requires separately approved root hDE01 Chromium sandbox fixture")
	}
	if os.Getenv("ASA_197_BROWSER_CGROUP_APPROVED_HOST") != "hDE01" {
		t.Fatal("approved Browser root cgroup test prerequisite not explicitly enabled")
	}
	s, owner, _, fixtureRoot := approvedBrowserCgroupRootFixture(t)
	account, err := user.Lookup("aibrowser")
	if err != nil {
		t.Fatalf("real dedicated aibrowser OS identity not provisioned: %v", err)
	}
	browserUID, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || browserUID == 0 {
		t.Fatal("invalid aibrowser numeric UID")
	}
	browserGID, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil || browserGID == 0 {
		t.Fatal("invalid aibrowser numeric GID")
	}
	agent, err := user.Lookup("aiagent")
	if err != nil {
		t.Fatal(err)
	}
	agentUID, err := strconv.ParseUint(agent.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	agentGID, err := strconv.ParseUint(agent.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	s.browserUID, s.browserGID = uint32(browserUID), uint32(browserGID)
	s.agentUID, s.agentGID = uint32(agentUID), uint32(agentGID)
	s.cfg.AgentUser = "aiagent"
	s.cfg.WorkerUser = "aiworker"
	if !distinctIsolatedBrowserIdentity(s.browserUID, s.browserGID, s.workerUID, s.workerGID, s.agentUID, s.agentGID) {
		t.Fatal("dedicated Browser numeric account aliases a protected identity")
	}
	for _, dir := range []string{filepath.Join(s.cfg.StateDir, "browser-home"), s.isolatedBrowserScratchDir()} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, int(browserUID), int(browserGID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.isolatedBrowserHome(); err != nil {
		t.Fatalf("root Browser identity/home proof: %v", err)
	}
	limits := s.cfg.EffectiveRuntime()
	limits.BrowserConcurrentSlots = 1
	s.cfg.Runtime = &limits
	owner.BrowserProfileMode = "isolated"
	owner.BrowserSandbox = true
	owner.PrincipalID = "sandbox-owner-A"
	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo = filepath.Clean(filepath.Join(repo, "..", ".."))
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatal(err)
	}
	compileCtx, compileCancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer compileCancel()
	exporter := exec.CommandContext(compileCtx, "go", "test", "./internal/browser",
		"-run", "^TestExportApprovedRootManagedBrowserTraceRunner$", "-count=1", "-v")
	exporter.Dir = repo
	exporter.Env = append(os.Environ(),
		"ASA_BROWSER_ROOT_PRODUCTION_TEST_ROOT="+fixtureRoot,
		"ASA_197_BROWSER_CGROUP_APPROVED_HOST=hDE01",
		"ASA_203_BROWSER_SANDBOX_GENERATOR=1", "TMPDIR=/var/tmp")
	output, err := exporter.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("APPROVED_BROWSER_PRODUCTION_MANAGED_SOURCE_EXPORTED")) {
		t.Fatalf("production sandboxed Browser source export failed: %v %s", err, output)
	}
	source, err := os.ReadFile(filepath.Join(owner.Workspace, "production-managed.mjs"))
	if err != nil || len(source) == 0 || len(source) > browserSessionMaxScript {
		t.Fatalf("invalid production sandbox source: %v length=%d", err, len(source))
	}
	if !bytes.Contains(source, []byte("const chromiumSandbox = true")) {
		t.Fatal("opt-in not present in actual Node source")
	}
	owner.Content = string(source)
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK || opened.SessionID == "" || !strings.Contains(opened.Output, `"chromium_sandbox":"verified_at_start"`) {
		t.Fatalf("real aibrowser/root-cgroup/Chromium sandbox refused or unverified: %+v", opened)
	}
	id := opened.SessionID
	var released sync.Once
	releaseGate := make(chan struct{})
	t.Cleanup(func() {
		released.Do(func() { close(releaseGate) })
		if s.browserAdmission.matches(id) {
			closed := s.browserSessionAction(context.Background(), browserOwnerControl(owner, id, "browser_session_close"))
			if !closed.OK {
				t.Errorf("uncertain sandbox test-owned Browser cleanup: %+v", closed)
			}
		}
	})
	entry, err := s.browserSessionEntry(browserOwnerControl(owner, id, "browser_session_status"))
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	nodePID, group, traceName, profileName := entry.pid, entry.browserGroup, entry.browserTraceScratchName, entry.browserScratchName
	private := entry.browserPrivate
	entry.mu.Unlock()
	if !private || group == nil || !group.verified || !group.containsPID(nodePID) {
		t.Fatal("sandboxed aibrowser Node not inside its private root-owned cgroup")
	}
	node, err := readBrowserSandboxProcess(nodePID)
	if err != nil || node.effectiveUID != s.browserUID || node.effectiveGID != s.browserGID {
		t.Fatalf("sandboxed Node OS account proof unavailable: %+v %v", node, err)
	}
	if err := s.verifyManagedBrowserSandbox(entry); err != nil {
		t.Fatalf("renderer sandbox stopped enforcing after ready: %v", err)
	}
	flow := browserOwnerControl(owner, id, "browser_session_flow")
	flow.Content = `[{"action":"snapshot"}]`
	if got := s.browserSessionAction(ctx, flow); !got.OK || got.Status != "result" {
		t.Fatalf("actual sandboxed FlowStep failed: %+v", got)
	}
	record := browserOwnerControl(owner, id, "browser_session_trace")
	record.Content = `{"operation":"record","steps":[{"action":"snapshot"}]}`
	result := s.browserSessionAction(ctx, record)
	if !result.OK || result.Status != "recorded" || !strings.HasPrefix(result.FileVersion, "sha256:") {
		t.Fatalf("sandboxed aibrowser real private ZIP recording failed: %+v", result)
	}
	tracePath := filepath.Join(s.isolatedBrowserScratchDir(), traceName, "trace.zip")
	info, err := os.Lstat(tracePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("private ZIP missing: %v", err)
	}
	var raw []byte
	var offset int64
	done := false
	for i := 0; i < 2048; i++ {
		request := browserOwnerControl(owner, id, "browser_session_trace")
		request.Content = fmt.Sprintf(`{"operation":"read","file_version":%q,"offset":%d}`, result.FileVersion, offset)
		chunk := s.browserSessionAction(ctx, request)
		if !chunk.OK || chunk.Offset == nil || *chunk.Offset != offset || chunk.NextOffset == nil || chunk.EOF == nil {
			t.Fatalf("sandboxed ZIP bounded read not verifiable at %d: %+v", offset, chunk)
		}
		decoded, decodeErr := base64.StdEncoding.Strict().DecodeString(chunk.Output)
		if decodeErr != nil || len(decoded) > 8192 || *chunk.NextOffset != offset+int64(len(decoded)) {
			t.Fatalf("sandboxed ZIP chunk invalid: %v %+v", decodeErr, chunk)
		}
		raw = append(raw, decoded...)
		offset = *chunk.NextOffset
		if *chunk.EOF {
			done = true
			break
		}
	}
	digest := sha256.Sum256(raw)
	if !done || len(raw) != int(info.Size()) || len(raw) < 4 ||
		!bytes.Equal(raw[:2], []byte{'P', 'K'}) ||
		"sha256:"+fmt.Sprintf("%x", digest) != result.FileVersion {
		t.Fatalf("sandboxed ZIP stream SHA/EOF invalid: done=%t size=%d disk=%d", done, len(raw), info.Size())
	}
	intruder := browserOwnerControl(owner, id, "browser_session_status")
	intruder.PrincipalID = "sandbox-other-principal-B"
	if got := s.browserSessionAction(ctx, intruder); got.OK || got.ErrorCode != "browser_session_not_found" {
		t.Fatalf("foreign principal reused sandboxed session: %+v", got)
	}
	// The detached worker/OS isolation proof from #202 remains intact:
	// authenticated worker B must not be able to read the private trace.
	workerB := Request{Action: "run", PrincipalID: intruder.PrincipalID, PrincipalClass: owner.PrincipalClass,
		Workspace: owner.Workspace, Command: fmt.Sprintf("test ! -r %q && test ! -x %q && test ! -r /proc/%d/environ", tracePath, s.isolatedBrowserScratchDir(), nodePID)}
	if shell := s.runContext(ctx, workerB); !shell.OK || shell.ExitCode != 0 {
		t.Fatalf("other principal's worker shell accessed Browser private state: %+v", shell)
	}
	originalCgroup := group.path
	entered := make(chan struct{})
	group.beforeKillTestHook = func() error {
		close(entered)
		<-releaseGate
		return nil
	}
	closedChan := make(chan Response, 1)
	go func() {
		closedChan <- s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close"))
	}()
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		t.Fatal("sandboxed normal-close never reached kernel barrier")
	}
	if !s.browserAdmission.matches(id) || !group.containsPID(nodePID) {
		// Node may exit gracefully before cgroup stop. Admission must still
		// be held, regardless of whether the Node PID survived.
		if !s.browserAdmission.matches(id) {
			t.Fatal("Browser admission released before cgroup cleanup")
		}
	}
	for _, path := range []string{tracePath, filepath.Join(s.isolatedBrowserScratchDir(), profileName)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("private scratch prematurely removed at barrier: %s %v", path, err)
		}
	}
	released.Do(func() { close(releaseGate) })
	select {
	case closed := <-closedChan:
		if !closed.OK || closed.Status != "closed" {
			t.Fatalf("sandboxed real Browser close failed: %+v", closed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sandboxed root-owned Browser did not reconcile cleanup")
	}
	if _, err := os.Stat(originalCgroup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original private cgroup still exists: %v", err)
	}
	for _, name := range []string{traceName, profileName} {
		if _, err := os.Lstat(filepath.Join(s.isolatedBrowserScratchDir(), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private Browser scratch survived proven stop: %s %v", name, err)
		}
	}
	if s.browserAdmission.reserved() {
		t.Fatal("sandbox Browser reservation not released after verified stop")
	}
	t.Log("REAL_AIBROWSER_ROOT_CGROUP_CHROMIUM_SANDBOX_TRACE_PRINCIPAL_SHA_EOF_CLEANUP_PASS")
}
