package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/ach1992/ai-server-agent/internal/systemexec"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This opt-in test proves source-build -> bearer MCP -> Unix Executor ->
// sandboxed worker source reads -> trusted real gopls -> direct-client output.
// It uses only disposable local state and never touches installed services.
// A passing SDK test is not proof of a particular ChatGPT UI's presentation.
func TestDirectMCPGoSemanticClientAcceptance(t *testing.T) {
	if testing.Short() || os.Getenv("ASA_MCP_GOPLS_ACCEPTANCE") != "1" {
		t.Skip("opt in to real direct MCP/gopls acceptance with ASA_MCP_GOPLS_ACCEPTANCE=1")
	}
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("requires an unprivileged Linux worker with Landlock, not root")
	}
	// Deliberately use the same trusted path and optional dependency as
	// production; do not weaken binary trust for the test.
	if systemexec.First("/usr/local/bin/gopls", "/usr/bin/gopls") == "" {
		t.Skip("trusted root-owned optional gopls not found at either production path")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("required Go toolchain is absent: %v", err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "bearer")
	fixture := t.TempDir()
	cfg.WorkspaceDir = filepath.Join(fixture, "workspace")
	cfg.StateDir = filepath.Join(fixture, "state")
	cfg.LogDir = filepath.Join(fixture, "logs")
	cfg.ExecutorSocket = filepath.Join(fixture, "executor.sock")
	cfg.WorkerUser, cfg.AgentUser = current.Username, current.Username
	for _, dir := range []string{cfg.WorkspaceDir, cfg.StateDir, cfg.LogDir, filepath.Join(cfg.StateDir, "worker-home")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg.ExecutorToken), "audit-fingerprint.key"), []byte("v1:"+hex.EncodeToString(key)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(fixture, "config.json")
	if err := config.Save(cfgFile, cfg); err != nil {
		t.Fatal(err)
	}

	// A real compiled production executable contains the self-executing
	// workspace-helper. No fake Executor responses are used.
	agentBinary := filepath.Join(fixture, "ai-server-agent")
	build := exec.Command("go", "build", "-trimpath", "-o", agentBinary, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated Agent: %v: %s", err, output)
	}
	procCtx, stop := context.WithCancel(context.Background())
	process := exec.CommandContext(procCtx, agentBinary, "-config", cfgFile, "executor")
	var stderr bytes.Buffer
	process.Stdout, process.Stderr = &stderr, &stderr
	if err := process.Start(); err != nil {
		stop()
		t.Fatal(err)
	}
	terminated := make(chan error, 1)
	go func() { terminated <- process.Wait() }()
	t.Cleanup(func() {
		stop()
		select {
		case <-terminated:
		case <-time.After(5 * time.Second):
			t.Error("disposable Executor failed to stop")
		}
	})
	for until := time.Now().Add(8 * time.Second); ; {
		conn, err := net.DialTimeout("unix", cfg.ExecutorSocket, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		select {
		case exited := <-terminated:
			t.Fatalf("disposable Executor exited before readiness: %v; log=%q", exited, stderr.String())
		default:
		}
		if time.Now().After(until) {
			t.Fatalf("Executor was not ready: %v; log=%q", err, stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	agent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(agent.Handler())
	defer web.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	httpClient := web.Client()
	httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
	connect := func() *mcpsdk.ClientSession {
		t.Helper()
		client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "direct-gopls-acceptance", Version: "v1"}, nil)
		session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{
			Endpoint: web.URL + cfg.MCPPath, HTTPClient: httpClient,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	session := connect()
	defer func() { _ = session.Close() }()
	catalog, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range catalog.Tools {
		if strings.HasPrefix(tool.Name, "code_") {
			seen[tool.Name] = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint ||
				tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatalf("%s changed read-only public contract", tool.Name)
			}
		}
	}
	for _, name := range []string{"code_definition", "code_references", "code_symbols", "code_diagnostics"} {
		if !seen[name] {
			t.Fatalf("public MCP catalog does not expose %s", name)
		}
	}
	// Every tool must deliver the same structured metadata and sufficiently
	// small text fallback to support text-only clients.
	call := func(name string, args map[string]any) (executor.Response, string) {
		t.Helper()
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s transport: %v", name, err)
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var response executor.Response
		if err := json.Unmarshal(encoded, &response); err != nil {
			t.Fatalf("%s missing structured response: %v", name, err)
		}
		if result.IsError == response.OK {
			t.Fatalf("%s error hint disagrees with Executor result: %+v", name, response)
		}
		if len(result.Content) != 1 {
			t.Fatalf("%s text fallback item count = %d", name, len(result.Content))
		}
		text, ok := result.Content[0].(*mcpsdk.TextContent)
		if !ok {
			t.Fatalf("%s omitted text-only fallback: %T", name, result.Content[0])
		}
		if len(text.Text) > 32<<10 {
			t.Fatalf("%s unbounded fallback = %d bytes", name, len(text.Text))
		}
		return response, text.Text
	}

	// This realistic 53 KiB Go source exceeds the previous 32 KiB adapter
	// boundary and exercises the complete, untruncated didOpen document.
	source := "package demo\nfunc Hello() {}\nfunc main() { Hello() }\n" +
		strings.Repeat("// Representative Go source for a larger semantic document\n", 900)
	if len(source) <= 47_350 || len(source) >= 64<<10 {
		t.Fatalf("invalid large Go fixture length: %d", len(source))
	}
	for name, content := range map[string]string{
		"go.mod":    "module example.org/direct-gopls-acceptance\n\ngo 1.26.0\n",
		"main.go":   source,
		"target.go": "package demo\nfunc TargetValue() int { return 42 }\n",
		"caller.go": "package demo\nfunc Use() int { return TargetValue() }\n",
	} {
		if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(path string) executor.Response {
		t.Helper()
		value, _ := call("workspace_stat", map[string]any{"workspace": cfg.WorkspaceDir, "path": path})
		if !value.OK || value.FileVersion == "" {
			t.Fatalf("worker stat %s: %+v", path, value)
		}
		return value
	}
	mainVersion := stat("main.go").FileVersion
	if size := stat("main.go").FileSize; size == nil || *size != int64(len(source)) {
		t.Fatal("MCP worker metadata did not preserve exact source length")
	}
	code := func(method, path, version string, line, column int) executor.Response {
		t.Helper()
		args := map[string]any{"workspace": cfg.WorkspaceDir, "path": path,
			"file_version": version, "line": line, "character": column}
		result, fallback := call("code_"+method, args)
		if result.OK && (result.FileVersion != version || !json.Valid([]byte(result.Output))) {
			t.Fatalf("code_%s omitted version or returned invalid JSON: %+v", method, result)
		}
		if result.OK && len(result.Output) < 4000 {
			var decoded executor.Response
			if err := json.Unmarshal([]byte(fallback), &decoded); err != nil || decoded.FileVersion != version ||
				decoded.Output != result.Output {
				t.Fatalf("code_%s small text fallback missing semantic content: %v", method, err)
			}
		}
		return result
	}
	started := time.Now()
	definition := code("definition", "main.go", mainVersion, 2, 14)
	if !definition.OK || !strings.Contains(definition.Output, "\"line\":1") {
		t.Fatalf("real large-Go definition failed: %+v", definition)
	}
	references := code("references", "main.go", mainVersion, 2, 14)
	if !references.OK || !strings.Contains(references.Output, "main.go") {
		t.Fatalf("real gopls references missing: %+v", references)
	}
	symbols := code("symbols", "main.go", mainVersion, 2, 14)
	if !symbols.OK || !strings.Contains(symbols.Output, "Hello") {
		t.Fatalf("real gopls symbols missing: %+v", symbols)
	}
	diagnostics := code("diagnostics", "main.go", mainVersion, 2, 14)
	if !diagnostics.OK {
		t.Fatalf("real gopls diagnostics failed: %+v", diagnostics)
	}
	// The stateless public MCP transport can be replaced between calls; the
	// owner/version must still select the correct workspace and source.
	_ = session.Close()
	session = connect()

	callerVersion := stat("caller.go").FileVersion
	targetVersion := stat("target.go").FileVersion
	target := code("definition", "caller.go", callerVersion, 1, 26)
	if !target.OK || !strings.Contains(target.Output, "\"uri\":\"target.go\"") ||
		!strings.Contains(target.Output, "\"file_version\":\""+targetVersion+"\"") {
		t.Fatalf("cross-file semantic target has no proven worker version: %+v", target)
	}
	replacement := "package demo\nfunc TargetValue() int { return 43 }\n"
	changed, _ := call("workspace_write", map[string]any{
		"workspace": cfg.WorkspaceDir, "path": "target.go", "content": replacement,
		"file_version": targetVersion,
	})
	if !changed.OK {
		t.Fatalf("version-pinned worker edit failed: %+v", changed)
	}
	updatedVersion := stat("target.go").FileVersion
	if updatedVersion == targetVersion {
		t.Fatal("target version did not advance")
	}
	refreshed := code("definition", "caller.go", callerVersion, 1, 26)
	if !refreshed.OK || !strings.Contains(refreshed.Output, "\"file_version\":\""+updatedVersion+"\"") {
		t.Fatalf("updated cross-file version absent: %+v", refreshed)
	}
	// The source itself must fail closed on a stale client-supplied version.
	revised := "package demo\nfunc Hello() {}\nfunc main() { neverDeclaredSymbol(); Hello() }\n"
	replaced, _ := call("workspace_write", map[string]any{
		"workspace": cfg.WorkspaceDir, "path": "main.go",
		"file_version": mainVersion, "content": revised,
	})
	if !replaced.OK {
		t.Fatalf("worker source edit rejected: %+v", replaced)
	}
	stale := code("definition", "main.go", mainVersion, 2, 37)
	if stale.OK || stale.ErrorCode != "file_changed" {
		t.Fatalf("stale source semantic query was accepted: %+v", stale)
	}
	newVersion := stat("main.go").FileVersion
	if newVersion == mainVersion {
		t.Fatal("source version did not advance")
	}
	broken := code("diagnostics", "main.go", newVersion, 2, 14)
	if !broken.OK || !strings.Contains(broken.Output, "neverDeclaredSymbol") {
		t.Fatalf("real gopls failed to diagnose updated broken source: %+v", broken)
	}
	outside := code("definition", "../invalid.go", newVersion, 0, 0)
	if outside.OK || outside.ErrorCode != "invalid_code_path" {
		t.Fatalf("out-of-workspace source path was accepted: %+v", outside)
	}
	sha := sha256.Sum256([]byte(source))
	t.Logf("DIRECT_REAL_GOPLS_PASS source_bytes=%d sha256=%s methods=4 cross_file_versions=2 stale_rejected=true elapsed=%s",
		len(source), hex.EncodeToString(sha[:]), time.Since(started))
}
