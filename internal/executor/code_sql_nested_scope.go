package executor

// Locate an unambiguous direct CREATE ... AS SELECT entrypoint. A bounded
// PostgreSQL materialized-view output alias list may precede AS; it does not
// introduce a separate query scope or table source.
func sqlNavCreateQueryStart(tokens []sqlDDLToken, at int, materialized bool) int {
	if materialized && at < len(tokens) && tokens[at].text == "(" {
		i, count := at+1, 0
		for i < len(tokens) {
			if count >= 64 || !sqlNavPostgresBareIdentifierAllowed(tokens[i], false) {
				return -1
			}
			count++
			i++
			if i >= len(tokens) {
				return -1
			}
			if tokens[i].text == ")" {
				at = i + 1
				break
			}
			if tokens[i].text != "," {
				return -1
			}
			i++
		}
	}
	if at+1 < len(tokens) && tokens[at].keyword() == "AS" &&
		tokens[at+1].keyword() == "SELECT" {
		return at + 1
	}
	return -1
}

// Query-bearing CREATE and INSERT statements may contain one demonstrably
// complete direct SELECT. Reuse the exact same SELECT source/column decision
// used for standalone queries. Merely spotting FROM or JOIN cannot certify
// a physical source. The surrounding caller must first prove the statement
// header and the SELECT entrypoint.
func sqlNavNestedSelectScope(tokens []sqlDDLToken, start int, dialect string, materialized bool) (sources, columns []sqlNavOccurrence, invalid bool) {
	if start >= len(tokens) || tokens[start].keyword() != "SELECT" {
		return nil, nil, false // unsupported query family, not a table reference
	}
	end := len(tokens)
	if materialized && end >= start+3 &&
		tokens[end-2].keyword() == "WITH" && tokens[end-1].keyword() == "DATA" {
		end -= 2
	} else if materialized && end >= start+4 &&
		tokens[end-3].keyword() == "WITH" && tokens[end-2].keyword() == "NO" &&
		tokens[end-1].keyword() == "DATA" {
		end -= 3
	}
	body := tokens[start:end]
	sources, columns = sqlNavSelectAnalysis(body, dialect)
	if len(sources) > 0 {
		return sources, columns, false
	}
	// SELECT without a physical FROM has no dependencies. A table-valued
	// function (FROM fn(...)) can also be legitimate but is deliberately
	// unsupported for navigation; do not invalidate a valid view merely
	// because its dependency is not an ordinary relation.
	for i := 1; i < len(body); i++ {
		if i > 0 && body[i-1].text == "." {
			continue
		}
		if body[i].keyword() == "JOIN" {
			return nil, nil, true
		}
		if body[i].keyword() != "FROM" {
			continue
		}
		_, next, ok := sqlNavName(body, i+1)
		if ok && next < len(body) && body[next].text == "(" {
			return nil, nil, false
		}
		return nil, nil, true // ordinary source exists, but scope was rejected
	}
	return nil, nil, false
}

// Only INSERT INTO table [(one, column, list)] SELECT is a supported direct
// embedded-query header. The column list must be complete before its table
// target can become a source-backed use; unknown prefixes fail closed.
func sqlNavInsertColumnListEnd(tokens []sqlDDLToken, at int, dialect string) (int, bool) {
	if at >= len(tokens) || tokens[at].text != "(" {
		return at, true
	}
	at++
	count := 0
	for at < len(tokens) {
		if count >= 64 || !sqlNavQualifiedColumnHeadAllowed(tokens[at], dialect) ||
			tokens[at].literalBefore {
			return 0, false
		}
		count++
		at++
		if at >= len(tokens) {
			return 0, false
		}
		if tokens[at].text == ")" {
			return at + 1, true
		}
		if tokens[at].text != "," {
			return 0, false
		}
		at++
	}
	return 0, false
}
