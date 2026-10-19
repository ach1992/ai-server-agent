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
		{"postgres", `SELECT a.id FROM public.accounts a JOIN public.payments p ON (a.id=p.account_id) WHERE a.id > 0;`, "a.id", "public\x00accounts\x00column\x00id", "public\x00accounts", 3},
		{"postgres", `SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id=p.account_id LEFT JOIN public.payments q ON q.account_id = a.id WHERE a.id > 0;`, "a.id", "public\x00accounts\x00column\x00id", "public\x00accounts", 4},
		{"postgres", `SELECT accounts.id FROM public.accounts WHERE accounts.id = 1;`, "accounts.id", "public\x00accounts\x00column\x00id", "public\x00accounts", 2},
		{"postgres", `SELECT public.accounts.id FROM public.accounts WHERE public.accounts.id > 1;`, "public.accounts.id", "public\x00accounts\x00column\x00id", "public\x00accounts", 2},
		{"postgres", `SELECT "A"."AccountId" FROM "Public"."Accounts" AS "A";`, `"A"."AccountId"`, "Public\x00Accounts\x00column\x00AccountId", "Public\x00Accounts", 1},
		{"mysql", "SELECT a.`user_id` FROM `accounts` a WHERE a.`user_id` > 0;", "a.`user_id`", "accounts\x00column\x00user_id", "accounts", 2},
		{"mariadb", "SELECT a.`user_id` FROM `accounts` AS a WHERE a.`user_id` > 0;", "a.`user_id`", "accounts\x00column\x00user_id", "accounts", 2},
		{"mysql", "SELECT a.`user_id` FROM `accounts` AS a JOIN `payments` p ON p.`account_id` = a.`user_id` WHERE a.`user_id`>0;", "a.`user_id`", "accounts\x00column\x00user_id", "accounts", 3},
		{"mariadb", "SELECT a.`user_id` FROM `accounts` a JOIN `payments` p ON p.`account_id` = a.`user_id` WHERE a.`user_id`>0;", "a.`user_id`", "accounts\x00column\x00user_id", "accounts", 3},
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

func TestSQLSelectAliasPostgresSourceKeywordGrammarFailsClosed(t *testing.T) {
	// An unquoted PostgreSQL grammar keyword is not an ordinary FROM/JOIN
	// relation or alias. The original ONLY bug maps accounts.id to
	// "only".id when an actual quoted table "only" also exists.
	cases := []struct {
		name, source, selected string
	}{
		{"pg_only_from", "SELECT accounts.id FROM ONLY accounts;", "accounts.id"},
		{"pg_only_join", "SELECT accounts.id FROM payments p JOIN ONLY accounts ON p.account_id=accounts.id;", "accounts.id"},
		{"pg_only_qualified", "SELECT accounts.id FROM ONLY.accounts;", "accounts.id"},
		{"pg_reserved_table", "SELECT accounts.id FROM TABLE accounts;", "accounts.id"},
		{"pg_reserved_user", "SELECT accounts.id FROM USER accounts;", "accounts.id"},
		{"pg_reserved_current_user", "SELECT accounts.id FROM CURRENT_USER accounts;", "accounts.id"},
		{"pg_type_keyword_left", "SELECT accounts.id FROM LEFT accounts;", "accounts.id"},
		{"pg_type_keyword_join", "SELECT accounts.id FROM JOIN accounts;", "accounts.id"},
		{"pg_reserved_alias", "SELECT only.id FROM accounts AS ONLY;", "only.id"},
		{"pg_reserved_alias_implicit", "SELECT only.id FROM accounts ONLY;", "only.id"},
		{"pg_type_keyword_alias", "SELECT left.id FROM accounts AS LEFT;", "left.id"},
		{"pg_only_join_qualified", "SELECT accounts.id FROM payments p JOIN ONLY.accounts ON accounts.id=p.account_id;", "accounts.id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, valid := sqlNavAllOccurrences(tc.source, "postgres")
			if valid {
				for _, event := range events {
					if event.isColumn {
						t.Fatalf("invalid PostgreSQL binding emitted physical column: %+v", event)
					}
				}
			}
			start := strings.Index(tc.source, tc.selected)
			if start < 0 {
				t.Fatal("bad test selection")
			}
			at := start + strings.LastIndex(tc.selected, ".") + 2
			if column, ok := sqlNavSelected(tc.source, "postgres", 0, at); ok && column.isColumn {
				t.Fatalf("invalid PostgreSQL binding selectable: %+v", column)
			}
		})
	}
}

func TestSQLSelectAliasPostgresSourceKeywordClassMutations(t *testing.T) {
	// Exhaust the pinned PostgreSQL reserved/type-function keyword classes at
	// each ordinary FROM/JOIN name or alias binding boundary. Testing every
	// keyword catches the same bug family as ONLY/TABLE/LEFT instead of
	// creating a new point fix for each independently reported token.
	for keyword := range sqlNavPostgresSelectDisallowed {
		for _, source := range []string{
			"SELECT a.id FROM " + keyword + " a;",
			"SELECT a.id FROM accounts x JOIN " + keyword + " a ON x.id=a.id;",
			"SELECT " + keyword + ".id FROM accounts AS " + keyword + ";",
			"SELECT " + keyword + ".id FROM accounts " + keyword + ";",
		} {
			t.Run(keyword+"/"+source, func(t *testing.T) {
				events, valid := sqlNavAllOccurrences(source, "postgres")
				if valid {
					for _, ev := range events {
						if ev.isColumn {
							t.Fatalf("PostgreSQL keyword used as unquoted source/alias: %+v", ev)
						}
					}
				}
			})
		}
	}
}

