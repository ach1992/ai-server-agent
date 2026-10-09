package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func workspaceSearchTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	return &Server{
		cfg:                    config.Config{WorkspaceDir: root},
		workerUID:              uint32(os.Geteuid()),
		workerGID:              uint32(os.Getegid()),
		runs:                   newRunLimiterWith(2, 1),
		structuralSearchBinary: filepath.Join(root, "fake-ast-grep"),
	}, root
}

func writeFakeAstGrep(t *testing.T, path, body string) {
	t.Helper()
	script := "#!/bin/sh\nset -eu\nif [ \"${1:-}\" = \"--version\" ]; then printf 'ast-grep 0.45.3\\n'; exit 0; fi\n" + body
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

func astGrepFixtureJSON(t *testing.T, file, text string, line, column int, captureName, captureText string) string {
	t.Helper()
	raw := map[string]any{
		"text": text,
		"range": map[string]any{
			"byteOffset": map[string]any{"start": 10, "end": 10 + len(text)},
			"start":      map[string]any{"line": line, "column": column},
			"end":        map[string]any{"line": line, "column": column + len(text)},
		},
		"file":     file,
		"language": "Go",
		"metaVariables": map[string]any{
			"single": map[string]any{
				captureName: map[string]any{
					"text": captureText,
					"range": map[string]any{
						"byteOffset": map[string]any{"start": 12, "end": 12 + len(captureText)},
						"start":      map[string]any{"line": line, "column": column + 2},
						"end":        map[string]any{"line": line, "column": column + 2 + len(captureText)},
					},
				},
			},
			"multi":       map[string]any{},
			"transformed": map[string]any{},
		},
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWorkspaceSearchStructuralResultAndSafetyFlags(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "sample.go")
	if err := os.WriteFile(file, []byte("package sample\nfunc caller() { target(42) }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	match := astGrepFixtureJSON(t, file, "target(42)", 1, 16, "ARG", "42")
	body := fmt.Sprintf(`
[ "$(pwd)" = "/" ] || { echo bad-cwd >&2; exit 9; }
case " $* " in
  *" --rewrite "*|*" --update-all "*|*" --interactive "*|*" --follow "*) echo forbidden-flag >&2; exit 9;;
esac
case " $* " in *" --config /dev/null "*) :;; *) echo missing-safe-config >&2; exit 9;; esac
printf '%%s\n' %s
`, shellQuote(match))
	writeFakeAstGrep(t, s.structuralSearchBinary, body)

	resp := s.workspaceSearchContext(context.Background(), Request{
		SearchMode:  "structural",
		Workspace:   workspace,
		Language:    "go",
		Pattern:     "target($ARG)",
		SearchLimit: 10,
	})
	if !resp.OK {
		t.Fatalf("search failed: %+v", resp)
	}
	var result WorkspaceSearchResult
	if err := json.Unmarshal([]byte(resp.Output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.Truncated || result.Engine != "ast-grep" || result.EngineVersion != "0.45.3" || len(result.Matches) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	got := result.Matches[0]
	if got.File != "sample.go" || got.Text != "target(42)" || got.Range.Start.Line != 1 || got.Range.Start.Column != 16 {
		t.Fatalf("unexpected match: %+v", got)
	}
	if len(got.Captures) != 1 || got.Captures[0].Name != "ARG" || got.Captures[0].Text != "42" {
		t.Fatalf("unexpected captures: %+v", got.Captures)
	}
}

func TestWorkspaceSearchNoMatchExitOneIsComplete(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	writeFakeAstGrep(t, s.structuralSearchBinary, "exit 1\n")
	resp := s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "missing($A)"})
	if !resp.OK {
		t.Fatalf("no-match should be a successful complete search: %+v", resp)
	}
	var result WorkspaceSearchResult
	if err := json.Unmarshal([]byte(resp.Output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.Truncated || len(result.Matches) != 0 {
		t.Fatalf("unexpected no-match result: %+v", result)
	}
}

func TestWorkspaceSearchResultLimitStopsProducer(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "sample.go")
	if err := os.WriteFile(file, []byte("package sample\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for i := 0; i < 5; i++ {
		body.WriteString("printf '%s\\n' ")
		body.WriteString(shellQuote(astGrepFixtureJSON(t, file, fmt.Sprintf("target(%d)", i), i, 0, "ARG", fmt.Sprintf("%d", i))))
		body.WriteString("\n")
	}
	body.WriteString("sleep 5\n")
	writeFakeAstGrep(t, s.structuralSearchBinary, body.String())

	started := time.Now()
	resp := s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "target($A)", SearchLimit: 2, TimeoutMS: 5000})
	if !resp.OK {
		t.Fatalf("limited search failed: %+v", resp)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("result-limit cancellation was too slow: %v", elapsed)
	}
	var result WorkspaceSearchResult
	if err := json.Unmarshal([]byte(resp.Output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Complete || !result.Truncated || result.TruncationReason != "result_limit" || len(result.Matches) != 2 {
		t.Fatalf("unexpected limited result: %+v", result)
	}
}

func TestWorkspaceSearchTimeoutReturnsExplicitPartialState(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	writeFakeAstGrep(t, s.structuralSearchBinary, "sleep 5\n")
	resp := s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "target($A)", TimeoutMS: 50})
	if !resp.OK {
		t.Fatalf("timeout should return explicit partial state: %+v", resp)
	}
	var result WorkspaceSearchResult
	if err := json.Unmarshal([]byte(resp.Output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Complete || !result.Truncated || !result.TimedOut || result.TruncationReason != "timeout" {
		t.Fatalf("unexpected timeout result: %+v", result)
	}
}

func TestWorkspaceSearchRejectsWorkspaceAndPathEscape(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	writeFakeAstGrep(t, s.structuralSearchBinary, "exit 1\n")
	outside := t.TempDir()
	resp := s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: outside, Language: "go", Pattern: "x"})
	if resp.OK || resp.ErrorCode != "invalid_workspace" {
		t.Fatalf("outside workspace accepted: %+v", resp)
	}
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	resp = s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "x", SearchPaths: []string{"escape"}})
	if resp.OK || resp.ErrorCode != "invalid_search_path" {
		t.Fatalf("symlink path escape accepted: %+v", resp)
	}
}

func TestWorkspaceSearchValidatesInputsAndMissingEngine(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	cases := []Request{
		{SearchMode: "text", Workspace: workspace, Language: "go", Pattern: "x"},
		{SearchMode: "structural", Workspace: workspace, Language: "", Pattern: "x"},
		{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: ""},
		{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "x", SearchLimit: maxWorkspaceSearchLimit + 1},
		{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "x", TimeoutMS: -1},
		{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "x", SearchPaths: []string{"../escape"}},
	}
	for i, req := range cases {
		resp := s.workspaceSearchContext(context.Background(), req)
		if resp.OK || resp.ErrorCode == "" {
			t.Fatalf("case %d unexpectedly accepted: %+v", i, resp)
		}
	}
	s.structuralSearchBinary = filepath.Join(root, "missing-ast-grep")
	resp := s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "x"})
	if resp.OK || resp.ErrorCode != "search_engine_unavailable" {
		t.Fatalf("missing engine must fail clearly: %+v", resp)
	}
}

