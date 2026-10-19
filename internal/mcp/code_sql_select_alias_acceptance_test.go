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
	// Many real authenticated MCP calls share this acceptance context.
	// Individual Agent/Executor/Worker bounds remain unchanged; the
	// aggregate deadline must allow a slower occupied validation host.
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
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
		"migrations/001_accounts.sql":       "CREATE TABLE public.accounts (id BIGINT PRIMARY KEY, name TEXT);\n",
		"migrations/002_payments.sql":       "CREATE TABLE public.payments (id BIGINT PRIMARY KEY, account_id BIGINT REFERENCES public.accounts(id));\n",
		"migrations/003_quoted_only.sql":    "CREATE TABLE \"only\" (id BIGINT PRIMARY KEY);\n",
		"queries/list.sql":                  "SELECT a.id, p.account_id FROM public.accounts AS a JOIN public.payments p ON p.account_id = a.id WHERE a.id > 0;\n",
		"queries/pg_only_modifier.sql":      "SELECT accounts.id FROM ONLY accounts;\n",
		"queries/pg_join_only_modifier.sql": "SELECT accounts.id FROM public.payments p JOIN ONLY accounts ON p.account_id=accounts.id;\n",
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
	queryDialect := func(method, dialect string, character int, sourceVersion string) executor.Response {
		return call("code_"+method, map[string]any{"workspace": cfg.WorkspaceDir,
			"path": "queries/list.sql", "file_version": sourceVersion, "language": "sql",
			"sql_dialect": dialect, "line": 0, "character": character})
	}
	query := func(method string, character int, sourceVersion string) executor.Response {
		return queryDialect(method, "postgres", character, sourceVersion)
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
	// An invalid FROM/JOIN source scope, skipped SQL literal or malformed
	// ON predicate must not create a migration definition or reference through
	// the public, authenticated MCP path. This is a real
	// Agent -> Executor -> Worker fixture; it never executes SQL or opens a DB.
	for _, invalid := range []string{
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = AND p.account_id;\n",
		"SELECT a.id FROM public.accounts a LEFT RIGHT JOIN public.payments p ON a.id=p.account_id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id < > p.account_id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = 'orphan' p.account_id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = p.account_id 'orphan';\n",
		"SELECT a.id() FROM public.accounts a;\n",
		"SELECT a.id FROM public.'orphan'accounts a;\n",
		"SELECT a.id FROM public.accounts 'orphan' a;\n",
		"SELECT a.id FROM public.accounts AS 'orphan' a;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments 'orphan' p ON p.account_id=a.id;\n",
		"SELECT a.id FROM public.accounts a JUNK;\n",
		"SELECT a.id FROM public.accounts a JUNK JOIN public.payments p ON p.account_id=a.id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p NOISE ON p.account_id=a.id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p USING (id,);\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p USING (id) ON a.id=p.account_id;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p USING ('id');\n",
		"SELECT a.id FROM public.accounts a LEFT NOISE JOIN public.payments p ON p.account_id=a.id;\n",
		"SELECT accounts.id FROM ONLY accounts;\n",
		"SELECT accounts.id FROM public.payments p JOIN ONLY accounts ON p.account_id=accounts.id;\n",
		"SELECT accounts.id FROM TABLE accounts;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id=ONLY;\n",
		"SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id=TABLE;\n",
	} {
		if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "queries/list.sql"), []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		invalidVersion := stat("queries/list.sql")
		selected := "a.id"
		if strings.Contains(invalid, "accounts.id") {
			selected = "accounts.id"
		}
		at := strings.Index(invalid, selected)
		if at < 0 {
			t.Fatalf("missing malformed source selection %q", selected)
		}
		at += strings.LastIndex(selected, ".") + 2
		for _, method := range []string{"definition", "references"} {
			response := query(method, at, invalidVersion)
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

	// N4: validate table-level cursor positions, not just a.id columns.
	// Both SQL files remain in the workspace when we later resolve the real
	// quoted "only" migration; neither may pollute its code_references.
	for _, tc := range []struct{ path, source string }{
		{"queries/pg_only_modifier.sql", files["queries/pg_only_modifier.sql"]},
		{"queries/pg_join_only_modifier.sql", files["queries/pg_join_only_modifier.sql"]},
	} {
		at := strings.Index(tc.source, "ONLY")
		if at < 0 {
			t.Fatal("missing PostgreSQL ONLY modifier")
		}
		for _, method := range []string{"definition", "references"} {
			response := call("code_"+method, map[string]any{
				"workspace": cfg.WorkspaceDir, "path": tc.path,
				"file_version": stat(tc.path), "language": "sql", "sql_dialect": "postgres",
				"line": 0, "character": at + 1,
			})
			if response.OK {
				var targets []struct {
					URI string `json:"uri"`
				}
				if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
					t.Fatalf("invalid table-level %s returned unreadable output: %v", method, err)
				}
				for _, target := range targets {
					if strings.HasPrefix(target.URI, "migrations/") {
						t.Fatalf("PG ONLY modifier became source-backed %s target: %+v", method, response)
					}
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
	// An actual quoted PostgreSQL "only" migration exists beside accounts.
	// The preceding ONLY modifier negatives must never reach this real table,
	// while an explicitly quoted relation remains navigable.
	quotedOnly := "SELECT o.id FROM \"only\" o;\n"
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "queries/list.sql"), []byte(quotedOnly), 0600); err != nil {
		t.Fatal(err)
	}
	quotedVersion := stat("queries/list.sql")
	quotedDef := query("definition", strings.Index(quotedOnly, "o.id")+2, quotedVersion)
	if !quotedDef.OK || json.Unmarshal([]byte(quotedDef.Output), &locs) != nil || len(locs) != 1 ||
		locs[0].URI != "migrations/003_quoted_only.sql" ||
		locs[0].FileVersion != stat(locs[0].URI) {
		t.Fatalf("quoted PostgreSQL only table mapped to wrong declaration: %+v %+v", quotedDef, locs)
	}
	quotedRefs := query("references", strings.Index(quotedOnly, "o.id")+2, quotedVersion)
	if !quotedRefs.OK || json.Unmarshal([]byte(quotedRefs.Output), &locs) != nil || len(locs) != 2 {
		t.Fatalf("quoted PostgreSQL only table references lost: %+v %+v", quotedRefs, locs)
	}

	// ONLY is a nonreserved ordinary table name in MySQL/MariaDB. Reuse the
	// same real MCP/worker path and test-owned migration with dialect-correct
	// identifier quoting, rather than generalizing PostgreSQL's ONLY rule.
	for _, dialect := range []string{"mysql", "mariadb"} {
		if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "migrations/003_quoted_only.sql"),
			[]byte("CREATE TABLE `only` (id BIGINT PRIMARY KEY);\n"), 0600); err != nil {
			t.Fatal(err)
		}
		onlyQuery := "SELECT accounts.id FROM ONLY accounts;\n"
		if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "queries/list.sql"), []byte(onlyQuery), 0600); err != nil {
			t.Fatal(err)
		}
		onlyVersion := stat("queries/list.sql")
		at := strings.Index(onlyQuery, "accounts.id") + len("accounts.") + 1
		onlyDef := queryDialect("definition", dialect, at, onlyVersion)
		if !onlyDef.OK || json.Unmarshal([]byte(onlyDef.Output), &locs) != nil || len(locs) != 1 ||
			locs[0].URI != "migrations/003_quoted_only.sql" ||
			locs[0].FileVersion != stat(locs[0].URI) {
			t.Fatalf("%s ONLY ordinary table did not resolve to its own migration: %+v %+v", dialect, onlyDef, locs)
		}
		onlyRefs := queryDialect("references", dialect, at, onlyVersion)
		// The intentionally invalid PostgreSQL ONLY fixtures are valid
		// MySQL/MariaDB relation+alias expressions, contributing three
		// additional source-backed column uses in these dialects.
		if !onlyRefs.OK || json.Unmarshal([]byte(onlyRefs.Output), &locs) != nil || len(locs) != 5 {
			t.Fatalf("%s ONLY ordinary table reference mismatch: %+v %+v", dialect, onlyRefs, locs)
		}
		// The same unquoted table name must stay physical inside a JOIN.
		joinQuery := "SELECT o.id FROM public.accounts a JOIN ONLY o ON o.id=a.id;\n"
		if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "queries/list.sql"), []byte(joinQuery), 0600); err != nil {
			t.Fatal(err)
		}
		joinVersion := stat("queries/list.sql")
		joinAt := strings.Index(joinQuery, "o.id") + 2
		joinDef := queryDialect("definition", dialect, joinAt, joinVersion)
		if !joinDef.OK || json.Unmarshal([]byte(joinDef.Output), &locs) != nil || len(locs) != 1 ||
			locs[0].URI != "migrations/003_quoted_only.sql" {
			t.Fatalf("%s JOIN ONLY source mapped to wrong table: %+v %+v", dialect, joinDef, locs)
		}
		joinRefs := queryDialect("references", dialect, joinAt, joinVersion)
		if !joinRefs.OK || json.Unmarshal([]byte(joinRefs.Output), &locs) != nil || len(locs) != 6 {
			t.Fatalf("%s JOIN ONLY real references lost: %+v %+v", dialect, joinRefs, locs)
		}
	}

	// Independent-review R1/R2/R3: production-like cross-file navigation
	// after all positive reference-count assertions above. Test-owned SQL
	// is deliberately added late to avoid altering their fixture baselines.
	writeSQL := func(path, source string) {
		t.Helper()
		name := filepath.Join(cfg.WorkspaceDir, path)
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	invalidMigration := "migrations/004_invalid_partial.sql"
	writeSQL(invalidMigration, "CREATE TABLE impossible (id BIGINT, ONLY BIGINT);\n")
	for _, tc := range []struct{ name, source, selected string }{
		{"r1_pg_qualified_only", `SELECT ONLY.id FROM "only";` + "\n", "ONLY.id"},
		{"r2_view_invalid_on", "CREATE VIEW invalid_view AS SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = ONLY;\n", "public.accounts"},
		{"r2_view_unknown_gap", "CREATE VIEW invalid_view AS SELECT a.id FROM public.accounts a JUNK JOIN public.payments p ON a.id=p.id;\n", "public.accounts"},
		{"r2_insert_invalid_on", "INSERT INTO archive (id) SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = ONLY;\n", "public.accounts"},
		{"r3_invalid_ddl_column", "SELECT i.id FROM impossible i;\n", "i.id"},
		{"f1_qualified_projection_literal", "SELECT a.'ignored'id FROM public.accounts a;\n", "id"},
		{"f1_qualified_where_literal", "SELECT a.id FROM public.accounts a WHERE a.'ignored'id=1;\n", "a.id"},
		{"f2_unproven_view_body", "CREATE VIEW bogus AS FROBNICATE;\n", "bogus"},
		{"f2_function_join_bad_on", "CREATE VIEW bogus AS SELECT * FROM generate_series(1,2) g JOIN public.accounts a ON a.id=ONLY;\n", "public.accounts"},
		{"f2_where_bad_operand", "CREATE VIEW bogus AS SELECT a.id FROM public.accounts a WHERE ONLY;\n", "public.accounts"},
		{"f3_index_bad_suffix", "CREATE INDEX bogus ON public.accounts (id) ONLY;\n", "public.accounts"},
		{"f3_alter_unknown_action", "ALTER TABLE public.accounts ADD COLUMN okay INT, BOGUS REFERENCES public.payments(id);\n", "public.payments"},
		{"f4_projection_double_alias", "SELECT a.id AS uid EXTRA FROM public.accounts a;\n", "a.id"},
		{"f4_projection_unknown_tokens", "SELECT a.id JUNK EXTRA FROM public.accounts a;\n", "a.id"},
		{"r1_trailing_where_literal", "SELECT a.id FROM public.accounts a WHERE a.id=1 'discarded';\n", "a.id"},
		{"r3_invalid_insert_body", "INSERT INTO public.accounts (id) FROBNICATE;\n", "public.accounts"},
		{"r3_invalid_update_body", "UPDATE public.accounts FROBNICATE;\n", "public.accounts"},
		{"r5_reserved_index_name", "CREATE INDEX ONLY ON public.accounts (id);\n", "public.accounts"},
		{"r5_invalid_pg_alter_rename_index", "ALTER TABLE public.accounts RENAME INDEX ix TO new_ix;\n", "public.accounts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "queries/r123.sql"
			writeSQL(path, tc.source)
			start := strings.Index(tc.source, tc.selected)
			if start < 0 {
				t.Fatal("selection missing")
			}
			at := start + len(tc.selected) - 1
			for _, method := range []string{"definition", "references"} {
				response := call("code_"+method, map[string]any{
					"workspace": cfg.WorkspaceDir, "path": path, "file_version": stat(path),
					"language": "sql", "sql_dialect": "postgres",
					"line": 0, "character": at,
				})
				if response.OK {
					var targets []struct {
						URI string `json:"uri"`
					}
					if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
						t.Fatalf("%s returned invalid locations: %v", method, err)
					}
					for _, target := range targets {
						if strings.HasPrefix(target.URI, "migrations/") {
							t.Fatalf("%s fabricated migration navigation from %s: %+v", method, tc.name, response)
						}
					}
				}
			}
		})
	}
	// F2/F3: malformed CREATE statements must not become a source-backed
	// migration declaration even when the querying SQL itself is valid.
	for _, tc := range []struct{ name, ddl string }{
		{"incomplete_primary", "CREATE TABLE impossible (id BIGINT, PRIMARY KEY ONLY);\n"},
		{"f5_structural_datatype", "CREATE TABLE impossible (id SELECT);\n"},
		{"f5_incomplete_default", "CREATE TABLE impossible (id INT DEFAULT +);\n"},
		{"incomplete_unique", "CREATE TABLE impossible (id BIGINT, UNIQUE wrong);\n"},
		{"invalid_create_view_body", "CREATE VIEW impossible AS FROBNICATE;\n"},
		{"invalid_function_join", "CREATE VIEW impossible AS SELECT * FROM generate_series(1,2) g JOIN public.accounts a ON a.id=ONLY;\n"},
		{"r1_ignored_constraint_literal", "CREATE TABLE impossible (id BIGINT, PRIMARY KEY (id) 'discarded');\n"},
		{"r2_scalar_operator_view", "CREATE VIEW impossible AS SELECT +;\n"},
		{"r4_type_keyword_with", "CREATE TABLE impossible (id WITH);\n"},
		{"r4_type_keyword_only", "CREATE TABLE impossible (id ONLY);\n"},
		{"extra_materialized_suffix_literal", "CREATE MATERIALIZED VIEW impossible AS SELECT 1 'discarded' WITH NO DATA;\n"},
	} {
		t.Run("f2_f3_declaration_"+tc.name, func(t *testing.T) {
			writeSQL(invalidMigration, tc.ddl)
			source := "SELECT i.id FROM impossible i;\n"
			path := "queries/r123.sql"
			writeSQL(path, source)
			for _, method := range []string{"definition", "references"} {
				response := call("code_"+method, map[string]any{
					"workspace": cfg.WorkspaceDir, "path": path, "file_version": stat(path),
					"language": "sql", "sql_dialect": "postgres", "line": 0,
					"character": strings.Index(source, "i.id") + 2,
				})
				if response.OK {
					var targets []struct {
						URI string `json:"uri"`
					}
					if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
						t.Fatal(err)
					}
					for _, target := range targets {
						if target.URI == invalidMigration {
							t.Fatalf("unproven %s returned %s source: %+v", tc.name, method, response)
						}
					}
				}
			}
		})
	}
	// Specialized emission boundaries must not turn invalid migrations
	// into cross-file physical declarations. Reuse the authenticated MCP
	// client and Worker fixture, not a second parser or test harness.
	for _, tc := range []struct {
		name, dialect, ddl, relation string
		valid                        bool
	}{
		{"mysql_like_gap", "mysql", "CREATE TABLE phantom 'discarded' LIKE parent;", "phantom", false},
		{"mariadb_like_gap", "mariadb", "CREATE TABLE phantom 'discarded' LIKE parent;", "phantom", false},
		{"mysql_engine_gap", "mysql", "CREATE TABLE phantom (id INT) ENGINE='discarded'InnoDB;", "phantom", false},
		{"mariadb_engine_gap", "mariadb", "CREATE TABLE phantom (id INT) 'discarded' ENGINE=InnoDB;", "phantom", false},
		{"pg_constant_alias_gap", "postgres", "CREATE VIEW phantom AS SELECT 1 'discarded' AS id;", "phantom", false},
		{"pg_materialized_alias_gap", "postgres", "CREATE MATERIALIZED VIEW phantom AS SELECT 1 'discarded' AS id WITH NO DATA;", "phantom", false},
		{"mysql_like_valid", "mysql", "CREATE TABLE verified_like LIKE parent;", "verified_like", true},
		{"mariadb_engine_valid", "mariadb", "CREATE TABLE verified_engine (id INT) ENGINE=InnoDB;", "verified_engine", true},
		{"pg_view_valid", "postgres", "CREATE VIEW verified_view AS SELECT 1 AS id;", "verified_view", true},
		{"pg_materialized_valid", "postgres", "CREATE MATERIALIZED VIEW verified_mv AS SELECT 1 AS id WITH NO DATA;", "verified_mv", true},
	} {
		t.Run("review_lexical_crossfile_"+tc.name, func(t *testing.T) {
			writeSQL(invalidMigration, tc.ddl+"\n")
			query := "SELECT * FROM " + tc.relation + ";\n"
			path := "queries/r123.sql"
			writeSQL(path, query)
			cursor := strings.Index(query, tc.relation) + 1
			for _, method := range []string{"definition", "references"} {
				response := call("code_"+method, map[string]any{
					"workspace": cfg.WorkspaceDir, "path": path,
					"file_version": stat(path), "language": "sql",
					"sql_dialect": tc.dialect, "line": 0, "character": cursor,
				})
				if tc.valid && !response.OK {
					t.Fatalf("valid %s %s not navigable: %+v", tc.dialect, method, response)
				}
				if !response.OK {
					continue
				}
				var locations []struct {
					URI         string `json:"uri"`
					FileVersion string `json:"file_version"`
				}
				if err := json.Unmarshal([]byte(response.Output), &locations); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, loc := range locations {
					if loc.URI != invalidMigration {
						continue
					}
					if !tc.valid {
						t.Fatalf("invalid %s migration returned %s: %+v", tc.dialect, method, response)
					}
					if loc.FileVersion != stat(invalidMigration) {
						t.Fatalf("stale %s migration source version: %+v", method, loc)
					}
					found = true
				}
				if tc.valid && !found {
					t.Fatalf("valid %s declaration absent from %s: %+v", tc.dialect, method, response)
				}
			}
		})
	}
	// An invalid MATERIALIZED header must not falsify migration history
	// for an otherwise genuine declared view.
	writeSQL("migrations/007_mv_create.sql",
		"CREATE MATERIALIZED VIEW reporting.review_mv AS SELECT 1 AS id;\n")
	for _, malformed := range []string{
		"ALTER MATERIALIZED 'discarded' VIEW reporting.review_mv RENAME TO renamed;",
		"ALTER MATERIALIZED VIEW reporting.review_mv 'discarded' RENAME TO renamed;",
	} {
		t.Run("review_materialized_history_"+malformed, func(t *testing.T) {
			writeSQL("migrations/008_mv_alter.sql", malformed+"\n")
			path, query := "queries/r123.sql", "SELECT * FROM reporting.review_mv;\n"
			writeSQL(path, query)
			cursor := strings.Index(query, "review_mv") + 2
			for _, method := range []string{"definition", "references"} {
				response := call("code_"+method, map[string]any{
					"workspace": cfg.WorkspaceDir, "path": path, "file_version": stat(path),
					"language": "sql", "sql_dialect": "postgres", "line": 0, "character": cursor,
				})
				if !response.OK {
					t.Fatalf("invalid ALTER poisoned valid %s: %+v", method, response)
				}
				var locations []struct {
					URI         string `json:"uri"`
					FileVersion string `json:"file_version"`
				}
				if err := json.Unmarshal([]byte(response.Output), &locations); err != nil {
					t.Fatal(err)
				}
				hasCreate := false
				for _, loc := range locations {
					if loc.URI == "migrations/008_mv_alter.sql" {
						t.Fatalf("invalid ALTER fabricated %s lifecycle: %+v", method, response)
					}
					if loc.URI == "migrations/007_mv_create.sql" {
						if loc.FileVersion != stat(loc.URI) {
							t.Fatalf("invalid %s source file_version: %+v", method, loc)
						}
						hasCreate = true
					}
				}
				if !hasCreate {
					t.Fatalf("valid materialized declaration missing from %s: %+v", method, response)
				}
			}
		})
	}
	// A malformed INDEX suffix or ALTER action must not contaminate
	// code_references for a real, independently declared accounts table.
	badIndexPath := "migrations/005_bad_index.sql"
	badAlterPath := "migrations/006_bad_alter.sql"
	writeSQL(badIndexPath, "CREATE INDEX fake_idx ON public.accounts (id) ONLY;\n")
	writeSQL(badAlterPath, "ALTER TABLE public.accounts ADD COLUMN okay INT, BOGUS REFERENCES public.payments(id);\n")
	validQuery := "SELECT a.id FROM public.accounts a;\n"
	writeSQL("queries/r123.sql", validQuery)
	for _, selected := range []string{"a.id", "public.accounts"} {
		at := strings.Index(validQuery, selected) + len(selected) - 1
		response := call("code_references", map[string]any{
			"workspace": cfg.WorkspaceDir, "path": "queries/r123.sql",
			"file_version": stat("queries/r123.sql"), "language": "sql",
			"sql_dialect": "postgres", "line": 0, "character": at,
		})
		if response.OK {
			var targets []struct {
				URI string `json:"uri"`
			}
			if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
				t.Fatal(err)
			}
			for _, target := range targets {
				if target.URI == badIndexPath || target.URI == badAlterPath {
					t.Fatalf("invalid migration %s polluted %s references: %+v", target.URI, selected, response)
				}
			}
		}
	}
	// A valid nested JOIN remains navigable through real authenticated MCP.
	// The fail-closed R2 correction must not discard a provable contained
	// SELECT just because its enclosing operation is CREATE VIEW.
	embeddedPath := "queries/valid_embedded.sql"
	embedded := "CREATE VIEW valid_view AS SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id=p.account_id;\n"
	writeSQL(embeddedPath, embedded)
	embeddedAt := strings.Index(embedded, "FROM public.accounts") + len("FROM public.") + 1
	for _, method := range []string{"definition", "references"} {
		response := call("code_"+method, map[string]any{
			"workspace": cfg.WorkspaceDir, "path": embeddedPath, "file_version": stat(embeddedPath),
			"language": "sql", "sql_dialect": "postgres", "line": 0, "character": embeddedAt,
		})
		if !response.OK {
			t.Fatalf("valid embedded %s lost physical reference: %+v", method, response)
		}
		var targets []struct {
			URI         string `json:"uri"`
			FileVersion string `json:"file_version"`
		}
		if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
			t.Fatal(err)
		}
		want := "migrations/001_accounts.sql"
		if method == "references" {
			want = embeddedPath
		}
		found := false
		for _, loc := range targets {
			if loc.URI == want && loc.FileVersion == stat(want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("valid contained %s missing %s: %+v", method, want, response)
		}
	}
	// R1-R5: a complete supported direct DML is still a navigable
	// table use, including VALUES containing a separately elided string.
	// Check real bearer-authenticated definition and references with
	// exact target file_version after the negative cases.
	for _, sql := range []string{
		"INSERT INTO public.accounts (id,name) VALUES (1,'owned');\n",
		"INSERT INTO public.accounts (id) SELECT a.id FROM public.accounts a;\n",
		"UPDATE public.accounts SET id=2, name='owned' WHERE id=1;\n",
		"DELETE FROM public.accounts WHERE id=1;\n",
		"TRUNCATE TABLE public.accounts;\n",
	} {
		path := "queries/r123.sql"
		writeSQL(path, sql)
		pos := strings.Index(sql, "public.accounts") + len("public.") + 1
		for _, method := range []string{"definition", "references"} {
			response := call("code_"+method, map[string]any{
				"workspace": cfg.WorkspaceDir, "path": path, "file_version": stat(path),
				"language": "sql", "sql_dialect": "postgres", "line": 0, "character": pos,
			})
			if !response.OK {
				t.Fatalf("valid DML %s lost %s: %+v", sql, method, response)
			}
			var targets []struct {
				URI         string `json:"uri"`
				FileVersion string `json:"file_version"`
			}
			if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
				t.Fatal(err)
			}
			want := "migrations/001_accounts.sql"
			if method == "references" {
				want = path
			}
			matched := false
			for _, target := range targets {
				if target.URI == want && target.FileVersion == stat(want) {
					matched = true
				}
			}
			if !matched {
				t.Fatalf("valid DML %s missing versioned %s %s: %+v", sql, method, want, response)
			}
		}
	}
	// A separate USING query is introduced only after the fixed-count base
	// cases. It must preserve source-backed qualified identifiers while
	// refusing to attribute an unqualified USING member to one table.
	usingPath := "queries/using.sql"
	usingSQL := "SELECT a.id, p.account_id FROM public.accounts a JOIN public.payments p USING (id) WHERE p.account_id > 0;\n"
	writeSQL(usingPath, usingSQL)
	usingVersion := stat(usingPath)
	for _, method := range []string{"definition", "references"} {
		res := call("code_"+method, map[string]any{
			"workspace": cfg.WorkspaceDir, "path": usingPath,
			"file_version": usingVersion, "language": "sql", "sql_dialect": "postgres",
			"line": 0, "character": strings.Index(usingSQL, "p.account_id") + 2,
		})
		if !res.OK {
			t.Fatalf("JOIN USING lost authenticated %s: %+v", method, res)
		}
		var targets []struct {
			URI         string `json:"uri"`
			FileVersion string `json:"file_version"`
		}
		if err := json.Unmarshal([]byte(res.Output), &targets); err != nil {
			t.Fatal(err)
		}
		want := "migrations/002_payments.sql"
		if method == "references" {
			want = usingPath
		}
		found := false
		for _, target := range targets {
			if target.URI == want {
				if target.FileVersion != stat(want) {
					t.Fatalf("JOIN USING %s returned stale target: %+v", method, target)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("JOIN USING %s lacks %s: %+v", method, want, targets)
		}
	}
	// The unqualified USING member is a join constraint, not a physical
	// source-backed column owned by one side. Neither API may fabricate
	// a migration navigation target for that token.
	usingMemberAt := strings.Index(usingSQL, "USING (id)") + len("USING (") + 1
	for _, method := range []string{"definition", "references"} {
		res := call("code_"+method, map[string]any{
			"workspace": cfg.WorkspaceDir, "path": usingPath,
			"file_version": usingVersion, "language": "sql", "sql_dialect": "postgres",
			"line": 0, "character": usingMemberAt,
		})
		if res.OK {
			var targets []struct {
				URI string `json:"uri"`
			}
			if err := json.Unmarshal([]byte(res.Output), &targets); err != nil {
				t.Fatal(err)
			}
			for _, target := range targets {
				if strings.HasPrefix(target.URI, "migrations/") {
					t.Fatalf("unqualified USING token created fabricated %s target: %+v", method, targets)
				}
			}
		}
	}
	// The new USING fixture must also reject stale optimistic source versions.
	writeSQL(usingPath, usingSQL+"-- changed\n")
	staleUsing := call("code_definition", map[string]any{
		"workspace": cfg.WorkspaceDir, "path": usingPath,
		"file_version": usingVersion, "language": "sql", "sql_dialect": "postgres",
		"line": 0, "character": strings.Index(usingSQL, "p.account_id") + 2,
	})
	if staleUsing.OK || staleUsing.ErrorCode != "file_changed" {
		t.Fatalf("JOIN USING stale version accepted: %+v", staleUsing)
	}
	// The exact duplicate-normalization remediation must survive the real
	// authenticated MCP path for both MySQL-family dialects. A successfully
	// parsed but semantically duplicate USING list must never publish a
	// source-backed migration definition or reference.
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, members := range []string{"id, `ID`", "`Id`, `id`", "`ID`, id"} {
			invalid := "SELECT a.id FROM public.accounts a JOIN public.payments p USING (" + members + ");\n"
			writeSQL(usingPath, invalid)
			version := stat(usingPath)
			at := strings.Index(invalid, "a.id") + 2
			for _, method := range []string{"definition", "references"} {
				response := call("code_"+method, map[string]any{
					"workspace": cfg.WorkspaceDir, "path": usingPath,
					"file_version": version, "language": "sql", "sql_dialect": dialect,
					"line": 0, "character": at,
				})
				if response.OK {
					var targets []struct {
						URI string `json:"uri"`
					}
					if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
						t.Fatalf("%s duplicate USING %s malformed output: %v", dialect, method, err)
					}
					for _, target := range targets {
						if strings.HasPrefix(target.URI, "migrations/") {
							t.Fatalf("%s duplicate USING %s published physical source: %+v", dialect, method, targets)
						}
					}
				}
			}
		}
	}
	t.Log("DIRECT_REAL_SQL_ALIAS_MCP_PASS using_qualified=true r1_r2_r3_r4_r5_root_emission_proved=true definitions=2 references=5 versions=3 stale_rejected=true no_db=true malformed_on_rejected=true functions_excluded=true source_literals_rejected=true unconsumed_source_gaps_rejected=true pg_on_reserved_rejected=true pg_only_modifier_rejected=true pg_quoted_only_resolved=true mysql_only_resolved=true mariadb_only_resolved=true mysql_mariadb_join_only_resolved=true r1_r2_r3_crossfile_rejected=true f1_f2_f3_proven_emission_rejected=true f4_f5_pre_review_invariants=true n4_table_level_modifier_rejected=true n4_no_false_crossfile_refs=true")
}
