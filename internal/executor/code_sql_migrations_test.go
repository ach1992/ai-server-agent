package executor

import "testing"

func sqlNavHistoryFixture(file string, declaration, drop bool, line int) sqlNavFound {
	return sqlNavFound{
		sqlNavOccurrence: sqlNavOccurrence{declaration: declaration, drop: drop},
		sqlNavLocation:   sqlNavLocation{URI: file, FileVersion: "sha:fixture", Range: sqlDDLRange{Start: sqlDDLPosition{Line: line}}},
	}
}

func TestSQLMigrationVersionOrderingAndSourceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		file, dir, version string
		ok                 bool
	}{
		{"migrations/0001_create.sql", "migrations", "1", true},
		{"db/migrations/202610150401_init.SQL", "db/migrations", "202610150401", true},
		{"db/migrations/.hidden/003_create.sql", "", "", false},
		{"db/schema.sql", "", "", false},
		{"migrations/01_create.sql", "", "", false},
		{"migrations/003_create.psql", "", "", false},
	} {
		dir, version, ok := sqlNavMigrationVersion(tc.file)
		if ok != tc.ok || dir != tc.dir || version != tc.version {
			t.Errorf("version %q: (%q,%q,%t) want (%q,%q,%t)", tc.file, dir, version, ok, tc.dir, tc.version, tc.ok)
		}
	}
	if sqlNavCompareVersion("9", "10") >= 0 || sqlNavCompareVersion("999999999999999999", "1000000000000000000") >= 0 {
		t.Fatal("numeric SQL versions must be ordered without integer overflow")
	}
	if !sqlNavInsideMigrationTree("db/migrations/.hidden/003_nested.sql") || sqlNavInsideMigrationTree("db/queries/read.sql") {
		t.Fatal("source versus migration boundary misclassified")
	}
}

func TestSQLOrderedMigrationDefinitionsFailClosed(t *testing.T) {
	create1 := sqlNavHistoryFixture("db/migrations/001_original.sql", true, false, 0)
	drop := sqlNavHistoryFixture("db/migrations/002_drop.sql", false, true, 0)
	create3 := sqlNavHistoryFixture("db/migrations/003_recreate.sql", true, false, 0)
	location, err := sqlNavResolveOrderedDefinition([]sqlNavFound{create3, drop, create1}, nil, "db/queries/read.sql")
	if err != nil || location.URI != create3.URI {
		t.Fatalf("expected latest unique CREATE after DROP, got %+v %v", location, err)
	}
	for _, tc := range []struct {
		name   string
		items  []sqlNavFound
		parent []sqlNavFound
		source string
	}{
		{"old-migration-source", []sqlNavFound{create1, drop, create3}, nil, "db/migrations/001_original.sql"},
		{"nested-migration-source", []sqlNavFound{create1, drop, create3}, nil, "db/migrations/.hidden/007_query.sql"},
		{"duplicate-version", []sqlNavFound{create1, drop, create3, sqlNavHistoryFixture("db/migrations/003_other.sql", true, false, 0)}, nil, "db/queries/read.sql"},
		{"duplicate-live-create", []sqlNavFound{create1, create3}, nil, "db/queries/read.sql"},
		{"last-drop", []sqlNavFound{create1, drop}, nil, "db/queries/read.sql"},
		{"mixed-directories", []sqlNavFound{create1, drop, sqlNavHistoryFixture("other/migrations/003_recreate.sql", true, false, 0)}, nil, "db/queries/read.sql"},
		{"non-versioned-declaration", []sqlNavFound{create1, drop, sqlNavHistoryFixture("db/schema.sql", true, false, 0)}, nil, "db/queries/read.sql"},
		{"non-versioned-drop", []sqlNavFound{create1, drop, create3, sqlNavHistoryFixture("db/migrations/unordered.sql", false, true, 0)}, nil, "db/queries/read.sql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if loc, err := sqlNavResolveOrderedDefinition(tc.items, tc.parent, tc.source); err == nil {
				t.Fatalf("ambiguous history improperly resolved to %+v", loc)
			}
		})
	}
	// Column-specific DROP/RECREATE must not resurrect a definition that was
	// invalidated by a table-level lifecycle event.
	column, err := sqlNavResolveOrderedDefinition([]sqlNavFound{create1, create3}, []sqlNavFound{drop}, "db/queries/read.sql")
	if err != nil || column.URI != create3.URI {
		t.Fatalf("column state was not rebuilt after table drop: %+v %v", column, err)
	}
}

func TestSQLOrderedMigrationSameFileStatementSequence(t *testing.T) {
	create := sqlNavHistoryFixture("migrations/001_reset.sql", true, false, 1)
	drop := sqlNavHistoryFixture("migrations/001_reset.sql", false, true, 2)
	recreate := sqlNavHistoryFixture("migrations/001_reset.sql", true, false, 3)
	location, err := sqlNavResolveOrderedDefinition([]sqlNavFound{recreate, create, drop}, nil, "queries/read.sql")
	if err != nil || location.Range.Start.Line != 3 {
		t.Fatalf("same-file migration statement order lost: %+v %v", location, err)
	}
}
