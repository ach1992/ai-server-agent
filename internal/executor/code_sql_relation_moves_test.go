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
		{"only_schema_move", "ALTER TABLE ONLY reporting.events SET SCHEMA archive;", "postgres", "archive\x00events", false, true},
		{"if_exists_only_schema_move", "ALTER TABLE IF EXISTS ONLY reporting.events SET SCHEMA archive;", "postgres", "archive\x00events", false, true},
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

// Prove through the actual unprivileged worker and versioned source search,
// not only token slices: a move cannot resurrect an obsolete migration.
func TestSQLPostgresOrdinaryMovesRealWorkerDefinition(t *testing.T) {
	requireWorkerLandlockV2(t)
	helper := filepath.Join(t.TempDir(), "agent-file-helper")
	build := exec.Command("go", "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test-owned worker helper: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 65*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name, create, move string
	}{
		{
			name:   "table_set_schema",
			create: "CREATE TABLE reporting.events (id int PRIMARY KEY);\n",
			move:   "ALTER TABLE reporting.events SET SCHEMA archive;\n",
		},
		{
			name:   "view_rename",
			create: "CREATE VIEW reporting.events AS SELECT 1 AS id;\n",
			move:   "ALTER VIEW reporting.events RENAME TO archived_events;\n",
		},
		{
			name:   "table_rename",
			create: "CREATE TABLE reporting.events (id int PRIMARY KEY);\n",
			move:   "ALTER TABLE reporting.events RENAME TO archived_events;\n",
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
			queryBody := "SELECT * FROM reporting.events;\n"
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
				req.Language, req.SQLDialect, req.CodeMethod = "sql", "postgres", "definition"
				req.FileVersion, req.Line = stat(query), 0
				req.Character = strings.Index(queryBody, "events") + 2
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
			if stale.OK || stale.ErrorCode != "sql_schema_history_ambiguous" {
				t.Fatalf("old identity resurrected after schema/rename move: %+v", stale)
			}
			// The moved name must not be silently connected to the old
			// qualified CREATE. Its original declaration is no longer a
			// proven active definition (no SQL migration execution here).
			newRelation := "reporting.archived_events"
			if tc.name == "table_set_schema" {
				newRelation = "archive.events"
			}
			queryBody = "SELECT * FROM " + newRelation + ";\\n"
			write(query, queryBody)
			moved := definition()
			if moved.OK || moved.ErrorCode != "sql_schema_history_ambiguous" {
				t.Fatalf("new relation identity was linked to stale CREATE: %+v", moved)
			}
			queryBody = "SELECT * FROM reporting.events;\\n"
			write(query, queryBody)
			write(recreate, tc.create)
			expectOne(recreate)
		})
	}
}
