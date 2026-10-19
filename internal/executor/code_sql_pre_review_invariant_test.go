package executor

import "testing"

// Author-side supplemental checks: an apparently valid table/column symbol
// must not survive an unparsed projection or DDL scalar/type tail.
func TestSQLPreReviewStandaloneProjectionCannotPublishPartialGraph(t *testing.T) {
	for _, query := range []string{
		"SELECT a.id JUNK EXTRA FROM accounts a;",
		"SELECT a.id AS output EXTRA FROM accounts a;",
		"SELECT a.id, a.id ALIAS EXTRA FROM accounts a;",
	} {
		t.Run(query, func(t *testing.T) {
			items, ok := sqlNavAllOccurrences(query, "postgres")
			if ok && len(items) > 0 {
				t.Fatalf("unproven projection published navigation: %+v", items)
			}
		})
	}
	for _, query := range []string{
		"SELECT a.id FROM accounts a;",
		"SELECT 'unrelated' AS txt, a.id FROM accounts a;",
		"SELECT a.id, 'unrelated' AS txt FROM accounts a;",
		"SELECT a.id + 1 AS next_id FROM accounts a;",
		"SELECT a.id uid FROM accounts a;",
		"SELECT COUNT(*) AS total, a.id FROM accounts a;",
	} {
		t.Run("legal/"+query, func(t *testing.T) {
			items, ok := sqlNavAllOccurrences(query, "postgres")
			if !ok {
				t.Fatal("lexical failure for valid projection")
			}
			found := false
			for _, item := range items {
				if item.isColumn && item.identity == "accounts\x00column\x00id" {
					found = true
				}
			}
			if !found {
				t.Fatalf("lost existing valid column navigation: %+v", items)
			}
		})
	}
}

func TestSQLPreReviewDDLDefaultAndDatatypeCannotPublishPartialGraph(t *testing.T) {
	for _, query := range []string{
		"CREATE TABLE accounts (id INT DEFAULT +);",
		"CREATE TABLE accounts (id INT DEFAULT *);",
		"CREATE TABLE accounts (id SELECT);",
		"CREATE TABLE accounts (id PRIMARY);",
		"CREATE TABLE accounts (id FROM);",
	} {
		t.Run(query, func(t *testing.T) {
			items, ok := sqlNavAllOccurrences(query, "postgres")
			if ok && len(items) != 0 {
				t.Fatalf("invalid DDL published physical declaration: %+v", items)
			}
		})
	}
	for _, query := range []string{
		"CREATE TABLE accounts (id INT DEFAULT 100);",
		"CREATE TABLE accounts (id INT DEFAULT -7);",
		"CREATE TABLE accounts (id INT DEFAULT 'ready');",
	} {
		t.Run("legal/"+query, func(t *testing.T) {
			items, ok := sqlNavAllOccurrences(query, "postgres")
			if !ok {
				t.Fatal("lexical failure")
			}
			found := false
			for _, item := range items {
				if item.declaration && item.identity == "accounts" {
					found = true
				}
			}
			if !found {
				t.Fatalf("lost ordinary complete DDL: %+v", items)
			}
		})
	}
}
