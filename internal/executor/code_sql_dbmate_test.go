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

// R1 REQUIRED-001: noncanonical marker spelling must not silently degrade a
// dbmate migration into plain SQL. Otherwise Down DDL fabricates schema truth.
func TestSQLDBMateMalformedNearMarkersFailClosedAcrossDialects(t *testing.T) {
	migrations := []struct {
		name, up, down string
	}{
		{"no_space_after_comment", "--migrate:up", "--migrate:down"},
		{"space_before_colon", "-- migrate :up", "-- migrate :down"},
		{"mixed_case", "--MIGRATE:up", "--MIGRATE:down"},
		{"tab_after_colon", "--\tmigrate:\tup", "--\tmigrate:\tdown"},
		{"valid_up_bad_down", "-- migrate:up", "--migrate:down"},
		{"bad_up_valid_down", "--migrate:up", "-- migrate:down"},
		{"spaces_before_colon", "-- migrate \t:up", "-- migrate :down"},
	}
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		for _, item := range migrations {
			t.Run(dialect+"_"+item.name, func(t *testing.T) {
				source := item.up + "\nCREATE TABLE accounts (id INT);\n" +
					item.down + "\nDROP TABLE accounts;\nCREATE TABLE phantom_rollback (id INT);\n"
				if output, ok := sqlDDLDBMateForwardSQL(source, dialect); ok {
					t.Fatalf("malformed dbmate marker bypassed fail-closed mode: %q", output)
				}
				if occurrences, valid := sqlNavOccurrences(source, dialect); valid || len(occurrences) != 0 {
					t.Fatalf("malformed markers fabricated navigation: valid=%t %+v", valid, occurrences)
				}
				if symbols := sqlDDLSymbols(source, dialect); len(symbols) != 0 {
					t.Fatalf("malformed markers fabricated DDL outline: %+v", symbols)
				}
			})
		}
	}
}

func TestSQLDBMateMalformedMarkerMixedWithGooseFailsClosed(t *testing.T) {
	sources := []string{
		"-- +goose Up\nCREATE TABLE goose_table (id INT);\n" +
			"--migrate:up\nCREATE TABLE phantom (id INT);\n" +
			"-- +goose Down\nDROP TABLE goose_table;\n" +
			"--migrate:down\nDROP TABLE phantom;\n",
		"--migrate:up\nCREATE TABLE phantom (id INT);\n" +
			"-- +goose Up\nCREATE TABLE goose_table (id INT);\n" +
			"-- +goose Down\nDROP TABLE goose_table;\n" +
			"--migrate:down\nDROP TABLE phantom;\n",
	}
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		for i, source := range sources {
			if forward, valid := sqlDDLDBMateForwardSQL(source, dialect); valid {
				t.Errorf("%s mixed runners[%d] accepted: %q", dialect, i, forward)
			}
			if occ, valid := sqlNavOccurrences(source, dialect); valid || len(occ) != 0 {
				t.Errorf("%s mixed runners[%d] navigation leak: valid=%t %+v", dialect, i, valid, occ)
			}
			if symbols := sqlDDLSymbols(source, dialect); len(symbols) != 0 {
				t.Errorf("%s mixed runners[%d] DDL leak: %+v", dialect, i, symbols)
			}
		}
	}
}

// R1 REQUIRED-002: the CLI DELIMITER mode changes MySQL statement boundaries;
// a CREATE TABLE nested in a routine body is not a top-level declaration.
func TestSQLDBMateMySQLDelimiterProcedureFailsClosedAcrossConsumers(t *testing.T) {
	source := "-- migrate:up\n" +
		"DELIMITER $$\nCREATE PROCEDURE migrate_accounts()\n" +
		"BEGIN\n SELECT 1;\n CREATE TABLE phantom (id INT);\nEND$$\n" +
		"DELIMITER ;\n" +
		"-- migrate:down\nDROP PROCEDURE migrate_accounts;\n"
	plain := "DELIMITER $$\nCREATE PROCEDURE migrate_accounts()\n" +
		"BEGIN\n SELECT 1;\n CREATE TABLE phantom (id INT);\nEND$$\nDELIMITER ;\n"
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, item := range []struct{ name, source string }{
			{"dbmate_procedure", source},
			{"plain_procedure", plain},
		} {
			t.Run(dialect+"_"+item.name, func(t *testing.T) {
				if tokens, valid := sqlDDLTokens(item.source, dialect); valid || len(tokens) != 0 {
					t.Fatalf("DELIMITER mode must be rejected by shared lexer: valid=%t tokens=%+v", valid, tokens)
				}
				if symbols := sqlDDLSymbols(item.source, dialect); len(symbols) != 0 {
					t.Fatalf("procedure body invented a CREATE TABLE outline: %+v", symbols)
				}
				if occ, valid := sqlNavOccurrences(item.source, dialect); valid || len(occ) != 0 {
					t.Fatalf("DELIMITER mode invented a source graph: valid=%t %+v", valid, occ)
				}
			})
		}
	}
}

