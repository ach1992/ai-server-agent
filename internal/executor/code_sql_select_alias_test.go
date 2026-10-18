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

func TestSQLSelectAliasColumnGraph(t *testing.T) {
	cases := []struct {
		dialect, source, picked string
		wantIdentity, wantTable string
		wantUses                int
	}{
		{"postgres", `SELECT a.id, p.account_id FROM public.accounts AS a JOIN public.payments p ON p.account_id = a.id WHERE a.id > 0;`, "a.id", "public\x00accounts\x00column\x00id", "public\x00accounts", 3},
		{"postgres", `SELECT a.id, p.account_id FROM public.accounts AS a JOIN public.payments p ON p.account_id = a.id WHERE a.id > 0;`, "p.account_id", "public\x00payments\x00column\x00account_id", "public\x00payments", 2},
		{"postgres", `SELECT accounts.id FROM public.accounts WHERE accounts.id = 1;`, "accounts.id", "public\x00accounts\x00column\x00id", "public\x00accounts", 2},
		{"postgres", `SELECT public.accounts.id FROM public.accounts WHERE public.accounts.id > 1;`, "public.accounts.id", "public\x00accounts\x00column\x00id", "public\x00accounts", 2},
		{"postgres", `SELECT "A"."AccountId" FROM "Public"."Accounts" AS "A";`, `"A"."AccountId"`, "Public\x00Accounts\x00column\x00AccountId", "Public\x00Accounts", 1},
		{"mysql", "SELECT a.`user_id` FROM `accounts` a WHERE a.`user_id` > 0;", "a.`user_id`", "accounts\x00column\x00user_id", "accounts", 2},
		{"mariadb", "SELECT a.`user_id` FROM `accounts` AS a WHERE a.`user_id` > 0;", "a.`user_id`", "accounts\x00column\x00user_id", "accounts", 2},
	}
	for _, tc := range cases {
		t.Run(tc.dialect+"/"+tc.picked, func(t *testing.T) {
			events, valid := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !valid {
				t.Fatal("source graph unparseable")
			}
			uses := 0
			for _, ev := range events {
				if ev.isColumn && ev.identity == tc.wantIdentity {
					if ev.declaration || ev.drop || ev.tableIdentity != tc.wantTable {
						t.Fatalf("alias column fabricated a lifecycle event: %+v", ev)
					}
					uses++
				}
			}
			if uses != tc.wantUses {
				t.Fatalf("column uses %d != %d: %+v", uses, tc.wantUses, events)
			}
			position := strings.Index(tc.source, tc.picked)
			column := strings.LastIndex(tc.picked, ".") + 1
			if position < 0 {
				t.Fatal("bad test source")
			}
			chosen, ok := sqlNavSelected(tc.source, tc.dialect, 0, position+column+1)
			if !ok || chosen.identity != tc.wantIdentity || !chosen.isColumn {
				t.Fatalf("column selected wrong: %+v found=%v", chosen, ok)
			}
		})
	}
}

func TestSQLSelectAliasRefusesAmbiguousScopes(t *testing.T) {
	cases := []string{
		`WITH accounts AS (SELECT 1 AS id) SELECT a.id FROM accounts AS a;`,
		`SELECT a.id FROM (SELECT id FROM accounts) AS a;`,
		`SELECT a.id FROM accounts a UNION SELECT a.id FROM payments a;`,
		`SELECT a.id FROM accounts a JOIN payments a ON a.id = a.id;`,
		`SELECT a.id FROM public.accounts a JOIN archive.accounts a ON a.id = a.id;`,
		`SELECT a.id FROM accounts AS a JOIN payments p USING (id);`,
		`SELECT a.id FROM accounts AS a NATURAL JOIN payments p;`,
		`SELECT a.id FROM accounts AS a, payments p;`,
		`SELECT a.id FROM generate_series(1,2) AS a;`,
		`SELECT a.id FROM accounts a JOIN payments p WHERE a.id > 0;`,
		`SELECT a.id FROM accounts a WHERE EXISTS (SELECT 1 FROM payments);`,
		`SELECT a.id FROM accounts a LATERAL JOIN payments p ON a.id=p.id;`,
		`SELECT a.id FROM accounts a WHERE id IN (SELECT id FROM payments);`,
		`SELECT a.id FROM accounts a JOIN payments p ON a.id=p.id JOIN other o;`,
		`SELECT a.id FROM accounts a, other b WHERE a.id=b.id;`,
	}
	for _, source := range cases {
		t.Run(source, func(t *testing.T) {
			items, valid := sqlNavAllOccurrences(source, "postgres")
			if valid {
				for _, item := range items {
					if item.isColumn {
						t.Fatalf("unsupported scope invented a column: %s %+v", source, item)
					}
				}
			}
		})
	}
}

