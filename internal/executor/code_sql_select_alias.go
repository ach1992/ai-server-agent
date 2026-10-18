package executor

import "strings"

// SELECT qualified-column uses supplement the source-backed DDL graph. This is
// intentionally not a general SQL query analyzer: resolve only a bounded,
// single SELECT scope with explicit ordinary tables and optional table aliases.
// No CTE/subquery/derived-table, implicit comma join, NATURAL/LATERAL/USING or
// set operation can lend a fabricated column identity to another table.
// SQL execution, implicit database access, unqualified columns and projection
// alias inference are never part of this path.
func sqlNavSelectColumns(statement []sqlDDLToken, dialect string) []sqlNavOccurrence {
	if len(statement) < 6 || statement[0].keyword() != "SELECT" {
		return nil
	}
	// A nested SELECT or a changed SQL scope requires real SQL semantic
	// analysis. Unknown scope is no proof: emit *zero* aliased columns.
	for i, token := range statement {
		switch token.keyword() {
		case "WITH", "UNION", "INTERSECT", "EXCEPT", "LATERAL", "NATURAL", "USING", "CROSS", "TABLESAMPLE", "STRAIGHT_JOIN", "QUALIFY":
			return nil
		case "SELECT":
			if i != 0 {
				return nil
			}
		}
	}

	// Bindings are exact folded-or-quoted SQL names -> verified table source.
	// An alias hides its base table name; unaliased schema-qualified tables
	// accept both table.column and schema.table.column spellings.
	tables := make(map[string]sqlNavOccurrence)
	tableTokens := make(map[int]bool)
	var fromSeen, joined, onSeen, inFromScope bool
	depth, sourceCount := 0, 0
	for i := 1; i < len(statement); i++ {
		token := statement[i]
		switch token.text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return nil
			}
		}
		if depth != 0 {
			// A nested FROM/JOIN, even inside an expression, is outside
			// the supported single-scope SELECT grammar.
			if token.keyword() == "FROM" || token.keyword() == "JOIN" {
				return nil
			}
			continue
		}
		if token.text == "," && inFromScope {
			return nil // comma-separated FROM sources need separate scope analysis
		}
		switch token.keyword() {
		case "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "FETCH", "WINDOW", "FOR":
			if joined && !onSeen {
				return nil // incomplete JOIN cannot prove the column context
			}
			inFromScope = false
		case "ON":
			if !joined || onSeen {
				return nil
			}
			onSeen = true
		case "FROM", "JOIN":
			sourceCount++
			if sourceCount > 8 {
				return nil // bounded ordinary SELECT source set
			}
			if token.keyword() == "FROM" {
				if fromSeen {
					return nil
				}
				fromSeen, inFromScope = true, true
			} else {
				if !fromSeen || (joined && !onSeen) || !inFromScope {
					return nil
				}
				joined, onSeen = true, false
			}
			start := i + 1
			table, end, valid := sqlNavName(statement, start)
			if !valid || strings.Count(table.identity, "\x00") > 1 ||
				((dialect == "mysql" || dialect == "mariadb") &&
					!sqlNavMySQLNameAllowed(statement, start, end, dialect)) {
				return nil
			}
			if end < len(statement) && (statement[end].text == "(" || statement[end].text == ".") {
				return nil // table-valued function / incomplete qualification
			}
			for at := start; at < end; at++ {
				tableTokens[at] = true
			}
			// SQL aliases can be unadorned or use AS. Read at most one
			// identifier; a reserved clause keyword is never an alias.
			alias := ""
			aliasAt := end
			explicit := end < len(statement) && statement[end].keyword() == "AS"
			if explicit {
				aliasAt++
			}
			if aliasAt < len(statement) &&
				(statement[aliasAt].kind == 'i' || statement[aliasAt].kind == 'q') &&
				sqlNavSelectAliasAllowed(statement[aliasAt], dialect) {
				name, next, ok := sqlNavName(statement, aliasAt)
				if !ok || next != aliasAt+1 {
					return nil
				}
				alias = name.identity
				end = next
			} else if explicit {
				return nil // AS requires one valid alias
			}
			keys := []string{table.identity}
			if alias != "" {
				keys = []string{alias}
			} else if at := strings.LastIndex(table.identity, "\x00"); at >= 0 {
				keys = append(keys, table.identity[at+1:])
			}
			for _, key := range keys {
				if _, exists := tables[key]; exists {
					return nil // duplicate/ambiguous exposed table or alias
				}
				tables[key] = table
			}
			i = end - 1
		}
	}
	if depth != 0 || !fromSeen || (joined && !onSeen) || len(tables) == 0 {
		return nil
	}

	// Qualified spelling is proven by FROM/JOIN, not guessed from SELECT
	// expressions. Select only the actual column token, so a schema prefix
	// or alias declaration cannot masquerade as the column at the cursor.
	result := make([]sqlNavOccurrence, 0, 8)
	for i := 1; i+2 < len(statement) && len(result) < sqlNavigationMaxLocations; i++ {
		if tableTokens[i] || i > 0 && statement[i-1].text == "." ||
			(statement[i].kind != 'i' && statement[i].kind != 'q') || statement[i+1].text != "." {
			continue
		}
		_, end, valid := sqlNavName(statement, i)
		if !valid || end <= i+1 || end-i != 3 && end-i != 5 ||
			end < len(statement) && statement[end].text == "." {
			continue
		}
		column := statement[end-1]
		if tableTokens[end-1] || column.text == "" {
			continue
		}
		qualifier, _, valid := sqlNavName(statement[:end-2], i)
		if !valid {
			continue
		}
		table, found := tables[qualifier.identity]
		if !found {
			continue
		}
		if item, ok := sqlNavColumn(table, column, false, false); ok {
			result = append(result, item)
		}
	}
	return result
}

// Restrict bare aliases to ordinary identifiers rather than interpreting SQL
// grammar/clauses as aliases. Quoted identifiers still receive normal length,
// escape and scope validation from sqlNavName.
func sqlNavSelectAliasAllowed(token sqlDDLToken, dialect string) bool {
	if token.kind == 'q' {
		return true
	}
	if token.kind != 'i' || !sqlNavSimpleName.MatchString(token.text) {
		return false
	}
	switch token.keyword() {
	case "FROM", "JOIN", "ON", "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "FETCH", "WINDOW", "FOR", "BY", "AS", "INNER", "LEFT", "RIGHT", "FULL", "OUTER", "NATURAL", "CROSS", "LATERAL", "USING", "UNION", "EXCEPT", "INTERSECT", "WITH", "RETURNING", "LOCK", "TABLESAMPLE", "QUALIFY", "STRAIGHT_JOIN", "SELECT", "UPDATE", "DELETE", "INSERT", "VALUES", "INTO":
		return false
	}
	if dialect == "mysql" || dialect == "mariadb" {
		return sqlNavMySQLBareIdentifierAllowed(token, dialect, false)
	}
	return true
}
