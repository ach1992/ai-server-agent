package executor

import (
	"strings"
	"testing"
)

// dbmate applies only -- migrate:up sections on forward migration. The
// rollback section must not fabricate SQL schema declarations or lifecycle
// tombstones in the source-only graph.
func TestSQLDBMateRollbackDoesNotBecomeForwardSchema(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		t.Run(dialect, func(t *testing.T) {
			source := "-- migrate:up\nCREATE TABLE accounts (id BIGINT PRIMARY KEY);\n" +
				"-- migrate:down\nDROP TABLE accounts;\nCREATE TABLE phantom_rollback (id INT);\n"
			occ, valid := sqlNavOccurrences(source, dialect)
			if !valid || len(occ) != 1 || !occ[0].declaration ||
				occ[0].name != "accounts" || occ[0].span.Start.Line != 1 {
				t.Fatalf("dbmate rollback contaminated forward schema: valid=%v occurrences=%+v", valid, occ)
			}
			if got := sqlDDLSymbols(source, dialect); len(got) != 1 ||
				got[0].Name != "accounts" || got[0].Range.Start.Line != 1 {
				t.Fatalf("dbmate rollback contaminated source-only DDL symbols: %+v", got)
			}
			if _, selected := sqlNavSelected(source, dialect, 4, 19); selected {
				t.Fatal("rollback-only CREATE was selectable as forward schema")
			}
		})
	}
}

// dbmate supports repeated Up/Down blocks and the documented transaction
// option. The source graph must include each Up block at the original offsets.
func TestSQLDBMateRepeatedForwardBlocksAndOffsets(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		t.Run(dialect, func(t *testing.T) {
			source := "-- migrate:up transaction:false\r\n" +
				"CREATE TABLE accounts (id BIGINT);\r\n" +
				"-- migrate:down transaction:false\r\n" +
				"DROP TABLE accounts;\r\n" +
				"-- migrate:up\r\n" +
				"ALTER TABLE accounts ADD COLUMN active BOOLEAN;\r\n" +
				"-- migrate:down\r\n" +
				"ALTER TABLE accounts DROP COLUMN active;\r\n"
			forward, valid := sqlDDLDBMateForwardSQL(source, dialect)
			if !valid || len(forward) != len(source) {
				t.Fatalf("dbmate CRLF normalization changed byte offsets: valid=%t, source=%d forward=%d", valid, len(source), len(forward))
			}
			if strings.Contains(forward, "DROP TABLE") || strings.Contains(forward, "DROP COLUMN") ||
				!strings.Contains(forward, "CREATE TABLE accounts") ||
				!strings.Contains(forward, "ALTER TABLE accounts ADD COLUMN") {
				t.Fatalf("dbmate forward sections missing or rollback included: %q", forward)
			}
			occ, valid := sqlNavOccurrences(source, dialect)
			if !valid || len(occ) != 2 || !occ[0].declaration || occ[0].drop ||
				occ[0].name != "accounts" || occ[0].span.Start.Line != 1 ||
				occ[1].name != "accounts" || occ[1].declaration || occ[1].drop ||
				occ[1].span.Start.Line != 5 {
				t.Fatalf("wrong repeated Up source graph: valid=%t occurrences=%+v", valid, occ)
			}
			if _, found := sqlNavSelected(source, dialect, 7, 18); found {
				t.Fatal("Down-only column name was selectable")
			}
		})
	}
}

func TestSQLDBMateOrderedMigrationHistoryIgnoresRollback(t *testing.T) {
	firstSource := "-- migrate:up\nCREATE TABLE accounts (id INT);\n-- migrate:down\nDROP TABLE accounts;\n"
	secondSource := "-- migrate:up\nALTER TABLE accounts ADD COLUMN active BOOLEAN;\n" +
		"-- migrate:down\nDROP TABLE accounts;\n"
	var found []sqlNavFound
	for _, item := range []struct{ path, source string }{
		{"db/migrations/202610160001_create.sql", firstSource},
		{"db/migrations/202610160002_alter.sql", secondSource},
	} {
		occ, ok := sqlNavOccurrences(item.source, "postgres")
		if !ok || len(occ) != 1 {
			t.Fatalf("unexpected source graph for %s: %+v", item.path, occ)
		}
		for _, event := range occ {
			found = append(found, sqlNavFound{
				sqlNavOccurrence: event,
				sqlNavLocation: sqlNavLocation{
					URI: item.path, Range: event.span, FileVersion: "sha:fixture",
				},
			})
		}
	}
	got, err := sqlNavResolveOrderedDefinition(found, nil, "queries/read.sql")
	if err != nil || got.URI != "db/migrations/202610160001_create.sql" ||
		got.Range.Start.Line != 1 {
		t.Fatalf("dbmate Down falsely invalidated the forward CREATE: location=%+v err=%v", got, err)
	}
}

