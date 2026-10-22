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

// Actual unprivileged Landlocked Worker search: cross-file definition and
// references with version pinning. No database, host package install or
// privileged service mutation is required.
func TestSQLDirectDMLWorkerCrossFileNavigation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires unprivileged worker")
	}
	requireWorkerLandlockV2(t)
	if !systemexec.Trusted("/usr/bin/rg") {
		t.Skip("requires trusted system ripgrep")
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	helper := filepath.Join(t.TempDir(), "asa-sql-dml-helper")
	build := exec.Command("go", "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("worker helper: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	files := map[string]string{
		"migrations/001_accounts.sql": "CREATE TABLE accounts (id BIGINT PRIMARY KEY, label TEXT, active BOOLEAN);\n",
		"migrations/002_update.sql":   "UPDATE accounts SET label=id WHERE id=7;\n",
		"migrations/003_delete.sql":   "DELETE FROM accounts WHERE active IS NOT NULL;\n",
		"migrations/004_noise.sql":    "CREATE TABLE invoices (id BIGINT, label TEXT, active BOOLEAN);\n",
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
		r := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: path})
		if !r.OK || r.FileVersion == "" {
			t.Fatalf("stat %s: %+v", path, r)
		}
		return r.FileVersion
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	owner.Action, owner.Language, owner.SQLDialect = "code_inspect", "sql", "postgres"
	call := func(method, path string, at int, pinned string) Response {
		req := owner
		req.CodeMethod, req.Path, req.FileVersion = method, path, pinned
		req.Line, req.Character = 0, at
		return s.codeInspect(ctx, req)
	}
	locations := func(r Response, label string) []map[string]any {
		t.Helper()
		var locs []map[string]any
		if !r.OK || json.Unmarshal([]byte(r.Output), &locs) != nil {
			t.Fatalf("%s source navigation failed: %+v", label, r)
		}
		for _, loc := range locs {
			path, _ := loc["uri"].(string)
			pin, _ := loc["file_version"].(string)
			if pin == "" || path == "" || pin != version(path) {
				t.Fatalf("%s lacks versioned source proof: %+v", label, loc)
			}
			if path == "migrations/004_noise.sql" {
				t.Fatalf("%s borrowed a same-named column from another table: %+v", label, loc)
			}
		}
		return locs
	}
	for _, tc := range []struct {
		path, selected, column string
		references             int
	}{
		{"migrations/002_update.sql", "label=", "label", 2},
		{"migrations/002_update.sql", "=id", "id", 3},
		{"migrations/002_update.sql", "WHERE id", "id", 3},
		{"migrations/003_delete.sql", "active", "active", 2},
	} {
		at := strings.Index(files[tc.path], tc.selected)
		if at < 0 {
			t.Fatal("missing fixture column")
		}
		at += strings.LastIndex(tc.selected, tc.column)
		pinned := version(tc.path)
		def := locations(call("definition", tc.path, at, pinned), tc.path+"/definition")
		if len(def) != 1 || def[0]["uri"] != "migrations/001_accounts.sql" {
			t.Fatalf("column %s has no matching original declaration: %+v", tc.column, def)
		}
		refs := locations(call("references", tc.path, at, pinned), tc.path+"/references")
		if len(refs) != tc.references {
			t.Fatalf("column %s references %d != %d: %+v", tc.column, len(refs), tc.references, refs)
		}
	}
	// Reject source data edited after an optimistic version was issued.
	sourcePath := "migrations/002_update.sql"
	oldVersion := version(sourcePath)
	if err := os.WriteFile(filepath.Join(workspace, sourcePath),
		[]byte(files[sourcePath]+"-- external edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := call("definition", sourcePath, strings.Index(files[sourcePath], "label"), oldVersion)
	if stale.OK || stale.ErrorCode != "file_changed" {
		t.Fatalf("stale SQL source accepted: %+v", stale)
	}
	// Detect definition-file changes between candidate search and output.
	s.codeTargetSnapshotHook = func() {
		path := filepath.Join(workspace, "migrations/001_accounts.sql")
		if err := os.WriteFile(path, []byte(files["migrations/001_accounts.sql"]+"-- external edit\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deletePath := "migrations/003_delete.sql"
	race := call("definition", deletePath, strings.Index(files[deletePath], "active"), version(deletePath))
	s.codeTargetSnapshotHook = nil
	if race.OK || race.ErrorCode != "sql_schema_target_changed" {
		t.Fatalf("SQL definition race accepted: %+v", race)
	}
	t.Log("REAL_WORKER_SQL_DIRECT_DML_NAVIGATION_PASS")
}
