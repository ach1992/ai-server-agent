package executor

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/credential"
)

// Real, opt-in, test-owned systemd units and a private executable/config/
// socket. Never install, restart, reconfigure, or purge the host Agent.
// This is not a substitute for later clean-install/release acceptance.
func TestSharedRuntimeRealIsolatedSystemd(t *testing.T) {
	host, _ := os.Hostname()
	binary := os.Getenv("ASA137_E2E_BINARY")
	if host != "hDE01" || os.Geteuid() != 0 || os.Getenv("ASA137_E2E_HOST") != host || binary == "" {
		t.Skip("only explicitly authorized isolated root systemd test on hDE01")
	}
	fi, err := os.Stat(binary)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0111 == 0 {
		t.Fatalf("candidate test-only executable unavailable: %v", err)
	}
	worker, err := user.Lookup("aiworker")
	if err != nil {
		t.Fatal(err)
	}
	uid64, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid64, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := int(uid64), int(gid64)
	base, err := os.MkdirTemp("/tmp", "asa137-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	state := filepath.Join(base, "state")
	home := filepath.Join(state, "worker-home")
	root := filepath.Join(base, "work")
	logDir := filepath.Join(base, "logs")
	for _, dir := range []struct {
		path   string
		mode   os.FileMode
		worker bool
	}{
		{state, 0711, false}, {filepath.Join(state, "jobs"), 0711, false},
		{home, 0700, true}, {root, 0700, true}, {logDir, 0700, false},
	} {
		if err := os.Mkdir(dir.path, dir.mode); err != nil {
			t.Fatal(err)
		}
		if dir.worker {
			if err := os.Chown(dir.path, uid, gid); err != nil {
				t.Fatal(err)
			}
		}
	}
	paths := map[string]string{}
	for _, name := range []string{"project-a", "project-b", "scratch"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, uid, gid); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	token := "asa137-only-isolated-test-executor-token"
	tokenPath := filepath.Join(base, "executor.token")
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.WorkspaceDir = root
	cfg.StateDir = state
	cfg.LogDir = logDir
	cfg.ExecutorSocket = filepath.Join(base, "executor.sock")
	cfg.ExecutorToken = tokenPath
	cfg.BearerTokenFile = filepath.Join(base, "mcp.token")
	cfg.CredentialStoreFile = ""
	cfg.WorkerUser = "aiworker"
	cfg.AgentUser = "aiagent"
	cfgPath := filepath.Join(base, "config.json")
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	// Candidate's actual Go production binary owns the private root executor.
	// Only this private subprocess is terminated by this fixture.
	runner := exec.Command(binary, "-config", cfgPath, "executor")
	runner.Stdout = os.Stderr
	runner.Stderr = os.Stderr
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = runner.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- runner.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = runner.Process.Kill()
			<-done
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	ownerCtx := credential.WithPrincipal(ctx, credential.Principal{ID: "asa137-owner", Class: "direct", Name: "test-owner"})
	otherCtx := credential.WithPrincipal(ctx, credential.Principal{ID: "asa137-other", Class: "direct", Name: "test-other"})
	call := func(c context.Context, req Request) Response {
		t.Helper()
		resp, err := ClientCallContext(c, cfg.ExecutorSocket, token, req)
		if err != nil {
			t.Fatalf("private executor call %s: %v", req.Action, err)
		}
		return resp
	}
	readyDeadline := time.Now().Add(6 * time.Second)
	for {
		if _, err := os.Stat(cfg.ExecutorSocket); err == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatal("isolated executor socket never opened")
		}
		time.Sleep(25 * time.Millisecond)
	}
	testCommand := "printf 'CWD=%s HOME=%s XDG_CACHE_HOME=%s\\n' \"$PWD\" \"$HOME\" \"$XDG_CACHE_HOME\""
	for _, test := range []struct{ path, expect string }{
		{"", root}, {paths["project-a"], paths["project-a"]}, {paths["project-b"], paths["project-b"]}, {paths["scratch"], paths["scratch"]},
	} {
		result := call(ownerCtx, Request{Action: "run", Command: testCommand, Workspace: test.path})
		if !result.OK || !strings.Contains(result.Output, "CWD="+test.expect+" HOME="+home+" XDG_CACHE_HOME="+filepath.Join(home, ".cache")) {
			t.Fatalf("root-executor worker cwd/home mismatch: %+v", result)
		}
	}
	rootMismatch := call(ownerCtx, Request{Action: "start_job", Command: "printf no", Root: true, Approval: true, Workspace: paths["project-a"]})
	if rootMismatch.OK || rootMismatch.ErrorCode != "invalid_workspace" {
		t.Fatalf("root job silently ignored a worker-only CWD: %+v", rootMismatch)
	}
	// Each handle here is created by this test's private executor. Cleanup
	// stops only those exact opaque IDs, never enumerated pre-existing units.
	owned := []string{}
	t.Cleanup(func() {
		for _, id := range owned {
			if _, err := safeID(id); err == nil {
				_ = exec.Command("/usr/bin/systemctl", "stop", "ai-job-"+id+".service").Run()
			}
		}
	})
	start := func(kind, workspace, label, key string) Response {
		t.Helper()
		req := Request{Action: "start_job", Kind: kind, Workspace: workspace, Label: label, OperationID: key}
		req.Command = fmt.Sprintf("printf 'READY_%s\\n'; printf 'HOME=%%s\\n' \"$HOME\"; while true; do sleep 1; done", kind)
		resp := call(ownerCtx, req)
		if resp.JobID != "" {
			owned = append(owned, resp.JobID)
		}
		return resp
	}
	// Service has explicit label; task remains default finite job.
	first := start("service", paths["project-a"], "service-a", "asa137-a-serve")
	if !first.OK || first.Status != "accepted" || first.JobID == "" {
		t.Fatalf("service launch: %+v", first)
	}
	// A stopped/restarted private executor must rediscover the same transient
	// systemd service without re-launching the underlying side effect.
	if err := runner.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = runner.Wait()
	runner = exec.Command(binary, "-config", cfgPath, "executor")
	runner.Stdout, runner.Stderr = os.Stderr, os.Stderr
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	reconnectDeadline := time.Now().Add(6 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("unix", cfg.ExecutorSocket, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(reconnectDeadline) {
			t.Fatalf("private executor failed to restart: %v", dialErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	afterRestart := call(ownerCtx, Request{Action: "runtime_list", Workspace: paths["project-a"], Kind: "service"})
	if !afterRestart.OK || len(afterRestart.RuntimeEntries) != 1 ||
		afterRestart.RuntimeEntries[0].ID != first.JobID || afterRestart.RuntimeEntries[0].Status != "active" {
		t.Fatalf("service did not survive private executor restart: %+v", afterRestart)
	}
	replay := call(ownerCtx, Request{Action: "start_job", Kind: "service", Workspace: paths["project-a"],
		Label: "service-a", OperationID: "asa137-a-serve", Command: "printf 'READY_service\\n'; printf 'HOME=%s\\n' \"$HOME\"; while true; do sleep 1; done"})
	if !replay.OK || !replay.IdempotentReplay || replay.JobID != first.JobID {
		t.Fatalf("safe replay failed: %+v", replay)
	}
	conflict := call(ownerCtx, Request{Action: "start_job", Kind: "service", Workspace: paths["project-b"],
		Label: "service-a", OperationID: "asa137-a-serve", Command: "printf 'READY_service\\n'; printf 'HOME=%s\\n' \"$HOME\"; while true; do sleep 1; done"})
	if conflict.OK || conflict.ErrorCode != "idempotency_conflict" {
		t.Fatalf("cross-project operation key collision escaped: %+v", conflict)
	}
	second := start("service", paths["project-b"], "service-b", "asa137-b-serve")
	if !second.OK {
		t.Fatalf("second foreground service: %+v", second)
	}
	excess := start("service", paths["scratch"], "service-c", "asa137-c-serve")
	if excess.OK || excess.ErrorCode != "resource_limit" {
		t.Fatalf("service quota ignored: %+v", excess)
	}
	tasks := []Response{}
	for i := 0; i < 4; i++ {
		got := start("task", paths["scratch"], "task-load", fmt.Sprintf("asa137-scratch-%d", i))
		if !got.OK {
			t.Fatalf("task %d denied despite independent quota: %+v", i, got)
		}
		tasks = append(tasks, got)
	}
	overTask := start("task", paths["project-a"], "task-extra", "asa137-extra")
	if overTask.OK || overTask.ErrorCode != "resource_limit" {
		t.Fatalf("task quota overbooked: %+v", overTask)
	}
	listing := call(ownerCtx, Request{Action: "runtime_list", Workspace: paths["project-a"]})
	if !listing.OK || listing.Truncated || listing.RuntimeCapacity == nil ||
		!listing.RuntimeCapacity.StateReliable ||
		listing.RuntimeCapacity.TasksActive != 4 || listing.RuntimeCapacity.ServicesActive != 2 {
		t.Fatalf("passive global capacity/inventory: %+v", listing)
	}
	if len(listing.RuntimeEntries) != 1 || listing.RuntimeEntries[0].ID != first.JobID ||
		listing.RuntimeEntries[0].Kind != "service" || listing.RuntimeEntries[0].Status != "active" {
		t.Fatalf("workspace-scoped service not recoverable: %+v", listing)
	}
	other := call(otherCtx, Request{Action: "runtime_list", Workspace: paths["project-a"]})
	if !other.OK || len(other.RuntimeEntries) != 0 {
		t.Fatalf("principal disclosure via inventory: %+v", other)
	}
	out := call(ownerCtx, Request{Action: "job_output", JobID: first.JobID, Offset: 0, Limit: 1024})
	if !out.OK || !strings.Contains(out.Output, "HOME="+home) {
		t.Fatalf("service runner failed shared HOME or bounded output: %+v", out)
	}
	stopped := call(ownerCtx, Request{Action: "job_stop", JobID: first.JobID})
	if !stopped.OK || stopped.Status != "stopped" {
		t.Fatalf("service was not definitively stopped: %+v", stopped)
	}
	// A different service remains available, proving stop is ID-scoped.
	after := call(ownerCtx, Request{Action: "runtime_list", Workspace: paths["project-b"], Kind: "service"})
	if !after.OK || len(after.RuntimeEntries) != 1 || after.RuntimeEntries[0].ID != second.JobID ||
		after.RuntimeEntries[0].Status != "active" {
		t.Fatalf("other project was interrupted: %+v", after)
	}
	for _, task := range tasks {
		stopped := call(ownerCtx, Request{Action: "job_stop", JobID: task.JobID})
		if !stopped.OK || stopped.Status != "stopped" {
			t.Fatalf("task stop not verified: %+v", stopped)
		}
	}
	stopped = call(ownerCtx, Request{Action: "job_stop", JobID: second.JobID})
	if !stopped.OK || stopped.Status != "stopped" {
		t.Fatalf("second service stop not verified: %+v", stopped)
	}
	t.Logf("verified isolated production runner: 4 tasks, 2 services, source client CWD/HOME, principal inventory and exact ID stops")
}
