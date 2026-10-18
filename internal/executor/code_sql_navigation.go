package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SQL navigation is a deliberately small, offline supplement to SQLLens. It
// connects source-backed table/view declarations and uses across ordinary SQL
// migration files. It never infers a live database schema, runs SQL or reads
// project database configuration. Unknown constructs stay with the real LSP.
const (
	sqlNavigationSearchMatches = 256
	sqlNavigationMaxFiles      = 16
	sqlNavigationTotalBytes    = 2 << 20
	sqlNavigationMaxLocations  = 64
)

type sqlNavOccurrence struct {
	name, identity, searchTerm     string
	tableIdentity, tableSearchTerm string
	span                           sqlDDLRange
	declaration                    bool
	drop                           bool
	isColumn                       bool
	materialized                   bool // Proven PostgreSQL materialized-view declaration/drop
	requiresMaterialized           bool // REFRESH/DROP must not resolve to an ordinary table/view
	lifecycleOnly                  bool // Synthetic SET SCHEMA identity, not a selectable source token
}

type sqlNavLocation struct {
	URI         string      `json:"uri"`
	Range       sqlDDLRange `json:"range"`
	FileVersion string      `json:"file_version"`
}

type sqlNavFound struct {
	sqlNavOccurrence
	sqlNavLocation
}

// Avoid conflating unsupported identifier syntax or injected search patterns
// with proved SQL references. Quoted mixed-case identifiers remain distinct
// from folded unquoted identifiers, as PostgreSQL requires.
var (
	sqlNavSimpleName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sqlNavMySQLDelimiter = regexp.MustCompile(`(?mi)^\s*DELIMITER\b`)
)

func sqlNavName(tokens []sqlDDLToken, at int) (sqlNavOccurrence, int, bool) {
	_, span, next, ok := sqlDDLQualifiedName(tokens, at)
	// sqlDDLQualifiedName deliberately permits partial names for other SQL
	// consumers. Offline navigation must not turn an unmatched dot (or an
	// unsupported fourth qualifier) into a proved relation identity.
	if !ok || next-at > 5 || next < len(tokens) && tokens[next].text == "." {
		return sqlNavOccurrence{}, at, false
	}
	parts := make([]string, 0, 3)
	term := ""
	for i := at; i < next; i += 2 {
		word := tokens[i]
		if len(word.text) > 128 || len(word.text) == 0 ||
			strings.ContainsAny(word.text, "\x00\r\n") {
			return sqlNavOccurrence{}, at, false
		}
		if word.kind == 'i' && !sqlNavSimpleName.MatchString(word.text) {
			return sqlNavOccurrence{}, at, false
		}
		// Embedded quote/backtick/backslash requires dialect-specific escape
		// reconstruction for exhaustive cross-file discovery. Do not claim a
		// complete search of names that cannot be matched literally.
		if word.kind == 'q' && strings.ContainsAny(word.text, "\"`\\") {
			return sqlNavOccurrence{}, at, false
		}
		part := word.text
		if tokens[i].kind != 'q' {
			part = strings.ToLower(part)
		}
		parts = append(parts, part)
		term = tokens[i].text
	}
	// A cursor on the schema prefix names a schema, not the table itself.
	// Keep navigation selection on the final identifier only.
	span = sqlDDLRange{Start: tokens[next-1].start, End: tokens[next-1].end}
	return sqlNavOccurrence{name: strings.Join(parts, "."), identity: strings.Join(parts, "\x00"), searchTerm: term, span: span}, next, true
}

