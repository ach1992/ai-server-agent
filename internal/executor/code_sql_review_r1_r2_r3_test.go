package executor

import (
	"strings"
	"testing"
)

// These counterexamples were supplied by an independent reviewer. Exercise
// both public offline extraction variants so a non-column emission path cannot
// bypass the same syntax decision.

// The prior 101-keyword matrix checked only whether the forbidden token
// itself became a symbol. This tests *transitive* source pollution: a valid
// companion column or real quoted table must not become navigation evidence
// merely because its neighbor is malformed.
func TestSQLReviewKeywordMatrixNoTransitiveIdentity(t *testing.T) {
	constraints := map[string]bool{
		"CONSTRAINT": true, "PRIMARY": true, "FOREIGN": true,
		"UNIQUE": true, "CHECK": true, "LIKE": true,
	}
	for keyword := range sqlNavPostgresSelectDisallowed {
		quoted := `"` + strings.ToLower(keyword) + `"`
		query := "SELECT " + keyword + ".id FROM " + quoted + ";"
		events, valid := sqlNavAllOccurrences(query, "postgres")
		if valid {
			for _, event := range events {
				if event.isColumn {
					t.Fatalf("invalid leading ColId %s produced %+v", keyword, event)
				}
			}
		}
		if constraints[keyword] {
			continue
		}
		for _, create := range []string{
			"CREATE TABLE accounts (id BIGINT, " + keyword + " BIGINT);",
			"CREATE TABLE accounts (" + keyword + " BIGINT, id BIGINT);",
		} {
			events, valid := sqlNavAllOccurrences(create, "postgres")
			if valid {
				for _, event := range events {
					if event.identity == "accounts" || strings.HasPrefix(event.identity, "accounts\x00column\x00") {
						t.Fatalf("mixed malformed DDL %s leaked %+v", create, event)
					}
				}
			}
		}
	}
}