func TestSQLSelectAliasSourceKeywordDialectAndQuotingPositives(t *testing.T) {
	// Reserved syntax is dialect/context-specific. "only" is a valid
	// quoted PostgreSQL relation and a valid *unquoted* MySQL/MariaDB
	// relation, but not an unquoted PostgreSQL FROM/JOIN table.
	cases := []struct {
		name, dialect, source, selected, identity string
	}{
		{"pg_quoted_only", "postgres", "SELECT a.id FROM \"only\" AS a;", "a.id", "only\u0000column\u0000id"},
		{"pg_quoted_only_alias", "postgres", "SELECT \"only\".id FROM accounts AS \"only\";", "\"only\".id", "accounts\u0000column\u0000id"},
		{"pg_quoted_schema", "postgres", "SELECT a.id FROM \"ONLY\".accounts AS a;", "a.id", "ONLY\u0000accounts\u0000column\u0000id"},
		{"pg_unreserved_name", "postgres", "SELECT inherit.id FROM inherit;", "inherit.id", "inherit\u0000column\u0000id"},
		{"mysql_only_source", "mysql", "SELECT accounts.id FROM ONLY accounts;", "accounts.id", "only\u0000column\u0000id"},
		{"mariadb_only_source", "mariadb", "SELECT accounts.id FROM ONLY accounts;", "accounts.id", "only\u0000column\u0000id"},
		{"mysql_only_join", "mysql", "SELECT p.id FROM accounts a JOIN ONLY p ON p.account_id=a.id;", "p.id", "only\u0000column\u0000id"},
		{"mariadb_only_join", "mariadb", "SELECT p.id FROM accounts a JOIN ONLY p ON p.account_id=a.id;", "p.id", "only\u0000column\u0000id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, valid := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !valid {
				t.Fatal("valid dialect SQL source became globally unparseable")
			}
			var matched bool
			for _, item := range items {
				if item.isColumn && item.identity == tc.identity {
					matched = true
				}
			}
			if !matched {
				t.Fatalf("missing exact dialect-aware physical source %q: %+v", tc.identity, items)
			}
			at := strings.Index(tc.source, tc.selected)
			if at < 0 {
				t.Fatal("bad test selection")
			}
			at += strings.LastIndex(tc.selected, ".") + 2
			if selected, ok := sqlNavSelected(tc.source, tc.dialect, 0, at); !ok ||
				!selected.isColumn || selected.identity != tc.identity {
				t.Fatalf("unexpected dialect keyword binding: %+v found=%v", selected, ok)
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

// The migration column identity must never be inferred for PostgreSQL's
// positional table column-alias lists or incomplete JOIN predicates.
// Such forms are outside this bounded single-scope parser.
func TestSQLSelectAliasRejectsRenamingAndIncompleteJoins(t *testing.T) {
	cases := []struct{ name, source string }{
		{"from_single_alias_column", `SELECT a.name FROM public.accounts AS a (name);`},
		{"from_multiple_alias_columns", `SELECT a.name FROM public.accounts AS a (name, id);`},
		{"from_implicit_alias_column", `SELECT a.name FROM public.accounts a (name, id);`},
		{"from_quoted_alias_column", `SELECT a."Name" FROM public.accounts AS a ("Name", "Id");`},
		{"from_mixed_quoted_alias_column", `SELECT a.name FROM public.accounts AS a ("name", id);`},
		{"join_positional_alias_column", `SELECT p.account_id FROM public.accounts a JOIN public.payments AS p (account_id, id) ON p.account_id = a.id;`},
		{"join_unadorned_alias_column", `SELECT p.account_id FROM public.accounts a JOIN public.payments p (account_id) ON p.account_id = a.id;`},
		{"alias_list_before_following_join", `SELECT a.name FROM public.accounts AS a (name, id) JOIN payments p ON a.name=p.id;`},
		{"empty_on_end", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON;`},
		{"empty_on_where", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON WHERE a.id > 0;`},
		{"empty_on_order", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON ORDER BY a.id;`},
		{"empty_on_group", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON GROUP BY a.id;`},
		{"empty_on_next_join", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON JOIN archive z ON a.id=z.id;`},
		{"empty_parenthesized_on", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON ();`},
		{"nested_empty_parenthesized_on", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON (());`},
		{"empty_parenthesized_and_other", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON () AND a.id=p.id;`},
		{"truncated_equality", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON a.id =;`},
		{"truncated_parenthesized_equality", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON (a.id =);`},
		{"truncated_boolean", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON a.id=p.id AND;`},
		{"truncated_second_join", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON a.id=p.id JOIN archive z ON;`},
		{"truncated_left_join", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON a.id=p.id LEFT JOIN archive z ON;`},
		{"truncated_second_join_where", `SELECT a.id FROM public.accounts AS a JOIN public.payments p ON a.id=p.id JOIN archive z ON WHERE z.id=a.id;`},
		{"truncated_on_left_modifier", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id = p.id LEFT;`},
		{"malformed_on_next_join", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id=AND p.account_id LEFT JOIN reports r ON r.id=a.id;`},
		{"truncated_on_left_outer", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id=p.id LEFT OUTER;`},
		{"truncated_from_left", `SELECT a.id FROM public.accounts a LEFT;`},
		{"on_only_connector", `SELECT a.id FROM public.accounts a JOIN payments p ON AND;`},
		{"on_only_operator", `SELECT a.id FROM public.accounts a JOIN payments p ON =;`},
		{"interior_and_as_rhs", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id = AND p.account_id;`},
		{"interior_or_as_rhs", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id = OR p.account_id;`},
		{"interior_and_as_lhs", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id AND = p.account_id;`},
		{"interior_repeated_operator", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id == p.account_id;`},
		{"interior_unterminated_rhs", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id = p.account_id AND p.id = OR a.id;`},
		{"interior_broken_parenthesis", `SELECT a.id FROM public.accounts a JOIN payments p ON (a.id = AND p.account_id) OR (a.id=p.account_id);`},
		{"interior_multiple_connector", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id=p.account_id AND OR a.id=p.account_id;`},
		{"interior_missing_operator", `SELECT a.id FROM public.accounts a JOIN payments p ON a.id p.account_id;`},
		{"interior_unmatched_parenthesis", `SELECT a.id FROM public.accounts a JOIN payments p ON (a.id=p.account_id));`},
		{"join_left_right", `SELECT a.id FROM public.accounts a LEFT RIGHT JOIN payments p ON a.id=p.account_id;`},
		{"join_inner_left", `SELECT a.id FROM public.accounts a INNER LEFT JOIN payments p ON a.id=p.account_id;`},
		{"join_outer_alone", `SELECT a.id FROM public.accounts a OUTER JOIN payments p ON a.id=p.account_id;`},
		{"join_left_inner", `SELECT a.id FROM public.accounts a LEFT INNER JOIN payments p ON a.id=p.account_id;`},
		{"join_double_left", `SELECT a.id FROM public.accounts a LEFT LEFT JOIN payments p ON a.id=p.account_id;`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			occurrences, valid := sqlNavAllOccurrences(tc.source, "postgres")
			if valid {
				for _, item := range occurrences {
					if item.isColumn {
						t.Fatalf("unsupported SQL invented a table-column identity: %+v", item)
					}
				}
			}
			// No cursor position in this unsupported SELECT may start
			// source-backed column navigation to an unrelated declaration.
			for _, qualifier := range []string{"a.name", `a."Name"`, "p.account_id", "a.id"} {
				start := strings.Index(tc.source, qualifier)
				if start < 0 {
					continue
				}
				pos := start + strings.Index(qualifier, ".") + 2
				if selected, ok := sqlNavSelected(tc.source, "postgres", 0, pos); ok && selected.isColumn {
					t.Fatalf("unsupported SQL yielded offline selectable column %q: %+v", qualifier, selected)
				}
			}
		})
	}
}

// The ON completeness rule is dialect-neutral, while positional table
// column-alias lists are deliberately tested under PostgreSQL syntax above.
func TestSQLSelectAliasIncompleteOnAcrossDialects(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		for _, source := range []string{
			`SELECT a.id FROM accounts a JOIN payments p ON;`,
			`SELECT a.id FROM accounts a JOIN payments p ON WHERE a.id=1;`,
			`SELECT a.id FROM accounts a JOIN payments p ON ();`,
			`SELECT a.id FROM accounts a JOIN payments p ON a.id=;`,
			`SELECT a.id FROM accounts a JOIN payments p ON a.id=p.account_id INNER JOIN reports r ON;`,
		} {
			t.Run(dialect+"/"+source, func(t *testing.T) {
				items, valid := sqlNavAllOccurrences(source, dialect)
				if valid {
					for _, item := range items {
						if item.isColumn {
							t.Fatalf("incomplete ON created a %s column: %+v", dialect, item)
						}
					}
				}
			})
		}
	}
}

// Validate the deliberately small proven ON grammar instead of accepting
// arbitrary balanced token streams as a trustworthy SQL scope. These positive
// cases also guard the existing supported boolean/outer-join behavior.
func TestSQLSelectAliasSupportedOnGrammar(t *testing.T) {
	cases := []struct {
		dialect, source string
		wantUses        int
	}{
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON a.id=p.account_id AND p.id=a.id;`, 3},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON (a.id=p.account_id OR a.id=p.id) AND p.id>0;`, 3},
		{"postgres", `SELECT a.id FROM accounts a LEFT OUTER JOIN payments p ON (a.id=p.account_id);`, 2},
		{"postgres", `SELECT a.id FROM accounts a FULL OUTER JOIN payments p ON a.id=p.account_id;`, 2},
		{"postgres", `SELECT a.id FROM accounts a INNER JOIN payments p ON NOT (a.id != p.account_id);`, 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON a.id IS NOT NULL;`, 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON a.name ILIKE p.name AND a.id=p.account_id;`, 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON a.id <> p.account_id;`, 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON a.id <= p.account_id;`, 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON a.id != p.account_id;`, 2},
		{"mysql", "SELECT a.`id` FROM accounts a JOIN payments p ON a.`id` <> p.account_id;", 2},
		{"mariadb", "SELECT a.`id` FROM accounts a JOIN payments p ON a.`id` <= p.account_id;", 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON a.id /* has ' quote */ = p.account_id;`, 2},
		{"postgres", `SELECT "A'1"."id" FROM accounts "A'1" JOIN payments p ON "A'1"."id" = p.account_id;`, 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON a.id >= 12.5 AND p.account_id = -2;`, 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON TRUE WHERE a.id>0;`, 2},
		{"postgres", `SELECT a.id FROM accounts a JOIN payments p ON (TRUE OR a.id = p.account_id);`, 2},
		{"mysql", "SELECT a.`id` FROM `accounts` a LEFT JOIN payments p ON (a.`id` = p.account_id AND p.id >= 15);", 2},
		{"mariadb", "SELECT a.`id` FROM `accounts` a RIGHT OUTER JOIN payments p ON a.`id`=p.account_id OR p.account_id = a.`id`;", 3},
	}
	for _, tc := range cases {
		t.Run(tc.dialect+"/"+tc.source, func(t *testing.T) {
			occurrences, valid := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !valid {
				t.Fatal("supported SQL source became globally unparseable")
			}
			got := 0
			for _, item := range occurrences {
				if item.isColumn && strings.HasSuffix(item.identity, "\x00column\x00id") &&
					(item.tableIdentity == "accounts" || item.tableIdentity == "public\x00accounts") {
					got++
				}
			}
			if got != tc.wantUses {
				t.Fatalf("proven ON/alias column uses %d != %d: %+v", got, tc.wantUses, occurrences)
			}
		})
	}
}

