package executor

// A source-backed symbol is evidence about a *complete bounded statement*.
// These small grammar recognizers return false on unsupported constructions:
// identifiers and valid prefixes alone never authorize a partial emission.
// They are intentionally not general PostgreSQL/MySQL parsers.

func sqlNavProofParenEnd(tokens []sqlDDLToken, at, limit int) (int, bool) {
	if at >= limit || tokens[at].text != "(" {
		return 0, false
	}
	depth := 0
	for i := at; i < limit; i++ {
		switch tokens[i].text {
		case "(":
			depth++
			if depth > 16 {
				return 0, false
			}
		case ")":
			depth--
			if depth < 0 {
				return 0, false
			}
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func sqlNavProofSegments(tokens []sqlDDLToken, start, end int) ([][]sqlDDLToken, bool) {
	if start >= end || end-start > 512 {
		return nil, false
	}
	var segments [][]sqlDDLToken
	depth, first := 0, start
	for i := start; i < end; i++ {
		switch tokens[i].text {
		case "(":
			depth++
			if depth > 16 {
				return nil, false
			}
		case ")":
			depth--
			if depth < 0 {
				return nil, false
			}
		case ",":
			if depth == 0 {
				if first == i || len(segments) >= 64 {
					return nil, false
				}
				// Preserve the fact that a SQL string literal was elided
				// immediately before this delimiter without making a
				// lossy tokenizer appear to have parsed that literal.
				segment := append([]sqlDDLToken(nil), tokens[first:i]...)
				if tokens[i].literalBefore {
					segment[len(segment)-1].literalAfter = true
				}
				segments = append(segments, segment)
				first = i + 1
			}
		}
	}
	if depth != 0 || first == end || len(segments) >= 64 {
		return nil, false
	}
	segment := append([]sqlDDLToken(nil), tokens[first:end]...)
	if end < len(tokens) && tokens[end].literalBefore {
		segment[len(segment)-1].literalAfter = true
	}
	return append(segments, segment), true
}

// A bounded expression index key admits only a proven simple column or
// column op numeric expression. Unknown expression syntax cannot lend a
// real index table a physical occurrence.
func sqlNavIndexExpressionProven(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, tok := range tokens {
		if tok.literalBefore {
			return false
		}
	}
	if sqlNavProofNumber(tokens, dialect) {
		return true
	}
	if !sqlNavQualifiedColumnHeadAllowed(tokens[0], dialect) {
		return false
	}
	_, end, ok := sqlNavName(tokens, 0)
	if !ok {
		return false
	}
	if end == len(tokens) {
		return true
	}
	if end+1 >= len(tokens) || (tokens[end].text != "+" && tokens[end].text != "-") {
		return false
	}
	return sqlNavProofNumber(tokens[end+1:], dialect)
}

// The accepted key-list grammar contains one simple column per item.
// Complex expression index keys are instead parsed in a separate bounded
// index context and never become guessed column identities.
func sqlNavProofKeyList(tokens []sqlDDLToken, at, limit int, dialect string, index bool) (int, bool) {
	end, ok := sqlNavProofParenEnd(tokens, at, limit)
	if !ok {
		return 0, false
	}
	// Elided literals cannot be invisible key-list members or operands.
	// In particular: PRIMARY KEY (id 'ignored') is not PRIMARY KEY(id).
	for i := at + 1; i < end; i++ {
		if tokens[i].literalBefore {
			return 0, false
		}
	}
	items, ok := sqlNavProofSegments(tokens, at+1, end-1)
	if !ok {
		return 0, false
	}
	for _, item := range items {
		if len(item) == 0 || item[0].literalBefore {
			return 0, false
		}
		j := 0
		if item[0].text == "(" && index {
			if next, nested := sqlNavProofParenEnd(item, 0, len(item)); nested &&
				sqlNavIndexExpressionProven(item[1:next-1], dialect) {
				j = next
			} else {
				return 0, false
			}
		} else {
			// SQL functions (e.g. LOWER(email)) are valid expression index
			// keys, but not simple FK/PRIMARY/UNIQUE column lists.
			if !sqlNavQualifiedColumnHeadAllowed(item[0], dialect) {
				return 0, false
			}
			j = 1
			if index && j < len(item) && item[j].text == "(" {
				next, nested := sqlNavProofParenEnd(item, j, len(item))
				if !nested || !sqlNavIndexExpressionProven(item[j+1:next-1], dialect) {
					return 0, false
				}
				j = next
			}
		}
		if index && j < len(item) &&
			(item[j].keyword() == "ASC" || item[j].keyword() == "DESC") {
			j++
		}
		if index && j+1 < len(item) && item[j].keyword() == "NULLS" &&
			(item[j+1].keyword() == "FIRST" || item[j+1].keyword() == "LAST") {
			j += 2
		}
		if j != len(item) {
			return 0, false
		}
	}
	return end, true
}

func sqlNavProofReference(tokens []sqlDDLToken, at, limit int, dialect string) (int, bool) {
	if at >= limit || tokens[at].keyword() != "REFERENCES" {
		return 0, false
	}
	_, next, ok := sqlNavRelationName(tokens[:limit], at+1, dialect)
	if !ok {
		return 0, false
	}
	if next < limit && tokens[next].text == "(" {
		next, ok = sqlNavProofKeyList(tokens, next, limit, dialect, false)
		if !ok {
			return 0, false
		}
	}
	// Supported referential action: ON DELETE/UPDATE
	// (NO ACTION | RESTRICT | CASCADE | SET NULL/DEFAULT).
	for next < limit && tokens[next].keyword() == "ON" {
		if next+2 >= limit {
			return 0, false
		}
		event := tokens[next+1].keyword()
		if event != "DELETE" && event != "UPDATE" {
			return 0, false
		}
		next += 2
		switch tokens[next].keyword() {
		case "RESTRICT", "CASCADE":
			next++
		case "NO":
			if next+1 >= limit || tokens[next+1].keyword() != "ACTION" {
				return 0, false
			}
			next += 2
		case "SET":
			if next+1 >= limit || (tokens[next+1].keyword() != "NULL" &&
				tokens[next+1].keyword() != "DEFAULT") {
				return 0, false
			}
			next += 2
		default:
			return 0, false
		}
	}
	return next, true
}

func sqlNavProofConstraint(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) == 0 || tokens[0].literalBefore {
		return false
	}
	at := 0
	if tokens[at].keyword() == "CONSTRAINT" {
		if at+2 >= len(tokens) || !sqlNavQualifiedColumnHeadAllowed(tokens[at+1], dialect) {
			return false
		}
		at += 2
	}
	if at >= len(tokens) {
		return false
	}
	switch tokens[at].keyword() {
	case "PRIMARY":
		if at+2 >= len(tokens) || tokens[at+1].keyword() != "KEY" {
			return false
		}
		next, ok := sqlNavProofKeyList(tokens, at+2, len(tokens), dialect, false)
		return ok && next == len(tokens)
	case "FOREIGN":
		if at+2 >= len(tokens) || tokens[at+1].keyword() != "KEY" {
			return false
		}
		at += 2
		if (dialect == "mysql" || dialect == "mariadb") && at < len(tokens) &&
			tokens[at].text != "(" && sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect) {
			at++
		}
		next, ok := sqlNavProofKeyList(tokens, at, len(tokens), dialect, false)
		if !ok {
			return false
		}
		next, ok = sqlNavProofReference(tokens, next, len(tokens), dialect)
		return ok && next == len(tokens)
	case "UNIQUE":
		at++
		if at < len(tokens) && (tokens[at].keyword() == "KEY" || tokens[at].keyword() == "INDEX") {
			if dialect != "mysql" && dialect != "mariadb" {
				return false
			}
			at++
		}
		if (dialect == "mysql" || dialect == "mariadb") && at < len(tokens) &&
			tokens[at].text != "(" && sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect) {
			at++
		}
		next, ok := sqlNavProofKeyList(tokens, at, len(tokens), dialect, false)
		return ok && next == len(tokens)
	case "KEY", "INDEX", "FULLTEXT", "SPATIAL":
		if dialect != "mysql" && dialect != "mariadb" {
			return false
		}
		if tokens[at].keyword() == "FULLTEXT" || tokens[at].keyword() == "SPATIAL" {
			at++
			if at >= len(tokens) || (tokens[at].keyword() != "KEY" && tokens[at].keyword() != "INDEX") {
				return false
			}
		}
		at++
		if at+2 < len(tokens) && tokens[at].keyword() == "IF" &&
			tokens[at+1].keyword() == "NOT" && tokens[at+2].keyword() == "EXISTS" {
			if dialect != "mariadb" {
				return false
			}
			at += 3
		}
		if at < len(tokens) && tokens[at].text != "(" &&
			sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect) {
			at++
		}
		next, ok := sqlNavProofKeyList(tokens, at, len(tokens), dialect, true)
		return ok && next == len(tokens)
	case "CHECK":
		if at+1 >= len(tokens) || tokens[at+1].text != "(" {
			return false
		}
		next, ok := sqlNavProofParenEnd(tokens, at+1, len(tokens))
		return ok && next == len(tokens) &&
			sqlNavSelectOnPredicateComplete(tokens[at+2:next-1], dialect)
	case "LIKE":
		if dialect != "postgres" && dialect != "postgresql" {
			return false
		}
		_, next, ok := sqlNavRelationName(tokens, at+1, dialect)
		return ok && next == len(tokens)
	default:
		return false
	}
}