func TestSQLReviewAlterColumnBatchAtomicity(t *testing.T) {
	for _, tc := range []struct{ dialect, source string }{
		{"postgres", "ALTER TABLE accounts ADD COLUMN okay BIGINT, ADD COLUMN ONLY BIGINT;"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN okay BIGINT, DROP COLUMN ONLY;"},
		{"postgres", "ALTER TABLE accounts ADD COLUMN okay BIGINT, RENAME COLUMN old TO ONLY;"},
		{"mysql", "ALTER TABLE accounts ADD COLUMN okay BIGINT, ADD COLUMN SELECT INT;"},
		{"mariadb", "ALTER TABLE accounts ADD COLUMN okay BIGINT, DROP COLUMN SELECT;"},
	} {
		t.Run(tc.dialect+"/"+tc.source, func(t *testing.T) {
			got, valid := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !valid {
				return
			}
			for _, item := range got {
				if item.isColumn {
					t.Fatalf("partial invalid ALTER emitted a physical column: %+v", item)
				}
			}
		})
	}
	for _, tc := range []struct{ dialect, source string }{
		{"postgres", `ALTER TABLE accounts ADD COLUMN okay BIGINT, ADD COLUMN "only" BIGINT;`},
		{"mysql", "ALTER TABLE accounts ADD COLUMN okay BIGINT, ADD COLUMN `SELECT` INT;"},
	} {
		t.Run("positive/"+tc.dialect, func(t *testing.T) {
			got, valid := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !valid {
				t.Fatal("valid ALTER became unparseable")
			}
			count := 0
			for _, item := range got {
				if item.isColumn && item.declaration {
					count++
				}
			}
			if count != 2 {
				t.Fatalf("valid ALTER lost column declarations: %+v", got)
			}
		})
	}
}

func TestSQLReviewR1QualifiedLeadingKeywordNotColumn(t *testing.T) {
	for _, source := range []string{
		`SELECT ONLY.id FROM "only";`,
		`SELECT TABLE.id FROM "TABLE";`,
		`SELECT LEFT.id FROM "left";`,
		`SELECT ONLY.id FROM "only" WHERE ONLY.id > 0;`,
		`SELECT ONLY.id FROM "only" o JOIN payments p ON o.id = p.id;`,
	} {
		t.Run(source, func(t *testing.T) {
			occ, ok := sqlNavAllOccurrences(source, "postgres")
			if !ok {
				t.Fatal("unexpected lexical rejection")
			}
			for _, event := range occ {
				if event.isColumn {
					t.Fatalf("reserved leading ColId exposed a physical column: %+v", event)
				}
			}
			at := strings.Index(source, ".id") + 2
			if selected, found := sqlNavSelected(source, "postgres", 0, at); found && selected.isColumn {
				t.Fatalf("reserved leading ColId can navigate as physical column: %+v", selected)
			}
		})
	}
	for _, tc := range []struct{ dialect, source, want string }{
		{"postgres", `SELECT "only".id FROM "only";`, "only\x00column\x00id"},
		{"postgres", `SELECT o.select FROM "only" o;`, "only\x00column\x00select"},
		{"postgres", `SELECT o.only FROM "only" o;`, "only\x00column\x00only"},
		{"mysql", "SELECT ONLY.id FROM ONLY;", "only\x00column\x00id"},
		{"mariadb", "SELECT ONLY.id FROM ONLY;", "only\x00column\x00id"},
	} {
		t.Run(tc.dialect+"/"+tc.source, func(t *testing.T) {
			occ, ok := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !ok {
				t.Fatal("supported identifier rejected")
			}
			found := false
			for _, event := range occ {
				if event.isColumn && event.identity == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("legal dialect-scoped column missing: %+v", occ)
			}
		})
	}
}

func TestSQLReviewR2NonSelectQueryBodyCannotBypassScope(t *testing.T) {
	invalid := []string{
		"CREATE VIEW v AS SELECT a.id FROM accounts a JOIN payments p ON a.id = ONLY;",
		"CREATE VIEW v AS SELECT a.id FROM accounts a JUNK JOIN payments p ON a.id=p.id;",
		"CREATE MATERIALIZED VIEW v AS SELECT a.id FROM accounts a JOIN payments p ON a.id = ONLY;",
		"INSERT INTO archive (id) SELECT a.id FROM accounts a JOIN payments p ON a.id = ONLY;",
		"INSERT INTO archive (id) SELECT a.id FROM accounts a JUNK JOIN payments p ON a.id=p.id;",
		"CREATE TABLE report AS SELECT a.id FROM accounts a JOIN payments p ON a.id=ONLY;",
	}
	for _, source := range invalid {
		t.Run(source, func(t *testing.T) {
			for _, columns := range []bool{false, true} {
				occ, ok := sqlNavSourceOccurrences(source, "postgres", columns)
				if !ok {
					continue
				}
				for _, ev := range occ {
					if ev.identity == "accounts" || ev.identity == "payments" ||
						ev.tableIdentity == "accounts" || ev.tableIdentity == "payments" {
						t.Fatalf("invalid embedded query emitted unproven physical source: %+v", ev)
					}
				}
			}
			at := strings.Index(source, "FROM accounts")
			if at < 0 {
				t.Fatal("fixture missing source")
			}
			if ev, ok := sqlNavSelected(source, "postgres", 0, at+len("FROM a")); ok &&
				(ev.identity == "accounts" || ev.identity == "payments") {
				t.Fatalf("invalid embedded source navigated: %+v", ev)
			}
		})
	}
	for _, tc := range []struct{ source, target string }{
		{"CREATE VIEW v AS SELECT a.id FROM accounts a;", "accounts"},
		{"CREATE VIEW v AS SELECT a.id FROM accounts a JOIN payments p ON a.id=p.id;", "accounts"},
		{"CREATE VIEW v AS SELECT a.id FROM accounts a JOIN payments p ON a.id=p.id;", "payments"},
		{"INSERT INTO archive (id) SELECT a.id FROM accounts a JOIN payments p ON a.id=p.id;", "payments"},
		{"CREATE MATERIALIZED VIEW v AS SELECT a.id FROM accounts a;", "accounts"},
		{"INSERT INTO archive (id) SELECT a.id FROM accounts a;", "accounts"},
		{"DELETE FROM accounts WHERE id=1;", "accounts"},
		{"UPDATE accounts SET id=1;", "accounts"},
	} {
		t.Run("positive/"+tc.source, func(t *testing.T) {
			events, ok := sqlNavOccurrences(tc.source, "postgres")
			if !ok {
				t.Fatal("valid direct source became unparseable")
			}
			found := false
			for _, ev := range events {
				if ev.identity == tc.target && !ev.declaration {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing proven direct/contained source: %+v", events)
			}
		})
	}
}

func TestSQLReviewR3CreateDeclarationAllOrNothing(t *testing.T) {
	for _, tc := range []struct{ dialect, sql string }{
		{"postgres", "CREATE TABLE accounts (id BIGINT, ONLY BIGINT);"},
		{"postgres", "CREATE TABLE accounts (ONLY BIGINT, id BIGINT);"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, SELECT INT, name TEXT);"},
		{"postgres", "CREATE TABLE accounts (id BIGINT REFERENCES ONLY(id), name TEXT);"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, PRIMARY KEY (ONLY));"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, CONSTRAINT uq UNIQUE (ONLY));"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, FOREIGN KEY (ONLY) REFERENCES parents(id));"},
		{"postgres", "CREATE TABLE accounts (id BIGINT REFERENCES parents(ONLY), name TEXT);"},
		{"postgres", "CREATE TABLE accounts (id BIGINT, CONSTRAINT fk FOREIGN KEY (id) REFERENCES TABLE(id));"},
		{"postgres", "CREATE TABLE accounts (CONSTRAINT pk PRIMARY KEY(id), id BIGINT, ONLY BIGINT);"},
		{"mysql", "CREATE TABLE accounts (id BIGINT, SELECT INT);"},
		{"mariadb", "CREATE TABLE accounts (id BIGINT, SELECT INT);"},
	} {
		t.Run(tc.dialect+"/"+tc.sql, func(t *testing.T) {
			for _, all := range []bool{false, true} {
				var evs []sqlNavOccurrence
				var ok bool
				if all {
					evs, ok = sqlNavAllOccurrences(tc.sql, tc.dialect)
				} else {
					evs, ok = sqlNavOccurrences(tc.sql, tc.dialect)
				}
				if !ok {
					continue
				}
				for _, ev := range evs {
					if ev.identity == "accounts" || strings.HasPrefix(ev.identity, "accounts\x00column\x00") {
						t.Fatalf("malformed CREATE preserved partial declaration all=%t: %+v", all, ev)
					}
				}
			}
		})
	}
	for _, tc := range []struct{ dialect, sql, wanted string }{
		{"postgres", `CREATE TABLE accounts (id BIGINT, "only" BIGINT, name TEXT, CONSTRAINT pk PRIMARY KEY (id));`, "accounts\x00column\x00only"},
		{"mysql", "CREATE TABLE accounts (id BIGINT, `SELECT` INT);", "accounts\x00column\x00SELECT"},
	} {
		t.Run("positive/"+tc.dialect, func(t *testing.T) {
			evs, ok := sqlNavAllOccurrences(tc.sql, tc.dialect)
			if !ok {
				t.Fatal("valid CREATE rejected")
			}
			foundTable, foundColumn := false, false
			for _, ev := range evs {
				if ev.identity == "accounts" && ev.declaration {
					foundTable = true
				}
				if ev.identity == tc.wanted && ev.declaration {
					foundColumn = true
				}
			}
			if !foundTable || !foundColumn {
				t.Fatalf("valid CREATE source lost %+v", evs)
			}
		})
	}
}