// Horizontal tabs in the documented whitespace positions remain legitimate
// dbmate directives; malformed-near-marker rejection is not a broad ban on
// valid alternate whitespace.
func TestSQLDBMateValidCommentTabWhitespacePreserved(t *testing.T) {
	source := "--\tmigrate:up\nCREATE TABLE accounts (id INT);\n" +
		"--\tmigrate:down\nDROP TABLE accounts;\n"
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		occurrences, ok := sqlNavOccurrences(source, dialect)
		if !ok || len(occurrences) != 1 || !occurrences[0].declaration ||
			occurrences[0].name != "accounts" {
			t.Errorf("%s legal tab separator rejected: ok=%t occurrences=%+v", dialect, ok, occurrences)
		}
		if symbols := sqlDDLSymbols(source, dialect); len(symbols) != 1 ||
			symbols[0].Name != "accounts" {
			t.Errorf("%s legal tab separator lost DDL outline: %+v", dialect, symbols)
		}
	}
}

// R2 independent review REQUIRED-003: compound MySQL/MariaDB stored program
// bodies do not need a client DELIMITER command. Nested semicolons must never
// manufacture independent source-backed schema declarations or navigation.
func TestSQLMySQLCompoundRoutineWithoutDelimiterFailsClosed(t *testing.T) {
	routine := "CREATE PROCEDURE prepare_accounts()\n" +
		"BEGIN\n SELECT 1;\n CREATE TABLE phantom (id INT);\nEND;\n"
	cases := []struct {
		name, source string
	}{
		{"plain_procedure", routine},
		{"dbmate_procedure", "-- migrate:up\n" + routine +
			"-- migrate:down\nDROP PROCEDURE prepare_accounts;\n"},
		{"goose_procedure", "-- +goose Up\n" + routine +
			"-- +goose Down\nDROP PROCEDURE prepare_accounts;\n"},
		{"create_or_replace_procedure", "CREATE OR REPLACE " + routine[len("CREATE "):]},
		{"create_definer_procedure", "CREATE DEFINER = CURRENT_USER " + routine[len("CREATE "):]},
		{"create_definer_quoted_procedure", "CREATE DEFINER = 'service'@'localhost' " + routine[len("CREATE "):]},
		{"procedure_single_statement", "CREATE PROCEDURE prepare_accounts() SELECT 1;\n"},
		{"function", "CREATE FUNCTION calculate_total() RETURNS INT\n" +
			"BEGIN\n SELECT 1;\n CREATE TABLE phantom (id INT);\n RETURN 1;\nEND;\n"},
		{"trigger", "CREATE TRIGGER audit_insert BEFORE INSERT ON accounts\n" +
			"FOR EACH ROW BEGIN\n SELECT 1;\n CREATE TABLE phantom (id INT);\nEND;\n"},
		{"event", "CREATE EVENT nightly_cleanup ON SCHEDULE EVERY 1 DAY DO\n" +
			"BEGIN\n SELECT 1;\n CREATE TABLE phantom (id INT);\nEND;\n"},
		{"anonymous_compound", "BEGIN NOT ATOMIC\nSELECT 1;\n" +
			"CREATE TABLE phantom (id INT);\nEND;\n"},
	}
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, tc := range cases {
			t.Run(dialect+"_"+tc.name, func(t *testing.T) {
				if tokens, valid := sqlDDLTokens(tc.source, dialect); valid || len(tokens) != 0 {
					t.Fatalf("unsupported routine/body accepted by shared lexer: valid=%t tokens=%+v", valid, tokens)
				}
				if symbols := sqlDDLSymbols(tc.source, dialect); len(symbols) != 0 {
					t.Fatalf("nested procedural CREATE fabricated forward DDL: %+v", symbols)
				}
				if occ, valid := sqlNavOccurrences(tc.source, dialect); valid || len(occ) != 0 {
					t.Fatalf("nested procedural CREATE fabricated source navigation: valid=%t %+v", valid, occ)
				}
				if _, found := sqlNavSelected(tc.source, dialect, 3, 16); found {
					t.Fatal("cursor navigation selected a phantom routine-body table")
				}
			})
		}
	}
}

