package executor

import "strings"

// PostgreSQL 18's RESERVED_KEYWORD and TYPE_FUNC_NAME_KEYWORD token classes
// cannot be unquoted physical relation, alias or DDL column names (ColId positions).
// Other PostgreSQL keyword classes are allowed as names. Quoted identifiers
// and names after a qualification dot remain legal. This shared physical
// relation-name boundary also governs DDL/DML emission; the lexical tokenizer
// and operation-specific grammar remain unchanged.
//
// Source: postgres/postgres REL_18_STABLE src/include/parser/kwlist.h
// https://www.postgresql.org/docs/current/sql-keywords-appendix.html
var sqlNavPostgresSelectDisallowed = sqlNavKeywordSet(`
ALL ANALYSE ANALYZE AND ANY ARRAY AS ASC ASYMMETRIC BOTH CASE CAST CHECK COLLATE COLUMN
CONSTRAINT CREATE CURRENT_CATALOG CURRENT_DATE CURRENT_ROLE CURRENT_TIME CURRENT_TIMESTAMP
CURRENT_USER DEFAULT DEFERRABLE DESC DISTINCT DO ELSE END EXCEPT FALSE FETCH FOR FOREIGN FROM
GRANT GROUP HAVING IN INITIALLY INTERSECT INTO LATERAL LEADING LIMIT LOCALTIME LOCALTIMESTAMP
NOT NULL OFFSET ON ONLY OR ORDER PLACING PRIMARY REFERENCES RETURNING SELECT SESSION_USER SOME
SYMMETRIC SYSTEM_USER TABLE THEN TO TRAILING TRUE UNION UNIQUE USER USING VARIADIC WHEN WHERE
WINDOW WITH
AUTHORIZATION BINARY COLLATION CONCURRENTLY CROSS CURRENT_SCHEMA FREEZE FULL ILIKE INNER IS
ISNULL JOIN LEFT LIKE NATURAL NOTNULL OUTER OVERLAPS RIGHT SIMILAR TABLESAMPLE VERBOSE
`)

func sqlNavPostgresBareIdentifierAllowed(token sqlDDLToken, afterDot bool) bool {
	if token.kind == 'q' {
		return true
	}
	if token.kind != 'i' || !sqlNavSimpleName.MatchString(token.text) {
		return false
	}
	return afterDot || !sqlNavPostgresSelectDisallowed[token.keyword()]
}

func sqlNavRelationNameAllowed(tokens []sqlDDLToken, start, end int, dialect string) bool {
	if dialect == "mysql" || dialect == "mariadb" {
		return sqlNavMySQLNameAllowed(tokens, start, end, dialect)
	}
	if (dialect != "postgres" && dialect != "postgresql") || end <= start || end-start > 5 {
		return false
	}
	for at := start; at < end; at += 2 {
		if !sqlNavPostgresBareIdentifierAllowed(tokens[at], at > start) {
			return false
		}
	}
	return true
}

