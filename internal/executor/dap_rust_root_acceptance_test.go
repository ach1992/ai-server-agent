package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

// Operator-gated, fixture-only source acceptance against actual Debian LLDB19.
// The real product debug_launch/debug_action/debug_status/debug_stop paths
// must launch the original adapter AND Rust ELF in one private root cgroup.
// Does not install or modify an Agent/Executor service and does not prove
// trusted system-wide LLDB provisioning or connected installed MCP.
func TestApprovedRootRustDAPBrokerCgroupEndToEnd(t *testing.T) {
	const host = "hDE01"
	actual, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if actual != host || os.Geteuid() != 0 || os.Getenv("ASA_ROOT_RUST_DAP_APPROVED_HOST") != host {
		t.Skip("root Rust DAP acceptance requires explicitly authorized developer executor")
	}
	const proof = "/srv/ai-workspace/asa140-lldb-proof"
	const wrapper = "/srv/ai-workspace/asa140-rust-dap-bundle-prototype/bin/lldb-dap"
	const helper = "/srv/ai-workspace/asa140-root-e2e-20261015/worker-helper"
	for _, file := range []string{wrapper, helper, filepath.Join(proof, "rust_probe"), filepath.Join(proof, "rust_probe.rs")} {
		fi, err := os.Lstat(file)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("unexpected fixture file %s: %v", file, err)
		}
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(worker.Uid)
	if err != nil || uid == 0 {
		t.Fatalf("unexpected worker uid %v", err)
	}
	gid, err := strconv.Atoi(worker.Gid)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.Chmod(state, 0755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(state, "worker-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, uid, gid); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:       config.Config{WorkspaceDir: proof, StateDir: state},
		workerUID: uint32(uid), workerGID: uint32(gid),
		sessions: newStdioSessionBroker(), runs: newRunLimiterWith(2, 1),
		workspaceHelperBinary: helper, rustLLDBDAPBinary: wrapper,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 47*time.Second)
	defer cancel()
	stat := func(path string) string {
		t.Helper()
		r := s.workerWorkspaceFile(ctx, Request{Action: "workspace_stat", Workspace: proof, Path: path})
		if !r.OK || r.FileVersion == "" {
			t.Fatalf("worker source identity missing for %s: %+v", path, r)
		}
		return r.FileVersion
	}
	binVersion, srcVersion := stat("rust_probe"), stat("rust_probe.rs")
	req := Request{
		Action: "debug_launch", PrincipalID: "rust-root-proof", PrincipalClass: "direct",
		RequestID: "rust-root-proof", Language: "rust", DebugMode: "exec", Workspace: proof,
		DebugProgram: "rust_probe", FileVersion: binVersion, DebugStopOnEntry: true,
	}
	start := s.debugAction(ctx, req)
	if !start.OK || start.SessionID == "" {
		t.Fatalf("real contained Rust launch unavailable: %+v", start)
	}
	id := start.SessionID
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			if err := s.workerStdioClose(req, id); err != nil {
				t.Errorf("Rust test-owned process cleanup unknown: %v", err)
			}
		}
	})
	entry, err := s.sessions.get(req, id)
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	d, pid := entry.dap, entry.pid
	entry.mu.Unlock()
	if d == nil || d.containment == nil || !d.containment.verified ||
		!d.containment.containsPID(pid) {
		t.Fatal("real LLDB original adapter not atomically cgroup-contained")
	}
	group := d.containment.path
	if !strings.Contains(group, "/ai-server-agent-executor.service/asa-dap-") {
		t.Fatalf("invalid private adapter cgroup path %q", group)
	}
	t.Logf("real LLDB adapter cgroup membership confirmed pid=%d", pid)
	call := func(method string, thread int) Response {
		r := req
		r.Action, r.SessionID, r.DebugMethod, r.DebugThreadID = "debug_action", id, method, thread
		return s.debugAction(ctx, r)
	}
	bp := req
	bp.Action, bp.SessionID, bp.DebugMethod = "debug_action", id, "breakpoints"
	bp.Path, bp.FileVersion, bp.DebugLines = "rust_probe.rs", srcVersion, []int{3}
	if r := s.debugAction(ctx, bp); !r.OK {
		t.Fatalf("real Rust source breakpoints failed: %+v", r)
	}
	if r := call("configure", 0); !r.OK {
		t.Fatalf("real Rust configuration failed: %+v", r)
	}
	// LLDB may first stop at entry in the dynamic loader, before .rs symbols.
	// Continue once and demand a real source breakpoint rather than an entry.
	threads := call("threads", 0)
	if !threads.OK {
		t.Fatalf("Rust DAP threads missing: %+v", threads)
	}
	var threadObj struct {
		Result struct {
			Threads []struct {
				ID int `json:"id"`
			} `json:"threads"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(threads.Output), &threadObj); err != nil ||
		len(threadObj.Result.Threads) == 0 || threadObj.Result.Threads[0].ID <= 0 {
		t.Fatalf("Rust thread identity invalid: %v %s", err, threads.Output)
	}
	thread := threadObj.Result.Threads[0].ID
	running := call("continue", thread)
	if !running.OK {
		t.Fatalf("Rust DAP continue failed: %+v", running)
	}
	hit := false
	for until := time.Now().Add(12 * time.Second); time.Now().Before(until); {
		status := req
		status.Action, status.SessionID = "debug_status", id
		r := s.debugAction(ctx, status)
		if !r.OK {
			t.Fatalf("Rust DAP status missing: %+v", r)
		}
		if strings.Contains(r.Output, "\"reason\":\"breakpoint\"") {
			hit = true
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if !hit {
		t.Fatal("no verified Rust source breakpoint event after real LLDB continue")
	}
	stack := call("stackTrace", thread)
	if !stack.OK || !strings.Contains(stack.Output, "rust_probe.rs") {
		t.Fatalf("real Rust source stack missing: %+v", stack)
	}
	stop := req
	stop.Action, stop.SessionID = "debug_stop", id
	stopped := s.debugAction(ctx, stop)
	if !stopped.OK {
		t.Fatalf("Rust stop/cleanup failed: %+v", stopped)
	}
	cleaned = true
	if _, err := os.Lstat(group); !os.IsNotExist(err) {
		t.Fatalf("Rust cgroup not removed: %s (%v)", group, err)
	}
	if _, err := s.sessions.get(req, id); err == nil {
		t.Fatal("retired Rust debug session still addressable")
	}
	t.Logf("PASS real Rust LLDB DAP source path+breakpoint+stack+root cgroup cleanup")
}
