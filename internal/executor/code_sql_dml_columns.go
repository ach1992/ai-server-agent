package executor

import "strings"

// In a complete, single-target UPDATE/DELETE statement, each unqualified
// identifier that the existing bounded DML/predicate grammars have proven is
// a column of that *one* physical target. No live schema is queried. Unknown
// scopes, qualifiers and richer DML yield no column graph rather than links
// to a guessed migration.
//
// This is called only from sqlNavColumns after the shared whole-statement
// sqlNavDirectDMLProven gate, so table and column consumers cannot disagree
// about the statement's validity.
func sqlNavDirectDMLColumns(statement []sqlDDLToken, dialect string) []sqlNavOccurrence {
	if len(statement) < 4 || !sqlNavDirectDMLProven(statement, dialect) {
		return nil
	}
	operation := statement[0].keyword()
	at := 1
	switch operation {
	case "UPDATE":
		// PostgreSQL inheritance modifier, never a physical table name.
		if (dialect == "postgres" || dialect == "postgresql") && statement[at].keyword() == "ONLY" {
			at++
		}
	case "DELETE":
		if statement[at].keyword() != "FROM" {
			return nil
		}
		at++
		if (dialect == "postgres" || dialect == "postgresql") && statement[at].keyword() == "ONLY" {
			at++
		}
	default:
		return nil
	}
	target, afterTarget, ok := sqlNavRelationName(statement, at, dialect)
	if !ok {
		return nil
	}

	columns := make([]sqlNavOccurrence, 0, 16)
	where := len(statement)
	if operation == "UPDATE" {
		if afterTarget >= len(statement) || statement[afterTarget].keyword() != "SET" {
			return nil
		}
		start := afterTarget + 1
		// Parent grammar disallows nested scopes in SET except proven scalar
		// syntax. Find WHERE only after the complete assignment list.
		for i := start; i < len(statement); i++ {
			if statement[i].keyword() == "WHERE" {
				where = i
				break
			}
		}
		assignments, valid := sqlNavProofSegments(statement, start, where)
		if !valid || len(assignments) == 0 {
			return nil
		}
		for _, assignment := range assignments {
			_, lhsEnd, valid := sqlNavName(assignment, 0)
			if !valid || lhsEnd >= len(assignment) || assignment[lhsEnd].text != "=" {
				return nil
			}
			// PostgreSQL UPDATE target LHS is a bare ColId. MySQL and
			// MariaDB may use a matching single-table qualifier, but
			// never bind another physical table or unknown alias.
			lhs, safe := sqlNavDMLBoundColumn(target, assignment[:lhsEnd], dialect,
				operation == "UPDATE")
			if !safe {
				return nil
			}
			columns = append(columns, lhs)

			// An RHS may be an elided literal, numeric/boolean/NULL/DEFAULT
			// scalar or exactly one column name. Never turn a literal,
			// function, expression fragment or foreign qualifier into a
			// source-backed column.
			rhs := assignment[lhsEnd+1:]
			if len(rhs) > 0 && rhs[0].kind != 'p' {
				if sqlNavDMLConstant(rhs) {
					continue
				}
				_, end, valid := sqlNavName(rhs, 0)
				if valid && end == len(rhs) {
					value, safe := sqlNavDMLBoundColumn(target, rhs, dialect, false)
					if !safe {
						return nil
					}
					columns = append(columns, value)
				}
			}
		}
	} else {
		where = afterTarget
	}

	if where == len(statement) {
		return columns
	}
	if statement[where].keyword() != "WHERE" {
		return nil
	}
	// sqlNavDirectDMLProven has already accepted this ENTIRE bounded
	// predicate, including its operators, depth, skipped literals and
	// dialect. Scan only its recognized identifiers: WHERE conditions
	// cannot bring a new relation into this single-target scope.
	for i := where + 1; i < len(statement); {
		tok := statement[i]
		if sqlNavDMLConstant(statement[i:i+1]) ||
			!sqlNavOnNameAllowed(tok, dialect, false) {
			i++
			continue
		}
		_, end, valid := sqlNavName(statement, i)
		if !valid || end <= i {
			return nil
		}
		col, safe := sqlNavDMLBoundColumn(target, statement[i:end], dialect, false)
		if !safe {
			// A foreign qualifier cannot lend an invented target column
			// identity to other identifiers in that same predicate.
			return nil
		}
		columns = append(columns, col)
		i = end
	}
	return columns
}

func sqlNavDMLConstant(tokens []sqlDDLToken) bool {
	if len(tokens) != 1 || tokens[0].kind != 'i' {
		return false
	}
	switch tokens[0].keyword() {
	case "TRUE", "FALSE", "NULL", "DEFAULT":
		return true
	}
	return false
}

// A proven bare identifier belongs to the only DML target. A qualified
// reference belongs to it only if its explicit prefix matches the target
// name or exact schema.table name (no unknown table aliases or joins).
// The returned bool represents valid provenance, not presence of a column.
func sqlNavDMLBoundColumn(table sqlNavOccurrence, tokens []sqlDDLToken, dialect string, assignment bool) (sqlNavOccurrence, bool) {
	if len(tokens) == 0 || len(tokens) > 5 {
		return sqlNavOccurrence{}, false
	}
	parsed, end, valid := sqlNavName(tokens, 0)
	if !valid || end != len(tokens) || sqlNavDMLConstant(tokens) {
		return sqlNavOccurrence{}, false
	}
	for i := 0; i < len(tokens); i += 2 {
		if !sqlNavOnNameAllowed(tokens[i], dialect, i > 0) {
			return sqlNavOccurrence{}, false
		}
	}
	parts := strings.Split(parsed.identity, "\x00")
	tableParts := strings.Split(table.identity, "\x00")
	if len(parts) == 1 {
		col, ok := sqlNavDDLColumn(table, tokens[0], false, false, dialect)
		return col, ok
	}
	if assignment && (dialect == "postgres" || dialect == "postgresql") {
		return sqlNavOccurrence{}, false // UPDATE SET table.col is not a PG ColId
	}
	prefix := strings.Join(parts[:len(parts)-1], "\x00")
	validPrefix := len(parts) == 2 && prefix == tableParts[len(tableParts)-1] ||
		len(parts) == 3 && prefix == table.identity
	if !validPrefix {
		return sqlNavOccurrence{}, false
	}
	// A post-dot attribute may legally use otherwise-reserved keywords
	// such as accounts.select. This is not a bare DDL ColId position.
	return sqlNavColumn(table, tokens[len(tokens)-1], false, false)
}
