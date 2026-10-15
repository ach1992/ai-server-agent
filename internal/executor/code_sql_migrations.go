package executor

import (
	"errors"
	"path"
	"regexp"
	"sort"
	"strings"
)

// This is an intentionally narrow, offline *source-history* resolver, not a
// database migration runner. Only a single, numerically ordered migrations/
// directory with unambiguous version-prefixed .sql names may establish a
// post-migration definition. It makes no claim about which migrations ran.
var sqlNavVersionedMigration = regexp.MustCompile(`(?i)^([0-9]{3,20})[_-][^/]+\.sql$`)

type sqlNavMigrationEvent struct {
	item    sqlNavFound
	version string
}

func sqlNavMigrationVersion(file string) (dir, version string, ok bool) {
	dir = path.Dir(file)
	if path.Base(dir) != "migrations" {
		return "", "", false
	}
	match := sqlNavVersionedMigration.FindStringSubmatch(path.Base(file))
	if match == nil {
		return "", "", false
	}
	version = strings.TrimLeft(match[1], "0")
	if version == "" {
		version = "0"
	}
	return dir, version, true
}

func sqlNavInsideMigrationTree(file string) bool {
	for _, part := range strings.Split(path.Dir(file), "/") {
		if part == "migrations" {
			return true
		}
	}
	return false
}

func sqlNavCompareVersion(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

// A query outside the migration directory can resolve the last unique CREATE
// after a prior DROP/RENAME when every relevant declaration/lifecycle event is
// in one explicit numeric sequence. For a cursor inside a migration, the
// historical versus current schema identity remains ambiguous by design.
// This preserves fail-closed behavior for non-versioned, duplicate-version,
// independently managed, or concurrently edited SQL sources.
func sqlNavResolveOrderedDefinition(items, parentDrops []sqlNavFound, sourcePath string) (sqlNavLocation, error) {
	if sqlNavInsideMigrationTree(sourcePath) {
		return sqlNavLocation{}, errors.New("migration source cannot prove a post-migration definition")
	}
	var events []sqlNavMigrationEvent
	seenVersions := make(map[string]string)
	migrationDir := ""
	for _, item := range append(append([]sqlNavFound(nil), items...), parentDrops...) {
		if !item.declaration && !item.drop {
			continue
		}
		dir, version, ok := sqlNavMigrationVersion(item.URI)
		if !ok || (migrationDir != "" && migrationDir != dir) {
			return sqlNavLocation{}, errors.New("SQL declaration history lacks one unambiguous migration sequence")
		}
		migrationDir = dir
		if previous, exists := seenVersions[version]; exists && previous != item.URI {
			return sqlNavLocation{}, errors.New("duplicate SQL migration version")
		}
		seenVersions[version] = item.URI
		events = append(events, sqlNavMigrationEvent{item: item, version: version})
	}
	if len(events) < 2 {
		return sqlNavLocation{}, errors.New("insufficient ordered SQL migration history")
	}
	sort.Slice(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if order := sqlNavCompareVersion(a.version, b.version); order != 0 {
			return order < 0
		}
		if a.item.URI != b.item.URI {
			return a.item.URI < b.item.URI // duplicate version is rejected above
		}
		if a.item.Range.Start.Line != b.item.Range.Start.Line {
			return a.item.Range.Start.Line < b.item.Range.Start.Line
		}
		return a.item.Range.Start.Character < b.item.Range.Start.Character
	})
	var active *sqlNavLocation
	for _, event := range events {
		switch {
		case event.item.drop:
			active = nil
		case event.item.declaration:
			if active != nil {
				return sqlNavLocation{}, errors.New("duplicate definition without a preceding DROP")
			}
			location := event.item.sqlNavLocation
			active = &location
		}
	}
	if active == nil {
		return sqlNavLocation{}, errors.New("last SQL lifecycle event invalidated the definition")
	}
	return *active, nil
}
