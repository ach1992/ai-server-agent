package executor

import (
	"errors"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Offline source-history only: one homogeneous, ordered SQL migration family
// (numeric-prefix, Flyway V...__..., or golang-migrate .up.sql). Never infer live database
// state, implicit runner configuration, rollback or repeatable migrations.
var (
	sqlNavVersionedMigration = regexp.MustCompile(`(?i)^([0-9]{3,20})[_-][^/]+\.sql$`)
	sqlNavFlywayMigration    = regexp.MustCompile(`^V([0-9]{1,20}(?:[._][0-9]{1,20}){0,7})__[^/]+\.sql$`)
	sqlNavGoMigrate          = regexp.MustCompile(`^([0-9]{1,20})_[^/]+\.up\.sql$`)
)

type sqlNavMigrationEvent struct {
	item    sqlNavFound
	version string
}

func sqlNavRollbackFile(file string) bool {
	return sqlNavInsideMigrationTree(file) && strings.HasSuffix(strings.ToLower(path.Base(file)), ".down.sql")
}

func sqlNavMigrationInfo(file string) (dir, family, version string, ok bool) {
	dir = path.Dir(file)
	base := path.Base(file)
	// A golang-migrate up/down pair supplies one forward version. Never use
	// its rollback side to prove a schema or follow a selected rollback file.
	if sqlNavRollbackFile(file) {
		return "", "", "", false
	}
	if strings.HasSuffix(strings.ToLower(base), ".up.sql") {
		if path.Base(dir) != "migrations" && path.Base(dir) != "migration" {
			return "", "", "", false
		}
		match := sqlNavGoMigrate.FindStringSubmatch(base)
		if match == nil {
			return "", "", "", false
		}
		version = strings.TrimLeft(match[1], "0")
		if version == "" {
			version = "0"
		}
		if _, err := strconv.ParseUint(version, 10, 64); err != nil {
			return "", "", "", false
		}
		return dir, "golang-migrate", version, true
	}
	if path.Base(dir) == "migrations" {
		if match := sqlNavVersionedMigration.FindStringSubmatch(base); match != nil {
			version = strings.TrimLeft(match[1], "0")
			if version == "" {
				version = "0"
			}
			return dir, "numeric", version, true
		}
	}
	if path.Base(dir) != "migration" && path.Base(dir) != "migrations" {
		return "", "", "", false
	}
	match := sqlNavFlywayMigration.FindStringSubmatch(base)
	if match == nil {
		return "", "", "", false
	}
	parts := strings.FieldsFunc(match[1], func(r rune) bool { return r == '.' || r == '_' })
	for i := range parts {
		parts[i] = strings.TrimLeft(parts[i], "0")
		if parts[i] == "" {
			parts[i] = "0"
		}
	}
	for len(parts) > 1 && parts[len(parts)-1] == "0" {
		parts = parts[:len(parts)-1]
	}
	return dir, "flyway", strings.Join(parts, "."), true
}

func sqlNavMigrationVersion(file string) (dir, version string, ok bool) {
	dir, _, version, ok = sqlNavMigrationInfo(file)
	return dir, version, ok
}

func sqlNavInsideMigrationTree(file string) bool {
	for _, part := range strings.Split(path.Dir(file), "/") {
		if part == "migrations" || part == "migration" {
			return true
		}
	}
	return false
}

func sqlNavCompareVersion(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(left) && i < len(right); i++ {
		if len(left[i]) < len(right[i]) {
			return -1
		}
		if len(left[i]) > len(right[i]) {
			return 1
		}
		if order := strings.Compare(left[i], right[i]); order != 0 {
			return order
		}
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return 0
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
	migrationDir, migrationFamily := "", ""
	for _, item := range append(append([]sqlNavFound(nil), items...), parentDrops...) {
		if !item.declaration && !item.drop {
			continue
		}
		dir, family, version, ok := sqlNavMigrationInfo(item.URI)
		if !ok || (migrationDir != "" && migrationDir != dir) || (migrationFamily != "" && migrationFamily != family) {
			return sqlNavLocation{}, errors.New("SQL declaration history lacks one unambiguous migration sequence")
		}
		migrationDir, migrationFamily = dir, family
		if previous, exists := seenVersions[version]; exists && previous != item.URI {
			return sqlNavLocation{}, errors.New("duplicate SQL migration version")
		}
		seenVersions[version] = item.URI
		events = append(events, sqlNavMigrationEvent{item: item, version: version})
	}
	if len(events) == 0 {
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