func TestSQLMySQLCompoundGuardPreservesPlainDDLAndQuotedKeywords(t *testing.T) {
	plain := "CREATE TABLE accounts (id INT, note VARCHAR(255) DEFAULT 'CREATE PROCEDURE fake BEGIN END');\n" +
		"-- CREATE PROCEDURE ignored() BEGIN CREATE TABLE phantom (id INT); END;\n" +
		"ALTER TABLE accounts ADD COLUMN active BOOLEAN;\n"
	dbmate := "-- migrate:up\n" + plain +
		"-- migrate:down\nDROP TABLE accounts;\n"
	goose := "-- +goose Up\n" + plain +
		"-- +goose Down\nDROP TABLE accounts;\n"
	transaction := "BEGIN;\nCREATE TABLE accounts (id INT);\nCOMMIT;\n"
	transactionWork := "BEGIN WORK;\nCREATE TABLE accounts (id INT);\nCOMMIT;\n"
	rollbackOnlyProcedure := "-- migrate:up\nCREATE TABLE accounts (id INT);\n" +
		"-- migrate:down\nCREATE PROCEDURE rollback_only() BEGIN SELECT 1; END;\n"
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, tc := range []struct{ name, source string }{
			{"plain", plain}, {"dbmate", dbmate}, {"goose", goose}, {"transaction", transaction}, {"transaction_work", transactionWork}, {"dbmate_rollback_only_procedure", rollbackOnlyProcedure},
		} {
			t.Run(dialect+"_"+tc.name, func(t *testing.T) {
				symbols := sqlDDLSymbols(tc.source, dialect)
				if len(symbols) == 0 || symbols[0].Name != "accounts" {
					t.Fatalf("legitimate CREATE was suppressed: %+v", symbols)
				}
				occ, valid := sqlNavOccurrences(tc.source, dialect)
				if !valid || len(occ) == 0 || occ[0].name != "accounts" || !occ[0].declaration {
					t.Fatalf("legitimate source navigation was suppressed: valid=%t %+v", valid, occ)
				}
			})
		}
	}
	postgres := "CREATE FUNCTION ignored() RETURNS void LANGUAGE plpgsql AS $$\n" +
		"BEGIN\nCREATE TABLE phantom (id INT);\nEND;\n$$;\n" +
		"CREATE TABLE accounts (id INT);\n"
	if symbols := sqlDDLSymbols(postgres, "postgres"); len(symbols) != 1 || symbols[0].Name != "accounts" {
		t.Fatalf("PostgreSQL function-body isolation regressed: %+v", symbols)
	}
}

