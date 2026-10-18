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

// A relation move is a lifecycle transition, not a new CREATE or a harmless
// ALTER. A stale source-backed declaration must never be inferred as current.
func TestSQLPostgresOrdinaryRelationMoveOccurrences(t *testing.T) {
	cases := []struct {
		name, source, dialect string
		wantIdentity          string
		wantSelectable        bool
		wantMoved             bool
	}{
		{"table_rename", "ALTER TABLE reporting.events RENAME TO archived_events;", "postgres", "reporting\x00archived_events", true, true},
		{"view_rename", "ALTER VIEW reporting.events RENAME TO archived_events;", "postgres", "reporting\x00archived_events", true, true},
		{"schema_move", "ALTER TABLE reporting.events SET SCHEMA archive;", "postgres", "archive\x00events", false, true},
		{"view_schema_move", "ALTER VIEW reporting.events SET SCHEMA archive;", "postgresql", "archive\x00events", false, true},
		{"if_exists_schema_move", "ALTER TABLE IF EXISTS reporting.events SET SCHEMA archive;", "postgres", "archive\x00events", false, true},
		// PostgreSQL requires RENAME TO and SET SCHEMA without ONLY;
		// these deliberately malformed inputs are handled conservatively,
		// not asserted as PostgreSQL grammar acceptance.
		{"malformed_only_set_schema_conservative", "ALTER TABLE ONLY reporting.events SET SCHEMA archive;", "postgres", "archive\x00events", false, true},
		{"malformed_if_exists_only_set_schema_conservative", "ALTER TABLE IF EXISTS ONLY reporting.events SET SCHEMA archive;", "postgres", "archive\x00events", false, true},
		// Valid PostgreSQL ONLY subforms preserve the relation identity.
		{"valid_only_add_column", "ALTER TABLE ONLY reporting.events ADD COLUMN detail text;", "postgres", "", false, false},
		{"valid_if_exists_only_rename_column", "ALTER TABLE IF EXISTS ONLY reporting.events RENAME COLUMN old TO new;", "postgres", "", false, false},
		{"quoted_rename", `ALTER VIEW "reporting.data"."old.view" RENAME TO "new.view";`, "postgres", "reporting.data\x00new.view", true, true},
		{"quoted_schema_move", `ALTER TABLE "reporting.data"."old.view" SET SCHEMA "archive.data";`, "postgres", "archive.data\x00old.view", false, true},
		{"invalid_qualified_target", "ALTER TABLE reporting.events SET SCHEMA archive.other;", "postgres", "", false, true},
		{"invalid_qualified_rename", "ALTER TABLE reporting.events RENAME TO archive.other;", "postgres", "", false, true},
		{"missing_target", "ALTER TABLE reporting.events RENAME TO;", "postgres", "", false, true},
		{"column_rename_preserves_table", "ALTER TABLE reporting.events RENAME COLUMN id TO next_id;", "postgres", "", false, false},
		{"add_column_preserves_table", "ALTER TABLE reporting.events ADD COLUMN detail text;", "postgres", "", false, false},
		{"mysql_rename_unmodified", "ALTER TABLE reporting.events RENAME TO archived_events;", "mysql", "archived_events", true, true},
		{"mysql_unrecognized_schema_no_new_contract", "ALTER TABLE reporting.events SET SCHEMA archive;", "mysql", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			occurrences, valid := sqlNavOccurrences(tc.source, tc.dialect)
			if !valid {
				t.Fatalf("SQL tokenization refused ordinary statement %q", tc.source)
			}
			expected := 1
			if tc.wantIdentity != "" {
				expected = 2
			}
			if len(occurrences) != expected {
				t.Fatalf("wrong lifecycle item count for %q: %+v", tc.source, occurrences)
			}
			wantOld := "reporting\x00events"
			if tc.name == "quoted_rename" || tc.name == "quoted_schema_move" {
				wantOld = "reporting.data\x00old.view"
			}
			if occurrences[0].identity != wantOld ||
				occurrences[0].drop != tc.wantMoved || occurrences[0].declaration {
				t.Fatalf("stale original relation identity: %+v", occurrences[0])
			}
			if tc.wantIdentity == "" {
				return
			}
			target := occurrences[1]
			if !target.drop || target.declaration || target.identity != tc.wantIdentity ||
				target.lifecycleOnly == tc.wantSelectable {
				t.Fatalf("wrong target identity / selectable marker for %q: %+v", tc.source, target)
			}
		})
	}
}

