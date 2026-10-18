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
			{"only_multiple_add_then_rename", "ALTER TABLE ONLY ADD COLUMN detail INT, ADD COLUMN enabled BOOLEAN, RENAME TO archived;", "only", "archived"},
			{"only_drop_then_rename", "ALTER TABLE ONLY DROP COLUMN detail, RENAME TO archived;", "only", "archived"},
			{"ordinary_add_then_rename", "ALTER TABLE jobs ADD COLUMN detail INT, RENAME TO archived_jobs;", "jobs", "archived_jobs"},
			{"ordinary_rename_then_add", "ALTER TABLE jobs RENAME TO archived_jobs, ADD COLUMN detail INT;", "jobs", "archived_jobs"},
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
				if tc.name == "quoted_add_then_rename" {
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
