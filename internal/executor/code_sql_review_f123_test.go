package executor

import (
	"strings"
	"testing"
)

// These RED-before/GREEN-after cases exercise complete emission units, not
// just the forbidden cursor token. Any correct-looking adjacent source
// identity from a malformed SQL statement is a false migration reference.

// A standalone SELECT must prove its trailing clauses before navigation.
// Its owning query scope is not certified by a valid FROM header alone.
func TestSQLReviewSelectTailRequiresProof(t *testing.T) {
	for _, source := range []string{
		"SELECT a.id FROM accounts a WHERE ONLY;",
		"SELECT a.id FROM accounts a WHERE a.id = AND;",
		"SELECT a.id FROM accounts a ORDER BY;",
		"SELECT a.id FROM accounts a LIMIT ONLY;",
		"SELECT a.id FROM accounts a WHERE a.id > 0 JUNK;",
	} {
		t.Run(source, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var events []sqlNavOccurrence
				var ok bool
				if all {
					events, ok = sqlNavAllOccurrences(source, "postgres")
				} else {
					events, ok = sqlNavOccurrences(source, "postgres")
				}
				if ok && len(events) != 0 {
					t.Fatalf("invalid standalone SELECT produced physical links: %+v", events)
				}
			}
		})
	}
	for _, source := range []string{
		"SELECT 'unrelated' AS msg, a.id FROM accounts a WHERE a.id > 0;",
		"SELECT a.id FROM accounts a ORDER BY a.id DESC;",
		"SELECT a.id FROM accounts a LIMIT 10;",
	} {
		t.Run("positive/"+source, func(t *testing.T) {
			events, ok := sqlNavAllOccurrences(source, "postgres")
			if !ok {
				t.Fatal("valid SELECT became unparseable")
			}
			found := false
			for _, ev := range events {
				if ev.identity == "accounts\x00column\x00id" {
					found = true
				}
			}
			if !found {
				t.Fatalf("valid SELECT lost column navigation: %+v", events)
			}
		})
	}
}

func TestSQLReviewF1ConsumedQualifiedNameLiteralGaps(t *testing.T) {
	for _, source := range []string{
		"SELECT a.'ignored'id FROM accounts a;",
		"SELECT a.id, a.'ignored'id FROM accounts a;",
		"SELECT a.id FROM accounts a WHERE a.'ignored'id = 1;",
		"SELECT a.id FROM accounts a WHERE a.'ignored'from = 1;",
		"SELECT public.'ignored'accounts.id FROM public.accounts;",
		"SELECT a.id, a.'ignored'select FROM accounts a;",
	} {
		t.Run(source, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var got []sqlNavOccurrence
				var ok bool
				if all {
					got, ok = sqlNavAllOccurrences(source, "postgres")
				} else {
					got, ok = sqlNavOccurrences(source, "postgres")
				}
				if !ok {
					continue
				}
				for _, ev := range got {
					if ev.identity == "accounts" || ev.identity == "public\x00accounts" ||
						ev.isColumn {
						t.Fatalf("elided literal inside columnref leaked physical identity (all=%v): %+v", all, ev)
					}
				}
			}
		})
	}
	for _, tc := range []struct{ sql, want string }{
		{"SELECT 'unrelated' AS msg, a.id FROM accounts a;", "accounts\x00column\x00id"},
		{"SELECT a.select FROM accounts a;", "accounts\x00column\x00select"},
		{`SELECT "a"."id" FROM accounts AS "a";`, "accounts\x00column\x00id"},
		{"SELECT 'unrelated' AS msg, a.id FROM accounts a WHERE a.id=1;", "accounts\x00column\x00id"},
	} {
		t.Run("positive/"+tc.sql, func(t *testing.T) {
			got, ok := sqlNavAllOccurrences(tc.sql, "postgres")
			if !ok {
				t.Fatal("supported source rejected")
			}
			found := false
			for _, ev := range got {
				if ev.identity == tc.want && ev.isColumn {
					found = true
				}
			}
			if !found {
				t.Fatalf("valid qualified column lost: %+v", got)
			}
		})
	}
}