// R3 independent review REQUIRED-004: MySQL/MariaDB executable comments are
// grammar, not disposable trivia. A routine header hidden in one can turn
// the following CREATE TABLE into a procedure body, not a top-level table.
func TestSQLMySQLExecutableCommentsRejectPhantomForwardSchema(t *testing.T) {
	header := "/*!50003 CREATE PROCEDURE prepare_accounts() */\n"
	body := "CREATE TABLE phantom (id INT);\n"
	cases := []struct {
		name, dialect, source string
	}{
		{"mysql_versioned", "mysql", header + body},
		{"mariadb_mysql_style", "mariadb", header + body},
		{"mysql_unversioned", "mysql", "/*! CREATE PROCEDURE prepare_accounts() */\n" + body},
		{"mariadb_unversioned", "mariadb", "/*! CREATE PROCEDURE prepare_accounts() */\n" + body},
		{"mariadb_native", "mariadb", "/*M! CREATE PROCEDURE prepare_accounts() */\n" + body},
		{"mariadb_native_versioned", "mariadb", "/*M!100100 CREATE PROCEDURE prepare_accounts() */\n" + body},
		{"mariadb_native_version_range", "mariadb", "/*M!50701 CREATE PROCEDURE prepare_accounts() */\n" + body},
		{"mysql_conditional_fragment", "mysql", "CREATE /*!80000 PROCEDURE */ prepare_accounts()\n" + body},
		{"mysql_dbmate", "mysql", "-- migrate:up\n" + header + body +
			"-- migrate:down\nDROP PROCEDURE prepare_accounts;\n"},
		{"mariadb_dbmate", "mariadb", "-- migrate:up\n/*M! CREATE PROCEDURE prepare_accounts() */\n" + body +
			"-- migrate:down\nDROP PROCEDURE prepare_accounts;\n"},
		{"mysql_goose", "mysql", "-- +goose Up\n" + header + body +
			"-- +goose Down\nDROP PROCEDURE prepare_accounts;\n"},
		{"mariadb_goose", "mariadb", "-- +goose Up\n/*M! CREATE PROCEDURE prepare_accounts() */\n" + body +
			"-- +goose Down\nDROP PROCEDURE prepare_accounts;\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tokens, ok := sqlDDLTokens(tc.source, tc.dialect); ok || len(tokens) != 0 {
				t.Fatalf("executable-comment routine header was trusted: ok=%t tokens=%+v", ok, tokens)
			}
			if symbols := sqlDDLSymbols(tc.source, tc.dialect); len(symbols) != 0 {
				t.Fatalf("phantom routine-body DDL reached outline: %+v", symbols)
			}
			occ, valid := sqlNavOccurrences(tc.source, tc.dialect)
			if valid || len(occ) != 0 {
				t.Fatalf("phantom routine-body DDL reached navigation: valid=%t %+v", valid, occ)
			}
			offset := strings.Index(tc.source, "CREATE TABLE phantom")
			if offset < 0 {
				t.Fatal("invalid test fixture: missing phantom body")
			}
			prefix := tc.source[:offset]
			line := strings.Count(prefix, "\n")
			lastLine := strings.LastIndexByte(prefix, '\n')
			column := len(prefix) - lastLine - 1 + len("CREATE TABLE ")
			if _, found := sqlNavSelected(tc.source, tc.dialect, line, column); found {
				t.Fatal("phantom table is cursor-selectable as a source declaration")
			}
			// Cross-file navigation may only resolve genuine forward
			// declarations. A rejected source cannot enter migration history.
			var found []sqlNavFound
			for _, item := range occ {
				if item.declaration && item.name == "phantom" {
					found = append(found, sqlNavFound{
						sqlNavOccurrence: item,
						sqlNavLocation: sqlNavLocation{
							URI:   "db/migrations/202610160001_prepare.sql",
							Range: item.span, FileVersion: "sha:fixture",
						},
					})
				}
			}
			if loc, err := sqlNavResolveOrderedDefinition(found, nil, "queries/read.sql"); err == nil {
				t.Fatalf("phantom routine body resolved as cross-file schema: %+v", loc)
			}
		})
	}
}