func TestSQLSelectAliasPostgresOnReservedOperandFailsClosed(t *testing.T) {
	// The ON subgrammar must not certify a reserved SQL keyword as a
	// physical column/expression name. No migration target is justified
	// when a syntactically invalid operand follows a comparison.
	for _, tc := range []struct{ name, source string }{
		{"pg_only_rhs", "SELECT a.id FROM accounts a JOIN payments p ON a.id = ONLY;"},
		{"pg_table_rhs", "SELECT a.id FROM accounts a JOIN payments p ON a.id = TABLE;"},
		{"pg_user_rhs", "SELECT a.id FROM accounts a JOIN payments p ON a.id = USER;"},
		{"pg_left_rhs", "SELECT a.id FROM accounts a JOIN payments p ON a.id = LEFT;"},
		{"pg_only_lhs", "SELECT a.id FROM accounts a JOIN payments p ON ONLY = p.account_id;"},
		{"pg_type_keyword_with_and", "SELECT a.id FROM accounts a JOIN payments p ON a.id=p.account_id AND a.id=TABLE;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, valid := sqlNavAllOccurrences(tc.source, "postgres")
			if valid {
				for _, ev := range events {
					if ev.isColumn {
						t.Fatalf("invalid PostgreSQL ON operand emitted a column: %+v", ev)
					}
				}
			}
		})
	}
}

