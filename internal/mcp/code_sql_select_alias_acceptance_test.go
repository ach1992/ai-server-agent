package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
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

// This acceptance goes through a real bearer-authenticated direct MCP client,
// isolated Agent -> Unix Executor -> worker Landlock source search, without a
// live DB, privileged host changes or an optional SQL language-server binary.
// It proves public structured/fallback delivery on the source build, NOT an
// already-installed Stable or a specific ChatGPT UI rendering.
func TestDirectMCPSQLSelectAliasClientAcceptance(t *testing.T) {
	if os.Getenv("ASA_MCP_SQL_ALIAS_ACCEPTANCE") != "1" || testing.Short() {
		t.Skip("requires explicitly selected direct SQL alias MCP acceptance")
	}
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("requires unprivileged Linux worker")
	}
	if !systemexec.Trusted("/usr/bin/rg") {
		t.Skip("requires trusted root-owned ripgrep")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "bearer")
	private := t.TempDir()
	cfg.WorkspaceDir = filepath.Join(private, "workspace")
	cfg.StateDir = filepath.Join(private, "state")
	cfg.LogDir = filepath.Join(private, "logs")
	cfg.ExecutorSocket = filepath.Join(private, "executor.sock")
	cfg.WorkerUser, cfg.AgentUser = current.Username, current.Username
	for _, path := range []string{cfg.WorkspaceDir, cfg.StateDir, cfg.LogDir, filepath.Join(cfg.StateDir, "worker-home")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg.ExecutorToken), "audit-fingerprint.key"),
		[]byte("v1:"+hex.EncodeToString(key)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(private, "config.json")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	sourceBinary := filepath.Join(private, "ai-server-agent")
	build := exec.Command("go", "build", "-trimpath", "-o", sourceBinary, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	procCtx, stop := context.WithCancel(context.Background())
	executorCmd := exec.CommandContext(procCtx, sourceBinary, "-config", configPath, "executor")
	var logs bytes.Buffer
	executorCmd.Stdout, executorCmd.Stderr = &logs, &logs
	if err := executorCmd.Start(); err != nil {
		stop()
		t.Fatal(err)
	}
	terminated := make(chan error, 1)
	go func() { terminated <- executorCmd.Wait() }()
	t.Cleanup(func() {
		stop()
		select {
		case <-terminated:
		case <-time.After(5 * time.Second):
			t.Error("isolated Executor failed to stop")
		}
	})
	ready := false
	for until := time.Now().Add(8 * time.Second); time.Now().Before(until); {
		conn, err := net.DialTimeout("unix", cfg.ExecutorSocket, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("isolated Executor unavailable: %s", logs.String())
	}
	agent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(agent.Handler())
	defer web.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	httpClient := web.Client()
	httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "direct-sql-alias-acceptance", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{
		Endpoint: web.URL + cfg.MCPPath, HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	call := func(name string, args map[string]any) executor.Response {
		t.Helper()
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s MCP call: %v", name, err)
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var response executor.Response
		if err := json.Unmarshal(encoded, &response); err != nil {
			t.Fatalf("%s structured: %v", name, err)
		}
		if result.IsError == response.OK {
			t.Fatalf("%s error disagreement: %+v", name, response)
		}
		if len(result.Content) != 1 {
			t.Fatalf("%s missing text fallback", name)
		}
		fallback, ok := result.Content[0].(*mcpsdk.TextContent)
		if !ok || len(fallback.Text) > 32<<10 {
			t.Fatalf("%s invalid bounded fallback", name)
		}
		if response.OK {
			var decoded executor.Response
			if err := json.Unmarshal([]byte(fallback.Text), &decoded); err != nil || decoded.Output != response.Output || decoded.FileVersion != response.FileVersion {
				t.Fatalf("%s fallback differs from structured result: %v", name, err)
			}
		}
		return response
	}
	files := map[string]string{
		"migrations/001_accounts.sql": "CREATE TABLE public.accounts (id BIGINT PRIMARY KEY, name TEXT);\n",
		"migrations/002_payments.sql": "CREATE TABLE public.payments (id BIGINT PRIMARY KEY, account_id BIGINT REFERENCES public.accounts(id));\n",
		"queries/list.sql":            "SELECT a.id, p.account_id FROM public.accounts AS a JOIN public.payments p ON p.account_id = a.id WHERE a.id > 0;\n",
	}
	for name, source := range files {
		path := filepath.Join(cfg.WorkspaceDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(file string) string {
		t.Helper()
		result := call("workspace_stat", map[string]any{"workspace": cfg.WorkspaceDir, "path": file})
		if !result.OK || result.FileVersion == "" {
			t.Fatalf("stat %s: %+v", file, result)
		}
		return result.FileVersion
	}
	sourceVersion := stat("queries/list.sql")
	accountVersion, paymentVersion := stat("migrations/001_accounts.sql"), stat("migrations/002_payments.sql")
	query := func(method string, character int, sourceVersion string) executor.Response {
		return call("code_"+method, map[string]any{"workspace": cfg.WorkspaceDir,
			"path": "queries/list.sql", "file_version": sourceVersion, "language": "sql",
			"sql_dialect": "postgres", "line": 0, "character": character})
	}
	first := strings.Index(files["queries/list.sql"], "a.id") + 2
	definition := query("definition", first, sourceVersion)
	var locs []struct {
		URI         string `json:"uri"`
		FileVersion string `json:"file_version"`
	}
	if !definition.OK || json.Unmarshal([]byte(definition.Output), &locs) != nil || len(locs) != 1 ||
		locs[0].URI != "migrations/001_accounts.sql" || locs[0].FileVersion != accountVersion {
		t.Fatalf("aliased SQL source definition missing: %+v %+v", definition, locs)
	}
	refs := query("references", first, sourceVersion)
	if !refs.OK || json.Unmarshal([]byte(refs.Output), &locs) != nil || len(locs) != 5 {
		t.Fatalf("aliased SQL direct references missing: %+v %+v", refs, locs)
	}
	for _, loc := range locs {
		if loc.FileVersion != stat(loc.URI) {
			t.Fatalf("unstable referenced source: %+v", loc)
		}
	}
	joined := query("definition", strings.Index(files["queries/list.sql"], "p.account_id")+2, sourceVersion)
	if !joined.OK || json.Unmarshal([]byte(joined.Output), &locs) != nil || len(locs) != 1 ||
		locs[0].URI != "migrations/002_payments.sql" || locs[0].FileVersion != paymentVersion {
		t.Fatalf("joined alias unresolved: %+v %+v", joined, locs)
	}
	// Mutate ONLY the test-owned source fixture to simulate an external editor.
	// This test is a read-only MCP semantic acceptance: it must not bypass the
	// production audit reserve merely to invoke workspace_write on a low-disk host.
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "queries/list.sql"),
		[]byte(files["queries/list.sql"]+"-- changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := query("definition", first, sourceVersion)
	if stale.OK || stale.ErrorCode != "file_changed" {
		t.Fatalf("stale SQL source accepted: %+v", stale)
	}
	// A malformed JOIN predicate must not create a migration definition or
	// reference through the public, authenticated MCP path. This is a real
	// Agent -> Executor -> Worker fixture; it never executes SQL or opens a DB.
	for _, invalid := range []string{
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = AND p.account_id;\n",
		"SELECT a.id FROM public.accounts a LEFT RIGHT JOIN public.payments p ON a.id=p.account_id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id < > p.account_id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = 'orphan' p.account_id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = p.account_id 'orphan';\n",
		"SELECT a.id() FROM public.accounts a;\n",
	} {
		if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "queries/list.sql"), []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		invalidVersion := stat("queries/list.sql")
		for _, method := range []string{"definition", "references"} {
			response := query(method, strings.Index(invalid, "a.id")+2, invalidVersion)
			if response.OK {
				var targets []struct {
					URI string `json:"uri"`
				}
				if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
					t.Fatalf("malformed SQL yielded malformed %s output: %v %+v", method, err, response)
				}
				for _, target := range targets {
					if strings.HasPrefix(target.URI, "migrations/") {
						t.Fatalf("malformed SQL fabricated source-backed MCP %s: %+v", method, target)
					}
				}
				if _, err := systemexec.SQLLens(); err != nil {
					t.Fatalf("malformed SQL unexpectedly produced offline MCP %s: %+v", method, response)
				}
			}
		}
	}
	// The function token is distinct from a real qualified column in the
	// *same* SELECT. The real column still reaches the migration declaration;
	// references must exclude the function's location.
	mixed := "SELECT a.id(), a.id FROM public.accounts a;\n"
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "queries/list.sql"), []byte(mixed), 0600); err != nil {
		t.Fatal(err)
	}
	mixedVersion := stat("queries/list.sql")
	functionDef := query("definition", strings.Index(mixed, "a.id")+2, mixedVersion)
	if functionDef.OK {
		t.Fatalf("qualified function resolved as a migration column: %+v", functionDef)
	}
	realColumnAt := strings.LastIndex(mixed, "a.id") + 2
	realDef := query("definition", realColumnAt, mixedVersion)
	if !realDef.OK || json.Unmarshal([]byte(realDef.Output), &locs) != nil || len(locs) != 1 ||
		locs[0].URI != "migrations/001_accounts.sql" {
		t.Fatalf("real column adjacent to function lost its definition: %+v %+v", realDef, locs)
	}
	realRefs := query("references", realColumnAt, mixedVersion)
	if !realRefs.OK || json.Unmarshal([]byte(realRefs.Output), &locs) != nil || len(locs) != 3 {
		t.Fatalf("function polluted source-backed references: %+v %+v", realRefs, locs)
	}
	t.Log("DIRECT_REAL_SQL_ALIAS_MCP_PASS definitions=2 references=5 versions=3 stale_rejected=true no_db=true malformed_on_rejected=true functions_excluded=true")
}