func TestSQLReviewF2CreateQueryBodyProofBeforeDeclaration(t *testing.T) {
	bad := []string{
		"CREATE VIEW phantom AS FROBNICATE;",
		"CREATE VIEW phantom AS SELECT * FROM generate_series(1, 2) g JOIN accounts a ON a.id = ONLY;",
		"CREATE VIEW phantom AS SELECT * FROM generate_series(1, 2) g JOIN accounts a ON a.id = TABLE;",
		"CREATE VIEW phantom AS SELECT a.id FROM accounts a JOIN payments p ON a.id = ONLY;",
		"CREATE TABLE phantom AS SELECT a.id FROM accounts a JOIN payments p ON a.id = ONLY;",
		"CREATE MATERIALIZED VIEW phantom AS SELECT * FROM generate_series(1, 2) g JOIN accounts a ON a.id = ONLY;",
		"CREATE VIEW phantom AS SELECT a.id FROM accounts a NOISE;",
		"CREATE VIEW phantom AS SELECT a.id FROM accounts a WHERE ONLY;",
		"CREATE VIEW phantom AS SELECT a.id FROM accounts a WHERE a.id = AND;",
		"CREATE VIEW phantom AS SELECT a.id FROM accounts a ORDER BY;",
		"CREATE VIEW phantom AS SELECT a.id JUNK EXTRA FROM accounts a;",
		"CREATE VIEW phantom AS SELECT a.id FROM accounts a LIMIT ONLY;",
		// Valid but unsupported query classes cannot *prove* a physical
		// declaration. Omission is not a claim of SQL syntax invalidity.
		"CREATE VIEW phantom AS WITH cte AS (SELECT 1) SELECT * FROM cte;",
		"CREATE MATERIALIZED VIEW phantom AS TABLE accounts WITH NO DATA;",
	}
	for _, source := range bad {
		t.Run(source, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var got []sqlNavOccurrence
				var ok bool
				if all {
					got, ok = sqlNavAllOccurrences(source, "postgres")
				} else {
					got, ok = sqlNavOccurrences(source, "postgres")
				}
				if !ok {
					continue
				}
				if len(got) > 0 {
					t.Fatalf("unproven CREATE emitted physical graph all=%v: %+v", all, got)
				}
			}
		})
	}
	for _, tc := range []struct{ source, want string }{
		{"CREATE VIEW verified AS SELECT a.id FROM accounts a;", "verified"},
		{"CREATE VIEW verified AS SELECT 1 AS id;", "verified"},
		{"CREATE MATERIALIZED VIEW verified (id) AS SELECT a.id FROM accounts a WITH NO DATA;", "verified"},
		{"CREATE MATERIALIZED VIEW verified AS SELECT a.id FROM accounts a WITH DATA;", "verified"},
		{"CREATE TABLE verified AS SELECT a.id FROM accounts a;", "verified"},
	} {
		t.Run("positive/"+tc.source, func(t *testing.T) {
			got, ok := sqlNavOccurrences(tc.source, "postgres")
			if !ok {
				t.Fatal("supported CREATE SQL rejected")
			}
			found := false
			for _, ev := range got {
				if ev.identity == tc.want && ev.declaration {
					found = true
				}
			}
			if !found {
				t.Fatalf("proven CREATE declaration lost: %+v", got)
			}
		})
	}
}

