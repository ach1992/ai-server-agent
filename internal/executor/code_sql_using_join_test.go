package executor

import (
	"fmt"
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

// Column-name equivalence is intentionally not relation/alias equivalence.
// PostgreSQL folds only unquoted words, while MySQL/MariaDB columns are
// case-insensitive even when backtick-quoted. A malformed USING predicate
// must not leak any source-backed table/column occurrences.
func TestSQLUsingColumnDuplicateDialectEquivalence(t *testing.T) {
	for _, tc := range []struct {
		name, dialect, members string
		valid                  bool
	}{
		{"mysql_bare_quoted", "mysql", "id, `ID`", false},
		{"mysql_quoted_quoted", "mysql", "`Id`, `id`", false},
		{"mysql_quoted_against_bare", "mysql", "`ID`, id", false},
		{"mariadb_bare_quoted", "mariadb", "id, `ID`", false},
		{"mariadb_quoted_quoted", "mariadb", "`Id`, `id`", false},
		{"mariadb_quoted_against_bare", "mariadb", "`ID`, id", false},
		{"postgres_bare_quoted_equal", "postgres", "id, \"id\"", false},
		{"postgres_quoted_distinct", "postgres", "id, \"ID\"", true},
		{"postgres_quoted_distinct_2", "postgres", "\"Id\", \"id\"", true},
		{"mysql_distinct", "mysql", "`Id`, `other`", true},
		{"mariadb_distinct", "mariadb", "`ID`, `other`", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := "SELECT a.id FROM accounts a JOIN payments p USING (" + tc.members + ");"
			items, ok := sqlNavAllOccurrences(query, tc.dialect)
			if !ok {
				t.Fatal("invalid SQL fixture tokenization")
			}
			selected, found := sqlNavSelected(query, tc.dialect, 0, strings.Index(query, "a.id")+2)
			if tc.valid {
				if !found || !selected.isColumn || selected.identity != "accounts\x00column\x00id" {
					t.Fatalf("valid dialect-distinct USING lost source binding: %+v found=%t items=%+v", selected, found, items)
				}
			} else if found || len(items) > 0 {
				t.Fatalf("duplicate USING emitted physical source or column: selected=%+v found=%t items=%+v", selected, found, items)
			}
		})
	}
}

func TestSQLUsingColumnDuplicateEmbeddedSelectRejected(t *testing.T) {
	for _, dialect := range []string{"mysql", "mariadb", "postgres"} {
		duplicate := "id, `ID`"
		if dialect == "postgres" {
			duplicate = "id, \"id\""
		}
		for _, prefix := range []string{
			"CREATE VIEW summary AS ",
			"CREATE TABLE summary AS ",
			"INSERT INTO archive (id) ",
		} {
			t.Run(dialect+"_"+prefix, func(t *testing.T) {
				query := prefix + "SELECT a.id FROM accounts a JOIN payments p USING (" + duplicate + ");"
				items, ok := sqlNavAllOccurrences(query, dialect)
				if !ok {
					t.Fatal("SQL fixture tokenization failed")
				}
				if selected, found := sqlNavSelected(query, dialect, 0, strings.Index(query, "a.id")+2); found {
					t.Fatalf("duplicate embedded USING resolved unproven source: %+v", selected)
				}
				for _, item := range items {
					if item.isColumn || item.name == "summary" {
						t.Fatalf("duplicate embedded USING leaked physical identity: %+v", item)
					}
				}
			})
		}
	}
}

func TestSQLUsingColumnListBoundaryAndLiteralGaps(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		members := make([]string, 65)
		for i := range members {
			members[i] = fmt.Sprintf("column_%02d", i)
		}
		for _, tc := range []struct {
			suffix  string
			allowed bool
		}{
			{strings.Join(members[:64], ", "), true},
			{strings.Join(members, ", "), false},
			{"id, id", false},
			{"id, 'discarded' other", false},
			{"id 'discarded'", false},
			{"id, 'discarded'", false},
		} {
			query := "SELECT a.id FROM accounts a JOIN payments p USING (" + tc.suffix + ");"
			events, valid := sqlNavAllOccurrences(query, dialect)
			if !valid {
				t.Fatalf("%s: unlexable fixture %q", dialect, tc.suffix)
			}
			at := strings.Index(query, "a.id") + 2
			item, found := sqlNavSelected(query, dialect, 0, at)
			if tc.allowed && (!found || !item.isColumn) {
				t.Fatalf("%s expected bounded accepted list with %q", dialect, tc.suffix)
			}
			if !tc.allowed && (found || len(events) != 0) {
				t.Fatalf("%s accepted unproven/oversized list: %+v found=%t events=%+v", dialect, item, found, events)
			}
		}
	}
	// Ambiguous Unicode column equivalence is outside this bounded
	// MySQL/MariaDB offline proof. It must not silently appear unique.
	for _, dialect := range []string{"mysql", "mariadb"} {
		query := "SELECT a.id FROM accounts a JOIN payments p USING (`İd`, id);"
		events, valid := sqlNavAllOccurrences(query, dialect)
		if valid && len(events) > 0 {
			t.Fatalf("%s unsupported Unicode USING produced physical source binding: %+v", dialect, events)
		}
	}
}