// A supported column definition has a bare name, a datatype, and only
// explicitly recognised inline modifiers. A DEFAULT string or a complex
// expression that the lossy lexer cannot prove is intentionally unsupported.
func sqlNavProofColumnDef(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) > 0 && tokens[len(tokens)-1].literalAfter &&
		tokens[len(tokens)-1].keyword() != "DEFAULT" {
		return false // unrelated trailing SQL literal after a declaration
	}
	if len(tokens) < 2 || !sqlNavQualifiedColumnHeadAllowed(tokens[0], dialect) ||
		tokens[0].literalBefore || tokens[1].literalBefore ||
		(tokens[1].kind != 'i' && tokens[1].kind != 'q') {
		return false
	}
	at := 2
	switch tokens[1].keyword() {
	case "DOUBLE", "CHARACTER":
		if at < len(tokens) && (tokens[at].keyword() == "PRECISION" || tokens[at].keyword() == "VARYING") {
			at++
		}
	case "TIMESTAMP", "TIME":
		if at+2 < len(tokens) && tokens[at].keyword() == "WITH" &&
			tokens[at+1].keyword() == "TIME" && tokens[at+2].keyword() == "ZONE" {
			at += 3
		}
	}
	if at < len(tokens) && tokens[at].text == "(" {
		next, ok := sqlNavProofParenEnd(tokens, at, len(tokens))
		if !ok {
			return false
		}
		at = next
	}
	for at < len(tokens) {
		if tokens[at].literalBefore {
			return false
		}
		switch tokens[at].keyword() {
		case "PRIMARY":
			if at+1 >= len(tokens) || tokens[at+1].keyword() != "KEY" {
				return false
			}
			at += 2
		case "UNIQUE", "NULL":
			at++
		case "AUTO_INCREMENT", "UNSIGNED":
			if dialect != "mysql" && dialect != "mariadb" {
				return false
			}
			at++
		case "NOT":
			if at+1 >= len(tokens) || tokens[at+1].keyword() != "NULL" {
				return false
			}
			at += 2
		case "DEFAULT":
			// String DEFAULT values are elided by the tokenizer.
			// The closing comma or ')' marks their exact presence;
			// absent that evidence, a dangling DEFAULT is invalid.
			if at+1 == len(tokens) {
				return tokens[at].literalAfter
			}
			if tokens[at+1].literalBefore {
				return false
			}
			value := tokens[at+1]
			if value.kind != 'p' && value.keyword() != "TRUE" && value.keyword() != "FALSE" &&
				value.keyword() != "NULL" {
				return false
			}
			at += 2
		case "REFERENCES":
			next, ok := sqlNavProofReference(tokens, at, len(tokens), dialect)
			if !ok {
				return false
			}
			at = next
		case "CHECK":
			if at+1 >= len(tokens) || tokens[at+1].text != "(" {
				return false
			}
			next, ok := sqlNavProofParenEnd(tokens, at+1, len(tokens))
			if !ok || !sqlNavSelectOnPredicateComplete(tokens[at+2:next-1], dialect) {
				return false
			}
			at = next
		default:
			return false
		}
	}
	return true
}