func TestWorkspaceSearchRejectsEngineOutputOutsideWorkspace(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeFakeAstGrep(t, s.structuralSearchBinary, "printf '%s\\n' "+shellQuote(astGrepFixtureJSON(t, outside, "x", 0, 0, "A", "x"))+"\n")
	resp := s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "x"})
	if resp.OK || resp.ErrorCode != "search_output_invalid" {
		t.Fatalf("outside engine result accepted: %+v", resp)
	}
}

func TestWorkspaceSearchWithRealAstGrep(t *testing.T) {
	binary := os.Getenv("AST_GREP_ACCEPTANCE_BIN")
	if binary == "" {
		t.Skip("AST_GREP_ACCEPTANCE_BIN not set")
	}
	s, root := workspaceSearchTestServer(t)
	s.structuralSearchBinary = binary
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "sample.go")
	if err := os.WriteFile(file, []byte("package sample\nfunc caller() int { return oldName(2) }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sgconfig.yml"), []byte("this: [is: deliberately: invalid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	resp := s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "oldName($ARG)", SearchLimit: 10})
	if !resp.OK {
		t.Fatalf("real ast-grep search failed: %+v", resp)
	}
	var result WorkspaceSearchResult
	if err := json.Unmarshal([]byte(resp.Output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Matches) != 1 || result.Matches[0].File != "sample.go" || len(result.Matches[0].Captures) != 1 || result.Matches[0].Captures[0].Text != "2" {
		t.Fatalf("unexpected real ast-grep result: %+v", result)
	}
}

func TestAstGrepRewritePlanEvaluationDoesNotMutateSource(t *testing.T) {
	binary := os.Getenv("AST_GREP_ACCEPTANCE_BIN")
	if binary == "" {
		t.Skip("AST_GREP_ACCEPTANCE_BIN not set")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "sample.go")
	content := []byte("package sample\nfunc caller() int { return oldName(2) }\n")
	if err := os.WriteFile(file, content, 0644); err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(content)
	cmd := exec.Command(binary, "run", "--lang", "go", "--pattern", "oldName($ARG)", "--rewrite", "newName($ARG)", "--json=stream", file)
	cmd.Dir = "/"
	cmd.Env = []string{"HOME=/nonexistent", "XDG_CONFIG_HOME=/nonexistent", "XDG_CACHE_HOME=/nonexistent", "PATH=" + safeCommandPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "NO_COLOR=1"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rewrite plan command failed: %v", err)
	}
	if !strings.Contains(string(out), `"replacement":"newName(2)"`) {
		t.Fatalf("rewrite plan did not expose expected replacement: %s", out)
	}
	afterBytes, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	after := sha256.Sum256(afterBytes)
	if before != after {
		t.Fatalf("rewrite planning mutated source: before=%s after=%s", hex.EncodeToString(before[:]), hex.EncodeToString(after[:]))
	}
}

func TestAstGrepVersionCompatibility(t *testing.T) {
	for _, tc := range []struct {
		version string
		wantErr bool
	}{
		{"0.40.4", true},
		{"0.40.5", false},
		{"0.45.3", false},
		{"1.0.0", false},
		{"unknown", true},
	} {
		err := requireCompatibleAstGrepVersion(tc.version)
		if (err != nil) != tc.wantErr {
			t.Fatalf("version %q error=%v wantErr=%t", tc.version, err, tc.wantErr)
		}
	}
}

func TestWorkspaceSearchRejectsInvalidEngineRange(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "sample.go")
	if err := os.WriteFile(file, []byte("package sample\n"), 0644); err != nil {
		t.Fatal(err)
	}
	invalid := fmt.Sprintf(`{"text":"x","range":{"byteOffset":{"start":20,"end":10},"start":{"line":1,"column":2},"end":{"line":1,"column":1}},"file":%q,"language":"Go","metaVariables":{"single":{},"multi":{},"transformed":{}}}`, file)
	writeFakeAstGrep(t, s.structuralSearchBinary, "printf '%s\\n' "+shellQuote(invalid)+"\n")
	resp := s.workspaceSearchContext(context.Background(), Request{SearchMode: "structural", Workspace: workspace, Language: "go", Pattern: "x"})
	if resp.OK || resp.ErrorCode != "search_output_invalid" {
		t.Fatalf("invalid engine range accepted: %+v", resp)
	}
}

func TestTrustedWorkspaceSearchExecutableRejectsWorkerOwnedBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ast-grep")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if trustedWorkspaceSearchExecutable(path) {
		t.Fatal("worker-owned ast-grep binary was treated as trusted production tooling")
	}
}

