package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

// Exercise the complete public Executor route using real pinned JavaScript
// language-server modules, a real Node runtime and real ShellCheck, without
// installation or mutation of the resident Agent control plane.
func TestCodeInspectRealShellLanguageServer(t *testing.T) {
	node := os.Getenv("AGENT_BASH_LSP_PROOF_NODE")
	cli := os.Getenv("AGENT_BASH_LSP_PROOF_CLI")
	shellcheck := os.Getenv("AGENT_BASH_LSP_PROOF_SHELLCHECK")
	if node == "" && cli == "" && shellcheck == "" {
		t.Skip("real Shell LSP proof requires AGENT_BASH_LSP_PROOF_* fixtures")
	}
	if node == "" || cli == "" || shellcheck == "" {
		t.Fatal("partial Shell LSP fixture must never count as acceptance")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.bashNodeBinary = node
	s.bashCLIBinary = cli
	s.bashShellcheckBinary = shellcheck
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-shell-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated worker helper: %v %s", err, output)
	}
	s.workspaceHelperBinary = helper
	source := "#!/usr/bin/env bash\nsay_hello() {\n  local name=\"$1\"\n  printf 'Hello %s\\n' \"$name\"\n}\nsay_hello ACh\n"
	valid := filepath.Join(workspace, "install.bash")
	for path, value := range map[string]string{
		"install.bash": source,
		"package.json": `{"scripts":{"prepare":"touch DONT-RUN-PACKAGE","postinstall":"touch DONT-RUN-PACKAGE"}}`,
		".bashrc":      "touch DONT-RUN-BASHRC\n",
		"malicious.sh": "#!/bin/sh\ntouch DONT-RUN-SHELL-SOURCE\n",
	} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(path string) Response {
		t.Helper()
		got := s.workerWorkspaceFile(context.Background(), Request{
			Action: "workspace_stat", Workspace: workspace, Path: path,
		})
		if !got.OK || got.FileVersion == "" {
			t.Fatalf("worker stat %q: %+v", path, got)
		}
		return got
	}
	original := stat("install.bash")
	owner.Action, owner.Workspace, owner.Path, owner.FileVersion = "code_inspect", workspace, "install.bash", original.FileVersion
	owner.Language = "shell"
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	run := func(method string, line, char int) Response {
		t.Helper()
		owner.CodeMethod, owner.Line, owner.Character = method, line, char
		result := s.codeInspect(ctx, owner)
		if !result.OK || result.FileVersion != owner.FileVersion {
			t.Fatalf("real Bash %s failed: %+v", method, result)
		}
		return result
	}
	if value := run("symbols", 0, 0); !strings.Contains(value.Output, "say_hello") {
		t.Fatalf("real Bash symbol absent: %s", value.Output)
	}
	if value := run("definition", 5, 4); !strings.Contains(value.Output, `"uri":"install.bash"`) ||
		!strings.Contains(value.Output, original.FileVersion) {
		t.Fatalf("real Bash function definition not source-pinned: %s", value.Output)
	}
	if value := run("references", 5, 4); !strings.Contains(value.Output, "file_version") {
		t.Fatalf("real Bash function references absent: %s", value.Output)
	}
	broken := "#!/bin/sh\nif [ -f /tmp/test ]; then\necho one\n"
	if err := os.WriteFile(valid, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	owner.FileVersion = stat("install.bash").FileVersion
	if value := run("diagnostics", 1, 0); !strings.Contains(value.Output, "SC1046") {
		t.Fatalf("real ShellCheck incomplete-if error missing, do not invent clean result: %s", value.Output)
	}
	owner.FileVersion = original.FileVersion
	if result := s.codeInspect(ctx, owner); result.OK || result.ErrorCode != "file_changed" {
		t.Fatalf("stale Shell semantic snapshot accepted: %+v", result)
	}
	for _, name := range []string{"DONT-RUN-PACKAGE", "DONT-RUN-BASHRC", "DONT-RUN-SHELL-SOURCE"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
			t.Fatalf("Shell LSP executed project-defined script %q: %v", name, err)
		}
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("Shell LSP left %d sessions", active)
	}
}

func TestShellLSPUnsupportedWithoutTrustedResources(t *testing.T) {
	s, req, workspace := testStdioBroker(t)
	req.Action, req.CodeMethod, req.Path, req.Language = "code_inspect", "symbols", "a.sh", "shell"
	req.FileVersion = "v1:test"
	if err := os.WriteFile(filepath.Join(workspace, "a.sh"), []byte("#!/bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	req.Root = true
	if r := s.codeInspect(context.Background(), req); r.OK || r.ErrorCode != "code_requires_worker_version" {
		t.Fatalf("Root Shell read authorized: %+v", r)
	}
	req.Root = false
	req.Language = "python"
	if r := s.codeInspect(context.Background(), req); r.OK || r.ErrorCode != "code_language_mismatch" {
		t.Fatalf("path mismatch accepted: %+v", r)
	}
	req.Language = "shell"
	req.Path = "../outside.sh"
	if r := s.codeInspect(context.Background(), req); r.OK || r.ErrorCode != "invalid_code_path" {
		t.Fatalf("path escape accepted: %+v", r)
	}
	// Root-only selection cannot be overridden by a worker directory.
	if selected, err := systemexec.BashLSP(); err == nil && selected == filepath.Join(workspace, "node") {
		t.Fatal("worker-controlled executable selected by Bash LSP")
	}
}
