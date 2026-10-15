package executor

// SQL index navigation accepts only the conservative CREATE [UNIQUE|
// FULLTEXT|SPATIAL] INDEX [CONCURRENTLY] [IF NOT EXISTS] name ON [ONLY]
// table [USING method] (columns) form. It never guesses references from
// expression indexes, opclasses, predicates, INCLUDE, or arbitrary DDL.
// Indexed table/column references belong to the existing source graph; an
// index does not declare a table or a column.
func sqlNavIndexTarget(statement []sqlDDLToken, dialect string) (sqlNavOccurrence, int, bool) {
	if len(statement) < 6 || statement[0].keyword() != "CREATE" {
		return sqlNavOccurrence{}, 0, false
	}
	i := 1
	if statement[i].keyword() == "FULLTEXT" || statement[i].keyword() == "SPATIAL" {
		if dialect != "mysql" && dialect != "mariadb" {
			return sqlNavOccurrence{}, 0, false
		}
		i++
	} else if statement[i].keyword() == "UNIQUE" {
		i++
	}
	if i >= len(statement) || statement[i].keyword() != "INDEX" {
		return sqlNavOccurrence{}, 0, false
	}
	i++
	if i < len(statement) && statement[i].keyword() == "CONCURRENTLY" {
		if dialect != "postgres" {
			return sqlNavOccurrence{}, 0, false
		}
		i++
	}
	if i+2 < len(statement) && statement[i].keyword() == "IF" &&
		statement[i+1].keyword() == "NOT" && statement[i+2].keyword() == "EXISTS" {
		i += 3
	}
	if i >= len(statement) {
		return sqlNavOccurrence{}, 0, false
	}
	if statement[i].keyword() != "ON" {
		// The optional index name is a normal or qualified identifier, never a
		// user-provided expression or an implicit table name.
		_, next, ok := sqlNavName(statement, i)
		if !ok {
			return sqlNavOccurrence{}, 0, false
		}
		i = next
	}
	if i >= len(statement) || statement[i].keyword() != "ON" {
		return sqlNavOccurrence{}, 0, false
	}
	i++
	if i < len(statement) && statement[i].keyword() == "ONLY" {
		if dialect != "postgres" {
			return sqlNavOccurrence{}, 0, false
		}
		i++
	}
	table, next, ok := sqlNavName(statement, i)
	if !ok {
		return sqlNavOccurrence{}, 0, false
	}
	i = next
	if i+1 < len(statement) && statement[i].keyword() == "USING" {
		switch statement[i+1].keyword() {
		case "BTREE", "HASH", "GIST", "GIN", "SPGIST", "BRIN", "RTREE":
			i += 2
		default:
			return sqlNavOccurrence{}, 0, false
		}
	}
	if i >= len(statement) || statement[i].text != "(" {
		return sqlNavOccurrence{}, 0, false
	}
	return table, i, true
}