// In MySQL/MariaDB, ONLY is a nonreserved identifier, not an inheritance
// modifier. Disambiguate only an unquoted leading ONLY, and require a known
// next token so invalid PostgreSQL-style ONLY relation syntax never invents
// source-backed table or view lifecycle transitions.
func sqlNavMySQLBareOnlyName(tokens []sqlDDLToken, at int, dialect, operation, object string) bool {
	if at >= len(tokens) || tokens[at].keyword() != "ONLY" ||
		(dialect != "mysql" && dialect != "mariadb") {
		return false
	}
	next := at + 1
	if next < len(tokens) && tokens[next].text == "." {
		// ONLY can also name a schema. Require a complete qualified name
		// *and* the operation clause after that name; for example,
		// ONLY. RENAME TO x must not reinterpret RENAME as a table name.
		_, end, valid := sqlNavName(tokens, at)
		// MySQL/MariaDB table and view names have at most a database
		// qualifier; three-part names belong to column references only.
		if !valid || end != at+3 {
			return false
		}
		next = end
	}
	if operation == "DROP" {
		return next == len(tokens) || tokens[next].text == "," ||
			next+1 == len(tokens) &&
				(tokens[next].keyword() == "CASCADE" || tokens[next].keyword() == "RESTRICT")
	}
	if operation != "ALTER" || next >= len(tokens) {
		return false
	}
	if object == "VIEW" {
		return tokens[next].keyword() == "AS"
	}
	if object != "TABLE" {
		return false
	}
	switch tokens[next].keyword() {
	case "RENAME":
		return next+1 < len(tokens) &&
			(tokens[next+1].keyword() == "TO" || tokens[next+1].keyword() == "AS" ||
				tokens[next+1].keyword() == "COLUMN")
	case "ADD", "ALTER", "CHANGE", "MODIFY", "DROP", "CONVERT", "ORDER",
		"DISCARD", "IMPORT", "ENGINE", "AUTO_INCREMENT", "COMMENT", "DEFAULT",
		"ALGORITHM", "LOCK", "PARTITION", "REMOVE", "ENABLE", "DISABLE", "FORCE":
		return true
	}
	return false
}

func sqlNavAt(span sqlDDLRange, line, character int) bool {
	start, end := span.Start, span.End
	return (line > start.Line || line == start.Line && character >= start.Character) &&
		(line < end.Line || line == end.Line && character < end.Character)
}

// Preserve the existing table-only parser contract; column navigation opts
// in explicitly, without changing the semantics tested by table consumers.
func sqlNavOccurrences(source, dialect string) ([]sqlNavOccurrence, bool) {
	return sqlNavSourceOccurrences(source, dialect, false)
}

func sqlNavAllOccurrences(source, dialect string) ([]sqlNavOccurrence, bool) {
	return sqlNavSourceOccurrences(source, dialect, true)
}

