package executor

// Locate an unambiguous direct CREATE ... AS SELECT entrypoint. A bounded
// PostgreSQL materialized-view output alias list may precede AS; it does not
// introduce a separate query scope or table source.
func sqlNavCreateQueryStart(tokens []sqlDDLToken, at int, materialized bool) int {
	if materialized && at < len(tokens) && tokens[at].text == "(" {
		i, count := at+1, 0
		for i < len(tokens) {
			if count >= 64 || tokens[i].literalBefore ||
				!sqlNavPostgresBareIdentifierAllowed(tokens[i], false) {
				return -1
			}
			count++
			i++
			if i >= len(tokens) {
				return -1
			}
			if tokens[i].literalBefore {
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
		tokens[at+1].keyword() == "SELECT" &&
		!tokens[at].literalBefore && !tokens[at+1].literalBefore {
		return at + 1
	}
	return -1
}

// Query-bearing CREATE and INSERT statements may contain one demonstrably
// complete direct SELECT. Reuse the exact same SELECT source/column decision
// used for standalone queries. Merely spotting FROM or JOIN cannot certify
// a physical source. The surrounding caller must first prove the statement
// header and the SELECT entrypoint.
// A contained CREATE/INSERT query has three distinct outcomes:
// (1) supported and proven with ordinary table dependencies,
// (2) supported and proven with a trivial no-FROM constant projection, or
// (3) unsupported/unproven. Only (1) or (2) can justify its parent CREATE
// declaration. Do not mistake absence of table occurrences for validity.
// Complete query evidence, not absence of recognised errors, is the gate.
func sqlNavNestedSelectScope(tokens []sqlDDLToken, start int, dialect string, materialized bool) (sources, columns []sqlNavOccurrence, proven bool) {
	if start >= len(tokens) || tokens[start].keyword() != "SELECT" {
		return nil, nil, false
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
	if end <= start+1 {
		return nil, nil, false
	}
	// The PostgreSQL materialized-view population suffix is a separate
	// grammar boundary. No opaque literal may be silently discarded between
	// the proven SELECT body and WITH [NO] DATA.
	if end < len(tokens) {
		if tokens[end].literalBefore || tokens[end].literalAfter ||
			!sqlNavNoLiteralGaps(tokens[end:]) {
			return nil, nil, false
		}
	}
	body := tokens[start:end]
	sources, columns = sqlNavSelectAnalysis(body, dialect)
	if len(sources) > 0 {
		if sqlNavNestedBoundedBodyProof(body, dialect) {
			return sources, columns, true
		}
		return nil, nil, false
	}
	// A true scalar SELECT can have no physical source at all. Admit only
	// a complete, small constant projection grammar; complex expressions,
	// CTEs, functions, VALUES and TABLE remain valid SQL but unproven for
	// this offline navigation. An unsupported FROM function cannot certify
	// a CREATE view (nor bypass a malformed later JOIN).
	if sqlNavSelectConstantProjection(body, dialect) {
		return nil, nil, true
	}
	return nil, nil, false
}

func sqlNavSelectConstantProjection(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) < 2 || tokens[0].keyword() != "SELECT" ||
		tokens[len(tokens)-1].literalAfter {
		return false
	}
	// SELECT <numeric|TRUE|FALSE|NULL> [AS alias] [, ...]
	// Signed and unsigned numeric tokens are proved as complete scalars.
	// A quoted SQL string has been elided by the lexer, so it cannot be proved here.
	for at, count := 1, 0; at < len(tokens); {
		if count >= 64 || tokens[at].literalBefore {
			return false
		}
		// A punctuation token is not a scalar: '+', '*' or '(' alone
		// cannot certify a CREATE AS SELECT declaration.
		if tokens[at].kind == 'p' {
			number := sqlNavOnParser{tokens: tokens[at:], dialect: dialect}
			if !number.number() {
				return false
			}
			at += number.at
		} else if tokens[at].keyword() == "TRUE" ||
			tokens[at].keyword() == "FALSE" || tokens[at].keyword() == "NULL" {
			at++
		} else {
			return false
		}
		if at < len(tokens) && tokens[at].keyword() == "AS" {
			// AS and ColLabel are structural tokens; an intervening
			// elided literal cannot be part of the proven scalar.
			if at+1 >= len(tokens) ||
				!sqlNavNoLiteralGaps(tokens[at:at+2]) ||
				!sqlNavSelectAliasAllowed(tokens[at+1], dialect) {
				return false
			}
			at += 2
		}
		count++
		if at == len(tokens) {
			return true
		}
		if tokens[at].text != "," || tokens[at].literalBefore {
			return false
		}
		at++
		if at == len(tokens) {
			return false
		}
	}
	return false
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
			if tokens[at].literalBefore {
				return 0, false
			}
			return at + 1, true
		}
		if tokens[at].text != "," || tokens[at].literalBefore {
			return 0, false
		}
		at++
	}
	return 0, false
}
