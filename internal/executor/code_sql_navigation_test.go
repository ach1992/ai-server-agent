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

func TestSQLSourceNavigationGrammarAndSafeContexts(t *testing.T) {
	source := "-- REFERENCES forged\n" +
		"SELECT E'ignored\\'; SELECT * FROM forged;' AS literal;\n" +
		"CREATE TABLE public.accounts (id INT, CONSTRAINT fk FOREIGN KEY (id) REFERENCES public.parents(id));\n" +
		"ALTER TABLE public.accounts ADD COLUMN enabled BOOLEAN;\n" +
		"INSERT INTO public.accounts (id) VALUES (1);\n" +
		"SELECT a.id FROM public.accounts a JOIN public.parents p ON a.id = p.id;\n" +
		"COPY raw FROM STDIN;\n" +
		"SELECT * FROM forged;\n" +
		"\\.\n"
	got, valid := sqlNavOccurrences(source, "postgres")
	if !valid || len(got) != 6 {
		t.Fatalf("wrong SQL source graph; valid=%v occurrences=%+v", valid, got)
	}
	want := []string{"public.accounts", "public.parents", "public.accounts",
		"public.accounts", "public.accounts", "public.parents"}
	// The CREATE target and reference, ALTER, INSERT, SELECT two sources.
	if len(got) != len(want) {
		t.Fatalf("false SQL navigation symbols, including literal/COPY data: %+v", got)
	}
	for i, name := range want {
		if got[i].name != name {
			t.Fatalf("SQL occurrence %d name=%q, want=%q; all=%+v", i, got[i].name, name, got)
		}
	}
	if !got[0].declaration || got[2].declaration || got[2].drop {
		t.Fatalf("CREATE vs ALTER semantics lost: %+v", got)
	}
	for _, source := range []string{
		"DO $code$ SELECT * FROM forged; $code$;",
		"WITH accounts AS (SELECT 1) SELECT * FROM accounts;",
		"CREATE TABLE unclosed (id INT;",
		"SELECT * FROM 'accounts';",
	} {
		occ, valid := sqlNavOccurrences(source, "postgres")
		if valid && len(occ) != 0 {
			t.Fatalf("unsupported/unsafe query produced false table references: %q %+v", source, occ)
		}
	}
	for _, rename := range []string{
		"ALTER TABLE accounts RENAME TO account_archive;",
		"RENAME TABLE accounts TO account_archive;",
	} {
		items, valid := sqlNavOccurrences(rename, "mysql")
		if !valid || len(items) != 2 || !items[0].drop || items[0].name != "accounts" || !items[1].drop || items[1].name != "account_archive" {
			t.Fatalf("schema rename must invalidate naive declaration linking: %q %+v", rename, items)
		}
	}
	for _, sql := range []string{
		"CREATE TABLE Orders (id INT); SELECT * FROM orders;",
		"CREATE TABLE `orders` (id INT); SELECT * FROM `orders`;",
	} {
		occ, valid := sqlNavOccurrences(sql, "mysql")
		if !valid || len(occ) != 2 || occ[0].name != "orders" || occ[1].name != "orders" {
			t.Fatalf("MySQL quoted/unquoted table navigation lost: %q %+v", sql, occ)
		}
	}
}

func TestSQLSourceNavigationQuotedCaseAndUTF16(t *testing.T) {
	source := "CREATE TABLE \"Orders\" (id INT);\nCREATE TABLE orders (id INT);\n" +
		"SELECT * FROM \"Orders\"; -- 😀 prefix\n"
	got, valid := sqlNavOccurrences(source, "postgres")
	if !valid || len(got) != 3 || got[0].name != "Orders" ||
		got[1].name != "orders" || got[2].name != "Orders" {
		t.Fatalf("quoted PostgreSQL names were conflated: %+v", got)
	}
	if chosen, found := sqlNavSelected(source, "postgres", 2, 17); !found || chosen.name != "Orders" {
		t.Fatalf("quoted name not selected: %+v %v", chosen, found)
	}
	emoji := "SELECT '😀'; SELECT * FROM accounts;"
	pos := strings.Index(emoji, "accounts")
	// Emoji takes two UTF-16 code units, one more than its rune count.
	if _, found := sqlNavSelected(emoji, "postgres", 0, pos-2); !found {
		t.Fatal("UTF-16 character indexing failed")
	}
	if _, found := sqlNavSelected("SELECT 'accounts';", "postgres", 0, 9); found {
		t.Fatal("literal turned into table reference")
	}
	qualified := "SELECT * FROM public.orders;"
	if _, found := sqlNavSelected(qualified, "postgres", 0, 15); found {
		t.Fatal("schema prefix was misreported as a table")
	}
	if item, found := sqlNavSelected(qualified, "postgres", 0, 23); !found || item.name != "public.orders" {
		t.Fatalf("qualified table lookup failed: %+v %v", item, found)
	}
	if items, valid := sqlNavOccurrences("CREATE VIEW daily AS WITH orders AS (SELECT 1) SELECT * FROM orders;", "postgres"); !valid || len(items) != 1 || !items[0].declaration {
		t.Fatalf("CTE local name fabricated a table reference: %+v", items)
	}
	if items, valid := sqlNavOccurrences("DELIMITER //\nCREATE PROCEDURE demo() BEGIN SELECT 1; CREATE TABLE phantom (id INT); END //", "mysql"); valid || len(items) != 0 {
		t.Fatalf("MySQL custom DELIMITER invented SQL declarations: %+v", items)
	}
}