// Prove PostgreSQL moves and MySQL/MariaDB compound table renames through
// the actual unprivileged worker and versioned source search, not token slices:
// a prior migration CREATE must not survive a later relation move.
func TestSQLPostgresOrdinaryMovesRealWorkerDefinition(t *testing.T) {
	// The portable unit suite can skip unavailable optional prerequisites;
	// the separate trusted-ripgrep CI acceptance must fail instead.
	required := os.Getenv("ASA_REQUIRE_SQL_ORDINARY_WORKER") == "1"
	if os.Geteuid() == 0 {
		if required {
			t.Fatal("required ordinary SQL worker acceptance must run unprivileged")
		}
		t.Skip("worker-only SQL integration requires an ordinary UID")
	}
	if required {
		abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0,
			uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
		if errno != 0 || abi < 2 {
			t.Fatalf("required ordinary SQL worker acceptance needs Landlock ABI v2: abi=%d errno=%v", abi, errno)
		}
	} else {
		requireWorkerLandlockV2(t)
	}
	// Real source search uses the operator-provisioned trusted ripgrep.
	if !systemexec.Trusted("/usr/bin/rg") {
		if required {
			t.Fatal("required ordinary SQL worker acceptance needs trusted /usr/bin/rg")
		}
		t.Skip("trusted worker ripgrep unavailable")
	}
	helper := filepath.Join(t.TempDir(), "agent-file-helper")
	build := exec.Command("go", "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test-owned worker helper: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 65*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name, create, move, dialect, oldName, newName string
		uncertain                                     bool // unsupported compound grammar must not prove an old CREATE
	}{
		{
			name: "table_set_schema", dialect: "postgres", oldName: "reporting.events", newName: "archive.events",
			create: "CREATE TABLE reporting.events (id int PRIMARY KEY);\n",
			move:   "ALTER TABLE reporting.events SET SCHEMA archive;\n",
		},
		{
			name: "view_rename", dialect: "postgres", oldName: "reporting.events", newName: "reporting.archived_events",
			create: "CREATE VIEW reporting.events AS SELECT 1 AS id;\n",
			move:   "ALTER VIEW reporting.events RENAME TO archived_events;\n",
		},
		{
			name: "table_rename", dialect: "postgres", oldName: "reporting.events", newName: "reporting.archived_events",
			create: "CREATE TABLE reporting.events (id int PRIMARY KEY);\n",
			move:   "ALTER TABLE reporting.events RENAME TO archived_events;\n",
		},
		{
			name: "mysql_only_add_then_rename", dialect: "mysql", oldName: "ONLY", newName: "archived",
			create: "CREATE TABLE ONLY (id INT);\n",
			move:   "ALTER TABLE ONLY ADD COLUMN detail INT, RENAME TO archived;\n",
		},
		{
			name: "mysql_jobs_rename_then_add", dialect: "mysql", oldName: "jobs", newName: "archived_jobs",
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs RENAME TO archived_jobs, ADD COLUMN detail INT;\n",
		},
		{
			name: "mysql_optional_direct_ordinary", dialect: "mysql", oldName: "jobs", newName: "archived_jobs",
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs RENAME archived_jobs;\n",
		},
		{
			name: "mysql_optional_add_then_rename", dialect: "mysql", oldName: "jobs", newName: "archived_jobs",
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs ADD COLUMN detail INT, RENAME archived_jobs;\n",
		},
		{
			name: "mariadb_optional_direct_only", dialect: "mariadb", oldName: "ONLY", newName: "archived",
			create: "CREATE TABLE ONLY (id INT);\n",
			move:   "ALTER TABLE ONLY RENAME archived;\n",
		},
		{
			name: "mariadb_optional_rename_then_add", dialect: "mariadb", oldName: "ONLY", newName: "archived",
			create: "CREATE TABLE ONLY (id INT);\n",
			move:   "ALTER TABLE ONLY RENAME archived, ADD COLUMN detail INT;\n",
		},
		{
			name: "mariadb_only_rename_then_add", dialect: "mariadb", oldName: "ONLY", newName: "archived",
			create: "CREATE TABLE ONLY (id INT);\n",
			move:   "ALTER TABLE ONLY RENAME TO archived, ADD COLUMN detail INT;\n",
		},
		{
			name: "mariadb_jobs_add_then_rename", dialect: "mariadb", oldName: "jobs", newName: "archived_jobs",
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs ADD COLUMN detail INT, RENAME TO archived_jobs;\n",
		},
		{
			name: "mysql_unknown_valid_compound", dialect: "mysql", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs ADD COLUMN detail INT NOT NULL, RENAME TO archived_jobs;\n",
		},
		{
			name: "mysql_decimal_invalid_uncertain", dialect: "mysql", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs ADD COLUMN amount DECIMAL(5,6), RENAME TO archived_jobs;\n",
		},
		{
			name: "mysql_reserved_target_uncertain", dialect: "mysql", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs RENAME SELECT;\n",
		},
		{
			name: "mysql_reserved_aux_column_uncertain", dialect: "mysql", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs ADD COLUMN SELECT INT, RENAME TO archived_jobs;\n",
		},
		{
			name: "mariadb_offset_target_uncertain", dialect: "mariadb", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs RENAME TO OFFSET;\n",
		},
		{
			name: "mariadb_system_add_uncertain", dialect: "mariadb", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs ADD SYSTEM VARCHAR(64), RENAME TO archived_jobs;\n",
		},
		{
			name: "mariadb_multi_rename_reserved_uncertain", dialect: "mariadb", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "RENAME TABLE jobs TO archived_jobs, extra TO ROW_NUMBER;\n",
		},
		{
			name: "mariadb_reserved_target_uncertain", dialect: "mariadb", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs RENAME SELECT;\n",
		},
		{
			name: "mariadb_reserved_aux_column_uncertain", dialect: "mariadb", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "ALTER TABLE jobs ADD COLUMN SELECT INT, RENAME TO archived_jobs;\n",
		},
		{
			name: "mysql_direct_rename_reserved_uncertain", dialect: "mysql", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "RENAME TABLE jobs TO SELECT;\n",
		},
		{
			name: "mariadb_direct_rename_partial_uncertain", dialect: "mariadb", oldName: "jobs", newName: "archived_jobs", uncertain: true,
			create: "CREATE TABLE jobs (id INT);\n",
			move:   "RENAME TABLE jobs TO archived_jobs, other TO SELECT;\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, owner, workspace := testStdioBroker(t)
			s.runs = newRunLimiterWith(2, 1)
			s.workspaceHelperBinary = helper
			dir := filepath.Join(workspace, tc.name)
			if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "queries"), 0700); err != nil {
				t.Fatal(err)
			}
			create := tc.name + "/migrations/001_create.sql"
			move := tc.name + "/migrations/002_move.sql"
			recreate := tc.name + "/migrations/003_recreate.sql"
			query := tc.name + "/queries/read.sql"
			write := func(path, source string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(workspace, path), []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(create, tc.create)
			queryBody := "SELECT * FROM " + tc.oldName + ";\n"
			write(query, queryBody)
			stat := func(path string) string {
				t.Helper()
				got := s.workerWorkspaceFile(ctx, Request{Action: "workspace_stat", Workspace: workspace, Path: path})
				if !got.OK || got.FileVersion == "" {
					t.Fatalf("worker file version unavailable for %s: %+v", path, got)
				}
				return got.FileVersion
			}
			definition := func() Response {
				t.Helper()
				req := owner
				req.Action, req.Workspace, req.Path = "code_inspect", workspace, query
				req.Language, req.SQLDialect, req.CodeMethod = "sql", tc.dialect, "definition"
				req.FileVersion, req.Line = stat(query), 0
				req.Character = len(queryBody) - len(";\n") - 1
				return s.codeInspect(ctx, req)
			}
			expectOne := func(want string) {
				t.Helper()
				got := definition()
				if !got.OK {
					t.Fatalf("current declaration not reachable: %+v", got)
				}
				var locations []sqlNavLocation
				if err := json.Unmarshal([]byte(got.Output), &locations); err != nil ||
					len(locations) != 1 || locations[0].URI != want ||
					locations[0].FileVersion != stat(want) {
					t.Fatalf("wrong worker-backed active SQL declaration: %+v / %+v (%v)", got, locations, err)
				}
			}
			expectOne(create)
			write(move, tc.move)
			stale := definition()
			if tc.uncertain {
				if stale.OK || stale.ErrorCode != "sql_schema_unparseable" {
					t.Fatalf("unknown compound ALTER falsely proved active history: %+v", stale)
				}
				return // no defined target or later migration may be inferred
			}
			if stale.OK || stale.ErrorCode != "sql_schema_history_ambiguous" {
				t.Fatalf("old identity resurrected after schema/rename move: %+v", stale)
			}
			// The moved name must not be silently connected to the old
			// qualified CREATE. Its original declaration is no longer a
			// proven active definition (no SQL migration execution here).
			queryBody = "SELECT * FROM " + tc.newName + ";\n"
			write(query, queryBody)
			moved := definition()
			if moved.OK || moved.ErrorCode != "sql_schema_history_ambiguous" {
				t.Fatalf("new relation identity was linked to stale CREATE: %+v", moved)
			}
			queryBody = "SELECT * FROM " + tc.oldName + ";\n"
			write(query, queryBody)
			write(recreate, tc.create)
			expectOne(recreate)
		})
	}
}

