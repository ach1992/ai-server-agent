package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLanguageNeutralProfileValidation(t *testing.T) {
	cases := map[string]string{
		"a.go": "", "a.ts": "typescript", "a.tsx": "typescriptreact",
		"a.js": "javascript", "a.jsx": "javascriptreact",
		"a.mts": "typescript", "a.cjs": "javascript",
		"a.php": "", "escape.sql": "",
	}
	for path, want := range cases {
		if got := typeScript7LSPProfile.languageForPath(path); got != want {
			t.Errorf("%s language %q, want %q", path, got, want)
		}
	}
	raw := json.RawMessage(`[{"uri":"a.ts","range":{"start":{"line":0}}},{"uri":"b.js","range":{"start":{"line":0}}}]`)
	targets, err := codeTargetPathsForProfile(raw, typeScript7LSPProfile)
	if err != nil || len(targets) != 2 || targets[0] != "a.ts" || targets[1] != "b.js" {
		t.Fatalf("unexpected JS/TS targets: %v / %v", targets, err)
	}
	if _, err := codeTargetPathsForProfile(json.RawMessage(`[{"uri":"worker.conf"}]`), typeScript7LSPProfile); err == nil {
		t.Fatal("unsupported file extension was accepted as semantic read authority")
	}
	if _, err := codeTargetPathsForProfile(json.RawMessage(`[{"uri":"../outside.ts"}]`), typeScript7LSPProfile); err == nil {
		t.Fatal("out-of-workspace location accepted")
	}
	for _, path := range []string{"main.py", "module.pyi"} {
		if profile, language, ok := inspectProfileForPath(path); !ok ||
			language != "python" || profile.languageForPath(path) != "python" {
			t.Fatalf("Python LSP profile selection failed for %q", path)
		}
	}
	for _, path := range []string{"deploy.sh", "deploy.bash"} {
		if profile, language, ok := inspectProfileForPath(path); !ok ||
			language != "shell" || profile.languageForPath(path) != "shellscript" {
			t.Fatalf("Shell LSP profile missing: %q", path)
		}
	}
	for _, tc := range []struct{ path, lang string }{{"layout.html", "html"}, {"layout.htm", "html"}, {"theme.css", "css"}} {
		if profile, language, ok := inspectProfileForPath(tc.path); !ok || language != tc.lang || profile.languageForPath(tc.path) != tc.lang {
			t.Fatalf("Web LSP profile selection failed for %q", tc.path)
		}
	}
	if profile, lang, ok := inspectProfileForPath("src/lib.rs"); !ok || lang != "rust" ||
		profile.languageForPath("src/lib.rs") != "rust" {
		t.Fatal("Rust language profile selection failed")
	}
	if _, _, ok := inspectProfileForPath("unsafe.pyc"); ok {
		t.Fatal("binary compiled Python bytecode should not be an LSP source")
	}
}

// Execute only when supplied a real, separately provisioned test-only TS 7
// native binary, not during ordinary CI and never through an untrusted
// repository binary in production. This proof tests the *Agent broker* and
// true TS7 LSP, not merely a standalone language-server CLI.
func TestWorkerNativeTypeScript7LSP(t *testing.T) {
	binary := os.Getenv("AGENT_TS7_PROOF_BIN")
	if binary == "" {
		t.Skip("set AGENT_TS7_PROOF_BIN for a real native TS7 adapter proof")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cannot build worker-only safe workspace helper: %v: %s", err, output)
	}
	s.workspaceHelperBinary = helper
	filename := "main.ts"
	program := filepath.Join(workspace, filename)
	valid := "export function add(v: number): number { return v+1 }\nconst x: number = add(41)\n"
	if err := os.WriteFile(program, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	stat := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: filename})
	if !stat.OK || stat.FileVersion == "" {
		t.Fatalf("source version unavailable: %+v", stat)
	}
	owner.Path, owner.FileVersion = filename, stat.FileVersion
	probe := func(method, content string, line, character int) json.RawMessage {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		result, err := s.workerLSPInspection(ctx, owner, typeScript7LSPProfile, binary, program, content, method, line, character)
		if err != nil {
			t.Fatalf("%s native TypeScript LSP: %v", method, err)
		}
		if !json.Valid(result) {
			t.Fatalf("invalid %s LSP response: %s", method, result)
		}
		return result
	}
	symbols := probe("symbols", valid, 0, 0)
	if !strings.Contains(string(symbols), "add") || !strings.Contains(string(symbols), "x") {
		t.Fatalf("native TS symbols missing: %s", symbols)
	}
	definition := probe("definition", valid, 1, 18)
	if !strings.Contains(string(definition), `"uri":"main.ts"`) ||
		!strings.Contains(string(definition), stat.FileVersion) {
		t.Fatalf("TS definition not version-pinned: %s", definition)
	}
	references := probe("references", valid, 1, 18)
	if !strings.Contains(string(references), "file_version") {
		t.Fatalf("TS references not version-pinned: %s", references)
	}
	broken := "export function add(v: number): number { return v+1 }\nconst x: number = \"wrong\"\n"
	if err := os.WriteFile(program, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	newStat := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: filename})
	if !newStat.OK || newStat.FileVersion == owner.FileVersion {
		t.Fatalf("TS source version failed to advance: %+v", newStat)
	}
	owner.FileVersion = newStat.FileVersion
	diagnostic := probe("diagnostics", broken, 1, 6)
	if !strings.Contains(string(diagnostic), "assignable to type") {
		t.Fatalf("native TS real diagnostic missing: %s", diagnostic)
	}
	s.sessions.mu.Lock()
	count := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if count != 0 {
		t.Fatalf("broker leaked %d LSP sessions", count)
	}
}

// The same native TypeScript adapter must also analyze genuine JavaScript,
// including an opt-in checked JS error rather than treating JS as syntax text.
func TestWorkerNativeJavaScript7LSP(t *testing.T) {
	binary := os.Getenv("AGENT_TS7_PROOF_BIN")
	if binary == "" {
		t.Skip("set AGENT_TS7_PROOF_BIN for real checked-JS LSP proof")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-js-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cannot build JS worker-safe workspace helper: %v: %s", err, output)
	}
	s.workspaceHelperBinary = helper
	filename := "main.js"
	source := "// @ts-check\nfunction sum(value) { return value + 1 }\nsum(unknownValue)\n"
	file := filepath.Join(workspace, filename)
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	stat := s.workerWorkspaceFile(context.Background(), Request{
		Action: "workspace_stat", Workspace: workspace, Path: filename,
	})
	if !stat.OK || stat.FileVersion == "" {
		t.Fatalf("checked-JS source version unavailable: %+v", stat)
	}
	owner.Path, owner.FileVersion = filename, stat.FileVersion
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	symbols, err := s.workerLSPInspection(ctx, owner, typeScript7LSPProfile, binary, file, source, "symbols", 0, 0)
	if err != nil || !strings.Contains(string(symbols), "sum") {
		t.Fatalf("native JS semantic symbols unavailable: %s / %v", symbols, err)
	}
	results, err := s.workerLSPInspection(ctx, owner, typeScript7LSPProfile, binary, file, source, "diagnostics", 2, 4)
	if err != nil || !strings.Contains(string(results), "unknownValue") {
		t.Fatalf("native JS semantic error unproven: %s / %v", results, err)
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("JS LSP subprocess leaked %d sessions", active)
	}
}
