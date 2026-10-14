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

func TestCodeInspectMissingAndWrongLanguageDoNotLaunch(t *testing.T) {
	s, owner, workspace := testStdioBroker(t)
	owner.Action = "code_inspect"
	owner.Path = "main.ts"
	owner.FileVersion = "v1:unused"
	owner.CodeMethod = "definition"
	owner.Language = "go"
	if got := s.codeInspect(context.Background(), owner); got.OK || got.ErrorCode != "code_language_mismatch" {
		t.Fatalf("wrong extension/language reached adapter: %+v", got)
	}
	owner.Language = ""
	owner.Path = "schema.sql"
	if got := s.codeInspect(context.Background(), owner); got.OK || got.ErrorCode != "unsupported_code_language" {
		t.Fatalf("unimplemented adapter accepted: %+v", got)
	}
	owner.Path = "../../secrets.ts"
	if got := s.codeInspect(context.Background(), owner); got.OK || got.ErrorCode != "invalid_code_path" {
		t.Fatalf("path escape accepted: %+v", got)
	}
	owner.Path = "main.ts"
	owner.Workspace = ""
	if got := s.codeInspect(context.Background(), owner); got.OK || got.ErrorCode != "code_requires_worker_version" {
		t.Fatalf("missing workspace authorized: %+v", got)
	}
	owner.Workspace = workspace
	owner.Root = true
	if got := s.codeInspect(context.Background(), owner); got.OK || got.ErrorCode != "code_requires_worker_version" {
		t.Fatalf("root code inspection accepted: %+v", got)
	}
}

func TestCodeInspectRealNativeTypeScript7PublicAction(t *testing.T) {
	binary := os.Getenv("AGENT_TS7_PROOF_BIN")
	if binary == "" {
		t.Skip("real bounded public TS7 code_inspect test requires AGENT_TS7_PROOF_BIN")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.ts7Binary = binary // fixture; production never reads this from MCP requests
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-code-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker workspace helper: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	files := map[string]string{
		"tsconfig.json":     `{"compilerOptions":{"module":"nodenext","moduleResolution":"nodenext","strict":true,"plugins":[{"name":"./untrusted-hook.js"}]},"include":["*.ts"]}`,
		"package.json":      `{"name":"untrusted-project","scripts":{"prepare":"touch DONT-RUN-SCRIPTS","postinstall":"touch DONT-RUN-SCRIPTS"}}`,
		"untrusted-hook.js": `require('node:fs').writeFileSync('DONT-RUN-SCRIPTS','unsafe')`,
		"helper.ts":         "export function identity(value: number): number { return value }\n",
		"main.ts":           "import { identity } from './helper.js'\nexport const answer = identity(42)\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(path string) Response {
		t.Helper()
		result := s.workerWorkspaceFile(context.Background(), Request{
			Action: "workspace_stat", Workspace: workspace, Path: path,
		})
		if !result.OK || result.FileVersion == "" {
			t.Fatalf("worker stat %s: %+v", path, result)
		}
		return result
	}
	original := stat("main.ts")
	target := stat("helper.ts")
	owner.Path, owner.FileVersion = "main.ts", original.FileVersion
	owner.Action, owner.Language = "code_inspect", "typescript"
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	run := func(method string, line, char int) Response {
		t.Helper()
		owner.CodeMethod, owner.Line, owner.Character = method, line, char
		out := s.codeInspect(ctx, owner)
		if !out.OK || out.FileVersion != owner.FileVersion {
			t.Fatalf("source code_%s version/payload failure: %+v", method, out)
		}
		return out
	}
	if out := run("symbols", 0, 0); !strings.Contains(out.Output, "answer") {
		t.Fatalf("typescript symbols missing: %s", out.Output)
	}
	if out := run("definition", 1, 24); !strings.Contains(out.Output, `"uri":"helper.ts"`) ||
		!strings.Contains(out.Output, target.FileVersion) {
		t.Fatalf("cross-file ESM TypeScript target/version missing: %s", out.Output)
	}
	if out := run("references", 1, 24); !strings.Contains(out.Output, "file_version") {
		t.Fatalf("typescript references unavailable: %s", out.Output)
	}
	broken := "import { identity } from './helper.js'\nconst n: number = \"wrong\"\n"
	if err := os.WriteFile(filepath.Join(workspace, owner.Path), []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	owner.FileVersion = stat(owner.Path).FileVersion
	if out := run("diagnostics", 1, 6); !strings.Contains(out.Output, "assignable to type") {
		t.Fatalf("TypeScript real invalid-source diagnostics missing: %s", out.Output)
	}
	if _, err := os.Stat(filepath.Join(workspace, "DONT-RUN-SCRIPTS")); !os.IsNotExist(err) {
		t.Fatalf("read-only LSP ran project scripts or plugin: %v", err)
	}
	owner.FileVersion = original.FileVersion
	owner.CodeMethod = "symbols"
	if out := s.codeInspect(ctx, owner); out.OK || out.ErrorCode != "file_changed" {
		t.Fatalf("stale source did not fail closed: %+v", out)
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("code inspection leaked %d sessions", active)
	}
}

// Two caller identities using the same worker HOME must not receive another
// worktree's symbols or opaque session ownership. This is logical broker
// isolation, not a claim of separate Unix tenants on a shared worker UID.
func TestCodeInspectNativeTS7WorktreeAndPrincipalIsolation(t *testing.T) {
	binary := os.Getenv("AGENT_TS7_PROOF_BIN")
	if binary == "" {
		t.Skip("real native TS7 isolation fixture not supplied")
	}
	requireWorkerLandlockV2(t)
	s, first, left := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.ts7Binary = binary
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-isolation-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker helper: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	right := filepath.Join(s.cfg.WorkspaceDir, "other-worktree")
	if err := os.Mkdir(right, 0700); err != nil {
		t.Fatal(err)
	}
	samples := []struct{ dir, name, principal string }{
		{left, "Alpha", "direct-alpha"},
		{right, "Beta", "direct-beta"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, sample := range samples {
		source := "export const " + sample.name + " = 42\n"
		if err := os.WriteFile(filepath.Join(sample.dir, "same.ts"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		stat := s.workerWorkspaceFile(ctx, Request{Action: "workspace_stat", Workspace: sample.dir, Path: "same.ts"})
		if !stat.OK || stat.FileVersion == "" {
			t.Fatalf("cannot stat %s: %+v", sample.dir, stat)
		}
		req := first
		req.Action, req.CodeMethod = "code_inspect", "symbols"
		req.PrincipalID = sample.principal
		req.Workspace, req.Path, req.FileVersion = sample.dir, "same.ts", stat.FileVersion
		result := s.codeInspect(ctx, req)
		if !result.OK || !strings.Contains(result.Output, sample.name) {
			t.Fatalf("workspace/principal lost own semantic symbol: %+v", result)
		}
		other := "Alpha"
		if sample.name == other {
			other = "Beta"
		}
		if strings.Contains(result.Output, other) || strings.Contains(result.Output, left) ||
			strings.Contains(result.Output, right) {
			t.Fatalf("cross-worktree semantic data leaked: %s", result.Output)
		}
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("cross-project LSP left %d broker sessions", active)
	}
}
