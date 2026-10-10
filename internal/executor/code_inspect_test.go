package executor

import (
	"context"
	"errors"
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
	// A cross-file definition must carry a target version proven through
	// the worker-authority helper, not only a validated caller version.
	target := filepath.Join(workspace, "target.go")
	caller := filepath.Join(workspace, "caller.go")
	if err := os.WriteFile(target, []byte("package demo\nfunc TargetValue() int { return 42 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caller, []byte("package demo\nfunc Use() int { return TargetValue() }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	callerStat := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "caller.go"})
	targetStat := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "target.go"})
	if !callerStat.OK || !targetStat.OK {
		t.Fatalf("worker stat failed: caller=%+v target=%+v", callerStat, targetStat)
	}
	cross := owner
	cross.Path = "caller.go"
	cross.FileVersion = callerStat.FileVersion
	cross.CodeMethod = "definition"
	cross.Line = 1
	cross.Character = 26
	linked := s.codeInspect(context.Background(), cross)
	if !linked.OK || !strings.Contains(linked.Output, "\"uri\":\"target.go\"") || !strings.Contains(linked.Output, "\"file_version\":\""+targetStat.FileVersion+"\"") {
		t.Fatalf("cross-file target missing exact worker version: %+v", linked)
	}
	// A deterministic writer races the second, target-overlay-bound query.
	// The source caller.go is unchanged, so source-only validation cannot
	// detect this; the target identity must reject the stale result.
	s.codeTargetSnapshotHook = func() {
		if err := os.WriteFile(target, []byte("package demo\nfunc TargetValue() int { return 43 }\n"), 0600); err != nil {
			panic(err)
		}
	}
	changedTarget := s.codeInspect(context.Background(), cross)
	s.codeTargetSnapshotHook = nil
	if changedTarget.OK || changedTarget.ErrorCode != "code_target_changed" || changedTarget.ErrorClass != "conflict" {
		t.Fatalf("concurrently changed LSP target was accepted: %+v", changedTarget)
	}
	refreshedTarget := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: "target.go"})
	newResult := s.codeInspect(context.Background(), cross)
	if !newResult.OK || !strings.Contains(newResult.Output, "\"file_version\":\""+refreshedTarget.FileVersion+"\"") {
		t.Fatalf("stable target cannot be read against new exact version: %+v", newResult)
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

func TestCodeLocationVersionsRejectOutsideAndUnsafeTargets(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "project")
	raw := []byte(`[{"uri":"` + fileURI(filepath.Join(workspace, "pkg", "target.go")) + `","range":{"start":{"line":1,"character":2}}},{"uri":"` + fileURI("/etc/passwd") + `","range":{"start":{"line":1}}}]`)
	normalized, err := normalizeCodeLocations(workspace, raw)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := codeTargetPaths(normalized)
	if err != nil || len(paths) != 1 || paths[0] != filepath.Join("pkg", "target.go") {
		t.Fatalf("unsafe target collection: paths=%v err=%v", paths, err)
	}
	annotated, err := bindCodeLocationVersions(normalized, map[string]string{paths[0]: "v1:target-proof"})
	if err != nil || !strings.Contains(string(annotated), "v1:target-proof") || strings.Contains(string(annotated), "/etc/passwd") || !strings.Contains(string(annotated), `"external":true`) {
		t.Fatalf("path leak or unversioned target: %s (%v)", annotated, err)
	}
	if _, err := bindCodeLocationVersions(normalized, nil); !errors.Is(err, errCodeTargetUnstable) {
		t.Fatalf("missing target version unexpectedly accepted: %v", err)
	}
	if _, err := codeTargetPaths([]byte(`{"uri":"../escape.go"}`)); !errors.Is(err, errCodeTargetUnstable) {
		t.Fatalf("untrusted relative target unexpectedly accepted: %v", err)
	}
}
