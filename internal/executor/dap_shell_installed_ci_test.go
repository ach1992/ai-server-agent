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
	"syscall"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/credential"
	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

// shellDAPCIFixtureGuard is test-only. No filesystem deletion is permitted
// after an attempted debug_launch until the original broker's cleanup has
// been positively verified, including failure/transport-uncertain responses.
type shellDAPCIFixtureGuard struct {
	workspace        string
	launchDispatched bool
	sessionID        string
	cleanupVerified  bool
}

// finish runs the same conservative logic for real installed CI and its
// deterministic regression tests. verifyStop must include the actual
// authenticated broker stop and session/cgroup retirement checks.
func (g *shellDAPCIFixtureGuard) finish(verifyStop func(string) bool) (retained bool, err error) {
	if g.launchDispatched && !g.cleanupVerified {
		if g.sessionID == "" || verifyStop == nil || !verifyStop(g.sessionID) {
			return true, nil
		}
		g.cleanupVerified = true
	}
	return false, os.RemoveAll(g.workspace)
}

// This opt-in test runs ONLY inside the disposable GitHub-hosted CI runner,
// after the source installer has started the real Agent/Root Executor units.
// It uses their original authenticated Unix transport and a scratch aiworker
// workspace, not a live/occupied development or production installation.
// No sysctl, unit configuration, global cgroup, or other session is changed.
func TestShellDAPInstalledPrivateCgroupLifecycleCI(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("ASA_REQUIRE_SHELL_DAP_INSTALLED_CI") != "1" {
		t.Skip("disposable installed Shell DAP CI fixture only")
	}
	if os.Geteuid() != 0 {
		t.Fatal("mandatory installed Shell DAP CI acceptance requires root caller")
	}
	if err := shellDAPIPCPrerequisite(); err != nil {
		t.Fatalf("mandatory CI runner lacks Shell IPC policy; no sysctl changes permitted: %v", err)
	}
	cfg, err := config.Load("/etc/ai-server-agent/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(cfg.WorkspaceDir) != "/srv/ai-workspace" {
		t.Fatalf("unexpected disposable CI worker workspace: %q", cfg.WorkspaceDir)
	}
	token, err := os.ReadFile(cfg.ExecutorToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for i := range token {
			token[i] = 0
		}
	}()
	secret := strings.TrimSpace(string(token))
	if secret == "" {
		t.Fatal("empty executor token")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	const principal = "ci-private-shell-dap-proof"
	requestID := "ci-private-shell-dap-proof"
	owner := credential.Principal{ID: principal, Class: "direct", Name: "CI Shell DAP"}
	ownerCtx := credential.WithPrincipal(ctx, owner)

	call := func(q Request) Response {
		t.Helper()
		q.RequestID = requestID
		r, e := ClientCallContext(ownerCtx, cfg.ExecutorSocket, secret, q)
		if e != nil {
			t.Fatalf("authenticated installed Executor call %s failed: %v", q.Action, e)
		}
		return r
	}

	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(worker.Uid)
	if err != nil || uid == 0 {
		t.Fatalf("invalid aiworker UID: %v", err)
	}
	gid, err := strconv.Atoi(worker.Gid)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(cfg.WorkspaceDir, "asa-shell-installed-ci-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := shellDAPCIFixtureGuard{workspace: workspace}
	var group string
	// Register before any launch dispatch. Unknown dispatch/cleanup retains
	// source evidence; only verified broker stop can authorize its removal.
	t.Cleanup(func() {
		retained, cleanupErr := fixture.finish(func(id string) bool {
			bounded, cancelCleanup := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancelCleanup()
			cleanupCtx := credential.WithPrincipal(bounded, owner)
			stop, stopErr := ClientCallContext(cleanupCtx, cfg.ExecutorSocket, secret, Request{
				Action: "debug_stop", SessionID: id, Workspace: workspace,
				RequestID: requestID,
			})
			if stopErr != nil || !stop.OK {
				t.Errorf("unverified test-owned Shell DAP stop; retaining fixture: err=%v code=%s", stopErr, stop.ErrorCode)
				return false
			}
			retired, statusErr := ClientCallContext(cleanupCtx, cfg.ExecutorSocket, secret, Request{
				Action: "debug_status", SessionID: id, Workspace: workspace,
				RequestID: requestID,
			})
			if statusErr != nil || retired.OK || retired.ErrorCode != "debug_session_not_found" {
				t.Errorf("test-owned Shell DAP retirement unverified; retaining fixture: err=%v code=%s", statusErr, retired.ErrorCode)
				return false
			}
			if group != "" {
				if _, statErr := os.Lstat(group); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("test-owned DAP cgroup removal unverified; retaining fixture: %v", statErr)
					return false
				}
			}
			return true
		})
		if cleanupErr != nil {
			t.Errorf("test-owned workspace cleanup: %v", cleanupErr)
		}
		if retained {
			t.Logf("test-owned workspace retained after uncertain Shell DAP launch/stop: %s", workspace)
		}
	})
	if err := os.Chown(workspace, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	const source = "#!/usr/bin/env bash\nalpha=7\nbeta=5\nTOTAL=$((alpha+beta))\nprintf '%s\\n' \"$TOTAL\" > witness.txt\n"
	if err := os.WriteFile(filepath.Join(workspace, "probe.sh"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Join(workspace, "probe.sh"), uid, gid); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(workspace, "witness.txt")
	stat := call(Request{Action: "workspace_stat", Workspace: workspace, Path: "probe.sh"})
	if !stat.OK || stat.FileVersion == "" {
		t.Fatalf("real aiworker source identity missing: code=%s", stat.ErrorCode)
	}
	parent := "/sys/fs/cgroup/system.slice/ai-server-agent-executor.service"
	groups := func() (map[string]bool, error) {
		paths, e := filepath.Glob(filepath.Join(parent, "asa-dap-*"))
		if e != nil {
			return nil, e
		}
		found := make(map[string]bool, len(paths))
		for _, p := range paths {
			found[p] = true
		}
		return found, nil
	}
	prior, err := groups()
	if err != nil {
		t.Fatal(err)
	}
	launch := Request{
		Action: "debug_launch", Language: "shell", DebugMode: "bash",
		Workspace: workspace, DebugProgram: "probe.sh", FileVersion: stat.FileVersion,
		DebugStopOnEntry: true,
	}
	launch.RequestID = requestID
	fixture.launchDispatched = true // Must precede transport, even if its result is lost.
	start, launchErr := ClientCallContext(ownerCtx, cfg.ExecutorSocket, secret, launch)
	fixture.sessionID = start.SessionID // Capture even from an unsuccessful response.
	if launchErr != nil {
		t.Fatalf("installed Shell DAP launch transport outcome uncertain; fixture retained: %v", launchErr)
	}
	if !start.OK || start.SessionID == "" {
		if !systemexec.Trusted("/usr/local/bin/ai-server-agent") {
			// Diagnostic metadata only; no change to original root-executable checks.
			for _, path := range []string{"/", "/usr", "/usr/local", "/usr/local/bin", "/usr/local/bin/ai-server-agent"} {
				info, statErr := os.Lstat(path)
				if statErr != nil {
					t.Logf("untrusted Agent path %q: %v", path, statErr)
					continue
				}
				meta, ok := info.Sys().(*syscall.Stat_t)
				if !ok {
					t.Logf("untrusted Agent path %q: metadata unavailable", path)
					continue
				}
				t.Logf("untrusted Agent component path=%q mode=%s uid=%d gid=%d", path, info.Mode(), meta.Uid, meta.Gid)
			}
		}
		t.Fatalf("real installed Shell DAP launch failed: code=%s class=%s reason=%q trusted_bash=%t trusted_installed_agent=%t", start.ErrorCode, start.ErrorClass, start.Error, systemexec.Trusted("/usr/bin/bash"), systemexec.Trusted("/usr/local/bin/ai-server-agent"))
	}
	id := start.SessionID
	after, err := groups()
	if err != nil {
		t.Fatal(err)
	}
	for p := range after {
		if !prior[p] {
			if group != "" {
				t.Fatalf("more than one new DAP cgroup in disposable runner: %q and %q", group, p)
			}
			group = p
		}
	}
	if group == "" {
		t.Fatal("real installed Executor failed to create a private DAP cgroup")
	}
	fi, err := os.Stat(group)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("private DAP cgroup lacks inspectable inode owner metadata")
	}
	if st.Uid != 0 || st.Gid != 0 || fi.Mode().Perm() != 0700 {
		t.Fatalf("private DAP cgroup lacks original root ownership/mode: uid=%d gid=%d mode=%s", st.Uid, st.Gid, fi.Mode())
	}
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("script executed before Root Executor activation: %v", err)
	}

	action := func(method string, thread int) Response {
		t.Helper()
		r := call(Request{Action: "debug_action", SessionID: id, Workspace: workspace, DebugMethod: method, DebugThreadID: thread})
		if !r.OK {
			t.Fatalf("real Shell DAP %s failed: code=%s class=%s", method, r.ErrorCode, r.ErrorClass)
		}
		return r
	}
	bp := call(Request{
		Action: "debug_action", SessionID: id, Workspace: workspace,
		DebugMethod: "breakpoints", Path: "probe.sh", FileVersion: stat.FileVersion, DebugLines: []int{5},
	})
	if !bp.OK {
		t.Fatalf("real Shell source breakpoint registration failed: code=%s", bp.ErrorCode)
	}
	configured := action("configure", 0)

	// The actual Bash process must be running as aiworker, still stopped in
	// the test's newly created private root-owned cgroup, before any marker write.
	pids, err := os.ReadFile(filepath.Join(group, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	bash, err := os.Stat("/usr/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	bashPID := 0
	for _, line := range strings.Fields(string(pids)) {
		pid, e := strconv.Atoi(line)
		if e != nil {
			t.Fatal(e)
		}
		exe, e := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
		if e == nil && os.SameFile(exe, bash) {
			status, e := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
			if e != nil || !strings.Contains(string(status), fmt.Sprintf("Uid:\t%d\t", uid)) {
				t.Fatalf("real Bash child is not verified worker UID %d: %v", uid, e)
			}
			bashPID = pid
		}
	}
	if bashPID <= 0 {
		t.Fatal("root-owned private cgroup does not contain an actual pinned Bash child")
	}
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker script ran before its entry stop: %v", err)
	}

	waitFor := func(reason string, immediate Response) {
		t.Helper()
		// A response can already carry the stopped event; status polls only
		// receive subsequently retained frames after that response consumes it.
		if strings.Contains(immediate.Output, "\"event\":\"stopped\"") &&
			strings.Contains(immediate.Output, "\"reason\":\""+reason+"\"") {
			return
		}
		for deadline := time.Now().Add(7 * time.Second); time.Now().Before(deadline); {
			resp := call(Request{Action: "debug_status", SessionID: id, Workspace: workspace})
			if !resp.OK {
				t.Fatalf("real root session status failed while waiting for %s: %s", reason, resp.ErrorCode)
			}
			if strings.Contains(resp.Output, "\"event\":\"stopped\"") &&
				strings.Contains(resp.Output, "\"reason\":\""+reason+"\"") {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("no real Shell DAP %s stop from installed Executor", reason)
	}
	waitFor("entry", configured)
	stack := action("stackTrace", 1)
	if !strings.Contains(stack.Output, "probe.sh") || strings.Contains(stack.Output, "/proc/self/fd/6") {
		t.Fatalf("original Shell source mapping lost: %q", stack.Output)
	}

	other := Request{Action: "debug_status", SessionID: id, Workspace: workspace,
		RequestID: "ci-wrong-principal"}
	otherCtx := credential.WithPrincipal(ctx, credential.Principal{ID: "unrelated-ci-principal", Class: "direct", Name: "foreign CI caller"})
	denied, e := ClientCallContext(otherCtx, cfg.ExecutorSocket, secret, other)
	if e != nil || denied.OK {
		t.Fatalf("cross-principal session status was not denied: %v %v", e, denied.OK)
	}
	continued := action("continue", 1)
	waitFor("breakpoint", continued)
	vars := call(Request{Action: "debug_action", SessionID: id, Workspace: workspace,
		DebugMethod: "variables", DebugReference: 1})
	if !vars.OK || !strings.Contains(vars.Output, "alpha") || !strings.Contains(vars.Output, "beta") {
		t.Fatalf("real Bash scalar variables missing: code=%s", vars.ErrorCode)
	}
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Bash executed breakpointed source line before continue: %v", err)
	}
	stop := call(Request{Action: "debug_stop", SessionID: id, Workspace: workspace})
	if !stop.OK {
		t.Fatalf("real pinned Bash root-owned cgroup cleanup failed: code=%s", stop.ErrorCode)
	}
	if _, err := os.Lstat(group); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private root-owned DAP cgroup not removed after verified stop: %v", err)
	}
	retired := call(Request{Action: "debug_status", SessionID: id, Workspace: workspace})
	if retired.OK || retired.ErrorCode != "debug_session_not_found" {
		t.Fatalf("retired Shell DAP session remains addressable or retirement is unverified: code=%s", retired.ErrorCode)
	}
	fixture.cleanupVerified = true
	t.Logf("PASS installed Root Executor/private cgroup Shell DAP: original Bash UID, stop, stack/variables, principal denial and verified cleanup")
}