func sqlNavSourceOccurrences(source, dialect string, withColumns bool) ([]sqlNavOccurrence, bool) {
	tokens, ok := sqlDDLTokens(source, dialect)
	if !ok {
		return nil, false
	}
	if dialect == "mysql" || dialect == "mariadb" {
		// Custom DELIMITER changes statement boundaries; parsing semicolons
		// inside a routine would invent table declarations. Fail closed.
		if sqlNavMySQLDelimiter.MatchString(source) {
			return nil, false
		}
	}
	var result []sqlNavOccurrence
	scan := func(statement []sqlDDLToken) {
		if len(statement) < 2 {
			return
		}
		operation := statement[0].keyword()
		// CTEs and executable/procedural SQL have name-shadowing and scope
		// rules beyond this bounded source analyzer. Do not fabricate links.
		switch operation {
		case "CREATE", "ALTER", "DROP", "RENAME", "INSERT", "UPDATE", "DELETE", "SELECT", "TRUNCATE", "REFRESH":
		default:
			return
		}
		hasWithClause := false
		for i, token := range statement {
			if token.keyword() != "WITH" {
				continue
			}
			// PostgreSQL CREATE MATERIALIZED VIEW ... AS SELECT ... WITH
			// [NO] DATA ends in a population clause, not a scoped CTE.
			// Keep the conservative CTE guard for every other WITH form.
			if operation == "CREATE" && statement[1].keyword() == "MATERIALIZED" &&
				(i+2 == len(statement) && statement[i+1].keyword() == "DATA" ||
					i+3 == len(statement) && statement[i+1].keyword() == "NO" &&
						statement[i+2].keyword() == "DATA") {
				continue
			}
			hasWithClause = true
			break
		}
		appendName := func(at int, declaration, drop bool) int {
			occ, end, ok := sqlNavName(statement, at)
			if !ok {
				return at
			}
			occ.declaration, occ.drop = declaration, drop
			result = append(result, occ)
			return end
		}
		at := 1
		switch operation {
		case "CREATE":
			if at+1 < len(statement) && statement[at].keyword() == "OR" &&
				statement[at+1].keyword() == "REPLACE" {
				at += 2
			}
			for at < len(statement) {
				switch statement[at].keyword() {
				case "TEMP", "TEMPORARY", "UNLOGGED", "GLOBAL", "LOCAL":
					at++
				default:
					goto createdType
				}
			}
		createdType:
			materialized := at < len(statement) && statement[at].keyword() == "MATERIALIZED"
			if materialized {
				// No native MySQL/MariaDB materialized view and no
				// PostgreSQL TEMP/OR REPLACE materialized-view syntax.
				if (dialect != "postgres" && dialect != "postgresql") ||
					at != 1 || at+1 >= len(statement) || statement[at+1].keyword() != "VIEW" {
					return
				}
				at++
			}
			if at < len(statement) && (statement[at].keyword() == "TABLE" ||
				statement[at].keyword() == "VIEW") {
				object := statement[at].keyword()
				at++
				if at+2 < len(statement) && statement[at].keyword() == "IF" &&
					statement[at+1].keyword() == "NOT" && statement[at+2].keyword() == "EXISTS" {
					at += 3
				}
				occ, next, valid := sqlNavName(statement, at)
				if valid && next < len(statement) &&
					((object == "TABLE" && (statement[next].text == "(" ||
						statement[next].keyword() == "AS" || statement[next].keyword() == "LIKE")) ||
						(object == "VIEW" && (materialized && sqlDDLMaterializedQuery(statement, next) ||
							!materialized && statement[next].keyword() == "AS"))) {
					occ.declaration = true
					occ.materialized = materialized
					result = append(result, occ)
				} else if materialized {
					return // malformed/unsupported header cannot supply query references
				}
			} else if table, _, ok := sqlNavIndexTarget(statement, dialect); ok {
				result = append(result, table)
			}
		case "ALTER", "DROP":
			materialized := statement[at].keyword() == "MATERIALIZED"
			if materialized {
				if (dialect != "postgres" && dialect != "postgresql") ||
					at+1 >= len(statement) || statement[at+1].keyword() != "VIEW" {
					return
				}
				at++
			}
			if statement[at].keyword() == "TABLE" || statement[at].keyword() == "VIEW" {
				object := statement[at].keyword()
				at++
				if at+1 < len(statement) && statement[at].keyword() == "IF" &&
					statement[at+1].keyword() == "EXISTS" {
					at += 2
				}
				if at < len(statement) && statement[at].keyword() == "ONLY" {
					if operation == "ALTER" && !materialized && object == "TABLE" &&
						(dialect == "postgres" || dialect == "postgresql") {
						at++ // PostgreSQL inheritance-scoped ALTER TABLE
					} else if !sqlNavMySQLBareOnlyName(statement, at, dialect, operation, object) {
						// Unsupported modifier or malformed unquoted identifier
						// cannot synthesize a relation lifecycle transition.
						return
					}
				}
				// PostgreSQL ordinary TABLE/VIEW RENAME TO and SET SCHEMA
				// change the relation identity. Never serve the old CREATE as
				// current source, nor mistake a renamed relation in the same
				// schema for an unrelated unqualified table. A missing/invalid
				// target still invalidates the original source; no fabricated
				// new identifier is emitted.
				if !materialized && operation == "ALTER" &&
					(dialect == "postgres" || dialect == "postgresql") {
					relation, end, valid := sqlNavName(statement, at)
					if valid && end+1 < len(statement) &&
						(statement[end].keyword() == "RENAME" && statement[end+1].keyword() == "TO" ||
							statement[end].keyword() == "SET" && statement[end+1].keyword() == "SCHEMA") {
						relation.drop = true
						result = append(result, relation)
						target, next, ok := sqlNavName(statement, end+2)
						if ok && next == len(statement) && !strings.Contains(target.identity, "\x00") {
							if statement[end].keyword() == "RENAME" {
								if prefix := strings.LastIndex(relation.identity, "\x00"); prefix >= 0 {
									target.identity = relation.identity[:prefix+1] + target.identity
								}
							} else {
								last := strings.LastIndex(relation.identity, "\x00")
								target.identity += "\x00" + relation.identity[last+1:]
								target.searchTerm = relation.searchTerm
								target.lifecycleOnly = true
							}
							target.name = strings.ReplaceAll(target.identity, "\x00", ".")
							target.drop = true
							result = append(result, target)
						}
						return
					}
				}
				if materialized && operation == "ALTER" {
					// A materialized relation's identity may change across ordered
					// migrations. Invalidate the old CREATE; a limited parser must
					// not present a stale definition after an unsupported ALTER.
					relation, end, valid := sqlNavName(statement, at)
					if !valid || end >= len(statement) {
						return
					}
					// These known clauses preserve the relation's identity.
					if end+1 < len(statement) &&
						(statement[end].keyword() == "OWNER" && statement[end+1].keyword() == "TO" ||
							statement[end].keyword() == "SET" && (statement[end+1].keyword() == "TABLESPACE" ||
								statement[end+1].text == "(") ||
							statement[end].keyword() == "RESET" && statement[end+1].text == "(") {
						return
					}
					// PostgreSQL RENAME [COLUMN] old TO new only changes the
					// column identity. Do not tombstone the relation's CREATE.
					// RENAME TO new is a distinct, relation-changing form.
					if statement[end].keyword() == "RENAME" {
						columnAt := end + 1
						if columnAt < len(statement) && statement[columnAt].keyword() == "COLUMN" {
							columnAt++
						}
						_, oldEnd, oldOK := sqlNavName(statement, columnAt)
						_, newEnd, newOK := sqlNavName(statement, columnAt+2)
						if oldOK && oldEnd == columnAt+1 &&
							columnAt+1 < len(statement) && statement[columnAt+1].keyword() == "TO" &&
							newOK && newEnd == len(statement) {
							return // unsupported column graph remains with SQLLens
						}
					}
					relation.drop, relation.materialized, relation.requiresMaterialized = true, true, true
					result = append(result, relation)
					if end+2 < len(statement) && (statement[end].keyword() == "RENAME" &&
						statement[end+1].keyword() == "TO" ||
						statement[end].keyword() == "SET" && statement[end+1].keyword() == "SCHEMA") {
						newName, next, ok := sqlNavName(statement, end+2)
						if ok && next == len(statement) && !strings.Contains(newName.identity, "\x00") {
							if statement[end].keyword() == "RENAME" {
								// Schema prefixes are separated in identity, not
								// by dots inside quoted SQL identifiers.
								if i := strings.LastIndex(relation.identity, "\x00"); i >= 0 {
									newName.identity = relation.identity[:i+1] + newName.identity
								}
							} else {
								// SET SCHEMA keeps the old relation's final name.
								last := strings.LastIndex(relation.identity, "\x00")
								newName.identity += "\x00" + relation.identity[last+1:]
								newName.searchTerm = relation.searchTerm
								newName.lifecycleOnly = true
							}
							newName.name = strings.ReplaceAll(newName.identity, "\x00", ".")
							newName.drop, newName.materialized, newName.requiresMaterialized = true, true, true
							result = append(result, newName)
						}
					}
					break
				}
				if materialized {
					// Parse the entire DROP list before reporting tombstones.
					// Partial/malformed statements must not poison history.
					var drops []sqlNavOccurrence
					for {
						if at >= len(statement) ||
							(statement[at].kind == 'i' &&
								(statement[at].keyword() == "CASCADE" || statement[at].keyword() == "RESTRICT")) {
							return // a trailing DROP modifier is not a relation name
						}
						occ, end, valid := sqlNavName(statement, at)
						if !valid {
							return
						}
						occ.drop, occ.materialized, occ.requiresMaterialized = true, true, true
						drops = append(drops, occ)
						if end < len(statement) && statement[end].text == "," {
							at = end + 1
							continue
						}
						if end < len(statement) &&
							(end+1 != len(statement) ||
								(statement[end].keyword() != "CASCADE" && statement[end].keyword() != "RESTRICT")) {
							return
						}
						result = append(result, drops...)
						break
					}
					break
				}
				if operation == "DROP" {
					// A malformed later item invalidates the whole DROP statement.
					// Do not emit earlier tombstones until the entire name list
					// (and optional trailing modifier) has been verified.
					var drops []sqlNavOccurrence
					for {
						if at >= len(statement) || statement[at].keyword() == "CASCADE" ||
							statement[at].keyword() == "RESTRICT" {
							return
						}
						if statement[at].keyword() == "ONLY" &&
							!sqlNavMySQLBareOnlyName(statement, at, dialect, operation, object) {
							return
						}
						occ, end, valid := sqlNavName(statement, at)
						if !valid || ((dialect == "mysql" || dialect == "mariadb") &&
							strings.Count(occ.identity, "\x00") > 1) {
							return // table/view names admit at most one database qualifier
						}
						occ.drop = true
						drops = append(drops, occ)
						if end == len(statement) {
							break
						}
						if statement[end].text == "," {
							at = end + 1
							continue
						}
						if end+1 != len(statement) ||
							(statement[end].keyword() != "CASCADE" && statement[end].keyword() != "RESTRICT") {
							return
						}
						break
					}
					result = append(result, drops...)
					break
				}
				// The relation header must be valid before searching for a
				// relation-changing clause; RENAME inside a qualified name
				// is not itself a relation rename.
				relation, end, valid := sqlNavName(statement, at)
				if !valid || end >= len(statement) || ((dialect == "mysql" || dialect == "mariadb") &&
					strings.Count(relation.identity, "\x00") > 1) {
					return // malformed or unsupported relation header
				}
				renameAt := -1
				for i := end; i+1 < len(statement); i++ {
					if statement[i].keyword() == "RENAME" &&
						(statement[i+1].keyword() == "TO" || statement[i+1].keyword() == "AS") {
						renameAt = i
						break
					}
				}
				if renameAt >= 0 {
					// Keep MySQL/MariaDB lifecycle history atomic. An
					// unsupported or incomplete target cannot poison the
					// original CREATE or invent a renamed relation.
					if dialect == "mysql" || dialect == "mariadb" {
						// Compound ALTER clauses need their own grammar: a
						// RENAME token deep inside a clause is not evidence
						// of a complete relation move.
						if renameAt != end {
							return
						}
						target, targetEnd, ok := sqlNavName(statement, renameAt+2)
						if !ok || targetEnd != len(statement) || strings.Count(target.identity, "\x00") > 1 {
							return // target too is a table name, not a three-part column name
						}
						relation.drop, target.drop = true, true
						result = append(result, relation, target)
					} else {
						relation.drop = true
						result = append(result, relation)
						appendName(renameAt+2, false, true)
					}
				} else {
					result = append(result, relation)
				}
			}
		case "REFRESH":
			if (dialect != "postgres" && dialect != "postgresql") ||
				at+1 >= len(statement) || statement[at].keyword() != "MATERIALIZED" ||
				statement[at+1].keyword() != "VIEW" {
				return
			}
			at += 2
			concurrent := at < len(statement) && statement[at].keyword() == "CONCURRENTLY"
			if concurrent {
				at++
			}
			occ, end, valid := sqlNavName(statement, at)
			if !valid {
				return
			}
			// REFRESH [CONCURRENTLY] name [WITH [NO] DATA].
			noData := end+2 < len(statement) && statement[end].keyword() == "WITH" &&
				statement[end+1].keyword() == "NO" && statement[end+2].keyword() == "DATA"
			withData := end+1 < len(statement) && statement[end].keyword() == "WITH" &&
				statement[end+1].keyword() == "DATA"
			if end != len(statement) && !(withData && end+2 == len(statement)) &&
				!(noData && end+3 == len(statement)) {
				return
			}
			if concurrent && noData {
				return // PostgreSQL disallows CONCURRENTLY WITH NO DATA
			}
			occ.materialized, occ.requiresMaterialized = true, true
			result = append(result, occ)
		case "RENAME":
			if statement[at].keyword() == "TABLE" {
				at++
				for at < len(statement) {
					end := appendName(at, false, true)
					if end == at || end >= len(statement) || statement[end].keyword() != "TO" {
						break
					}
					end = appendName(end+1, false, true)
					if end >= len(statement) || statement[end].text != "," {
						break
					}
					at = end + 1
				}
			}
		case "INSERT":
			if statement[at].keyword() == "INTO" {
				appendName(at+1, false, false)
			}
		case "UPDATE":
			appendName(at, false, false)
		case "TRUNCATE":
			if statement[at].keyword() == "TABLE" {
				at++
			}
			appendName(at, false, false)
		}
		if withColumns {
			result = append(result, sqlNavColumns(statement, dialect)...)
		}
		for i := 1; i+1 < len(statement); i++ {
			keyword := statement[i].keyword()
			switch keyword {
			case "REFERENCES":
				if operation == "CREATE" || operation == "ALTER" {
					appendName(i+1, false, false)
				}
			case "FROM", "JOIN":
				// Only source-backed ordinary query references, not CTE names
				// or implicit functions. More complex queries use SQLLens.
				if !hasWithClause && (operation == "SELECT" || operation == "DELETE" ||
					operation == "CREATE" || operation == "INSERT" || operation == "UPDATE") {
					_, next, valid := sqlNavName(statement, i+1)
					if valid && (next >= len(statement) || statement[next].text != "(") {
						appendName(i+1, false, false)
					}
				}
			}
		}
	}
	start, depth := 0, 0
	for i, token := range tokens {
		switch token.text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return nil, false
			}
		case ";":
			if depth == 0 {
				scan(tokens[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, false
	}
	if start < len(tokens) {
		scan(tokens[start:])
	}
	return result, true
}

func sqlNavSelected(source, dialect string, line, character int) (sqlNavOccurrence, bool) {
	occurrences, valid := sqlNavAllOccurrences(source, dialect)
	if !valid {
		return sqlNavOccurrence{}, false
	}
	for _, occ := range occurrences {
		if !occ.lifecycleOnly && sqlNavAt(occ.span, line, character) {
			return occ, true
		}
	}
	return sqlNavOccurrence{}, false
}

func sqlNavSearchGlobs(dialect string) []string {
	switch dialect {
	case "mysql", "mariadb":
		return []string{"*.[sS][qQ][lL]", "*.[mM][yY][sS][qQ][lL]", "*.[mM][aA][rR][iI][aA][dD][bB]}"}
	default:
		return []string{"*.[sS][qQ][lL]", "*.[pP][gG][sS][qQ][lL]", "*.[pP][sS][qQ][lL]}"}
	}
}

// Search only using the existing worker-authority, Landlocked ripgrep helper;
// then independently stat/read and source-validate every match. The search is
// repeated at completion and every target version rechecked: truncation,
// concurrent changes or unsafe files cannot become apparently complete links.
func (s *Server) sqlOfflineNavigation(parent context.Context, req Request, sourcePath, source string) (Response, bool) {
	if sqlNavRollbackFile(sourcePath) {
		return fileError("sql_schema_rollback_unsupported", "validation", errors.New("rollback migration cannot prove forward SQL references")), true
	}
	selected, found := sqlNavSelected(source, req.SQLDialect, req.Line, req.Character)
	if !found {
		return Response{}, false
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	query := req
	query.Action, query.Path, query.FileVersion = "workspace_text_search", "", ""
	query.Pattern = "(?i)" + regexp.QuoteMeta(selected.searchTerm)
	if selected.isColumn {
		query.Pattern = "(?i)(?:" + regexp.QuoteMeta(selected.searchTerm) + "|" + regexp.QuoteMeta(selected.tableSearchTerm) + ")"
	}
	query.SearchLimit, query.Globs = sqlNavigationSearchMatches, sqlNavSearchGlobs(req.SQLDialect)
	// Positive SQL-only globs with hidden traversal avoid silent loss of
	// migration files in dot-prefixed directories.
	query.SearchHidden = true
	search := func() (WorkspaceSearchResult, error) {
		response := s.workerWorkspaceFile(ctx, query)
		if !response.OK {
			return WorkspaceSearchResult{}, fmt.Errorf("worker SQL search: %s", response.ErrorCode)
		}
		var out WorkspaceSearchResult
		if err := json.Unmarshal([]byte(response.Output), &out); err != nil {
			return out, err
		}
		if !out.Complete || out.Truncated || out.TimedOut {
			return out, errors.New("SQL source candidate discovery is incomplete")
		}
		return out, nil
	}
	first, err := search()
	if err != nil {
		return fileError("sql_schema_search_incomplete", "resource", err), true
	}
	pathSet := map[string]bool{sourcePath: true}
	rollbackMatches := false
	for _, match := range first.Matches {
		if !isSQLSourcePath(match.File) {
			continue
		}
		path, pathErr := safeWorkspaceRelativeFile(match.File)
		if pathErr != nil || path != match.File {
			return fileError("sql_schema_unsafe_path", "security", errors.New("unsafe SQL search target")), true
		}
		if sqlNavRollbackFile(path) {
			rollbackMatches = true
			continue // down migration SQL is not forward-schema evidence
		}
		pathSet[path] = true
		if len(pathSet) > sqlNavigationMaxFiles {
			return fileError("sql_schema_file_limit", "resource", errors.New("SQL navigation exceeds bounded candidate file count")), true
		}
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	versions := make(map[string]string, len(paths))
	var foundItems []sqlNavFound
	tableLifecycleChanged := false
	var tableLifecycle []sqlNavFound
	total := 0
	for _, path := range paths {
		check := req
		check.Action, check.Path, check.FileVersion = "workspace_stat", path, ""
		check.Limit, check.Offset = 0, 0
		stat := s.workerWorkspaceFile(ctx, check)
		if !stat.OK || stat.FileVersion == "" || stat.FileSize == nil ||
			*stat.FileSize > codeMaxDocumentBytes {
			return fileError("sql_schema_target_unstable", "conflict", errors.New("SQL target is not safely inspectable")), true
		}
		total += int(*stat.FileSize)
		if total > sqlNavigationTotalBytes {
			return fileError("sql_schema_byte_limit", "resource", errors.New("SQL source graph exceeds memory budget")), true
		}
		check.Action, check.FileVersion, check.Limit = "workspace_read", stat.FileVersion, codeMaxDocumentBytes
		read := s.workerWorkspaceFile(ctx, check)
		if !read.OK || read.FileVersion != stat.FileVersion || read.EOF == nil || !*read.EOF ||
			read.OutputEncoding != "utf-8" {
			return fileError("sql_schema_target_unstable", "conflict", errors.New("SQL source snapshot changed")), true
		}
		if path == sourcePath && read.FileVersion != req.FileVersion {
			return fileError("file_changed", "conflict", errors.New("requested SQL file changed")), true
		}
		versions[path] = read.FileVersion
		occurrences, valid := sqlNavAllOccurrences(read.Output, req.SQLDialect)
		if !valid {
			return fileError("sql_schema_unparseable", "validation", errors.New("SQL target cannot be safely tokenized")), true
		}
		if sqlNavTableLifecycle(occurrences, selected) {
			tableLifecycleChanged = true
		}
		for _, occ := range occurrences {
			item := sqlNavFound{sqlNavOccurrence: occ,
				sqlNavLocation: sqlNavLocation{URI: path, Range: occ.span, FileVersion: read.FileVersion}}
			if occ.identity == selected.identity {
				foundItems = append(foundItems, item)
			}
			if selected.isColumn && !occ.isColumn && occ.drop && occ.identity == selected.tableIdentity {
				tableLifecycle = append(tableLifecycle, item)
			}
		}
	}
	// The worker search is not an atomic filesystem snapshot. A changed set of
	// matches invalidates results rather than quietly losing new references.
	if s.codeTargetSnapshotHook != nil {
		s.codeTargetSnapshotHook()
	}
	second, err := search()
	if err != nil || !bytes.Equal(sqlNavMatchIdentity(first.Matches), sqlNavMatchIdentity(second.Matches)) {
		return fileError("sql_schema_search_changed", "conflict", errors.New("SQL search changed during navigation")), true
	}
	for _, path := range paths {
		check := req
		check.Action, check.Path, check.FileVersion = "workspace_stat", path, ""
		check.Offset, check.Limit = 0, 0
		stat := s.workerWorkspaceFile(ctx, check)
		if !stat.OK || stat.FileVersion != versions[path] {
			return fileError("sql_schema_target_changed", "conflict", errors.New("SQL candidate file changed during navigation")), true
		}
	}
	// Source-backed relation kinds must agree in *both* directions.
	// A REFRESH or DROP MATERIALIZED VIEW cannot be a reference to an
	// ordinary table/view. Untyped SELECT uses can follow either kind.
	hasMaterialized, hasOrdinary := false, false
	for _, item := range foundItems {
		if item.declaration {
			if item.materialized {
				hasMaterialized = true
			} else {
				hasOrdinary = true
			}
		}
	}
	if hasMaterialized && hasOrdinary {
		return fileError("sql_schema_history_ambiguous", "validation",
			errors.New("source defines conflicting ordinary and materialized relation declarations")), true
	}
	// Typed relation lifecycle operations are not interchangeable. An
	// ordinary DROP TABLE/VIEW cannot belong to a materialized relation
	// history (nor a materialized DROP to an ordinary one). Reject the
	// conflicting source rather than returning a fabricated reference.
	for _, item := range foundItems {
		if item.drop && !item.isColumn &&
			(hasMaterialized && !item.materialized || hasOrdinary && item.materialized) {
			return fileError("sql_schema_history_ambiguous", "validation",
				errors.New("source contains a relation-kind-incompatible lifecycle operation")), true
		}
	}
	if !hasMaterialized {
		if selected.materialized || selected.requiresMaterialized {
			if !hasOrdinary {
				for _, item := range foundItems {
					if item.materialized && item.drop {
						return fileError("sql_schema_history_ambiguous", "validation",
							errors.New("materialized relation lifecycle has no source-backed active declaration")), true
					}
				}
			}
			foundItems = nil
		} else {
			// A type-specific materialized event does not belong to an
			// ordinary relation graph when no materialized CREATE exists.
			filtered := foundItems[:0]
			for _, item := range foundItems {
				if !item.materialized {
					filtered = append(filtered, item)
				}
			}
			foundItems = filtered
		}
	}
	var locations []sqlNavLocation
	if req.CodeMethod == "definition" {
		drops := 0
		hasMigrationDeclaration := false
		for _, item := range foundItems {
			if item.drop {
				drops++
			}
			if item.declaration {
				locations = append(locations, item.sqlNavLocation)
				if sqlNavInsideMigrationTree(item.URI) {
					hasMigrationDeclaration = true
				}
			}
		}
		// A lone declaration from a rollback or unsupported migration is
		// not proof of the forward schema: validate its naming even if unique.
		if drops > 0 || tableLifecycleChanged || len(locations) > 1 ||
			(!sqlNavInsideMigrationTree(sourcePath) && hasMigrationDeclaration) {
			location, resolveErr := sqlNavResolveOrderedDefinition(foundItems, tableLifecycle, sourcePath)
			if resolveErr != nil {
				return fileError("sql_schema_history_ambiguous", "validation", resolveErr), true
			}
			locations = []sqlNavLocation{location}
		}
	} else {
		for _, item := range foundItems {
			if !item.lifecycleOnly {
				locations = append(locations, item.sqlNavLocation)
			}
		}
	}
	if len(locations) == 0 && rollbackMatches {
		return fileError("sql_schema_history_ambiguous", "validation", errors.New("no forward SQL source identified, only rollback migration matches")), true
	}
	if len(locations) > sqlNavigationMaxLocations {
		return fileError("sql_schema_result_limit", "resource", errors.New("too many SQL reference locations")), true
	}
	if locations == nil {
		locations = []sqlNavLocation{}
	}
	data, err := json.Marshal(locations)
	if err != nil || len(data) > 16<<10 {
		return fileError("sql_schema_result_too_large", "resource", errors.New("SQL navigation exceeded bounded output")), true
	}
	return Response{OK: true, Status: "code_" + req.CodeMethod, OutputEncoding: "json", Output: string(data),
		FileVersion: req.FileVersion, BytesReturned: int64(len(data)), BytesSeen: int64(len(data))}, true
}

func sqlNavMatchIdentity(matches []WorkspaceSearchMatch) []byte {
	type identity struct {
		Path string
		Line int
		From int64
		To   int64
	}
	items := make([]identity, 0, len(matches))
	for _, m := range matches {
		items = append(items, identity{m.File, m.Range.Start.Line, m.Range.ByteOffset.Start, m.Range.ByteOffset.End})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		if items[i].From != items[j].From {
			return items[i].From < items[j].From
		}
		return items[i].To < items[j].To
	})
	data, _ := json.Marshal(items)
	return data
}
