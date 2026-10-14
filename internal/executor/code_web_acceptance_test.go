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

// Uses actual HTML/CSS LSP servers with immutable, script-disabled npm
// modules. The built-in integration path is tested; Browser/Playwright
// continues to own runtime visual assertions, not this passive LSP.
func TestCodeInspectRealHTMLCSSLanguageServers(t *testing.T) {
	node := os.Getenv("AGENT_WEB_LSP_PROOF_NODE")
	htmlCLI := os.Getenv("AGENT_WEB_LSP_PROOF_HTML")
	cssCLI := os.Getenv("AGENT_WEB_LSP_PROOF_CSS")
	if node == "" && htmlCLI == "" && cssCLI == "" {
		t.Skip("set AGENT_WEB_LSP_PROOF_* to exercise real HTML/CSS language servers")
	}
	if node == "" || htmlCLI == "" || cssCLI == "" {
		t.Fatal("partial HTML/CSS LSP fixture cannot prove acceptance")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.webNodeBinary = node
	s.webHTMLCLIBinary = htmlCLI
	s.webCSSCLIBinary = cssCLI
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-web-file-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker-only helper: %v %s", err, output)
	}
	s.workspaceHelperBinary = helper
	sources := map[string]string{
		"index.html":   "<!doctype html><html><body><main><h1>Title</h1><div id=\"hero\">Hi</div></main></body></html>",
		"styles.css":   "main { color: green; margin: 0; }",
		"package.json": `{"scripts":{"prepare":"touch WEB-MUST-NOT-EXECUTE","postinstall":"touch WEB-MUST-NOT-EXECUTE"}}`,
	}
	for file, body := range sources {
		if err := os.WriteFile(filepath.Join(workspace, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(path string) Response {
		t.Helper()
		r := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: path})
		if !r.OK || r.FileVersion == "" {
			t.Fatalf("worker stat %s: %+v", path, r)
		}
		return r
	}
	owner.Action = "code_inspect"
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	check := func(path, lang, method string) Response {
		t.Helper()
		owner.Path, owner.FileVersion, owner.Language, owner.CodeMethod = path, stat(path).FileVersion, lang, method
		r := s.codeInspect(ctx, owner)
		if !r.OK || r.FileVersion != owner.FileVersion {
			t.Fatalf("%s/%s semantic analysis error: %+v", lang, method, r)
		}
		return r
	}
	htmlSymbols := check("index.html", "html", "symbols")
	if !strings.Contains(htmlSymbols.Output, "main") || !strings.Contains(htmlSymbols.Output, "h1") {
		t.Fatalf("HTML syntax-aware structural symbols not proven: %s", htmlSymbols.Output)
	}
	cssSymbols := check("styles.css", "css", "symbols")
	if !strings.Contains(cssSymbols.Output, "main") {
		t.Fatalf("CSS selector symbols not proven: %s", cssSymbols.Output)
	}
	badCSS := "main { coloor: blue; color: ; background: #xxxxxx; }"
	if err := os.WriteFile(filepath.Join(workspace, "styles.css"), []byte(badCSS), 0600); err != nil {
		t.Fatal(err)
	}
	diag := check("styles.css", "css", "diagnostics")
	if !strings.Contains(diag.Output, "css-propertyvalueexpected") ||
		!strings.Contains(diag.Output, "unknownProperties") {
		t.Fatalf("real CSS parser/lint diagnostics not observed: %s", diag.Output)
	}
	// The HTML push mode can report no errors for some malformed markup:
	// a positive publication proves only the server's actual assessment,
	// not that HTML syntax/compliance or Browser behavior is fully checked.
	if out := check("index.html", "html", "diagnostics"); !strings.Contains(out.Output, `"kind":"full"`) {
		t.Fatalf("HTML push diagnostics were not correlated: %s", out.Output)
	}
	owner.Path, owner.FileVersion, owner.Language, owner.CodeMethod = "styles.css", "v1:stale-snapshot", "css", "symbols"
	if stale := s.codeInspect(ctx, owner); stale.OK || stale.ErrorCode != "file_changed" {
		t.Fatalf("stale CSS file version accepted: %+v", stale)
	}
	if _, err := os.Stat(filepath.Join(workspace, "WEB-MUST-NOT-EXECUTE")); !os.IsNotExist(err) {
		t.Fatalf("Node language server executed untrusted package scripts: %v", err)
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("HTML/CSS left %d retained LSP sessions", active)
	}
}
