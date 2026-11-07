package executor

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
)

// The opt-in disposable Dev root fixture exercises the production broker's
// failure *after* profile/trace creation and at cmd.Start (missing binary).
// No installed Agent service or other worktree is modified. Unlike a test
// that invokes scratch reclamation in isolation, this proves failed starts
// clean exactly their own precreated directories while another Browser
// reservation remains active.
func TestRootBrowserFailedLaunchReclaimsExactScratch(t *testing.T) {
	if os.Getenv("ASA_REQUIRE_BROWSER_FAILED_LAUNCH_CLEANUP") != "1" {
		t.Skip("disposable root/Executor-cgroup acceptance is opt-in")
	}
	if os.Geteuid() != 0 {
		t.Fatal("failed-start scratch acceptance must run as root")
	}
	mustID := func(name string) (uint32, uint32) {
		t.Helper()
		account, err := user.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		uid, err := strconv.ParseUint(account.Uid, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		gid, err := strconv.ParseUint(account.Gid, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		return uint32(uid), uint32(gid)
	}
	workerUID, workerGID := mustID("aiworker")
	agentUID, agentGID := mustID("aiagent")
	browserUID, browserGID := mustID("aibrowser")
	fixture, err := os.MkdirTemp("/var/tmp", "asa-browser-failed-start-")
	if err != nil {
		t.Fatal(err)
	}
	// No child can launch: the executable is deliberately absent, and each
	// cgroup is proven stopped/removed before the fixture is reclaimed.
	t.Cleanup(func() {
		if err := os.RemoveAll(fixture); err != nil {
			t.Errorf("reclaim failed-start fixture: %v", err)
		}
	})
	if err := os.Chmod(fixture, 0755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(fixture, "state")
	sources := filepath.Join(fixture, "sources")
	for _, dir := range []string{
		state, filepath.Join(state, "runtime"),
		filepath.Join(state, "runtime", "browser"), sources,
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	workspace := filepath.Join(sources, "worktree")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(workspace, int(workerUID), int(workerGID)); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(state, "browser-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, int(browserUID), int(browserGID)); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(state, "runtime", "browser", "isolated-sessions")
	if err := os.Mkdir(scratch, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(scratch, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scratch, 0711); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg: config.Config{
			WorkspaceDir: sources, StateDir: state,
			AgentUser: "aiagent", WorkerUser: "aiworker",
		},
		workerUID: workerUID, workerGID: workerGID,
		agentUID: agentUID, agentGID: agentGID,
		browserUID: browserUID, browserGID: browserGID,
		sessions: newStdioSessionBroker(),
		guard:    policy.New(nil),
		audit:    audit.New(filepath.Join(state, "audit.jsonl")),
	}
	if _, err := s.isolatedBrowserHome(); err != nil {
		t.Fatalf("synthetic root fixture is not an installed-layout match: %v", err)
	}
	granted, err := s.browserAdmission.tryMode("live-peer", "isolated", 2, nil)
	if err != nil || !granted {
		t.Fatalf("concurrent peer admission refused: %t %v", granted, err)
	}
	peerUID := s.browserAdmission.isolatedHostUID("live-peer")
	peerProfile := "asa-profile-" + strings.Repeat("a", 32)
	peerTrace := "asa-trace-" + strings.Repeat("a", 32)
	if err := s.prepareBrowserSessionScratch(peerUID, peerProfile, peerTrace); err != nil {
		t.Fatal(err)
	}
	peerMarker := filepath.Join(scratch, peerProfile, "peer-cookie")
	if err := os.WriteFile(peerMarker, []byte("PEER-PRESERVED"), 0600); err != nil {
		t.Fatal(err)
	}
	peerTraceMarker := filepath.Join(scratch, peerTrace, "peer-trace")
	if err := os.WriteFile(peerTraceMarker, []byte("PEER-TRACE"), 0600); err != nil {
		t.Fatal(err)
	}
	req := Request{
		Action: "browser_session_open", PrincipalID: "synthetic-owner",
		PrincipalClass: "direct", RequestID: "root-failed-browser",
		Workspace: workspace, BrowserProfileMode: "isolated",
	}
	const attempts = 12
	for attempt := 0; attempt < attempts; attempt++ {
		holder := fmt.Sprintf("failed-start-%d", attempt)
		granted, err := s.browserAdmission.tryMode(holder, "isolated", 2, nil)
		if err != nil || !granted {
			t.Fatalf("failed attempt %d could not reserve its own host UID: %t %v", attempt, granted, err)
		}
		hostUID := s.browserAdmission.isolatedHostUID(holder)
		if hostUID == peerUID || !validBrowserSessionHostUID(hostUID) {
			t.Fatalf("failed attempt reused live peer host UID: %d", hostUID)
		}
		group, err := createBrowserCgroup(s.cfg.EffectiveExecutorServiceUnit())
		if err != nil {
			t.Fatal(err)
		}
		// Passing an absent binary ensures the failure happens inside
		// the original broker after both exact scratch dirs are prepared,
		// when cmd.Start fails before Node/Chromium can execute.
		id, startErr := s.workerContainedBrowserSession(req, workspace,
			filepath.Join(fixture, "missing-node"), group, hostUID)
		if startErr == nil || id != "" {
			t.Fatalf("attempt %d unexpectedly launched: id=%q err=%v", attempt, id, startErr)
		}
		if errors.Is(startErr, errBrowserFailedLaunchCleanupUnverified) {
			t.Fatalf("attempt %d could not prove failed-launch cleanup: %v", attempt, startErr)
		}
		if group.path != "" {
			t.Fatalf("attempt %d left original private cgroup unverified: %s", attempt, group.path)
		}
		children, err := os.ReadDir(scratch)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 2 {
			t.Fatalf("attempt %d orphaned profile/trace while peer remained admitted: %v", attempt, children)
		}
		for _, entry := range children {
			if entry.Name() != peerProfile && entry.Name() != peerTrace {
				t.Fatalf("attempt %d deleted peer or retained its own directory: %s", attempt, entry.Name())
			}
		}
		if data, err := os.ReadFile(peerMarker); err != nil || string(data) != "PEER-PRESERVED" {
			t.Fatalf("attempt %d changed live peer cookie: %q %v", attempt, data, err)
		}
		if data, err := os.ReadFile(peerTraceMarker); err != nil || string(data) != "PEER-TRACE" {
			t.Fatalf("attempt %d changed live peer trace: %q %v", attempt, data, err)
		}
		if !s.browserAdmission.matches(holder) || s.browserAdmission.isolatedHostUID(holder) != hostUID {
			t.Fatal("failed launch unexpectedly changed reserved identity before verified cleanup")
		}
		if !s.browserAdmission.release(holder) {
			t.Fatal("failed launch remained reserved after verified cleanup")
		}
	}
	// When the kernel cgroup stop proof is unavailable, exact scratch
	// cannot be reclaimed or its UID recycled as though a failed launch
	// were a normal, verified stop. Simulate an unreachable private cgroup
	// before any child starts; recovery in this fixture is explicit only.
	granted, err = s.browserAdmission.tryMode("unknown-cleanup", "isolated", 2, nil)
	if err != nil || !granted {
		t.Fatalf("failed-stop fixture was not admitted: %t %v", granted, err)
	}
	unknownUID := s.browserAdmission.isolatedHostUID("unknown-cleanup")
	unknownGroup := &dapCgroup{path: filepath.Join(fixture, "missing-private-cgroup")}
	id, failErr := s.workerContainedBrowserSession(req, workspace,
		filepath.Join(fixture, "missing-node"), unknownGroup, unknownUID)
	if id != "" || !errors.Is(failErr, errBrowserFailedLaunchCleanupUnverified) {
		t.Fatalf("unverified cleanup was treated as a safe failed launch: id=%q err=%v", id, failErr)
	}
	children, err := os.ReadDir(scratch)
	if err != nil || len(children) != 4 {
		t.Fatalf("unverified cleanup silently removed unknown scratch: children=%v err=%v", children, err)
	}
	if !s.browserAdmission.matches("unknown-cleanup") ||
		s.browserAdmission.isolatedHostUID("unknown-cleanup") != unknownUID {
		t.Fatal("unverified failed launch released or changed the original host UID")
	}
	if ok, err := s.browserAdmission.tryMode("unsafe-replacement", "isolated", 2, nil); ok || err != nil {
		t.Fatalf("unverified scratch permitted UID reuse: admitted=%t err=%v", ok, err)
	}
	// This test's command could not have spawned a child because the
	// specified private cgroup path did not exist. Only the synthetic test
	// owner now reclaims exactly those two test-created names.
	for _, child := range children {
		if child.Name() == peerProfile || child.Name() == peerTrace {
			continue
		}
		if err := s.browserReclaimNamedScratch(child.Name(), true, unknownUID); err != nil {
			t.Fatalf("manual synthetic recovery failed: %v", err)
		}
	}
	if !s.browserAdmission.release("unknown-cleanup") {
		t.Fatal("synthetic recovery did not release original reservation")
	}
	for _, name := range []string{peerProfile, peerTrace} {
		if err := s.browserReclaimNamedScratch(name, true, peerUID); err != nil {
			t.Fatalf("exact peer fixture cleanup after verified no-child test: %v", err)
		}
	}
	if !s.browserAdmission.release("live-peer") {
		t.Fatal("peer reservation not released at fixture teardown")
	}
	fmt.Println("ROOT_BROWSER_FAILED_LAUNCH_EXACT_SCRATCH_CLEANUP_PASS")
}