// A valid comma-separated ALTER TABLE rename must invalidate ordered source
// history regardless of clause order. An unsupported non-comma compound must
// not fabricate a lifecycle event (F1).
func TestSQLMySQLCompoundAlterTableRenames(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, tc := range []struct {
			name, source, oldName, newName string
		}{
			{"only_add_then_rename", "ALTER TABLE ONLY ADD COLUMN detail INT, RENAME TO archived;", "only", "archived"},
			{"only_rename_then_add", "ALTER TABLE ONLY RENAME TO archived, ADD COLUMN detail INT;", "only", "archived"},
			{"only_varchar_add_then_rename", "ALTER TABLE ONLY ADD COLUMN detail VARCHAR(64), RENAME TO archived;", "only", "archived"},
			{"only_decimal_add_then_rename", "ALTER TABLE ONLY ADD COLUMN price DECIMAL(10,2), RENAME TO archived;", "only", "archived"},
			{"only_decimal_one_arg", "ALTER TABLE ONLY ADD COLUMN price DECIMAL(65), RENAME TO archived;", "only", "archived"},
			{"only_decimal_max_scale", "ALTER TABLE ONLY ADD COLUMN price DECIMAL(65,30), RENAME TO archived;", "only", "archived"},
			{"only_numeric_add_then_rename", "ALTER TABLE ONLY ADD COLUMN price NUMERIC(10,2), RENAME TO archived;", "only", "archived"},
			{"only_multiple_add_then_rename", "ALTER TABLE ONLY ADD COLUMN detail INT, ADD COLUMN enabled BOOLEAN, RENAME TO archived;", "only", "archived"},
			{"only_drop_then_rename", "ALTER TABLE ONLY DROP COLUMN detail, RENAME TO archived;", "only", "archived"},
			{"ordinary_add_then_rename", "ALTER TABLE jobs ADD COLUMN detail INT, RENAME TO archived_jobs;", "jobs", "archived_jobs"},
			{"ordinary_rename_then_add", "ALTER TABLE jobs RENAME TO archived_jobs, ADD COLUMN detail INT;", "jobs", "archived_jobs"},
			{"optional_direct_ordinary", "ALTER TABLE jobs RENAME archived_jobs;", "jobs", "archived_jobs"},
			{"optional_direct_only", "ALTER TABLE ONLY RENAME archived;", "only", "archived"},
			{"optional_add_then_rename_ordinary", "ALTER TABLE jobs ADD COLUMN detail INT, RENAME archived_jobs;", "jobs", "archived_jobs"},
			{"optional_rename_then_add_ordinary", "ALTER TABLE jobs RENAME archived_jobs, ADD COLUMN detail INT;", "jobs", "archived_jobs"},
			{"optional_add_then_rename_only", "ALTER TABLE ONLY ADD COLUMN detail INT, RENAME archived;", "only", "archived"},
			{"optional_rename_then_add_only", "ALTER TABLE ONLY RENAME archived, ADD COLUMN detail INT;", "only", "archived"},
			{"optional_qualified_only", "ALTER TABLE staging.ONLY RENAME archived;", "staging\x00only", "archived"},
			{"optional_quoted_only", "ALTER TABLE `ONLY` RENAME `archived`;", "ONLY", "archived"},
			{"optional_quoted_keyword_target", "ALTER TABLE jobs RENAME `INDEX`;", "jobs", "INDEX"},
			{"qualified_add_then_rename", "ALTER TABLE staging.ONLY ADD COLUMN detail INT, RENAME TO archived;", "staging\x00only", "archived"},
			{"qualified_rename_then_add", "ALTER TABLE ONLY.events RENAME TO archive.events, ADD COLUMN detail INT;", "only\x00events", "archive\x00events"},
			{"quoted_add_then_rename", "ALTER TABLE `ONLY` ADD COLUMN detail INT, RENAME TO `archived`;", "ONLY", "archived"},
		} {
			t.Run(dialect+"_"+tc.name, func(t *testing.T) {
				got, valid := sqlNavOccurrences(tc.source, dialect)
				if !valid || len(got) != 2 {
					t.Fatalf("valid compound ALTER lost relation lifecycle: valid=%v occurrences=%+v", valid, got)
				}
				if got[0].identity != tc.oldName || got[1].identity != tc.newName ||
					!got[0].drop || !got[1].drop || got[0].declaration || got[1].declaration {
					t.Fatalf("incorrect compound move lifecycle: %+v", got)
				}
				createSQL := "CREATE TABLE " + strings.ReplaceAll(tc.oldName, "\x00", ".") + " (id INT);"
				if tc.oldName == "ONLY" {
					createSQL = "CREATE TABLE `ONLY` (id INT);"
				}
				create, ok := sqlNavOccurrences(createSQL, dialect)
				if !ok || len(create) != 1 || !create[0].declaration || create[0].identity != tc.oldName {
					t.Fatalf("matching source declaration unavailable for ordered proof: %+v", create)
				}
				items := []sqlNavFound{{sqlNavOccurrence: create[0], sqlNavLocation: sqlNavLocation{URI: "db/migrations/001_create.sql"}}}
				for _, ev := range got {
					items = append(items, sqlNavFound{sqlNavOccurrence: ev, sqlNavLocation: sqlNavLocation{URI: "db/migrations/002_move.sql"}})
				}
				if location, err := sqlNavResolveOrderedDefinition(items[:2], nil, "db/queries/read.sql"); err == nil {
					t.Fatalf("obsolete CREATE considered active after compound ALTER: %+v", location)
				}
			})
		}
	}
}

