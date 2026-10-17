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

	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

// PostgreSQL materialized views are persisted relations, but SQLLens cannot
// infer the current database. Only conservative, source-backed declarations,
// refresh references and migration tombstones are admitted.
func TestSQLPostgresMaterializedViewSourceGraph(t *testing.T) {
	source := "CREATE MATERIALIZED VIEW IF NOT EXISTS analytics.\"DailySummary\" AS SELECT * FROM public.orders;\n" +
		"REFRESH MATERIALIZED VIEW CONCURRENTLY analytics.\"DailySummary\" WITH DATA;\n" +
		"SELECT * FROM analytics.\"DailySummary\";\n" +
		"DROP MATERIALIZED VIEW IF EXISTS analytics.\"DailySummary\", analytics.old_summary RESTRICT;\n"
	items, valid := sqlNavOccurrences(source, "postgres")
	if !valid || len(items) != 6 {
		t.Fatalf("materialized source graph incomplete/unsafe: valid=%t occurrences=%+v", valid, items)
	}
	want := []string{"analytics.DailySummary", "public.orders", "analytics.DailySummary",
		"analytics.DailySummary", "analytics.DailySummary", "analytics.old_summary"}
	for i, item := range items {
		if item.name != want[i] {
			t.Fatalf("occurrence %d = %q, want %q: %+v", i, item.name, want[i], items)
		}
	}
	if !items[0].declaration || !items[0].materialized || items[0].drop ||
		!items[2].requiresMaterialized || items[2].declaration || items[2].drop ||
		!items[4].drop || !items[4].requiresMaterialized ||
		!items[5].drop || !items[5].materialized {
		t.Fatalf("materialized relation lifecycle/refresh flags are wrong: %+v", items)
	}
	if selected, ok := sqlNavSelected(source, "postgres", 1,
		strings.Index(strings.Split(source, "\n")[1], "DailySummary")+2); !ok ||
		!selected.requiresMaterialized || selected.name != "analytics.DailySummary" {
		t.Fatalf("cannot select REFRESH view name: %+v, found=%v", selected, ok)
	}
	symbols := sqlDDLSymbols(source, "postgres")
	if len(symbols) != 1 || symbols[0].Name != "analytics.DailySummary" ||
		symbols[0].Kind != 11 || symbols[0].Detail != "CREATE MATERIALIZED VIEW (offline source)" {
		t.Fatalf("offline document outline missed materialized view: %+v", symbols)
	}
	encoded, err := appendSQLDDLSymbols([]byte("[]"), source, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	var outline []sqlDDLSymbol
	if err := json.Unmarshal(encoded, &outline); err != nil || len(outline) != 1 {
		t.Fatalf("invalid materialized source symbols: %s %v", encoded, err)
	}
	repeated, err := appendSQLDDLSymbols(encoded, source, "postgres")
	if err != nil || string(repeated) != string(encoded) {
		t.Fatalf("materialized symbols duplicated: %s, %v", repeated, err)
	}
}

func TestSQLPostgresMaterializedViewOptionalColumnsAndRefresh(t *testing.T) {
	// The CTE name shadows a relation. Navigation must expose the view
	// declaration without pretending the CTE is a persistent table.
	cte := "CREATE MATERIALIZED VIEW summary AS WITH orders AS (SELECT 1) SELECT * FROM orders;"
	if occ, ok := sqlNavOccurrences(cte, "postgres"); !ok || len(occ) != 1 ||
		!occ[0].declaration || occ[0].name != "summary" {
		t.Fatalf("materialized CTE shadowing invented relations: %+v", occ)
	}
	source := "CREATE MATERIALIZED VIEW \"Reports\".daily (total) AS SELECT count(*) FROM orders WITH NO DATA;\n" +
		"REFRESH MATERIALIZED VIEW \"Reports\".daily WITH NO DATA;\n"
	for _, dialect := range []string{"postgres", "postgresql"} {
		got, valid := sqlNavOccurrences(source, dialect)
		if !valid || len(got) != 3 ||
			got[0].name != "Reports.daily" || !got[0].declaration ||
			got[1].name != "orders" || got[2].name != "Reports.daily" || !got[2].requiresMaterialized {
			t.Fatalf("optional columns / refresh lost in %s: %+v", dialect, got)
		}
		symbols := sqlDDLSymbols(source, dialect)
		if len(symbols) != 1 || symbols[0].Name != "Reports.daily" {
			t.Fatalf("materialized outline lost in %s: %+v", dialect, symbols)
		}
	}
}

func TestSQLMaterializedViewUnsupportedSyntaxFailsClosed(t *testing.T) {
	unsupported := []string{
		"CREATE MATERIALIZED VIEW missing_as SELECT * FROM accounts;",
		"CREATE MATERIALIZED VIEW missing_query AS;",
		"CREATE MATERIALIZED VIEW incomplete AS SELECT;",
		"CREATE MATERIALIZED VIEW wrong_with AS WITH NO DATA;",
		"CREATE MATERIALIZED VIEW incomplete_cte AS WITH item AS (SELECT 1);",
		"CREATE MATERIALIZED VIEW unclosed_cte AS WITH item AS (SELECT 1;",
		"CREATE MATERIALIZED VIEW storage WITH (fillfactor=90) AS SELECT * FROM accounts;",
		"CREATE MATERIALIZED VIEW bad_columns (a,) AS SELECT * FROM accounts;",
		"CREATE OR REPLACE MATERIALIZED VIEW unsupported AS SELECT * FROM accounts;",
		"CREATE TEMP MATERIALIZED VIEW unsupported AS SELECT * FROM accounts;",
		"REFRESH MATERIALIZED VIEW CONCURRENTLY mv WITH NO DATA;",
		"REFRESH MATERIALIZED VIEW mv WITH IF DATA;",
		"DROP MATERIALIZED VIEW accounts,;",
		"DROP MATERIALIZED VIEW accounts INVALID;",
		"DROP MATERIALIZED VIEW CASCADE;",
	}
	for _, source := range unsupported {
		outline := sqlDDLSymbols(source, "postgres")
		occ, valid := sqlNavOccurrences(source, "postgres")
		if len(outline) != 0 || valid && len(occ) != 0 {
			t.Fatalf("unsupported PostgreSQL materialized syntax invented source truth: %q, symbols=%+v, occurrences=%+v", source, outline, occ)
		}
	}
	for _, dialect := range []string{"mysql", "mariadb"} {
		source := "CREATE MATERIALIZED VIEW native_only AS SELECT * FROM accounts;"
		if symbols := sqlDDLSymbols(source, dialect); len(symbols) != 0 {
			t.Fatalf("non-PostgreSQL materialized view was accepted as native DDL: %s %+v", dialect, symbols)
		}
		if occ, valid := sqlNavOccurrences(source, dialect); valid && len(occ) > 0 {
			t.Fatalf("non-PostgreSQL materialized source graph fabricated: %s %+v", dialect, occ)
		}
	}
	if old := sqlDDLSymbols("CREATE VIEW normal AS SELECT 1;", "mysql"); len(old) != 1 || old[0].Detail != "CREATE VIEW (offline source)" {
		t.Fatalf("ordinary views regressed: %+v", old)
	}
}

// Exercise actual bounded worker search, file-version checks and migration
// resolution without a database, SQL execution, or any installed-host changes.
func TestSQLMaterializedViewWorkerCrossFileHistory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("worker-only integration fixture")
	}
	requireWorkerLandlockV2(t)
	if !systemexec.Trusted("/usr/bin/rg") {
		t.Skip("trusted worker ripgrep unavailable")
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	helper := filepath.Join(t.TempDir(), "asa-materialized-source-helper")
	build := exec.Command("go", "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	files := map[string]string{
		"migrations/001_create.sql": "CREATE MATERIALIZED VIEW reporting.monthly AS SELECT * FROM accounts WITH NO DATA;\n",
		"queries/refresh.sql":       "REFRESH MATERIALIZED VIEW CONCURRENTLY reporting.monthly WITH DATA;\n",
		"queries/read.sql":          "SELECT * FROM reporting.monthly;\n",
	}
	for path, source := range files {
		full := filepath.Join(workspace, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	version := func(path string) string {
		t.Helper()
		stat := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: path})
		if !stat.OK || stat.FileVersion == "" {
			t.Fatalf("cannot stat %s: %+v", path, stat)
		}
		return stat.FileVersion
	}
	owner.Action, owner.Path, owner.Language, owner.SQLDialect = "code_inspect", "queries/refresh.sql", "sql", "postgres"
	owner.CodeMethod, owner.FileVersion, owner.Line = "definition", version(owner.Path), 0
	owner.Character = strings.Index(files[owner.Path], "monthly") + 2
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	get := func(method string) []sqlNavLocation {
		t.Helper()
		owner.CodeMethod = method
		response := s.codeInspect(ctx, owner)
		var locations []sqlNavLocation
		if !response.OK || json.Unmarshal([]byte(response.Output), &locations) != nil {
			t.Fatalf("materialized %s navigation failed: %+v", method, response)
		}
		return locations
	}
	definitions := get("definition")
	if len(definitions) != 1 || definitions[0].URI != "migrations/001_create.sql" ||
		definitions[0].FileVersion != version(definitions[0].URI) {
		t.Fatalf("wrong source-backed materialized declaration: %+v", definitions)
	}
	references := get("references")
	if len(references) != 3 {
		t.Fatalf("missing materialized cross-file uses: %+v", references)
	}
	seen := make(map[string]bool)
	for _, target := range references {
		if target.FileVersion != version(target.URI) {
			t.Fatalf("unstable target file version: %+v", target)
		}
		seen[target.URI] = true
	}
	for path := range files {
		if !seen[path] {
			t.Fatalf("missing %s from materialized source references: %+v", path, references)
		}
	}
	drop := "migrations/002_drop.sql"
	if err := os.WriteFile(filepath.Join(workspace, drop),
		[]byte("DROP MATERIALIZED VIEW reporting.monthly;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.CodeMethod = "definition"
	afterDrop := s.codeInspect(ctx, owner)
	if afterDrop.OK || afterDrop.ErrorCode != "sql_schema_history_ambiguous" {
		t.Fatalf("DROP failed to invalidate ordered materialized-view history: %+v", afterDrop)
	}
	if err := os.Remove(filepath.Join(workspace, drop)); err != nil {
		t.Fatal(err)
	}
	// An ordinary table declaration with the same name is NOT a valid
	// definition target for REFRESH MATERIALIZED VIEW.
	if err := os.WriteFile(filepath.Join(workspace, "migrations/001_create.sql"),
		[]byte("CREATE TABLE reporting.monthly (id INT);\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wrongKind := get("definition")
	if len(wrongKind) != 0 {
		t.Fatalf("REFRESH incorrectly linked to an ordinary table: %+v", wrongKind)
	}
}
