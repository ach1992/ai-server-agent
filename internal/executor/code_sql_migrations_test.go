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
		{"db/migration/V1__init.sql", "db/migration", "1", true},
		{"db/migration/V001.002__alter.sql", "db/migration", "1.2", true},
		{"migrations/V001_002__alter.sql", "migrations", "1.2", true},
		{"db/migration/V01.00__same.sql", "db/migration", "1", true},
		{"db/migration/U1__undo.sql", "", "", false},
		{"db/migration/R__repeatable.sql", "", "", false},
		{"db/migration/V1__undo.down.sql", "", "", false},
		{"migrations/001_create.up.sql", "migrations", "1", true},
		{"migration/1_base.up.sql", "migration", "1", true},
		{"db/migrations/18446744073709551615_last.up.sql", "db/migrations", "18446744073709551615", true},
		{"db/migrations/18446744073709551616_overflow.up.sql", "", "", false},
		{"db/migrations/2_bad.up.SQL", "", "", false},
		{"migrations/001_create.down.sql", "", "", false},
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

func TestSQLFlywayMigrationHistoryRemainsSourceOnly(t *testing.T) {
	first := sqlNavHistoryFixture("db/migration/V1__create.sql", true, false, 0)
	drop := sqlNavHistoryFixture("db/migration/V1.2__drop.sql", false, true, 0)
	latest := sqlNavHistoryFixture("db/migration/V1.10__recreate.sql", true, false, 0)
	location, err := sqlNavResolveOrderedDefinition([]sqlNavFound{latest, first, drop}, nil, "queries/read.sql")
	if err != nil || location.URI != latest.URI {
		t.Fatalf("Flyway dotted numeric ordering lost: %+v %v", location, err)
	}
	if sqlNavCompareVersion("1.2", "1.10") >= 0 || sqlNavCompareVersion("1.9", "2") >= 0 {
		t.Fatal("Flyway components must be compared numerically")
	}
	single, err := sqlNavResolveOrderedDefinition([]sqlNavFound{first}, nil, "queries/read.sql")
	if err != nil || single.URI != first.URI {
		t.Fatalf("unambiguous single source migration rejected: %+v %v", single, err)
	}
	for _, tc := range []struct {
		name   string
		items  []sqlNavFound
		source string
	}{
		{"duplicate normalized version", []sqlNavFound{first, sqlNavHistoryFixture("db/migration/V01.0__second.sql", true, false, 0)}, "queries/read.sql"},
		{"mixed migration families", []sqlNavFound{sqlNavHistoryFixture("migrations/V1__create.sql", true, false, 0), sqlNavHistoryFixture("migrations/002_drop.sql", false, true, 0)}, "queries/read.sql"},
		{"undo source", []sqlNavFound{sqlNavHistoryFixture("db/migration/U1__restore.sql", true, false, 0)}, "queries/read.sql"},
		{"repeatable source", []sqlNavFound{sqlNavHistoryFixture("db/migration/R__view.sql", true, false, 0)}, "queries/read.sql"},
		{"rollback source", []sqlNavFound{sqlNavHistoryFixture("migrations/001_create.down.sql", true, false, 0)}, "queries/read.sql"},
		{"query inside migration", []sqlNavFound{first, drop, latest}, "db/migration/V1__create.sql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if loc, err := sqlNavResolveOrderedDefinition(tc.items, nil, tc.source); err == nil {
				t.Fatalf("ambiguous migration source was accepted: %+v", loc)
			}
		})
	}
}

func TestSQLGolangMigrateSourceHistory(t *testing.T) {
	create := sqlNavHistoryFixture("db/migrations/1_create.up.sql", true, false, 0)
	drop := sqlNavHistoryFixture("db/migrations/2_drop.up.sql", false, true, 0)
	recreate := sqlNavHistoryFixture("db/migrations/10_recreate.up.sql", true, false, 0)
	got, err := sqlNavResolveOrderedDefinition([]sqlNavFound{recreate, drop, create}, nil, "db/query.sql")
	if err != nil || got.URI != recreate.URI {
		t.Fatalf("ordered forward-only migration resolution failed: %+v %v", got, err)
	}
	for _, tc := range []struct {
		name  string
		items []sqlNavFound
	}{
		{"rollback is not forward", []sqlNavFound{sqlNavHistoryFixture("db/migrations/1_create.down.sql", true, false, 0)}},
		{"mixed runner families", []sqlNavFound{create, sqlNavHistoryFixture("db/migrations/002_drop.sql", false, true, 0)}},
		{"duplicate numeric up version", []sqlNavFound{create, sqlNavHistoryFixture("db/migrations/01_other.up.sql", true, false, 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if found, err := sqlNavResolveOrderedDefinition(tc.items, nil, "db/query.sql"); err == nil {
				t.Fatalf("ambiguous migration history accepted: %+v", found)
			}
		})
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