// RENAME COLUMN and RENAME INDEX/KEY never move the containing table.
// Quoted target names that spell grammar keywords are still identifiers.
func TestSQLMySQLRenameDisambiguation(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, source := range []string{
			"ALTER TABLE jobs RENAME COLUMN old_name TO new_name;",
			"ALTER TABLE jobs RENAME INDEX old_index TO new_index;",
			"ALTER TABLE jobs RENAME KEY old_index TO new_index;",
		} {
			t.Run(dialect+"_"+source, func(t *testing.T) {
				occurrences, valid := sqlNavOccurrences(source, dialect)
				if !valid || len(occurrences) != 1 || occurrences[0].drop || occurrences[0].identity != "jobs" {
					t.Fatalf("column/index rename invented table move: %+v valid=%t", occurrences, valid)
				}
			})
		}
		sql := "ALTER TABLE jobs RENAME `INDEX`;"
		occurrences, valid := sqlNavOccurrences(sql, dialect)
		if !valid || len(occurrences) != 2 || !occurrences[0].drop || !occurrences[1].drop || occurrences[1].identity != "INDEX" {
			t.Fatalf("quoted target keyword lost real table rename: %+v valid=%t", occurrences, valid)
		}
	}
}

// Reserved bare SQL words are never sufficient proof of a table move.  A
// malformed ALTER in a later migration must poison ordered source history,
// rather than emit fake tombstones or resurrect the earlier CREATE.
func TestSQLMySQLReservedAlterNamesFailClosed(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, statement := range []string{
			"ALTER TABLE jobs RENAME SELECT;",
			"ALTER TABLE jobs RENAME TO SELECT;",
			"ALTER TABLE jobs ADD COLUMN SELECT INT, RENAME TO archived_jobs;",
			"ALTER TABLE jobs RENAME TO archived_jobs, ADD COLUMN SELECT INT;",
			"ALTER TABLE jobs DROP COLUMN FROM, RENAME TO archived_jobs;",
			"ALTER TABLE jobs RENAME TO archived_jobs, DROP COLUMN GROUP;",
			"ALTER TABLE SELECT RENAME TO archived_jobs;",
			"ALTER TABLE jobs RENAME WHERE;",
		} {
			t.Run(dialect+"/"+statement, func(t *testing.T) {
				for _, source := range []string{statement, "CREATE TABLE jobs (id INT);\n" + statement} {
					items, valid := sqlNavOccurrences(source, dialect)
					if valid || len(items) != 0 {
						t.Fatalf("reserved unquoted name certified lifecycle: valid=%t events=%+v", valid, items)
					}
					columns, allValid := sqlNavAllOccurrences(source, dialect)
					if allValid || len(columns) != 0 {
						t.Fatalf("reserved unquoted name certified column or table history: valid=%t events=%+v", allValid, columns)
					}
				}
			})
		}
	}
}

