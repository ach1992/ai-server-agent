package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

func TestSQLConstraintColumnSourceReferences(t *testing.T) {
	for _, tc := range []struct {
		name, sql, dialect string
		want               []string
	}{
		{"postgres_create_constraints", "CREATE TABLE orders (id INT, account_id INT, PRIMARY KEY (id), CONSTRAINT fk FOREIGN KEY (account_id) REFERENCES accounts(id), CONSTRAINT uq UNIQUE (id, account_id));", "postgres", []string{"orders.id", "orders.account_id", "orders.id", "orders.account_id", "orders.id", "orders.account_id", "accounts.id"}},
		{"postgres_alter_constraints", "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (account_id) REFERENCES accounts(id), ADD CONSTRAINT uq UNIQUE (account_id), ADD PRIMARY KEY (id);", "postgres", []string{"orders.account_id", "orders.account_id", "orders.id", "accounts.id"}},
		{"mysql_create_constraints", "CREATE TABLE orders (id BIGINT, account_id BIGINT, PRIMARY KEY (id), CONSTRAINT fk FOREIGN KEY fk_idx (account_id) REFERENCES accounts(id), UNIQUE KEY account_unique (account_id));", "mysql", []string{"orders.id", "orders.account_id", "orders.id", "orders.account_id", "orders.account_id", "accounts.id"}},
		{"mariadb_alter_constraints", "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (account_id) REFERENCES accounts(id), ADD UNIQUE KEY uq (id);", "mariadb", []string{"orders.id", "orders.account_id", "accounts.id"}},
		{"unknown_expression", "ALTER TABLE orders ADD CONSTRAINT uq UNIQUE ((id+1));", "postgres", nil},
		{"foreign_expression", "CREATE TABLE orders (id INT, FOREIGN KEY ((id+1)) REFERENCES accounts(id));", "mysql", nil},
		{"mysql_create_key", "CREATE TABLE orders (id BIGINT, account_id INT, KEY ix_id (id), FULLTEXT INDEX ix_text (account_id));", "mysql", []string{"orders.id", "orders.account_id", "orders.id", "orders.account_id"}},
		{"mysql_key_expression_ignored", "CREATE TABLE orders (id BIGINT, KEY ix_id ((id+1)));", "mysql", []string{"orders.id"}},
		// PostgreSQL has no MySQL-style KEY table constraint. Its whole
		// CREATE TABLE is invalid, so even id must not become a declaration.
		{"postgres_fake_mysql_key_ignored", "CREATE TABLE orders (id BIGINT, KEY ix_id (id));", "postgres", nil},
		{"column_inline_key_no_duplicate", "CREATE TABLE orders (id BIGINT PRIMARY KEY, account_id INT UNIQUE);", "mysql", []string{"orders.id", "orders.account_id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all, ok := sqlNavAllOccurrences(tc.sql, tc.dialect)
			if !ok {
				t.Fatal("SQL tokenizer rejected DDL")
			}
			if tc.name == "foreign_expression" && len(all) != 0 {
				t.Fatalf("invalid FOREIGN KEY expression emitted a partial CREATE identity: %+v", all)
			}
			var got []string
			for _, item := range all {
				if item.isColumn {
					got = append(got, item.name)
					if item.drop {
						t.Fatalf("constraint column recorded as tombstone: %+v", item)
					}
				}
				if tc.name == "postgres_fake_mysql_key_ignored" &&
					(item.declaration || item.identity == "orders") {
					t.Fatalf("invalid PostgreSQL CREATE declared a physical table: %+v", item)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("constraint source graph: %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("constraint column %d = %s, want %s; all=%+v", i, got[i], tc.want[i], all)
				}
			}
		})
	}
}

// This is the real Landlocked worker search/read/versioned-response contract:
// both local FK and remote referenced columns resolve without a live DB.
func TestSQLConstraintWorkerCrossFileQualifiedVersions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("worker identity required")
	}
	requireWorkerLandlockV2(t)
	if !systemexec.Trusted("/usr/bin/rg") {
		t.Skip("trusted worker ripgrep unavailable")
	}
	s, owner, workspace := testStdioBroker(t)
	s.runs = newRunLimiterWith(2, 1)
	s.workspaceHelperBinary = buildNodeProofWorkspaceHelper(t)
	files := map[string]string{
		"migrations/001_accounts.sql": "CREATE TABLE accounts (id BIGINT PRIMARY KEY);\n",
		"migrations/002_orders.sql":   "CREATE TABLE orders (id BIGINT, account_id BIGINT, CONSTRAINT fk FOREIGN KEY (account_id) REFERENCES accounts(id));\n",
		"queries/write.sql":           "INSERT INTO orders (account_id) VALUES (7);\n",
	}
	for name, source := range files {
		full := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	version := func(name string) string {
		got := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: name})
		if !got.OK || got.FileVersion == "" {
			t.Fatalf("worker source stat %s: %+v", name, got)
		}
		return got.FileVersion
	}
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	owner.Action, owner.Language, owner.SQLDialect, owner.Path = "code_inspect", "sql", "postgres", "migrations/002_orders.sql"
	owner.FileVersion = version(owner.Path)
	owner.Line = 0
	src := files[owner.Path]
	for _, tc := range []struct {
		name      string
		character int
		method    string
		want      []string
	}{
		{"local_fk_definition", strings.Index(src, "FOREIGN KEY (account_id)") + len("FOREIGN KEY ("), "definition", []string{"migrations/002_orders.sql"}},
		{"local_fk_references", strings.Index(src, "FOREIGN KEY (account_id)") + len("FOREIGN KEY ("), "references", []string{"migrations/002_orders.sql", "migrations/002_orders.sql", "queries/write.sql"}},
		{"remote_fk_definition", strings.Index(src, "REFERENCES accounts(id)") + len("REFERENCES accounts("), "definition", []string{"migrations/001_accounts.sql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner.CodeMethod, owner.Character = tc.method, tc.character
			got := s.codeInspect(ctx, owner)
			if !got.OK {
				t.Fatalf("worker source graph: %+v", got)
			}
			var locations []sqlNavLocation
			if err := json.Unmarshal([]byte(got.Output), &locations); err != nil || len(locations) != len(tc.want) {
				t.Fatalf("wrong source locations %+v err %v", got, err)
			}
			for i, location := range locations {
				if location.URI != tc.want[i] || location.FileVersion != version(location.URI) {
					t.Fatalf("unbacked or stale FK location %d: %+v", i, location)
				}
			}
		})
	}
}