func TestSQLMySQLExecutableCommentGuardPreservesRollbackAndOrdinaryComments(t *testing.T) {
	ordinary := "/* ordinary CREATE PROCEDURE fake() */\r\n" +
		"-- /*!50003 CREATE PROCEDURE ignored() */\r\n" +
		"# /*M! CREATE PROCEDURE ignored() */\r\n" +
		"SELECT /*+ MAX_EXECUTION_TIME(1000) */ 1;\r\n" +
		"CREATE TABLE accounts (note VARCHAR(128) DEFAULT '/*!50003 CREATE PROCEDURE fake() */', " +
		"`/*M!identifier*/` INT);\r\n" +
		"ALTER TABLE accounts ADD COLUMN active BOOLEAN;\r\n"
	rollback := "/*!50003 CREATE PROCEDURE rollback_only() */\n" +
		"CREATE TABLE phantom (id INT);\n"
	mariaRollback := "/*M!100100 CREATE PROCEDURE rollback_only() */\n" +
		"CREATE TABLE phantom (id INT);\n"
	for _, dialect := range []string{"mysql", "mariadb"} {
		for _, tc := range []struct{ name, source string }{
			{"ordinary", ordinary},
			{"dbmate_forward", "-- migrate:up\n" + ordinary + "-- migrate:down\nDROP TABLE accounts;\n"},
			{"goose_forward", "-- +goose Up\n" + ordinary + "-- +goose Down\nDROP TABLE accounts;\n"},
			{"dbmate_rollback", "-- migrate:up\nCREATE TABLE accounts (id INT);\n" +
				"-- migrate:down\n" + rollback},
			{"goose_rollback", "-- +goose Up\nCREATE TABLE accounts (id INT);\n" +
				"-- +goose Down\n" + rollback},
			{"dbmate_repeated", "-- migrate:up\nCREATE TABLE accounts (id INT);\n" +
				"-- migrate:down\n" + rollback +
				"-- migrate:up\nALTER TABLE accounts ADD COLUMN active BOOLEAN;\n" +
				"-- migrate:down\nDROP TABLE accounts;\n"},
			{"goose_maria_rollback", "-- +goose Up\nCREATE TABLE accounts (id INT);\n" +
				"-- +goose Down\n" + mariaRollback},
		} {
			t.Run(dialect+"_"+tc.name, func(t *testing.T) {
				tokens, ok := sqlDDLTokens(tc.source, dialect)
				if !ok || len(tokens) == 0 {
					t.Fatalf("safe forward SQL rejected: valid=%t tokens=%+v", ok, tokens)
				}
				symbols := sqlDDLSymbols(tc.source, dialect)
				if len(symbols) == 0 || symbols[0].Name != "accounts" {
					t.Fatalf("legitimate forward table not found: %+v", symbols)
				}
				occ, valid := sqlNavOccurrences(tc.source, dialect)
				if !valid || len(occ) == 0 || !occ[0].declaration || occ[0].name != "accounts" {
					t.Fatalf("legitimate forward navigation lost: valid=%t %+v", valid, occ)
				}
				start := strings.Index(tc.source, "CREATE TABLE accounts")
				if start < 0 {
					t.Fatal("invalid fixture: missing legitimate table")
				}
				prefix := tc.source[:start]
				line := strings.Count(prefix, "\n")
				character := len(prefix) - strings.LastIndexByte(prefix, '\n') - 1
				if symbols[0].Range.Start.Line != line || symbols[0].Range.Start.Character != character ||
					occ[0].span.Start.Line != line || occ[0].span.Start.Character != character+len("CREATE TABLE ") {
					t.Fatalf("forward source offset changed: symbol=%+v nav=%+v expected line=%d char=%d",
						symbols[0].Range, occ[0].span, line, character)
				}
			})
		}
	}
	// MariaDB-only executable comments are ordinary comments in MySQL.
	mysqlOrdinary := "/*M!100100 CREATE PROCEDURE ignored() */\nCREATE TABLE accounts (id INT);\n"
	if got := sqlDDLSymbols(mysqlOrdinary, "mysql"); len(got) != 1 ||
		got[0].Name != "accounts" || got[0].Range.Start.Line != 1 {
		t.Fatalf("MariaDB-only comment incorrectly rejected for MySQL: %+v", got)
	}
	// Executable MySQL comments are ordinary comments to PostgreSQL.
	postgres := "/*!50003 CREATE PROCEDURE ignored() */\r\n" +
		"/*M!100100 CREATE PROCEDURE ignored() */\r\n" +
		"CREATE TABLE accounts (id INT);\r\n"
	got := sqlDDLSymbols(postgres, "postgres")
	if len(got) != 1 || got[0].Name != "accounts" || got[0].Range.Start.Line != 2 {
		t.Fatalf("PostgreSQL comment/offset handling regressed: %+v", got)
	}
}