// Quoting and qualification make reserved words legal identifiers; ONLY and
// other nonreserved words can also be legal names, subject to the separate
// conservative statement-tokenizer safety rules. Do not reject valid lifecycle history
// with a blanket keyword filter.
func TestSQLMySQLReservedAlterNamesValidContexts(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, tc := range []struct{ statement, target string }{
			{"ALTER TABLE jobs RENAME `SELECT`;", "SELECT"},
			{"ALTER TABLE jobs RENAME TO archive.SELECT;", "archive\x00select"},
			{"ALTER TABLE jobs ADD COLUMN `SELECT` INT, RENAME TO archived_jobs;", "archived_jobs"},
			{"ALTER TABLE jobs RENAME TO archived_jobs, ADD COLUMN `SELECT` INT;", "archived_jobs"},
			{"ALTER TABLE jobs RENAME ONLY;", "only"},
			{"ALTER TABLE jobs ADD COLUMN ONLY INT, RENAME TO archived_jobs;", "archived_jobs"},
			{"ALTER TABLE archive.SELECT RENAME TO archived_jobs;", "archived_jobs"},
		} {
			t.Run(dialect+"/"+tc.statement, func(t *testing.T) {
				events, valid := sqlNavOccurrences(tc.statement, dialect)
				if !valid || len(events) != 2 || !events[0].drop || !events[1].drop ||
					events[1].identity != tc.target {
					t.Fatalf("legal quoted/qualified/nonreserved identity lost: valid=%t events=%+v", valid, events)
				}
			})
		}
	}
}

// The same reserved-name boundary applies to direct RENAME TABLE and CREATE/
// DROP declarations; a malformed later pair must not leave partial tombstones.
func TestSQLMySQLReservedDirectLifecycleNames(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, statement := range []string{
			"RENAME TABLE jobs TO SELECT;",
			"RENAME TABLE SELECT TO archived_jobs;",
			"RENAME TABLE jobs TO archived_jobs, extra TO SELECT;",
			"RENAME TABLE jobs TO archived_jobs, extra TO;",
		} {
			t.Run(dialect+"/"+statement, func(t *testing.T) {
				got, valid := sqlNavOccurrences("CREATE TABLE jobs (id INT);\n"+statement, dialect)
				if valid || len(got) != 0 {
					t.Fatalf("invalid RENAME TABLE must poison source graph atomically: valid=%t got=%+v", valid, got)
				}
			})
		}
		for _, statement := range []string{
			"CREATE TABLE SELECT (id INT);",
			"DROP TABLE SELECT;",
			"DROP TABLE jobs, SELECT;",
		} {
			t.Run(dialect+"/"+statement, func(t *testing.T) {
				got, _ := sqlNavOccurrences(statement, dialect)
				if len(got) != 0 {
					t.Fatalf("reserved unquoted name fabricated CREATE/DROP lifecycle: %+v", got)
				}
			})
		}
		for _, tc := range []struct{ statement, oldName, newName string }{
			{"RENAME TABLE jobs TO __BT__SELECT__BT__;", "jobs", "SELECT"},
			{"RENAME TABLE jobs TO archive.SELECT;", "jobs", "archive\x00select"},
			{"RENAME TABLE ONLY TO archived;", "only", "archived"},
			{"RENAME TABLE jobs TO archived_jobs, oldtable TO nexttable;", "jobs", "archived_jobs"},
		} {
			t.Run(dialect+"/"+tc.statement, func(t *testing.T) {
				statement := strings.ReplaceAll(tc.statement, "__BT__", string(rune(96)))
				got, valid := sqlNavOccurrences(statement, dialect)
				if !valid || len(got) < 2 || got[0].identity != tc.oldName ||
					got[1].identity != tc.newName || !got[0].drop || !got[1].drop {
					t.Fatalf("legitimate direct RENAME TABLE lost: valid=%t got=%+v", valid, got)
				}
			})
		}
	}
}