func sqlNavProofCreateTableEntry(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) == 0 {
		return false
	}
	switch tokens[0].keyword() {
	case "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "KEY", "INDEX", "FULLTEXT", "SPATIAL", "LIKE":
		return sqlNavProofConstraint(tokens, dialect)
	}
	return sqlNavProofColumnDef(tokens, dialect)
}

// A PostgreSQL CREATE TABLE "(...)" grammar must be fully consumed.
// MySQL/MariaDB's ordinary ENGINE=name option is permitted as a compact
// known suffix; other table options stay with the SQL language server.
func sqlNavCreateTableWholeProof(tokens []sqlDDLToken, open int, dialect string) bool {
	if open >= len(tokens) || tokens[open].text != "(" {
		return false
	}
	end, ok := sqlNavProofParenEnd(tokens, open, len(tokens))
	if !ok {
		return false
	}
	entries, ok := sqlNavProofSegments(tokens, open+1, end-1)
	if !ok {
		return false
	}
	for _, entry := range entries {
		if !sqlNavProofCreateTableEntry(entry, dialect) {
			return false
		}
	}
	if end == len(tokens) {
		return true
	}
	if (dialect == "mysql" || dialect == "mariadb") && end+3 == len(tokens) &&
		tokens[end].keyword() == "ENGINE" && tokens[end+1].text == "=" &&
		sqlNavQualifiedColumnHeadAllowed(tokens[end+2], dialect) {
		return true
	}
	return false
}

