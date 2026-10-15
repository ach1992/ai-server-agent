package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonDAPRejectsUnsafeModesBeforeExecution(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	owner.Action = "debug_launch"
	owner.Language = "python"
	owner.DebugProgram = "src/sample.py"
	owner.FileVersion = "v1:fake"
	owner.DebugMode = "attach"
	if out := s.debugAction(context.Background(), owner); out.OK || out.ErrorCode != "python_dap_unsupported_mode" {
		t.Fatalf("remote/PID attach accepted: %+v", out)
	}
	owner.DebugMode = "script"
	owner.DebugProgram = "../../outside.py"
	if out := s.debugAction(context.Background(), owner); out.OK || out.ErrorCode != "python_dap_invalid_source" {
		t.Fatalf("unsafe Python source accepted: %+v", out)
	}
	owner.DebugProgram = "src/sample.go"
	if out := s.debugAction(context.Background(), owner); out.OK || out.ErrorCode != "python_dap_invalid_source" {
		t.Fatalf("cross-language executable accepted: %+v", out)
	}
	owner.DebugProgram = "__init__.py"
	owner.DebugMode = "module"
	if out := s.debugAction(context.Background(), owner); out.OK || out.ErrorCode != "python_dap_invalid_module" {
		t.Fatalf("package initializer accepted as module/test: %+v", out)
	}
	owner.DebugMode = "test"
	if out := s.debugAction(context.Background(), owner); out.OK || out.ErrorCode != "python_dap_invalid_module" {
		t.Fatalf("package initializer accepted as test module: %+v", out)
	}
	owner.DebugMode = "script"
	owner.Root = true
	owner.Approval = true
	if out := s.debugAction(context.Background(), owner); out.OK || out.ErrorCode != "debug_worker_only" {
		t.Fatalf("root Python debug admitted: %+v", out)
	}
	s.sessions.mu.Lock()
	n := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if n != 0 {
		t.Fatalf("invalid Python modes spawned %d sessions", n)
	}
}

func TestPythonDAPTypedPassiveAdapterStatus(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	owner.Action = "debug_adapter_status"
	owner.Language = "python"
	resp := s.debugAction(context.Background(), owner)
	if !resp.OK || !strings.Contains(resp.Output, `"adapter":"python/debugpy"`) ||
		!strings.Contains(resp.Output, `"attach_supported":false`) ||
		!strings.Contains(resp.Output, `"module"`) {
		t.Fatalf("Python adapter falsely reported capability: %+v", resp)
	}
	owner.Language = "made-up"
	if resp := s.debugAction(context.Background(), owner); resp.OK || resp.ErrorCode != "unsupported_debug_language" {
		t.Fatalf("unsupported debug adapter reported ready: %+v", resp)
	}
	owner.Language = ""
	resp = s.debugAction(context.Background(), owner)
	if !resp.OK || !strings.Contains(resp.Output, `"adapter":"go/delve"`) {
		t.Fatalf("default Go/Delve adapter readiness regressed: %+v", resp)
	}
}

func TestPythonDAPNoUncontainedWorkerTargetLaunch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("must be tested as unprivileged aiworker")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go builder unavailable for actual versioned worker helper")
	}
	helper := filepath.Join(t.TempDir(), "agent-workspace-helper")
	builder := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	builder.Dir = filepath.Join("..", "..")
	if out, err := builder.CombinedOutput(); err != nil {
		t.Fatalf("version worker helper: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	path := filepath.Join(workspace, "main.py")
	if err := os.WriteFile(path, []byte("value=7\nprint(value)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "main.py"})
	if !metadata.OK {
		t.Fatalf("source metadata failed: %+v", metadata)
	}
	owner.Action = "debug_launch"
	owner.Language = "python"
	owner.DebugMode = "script"
	owner.DebugProgram = "main.py"
	owner.FileVersion = metadata.FileVersion
	result := s.debugAction(context.Background(), owner)
	if result.OK || result.ErrorCode != "dap_containment_unavailable" || result.SessionID != "" {
		t.Fatalf("unprivileged Python debug target would have launched: %+v", result)
	}
	s.sessions.mu.Lock()
	n := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if n != 0 {
		t.Fatalf("uncontained Python debug left %d sessions", n)
	}
}