// MariaDB's default-mode reserved set differs from MySQL 8.4 and contains
// version-qualified tokens. The offline parser uses the conservative modern
// MariaDB subset: never certify an invalid later rename or DROP as a move.
func TestSQLMariaDBReservedAndContextualNames(t *testing.T) {
	for _, statement := range []string{
		"ALTER TABLE jobs RENAME TO OFFSET;",
		"ALTER TABLE jobs RENAME TO ROW_NUMBER;",
		"ALTER TABLE jobs RENAME TO SQL_AFTER_GTIDS;",
		"ALTER TABLE jobs RENAME TO SQL_BEFORE_GTIDS;",
		"ALTER TABLE jobs ADD COLUMN OFFSET INT, RENAME TO archived_jobs;",
		"ALTER TABLE jobs RENAME TO archived_jobs, ADD COLUMN ROW_NUMBER INT;",
		"ALTER TABLE jobs ADD SYSTEM VARCHAR(64), RENAME TO archived_jobs;",
		"ALTER TABLE jobs RENAME TO archived_jobs, ADD SYSTEM VARCHAR(64);",
		"RENAME TABLE jobs TO ROW_NUMBER;",
		"RENAME TABLE jobs TO archived_jobs, extra TO OFFSET;",
	} {
		t.Run("invalid/"+statement, func(t *testing.T) {
			for _, source := range []string{statement, "CREATE TABLE jobs (id INT);\n" + statement} {
				items, valid := sqlNavOccurrences(source, "mariadb")
				if valid || len(items) != 0 {
					t.Fatalf("invalid MariaDB statement certified source history: valid=%t events=%+v", valid, items)
				}
			}
		})
	}
	for _, statement := range []string{
		"DROP TABLE ROW_NUMBER;",
		"DROP TABLE jobs, OFFSET;",
		"CREATE TABLE OFFSET (id INT);",
	} {
		t.Run("invalid_direct/"+statement, func(t *testing.T) {
			items, _ := sqlNavOccurrences(statement, "mariadb")
			if len(items) != 0 {
				t.Fatalf("invalid MariaDB direct identifier fabricated lifecycle: %+v", items)
			}
		})
	}
	for _, tc := range []struct{ source, identity string }{
		{"CREATE TABLE jobs (id INT);\nDROP TABLE jobs, OFFSET;", "jobs"},
		{"CREATE TABLE `ROW_NUMBER` (id INT);\nDROP TABLE ROW_NUMBER;", "ROW_NUMBER"},
	} {
		t.Run("invalid_drop_preserves_create/"+tc.identity, func(t *testing.T) {
			items, valid := sqlNavOccurrences(tc.source, "mariadb")
			if !valid || len(items) != 1 || !items[0].declaration || items[0].drop ||
				items[0].identity != tc.identity {
				t.Fatalf("invalid DROP poisoned a previous legal CREATE: valid=%t events=%+v", valid, items)
			}
		})
	}
	for _, statement := range []string{
		"ALTER TABLE jobs RENAME TO DATABASE;",
		"ALTER TABLE jobs RENAME TO WINDOW;",
		"ALTER TABLE jobs ADD COLUMN SYSTEM INT, RENAME TO archived_jobs;",
	} {
		t.Run("mysql_reserved_unchanged/"+statement, func(t *testing.T) {
			items, valid := sqlNavOccurrences(statement, "mysql")
			if valid || len(items) != 0 {
				t.Fatalf("MySQL 8.4 must keep rejecting its reserved names: valid=%t events=%+v", valid, items)
			}
		})
	}
	for _, tc := range []struct{ statement, target string }{
		{"ALTER TABLE jobs RENAME TO DATABASE;", "database"},
		{"ALTER TABLE jobs RENAME TO WINDOW;", "window"},
		{"ALTER TABLE jobs ADD COLUMN DATABASE INT, RENAME TO archived_jobs;", "archived_jobs"},
		{"ALTER TABLE jobs ADD COLUMN WINDOW INT, RENAME TO archived_jobs;", "archived_jobs"},
		{"ALTER TABLE jobs RENAME TO `ROW_NUMBER`;", "ROW_NUMBER"},
		{"ALTER TABLE jobs ADD COLUMN `SYSTEM` VARCHAR(64), RENAME TO archived_jobs;", "archived_jobs"},
		{"ALTER TABLE jobs ADD COLUMN SYSTEM VARCHAR(64), RENAME TO archived_jobs;", "archived_jobs"},
		{"ALTER TABLE jobs RENAME TO archive.OFFSET;", "archive\x00offset"},
		{"RENAME TABLE jobs TO archive.ROW_NUMBER;", "archive\x00row_number"},
	} {
		t.Run("valid/"+tc.statement, func(t *testing.T) {
			items, valid := sqlNavOccurrences(tc.statement, "mariadb")
			if !valid || len(items) != 2 || !items[0].drop || !items[1].drop || items[1].identity != tc.target {
				t.Fatalf("legal MariaDB identity was rejected: valid=%t events=%+v", valid, items)
			}
		})
	}
	for _, statement := range []string{
		"CREATE TABLE DATABASE (id INT);",
		"CREATE TABLE WINDOW (id INT);",
		"CREATE TABLE jobs (system INT);",
		"DROP TABLE WINDOW;",
	} {
		t.Run("valid_direct/"+statement, func(t *testing.T) {
			items, valid := sqlNavOccurrences(statement, "mariadb")
			if !valid || len(items) != 1 {
				t.Fatalf("legal MariaDB declaration lost: valid=%t events=%+v", valid, items)
			}
		})
	}
}

// MySQL's optional AS and MariaDB's optional TO are not interchangeable.
// Likewise MariaDB DROP COLUMN RESTRICT is not a MySQL ALTER TABLE clause.
func TestSQLMySQLMariaDBRenameGrammarBoundaries(t *testing.T) {
	for _, tc := range []struct {
		dialect, source string
		validMove       bool
	}{
		{"mysql", "ALTER TABLE jobs RENAME AS archived_jobs;", true},
		{"mariadb", "ALTER TABLE jobs RENAME AS archived_jobs;", false},
		{"mysql", "ALTER TABLE jobs DROP COLUMN detail RESTRICT, RENAME archived_jobs;", false},
		{"mariadb", "ALTER TABLE jobs DROP COLUMN detail RESTRICT, RENAME archived_jobs;", true},
	} {
		t.Run(tc.dialect+"_"+tc.source, func(t *testing.T) {
			occurrences, valid := sqlNavOccurrences(tc.source, tc.dialect)
			if tc.validMove {
				if !valid || len(occurrences) != 2 || !occurrences[0].drop || !occurrences[1].drop ||
					occurrences[0].identity != "jobs" || occurrences[1].identity != "archived_jobs" {
					t.Fatalf("valid dialect-specific move missing: valid=%t occurrences=%+v", valid, occurrences)
				}
			} else if valid || len(occurrences) != 0 {
				t.Fatalf("unsupported dialect-specific syntax fabricated lifecycle: valid=%t occurrences=%+v", valid, occurrences)
			}
		})
	}
}