func TestSQLSelectAliasDoesNotConfuseNamesOrStrings(t *testing.T) {
	cases := []struct {
		dialect, source string
		want            int
	}{
		{"postgres", `SELECT 'a.id' AS msg FROM public.accounts a WHERE a.id=1;`, 1},
		{"postgres", `SELECT a.id FROM public.accounts a; -- a.id ignored`, 1},
		{"postgres", `SELECT b.id FROM public.accounts a;`, 0},
		{"postgres", `SELECT a.* FROM public.accounts a;`, 0},
		{"postgres", `SELECT a.id FROM public.accounts AS a;`, 1},
		{"postgres", `SELECT accounts.id FROM public.accounts AS a;`, 0},
		{"postgres", `SELECT public.accounts.id FROM public.accounts a;`, 0},
		{"postgres", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id=p.account_id;`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			items, ok := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !ok {
				t.Fatal("valid source became globally unparseable")
			}
			n := 0
			for _, item := range items {
				if item.isColumn {
					n++
				}
			}
			if n != tc.want {
				t.Fatalf("column references %d != %d: %+v", n, tc.want, items)
			}
		})
	}
}

func TestSQLSelectAliasWorkerCrossFileNavigation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("worker-only fixture")
	}
	requireWorkerLandlockV2(t)
	if !systemexec.Trusted("/usr/bin/rg") {
		t.Skip("trusted rg unavailable")
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	helper := filepath.Join(t.TempDir(), "asa-sql-select-alias-helper")
	build := exec.Command("go", "build", "-o", helper, "./cmd/ai-server-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	files := map[string]string{
		"migrations/001_accounts.sql": "CREATE TABLE public.accounts (id BIGINT PRIMARY KEY, name TEXT);\n",
		"migrations/002_payments.sql": "CREATE TABLE public.payments (id BIGINT PRIMARY KEY, account_id BIGINT REFERENCES public.accounts(id));\n",
		"queries/list.sql":            "SELECT a.id, p.account_id FROM public.accounts AS a JOIN public.payments p ON p.account_id = a.id WHERE a.id > 0;\n",
	}
	for name, body := range files {
		file := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	version := func(path string) string {
		t.Helper()
		resp := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: path})
		if !resp.OK || resp.FileVersion == "" {
			t.Fatalf("stat: %+v", resp)
		}
		return resp.FileVersion
	}
	owner.Action, owner.Language, owner.SQLDialect = "code_inspect", "sql", "postgres"
	owner.CodeMethod, owner.Path = "definition", "queries/list.sql"
	owner.Character = strings.Index(files[owner.Path], "a.id") + 2
	owner.FileVersion = version(owner.Path)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	resp := s.codeInspect(ctx, owner)
	var locs []sqlNavLocation
	if !resp.OK || json.Unmarshal([]byte(resp.Output), &locs) != nil || len(locs) != 1 ||
		locs[0].URI != "migrations/001_accounts.sql" || locs[0].FileVersion != version(locs[0].URI) {
		t.Fatalf("wrong aliased column definition: %+v %+v", resp, locs)
	}
	owner.CodeMethod = "references"
	resp = s.codeInspect(ctx, owner)
	if !resp.OK || json.Unmarshal([]byte(resp.Output), &locs) != nil || len(locs) != 5 {
		t.Fatalf("expected aliased query x3, declaration x1, FK x1: %+v %+v", resp, locs)
	}
	for _, loc := range locs {
		if loc.FileVersion != version(loc.URI) {
			t.Fatalf("stale source target: %+v", loc)
		}
	}
	owner.CodeMethod = "definition"
	owner.Character = strings.Index(files[owner.Path], "p.account_id") + len("p.") + 1
	resp = s.codeInspect(ctx, owner)
	if !resp.OK || json.Unmarshal([]byte(resp.Output), &locs) != nil || len(locs) != 1 ||
		locs[0].URI != "migrations/002_payments.sql" {
		t.Fatalf("wrong joined alias column definition: %+v %+v", resp, locs)
	}
	// A later source-backed table DROP must not yield a stale live column
	// definition. The migration is test-owned and leaves no installed state.
	dropped := filepath.Join(workspace, "migrations/003_drop.sql")
	if err := os.WriteFile(dropped, []byte("DROP TABLE public.accounts;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.Character = strings.Index(files[owner.Path], "a.id") + 2
	poisoned := s.codeInspect(ctx, owner)
	if poisoned.OK || poisoned.ErrorCode != "sql_schema_history_ambiguous" {
		t.Fatalf("post-DROP aliased column claimed active definition: %+v", poisoned)
	}
	if err := os.Remove(dropped); err != nil {
		t.Fatal(err)
	}
	// A real source mutation must make old query version unusable.
	if err := os.WriteFile(filepath.Join(workspace, owner.Path), []byte(files[owner.Path]+"-- mutated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := s.codeInspect(ctx, owner)
	if stale.OK || stale.ErrorCode != "file_changed" {
		t.Fatalf("stale query accepted: %+v", stale)
	}
}