func TestWorkspaceSearchAggregateOutputIsBounded(t *testing.T) {
	s, root := workspaceSearchTestServer(t)
	workspace := filepath.Join(root, "repo")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "sample.go")
	if err := os.WriteFile(file, []byte("package sample\n"), 0644); err != nil {
		t.Fatal(err)
	}
	largeText := strings.Repeat("x", maxWorkspaceSearchTextBytes+512)
	line := astGrepFixtureJSON(t, file, largeText, 0, 0, "A", "x")
	body := "i=0\nwhile [ $i -lt 300 ]; do printf '%s\\n' " + shellQuote(line) + "; i=$((i+1)); done\n"
	writeFakeAstGrep(t, s.structuralSearchBinary, body)
	resp := s.workspaceSearchContext(context.Background(), Request{
		SearchMode:  "structural",
		Workspace:   workspace,
		Language:    "go",
		Pattern:     "$A",
		SearchLimit: maxWorkspaceSearchLimit,
	})
	if !resp.OK {
		t.Fatalf("bounded flood search failed: %+v", resp)
	}
	var result WorkspaceSearchResult
	if err := json.Unmarshal([]byte(resp.Output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Complete || !result.Truncated || result.TruncationReason != "output_limit" || len(result.Matches) == 0 || len(result.Matches) >= 300 {
		t.Fatalf("aggregate output bound was not explicit: matches=%d result=%+v", len(result.Matches), result)
	}
	if len(resp.Output) > 2*maxWorkspaceSearchResultBytes {
		t.Fatalf("encoded response unexpectedly exceeded bounded envelope: %d", len(resp.Output))
	}
}