// The offline supplement is intentionally not a complete MariaDB/MySQL
// ALTER grammar. Valid rename-bearing statements beyond its bounded clause
// support must fail closed instead of resurrecting an earlier migration's
// CREATE as a proven current table.
func TestSQLMySQLUnknownCompoundRenameDoesNotProveHistory(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, statement := range []string{
			"ALTER TABLE jobs ADD COLUMN detail INT NOT NULL, RENAME TO archived_jobs;",
			"ALTER TABLE ONLY ADD INDEX idx_id (id), RENAME TO archived;",
			"ALTER TABLE jobs RENAME TO archived_jobs, MODIFY COLUMN id BIGINT;",
			"ALTER TABLE ONLY RENAME TO archived, ADD COLUMN detail INT NOT NULL;",
			"ALTER TABLE ONLY ADD COLUMN IF NOT EXISTS detail INT, RENAME TO archived;",
		} {
			t.Run(dialect+"_"+statement, func(t *testing.T) {
				events, valid := sqlNavOccurrences(statement, dialect)
				if valid || len(events) != 0 {
					t.Fatalf("unknown compound ALTER should fail closed, not invent partial lifecycle: valid=%t events=%+v", valid, events)
				}
			})
		}
	}
}

// An unsupported dialect/action must not turn PostgreSQL-specific ONLY into
// a fictitious ALTER/DROP of an unrelated source-backed relation.
func TestSQLOnlyDialectBoundariesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, sql, dialect string
	}{
		{"mysql_alter", "ALTER TABLE ONLY reporting.events RENAME TO archived_events;", "mysql"},
		{"mysql_alter_if_exists_unsupported", "ALTER TABLE IF EXISTS ONLY ADD COLUMN detail INT, RENAME TO archived;", "mysql"},
		{"mariadb_alter", "ALTER TABLE ONLY reporting.events RENAME AS archived_events;", "mariadb"},
		{"mysql_alter_column", "ALTER TABLE ONLY reporting.events ADD COLUMN payload INT;", "mysql"},
		{"mariadb_alter_column", "ALTER TABLE ONLY reporting.events DROP COLUMN payload;", "mariadb"},
		{"mysql_drop", "DROP TABLE ONLY reporting.events;", "mysql"},
		{"mariadb_drop", "DROP VIEW ONLY reporting.events;", "mariadb"},
		{"postgres_drop", "DROP TABLE ONLY reporting.events;", "postgres"},
		{"postgres_view_only", "ALTER VIEW ONLY reporting.events RENAME TO archive;", "postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, ok := sqlNavOccurrences(tc.sql, tc.dialect)
			if !ok {
				return // safe parse rejection is acceptable
			}
			if len(items) != 0 {
				t.Fatalf("unsupported ONLY syntax invented source lifecycle %q: %+v", tc.sql, items)
			}
			// Column navigation must not silently recover an invalid table header.
			columns, ok := sqlNavAllOccurrences(tc.sql, tc.dialect)
			if ok && len(columns) != 0 {
				t.Fatalf("unsupported ONLY syntax invented column sources %q: %+v", tc.sql, columns)
			}
		})
	}
}

// Malformed qualified ONLY must not be silently reduced to the unqualified
// table name, nor may an invalid later item leave earlier DROP tombstones.
// This is a source-navigation correctness boundary: no SQL is executed.
func TestSQLMySQLOnlyMalformedLifecycleFailClosed(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, tc := range []struct {
			name, statement string
		}{
			{"drop_table_missing_qualifier", "DROP TABLE ONLY.;"},
			{"drop_view_missing_qualifier", "DROP VIEW ONLY.;"},
			{"alter_rename_missing_source_qualifier", "ALTER TABLE ONLY. RENAME TO archived;"},
			{"drop_list_late_invalid_only_modifier", "DROP TABLE ONLY, archive, ONLY reporting.events;"},
			{"drop_list_late_incomplete_qualifier", "DROP TABLE ONLY, archive, ONLY.;"},
			{"drop_list_late_trailing_comma", "DROP TABLE ONLY, archive,;"},
			{"alter_rename_missing_target", "ALTER TABLE ONLY RENAME TO;"},
			{"alter_rename_incomplete_target", "ALTER TABLE ONLY RENAME TO archived.;"},
			{"alter_rename_extra_tokens", "ALTER TABLE ONLY RENAME TO archived garbage;"},
			{"alter_rename_embedded_unvalidated_spec", "ALTER TABLE ONLY ADD COLUMN detail INT RENAME TO archived;"},
			{"alter_rename_qualified_source_trailing_dot", "ALTER TABLE ONLY.events. RENAME TO archived;"},
			{"drop_list_malformed_second_only", "DROP TABLE archive, ONLY reporting.events;"},
			{"drop_list_malformed_qualified", "DROP TABLE archive, ONLY.events.;"},
			{"drop_only_three_part_qualifier", "DROP TABLE ONLY.events.archive;"},
			{"alter_only_three_part_qualifier", "ALTER TABLE ONLY.events.archive RENAME TO archived;"},
			{"drop_list_late_three_part_qualifier", "DROP TABLE archive, ONLY.events.archive;"},
			{"drop_qualified_only_three_part", "DROP TABLE staging.ONLY.extra;"},
			{"alter_qualified_only_three_part", "ALTER TABLE staging.ONLY.RENAME TO archived;"},
			{"alter_rename_three_part_target", "ALTER TABLE ONLY RENAME TO archive.events.extra;"},
			{"alter_qualified_source_three_part_target", "ALTER TABLE staging.ONLY RENAME TO archive.events.extra;"},
			{"alter_rename_invalid_varchar_arity", "ALTER TABLE ONLY ADD COLUMN detail VARCHAR(64,2), RENAME TO archived;"},
			{"alter_rename_invalid_decimal_arity", "ALTER TABLE ONLY ADD COLUMN amount DECIMAL(10,2,3), RENAME TO archived;"},
			{"invalid_decimal_scale_gt_precision", "ALTER TABLE jobs ADD COLUMN amount DECIMAL(5,6), RENAME TO archive;"},
			{"invalid_decimal_precision_zero", "ALTER TABLE jobs ADD COLUMN amount DECIMAL(0,0), RENAME TO archive;"},
			{"invalid_decimal_precision_over_max", "ALTER TABLE jobs ADD COLUMN amount DECIMAL(66,1), RENAME TO archive;"},
			{"invalid_decimal_scale_over_max", "ALTER TABLE jobs ADD COLUMN amount DECIMAL(65,31), RENAME TO archive;"},
			{"invalid_numeric_scale", "ALTER TABLE jobs ADD COLUMN amount NUMERIC(5,6), RENAME TO archive;"},
			{"invalid_varchar_zero", "ALTER TABLE jobs ADD COLUMN detail VARCHAR(0), RENAME TO archive;"},
			{"invalid_char_length", "ALTER TABLE jobs ADD COLUMN detail CHAR(256), RENAME TO archive;"},
			{"invalid_binary_length", "ALTER TABLE jobs ADD COLUMN detail BINARY(256), RENAME TO archive;"},
			{"invalid_float_args_unverified", "ALTER TABLE jobs ADD COLUMN detail FLOAT(999999,999), RENAME TO archive;"},
		} {
			t.Run(dialect+"_"+tc.name, func(t *testing.T) {
				items, parsed := sqlNavOccurrences(tc.statement, dialect)
				if parsed && len(items) != 0 {
					t.Fatalf("malformed lifecycle fabricated relation occurrences: %q: %+v", tc.statement, items)
				}
				all, parsed := sqlNavAllOccurrences(tc.statement, dialect)
				if parsed && len(all) != 0 {
					t.Fatalf("malformed lifecycle fabricated navigation/columns: %q: %+v", tc.statement, all)
				}
			})
		}
	}
}

