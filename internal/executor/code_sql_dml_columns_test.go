package executor

import (
	"testing"
)

func TestSQLDirectDMLColumnNavigationAcrossDialects(t *testing.T) {
	tests := []struct {
		name, dialect, source, table, column string
		wantUseCount                         int
	}{
		{"postgres_update_set", "postgres",
			"UPDATE public.accounts SET label = 1 WHERE id = 7 AND active IS NOT NULL;",
			"public\x00accounts", "label", 1},
		{"postgres_update_where", "postgres",
			"UPDATE public.accounts SET label = 1 WHERE id = 7 AND active IS NOT NULL;",
			"public\x00accounts", "id", 1},
		{"postgres_update_rhs_column", "postgres",
			"UPDATE public.accounts SET label = id WHERE active IS NOT NULL;",
			"public\x00accounts", "id", 1},
		{"postgres_update_only", "postgres",
			"UPDATE ONLY public.accounts SET label = id WHERE public.accounts.id > 0;",
			"public\x00accounts", "id", 2},
		{"postgres_update_quoted", "postgres",
			`UPDATE "Billing"."Accounts" SET "State" = "Previous" WHERE "State" IS NOT NULL;`,
			"Billing\x00Accounts", "State", 2},
		{"mysql_update", "mysql",
			"UPDATE app.accounts SET label = id, active = TRUE WHERE accounts.id > 0;",
			"app\x00accounts", "id", 2},
		{"mariadb_update", "mariadb",
			"UPDATE app.accounts SET label = id WHERE id > 0;",
			"app\x00accounts", "id", 2},
		{"postgres_delete", "postgres",
			"DELETE FROM public.accounts WHERE id = 7 AND active IS NOT NULL;",
			"public\x00accounts", "id", 1},
		{"postgres_delete_only", "postgres",
			"DELETE FROM ONLY public.accounts WHERE accounts.id = 7;",
			"public\x00accounts", "id", 1},
		{"postgres_update_string_scalar", "postgres",
			"UPDATE accounts SET label='approved' WHERE id=7;",
			"accounts", "label", 1},
		{"postgres_where_postdot_keyword", "postgres",
			"DELETE FROM accounts WHERE accounts.select = 7;",
			"accounts", "select", 1},
		{"mysql_qualified_set", "mysql",
			"UPDATE app.accounts SET accounts.label = 4 WHERE accounts.id = 7;",
			"app\x00accounts", "label", 1},
		{"mysql_delete_quoted", "mysql",
			"DELETE FROM `app`.`accounts` WHERE `status` IS NOT NULL;",
			"app\x00accounts", "status", 1},
		{"mariadb_delete", "mariadb",
			"DELETE FROM accounts WHERE account_id >= 3;",
			"accounts", "account_id", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, ok := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !ok {
				t.Fatal("bounded direct DML unexpectedly unparseable")
			}
			var found []sqlNavOccurrence
			for _, ev := range events {
				if ev.isColumn && ev.tableIdentity == tc.table && ev.searchTerm == tc.column {
					if ev.declaration || ev.drop ||
						ev.identity != tc.table+"\x00column\x00"+tc.column {
						t.Fatalf("DML use invented declaration/drop or wrong source: %+v", ev)
					}
					found = append(found, ev)
					chosen, selected := sqlNavSelected(tc.source, tc.dialect, ev.span.Start.Line, ev.span.Start.Character)
					if !selected || !chosen.isColumn || chosen.identity != ev.identity {
						t.Fatalf("DML cursor selection lost physical target: %+v found=%v", chosen, selected)
					}
				}
			}
			if len(found) != tc.wantUseCount {
				t.Fatalf("DML column uses %d want %d: %+v", len(found), tc.wantUseCount, events)
			}
		})
	}
}

func TestSQLDirectDMLColumnNavigationPreservesSafeScopes(t *testing.T) {
	cases := []struct {
		name, dialect, source string
	}{
		{"unproven_update_from", "postgres", "UPDATE accounts SET id=1 FROM other WHERE accounts.id = other.id;"},
		{"unproven_delete_using", "postgres", "DELETE FROM accounts USING other WHERE accounts.id = other.id;"},
		{"unproven_subquery", "postgres", "DELETE FROM accounts WHERE id IN (SELECT id FROM other);"},
		{"unproven_returning", "postgres", "UPDATE accounts SET id=1 RETURNING id;"},
		{"unproven_update_alias", "postgres", "UPDATE accounts a SET id=1 WHERE a.id=2;"},
		{"unproven_cte", "postgres", "WITH src AS (SELECT id FROM accounts) UPDATE accounts SET id=1;"},
		{"invalid_extra_qualification", "mysql", "UPDATE accounts SET id = unknown.column;"},
		{"invalid_unknown_where_scope", "postgres", "DELETE FROM accounts WHERE unknown.id = 1;"},
		{"invalid_partial_predicate", "postgres", "UPDATE accounts SET id=1 WHERE id = ;"},
		{"invalid_implicit_mariadb_join", "mariadb", "DELETE accounts FROM accounts JOIN other ON accounts.id=other.id;"},
		{"reserved_bare_column", "postgres", "UPDATE accounts SET select=1 WHERE id=2;"},
		{"comment_elided_gap", "postgres", "UPDATE accounts SET id = 1 WHERE id = 'hidden';"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, ok := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !ok {
				return // global tokenizer already rejected ambiguous evidence
			}
			for _, ev := range found {
				if ev.isColumn {
					t.Fatalf("unsupported SQL generated a physical DML column: %+v source=%q", ev, tc.source)
				}
			}
		})
	}

	// Only the proven statement may contribute a DML occurrence. An invalid
	// neighboring statement must not invalidate an independent safe one.
	source := "UPDATE accounts SET id=2; DELETE FROM accounts USING other; DELETE FROM accounts WHERE id=3;"
	found, ok := sqlNavAllOccurrences(source, "postgres")
	if !ok {
		t.Fatal("independent bounded statements were poisoned")
	}
	var ids int
	for _, ev := range found {
		if ev.isColumn && ev.searchTerm == "id" {
			ids++
		}
	}
	if ids != 2 {
		t.Fatalf("column links borrowed from unsupported statement or lost proven statements: %d %+v", ids, found)
	}
}

func TestSQLDirectDMLColumnNavigationDoesNotMakeConstantsOrTextColumns(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		s := "UPDATE accounts SET active=TRUE, id=DEFAULT WHERE id=42 AND active IS NOT NULL;"
		got, valid := sqlNavAllOccurrences(s, dialect)
		if !valid {
			t.Fatalf("%s DML source rejected", dialect)
		}
		counts := make(map[string]int)
		for _, ev := range got {
			if !ev.isColumn {
				continue
			}
			if ev.searchTerm == "TRUE" || ev.searchTerm == "FALSE" ||
				ev.searchTerm == "NULL" || ev.searchTerm == "DEFAULT" ||
				ev.searchTerm == "42" {
				t.Fatalf("%s scalar constant invented a SQL physical column: %+v", dialect, ev)
			}
			counts[ev.searchTerm]++
		}
		if counts["active"] != 2 || counts["id"] != 2 || len(counts) != 2 {
			t.Fatalf("%s physical column uses lost or constants mapped: %+v", dialect, counts)
		}
	}
}