func TestSQLReviewF3CompleteDDLActionAndIndexProof(t *testing.T) {
	for _, tc := range []struct{ dialect, sql string }{
		{"postgres", "CREATE TABLE accounts LIKE other ONLY;"},
		{"mysql", "CREATE TABLE accounts LIKE other JUNK;"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, PRIMARY KEY ONLY);"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, UNIQUE x);"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, FOREIGN KEY ONLY);"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, CONSTRAINT pk PRIMARY KEY ONLY);"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, PRIMARY KEY (id) ONLY);"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN okay BIGINT, BOGUS REFERENCES parents(id);"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN okay BIGINT, ADD CONSTRAINT fk FOREIGN KEY (okay) REFERENCES parents(id) BOGUS;"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN okay BIGINT, RENAME TO only_new EXTRA;"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN okay BIGINT, RENAME TO archive;"},
		{"postgres", "ALTER TABLE accounts RENAME TO archive, ADD COLUMN okay BIGINT;"},
		{"postgres", "CREATE INDEX ix ON accounts (id) ONLY;"},
		{"postgres", "CREATE INDEX ix ON accounts (id,) ;"},
		{"postgres", "CREATE INDEX ix ON accounts ((id BOGUS));"},
		{"postgres", "CREATE INDEX ix ON accounts ((id +));"},
		{"postgres", "CREATE TABLE accounts (id INT AUTO_INCREMENT);"},
		{"postgres", "CREATE TABLE accounts (id INT UNSIGNED);"},
		{"postgres", "CREATE TABLE accounts (id INT 'ignored');"},
		{"postgres", "CREATE TABLE accounts (id INT, PRIMARY KEY (id 'ignored'));"},
		{"postgres", "CREATE INDEX ix ON accounts (id) WHERE active ONLY;"},
		{"mysql", "CREATE INDEX ix ON accounts (id) ONLY;"},
		{"mariadb", "ALTER TABLE accounts ADD COLUMN okay INT, JUNK REFERENCES parents(id);"},
	} {
		t.Run(tc.dialect+"/"+tc.sql, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var got []sqlNavOccurrence
				var ok bool
				if all {
					got, ok = sqlNavAllOccurrences(tc.sql, tc.dialect)
				} else {
					got, ok = sqlNavOccurrences(tc.sql, tc.dialect)
				}
				if !ok {
					continue
				}
				if len(got) > 0 {
					t.Fatalf("invalid bounded statement left physical evidence all=%v: %+v", all, got)
				}
			}
		})
	}
	for _, tc := range []struct{ dialect, sql, want string }{
		{"postgres", "CREATE TABLE accounts (id BIGINT, PRIMARY KEY (id));", "accounts"},
		{"postgres", `CREATE TABLE accounts (id BIGINT, CONSTRAINT pk PRIMARY KEY ("id"));`, "accounts"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN okay BIGINT, ADD CONSTRAINT fk FOREIGN KEY (okay) REFERENCES parents(id);", "accounts"},
		{"postgres", "CREATE INDEX ix ON accounts (id) WHERE active;", "accounts"},
		{"postgres", "CREATE INDEX ix ON accounts (id DESC, other);", "accounts"},
		{"mysql", "CREATE TABLE accounts LIKE other;", "accounts"},
		{"mariadb", "CREATE TABLE accounts LIKE other;", "accounts"},
		{"mysql", "CREATE INDEX ix ON accounts (id);", "accounts"},
		{"mysql", "ALTER TABLE accounts ADD INDEX ix (id), ADD COLUMN okay BIGINT;", "accounts"},
		{"mariadb", "ALTER TABLE accounts ADD UNIQUE KEY uk (id), ADD COLUMN okay BIGINT;", "accounts"},
	} {
		t.Run("positive/"+tc.dialect+"/"+tc.sql, func(t *testing.T) {
			got, ok := sqlNavOccurrences(tc.sql, tc.dialect)
			if !ok {
				t.Fatal("valid SQL became globally unparseable")
			}
			found := false
			for _, ev := range got {
				if ev.identity == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("valid supported statement lost relation evidence: %+v", got)
			}
		})
	}
}

// Namespace token visibility is not enough: invalid DDL must not create
// a definition source for another file's otherwise valid column lookup.
func TestSQLReviewF3NoTransitiveBadMigrationColumns(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE accounts (id BIGINT, PRIMARY KEY ONLY);",
		"CREATE TABLE accounts (id BIGINT, UNIQUE x);",
		"CREATE TABLE accounts (id BIGINT, FOREIGN KEY ONLY);",
	} {
		got, ok := sqlNavAllOccurrences(sql, "postgres")
		if !ok {
			continue
		}
		for _, ev := range got {
			if ev.identity == "accounts" || strings.HasPrefix(ev.identity, "accounts\x00column\x00") {
				t.Fatalf("%q offers a false migration declaration: %+v", sql, ev)
			}
		}
	}
}