// ONLY is not reserved in MySQL/MariaDB; unlike PostgreSQL's inheritance
// modifier, an unquoted ONLY can be a real schema, table or view identifier.
// The adjacent invalid modifier spellings above must still fail closed.
func TestSQLMySQLBareOnlyIdentifiers(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, tc := range []struct {
			name, source string
			want         []string
		}{
			{"rename_table", "ALTER TABLE ONLY RENAME TO archived;", []string{"only", "archived"}},
			{"rename_qualified_table", "ALTER TABLE staging.ONLY RENAME TO archived;", []string{"staging\x00only", "archived"}},
			{"rename_only_schema", "ALTER TABLE ONLY.events RENAME TO archived;", []string{"only\x00events", "archived"}},
			{"rename_quoted_only", "ALTER TABLE `ONLY` RENAME TO `archived`;", []string{"ONLY", "archived"}},
			{"rename_qualified_target", "ALTER TABLE ONLY RENAME TO archive.events;", []string{"only", "archive\x00events"}},
			{"rename_only_schema_quoted_table", "ALTER TABLE ONLY.`events` RENAME TO archived;", []string{"only\x00events", "archived"}},
			{"drop_table", "DROP TABLE ONLY;", []string{"only"}},
			{"drop_view", "DROP VIEW ONLY;", []string{"only"}},
			{"drop_only_schema", "DROP TABLE ONLY.events;", []string{"only\x00events"}},
			{"drop_qualified_view", "DROP VIEW ONLY.events;", []string{"only\x00events"}},
			{"drop_list", "DROP TABLE ONLY, archive;", []string{"only", "archive"}},
			{"drop_list_qualified", "DROP TABLE ONLY, archive, ONLY.events;", []string{"only", "archive", "only\x00events"}},
			{"drop_list_restrict", "DROP TABLE ONLY, archive RESTRICT;", []string{"only", "archive"}},
			{"drop_quoted_only", "DROP TABLE `ONLY`, archive;", []string{"ONLY", "archive"}},
			{"alter_add_column", "ALTER TABLE ONLY ADD COLUMN value INT;", []string{"only"}},
			{"alter_rename_column", "ALTER TABLE ONLY RENAME COLUMN value TO next_value;", []string{"only"}},
		} {
			t.Run(dialect+"_"+tc.name, func(t *testing.T) {
				items, ok := sqlNavOccurrences(tc.source, dialect)
				if !ok || len(items) != len(tc.want) {
					t.Fatalf("offline identifier navigation invalid: ok=%v items=%+v want=%v", ok, items, tc.want)
				}
				for i, want := range tc.want {
					if items[i].identity != want {
						t.Fatalf("wrong identifier at %d: %+v; want %q", i, items[i], want)
					}
					if (tc.name != "alter_add_column" && tc.name != "alter_rename_column") && !items[i].drop {
						t.Fatalf("relation lifecycle failed closed incorrectly: %+v", items)
					}
				}
			})
		}
		for _, tc := range []struct {
			source string
			want   []string
		}{
			{"ALTER TABLE ONLY ADD COLUMN value INT;", []string{"only", "only\x00column\x00value"}},
			{"ALTER TABLE ONLY RENAME COLUMN value TO next_value;", []string{"only", "only\x00column\x00value", "only\x00column\x00next_value"}},
		} {
			t.Run(dialect+"_columns_"+tc.source, func(t *testing.T) {
				items, ok := sqlNavAllOccurrences(tc.source, dialect)
				if !ok || len(items) != len(tc.want) {
					t.Fatalf("column identities wrong: ok=%v got=%+v want=%v", ok, items, tc.want)
				}
				for i, want := range tc.want {
					if items[i].identity != want {
						t.Fatalf("wrong identity %d: %+v; want %q", i, items[i], want)
					}
				}
			})
		}
	}
}