func TestSQLSelectAliasPostgresOnKeywordClassMutations(t *testing.T) {
	// Bare SQL grammar keywords cannot act as physical ON identifiers,
	// even if the rest of a JOIN looks structurally complete.
	for keyword := range sqlNavPostgresSelectDisallowed {
		switch keyword {
		case "TRUE", "FALSE", "NULL":
			continue // supported constant expressions, not identifiers
		}
		source := "SELECT a.id FROM accounts a JOIN payments p ON a.id=" + keyword + ";"
		t.Run(keyword, func(t *testing.T) {
			items, valid := sqlNavAllOccurrences(source, "postgres")
			if valid {
				for _, item := range items {
					if item.isColumn {
						t.Fatalf("reserved PostgreSQL ON operand invented a column: %+v", item)
					}
				}
			}
		})
	}
}

func TestSQLSelectAliasOnDialectKeywordAndConstantPositives(t *testing.T) {
	for _, tc := range []struct {
		name, dialect, source string
	}{
		{"pg_quoted_keyword", "postgres", "SELECT a.id FROM accounts a JOIN payments p ON a.id=p.\"only\";"},
		{"pg_true_constant", "postgres", "SELECT a.id FROM accounts a JOIN payments p ON a.id=TRUE;"},
		{"pg_null_constant", "postgres", "SELECT a.id FROM accounts a JOIN payments p ON a.id=NULL;"},
		{"mysql_only_operand", "mysql", "SELECT a.id FROM accounts a JOIN payments p ON a.id=ONLY;"},
		{"mariadb_only_operand", "mariadb", "SELECT a.id FROM accounts a JOIN payments p ON a.id=ONLY;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, valid := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !valid {
				t.Fatal("supported source unparseable")
			}
			var references int
			for _, item := range items {
				if item.isColumn && item.identity == "accounts\u0000column\u0000id" {
					references++
				}
			}
			if references != 2 {
				t.Fatalf("supported ON expression lost expected accounts.id projection and ON references: %d %+v", references, items)
			}
		})
	}
}

func TestSQLSelectAliasOnGrammarFailsClosed(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		// The ON budget is independent of the general SQL document limit.
		// A too-long expression must fail atomically, never emit partial refs.
		overbudget := "SELECT a.id FROM accounts a JOIN payments p ON " +
			strings.Repeat("TRUE AND ", sqlNavOnMaxTokens/2) + "TRUE;"
		items, valid := sqlNavAllOccurrences(overbudget, dialect)
		if valid {
			for _, item := range items {
				if item.isColumn {
					t.Fatalf("over-budget %s ON returned partial column graph: %+v", dialect, item)
				}
			}
		}
		for _, clause := range []string{
			"a.id = AND p.account_id", "a.id = OR p.account_id",
			"a.id == p.account_id", "a.id AND = p.account_id",
			"a.id = p.account_id AND OR p.account_id = a.id",
			"a.id=p.account_id AND (a.id = OR p.account_id)",
			"a.id = p.account_id AND p.id IS", "a.id = p.account_id AND p.id =",
			"a.id = p.account_id AND p.id = 'example'", // literals removed by the bounded tokenizer
			"a.id < > p.account_id", "a.id < = p.account_id",
			"a.id > = p.account_id", "a.id ! = p.account_id",
			"a.id < /* inserted comment */ > p.account_id",
			"a.id = 'orphan' p.account_id",
			"a.id = p.account_id 'orphan'",
			"a.id = p.account_id /* harmless */ 'orphan'",
			"a.id = $tag$orphan$tag$ p.account_id",
			"a.id = p.account_id $tag$orphan$tag$",
			strings.Repeat("(", sqlNavOnMaxDepth+1) + "a.id=p.account_id" + strings.Repeat(")", sqlNavOnMaxDepth+1),
		} {
			source := "SELECT a.id FROM accounts a JOIN payments p ON " + clause + ";"
			t.Run(dialect+"/"+clause, func(t *testing.T) {
				items, valid := sqlNavAllOccurrences(source, dialect)
				if valid {
					for _, item := range items {
						if item.isColumn {
							t.Fatalf("unsupported ON generated fabricated column: %+v", item)
						}
					}
				}
			})
		}
		if dialect == "mysql" || dialect == "mariadb" {
			for _, source := range []string{
				`SELECT a.id FROM accounts a FULL JOIN payments p ON a.id=p.account_id;`,
				`SELECT a.id FROM accounts a JOIN payments p ON a.name ILIKE p.name;`,
			} {
				t.Run(dialect+"/unsupported_dialect/"+source, func(t *testing.T) {
					items, valid := sqlNavAllOccurrences(source, dialect)
					if valid {
						for _, item := range items {
							if item.isColumn {
								t.Fatalf("unsupported dialect grammar created column: %+v", item)
							}
						}
					}
				})
			}
		}
		for _, prefix := range []string{"LEFT RIGHT", "INNER LEFT", "OUTER", "FULL INNER", "LEFT OUTER RIGHT", "RIGHT OUTER OUTER"} {
			source := "SELECT a.id FROM accounts a " + prefix + " JOIN payments p ON a.id=p.account_id;"
			t.Run(dialect+"/"+prefix, func(t *testing.T) {
				items, valid := sqlNavAllOccurrences(source, dialect)
				if valid {
					for _, item := range items {
						if item.isColumn {
							t.Fatalf("invalid JOIN modifier created column: %+v", item)
						}
					}
				}
			})
		}
	}
}

