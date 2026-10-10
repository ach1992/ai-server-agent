package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/audit"
)

func TestRealGoDelveDebugLoopWorker(t *testing.T) { runRealGoDelveDebugLoopWorker(t, "manual") }

func TestRealGoDelveTTLStopsDebuggee(t *testing.T) { runRealGoDelveDebugLoopWorker(t, "expiry") }

func TestRealGoDelveCrashKillsDebuggee(t *testing.T) { runRealGoDelveDebugLoopWorker(t, "crash") }

func runRealGoDelveDebugLoopWorker(t *testing.T, shutdown string) {
	dlv := os.Getenv("AGENT_DLV_PROOF_BIN")
	if dlv == "" {
		t.Skip("Delve reference fixture not provided")
	}
	goBin := os.Getenv("AGENT_GO_PROOF_BIN")
	if goBin == "" {
		var err error
		goBin, err = exec.LookPath("go")
		if err != nil {
			t.Skip("Go compiler not available")
		}
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.audit = audit.New(filepath.Join(s.cfg.StateDir, "audit.jsonl"))
	helper := filepath.Join(t.TempDir(), "ai-server-agent")
	cmd := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	cmd.Dir = filepath.Join("..", "..")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build workspace helper: %v %s", err, b)
	}
	s.workspaceHelperBinary = helper
	s.dlvBinary = dlv
	source := `package main
import "fmt"
func score(v int) int {
    result:=v+1
    return result
}
func main(){
    answer:=score(41)
    fmt.Println("ANSWER",answer)
}
`
	srcFile := filepath.Join(workspace, "main.go")
	if err := os.WriteFile(srcFile, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.org/agent-dap-acceptance\n\ngo 1.23.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(workspace, "demo")
	build := exec.Command(goBin, "build", "-gcflags=all=-N -l", "-o", binPath, ".")
	build.Dir = workspace
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build debug binary: %v %s", err, b)
	}
	stat := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "demo"})
	if !stat.OK {
		t.Fatalf("debug binary worker stat: %+v", stat)
	}
	rootAttempt := owner
	rootAttempt.Root = true
	rootAttempt.Approval = true
	rootAttempt.Action = "debug_launch"
	if x := s.debugAction(context.Background(), rootAttempt); x.OK || x.ErrorCode != "debug_worker_only" {
		t.Fatalf("privileged debug wrongly accepted: %+v", x)
	}
	owner.Action = "debug_launch"
	owner.DebugProgram = "demo"
	owner.FileVersion = stat.FileVersion
	owner.DebugStopOnEntry = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opened := s.debugAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("real Delve launch: %+v", opened)
	}
	owner.SessionID = opened.SessionID
	_, backend, backendErr := s.debugEntry(owner)
	if backendErr != nil {
		t.Fatal(backendErr)
	}
	privateSocketDir := backend.socketDir
	if info, err := os.Lstat(privateSocketDir); err != nil || info.Mode().Perm() != 0710 {
		t.Fatalf("private DAP socket directory ownership/mode: %v %v", info, err)
	}
	defer func() { owner.Action = "debug_stop"; _ = s.debugAction(context.Background(), owner) }()
	t.Logf("launch: %s %s", opened.SessionID, opened.Output)
	intruder := owner
	intruder.PrincipalID = "other-principal"
	intruder.Action = "debug_status"
	if x := s.debugAction(ctx, intruder); x.OK || x.ErrorCode != "debug_session_not_found" {
		t.Fatalf("other principal debug access granted: %+v", x)
	}
	otherWorkspace := owner
	otherWorkspace.Workspace = s.cfg.WorkspaceDir
	otherWorkspace.Action = "debug_status"
	if x := s.debugAction(ctx, otherWorkspace); x.OK || x.ErrorCode != "debug_session_not_found" {
		t.Fatalf("other workspace debug access granted: %+v", x)
	}
	call := func(method string) Response {
		t.Helper()
		owner.Action = "debug_action"
		owner.DebugMethod = method
		x := s.debugAction(ctx, owner)
		if !x.OK {
			t.Fatalf("real %s failed: %+v", method, x)
		}
		t.Logf("%s %s", method, x.Output)
		return x
	}
	sourceStat := s.workerWorkspaceFile(ctx, Request{Action: "workspace_stat", Workspace: workspace, Path: "main.go"})
	if !sourceStat.OK {
		t.Fatal(sourceStat.Error)
	}
	owner.Path = "main.go"
	owner.FileVersion = "stale-version"
	owner.DebugLines = []int{4}
	owner.Action = "debug_action"
	owner.DebugMethod = "breakpoints"
	if stale := s.debugAction(ctx, owner); stale.OK || stale.ErrorCode != "debug_source_changed" {
		t.Fatalf("stale source breakpoint was accepted: %+v", stale)
	}
	owner.FileVersion = sourceStat.FileVersion
	br := call("breakpoints")
	if !strings.Contains(br.Output, "verified") {
		t.Fatalf("breakpoint not confirmed: %+v", br)
	}
	var initialEvents struct {
		Events []struct {
			Event string `json:"event"`
			Body  struct {
				SystemProcessID int `json:"systemProcessId"`
			} `json:"body"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(br.Output), &initialEvents); err != nil {
		t.Fatal(err)
	}
	debuggeePID := 0
	for _, event := range initialEvents.Events {
		if event.Event == "process" {
			debuggeePID = event.Body.SystemProcessID
		}
	}
	if debuggeePID <= 0 {
		t.Fatalf("Delve did not supply actual debuggee PID: %s", br.Output)
	}
	owner.DebugLines = nil
	call("configure")
	owner.DebugThreadID = 1
	call("continue")
	time.Sleep(250 * time.Millisecond)
	owner.Action = "debug_status"
	status := s.debugAction(ctx, owner)
	t.Logf("status: %s", status.Output)
	th := call("threads")
	var threadBody struct {
		Result struct {
			Threads []struct {
				ID int `json:"id"`
			} `json:"threads"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(th.Output), &threadBody); err != nil || len(threadBody.Result.Threads) == 0 {
		t.Fatalf("invalid real threads: %v %s", err, th.Output)
	}
	owner.DebugThreadID = threadBody.Result.Threads[0].ID
	st := call("stackTrace")
	var stack struct {
		Result struct {
			StackFrames []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"stackFrames"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(st.Output), &stack); err != nil || len(stack.Result.StackFrames) == 0 {
		t.Fatalf("invalid stack %v %s", err, st.Output)
	}
	owner.DebugFrameID = stack.Result.StackFrames[0].ID
	scopes := call("scopes")
	var scope struct {
		Result struct {
			Scopes []struct {
				VariablesReference int `json:"variablesReference"`
			} `json:"scopes"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(scopes.Output), &scope); err != nil || len(scope.Result.Scopes) == 0 {
		t.Fatalf("invalid scopes %v %s", err, scopes.Output)
	}
	owner.DebugReference = scope.Result.Scopes[0].VariablesReference
	call("variables")
	owner.DebugExpression = "1+2"
	call("evaluate")
	call("next")
	// An external workspace edit must fail closed for source-sensitive
	// debugger actions, without preventing a subsequent explicit stop.
	if err := os.WriteFile(srcFile, []byte(source+"//changed externally\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.Action = "debug_action"
	owner.DebugMethod = "threads"
	if x := s.debugAction(ctx, owner); x.OK || x.ErrorCode != "debug_source_changed" {
		t.Fatalf("source changed during debugger session was not detected: %+v", x)
	}
	owner.Action = "debug_stop"
	switch shutdown {
	case "expiry":
		e, err := s.sessions.get(owner, opened.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		s.expireProcessSession(e)
	case "crash":
		e, err := s.sessions.get(owner, opened.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		adapterPID, pin := e.pid, e.pidPin
		e.mu.Unlock()
		if pin == nil || adapterPID <= 0 {
			t.Fatal("adapter process identity not pinned")
		}
		if err := syscall.Kill(debuggeePID, 0); err != nil {
			t.Fatalf("debuggee not live at crash proof start: %v", err)
		}
		// Simulate an abrupt Delve crash, with no DAP disconnect handshake.
		// Only this newly created, pidfd-pinned disposable adapter is killed.
		if err := syscall.Kill(adapterPID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Fatal("broker did not reconcile adapter SIGKILL")
		}
		e.mu.Lock()
		cleanupErr := e.cleanupErr
		e.mu.Unlock()
		if cleanupErr != nil {
			t.Fatalf("crashed Delve left cleanup unverified: %v", cleanupErr)
		}
		if err := s.workerStdioClose(owner, opened.SessionID); err != nil {
			t.Fatalf("crash session cleanup: %v", err)
		}
	case "manual":
		stop := s.debugAction(ctx, owner)
		if !stop.OK {
			t.Fatalf("debug terminate and cleanup: %+v", stop)
		}
	default:
		t.Fatalf("unexpected test shutdown mode %q", shutdown)
	}
	if _, err := os.Lstat(privateSocketDir); !os.IsNotExist(err) {
		t.Fatalf("DAP socket directory remained after verified cleanup: %v", err)
	}
	// Do not mistake closing only the adapter socket for actually stopping
	// the traced debuggee (which may have an independent process group).
	exited := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(debuggeePID, 0)
		if errors.Is(err, syscall.ESRCH) {
			exited = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !exited {
		t.Fatalf("debuggee PID %d survived reported successful stop", debuggeePID)
	}
	owner.Action = "debug_status"
	if x := s.debugAction(ctx, owner); x.OK {
		t.Fatalf("closed debug session remained addressable: %+v", x)
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "1+2") || strings.Contains(string(data), "ANSWER") {
		t.Fatal("DAP expression or debuggee values leaked into audit")
	}
	expectedActions := []string{"debug_launch", "debug_action", "debug_status"}
	if shutdown != "crash" {
		expectedActions = append(expectedActions, "debug_stop")
	}
	for _, action := range expectedActions {
		if strings.Count(string(data), `"action":"`+action+`"`) < 2 {
			t.Fatalf("DAP audit missing start/completion for %s", action)
		}
	}
}
