package executor

import (
	"strings"
	"testing"
)

// These fixtures mirror the migration runner's documented forward/rollback
// grammar. No database connection or execution occurs.
func TestSQLGooseForwardOnlySourceGraph(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mariadb"} {
		t.Run(dialect, func(t *testing.T) {
			source := "-- +goose NO TRANSACTION\n-- +goose Up\n" +
				"CREATE TABLE audit_events (id BIGINT PRIMARY KEY);\n" +
				"-- +goose Down\nDROP TABLE audit_events;\n" +
				"CREATE TABLE fabricated_rollback (id INT);\n"
			occ, ok := sqlNavOccurrences(source, dialect)
			if !ok || len(occ) != 1 || !occ[0].declaration || occ[0].name != "audit_events" ||
				occ[0].span.Start.Line != 2 {
				t.Fatalf("rollback SQL contaminated forward source graph: valid=%t %+v", ok, occ)
			}
			symbols := sqlDDLSymbols(source, dialect)
			if len(symbols) != 1 || symbols[0].Name != "audit_events" ||
				symbols[0].Range.Start.Line != 2 {
				t.Fatalf("rollback SQL contaminated DDL outline: %+v", symbols)
			}
			if _, found := sqlNavSelected(source, dialect, 5, 18); found {
				t.Fatal("rollback-only definition was selectable")
			}
			if got, ok := sqlDDLGooseForwardSQL(source, dialect); !ok ||
				len(got) != len(source) || !strings.Contains(got, "CREATE TABLE audit_events") ||
				strings.Contains(got, "fabricated_rollback") {
				t.Fatalf("goose Up normalization lost source positions or included Down: %t %q", ok, got)
			}
		})
	}
}

func TestSQLGooseAnnotationsAreCaseInsensitive(t *testing.T) {
	source := "-- +goose up\nCREATE VIEW report AS SELECT 1;\n-- +goose down\nDROP VIEW report;\n"
	occ, valid := sqlNavOccurrences(source, "postgres")
	if !valid || len(occ) != 1 || !occ[0].declaration || occ[0].name != "report" {
		t.Fatalf("lowercase goose Up/Down lost source truth: %+v %t", occ, valid)
	}
}

func TestSQLGooseUnsafeAnnotationsFailClosed(t *testing.T) {
	for _, source := range []string{
		"-- +goose Down\nDROP TABLE a;\n-- +goose Up\nCREATE TABLE a (id INT);",
		"-- +goose Up\nCREATE TABLE a (id INT);\n-- +goose Up\nCREATE TABLE a (id INT);",
		"-- +goose Up\nCREATE TABLE a (id INT);\n-- +goose Down\nDROP TABLE a;\n-- +goose Down",
		"-- +goose Up\nCREATE TABLE a (id INT);\n-- +goose ENVSUB ON\nCREATE TABLE dynamic (id INT);",
		"-- +goose Up\n-- +goose StatementBegin\nCREATE TABLE a (id INT);\n-- +goose StatementEnd",
		" -- +goose Up\nCREATE TABLE a (id INT);",
		"-- +goose Up\nCREATE TABLE a (id INT)\n-- +goose Down\nDROP TABLE a;",
		"SELECT $$\n-- +goose Up\nCREATE TABLE fabricated (id INT);\n$$;",
		"/*\n-- +goose Up\n*/\nCREATE TABLE fabricated (id INT);",
		"-- +goose NO TRANSACTION\nCREATE TABLE a (id INT);",
	} {
		for _, dialect := range []string{"postgres", "mysql"} {
			if dialect == "mysql" && strings.Contains(source, "$$") {
				continue
			}
			occ, ok := sqlNavOccurrences(source, dialect)
			if ok || len(occ) != 0 || len(sqlDDLSymbols(source, dialect)) != 0 {
				t.Errorf("%s accepted unsafe goose source: %q %+v", dialect, source, occ)
			}
		}
	}
}

func TestSQLGooseOrdinarySQLWithoutRunnerAnnotationsUnchanged(t *testing.T) {
	source := "CREATE TABLE normal (id INT); -- literal mentioning +goose Up\nSELECT '-- +goose Down' AS note;"
	output, ok := sqlDDLGooseForwardSQL(source, "postgres")
	if !ok || output != source {
		t.Fatalf("ordinary SQL was mutated by annotation handling: ok=%v %q", ok, output)
	}
}