func TestSQLSelectAliasElidedLiteralAtEOFAndClauseBoundary(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		for _, source := range []string{
			"SELECT a.id FROM accounts a JOIN payments p ON a.id=p.account_id 'orphan'",
			"SELECT a.id FROM accounts a JOIN payments p ON a.id=p.account_id 'orphan' WHERE a.id=1;",
			"SELECT a.id FROM accounts a JOIN payments p ON a.id=p.account_id 'orphan' JOIN other o ON o.id=a.id;",
			"SELECT a.id FROM accounts a JOIN payments p ON a.id='orphan' p.account_id;",
			"SELECT a.id FROM accounts a 'orphan' JOIN payments p ON a.id=p.account_id;",
			"SELECT a.id FROM accounts a JOIN payments p 'orphan' ON a.id=p.account_id;",
			"SELECT a.id FROM accounts a 'orphan'",
			"SELECT a.id FROM accounts a 'orphan';",
			"SELECT a.id FROM accounts a JOIN payments p ON a.id = 'orphan' p.account_id;",
		} {
			t.Run(dialect+"/"+source, func(t *testing.T) {
				items, valid := sqlNavAllOccurrences(source, dialect)
				if valid {
					for _, item := range items {
						if item.isColumn {
							t.Fatalf("elided ON literal made a false column: %+v", item)
						}
					}
				}
			})
		}
	}
}

func TestSQLSelectAliasConsumedLiteralMarkersFailClosed(t *testing.T) {
	// In the tokenizer, SQL string contents are elided, but their presence
	// must invalidate every source/alias token consumed by sqlNavName.
	// An invalid FROM/JOIN must never acquire a physical column identity.
	cases := []struct {
		name, source string
	}{
		{"before_source", "SELECT a.id FROM 'orphan' public.accounts a;"},
		{"inside_qualified_name", "SELECT a.id FROM public.'orphan'accounts a;"},
		{"between_source_and_alias", "SELECT a.id FROM public.accounts 'orphan' a;"},
		{"before_explicit_as", "SELECT a.id FROM public.accounts 'orphan' AS a;"},
		{"after_explicit_as", "SELECT a.id FROM public.accounts AS 'orphan' a;"},
		{"before_quoted_alias", "SELECT \"a\".id FROM public.accounts AS 'orphan' \"a\";"},
		{"join_before_source", "SELECT a.id FROM public.accounts a JOIN 'orphan' public.payments p ON p.account_id=a.id;"},
		{"join_inside_name", "SELECT a.id FROM public.accounts a JOIN public.'orphan'payments p ON p.account_id=a.id;"},
		{"join_before_alias", "SELECT a.id FROM public.accounts a JOIN public.payments 'orphan' p ON p.account_id=a.id;"},
		{"join_after_as", "SELECT a.id FROM public.accounts a JOIN public.payments AS 'orphan' p ON p.account_id=a.id;"},
		{"without_semicolon", "SELECT a.id FROM public.accounts 'orphan' a"},
		{"before_alias_then_clause", "SELECT a.id FROM public.accounts 'orphan' a WHERE a.id>0;"},
	}
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		for _, tc := range cases {
			t.Run(dialect+"/"+tc.name, func(t *testing.T) {
				events, valid := sqlNavAllOccurrences(tc.source, dialect)
				if valid {
					for _, ev := range events {
						if ev.isColumn {
							t.Fatalf("elided literal invented a %s column: %+v", dialect, ev)
						}
					}
				}
				if at := strings.Index(tc.source, "a.id"); at >= 0 {
					if chosen, ok := sqlNavSelected(tc.source, dialect, 0, at+2); ok && chosen.isColumn {
						t.Fatalf("elided literal exposed a selectable %s column: %+v", dialect, chosen)
					}
				}
			})
		}
	}
}

func TestSQLSelectAliasUnrecognizedSourceGapsFailClosed(t *testing.T) {
	// Do not interpret a valid-looking table binding from an incomplete or
	// unconsumed FROM/JOIN grammar. ON has its own bounded expression grammar.
	cases := []struct {
		name, source string
	}{
		{"unknown_at_end", "SELECT a.id FROM public.accounts a JUNK;"},
		{"unknown_before_join", "SELECT a.id FROM public.accounts a JUNK JOIN public.payments p ON p.account_id=a.id;"},
		{"extra_alias", "SELECT a.id FROM public.accounts a b;"},
		{"repeated_as", "SELECT a.id FROM public.accounts AS a AS b;"},
		{"extra_explicit_alias", "SELECT a.id FROM public.accounts AS a b;"},
		{"unexpected_period", "SELECT a.id FROM public.accounts a . other;"},
		{"unexpected_punctuation", "SELECT a.id FROM public.accounts a ! JOIN public.payments p ON p.account_id=a.id;"},
		{"junk_between_join_kind", "SELECT a.id FROM public.accounts a LEFT NOISE JOIN public.payments p ON p.account_id=a.id;"},
		{"junk_after_outer", "SELECT a.id FROM public.accounts a LEFT OUTER NOISE JOIN public.payments p ON p.account_id=a.id;"},
		{"junk_before_on", "SELECT a.id FROM public.accounts a JOIN public.payments p NOISE ON p.account_id=a.id;"},
		{"second_join_without_on", "SELECT a.id FROM public.accounts a JOIN public.payments p JOIN more m ON m.id=a.id;"},
		{"junk_between_chained_joins", "SELECT a.id FROM public.accounts a JOIN public.payments p ON p.account_id=a.id INNER NOISE JOIN public.payments q ON q.account_id=a.id;"},
		{"dangling_join_modifier", "SELECT a.id FROM public.accounts a RIGHT NOISE;"},
		{"invalid_no_semicolon", "SELECT a.id FROM public.accounts a JUNK"},
	}
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		for _, tc := range cases {
			t.Run(dialect+"/"+tc.name, func(t *testing.T) {
				events, valid := sqlNavAllOccurrences(tc.source, dialect)
				if valid {
					for _, ev := range events {
						if ev.isColumn {
							t.Fatalf("unconsumed %s source grammar invented a column: %+v", dialect, ev)
						}
					}
				}
				if at := strings.Index(tc.source, "a.id"); at >= 0 {
					if chosen, ok := sqlNavSelected(tc.source, dialect, 0, at+2); ok && chosen.isColumn {
						t.Fatalf("invalid %s source gave selectable column: %+v", dialect, chosen)
					}
				}
			})
		}
	}
}

