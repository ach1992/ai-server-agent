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

func TestSQLLSPRequiresExplicitDialectAndRejectsInvalidScope(t *testing.T) {
	s, req, _ := testStdioBroker(t)
	req.Action = "code_inspect"
	req.CodeMethod = "symbols"
	req.Path = "001_create_tables.sql"
	req.FileVersion = "v1:unused"
	req.Language = "sql"
	if x := s.codeInspect(context.Background(), req); x.OK || x.ErrorCode != "sql_dialect_required" {
		t.Fatalf("implicit SQL grammar unexpectedly allowed: %+v", x)
	}
	req.SQLDialect = "oracle"
	if x := s.codeInspect(context.Background(), req); x.OK || x.ErrorCode != "unsupported_sql_dialect" {
		t.Fatalf("unrecognized SQL syntax silently accepted: %+v", x)
	}
	req.SQLDialect = "mysql"
	req.Language = "python"
	if x := s.codeInspect(context.Background(), req); x.OK || x.ErrorCode != "code_language_mismatch" {
		t.Fatalf("mismatched SQL language accepted: %+v", x)
	}
	req.Path = "main.go"
	req.Language = "go"
	if x := s.codeInspect(context.Background(), req); x.OK || x.ErrorCode != "invalid_sql_dialect_scope" {
		t.Fatalf("SQL grammar smuggled into Go: %+v", x)
	}
	req.Path = "../../outside.sql"
	req.Language = "sql"
	if x := s.codeInspect(context.Background(), req); x.OK || x.ErrorCode != "invalid_code_path" {
		t.Fatalf("SQL source escaped workspace: %+v", x)
	}
}

func TestSQLLSPProfileDisablesProjectPluginExecution(t *testing.T) {
	for _, dialect := range []string{"postgres", "postgresql", "mysql", "mariadb"} {
		profile, ok := sqlLSPProfile(dialect, "/root/pinned/node", "/root/pinned/cli.js")
		if !ok || profile.arguments[0] != "SQLLENS_NO_PLUGINS=1" ||
			profile.arguments[1] != "SQLLENS_USER_CONFIG=/dev/null" ||
			profile.arguments[3] != "/root/pinned/node" ||
			profile.arguments[4] != "/root/pinned/cli.js" ||
			!profile.waitForInitialPush || !profile.pushOnlyDiagnostics {
			t.Fatalf("SQL profile would allow project-controlled executable/plugin: %s: %+v", dialect, profile)
		}
		want := "sql-mysql"
		if strings.HasPrefix(dialect, "postgre") || dialect == "postgres" {
			want = "sql-postgres"
		}
		if got := profile.languageForPath("migration.sql"); got != want {
			t.Fatalf("dialect overridden by project file path: %s -> %s", dialect, got)
		}
	}
	for _, bad := range []string{"", "sqlite", "postgres;curl", "POSTGRES", "../../postgres"} {
		if _, ok := sqlLSPProfile(bad, "/bin/node", "/opt/lsp/cli.js"); ok {
			t.Fatalf("unrecognized dialect accepted: %q", bad)
		}
	}
}

