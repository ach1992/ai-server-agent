package executor

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestSQLDDLSourceOutlineRealMigrations(t *testing.T) {
	for _, tc := range []struct {
		name, dialect, source string
		table, column         string
	}{
		{"postgres_create", "postgres", postgresMigration001, "acceptance_accounts", "payload"},
		{"mysql_create", "mysql", mysqlMigration001, "acceptance_accounts", "label"},
		{"mariadb_create", "mariadb", mysqlMigration001, "acceptance_accounts", "label"},
		{"postgres_alter", "postgresql", postgresMigration002, "acceptance_accounts", ""},
		{"mysql_alter", "mysql", mysqlMigration002, "acceptance_accounts", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sqlDDLSymbols(tc.source, tc.dialect)
			if len(got) == 0 || got[0].Name != tc.table ||
				got[0].Kind != 23 || got[0].Range.End.Line < got[0].Range.Start.Line {
				t.Fatalf("DDL table missing or invalid: %+v", got)
			}
			if tc.column != "" {
				seen := false
				for _, child := range got[0].Children {
					if child.Name == tc.column && child.Kind == 8 {
						seen = true
					}
				}
				if !seen {
					t.Fatalf("DDL source column absent: %+v", got[0].Children)
				}
			}
		})
	}
}

func TestSQLDDLSourceOutlineEscapingAndSourceSpans(t *testing.T) {
	source := "-- CREATE TABLE forged (id int);\n" +
		"SELECT 'CREATE TABLE fake (id int);' AS ignored;\n" +
		"DO $tag$ BEGIN CREATE TABLE secret (x int); END $tag$;\n" +
		"/* CREATE TABLE ignored2 (id int); */\n" +
		"CREATE TABLE IF NOT EXISTS public.\"درخت😀\" (\n" +
		"  \"نام😀\" VARCHAR(50), price DECIMAL(10, 2),\n" +
		"  CONSTRAINT pk PRIMARY KEY (\"نام😀\")\n" +
		");\nALTER TABLE public.\"درخت😀\" ADD COLUMN active BOOLEAN;\n"
	out := sqlDDLSymbols(source, "postgres")
	if len(out) != 2 || out[0].Name != "public.درخت😀" ||
		out[0].SelectionRange.Start.Line != 4 ||
		out[0].SelectionRange.End.Character <= out[0].SelectionRange.Start.Character ||
		out[1].Detail != "ALTER TABLE (offline source)" ||
		len(out[0].Children) != 2 || out[0].Children[0].Name != "نام😀" ||
		out[0].Children[1].Name != "price" {
		t.Fatalf("forged symbols, broken quoted names, constraints, or UTF-16 positions: %+v", out)
	}
	mysql := "/* CREATE TABLE fake (id int) */\nCREATE TABLE `orders` (`order-id` bigint, total DECIMAL(12, 2), PRIMARY KEY (`order-id`)) ENGINE=InnoDB;\n"
	out = sqlDDLSymbols(mysql, "mysql")
	if len(out) != 1 || out[0].Name != "orders" || len(out[0].Children) != 2 ||
		out[0].Children[0].Name != "order-id" {
		t.Fatalf("MySQL qualified identifiers/constraints incorrect: %+v", out)
	}
}

func TestSQLDDLRepeatedTableMigrationStatementsRemainVisible(t *testing.T) {
	source := "CREATE TABLE inventory (id BIGINT); ALTER TABLE inventory ADD COLUMN name TEXT;\n" +
		"ALTER TABLE inventory ADD COLUMN enabled BOOLEAN;"
	result, err := appendSQLDDLSymbols([]byte("[]"), source, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	var values []sqlDDLSymbol
	if err := json.Unmarshal(result, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || values[0].Name != "inventory" ||
		values[1].Name != "inventory" || values[2].Name != "inventory" ||
		values[0].Range.Start == values[1].Range.Start {
		t.Fatalf("repeated DDL statements collapsed: %+v", values)
	}
	again, err := appendSQLDDLSymbols(result, source, "postgres")
	if err != nil || string(again) != string(result) {
		t.Fatalf("repeated postprocessing duplicated symbols: %s %v", again, err)
	}
}

func TestSQLDDLFailClosedOrUnsupportedDoesNotClaimSymbols(t *testing.T) {
	for _, tc := range []struct{ sql, dialect string }{
		{"'CREATE TABLE fake (id int);'", "postgres"},
		{"/* CREATE TABLE fake (id int);", "postgres"},
		{"DO $$ BEGIN CREATE TABLE fake(id int); END $$;", "postgres"},
		{"CREATE TABLE \"unterminated", "postgres"},
		{"CREATE TABLE", "postgres"},
		{"CREATE TABLE fake (id int);", "sqlite"},
		{"CREATE TABLE fake (id int);", ""},
		{"CREATE TABLE fake (id int);", "Postgres"},
	} {
		if got := sqlDDLSymbols(tc.sql, tc.dialect); len(got) != 0 {
			t.Fatalf("unsafe/unsupported SQL fabricated declarations: %q/%q %+v", tc.sql, tc.dialect, got)
		}
	}
}

func TestSQLDDLOutputBoundFailsClosed(t *testing.T) {
	var source strings.Builder
	for i := 0; i < 1800; i++ {
		source.WriteString("CREATE TABLE t")
		source.WriteString(strconv.Itoa(i))
		source.WriteString(" (id INT);\n")
	}
	if source.Len() > codeMaxDocumentBytes {
		t.Fatalf("test source exceeded LSP document budget: %d", source.Len())
	}
	_, err := appendSQLDDLSymbols([]byte("[]"), source.String(), "postgres")
	if !errors.Is(err, errSQLDDLOutputLimit) {
		t.Fatalf("oversized SQL outline returned unbounded or truncated payload: %v", err)
	}
}

func TestAppendSQLDDLSymbolsPreservesCTEsAndDoesNotDuplicate(t *testing.T) {
	sql := "CREATE TABLE customers (id BIGINT PRIMARY KEY);"
	original := []byte(`[{"name":"weekly","kind":13,"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":6}}}]`)
	newData, err := appendSQLDDLSymbols(original, sql, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	var decoded []struct{ Name string }
	if err := json.Unmarshal(newData, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0].Name != "weekly" || decoded[1].Name != "customers" {
		t.Fatalf("lost language-server outline or DDL: %s", newData)
	}
	newData, err = appendSQLDDLSymbols(newData, sql, "mysql")
	if err != nil || strings.Count(string(newData), "\"name\":\"customers\"") != 1 {
		t.Fatalf("duplicate DDL symbol on re-analysis: %s %v", newData, err)
	}
	if _, err := appendSQLDDLSymbols([]byte("{broken"), sql, "mysql"); err == nil {
		t.Fatal("invalid LSP JSON accepted as valid result")
	}
}
