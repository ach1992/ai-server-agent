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
	"golang.org/x/sys/unix"
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
	// A valid CTE is not a proven bounded SELECT query body. Retain
	// document outline syntax separately, but do not synthesize a
	// physical VIEW declaration or falsely link its shadowed CTE name.
	cte := "CREATE MATERIALIZED VIEW summary AS WITH orders AS (SELECT 1) SELECT * FROM orders;"
	if occ, ok := sqlNavOccurrences(cte, "postgres"); ok && len(occ) != 0 {
		t.Fatalf("unproven materialized CTE emitted physical identity: %+v", occ)
	}
	if symbols := sqlDDLSymbols(cte, "postgres"); len(symbols) != 1 ||
		symbols[0].Name != "summary" {
		t.Fatalf("valid materialized CTE lost non-binding document outline: %+v", symbols)
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
	// The ordinary local suite may skip when no real worker sandbox is
	// available. CI's named acceptance step must instead fail, not give
	// the appearance of a successful integration proof after SKIP.
	required := os.Getenv("ASA_REQUIRE_SQL_MATERIALIZED_WORKER") == "1"
	if os.Geteuid() == 0 {
		if required {
			t.Fatal("required SQL worker acceptance must run unprivileged")
		}
		t.Skip("worker-only integration fixture")
	}
	if required {
		abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0,
			uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
		if errno != 0 || abi < 2 {
			t.Fatalf("required SQL worker acceptance needs Landlock ABI v2: abi=%d errno=%v", abi, errno)
		}
	} else {
		requireWorkerLandlockV2(t)
	}
	if !systemexec.Trusted("/usr/bin/rg") {
		if required {
			t.Fatal("required SQL worker acceptance needs trusted /usr/bin/rg")
		}
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
	dropRefs := get("references")
	if len(dropRefs) != 4 {
		t.Fatalf("genuine materialized DROP must remain in references: %+v", dropRefs)
	}
	sawMaterializedDrop := false
	for _, ref := range dropRefs {
		sawMaterializedDrop = sawMaterializedDrop || ref.URI == drop
	}
	if !sawMaterializedDrop {
		t.Fatalf("missing genuine DROP MATERIALIZED VIEW reference: %+v", dropRefs)
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
	// R1: prove the reverse navigation direction through actual worker search.
	writeSQL := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	selectSQL := func(path, source, term, method string) {
		t.Helper()
		idx := strings.Index(source, term)
		if idx < 0 {
			t.Fatalf("missing %q in %q", term, source)
		}
		owner.Path, owner.FileVersion = path, version(path)
		owner.CodeMethod, owner.Line, owner.Character = method, 0, idx+1
	}
	ambiguous := func(label string) {
		t.Helper()
		response := s.codeInspect(ctx, owner)
		if response.OK || response.ErrorCode != "sql_schema_history_ambiguous" {
			t.Fatalf("%s: source history was incorrectly resolved: %+v", label, response)
		}
	}
	for _, ordinary := range []string{
		"CREATE TABLE reporting.monthly (id integer);\n",
		"CREATE VIEW reporting.monthly AS SELECT 1;\n",
	} {
		writeSQL("migrations/001_create.sql", ordinary)
		selectSQL("migrations/001_create.sql", ordinary, "monthly", "references")
		refs := get("references")
		if len(refs) != 2 {
			t.Fatalf("%q: ordinary declaration should have only itself and SELECT, got %+v", ordinary, refs)
		}
		for _, ref := range refs {
			if ref.URI == "queries/refresh.sql" || ref.FileVersion != version(ref.URI) {
				t.Fatalf("%q: wrong-kind REFRESH included: %+v", ordinary, refs)
			}
		}
		sameKindDrop := "DROP VIEW reporting.monthly;\n"
		if strings.Contains(ordinary, "TABLE") {
			sameKindDrop = "DROP TABLE reporting.monthly;\n"
		}
		writeSQL("queries/ordinary_drop.sql", sameKindDrop)
		selectSQL("migrations/001_create.sql", ordinary, "monthly", "references")
		sameKindRefs := get("references")
		if len(sameKindRefs) != 3 {
			t.Fatalf("%q: ordinary same-kind DROP not preserved: %+v", ordinary, sameKindRefs)
		}
		for _, ref := range sameKindRefs {
			if ref.URI == "queries/refresh.sql" {
				t.Fatalf("%q: materialized REFRESH joined ordinary graph: %+v", ordinary, sameKindRefs)
			}
		}
		if err := os.Remove(filepath.Join(workspace, "queries/ordinary_drop.sql")); err != nil {
			t.Fatal(err)
		}
		// A materialized-only DROP conflicts with an ordinary source
		// declaration; do not quietly mix typed lifecycle histories.
		writeSQL("queries/wrong_kind_drop.sql", "DROP MATERIALIZED VIEW reporting.monthly;\n")
		selectSQL("migrations/001_create.sql", ordinary, "monthly", "references")
		ambiguous("ordinary declaration with materialized DROP")
		if err := os.Remove(filepath.Join(workspace, "queries/wrong_kind_drop.sql")); err != nil {
			t.Fatal(err)
		}
		selectSQL("queries/refresh.sql", files["queries/refresh.sql"], "monthly", "definition")
		if got := get("definition"); len(got) != 0 {
			t.Fatalf("%q: REFRESH linked to ordinary declaration: %+v", ordinary, got)
		}
	}
	mv := "CREATE MATERIALIZED VIEW reporting.monthly AS SELECT 1 AS id;\n"
	writeSQL("migrations/001_create.sql", mv)
	selectSQL("migrations/001_create.sql", mv, "monthly", "references")
	if refs := get("references"); len(refs) != 3 {
		t.Fatalf("real materialized declaration lost REFRESH/SELECT references: %+v", refs)
	}
	selectSQL("queries/refresh.sql", files["queries/refresh.sql"], "monthly", "definition")
	if def := get("definition"); len(def) != 1 || def[0].URI != "migrations/001_create.sql" {
		t.Fatalf("real materialized CREATE not linked: %+v", def)
	}
	// Both directions must reject conflicting ordinary and materialized
	// declarations of the same qualified name.
	conflict := "migrations/002_conflict.sql"
	writeSQL(conflict, "CREATE TABLE reporting.monthly (id integer);\n")
	ambiguous("refresh with mixed-kind declarations")
	selectSQL("migrations/001_create.sql", mv, "monthly", "references")
	ambiguous("materialized declaration with mixed-kind declarations")
	if err := os.Remove(filepath.Join(workspace, conflict)); err != nil {
		t.Fatal(err)
	}
	// R2-1: DROP TABLE/VIEW are typed operations and cannot become
	// references to a materialized CREATE, even if their names match.
	for _, wrongDrop := range []string{
		"DROP VIEW reporting.monthly;\n",
		"DROP TABLE reporting.monthly;\n",
	} {
		wrongPath := "queries/wrong_kind_drop.sql"
		writeSQL(wrongPath, wrongDrop)
		selectSQL("migrations/001_create.sql", mv, "monthly", "references")
		ambiguous("materialized CREATE with " + strings.TrimSpace(wrongDrop))
		selectSQL("queries/refresh.sql", files["queries/refresh.sql"], "monthly", "definition")
		ambiguous("materialized REFRESH with " + strings.TrimSpace(wrongDrop))
		if err := os.Remove(filepath.Join(workspace, wrongPath)); err != nil {
			t.Fatal(err)
		}
	}

	// R2: the source-only resolver must not resurrect a CREATE after a
	// relation identity changes in a later ordered migration.
	alterPath := "migrations/002_alter.sql"
	for _, tc := range []struct {
		label, alter, newName string
	}{
		{"rename", "ALTER MATERIALIZED VIEW reporting.monthly RENAME TO renamed;\n", "reporting.renamed"},
		{"set-schema", "ALTER MATERIALIZED VIEW reporting.monthly SET SCHEMA archive;\n", "archive.monthly"},
		{"unknown", "ALTER MATERIALIZED VIEW reporting.monthly IDENTITY UNKNOWN;\n", ""},
		{"incomplete-rename", "ALTER MATERIALIZED VIEW reporting.monthly RENAME TO;\n", ""},
	} {
		writeSQL(alterPath, tc.alter)
		selectSQL("queries/refresh.sql", files["queries/refresh.sql"], "monthly", "definition")
		ambiguous(tc.label + " obsolete source")
		if tc.newName != "" {
			target := "queries/new_materialized.sql"
			newQuery := "REFRESH MATERIALIZED VIEW " + tc.newName + ";\n"
			writeSQL(target, newQuery)
			suffix := tc.newName[strings.LastIndex(tc.newName, ".")+1:]
			selectSQL(target, newQuery, suffix, "definition")
			response := s.codeInspect(ctx, owner)
			if response.OK {
				var locations []sqlNavLocation
				if json.Unmarshal([]byte(response.Output), &locations) != nil || len(locations) != 0 {
					t.Fatalf("%s new identity fabricated old CREATE: %+v", tc.label, response)
				}
			} else if response.ErrorCode != "sql_schema_history_ambiguous" {
				t.Fatalf("%s new identity unexpected error: %+v", tc.label, response)
			}
			if err := os.Remove(filepath.Join(workspace, target)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(filepath.Join(workspace, alterPath)); err != nil {
			t.Fatal(err)
		}
		selectSQL("queries/refresh.sql", files["queries/refresh.sql"], "monthly", "definition")
		if def := get("definition"); len(def) != 1 || def[0].URI != "migrations/001_create.sql" {
			t.Fatalf("%s: valid definition not restored after ALTER removal: %+v", tc.label, def)
		}
	}
	// R2-2: the same materialized relation survives a column rename.
	// Only the column's identity changes; RENAME TO of the relation above
	// must continue to invalidate its previous identity.
	for _, columnRename := range []string{
		"ALTER MATERIALIZED VIEW reporting.monthly RENAME COLUMN id TO account_id;\n",
		"ALTER MATERIALIZED VIEW reporting.monthly RENAME id TO account_id;\n",
		"ALTER MATERIALIZED VIEW IF EXISTS reporting.monthly RENAME COLUMN id TO account_id;\n",
	} {
		writeSQL(alterPath, columnRename)
		selectSQL("queries/refresh.sql", files["queries/refresh.sql"], "monthly", "definition")
		definitions := get("definition")
		if len(definitions) != 1 || definitions[0].URI != "migrations/001_create.sql" {
			t.Fatalf("column rename invalidated materialized relation identity: %q => %+v", columnRename, definitions)
		}
		selectSQL("migrations/001_create.sql", mv, "monthly", "references")
		refs := get("references")
		if len(refs) != 3 {
			t.Fatalf("column rename changed relation references: %q => %+v", columnRename, refs)
		}
		if err := os.Remove(filepath.Join(workspace, alterPath)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSQLMaterializedViewPopulationSuffixMustBeTerminal(t *testing.T) {
	valid := []string{
		"CREATE MATERIALIZED VIEW reporting.good AS SELECT 1 WITH DATA;",
		"CREATE MATERIALIZED VIEW reporting.good AS SELECT * FROM orders WITH NO DATA;",
		"CREATE MATERIALIZED VIEW reporting.good AS TABLE orders WITH NO DATA;",
		"CREATE MATERIALIZED VIEW reporting.good AS VALUES (1) WITH DATA;",
		"CREATE MATERIALIZED VIEW reporting.good AS WITH cte AS (SELECT 1) SELECT * FROM cte WITH NO DATA;",
		"CREATE MATERIALIZED VIEW reporting.good AS SELECT * FROM generate_series(1, 3) WITH ORDINALITY;",
		"CREATE MATERIALIZED VIEW reporting.good AS SELECT 1 ORDER BY 1 FETCH FIRST 1 ROWS WITH TIES;",
	}
	for _, source := range valid {
		symbols := sqlDDLSymbols(source, "postgres")
		if len(symbols) != 1 || symbols[0].Name != "reporting.good" {
			t.Errorf("valid materialized population clause was rejected: %q => %+v", source, symbols)
		}
		found, ok := sqlNavOccurrences(source, "postgres")
		// Physical navigation is a stronger contract than document syntax.
		// CTE, TABLE, VALUES, table functions and complex ORDER/FETCH bodies
		// remain valid SQL but cannot prove a complete bounded source scope.
		proven := source == valid[0] || source == valid[1]
		if proven {
			if !ok || len(found) == 0 || !found[0].declaration || !found[0].materialized {
				t.Errorf("proven materialized source declaration not indexed: %q => %+v", source, found)
			}
		} else if ok && len(found) != 0 {
			t.Errorf("unproven valid materialized SQL fabricated source navigation: %q => %+v", source, found)
		}
	}
	invalid := []string{
		"CREATE MATERIALIZED VIEW reporting.bad AS SELECT 1 WITH DATA EXTRA;",
		"CREATE MATERIALIZED VIEW reporting.bad AS SELECT * FROM orders WITH NO DATA EXTRA;",
		"CREATE MATERIALIZED VIEW reporting.bad AS SELECT 1 WITH NO;",
		"CREATE MATERIALIZED VIEW reporting.bad AS SELECT 1 WITH;",
		"CREATE MATERIALIZED VIEW reporting.bad AS SELECT 1 WITH DATA WITH NO DATA;",
		"CREATE MATERIALIZED VIEW reporting.bad AS SELECT 1 WITH NO DATA WITH DATA;",
		"CREATE MATERIALIZED VIEW reporting.bad AS TABLE orders WITH NO DATA INVALID;",
		"CREATE MATERIALIZED VIEW reporting.bad AS VALUES (1) WITH DATA MORE;",
		"CREATE MATERIALIZED VIEW reporting.bad AS WITH cte AS (SELECT 1) SELECT * FROM cte WITH NO DATA EXTRA;",
	}
	for _, source := range invalid {
		if symbols := sqlDDLSymbols(source, "postgres"); len(symbols) != 0 {
			t.Errorf("malformed population suffix produced a symbol: %q => %+v", source, symbols)
		}
		if found, ok := sqlNavOccurrences(source, "postgres"); !ok || len(found) != 0 {
			t.Errorf("malformed materialized DDL produced source links: %q => %+v valid=%v", source, found, ok)
		}
	}
}

func TestSQLMaterializedAlterInvalidatesSourceIdentity(t *testing.T) {
	cases := []struct {
		sql       string
		wantNames []string
	}{
		{"ALTER MATERIALIZED VIEW reporting.old_mv RENAME TO new_mv;",
			[]string{"reporting.old_mv", "reporting.new_mv"}},
		{"ALTER MATERIALIZED VIEW IF EXISTS reporting.old_mv SET SCHEMA archive;",
			[]string{"reporting.old_mv", "archive.old_mv"}},
		{`ALTER MATERIALIZED VIEW "reporting.data"."old.mv" RENAME TO "new.mv";`,
			[]string{"reporting.data.old.mv", "reporting.data.new.mv"}},
		{`ALTER MATERIALIZED VIEW "reporting.data"."old.mv" SET SCHEMA "archive.data";`,
			[]string{"reporting.data.old.mv", "archive.data.old.mv"}},
		{"ALTER MATERIALIZED VIEW reporting.old_mv RENAME TO;",
			[]string{"reporting.old_mv"}},
		{"ALTER MATERIALIZED VIEW reporting.old_mv IDENTITY UNKNOWN;",
			[]string{"reporting.old_mv"}},
		{"ALTER MATERIALIZED VIEW reporting.old_mv RENAME COLUMN old_id TO new_id;", nil},
		{"ALTER MATERIALIZED VIEW reporting.old_mv RENAME old_id TO new_id;", nil},
		{"ALTER MATERIALIZED VIEW IF EXISTS reporting.old_mv RENAME COLUMN old_id TO new_id;", nil},
		{`ALTER MATERIALIZED VIEW reporting.old_mv RENAME COLUMN "old.id" TO "new.id";`, nil},
		{"ALTER MATERIALIZED VIEW reporting.old_mv RENAME COLUMN old_id TO;",
			[]string{"reporting.old_mv"}},
		{"ALTER MATERIALIZED VIEW reporting.old_mv RENAME old_id TO new_id EXTRA;",
			[]string{"reporting.old_mv"}},
		{"ALTER MATERIALIZED VIEW reporting.old_mv OWNER TO custodian;", nil},
		{"ALTER MATERIALIZED VIEW reporting.old_mv SET TABLESPACE fast_space;", nil},
		{"ALTER MATERIALIZED VIEW reporting.old_mv SET (fillfactor = 70);", nil},
	}
	for _, tc := range cases {
		events, valid := sqlNavOccurrences(tc.sql, "postgres")
		if !valid || len(events) != len(tc.wantNames) {
			t.Errorf("alter %q: got %+v (valid=%v), want %v", tc.sql, events, valid, tc.wantNames)
			continue
		}
		for i, want := range tc.wantNames {
			if events[i].name != want || !events[i].drop || !events[i].materialized ||
				!events[i].requiresMaterialized || events[i].declaration {
				t.Errorf("alter %q: bad lifecycle marker at %d: %+v, want %s", tc.sql, i, events[i], want)
			}
		}
	}
	if events, ok := sqlNavOccurrences("ALTER MATERIALIZED VIEW reporting.old_mv RENAME TO new_mv;", "mysql"); ok && len(events) > 0 {
		t.Errorf("MySQL fabricated ALTER MATERIALIZED VIEW support: %+v", events)
	}
	move := "ALTER MATERIALIZED VIEW reporting.old_mv SET SCHEMA archive;"
	if selected, ok := sqlNavSelected(move, "postgres", 0, strings.Index(move, "archive")+2); ok {
		t.Errorf("synthetic SET SCHEMA target incorrectly exposed as selectable object: %+v", selected)
	}
	rename := "ALTER MATERIALIZED VIEW reporting.old_mv RENAME TO new_mv;"
	if selected, ok := sqlNavSelected(rename, "postgres", 0, strings.Index(rename, "new_mv")+2); !ok ||
		selected.name != "reporting.new_mv" || !selected.drop {
		t.Errorf("genuine RENAME TO target cannot be selected: %+v (found=%t)", selected, ok)
	}
}