func TestCodeInspectRealOfflineSQLPostgresMySQL(t *testing.T) {
	node := os.Getenv("AGENT_SQL_LSP_PROOF_NODE")
	cli := os.Getenv("AGENT_SQL_LSP_PROOF_CLI")
	if node == "" && cli == "" {
		t.Skip("requires isolated offline SQL native Node and CLI fixture")
	}
	if node == "" || cli == "" {
		t.Fatal("SQL LSP fixture must specify both node and executable")
	}
	requireWorkerLandlockV2(t)
	if !systemexec.Trusted(systemexec.SQLLensEnv) {
		t.Skip("trusted fixed env executable not available on this platform")
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.sqlNodeBinary = node
	s.sqlCLIBinary = cli
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "asa-sql-file-helper")
	build := exec.Command(goBin, "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker file helper: %v %s", err, output)
	}
	s.workspaceHelperBinary = helper
	// A malicious repository plugin and an optional DB setting are not
	// executable authority. In particular, no connection or project import
	// is permitted by this capability's fixed process environment.
	fixtures := map[string]string{
		".sqllens.json":              `{"plugins":["./malicious-plugin.mjs"],"default":"databricks","dialects":[{"files":"*.sql","dialect":"snowflake"}]}`,
		"malicious-plugin.mjs":       `import {writeFileSync} from "node:fs";writeFileSync("SQL-MUST-NOT-EXECUTE","unexpected execution");export default ()=>({});`,
		"postgres.sql":               "WITH weekly AS (SELECT 1 AS amount) SELECT amount FROM weekly;\n",
		"mysql.sql":                  "WITH weekly AS (SELECT 1 AS amount) SELECT amount FROM weekly;\n",
		"schema-version.sql":         "CREATE TABLE inventory (item_id bigint PRIMARY KEY);\n",
		"mysql-migration-001.sql":    mysqlMigration001,
		"mysql-migration-002.sql":    mysqlMigration002,
		"postgres-migration-001.sql": postgresMigration001,
		"postgres-migration-002.sql": postgresMigration002,
	}
	for path, value := range fixtures {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(path string) Response {
		t.Helper()
		resp := s.workerWorkspaceFile(context.Background(), Request{Action: "workspace_stat", Workspace: workspace, Path: path})
		if !resp.OK || resp.FileVersion == "" {
			t.Fatalf("SQL source metadata not proven: %+v", resp)
		}
		return resp
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, tc := range []struct{ path, dialect string }{
		{"postgres.sql", "postgres"}, {"mysql.sql", "mysql"}, {"mysql.sql", "mariadb"},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(workspace, tc.path),
				[]byte("WITH weekly AS (SELECT 1 AS amount) SELECT amount FROM weekly;\n"), 0600); err != nil {
				t.Fatal(err)
			}
			version := stat(tc.path).FileVersion
			req := owner
			req.Action, req.Workspace, req.Path = "code_inspect", workspace, tc.path
			req.Language, req.SQLDialect, req.FileVersion = "sql", tc.dialect, version
			req.CodeMethod = "symbols"
			symbols := s.codeInspect(ctx, req)
			if !symbols.OK || symbols.FileVersion != version || !strings.Contains(symbols.Output, "weekly") {
				t.Fatalf("%s CTE semantic symbols missing or unversioned: %+v", tc.dialect, symbols)
			}
			req.CodeMethod = "diagnostics"
			incorrect := "WITH weekly AS (SELECT 1 AS amount) SELECT FROM;\n"
			if err := os.WriteFile(filepath.Join(workspace, tc.path), []byte(incorrect), 0600); err != nil {
				t.Fatal(err)
			}
			req.FileVersion = stat(tc.path).FileVersion
			diag := s.codeInspect(ctx, req)
			if !diag.OK || diag.FileVersion != req.FileVersion ||
				!strings.Contains(diag.Output, "severity") {
				t.Fatalf("%s syntax error not positively diagnosed: %+v", tc.dialect, diag)
			}
			req.FileVersion = version
			if stale := s.codeInspect(ctx, req); stale.OK || stale.ErrorCode != "file_changed" {
				t.Fatalf("%s accepted stale file version: %+v", tc.dialect, stale)
			}
		})
	}
	// Real migrations, not only trivial CTEs: prove grammar/symbol responses
	// are produced in both engine families without opening a database.
	for _, migration := range []struct{ path, dialect, table string }{
		{"mysql-migration-001.sql", "mysql", "acceptance_accounts"},
		{"postgres-migration-001.sql", "postgres", "acceptance_accounts"},
	} {
		req := owner
		req.Action, req.Language, req.SQLDialect = "code_inspect", "sql", migration.dialect
		req.Path, req.FileVersion, req.CodeMethod = migration.path, stat(migration.path).FileVersion, "symbols"
		out := s.codeInspect(ctx, req)
		if !out.OK || out.FileVersion != req.FileVersion {
			t.Fatalf("%s real migration LSP processing failed: %+v", migration.dialect, out)
		}
		if !strings.Contains(out.Output, migration.table) ||
			!strings.Contains(out.Output, "DDL column") ||
			!strings.Contains(out.Output, "selectionRange") {
			t.Fatalf("%s DDL semantic table/column outline unavailable: %s", migration.dialect, out.Output)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "SQL-MUST-NOT-EXECUTE")); !os.IsNotExist(err) {
		t.Fatalf("untrusted SQL project plugin executed: %v", err)
	}
	s.sessions.mu.Lock()
	active := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if active != 0 {
		t.Fatalf("SQL LSP sessions leaked: %d", active)
	}
}
