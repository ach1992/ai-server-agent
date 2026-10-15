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

func TestSQLColumnSourceGraphAndTombstones(t *testing.T) {
	source := "CREATE TABLE accounts (id BIGINT PRIMARY KEY, label TEXT, CONSTRAINT pk UNIQUE (id));\n" +
		"CREATE TABLE orders (id BIGINT, account_id BIGINT REFERENCES accounts(id));\n" +
		"ALTER TABLE accounts ADD COLUMN active BOOLEAN;\n" +
		"INSERT INTO accounts (id, label) VALUES (1, 'x');\n" +
		"ALTER TABLE accounts RENAME COLUMN label TO title;\n" +
		"ALTER TABLE accounts DROP COLUMN active;\n"
	all, valid := sqlNavAllOccurrences(source, "postgres")
	if !valid {
		t.Fatal("SQL column scan rejected valid source")
	}
	counts := make(map[string]int)
	var tombstones int
	for _, x := range all {
		if x.isColumn {
			counts[x.name]++
			if x.drop {
				tombstones++
			}
		}
	}
	for name, want := range map[string]int{"accounts.id": 4, "accounts.label": 3, "orders.id": 1,
		"orders.account_id": 1, "accounts.active": 2, "accounts.title": 1} {
		if counts[name] != want {
			t.Fatalf("column %q count %d not %d: %+v", name, counts[name], want, all)
		}
	}
	if tombstones != 3 {
		t.Fatalf("DROP/RENAME not tracked: %d", tombstones)
	}
	for _, sql := range []string{
		"ALTER TABLE accounts CHANGE old new BIGINT;",
		"ALTER TABLE accounts CHANGE COLUMN old new BIGINT;",
	} {
		events, ok := sqlNavAllOccurrences(sql, "mysql")
		if !ok || len(events) != 3 || !events[1].drop || !events[2].drop || events[1].name != "accounts.old" || events[2].name != "accounts.new" {
			t.Fatalf("MySQL CHANGE left ambiguous column identity live: %s %+v", sql, events)
		}
	}
	line := strings.Split(source, "\n")[1]
	column := strings.LastIndex(line, "accounts(id)") + len("accounts(")
	found, ok := sqlNavSelected(source, "postgres", 1, column)
	if !ok || !found.isColumn || found.name != "accounts.id" || found.declaration || found.drop {
		t.Fatalf("FK column selection failed: %+v %v", found, ok)
	}
}

func TestSQLColumnGraphRejectsUnsafeScope(t *testing.T) {
	for _, tc := range []struct{ sql, dialect string }{
		{"SELECT id FROM accounts;", "postgres"},
		{"SELECT x.id FROM accounts AS x;", "postgres"},
		{"WITH accounts AS (SELECT 1 AS id) SELECT id FROM accounts;", "postgres"},
		{"SELECT 'REFERENCES accounts(id)' AS s;", "postgres"},
		{"DO $tag$ ALTER TABLE accounts ADD COLUMN fake INT; $tag$;", "postgres"},
		{"ALTER TABLE accounts ADD CONSTRAINT pk PRIMARY KEY((id+1));", "postgres"},
		{"ALTER TABLE accounts ADD INDEX idx_id ((id+1));", "mysql"}, // expression indexes are not source-backed column identifiers
		{"SELECT E'quoted\\'; INSERT INTO accounts (id) VALUES (1);' AS s;", "postgres"},
	} {
		out, valid := sqlNavAllOccurrences(tc.sql, tc.dialect)
		if valid {
			for _, item := range out {
				if item.isColumn {
					t.Fatalf("fabricated SQL column: %q %+v", tc.sql, item)
				}
			}
		}
	}
}

func TestSQLColumnWorkerCrossFileVersionAndLifecycle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("worker-only fixture")
	}
	requireWorkerLandlockV2(t)
	if !systemexec.Trusted("/usr/bin/rg") {
		t.Skip("trusted rg not available")
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	helper := filepath.Join(t.TempDir(), "asa-sql-column-helper")
	build := exec.Command("go", "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	files := map[string]string{
		"migrations/001.sql": "CREATE TABLE accounts (id BIGINT PRIMARY KEY, label TEXT);\n",
		"migrations/002.sql": "CREATE TABLE orders (id BIGINT, account_id BIGINT REFERENCES accounts(id));\n",
		".hidden/003.sql":    "INSERT INTO accounts (id) VALUES (1);\n",
		"migrations/004.sql": "ALTER TABLE accounts ADD COLUMN active BOOLEAN;\n",
		"other.sql":          "CREATE TABLE payments (id BIGINT PRIMARY KEY);\n",
	}
	for name, sql := range files {
		full := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(sql), 0600); err != nil {
			t.Fatal(err)
		}
	}
	version := func(name string) string {
		t.Helper()
		got := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: name})
		if !got.OK || got.FileVersion == "" {
			t.Fatalf("stat %s: %+v", name, got)
		}
		return got.FileVersion
	}
	owner.Action, owner.Language, owner.SQLDialect = "code_inspect", "sql", "postgres"
	owner.CodeMethod, owner.Path, owner.Line = "definition", "migrations/002.sql", 0
	owner.Character = strings.Index(files[owner.Path], "accounts(id)") + len("accounts(")
	owner.FileVersion = version(owner.Path)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	def := s.codeInspect(ctx, owner)
	var targets []sqlNavLocation
	if !def.OK || json.Unmarshal([]byte(def.Output), &targets) != nil || len(targets) != 1 ||
		targets[0].URI != "migrations/001.sql" || targets[0].FileVersion != version(targets[0].URI) {
		t.Fatalf("wrong FK column definition: %+v %+v", def, targets)
	}
	owner.CodeMethod = "references"
	refs := s.codeInspect(ctx, owner)
	if !refs.OK || json.Unmarshal([]byte(refs.Output), &targets) != nil || len(targets) != 3 {
		t.Fatalf("CREATE/FK/hidden INSERT refs missing: %+v %+v", refs, targets)
	}
	for _, target := range targets {
		if target.URI == "other.sql" || target.FileVersion != version(target.URI) {
			t.Fatalf("cross-table col collision or unstable target: %+v", target)
		}
	}
	owner.CodeMethod = "definition"
	s.codeTargetSnapshotHook = func() {
		if err := os.WriteFile(filepath.Join(workspace, "migrations/001.sql"),
			[]byte(files["migrations/001.sql"]+"-- touched\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stale := s.codeInspect(ctx, owner)
	s.codeTargetSnapshotHook = nil
	if stale.OK || stale.ErrorCode != "sql_schema_target_changed" {
		t.Fatalf("source column target race accepted: %+v", stale)
	}
	owner.Path, owner.CodeMethod = "migrations/004.sql", "definition"
	owner.FileVersion = version(owner.Path)
	owner.Character = strings.Index(files[owner.Path], "active")
	added := s.codeInspect(ctx, owner)
	if !added.OK || json.Unmarshal([]byte(added.Output), &targets) != nil ||
		len(targets) != 1 || targets[0].URI != owner.Path {
		t.Fatalf("ALTER ADD COLUMN definition missing: %+v %+v", added, targets)
	}
	// Table removal never names the column, yet must poison live-schema claims.
	if err := os.WriteFile(filepath.Join(workspace, "migrations/005.sql"),
		[]byte("DROP TABLE accounts;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ambiguous := s.codeInspect(ctx, owner)
	if ambiguous.OK || ambiguous.ErrorCode != "sql_schema_history_ambiguous" {
		t.Fatalf("table DROP did not invalidate column: %+v", ambiguous)
	}
}