func TestSQLSourceNavigationWorkerCrossFileVersionsAndHistory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("worker-only integration fixture requires ordinary UID")
	}
	requireWorkerLandlockV2(t)
	if !systemexec.Trusted("/usr/bin/rg") {
		t.Skip("trusted worker ripgrep unavailable")
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	binary := filepath.Join(t.TempDir(), "agent-workspace-helper")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v: %s", err, out)
	}
	s.workspaceHelperBinary = binary
	files := map[string]string{
		"migrations/001_accounts.sql":          "CREATE TABLE accounts (id BIGINT PRIMARY KEY);\n",
		"migrations/002_payments.sql":          "CREATE TABLE payments (account_id BIGINT REFERENCES accounts(id));\nALTER TABLE accounts ADD COLUMN active BOOLEAN;\n",
		"queries/read.sql":                     "SELECT a.id FROM accounts a JOIN payments p ON a.id = p.account_id;\n",
		"migrations/004_upper.SQL":             "INSERT INTO accounts (id) VALUES (2);\n",
		"migrations/.hidden/005_private.sql":   "INSERT INTO accounts (id) VALUES (3);\n",
		"migrations/.hidden/.git/internal.sql": "CREATE TABLE accounts (id INT);\n",
		".gitignore":                           "migrations/004_upper.SQL\n",
		"migrations/ignored.sql":               "-- CREATE TABLE accounts (id INT);\nSELECT 'FROM accounts' AS ignored;\n",
	}
	for name, data := range files {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	external := filepath.Join(t.TempDir(), "leak.sql")
	if err := os.WriteFile(external, []byte("CREATE TABLE accounts (secret INT);"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(workspace, "migrations", "outbound.sql")); err != nil {
		t.Fatal(err)
	}
	version := func(name string) string {
		t.Helper()
		got := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: name})
		if !got.OK {
			t.Fatalf("stat %s: %+v", name, got)
		}
		return got.FileVersion
	}
	owner.Action, owner.Path, owner.Language, owner.SQLDialect = "code_inspect", "migrations/002_payments.sql", "sql", "postgres"
	owner.CodeMethod, owner.FileVersion, owner.Line, owner.Character = "definition", version(owner.Path), 1, 13
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	def := s.codeInspect(ctx, owner)
	if !def.OK {
		t.Fatalf("source-only cross-file definition failed: %+v", def)
	}
	var targets []sqlNavLocation
	if err := json.Unmarshal([]byte(def.Output), &targets); err != nil || len(targets) != 1 ||
		targets[0].URI != "migrations/001_accounts.sql" ||
		targets[0].FileVersion != version(targets[0].URI) {
		t.Fatalf("wrong versioned CREATE source: %+v (%v)", def, err)
	}
	owner.CodeMethod = "references"
	refs := s.codeInspect(ctx, owner)
	if !refs.OK {
		t.Fatalf("source-only cross-file references failed: %+v", refs)
	}
	if err := json.Unmarshal([]byte(refs.Output), &targets); err != nil || len(targets) != 6 {
		t.Fatalf("wrong migration references: %+v (%v)", refs, err)
	}
	wantPaths := []string{"migrations/.hidden/005_private.sql", "migrations/001_accounts.sql", "migrations/002_payments.sql", "migrations/002_payments.sql", "migrations/004_upper.SQL", "queries/read.sql"}
	for i, target := range targets {
		if target.URI != wantPaths[i] || target.FileVersion != version(target.URI) {
			t.Fatalf("unstable/wrong reference %d: %+v", i, target)
		}
	}
	old := owner.FileVersion
	if err := os.WriteFile(filepath.Join(workspace, owner.Path), []byte(files[owner.Path]+"-- changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if stale := s.codeInspect(ctx, owner); stale.OK || stale.ErrorCode != "file_changed" {
		t.Fatalf("accepted stale SQL source: %+v (old=%s)", stale, old)
	}
	owner.FileVersion = version(owner.Path)
	owner.CodeMethod = "definition"
	s.codeTargetSnapshotHook = func() {
		if err := os.WriteFile(filepath.Join(workspace, "migrations/001_accounts.sql"),
			[]byte("CREATE TABLE accounts (id BIGINT PRIMARY KEY, altered INT);\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	changed := s.codeInspect(ctx, owner)
	s.codeTargetSnapshotHook = nil
	if changed.OK || changed.ErrorCode != "sql_schema_target_changed" {
		t.Fatalf("changed definition file was accepted without version conflict: %+v", changed)
	}
	if err := os.WriteFile(filepath.Join(workspace, "migrations/003_drop.sql"),
		[]byte("DROP TABLE accounts;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.CodeMethod = "definition"
	if ambiguous := s.codeInspect(ctx, owner); ambiguous.OK || ambiguous.ErrorCode != "sql_schema_history_ambiguous" {
		t.Fatalf("a DROP was silently treated as still-active schema: %+v", ambiguous)
	}
}

func TestSQLSourceNavigationLimitsAndDuplicateDeclarations(t *testing.T) {
	source := "CREATE TABLE accounts (id INT);\nCREATE TABLE accounts (id INT);\n"
	got, valid := sqlNavOccurrences(source, "postgres")
	if !valid || len(got) != 2 || !got[0].declaration || !got[1].declaration {
		t.Fatalf("recreated table must retain distinct definitions: %+v", got)
	}
}

func TestSQLSourceNavigationQuotedSchemaIdentityAndMultitargetHistory(t *testing.T) {
	source := `CREATE TABLE "public.accounts" (id INT);
CREATE TABLE public.accounts (id INT);
SELECT * FROM "public.accounts";
`
	occ, valid := sqlNavOccurrences(source, "postgres")
	if !valid || len(occ) != 3 || occ[0].identity == occ[1].identity ||
		occ[0].identity != occ[2].identity {
		t.Fatalf("quoted dotted identifier conflated with schema-qualified name: %+v", occ)
	}
	quotedUnicode := `CREATE TABLE "درخت😀" (id INT);
SELECT * FROM "درخت😀";`
	occ, valid = sqlNavOccurrences(quotedUnicode, "postgres")
	if !valid || len(occ) != 2 || occ[0].identity != occ[1].identity ||
		occ[0].searchTerm != "درخت😀" {
		t.Fatalf("quoted UTF-8 SQL identifiers unavailable: %+v", occ)
	}
	pgLowercase := `CREATE TABLE accounts (id INT);
SELECT * FROM "accounts";`
	occ, valid = sqlNavOccurrences(pgLowercase, "postgres")
	if !valid || len(occ) != 2 || occ[0].identity != occ[1].identity {
		t.Fatalf("quoted lowercase vs unquoted Postgres table identity broken: %+v", occ)
	}
	mysql := "CREATE TABLE \u0060order-item\u0060 (id INT); SELECT * FROM \u0060order-item\u0060;"
	occ, valid = sqlNavOccurrences(mysql, "mysql")
	if !valid || len(occ) != 2 || occ[0].identity != occ[1].identity {
		t.Fatalf("MySQL quoted hyphen name unavailable: %+v", occ)
	}
	for _, tc := range []struct {
		sql   string
		names []string
	}{
		{"DROP TABLE IF EXISTS accounts, archive;", []string{"accounts", "archive"}},
		{"DROP VIEW public.first, public.second;", []string{"public.first", "public.second"}},
		{"RENAME TABLE old TO new, second TO third;", []string{"old", "new", "second", "third"}},
		{"ALTER TABLE records RENAME TO archived;", []string{"records", "archived"}},
	} {
		got, valid := sqlNavOccurrences(tc.sql, "mysql")
		if !valid || len(got) != len(tc.names) {
			t.Fatalf("SQL schema-history changes lost: %q %+v", tc.sql, got)
		}
		for i, name := range tc.names {
			if got[i].name != name || !got[i].drop {
				t.Fatalf("SQL rename/drop %q incorrect item %d: %+v", tc.sql, i, got[i])
			}
		}
	}
	columnRename, valid := sqlNavOccurrences("ALTER TABLE records RENAME COLUMN old TO new;", "postgres")
	if !valid || len(columnRename) != 1 || columnRename[0].name != "records" || columnRename[0].drop {
		t.Fatalf("column rename must not tombstone entire table: %+v", columnRename)
	}
}
