package executor

// Direct DML navigation has the same proof obligation as CREATE and SELECT:
// a valid target identifier is not enough to publish a physical occurrence.
// This bounded subset proves the complete direct body before the existing
// statement dispatcher may emit either table or target-column events.
// Complex valid DML remains available to SQLLens, not guessed by this graph.
func sqlNavDirectDMLProven(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) < 3 || len(tokens) > 512 || tokens[0].literalBefore {
		return false
	}
	at, operation := 1, tokens[0].keyword()
	switch operation {
	case "INSERT":
		if tokens[at].keyword() != "INTO" || tokens[at].literalBefore {
			return false
		}
		at++
	case "UPDATE":
		if (dialect == "postgres" || dialect == "postgresql") && tokens[at].keyword() == "ONLY" {
			at++
		}
	case "DELETE":
		if tokens[at].keyword() != "FROM" {
			return false
		}
		at++
		if at < len(tokens) && (dialect == "postgres" || dialect == "postgresql") && tokens[at].keyword() == "ONLY" {
			at++
		}
	case "TRUNCATE":
		if tokens[at].keyword() == "TABLE" {
			at++
		}
	default:
		return false
	}
	_, end, valid := sqlNavRelationName(tokens, at, dialect)
	if !valid || end < len(tokens) && tokens[end].literalBefore {
		return false
	}
	if operation == "TRUNCATE" {
		// Basic TRUNCATE, plus the PostgreSQL single-table identity/action
		// suffix. CASCADE/RESTRICT cannot be used as table aliases.
		if tokens[len(tokens)-1].literalAfter {
			return false
		}
		if end < len(tokens) && (dialect == "postgres" || dialect == "postgresql") &&
			end+1 < len(tokens) &&
			(tokens[end].keyword() == "RESTART" || tokens[end].keyword() == "CONTINUE") &&
			tokens[end+1].keyword() == "IDENTITY" {
			end += 2
		}
		if end < len(tokens) && (dialect == "postgres" || dialect == "postgresql") &&
			(tokens[end].keyword() == "CASCADE" || tokens[end].keyword() == "RESTRICT") {
			end++
		}
		return end == len(tokens) && sqlNavNoLiteralGaps(tokens)
	}
	if operation == "DELETE" {
		return sqlNavDMLWhereProven(tokens, end, dialect)
	}
	if operation == "INSERT" {
		end, valid = sqlNavInsertColumnListEnd(tokens, end, dialect)
		if !valid || end >= len(tokens) || tokens[end].literalBefore {
			return false
		}
		switch tokens[end].keyword() {
		case "SELECT":
			_, _, proven := sqlNavNestedSelectScope(tokens, end, dialect, false)
			return proven
		case "VALUES":
			return sqlNavDMLValuesProven(tokens, end+1, dialect)
		default:
			return false
		}
	}
	// UPDATE target SET col = scalar [, ...] [WHERE bounded predicate]
	// Assignment LHS and scalar RHS are independently validated; no
	// multi-target UPDATE/FROM/RETURNING or other inferred DML semantics.
	if end >= len(tokens) || tokens[end].keyword() != "SET" {
		return false
	}
	start := end + 1
	where := len(tokens)
	depth := 0
	for i := start; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return false
			}
		}
		if depth == 0 && tokens[i].keyword() == "WHERE" {
			where = i
			break
		}
	}
	if depth != 0 || start >= where {
		return false
	}
	assignments, ok := sqlNavProofSegments(tokens, start, where)
	if !ok || len(assignments) == 0 {
		return false
	}
	for _, assignment := range assignments {
		if !sqlNavDMLAssignmentProven(assignment, dialect) {
			return false
		}
	}
	if where == len(tokens) {
		return true
	}
	return sqlNavDMLWhereProven(tokens, where, dialect)
}

func sqlNavDMLWhereProven(tokens []sqlDDLToken, at int, dialect string) bool {
	if at == len(tokens) {
		return true
	}
	return at+1 < len(tokens) && tokens[at].keyword() == "WHERE" &&
		sqlNavSelectOnPredicateComplete(tokens[at+1:], dialect)
}

// A quoted string is intentionally elided by the shared tokenizer. It can
// count as one complete scalar ONLY when a delimiter/statement boundary
// proves that no visible expression was joined to it. Every other gap is
// unsupported. This never interprets or executes the literal's content.
func sqlNavDMLScalarProven(tokens []sqlDDLToken, trailingLiteral bool, dialect string) bool {
	if len(tokens) == 0 {
		return trailingLiteral
	}
	if trailingLiteral || !sqlNavNoLiteralGaps(tokens) {
		return false
	}
	if len(tokens) == 1 {
		switch tokens[0].keyword() {
		case "TRUE", "FALSE", "NULL", "DEFAULT":
			return true
		}
	}
	if sqlNavProofNumber(tokens, dialect) {
		return true
	}
	if !sqlNavOnNameAllowed(tokens[0], dialect, false) {
		return false
	}
	_, end, ok := sqlNavName(tokens, 0)
	return ok && end == len(tokens)
}

func sqlNavDMLAssignmentProven(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) < 2 || tokens[0].literalBefore ||
		!sqlNavQualifiedColumnHeadAllowed(tokens[0], dialect) {
		return false
	}
	_, end, ok := sqlNavName(tokens, 0)
	if !ok || end >= len(tokens) || tokens[end].text != "=" ||
		tokens[end].literalBefore || tokens[0].literalAfter {
		return false
	}
	// If a final elided string follows '=', sqlNavProofSegments carries
	// its presence as literalAfter on the '=' boundary.
	if end+1 == len(tokens) && tokens[end].literalAfter {
		return tokens[end].literalAfterCount == 1
	}
	return sqlNavDMLScalarProven(tokens[end+1:], false, dialect) &&
		!tokens[len(tokens)-1].literalAfter
}

func sqlNavDMLValuesProven(tokens []sqlDDLToken, at int, dialect string) bool {
	rows := 0
	for at < len(tokens) {
		if rows >= 64 || tokens[at].text != "(" || tokens[at].literalBefore {
			return false
		}
		end, ok := sqlNavProofParenEnd(tokens, at, len(tokens))
		if !ok || tokens[end-1].literalAfter {
			return false
		}
		// Each value must be exactly one supported scalar or one opaque
		// quoted literal. Commas/closing brackets own an elided literal
		// immediately before themselves, not arbitrary surrounding text.
		start, count := at+1, 0
		for i := start; i < end; i++ {
			if tokens[i].text != "," && i != end-1 {
				continue
			}
			if count >= 64 || tokens[i].literalBeforeCount > 1 ||
				!sqlNavDMLScalarProven(tokens[start:i], tokens[i].literalBefore, dialect) {
				return false
			}
			count++
			start = i + 1
		}
		if count == 0 || start != end {
			return false
		}
		rows++
		at = end
		if at == len(tokens) {
			return true
		}
		if tokens[at].text != "," || tokens[at].literalBefore || tokens[at].literalAfter {
			return false
		}
		at++
	}
	return false
}
