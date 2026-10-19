package executor

import (
	"strings"
	"testing"
)

// Regression: JOIN ... USING is a common bounded SQL source relation.
// Only qualified column references are navigable; the unqualified USING
// member itself must not acquire an invented single-table identity.
func TestSQLSelectUsingQualifiedSources(t *testing.T) {
	cases := []struct {
		name, dialect, query, selected, identity string
	}{
		{"pg_join", "postgres", "SELECT a.id, p.id FROM accounts AS a JOIN payments AS p USING (id) WHERE p.id > 0;", "p.id", "payments\x00column\x00id"},
		{"pg_other_column", "postgres", "SELECT a.id, p.amount FROM accounts a JOIN payments p USING (id) WHERE p.amount > 0;", "p.amount", "payments\x00column\x00amount"},
		{"pg_multiple_columns", "postgres", "SELECT a.id FROM accounts a LEFT JOIN payments p USING (id, tenant_id) WHERE a.id > 0;", "a.id", "accounts\x00column\x00id"},
		{"pg_chained", "postgres", "SELECT a.id, n.account_id FROM accounts a JOIN payments p USING (id) INNER JOIN notes n ON n.account_id = a.id;", "n.account_id", "notes\x00column\x00account_id"},
		{"mysql_join", "mysql", "SELECT a.id, p.amount FROM accounts a JOIN payments p USING (id) WHERE p.amount > 0;", "p.amount", "payments\x00column\x00amount"},
		{"mariadb_join", "mariadb", "SELECT a.id FROM accounts a RIGHT JOIN payments p USING (id);", "a.id", "accounts\x00column\x00id"},
		{"pg_quoted", "postgres", `SELECT a."Id" FROM "Accounts" a JOIN "Payments" p USING ("Id");`, `a."Id"`, "Accounts\x00column\x00Id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, valid := sqlNavAllOccurrences(tc.query, tc.dialect)
			if !valid {
				t.Fatal("SQL statement tokenization failed")
			}
			cursor := strings.Index(tc.query, tc.selected)
			if cursor < 0 {
				t.Fatal("bad test cursor")
			}
			cursor += len(tc.selected) - 1
			selected, ok := sqlNavSelected(tc.query, tc.dialect, 0, cursor)
			if !ok || !selected.isColumn || selected.identity != tc.identity {
				t.Fatalf("qualified JOIN USING use not resolved: selected=%+v found=%v all=%+v", selected, ok, events)
			}
			for _, ev := range events {
				if ev.isColumn && ev.declaration {
					t.Fatalf("query fabricated a physical declaration: %+v", ev)
				}
			}
		})
	}
}

func TestSQLSelectUsingMalformedScopeFailsClosed(t *testing.T) {
	// Invalid USING syntax, elided SQL strings, unsupported join variants,
	// ambiguous table aliases or SQL grammar must not produce any
	// source-backed table/column occurrence for this SELECT.
	cases := []string{
		"SELECT a.id FROM accounts a JOIN payments p USING;",
		"SELECT a.id FROM accounts a JOIN payments p USING ();",
		"SELECT a.id FROM accounts a JOIN payments p USING (id,);",
		"SELECT a.id FROM accounts a JOIN payments p USING (,id);",
		"SELECT a.id FROM accounts a JOIN payments p USING (id id);",
		"SELECT a.id FROM accounts a JOIN payments p USING (id,,other);",
		"SELECT a.id FROM accounts a JOIN payments p USING (id.id);",
		"SELECT a.id FROM accounts a JOIN payments p USING (id) ON a.id=p.id;",
		"SELECT a.id FROM accounts a JOIN payments p ON a.id=p.id USING (id);",
		"SELECT a.id FROM accounts a JOIN payments p USING (id) JOIN notes n;",
		"SELECT a.id FROM accounts a JOIN payments a USING (id);",
		"SELECT a.id FROM accounts a JOIN payments p USING ('id');",
		"SELECT a.id FROM accounts a JOIN payments p USING (i'd'd);",
		"SELECT a.id FROM accounts a JOIN payments p USING (id) GARBAGE;",
		"SELECT a.id FROM accounts a NATURAL JOIN payments p USING (id);",
		"SELECT a.id FROM accounts a CROSS JOIN payments p USING (id);",
		"SELECT a.id FROM accounts a JOIN payments p USING (id) UNION SELECT a.id FROM accounts a;",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			events, valid := sqlNavAllOccurrences(tc, "postgres")
			if valid && len(events) != 0 {
				t.Fatalf("unproven USING emitted physical source or column: %+v", events)
			}
		})
	}
}

func TestSQLSelectUsingEmbeddedQueriesReuseSameProof(t *testing.T) {
	for _, source := range []string{
		"CREATE VIEW summary AS SELECT a.id FROM accounts a JOIN payments p USING (id);",
		"CREATE MATERIALIZED VIEW summary AS SELECT a.id FROM accounts a JOIN payments p USING (id) WITH NO DATA;",
		"CREATE TABLE summary AS SELECT a.id FROM accounts a JOIN payments p USING (id);",
		"INSERT INTO archive (id) SELECT a.id FROM accounts a JOIN payments p USING (id);",
	} {
		t.Run(source, func(t *testing.T) {
			events, valid := sqlNavAllOccurrences(source, "postgres")
			if !valid {
				t.Fatal("embedded query did not tokenize")
			}
			pos := strings.Index(source, "a.id")
			if pos < 0 {
				t.Fatal("fixture missing qualified column")
			}
			item, ok := sqlNavSelected(source, "postgres", 0, pos+2)
			if !ok || !item.isColumn || item.identity != "accounts\x00column\x00id" {
				t.Fatalf("valid embedded USING lost versioned source identity: %+v found=%v events=%+v", item, ok, events)
			}
		})
	}

	for _, source := range []string{
		"CREATE VIEW summary AS SELECT a.id FROM accounts a JOIN payments p USING (id,);",
		"CREATE MATERIALIZED VIEW summary AS SELECT a.id FROM accounts a JOIN payments p USING (id) ON a.id=p.id;",
		"CREATE TABLE summary AS SELECT a.id FROM accounts a JOIN payments p USING ('id');",
		"INSERT INTO archive (id) SELECT a.id FROM accounts a JOIN payments p USING (id) GARBAGE;",
	} {
		t.Run(source, func(t *testing.T) {
			events, valid := sqlNavAllOccurrences(source, "postgres")
			if valid {
				for _, ev := range events {
					if ev.isColumn || ev.name == "summary" {
						t.Fatalf("unproven embedded USING polluted schema graph: %+v", ev)
					}
				}
			}
		})
	}
}
