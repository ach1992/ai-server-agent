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
	name, identity, searchTerm string
	span                       sqlDDLRange
	declaration                bool
	drop                       bool
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
	if !ok || next-at > 5 {
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

func sqlNavAt(span sqlDDLRange, line, character int) bool {
	start, end := span.Start, span.End
	return (line > start.Line || line == start.Line && character >= start.Character) &&
		(line < end.Line || line == end.Line && character < end.Character)
}

func sqlNavOccurrences(source, dialect string) ([]sqlNavOccurrence, bool) {
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
		case "CREATE", "ALTER", "DROP", "RENAME", "INSERT", "UPDATE", "DELETE", "SELECT", "TRUNCATE":
		default:
			return
		}
		hasWithClause := false
		for _, token := range statement {
			if token.keyword() == "WITH" {
				hasWithClause = true
				break
			}
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
						(object == "VIEW" && statement[next].keyword() == "AS")) {
					occ.declaration = true
					result = append(result, occ)
				}
			}
		case "ALTER", "DROP":
			if statement[at].keyword() == "TABLE" || statement[at].keyword() == "VIEW" {
				at++
				if at+1 < len(statement) && statement[at].keyword() == "IF" &&
					statement[at+1].keyword() == "EXISTS" {
					at += 2
				}
				if at < len(statement) && statement[at].keyword() == "ONLY" {
					at++
				}
				isLifecycleChange := operation == "DROP"
				if operation == "ALTER" {
					for i := at; i+1 < len(statement); i++ {
						if statement[i].keyword() == "RENAME" &&
							(statement[i+1].keyword() == "TO" || statement[i+1].keyword() == "AS") {
							isLifecycleChange = true
							break
						}
					}
				}
				end := appendName(at, false, isLifecycleChange)
				if operation == "DROP" {
					// DROP TABLE/VIEW accepts comma-separated object names.
					for end > at && end < len(statement) && statement[end].text == "," {
						next := appendName(end+1, false, true)
						if next == end+1 {
							break
						}
						end = next
					}
				} else if isLifecycleChange {
					// Both the old and new names need lifecycle tombstones: a
					// prior CREATE for either cannot prove the active schema.
					for i := end; i+1 < len(statement); i++ {
						if statement[i].keyword() == "RENAME" &&
							(statement[i+1].keyword() == "TO" || statement[i+1].keyword() == "AS") {
							appendName(i+2, false, true)
							break
						}
					}
				}
			}
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
	occurrences, valid := sqlNavOccurrences(source, dialect)
	if !valid {
		return sqlNavOccurrence{}, false
	}
	for _, occ := range occurrences {
		if sqlNavAt(occ.span, line, character) {
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
	selected, found := sqlNavSelected(source, req.SQLDialect, req.Line, req.Character)
	if !found {
		return Response{}, false
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	query := req
	query.Action, query.Path, query.FileVersion = "workspace_text_search", "", ""
	query.Pattern = "(?i)" + regexp.QuoteMeta(selected.searchTerm)
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
	for _, match := range first.Matches {
		if !isSQLSourcePath(match.File) {
			continue
		}
		path, pathErr := safeWorkspaceRelativeFile(match.File)
		if pathErr != nil || path != match.File {
			return fileError("sql_schema_unsafe_path", "security", errors.New("unsafe SQL search target")), true
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
		occurrences, valid := sqlNavOccurrences(read.Output, req.SQLDialect)
		if !valid {
			return fileError("sql_schema_unparseable", "validation", errors.New("SQL target cannot be safely tokenized")), true
		}
		for _, occ := range occurrences {
			if occ.identity == selected.identity {
				foundItems = append(foundItems, sqlNavFound{sqlNavOccurrence: occ,
					sqlNavLocation: sqlNavLocation{URI: path, Range: occ.span, FileVersion: read.FileVersion}})
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
	var locations []sqlNavLocation
	if req.CodeMethod == "definition" {
		drops := 0
		for _, item := range foundItems {
			if item.drop {
				drops++
			}
			if item.declaration {
				locations = append(locations, item.sqlNavLocation)
			}
		}
		if drops > 0 || len(locations) > 1 {
			return fileError("sql_schema_history_ambiguous", "validation",
				errors.New("table was dropped, renamed or redefined; static migration ordering cannot prove the active schema")), true
		}
	} else {
		for _, item := range foundItems {
			locations = append(locations, item.sqlNavLocation)
		}
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