func TestSQLDBMateRejectsMalformedOrAmbiguousAnnotations(t *testing.T) {
	base := "-- migrate:up\nCREATE TABLE accounts (id INT);\n-- migrate:down\nDROP TABLE accounts;\n"
	for _, source := range []string{
		"-- migrate:down\nDROP TABLE accounts;\n",
		"-- migrate:up\nCREATE TABLE accounts (id INT);\n", // unpaired block
		"-- migrate:up\nCREATE TABLE accounts (id INT);\n-- migrate:up\nCREATE TABLE second (id INT);\n-- migrate:down\n",
		"-- migrate:up\nCREATE TABLE accounts (id INT);\n-- migrate:down\nDROP TABLE accounts;\n-- migrate:down\n",
		"-- migrate:up transaction:unsafe\nCREATE TABLE accounts (id INT);\n-- migrate:down\n",
		"-- migrate:up transaction:off\nCREATE TABLE accounts (id INT);\n-- migrate:down\n",
		"-- migrate:statement-begin\n" + base,
		" -- migrate:up\nCREATE TABLE accounts (id INT);\n-- migrate:down\n",
		"CREATE TABLE prelude (id INT);\n" + base,
		"-- migrate:up\nCREATE TABLE broken (id INT)\n-- migrate:down\nDROP TABLE broken;\n",
		"-- migrate:up\nCREATE TABLE accounts (id INT);\n-- +goose Down\n-- migrate:down\nDROP TABLE accounts;\n",
		strings.Repeat("-- migrate:up\nSELECT 1;\n-- migrate:down\nSELECT 2;\n", 33),
		"/*\n-- migrate:up\n*/\n" + base,
		"SELECT '\n-- migrate:up\nCREATE TABLE phantom (id INT);\n';\n" + base,
	} {
		for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
			got, valid := sqlNavOccurrences(source, dialect)
			if valid || len(got) != 0 || len(sqlDDLSymbols(source, dialect)) != 0 {
				t.Errorf("%s falsely accepted ambiguous dbmate source %q: valid=%t occurrences=%+v", dialect, source, valid, got)
			}
		}
	}
	// A PostgreSQL dollar-quoted literal may contain marker-looking lines.
	// These are not dbmate directives, even at column zero.
	pg := "SELECT $sql$\n-- migrate:up\nCREATE TABLE phantom (id INT);\n$sql$;\n" + base
	if occ, valid := sqlNavOccurrences(pg, "postgres"); valid || len(occ) != 0 {
		t.Fatalf("dbmate marker inside dollar-quoted string was trusted: valid=%t %+v", valid, occ)
	}
}

func TestSQLDBMateOrdinarySQLAndGoosePreserved(t *testing.T) {
	for _, source := range []string{
		"CREATE TABLE ordinary (id INT);\nSELECT '-- migrate:up' AS literal;",
		"-- comment mentioning migrate:up options\nCREATE VIEW normal AS SELECT 1;",
		"-- +goose Up\nCREATE TABLE goose_table (id INT);\n-- +goose Down\nDROP TABLE goose_table;",
	} {
		for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
			forward, valid := sqlDDLDBMateForwardSQL(source, dialect)
			if !valid || forward != source {
				t.Errorf("%s ordinary SQL or Goose source was changed: %q -> %q valid=%t", dialect, source, forward, valid)
			}
		}
	}
}

// A forward view may reference a real table; the rollback view drop must not
// become a lifecycle tombstone or a selectable source symbol.
func TestSQLDBMateViewDependenciesWithoutRollbackTombstones(t *testing.T) {
	source := "-- migrate:up\n" +
		"CREATE VIEW recent_accounts AS SELECT a.id FROM accounts a;\n" +
		"-- migrate:down\nDROP VIEW recent_accounts;\n"
	occ, valid := sqlNavOccurrences(source, "postgres")
	if !valid || len(occ) != 2 ||
		!occ[0].declaration || occ[0].name != "recent_accounts" ||
		occ[1].declaration || occ[1].name != "accounts" {
		t.Fatalf("forward view/table references were not source-backed: valid=%t %+v", valid, occ)
	}
	if _, found := sqlNavSelected(source, "postgres", 3, 13); found {
		t.Fatal("rollback DROP VIEW became a navigable SQL source occurrence")
	}
	if symbols := sqlDDLSymbols(source, "postgres"); len(symbols) != 1 || symbols[0].Name != "recent_accounts" {
		t.Fatalf("rollback view DDL leaked into forward outline: %+v", symbols)
	}
}

func TestSQLDBMateMarkerInPostgresCopyDataFailsClosed(t *testing.T) {
	source := "COPY raw FROM STDIN;\n-- migrate:up\n\\\\.\n" +
		"-- migrate:up\nCREATE TABLE phantom (id INT);\n-- migrate:down\nDROP TABLE phantom;\n"
	items, valid := sqlNavOccurrences(source, "postgres")
	if valid || len(items) != 0 || len(sqlDDLSymbols(source, "postgres")) != 0 {
		t.Fatalf("COPY data masquerading as dbmate directive was trusted: valid=%t %+v", valid, items)
	}
}
