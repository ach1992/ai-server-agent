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

func TestCodeGoRealWorkerReadAndGopls(t *testing.T) {
	gopls := os.Getenv("AGENT_GOPLS_PROOF_BIN")
	if gopls == "" {
		t.Skip("optional real gopls fixture not provided")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	helper := filepath.Join(t.TempDir(), "ai-server-agent")
	build := exec.Command("/srv/ai-workspace/.toolchains/go1.27.1/bin/go", "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build file helper: %v: %s", err, b)
	}
	s.workspaceHelperBinary = helper
	source := "package demo\nfunc Hello() {}\nfunc main() { Hello() }\n"
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.org/real-gopls-public\n\ngo 1.26.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(workspace, "gopls-wrapper")
	script := "#!/bin/sh\nPATH=/srv/ai-workspace/.toolchains/go1.27.1/bin:$PATH\nexport PATH\nexec \"" + gopls + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.goplsBinary = wrapper
	stat := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "main.go"})
	if !stat.OK || stat.FileVersion == "" {
		t.Fatalf("worker safe stat: %+v", stat)
	}
	owner.Path = "main.go"
	owner.FileVersion = stat.FileVersion
	owner.Line = 2
	owner.Character = 14
	for _, method := range []string{"definition", "references", "symbols", "diagnostics"} {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		owner.CodeMethod = method
		out := s.codeInspect(ctx, owner)
		cancel()
		t.Logf("%s ok=%t code=%s output=%s", method, out.OK, out.ErrorCode, out.Output)
		if !out.OK {
			t.Fatalf("real gopls %s failed: %+v", method, out)
		}
		if out.FileVersion != stat.FileVersion {
			t.Fatalf("version not preserved: %+v", out)
		}
		if method == "definition" && !strings.Contains(out.Output, "\\\"line\\\":1") && !strings.Contains(out.Output, "\"line\":1") {
			t.Fatalf("incorrect definition: %+v", out)
		}
	}
	// A source error must be an actual diagnostic, not a quietly empty
	// "success" response or a copy of stale diagnostics from an earlier file.
	broken := "package demo\nfunc main() { neverDeclaredSymbol() }\n"
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	changed := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "main.go"})
	if !changed.OK || changed.FileVersion == stat.FileVersion {
		t.Fatalf("fixture version did not advance: %+v", changed)
	}
	diag := owner
	diag.CodeMethod = "diagnostics"
	diag.FileVersion = changed.FileVersion
	checked := s.codeInspect(context.Background(), diag)
	t.Logf("real diagnostics on invalid source: %+v", checked)
	if !checked.OK || !strings.Contains(checked.Output, "neverDeclaredSymbol") {
		t.Fatalf("real gopls diagnostics missing error: %+v", checked)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte(source+"// change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.CodeMethod = "definition"
	if out := s.codeInspect(context.Background(), owner); out.OK || out.ErrorCode != "file_changed" {
		t.Fatalf("stale file allowed: %+v", out)
	}
}
