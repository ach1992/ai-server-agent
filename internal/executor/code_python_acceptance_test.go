package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real Python ty semantic navigation and diagnostics through the same
// worker file-version, private Broker, public Executor code_inspect path.
// Only the unprivileged fixture binary is injected in this opt-in test.
func TestCodeInspectRealNativePythonTy(t *testing.T) {
	binary := os.Getenv("AGENT_TY_PROOF_BIN")
	if binary == "" {
		t.Skip("set AGENT_TY_PROOF_BIN to run actual native Python ty semantic proof")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.tyBinary = binary
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-python-workspace-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("worker helper build failed: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	if err := os.MkdirAll(filepath.Join(workspace, "tests"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".venv", "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	// Neither project metadata nor a project-local interpreter may run
	// as a side effect of semantic read-only source inspection.
	files := map[string]string{
		"pyproject.toml":        "[project]\nname = \"ty-probe\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n[tool.ty.environment]\npython = \".venv/bin/python\"\n",
		".venv/bin/python":      "#!/bin/sh\nprintf exploited > SHOULD-NOT-EXECUTE\nexit 42\n",
		"helpers.py":            "def multiply(value: int) -> int:\n    return value * 2\n",
		"main.py":               "from helpers import multiply\nanswer: int = multiply(3)\n",
		"tests/test_helpers.py": "from helpers import multiply\n\ndef test_multiply():\n    assert multiply(2) == 4\n",
	}
	for name, value := range files {
		path := filepath.Join(workspace, name)
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(workspace, ".venv/bin/python"), 0700); err != nil {
		t.Fatal(err)
	}
	stat := func(name string) Response {
		t.Helper()
		got := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: name})
		if !got.OK || got.FileVersion == "" {
			t.Fatalf("safe source stat %s: %+v", name, got)
		}
		return got
	}
	firstVersion := stat("main.py")
	helperVersion := stat("helpers.py")
	owner.Action, owner.Path, owner.FileVersion = "code_inspect", "main.py", firstVersion.FileVersion
	owner.Language = "python"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	run := func(method string, line, char int) Response {
		t.Helper()
		owner.CodeMethod, owner.Line, owner.Character = method, line, char
		result := s.codeInspect(ctx, owner)
		if !result.OK || result.FileVersion != owner.FileVersion {
			t.Fatalf("python code_%s failed: %+v", method, result)
		}
		return result
	}
	if out := run("symbols", 0, 0); !strings.Contains(out.Output, "answer") {
		t.Fatalf("real Python document symbol unavailable: %s", out.Output)
	}
	if out := run("definition", 1, 17); !strings.Contains(out.Output, `"uri":"helpers.py"`) ||
		!strings.Contains(out.Output, helperVersion.FileVersion) {
		t.Fatalf("import target not versioned or missing: %s", out.Output)
	}
	if out := run("references", 1, 17); !strings.Contains(out.Output, "file_version") {
		t.Fatalf("real Python references unavailable: %s", out.Output)
	}
	owner.Path, owner.FileVersion = "tests/test_helpers.py", stat("tests/test_helpers.py").FileVersion
	if out := run("symbols", 2, 2); !strings.Contains(out.Output, "test_multiply") {
		t.Fatalf("Python test module structure unproven: %s", out.Output)
	}
	owner.Path = "main.py"
	bad := "from helpers import multiply\nanswer: int = \"not-an-int\"\n"
	if err := os.WriteFile(filepath.Join(workspace, "main.py"), []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	owner.FileVersion = stat("main.py").FileVersion
	if out := run("diagnostics", 1, 6); !strings.Contains(out.Output, "not assignable") {
		t.Fatalf("real Python type diagnostic unavailable: %s", out.Output)
	}
	if _, err := os.Stat(filepath.Join(workspace, "SHOULD-NOT-EXECUTE")); !os.IsNotExist(err) {
		t.Fatalf("Python LSP implicitly executed project-local interpreter: %v", err)
	}
	owner.FileVersion = firstVersion.FileVersion
	if out := s.codeInspect(ctx, owner); out.OK || out.ErrorCode != "file_changed" {
		t.Fatalf("Python source version change not rejected: %+v", out)
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("Python LSP broker session leaked: %d", active)
	}
}