func TestSQLSelectAliasProjectionLiteralsAndQuotedSources(t *testing.T) {
	cases := []struct {
		dialect, source, columnIdentity string
	}{
		{"postgres", `SELECT 'a.id' AS msg, "A".id FROM "public"."accounts" /* quote ' only in comment */ AS "A" JOIN public.payments p ON ("A".id = p.account_id);`, "public\x00accounts\x00column\x00id"},
		{"mysql", "SELECT 'a.id' AS msg, a.`id` FROM `accounts` /* quote ' only in comment */ AS a JOIN payments p ON (a.`id` = p.account_id);", "accounts\x00column\x00id"},
		{"mariadb", "SELECT 'a.id' AS msg, a.`id` FROM `accounts` /* quote ' only in comment */ AS a JOIN payments p ON (a.`id` = p.account_id);", "accounts\x00column\x00id"},
	}
	for _, tc := range cases {
		t.Run(tc.dialect, func(t *testing.T) {
			events, valid := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !valid {
				t.Fatal("supported source became unparseable")
			}
			count := 0
			for _, ev := range events {
				if ev.isColumn && ev.identity == tc.columnIdentity {
					count++
				}
			}
			if count != 2 {
				t.Fatalf("legitimate source-backed projection/ON references = %d, want 2: %+v", count, events)
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

// An SQL-qualified function identifier is not a physical table column, even
// if the prefix matches a currently bound table alias. Nearby real columns
// must remain navigable, including quoted identifiers and whitespace/comments.
func TestSQLSelectAliasQualifiedFunctionIsNotColumn(t *testing.T) {
	for _, tc := range []struct {
		dialect, source, function string
		want                      int
	}{
		{"postgres", `SELECT a.id() FROM public.accounts a;`, "a.id", 0},
		{"postgres", `SELECT a.id (), a.id FROM public.accounts a;`, "a.id", 1},
		{"postgres", `SELECT a.id /* comment */ (), a.id FROM public.accounts a;`, "a.id", 1},
		{"postgres", `SELECT "A"."ID"(), "A"."ID" FROM public.accounts AS "A";`, `"A"."ID"`, 1},
		{"mysql", "SELECT a.`id`(), a.`id` FROM accounts a;", "a.`id`", 1},
		{"mariadb", "SELECT a.`id`() FROM accounts a;", "a.`id`", 0},
	} {
		t.Run(tc.dialect+"/"+tc.source, func(t *testing.T) {
			items, ok := sqlNavAllOccurrences(tc.source, tc.dialect)
			if !ok {
				t.Fatal("source unexpectedly invalid")
			}
			found := 0
			for _, item := range items {
				if item.isColumn {
					found++
				}
			}
			if found != tc.want {
				t.Fatalf("qualified function captured as table column: got %d want %d in %+v", found, tc.want, items)
			}
			colAt := strings.Index(tc.source, tc.function) + len(tc.function) - 1
			if colAt < 0 {
				t.Fatal("missing function")
			}
			selected, ok := sqlNavSelected(tc.source, tc.dialect, 0, colAt)
			if ok && selected.isColumn {
				t.Fatalf("function identifier selected as column: %+v", selected)
			}
		})
	}
}

func TestSQLSelectAliasNeverReturnsPartialOutput(t *testing.T) {
	// A capped SELECT must not quietly return a subset of references. The
	// wider source graph treats an unsupported excessive result as unknown.
	over := "SELECT " + strings.Repeat("a.id, ", sqlNavigationMaxLocations) +
		"a.id FROM accounts a;"
	got, valid := sqlNavAllOccurrences(over, "postgres")
	if !valid {
		t.Fatal("ordinary SQL source became unparseable")
	}
	for _, item := range got {
		if item.isColumn {
			t.Fatalf("large SELECT returned misleading partial refs: %+v", item)
		}
	}
	allowed := "SELECT " + strings.Repeat("a.id, ", sqlNavigationMaxLocations-1) +
		"a.id FROM accounts a;"
	items, ok := sqlNavAllOccurrences(allowed, "postgres")
	if !ok {
		t.Fatal("boundary SQL source became unparseable")
	}
	found := 0
	for _, item := range items {
		if item.isColumn {
			found++
		}
	}
	if found != sqlNavigationMaxLocations {
		t.Fatalf("at-cap references lost: %d", found)
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
		"migrations/001_accounts.sql":    "CREATE TABLE public.accounts (id BIGINT PRIMARY KEY, name TEXT);\n",
		"migrations/002_payments.sql":    "CREATE TABLE public.payments (id BIGINT PRIMARY KEY, account_id BIGINT REFERENCES public.accounts(id));\n",
		"migrations/003_quoted_only.sql": "CREATE TABLE \"only\" (id BIGINT PRIMARY KEY);\n",
		"queries/list.sql":               "SELECT a.id, p.account_id FROM public.accounts AS a JOIN public.payments p ON p.account_id = a.id WHERE a.id > 0;\n",
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
	// This context spans the entire cross-file matrix, not one Worker call.
	// Keep the independently bounded production search/read deadlines intact;
	// avoid converting cumulative validation-host latency into a false
	// source-navigation failure as the matrix grows.
	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
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
	// Regression for the author-separated review: even when a real
	// source-backed accounts.name exists in a distinct migration, SQL
	// table-column aliases rename physical columns positionally. A query
	// that says a.name after AS a (name, id) cannot navigate as if
	// accounts.name were its declaration. Likewise an incomplete JOIN ON
	// must not produce source-backed definitions or references.
	badQueries := []struct {
		name, source, selected string
	}{
		{"pg_positional_alias", "SELECT a.name FROM public.accounts AS a (name, id);\n", "a.name"},
		{"pg_quoted_positional_alias", `SELECT a."name" FROM public.accounts AS a ("name", "id");` + "\n", `a."name"`},
		{"join_positional_alias", "SELECT p.account_id FROM public.accounts a JOIN public.payments AS p (account_id, id) ON p.account_id = a.id;\n", "p.account_id"},
		{"join_missing_on_predicate", "SELECT a.id FROM public.accounts AS a JOIN public.payments p ON;\n", "a.id"},
		{"join_empty_parenthesized_on", "SELECT a.id FROM public.accounts AS a JOIN public.payments p ON ();\n", "a.id"},
		{"join_interior_bad_operand", "SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = AND p.account_id;\n", "a.id"},
		{"join_conflicting_modifiers", "SELECT a.id FROM public.accounts a LEFT RIGHT JOIN public.payments p ON a.id=p.account_id;\n", "a.id"},
		{"join_spaced_comparator", "SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id < > p.account_id;\n", "a.id"},
		{"join_elided_string_literal", "SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = 'orphan' p.account_id;\n", "a.id"},
		{"join_trailing_string_literal", "SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id = p.account_id 'orphan';\n", "a.id"},
		{"function_qualified_identifier", "SELECT a.id() FROM public.accounts a;\n", "a.id"},
		{"from_literal_inside_name", "SELECT a.id FROM public.'orphan'accounts AS a;\n", "a.id"},
		{"from_literal_before_source", "SELECT a.id FROM 'orphan' public.accounts a;\n", "a.id"},
		{"from_literal_before_alias", "SELECT a.id FROM public.accounts 'orphan' a;\n", "a.id"},
		{"from_literal_after_as", "SELECT a.id FROM public.accounts AS 'orphan' a;\n", "a.id"},
		{"join_literal_before_alias", "SELECT a.id FROM public.accounts a JOIN public.payments 'orphan' p ON p.account_id=a.id;\n", "a.id"},
		{"from_unknown_gap_single", "SELECT a.id FROM public.accounts a JUNK;\n", "a.id"},
		{"from_unknown_gap_before_join", "SELECT a.id FROM public.accounts a JUNK JOIN public.payments p ON p.account_id=a.id;\n", "a.id"},
		{"join_unknown_gap_before_on", "SELECT a.id FROM public.accounts a JOIN public.payments p NOISE ON p.account_id=a.id;\n", "a.id"},
		{"join_unknown_modifier", "SELECT a.id FROM public.accounts a LEFT NOISE JOIN public.payments p ON p.account_id=a.id;\n", "a.id"},
		{"repeated_as_alias", "SELECT a.id FROM public.accounts AS a AS b;\n", "a.id"},
		{"pg_only_physical_misbinding", "SELECT accounts.id FROM ONLY accounts;\n", "accounts.id"},
		{"pg_only_modifier_source_token", "SELECT accounts.id FROM ONLY accounts;\n", "ONLY"},
		{"pg_join_only_modifier_source_token", "SELECT accounts.id FROM public.payments p JOIN ONLY accounts ON p.account_id=accounts.id;\n", "ONLY"},
		{"pg_reserved_fk_source_token", "CREATE TABLE demo (id BIGINT REFERENCES ONLY(id));\n", "ONLY"},
		{"pg_only_join_misbinding", "SELECT accounts.id FROM public.payments p JOIN ONLY accounts ON p.account_id=accounts.id;\n", "accounts.id"},
		{"pg_reserved_table_keyword", "SELECT accounts.id FROM TABLE accounts;\n", "accounts.id"},
		{"pg_typefunc_keyword", "SELECT accounts.id FROM LEFT accounts;\n", "accounts.id"},
		{"pg_reserved_only_on_operand", "SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id=ONLY;\n", "a.id"},
		{"pg_reserved_table_on_operand", "SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id=TABLE;\n", "a.id"},
		{"r1_reserved_leading_column", `SELECT ONLY.id FROM "only";` + "\n", "ONLY.id"},
		{"r2_invalid_create_view_on", "CREATE VIEW bad AS SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id=ONLY;\n", "public.accounts"},
		{"r2_invalid_create_view_gap", "CREATE VIEW bad AS SELECT a.id FROM public.accounts a JUNK JOIN public.payments p ON a.id=p.id;\n", "public.accounts"},
		{"r2_invalid_insert_select", "INSERT INTO archive (id) SELECT a.id FROM public.accounts a JOIN public.payments p ON a.id=ONLY;\n", "public.accounts"},
		{"f1_qualified_literal_in_projection", "SELECT a.'ignored'id FROM public.accounts a;\n", "id"},
		{"f1_qualified_literal_in_where", "SELECT a.id FROM public.accounts a WHERE a.'ignored'id=1;\n", "a.id"},
		{"f2_unproven_view_as_body", "CREATE VIEW invalid_view AS FROBNICATE;\n", "invalid_view"},
		{"f2_function_source_with_bad_on", "CREATE VIEW invalid_view AS SELECT a.id FROM generate_series(1,2) g JOIN public.accounts a ON a.id=ONLY;\n", "public.accounts"},
		{"f3_unproven_index_suffix", "CREATE INDEX ix_bad ON public.accounts (id) ONLY;\n", "public.accounts"},
		{"f3_unproven_alter_action", "ALTER TABLE public.accounts ADD COLUMN okay INT, BOGUS REFERENCES public.payments(id);\n", "public.accounts"},
		{"f4_projection_double_alias", "SELECT a.id AS uid EXTRA FROM public.accounts a;\n", "a.id"},
		{"f4_projection_unknown_tokens", "SELECT a.id JUNK EXTRA FROM public.accounts a;\n", "a.id"},
		{"r1_predicate_discarded_literal", "SELECT a.id FROM public.accounts a WHERE a.id=1 'discarded';\n", "a.id"},
		{"r1_bad_index_predicate_literal", "CREATE INDEX ix_bad ON public.accounts (id) WHERE id=1 'discarded';\n", "public.accounts"},
		{"r3_insert_partial_body", "INSERT INTO public.accounts (id) FROBNICATE;\n", "public.accounts"},
		{"r3_update_partial_body", "UPDATE public.accounts FROBNICATE;\n", "public.accounts"},
		{"r3_delete_partial_body", "DELETE FROM public.accounts FROBNICATE;\n", "public.accounts"},
		{"r5_invalid_index_name", "CREATE INDEX ONLY ON public.accounts (id);\n", "public.accounts"},
		{"r5_invalid_pg_index_rename", "ALTER TABLE public.accounts RENAME INDEX old TO newer;\n", "public.accounts"},
	}
	for _, tc := range badQueries {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(workspace, owner.Path), []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			owner.FileVersion = version(owner.Path)
			owner.Character = strings.Index(tc.source, tc.selected) + strings.LastIndex(tc.selected, ".") + 2
			if owner.Character < 2 {
				t.Fatalf("missing selected token: %q", tc.selected)
			}
			for _, method := range []string{"definition", "references"} {
				owner.CodeMethod = method
				if selected, ok := sqlNavSelected(tc.source, owner.SQLDialect, 0, owner.Character); ok && selected.isColumn {
					t.Fatalf("unsupported SQL exposed a fake %s column binding: %+v", method, selected)
				}
				response, handled := s.sqlOfflineNavigation(ctx, owner, owner.Path, tc.source)
				if handled || response.OK {
					t.Fatalf("unsupported %s SQL produced a source-backed target from migration: %+v", method, response)
				}
				// If no independent optional SQL language server is installed,
				// the public entrypoint must also reject, not return the wrong
				// migration target. An installed SQLLens remains independent.
				if _, sqlErr := systemexec.SQLLens(); sqlErr != nil {
					public := s.codeInspect(ctx, owner)
					if public.OK {
						t.Fatalf("unsupported %s SQL navigated to a fabricated migration target: %+v", method, public)
					}
				}
			}
		})
	}
	// A real table named "only" is present alongside public.accounts.
	// PostgreSQL's unquoted ONLY modifier above must not bind to it, but
	// a deliberately quoted table name must remain fully navigable.
	quotedOnly := "SELECT o.id FROM \"only\" AS o;\n"
	if err := os.WriteFile(filepath.Join(workspace, owner.Path), []byte(quotedOnly), 0600); err != nil {
		t.Fatal(err)
	}
	owner.FileVersion = version(owner.Path)
	owner.Character = strings.Index(quotedOnly, "o.id") + 2
	owner.CodeMethod = "definition"
	resp = s.codeInspect(ctx, owner)
	if !resp.OK || json.Unmarshal([]byte(resp.Output), &locs) != nil || len(locs) != 1 ||
		locs[0].URI != "migrations/003_quoted_only.sql" ||
		locs[0].FileVersion != version(locs[0].URI) {
		t.Fatalf("quoted PostgreSQL only table did not resolve to its own declaration: %+v %+v", resp, locs)
	}
	owner.CodeMethod = "references"
	resp = s.codeInspect(ctx, owner)
	if !resp.OK || json.Unmarshal([]byte(resp.Output), &locs) != nil || len(locs) != 2 {
		t.Fatalf("quoted PostgreSQL only table references lost: %+v %+v", resp, locs)
	}

	// F2/F3 real Worker/Landlock: invalid declarations cannot become
	// genuine cross-file code_definition or code_references sources,
	// including an invalid view body and complete constraint grammar.
	invalidDDLPath := filepath.Join(workspace, "migrations/004_invalid_partial.sql")
	for _, tc := range []struct{ name, ddl, table string }{
		{"reserved_column", "CREATE TABLE impossible (id BIGINT, ONLY BIGINT);\n", "impossible"},
		{"incomplete_primary", "CREATE TABLE impossible (id BIGINT, PRIMARY KEY ONLY);\n", "impossible"},
		{"f5_structural_datatype", "CREATE TABLE impossible (id SELECT);\n", "impossible"},
		{"f5_incomplete_default", "CREATE TABLE impossible (id INT DEFAULT +);\n", "impossible"},
		{"unknown_alter_like_ddl", "CREATE TABLE impossible (id BIGINT, UNIQUE x);\n", "impossible"},
		{"bad_view_body", "CREATE VIEW impossible AS FROBNICATE;\n", "impossible"},
		{"bad_view_function_join", "CREATE VIEW impossible AS SELECT * FROM generate_series(1,2) g JOIN public.accounts a ON a.id=ONLY;\n", "impossible"},
		{"r1_literal_after_constraint", "CREATE TABLE impossible (id BIGINT, PRIMARY KEY (id) 'discarded');\n", "impossible"},
		{"r2_invalid_scalar_view", "CREATE VIEW impossible AS SELECT +;\n", "impossible"},
		{"r4_reserved_datatype_with", "CREATE TABLE impossible (id WITH);\n", "impossible"},
		{"r4_reserved_datatype_only", "CREATE TABLE impossible (id ONLY);\n", "impossible"},
		{"extra_materialized_suffix_literal", "CREATE MATERIALIZED VIEW impossible AS SELECT 1 'discarded' WITH DATA;\n", "impossible"},
	} {
		t.Run("f2_f3_crossfile_"+tc.name, func(t *testing.T) {
			if err := os.WriteFile(invalidDDLPath, []byte(tc.ddl), 0600); err != nil {
				t.Fatal(err)
			}
			query := "SELECT i.id FROM " + tc.table + " i;\n"
			if err := os.WriteFile(filepath.Join(workspace, owner.Path), []byte(query), 0600); err != nil {
				t.Fatal(err)
			}
			owner.FileVersion = version(owner.Path)
			owner.Character = strings.Index(query, "i.id") + 2
			for _, method := range []string{"definition", "references"} {
				owner.CodeMethod = method
				response := s.codeInspect(ctx, owner)
				if response.OK {
					var targets []sqlNavLocation
					if err := json.Unmarshal([]byte(response.Output), &targets); err != nil {
						t.Fatalf("malformed %s locations: %v", method, err)
					}
					for _, target := range targets {
						if target.URI == "migrations/004_invalid_partial.sql" {
							t.Fatalf("invalid %s migration became %s target: %+v", tc.name, method, response)
						}
					}
				}
			}
		})
	}
	if err := os.Remove(invalidDDLPath); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workspace, owner.Path), []byte(files[owner.Path]), 0600); err != nil {
		t.Fatal(err)
	}
	owner.FileVersion = version(owner.Path)
	owner.CodeMethod = "definition"
	owner.Character = strings.Index(files[owner.Path], "a.id") + 2
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