// The same bounded action list is consumed before *any* ALTER table/column,
// FK, or index events are appended. A later unknown command cannot borrow the
// earlier valid action's table identity.
func sqlNavAlterTableWholeProof(tokens []sqlDDLToken, start int, dialect string) bool {
	actions, ok := sqlNavProofSegments(tokens, start, len(tokens))
	if !ok {
		return false
	}
	for _, action := range actions {
		if !sqlNavProofAlterAction(action, dialect) {
			return false
		}
		// PostgreSQL table-level RENAME TO and SET SCHEMA use their own
		// standalone ALTER TABLE productions, not the comma-delimited
		// alter_table_cmd list. MySQL/MariaDB permit mixed RENAME actions.
		if len(actions) > 1 && (dialect == "postgres" || dialect == "postgresql") &&
			(action[0].keyword() == "RENAME" && len(action) > 1 &&
				action[1].keyword() == "TO" ||
				action[0].keyword() == "SET" && len(action) > 1 &&
					action[1].keyword() == "SCHEMA") {
			return false
		}
	}
	return true
}

func sqlNavProofAlterAction(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) < 2 || tokens[0].literalBefore {
		return false
	}
	switch tokens[0].keyword() {
	case "ADD":
		at := 1
		if tokens[at].keyword() == "COLUMN" {
			at++
		}
		if at+2 < len(tokens) && tokens[at].keyword() == "IF" &&
			tokens[at+1].keyword() == "NOT" && tokens[at+2].keyword() == "EXISTS" {
			if dialect == "postgres" || dialect == "postgresql" || dialect == "mariadb" {
				at += 3
			} else {
				return false
			}
		}
		if at >= len(tokens) {
			return false
		}
		switch tokens[at].keyword() {
		case "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "KEY", "INDEX", "FULLTEXT", "SPATIAL", "CHECK":
			return sqlNavProofConstraint(tokens[at:], dialect)
		}
		return sqlNavProofColumnDef(tokens[at:], dialect)
	case "DROP":
		at := 1
		if tokens[at].keyword() == "CONSTRAINT" {
			at++
			return at+1 == len(tokens) && sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect)
		}
		if tokens[at].keyword() == "INDEX" || tokens[at].keyword() == "KEY" {
			at++
			return (dialect == "mysql" || dialect == "mariadb") && at+1 == len(tokens) &&
				sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect)
		}
		if tokens[at].keyword() == "COLUMN" {
			at++
		}
		if at+1 < len(tokens) && tokens[at].keyword() == "IF" && tokens[at+1].keyword() == "EXISTS" {
			at += 2
		}
		if at >= len(tokens) || !sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect) {
			return false
		}
		at++
		if at == len(tokens) {
			return true
		}
		return at+1 == len(tokens) && (tokens[at].keyword() == "RESTRICT" || tokens[at].keyword() == "CASCADE")
	case "RENAME":
		at := 1
		if tokens[at].keyword() == "COLUMN" || tokens[at].keyword() == "INDEX" || tokens[at].keyword() == "KEY" {
			at++
			if at+2 >= len(tokens) || !sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect) ||
				tokens[at+1].keyword() != "TO" ||
				!sqlNavQualifiedColumnHeadAllowed(tokens[at+2], dialect) {
				return false
			}
			return at+3 == len(tokens)
		}
		if tokens[at].keyword() == "TO" || tokens[at].keyword() == "AS" {
			at++
		}
		if at >= len(tokens) {
			return false
		}
		if dialect == "postgres" || dialect == "postgresql" {
			if tokens[1].keyword() != "TO" {
				return false
			}
		}
		_, next, ok := sqlNavRelationName(tokens, at, dialect)
		return ok && next == len(tokens)
	case "SET":
		if dialect != "postgres" && dialect != "postgresql" ||
			tokens[1].keyword() != "SCHEMA" {
			return false
		}
		_, next, ok := sqlNavRelationName(tokens, 2, dialect)
		return ok && next == len(tokens)
	case "CHANGE", "MODIFY":
		if dialect != "mysql" && dialect != "mariadb" {
			return false
		}
		at := 1
		if tokens[at].keyword() == "COLUMN" {
			at++
		}
		if tokens[0].keyword() == "CHANGE" {
			if at >= len(tokens) || !sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect) {
				return false
			}
			at++
		}
		return sqlNavProofColumnDef(tokens[at:], dialect)
	default:
		return false
	}
}

func sqlNavIndexWholeProof(tokens []sqlDDLToken, open int, dialect string) bool {
	end, ok := sqlNavProofKeyList(tokens, open, len(tokens), dialect, true)
	if !ok {
		return false
	}
	if end == len(tokens) {
		return true
	}
	if tokens[end].keyword() == "WHERE" && (dialect == "postgres" || dialect == "postgresql") &&
		end+1 < len(tokens) {
		predicate := tokens[end+1:]
		// A single boolean column and the bounded ordinary comparison
		// grammar are the supported index predicate subset.
		if len(predicate) == 1 && sqlNavQualifiedColumnHeadAllowed(predicate[0], dialect) &&
			!predicate[0].literalBefore {
			return true
		}
		return sqlNavSelectOnPredicateComplete(predicate, dialect)
	}
	return false
}