// SELECT qualified-column uses supplement the source-backed DDL graph. This is
// intentionally not a general SQL query analyzer: resolve only a bounded,
// single SELECT scope with explicit ordinary tables and optional table aliases.
// No CTE/subquery/derived-table, implicit comma join, NATURAL/LATERAL/USING,
// positional table column-alias list or incomplete JOIN predicate can lend
// a fabricated column identity to another table.
// SQL execution, implicit database access, unqualified columns and projection
// alias inference are never part of this path.
// sqlNavSelectAnalysis is the *single* provenance decision for both
// table-level and column-level SELECT navigation. If any part of the bounded
// statement cannot be proven, neither kind of source-backed occurrence is
// emitted. Never add a second FROM/JOIN scanner for table references.
func sqlNavSelectAnalysis(statement []sqlDDLToken, dialect string) (sources, columns []sqlNavOccurrence) {
	if len(statement) < 4 || statement[0].keyword() != "SELECT" {
		return nil, nil
	}
	// A nested SELECT or a changed SQL scope requires real SQL semantic
	// analysis. Unknown scope is no proof: emit *zero* aliased columns.
	for i, token := range statement {
		// A keyword used as a qualified identifier (a.select, a.with,
		// etc.) is not a new SQL scope or a set operation. PostgreSQL
		// permits keyword attributes after a dot, unlike bare ColId.
		if i > 0 && statement[i-1].text == "." {
			continue
		}
		switch token.keyword() {
		case "WITH", "UNION", "INTERSECT", "EXCEPT", "LATERAL", "NATURAL", "USING", "CROSS", "TABLESAMPLE", "STRAIGHT_JOIN", "QUALIFY":
			return nil, nil
		case "SELECT":
			if i != 0 {
				return nil, nil
			}
		}
	}

	// Bindings are exact folded-or-quoted SQL names -> verified table source.
	// An alias hides its base table name; unaliased schema-qualified tables
	// accept both table.column and schema.table.column spellings.
	tables := make(map[string]sqlNavOccurrence)
	tableTokens := make(map[int]bool)
	sources = make([]sqlNavOccurrence, 0, 8)
	var fromSeen, joined, onSeen, inFromScope bool
	joinModifier := ""     // empty, INNER, LEFT/RIGHT/FULL, or LEFT/RIGHT/FULL OUTER
	onPredicateStart := -1 // current ON start, or -1 after validated boundary
	depth, sourceCount := 0, 0
	for i := 1; i < len(statement); i++ {
		token := statement[i]
		if inFromScope && token.literalBefore {
			return nil, nil // elided string makes FROM/JOIN scope unprovable
		}
		// PostgreSQL permits reserved keyword attribute names *after* a
		// dot, e.g. a.from or a.join. They cannot open a FROM/JOIN/ON
		// clause or change scope; the full ON predicate is validated at
		// its normal boundary rather than token by token here.
		if i > 0 && statement[i-1].text == "." &&
			(token.kind == 'i' || token.kind == 'q') {
			continue
		}
		// Every top-level token between ordinary table bindings is part of
		// the bounded FROM/JOIN grammar; unknown gaps are not harmless.
		// While an ON expression is pending, its separate grammar owns the
		// tokens until JOIN/clause/EOF. Do not mistake them for table syntax.
		if inFromScope && onPredicateStart < 0 {
			keyword := token.keyword()
			switch {
			case joined && !onSeen:
				if keyword != "ON" {
					return nil, nil // a JOIN source must be followed by ON
				}
			case joinModifier != "":
				if keyword != "JOIN" && !(keyword == "OUTER" &&
					(joinModifier == "LEFT" || joinModifier == "RIGHT" || joinModifier == "FULL")) {
					return nil, nil // modifier can only be followed by OUTER/JOIN
				}
			default:
				switch keyword {
				case "JOIN", "INNER", "LEFT", "RIGHT", "FULL",
					"WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "FETCH", "WINDOW", "FOR":
					// Complete ordinary table reference, supported next construct.
				default:
					return nil, nil // unconsumed source/alias/punctuation
				}
			}
		}
		switch token.text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return nil, nil
			}
		}
		if depth != 0 {
			// A nested FROM/JOIN, even inside an expression, is outside
			// the supported single-scope SELECT grammar.
			if token.keyword() == "FROM" || token.keyword() == "JOIN" {
				return nil, nil
			}
			continue
		}
		if token.text == "," && inFromScope {
			return nil, nil // comma-separated FROM sources need separate scope analysis
		}
		switch token.keyword() {
		case "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "FETCH", "WINDOW", "FOR":
			if joinModifier != "" || joined && (!onSeen || onPredicateStart >= 0 &&
				!sqlNavSelectOnPredicateComplete(statement[onPredicateStart:i], dialect)) {
				return nil, nil // an ON keyword alone cannot prove a JOIN predicate
			}
			onPredicateStart = -1
			inFromScope = false
		case "INNER", "LEFT", "RIGHT", "FULL":
			// Exactly one JOIN kind may precede JOIN. Accept optional OUTER
			// only after LEFT/RIGHT/FULL. Mixed or repeated kinds can make
			// invalid SQL look like a valid source-backed column graph.
			if !fromSeen || !inFromScope || joinModifier != "" ||
				token.keyword() == "FULL" && dialect != "postgres" && dialect != "postgresql" {
				return nil, nil // MySQL and MariaDB do not support FULL [OUTER] JOIN
			}
			if joined && onPredicateStart >= 0 {
				if !sqlNavSelectOnPredicateComplete(statement[onPredicateStart:i], dialect) {
					return nil, nil
				}
				onPredicateStart = -1
			}
			joinModifier = token.keyword()
		case "OUTER":
			if joinModifier != "LEFT" && joinModifier != "RIGHT" && joinModifier != "FULL" {
				return nil, nil
			}
			joinModifier += " OUTER"
		case "ON":
			if !joined || onSeen || joinModifier != "" {
				return nil, nil
			}
			onSeen = true
			onPredicateStart = i + 1
		case "FROM", "JOIN":
			if token.keyword() == "FROM" && joinModifier != "" {
				return nil, nil
			}
			if token.keyword() == "JOIN" {
				joinModifier = ""
			}
			sourceCount++
			if sourceCount > 8 {
				return nil, nil // bounded ordinary SELECT source set
			}
			if token.keyword() == "FROM" {
				if fromSeen {
					return nil, nil
				}
				fromSeen, inFromScope = true, true
			} else {
				if !fromSeen || !inFromScope || joined && (!onSeen ||
					onPredicateStart >= 0 && !sqlNavSelectOnPredicateComplete(statement[onPredicateStart:i], dialect)) {
					return nil, nil
				}
				joined, onSeen = true, false
				onPredicateStart = -1
			}
			start := i + 1
			table, end, valid := sqlNavRelationName(statement, start, dialect)
			if !valid || strings.Count(table.identity, "\x00") > 1 {
				return nil, nil
			}
			if end < len(statement) && (statement[end].text == "(" || statement[end].text == ".") {
				return nil, nil // table-valued function / incomplete qualification
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
					return nil, nil
				}
				alias = name.identity
				end = next
			} else if explicit {
				return nil, nil // AS requires one valid alias
			}
			// PostgreSQL FROM table AS alias (new_col, ...) renames the
			// table columns positionally. Binding alias.new_col directly
			// to table.new_col is semantically wrong. Reject the *whole*
			// SELECT until precise positional remapping is supported.
			if alias != "" && end < len(statement) && statement[end].text == "(" {
				return nil, nil
			}
			// sqlNavName and alias parsing advance past whole token ranges.
			// The outer loop never visits these tokens: inspect every
			// consumed name, qualification dot, optional AS and alias for
			// skipped SQL strings before certifying a physical table binding.
			for at := start; at < end; at++ {
				if statement[at].literalBefore {
					return nil, nil
				}
			}
			keys := []string{table.identity}
			if alias != "" {
				keys = []string{alias}
			} else if at := strings.LastIndex(table.identity, "\x00"); at >= 0 {
				keys = append(keys, table.identity[at+1:])
			}
			for _, key := range keys {
				if _, exists := tables[key]; exists {
					return nil, nil // duplicate/ambiguous exposed table or alias
				}
				tables[key] = table
			}
			sources = append(sources, table)
			i = end - 1
		}
	}
	if depth != 0 || !fromSeen || joinModifier != "" ||
		inFromScope && statement[len(statement)-1].literalAfter ||
		joined && (!onSeen || onPredicateStart >= 0 &&
			!sqlNavSelectOnPredicateComplete(statement[onPredicateStart:], dialect)) || len(tables) == 0 {
		return nil, nil
	}

	// Qualified spelling is proven by FROM/JOIN, not guessed from SELECT
	// expressions. Select only the actual column token, so a schema prefix
	// or alias declaration cannot masquerade as the column at the cursor.
	result := make([]sqlNavOccurrence, 0, 8)
	for i := 1; i+2 < len(statement); i++ {
		if tableTokens[i] || i > 0 && statement[i-1].text == "." ||
			(statement[i].kind != 'i' && statement[i].kind != 'q') || statement[i+1].text != "." {
			continue
		}
		_, end, valid := sqlNavName(statement, i)
		if !valid || end <= i+1 || end-i != 3 && end-i != 5 ||
			end < len(statement) && (statement[end].text == "." || statement[end].text == "(") {
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
			if len(result) >= sqlNavigationMaxLocations {
				return nil, nil // no silent partial column graph on output cap
			}
			result = append(result, item)
		}
	}
	return sources, result
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
	return (dialect == "postgres" || dialect == "postgresql") &&
		sqlNavPostgresBareIdentifierAllowed(token, false)
}
