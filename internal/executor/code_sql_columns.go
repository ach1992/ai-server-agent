package executor

// Column navigation supplements table-level SQL source navigation. Only
// deterministic DDL/foreign-key/INSERT contexts are recognized. It never
// infers column lineage from unqualified SELECT expressions, aliases, views,
// stored procedures, or a database connection.
func sqlNavColumn(table sqlNavOccurrence, token sqlDDLToken, declaration, drop bool) (sqlNavOccurrence, bool) {
	if table.isColumn || table.identity == "" {
		return sqlNavOccurrence{}, false
	}
	child, _, ok := sqlNavName([]sqlDDLToken{token}, 0)
	if !ok || child.name == "" {
		return sqlNavOccurrence{}, false
	}
	return sqlNavOccurrence{
		name:       table.name + "." + child.name,
		identity:   table.identity + "\x00column\x00" + child.identity,
		searchTerm: child.searchTerm, tableIdentity: table.identity, tableSearchTerm: table.searchTerm,
		span: child.span, declaration: declaration, drop: drop, isColumn: true,
	}, true
}

// DDL column identifiers are bare ColId positions, unlike a qualified
// SELECT attribute after a dot. Validate before publishing a physical source
// occurrence; SQL keyword tokens are not unquoted column declarations/uses.
func sqlNavDDLColumn(table sqlNavOccurrence, token sqlDDLToken, declaration, drop bool, dialect string) (sqlNavOccurrence, bool) {
	if token.literalBefore {
		return sqlNavOccurrence{}, false
	}
	if dialect == "postgres" || dialect == "postgresql" {
		if !sqlNavPostgresBareIdentifierAllowed(token, false) {
			return sqlNavOccurrence{}, false
		}
	} else if (dialect != "mysql" && dialect != "mariadb") ||
		!sqlNavMySQLBareIdentifierAllowed(token, dialect, false) {
		return sqlNavOccurrence{}, false
	}
	return sqlNavColumn(table, token, declaration, drop)
}

func sqlNavColumnList(table sqlNavOccurrence, tokens []sqlDDLToken, at int, declaration bool, dialect string) []sqlNavOccurrence {
	if at >= len(tokens) || tokens[at].text != "(" {
		return nil
	}
	result := make([]sqlNavOccurrence, 0, 8)
	depth, start := 1, at+1
	add := func(end int) {
		if start >= end || len(result) >= 64 {
			return
		}
		first := tokens[start]
		if first.kind != 'i' && first.kind != 'q' {
			return
		}
		if declaration {
			switch first.keyword() {
			case "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "KEY", "INDEX", "FULLTEXT", "SPATIAL", "VECTOR", "EXCLUDE", "LIKE", "PERIOD":
				return
			}
			if start+1 >= end || (tokens[start+1].kind != 'i' && tokens[start+1].kind != 'q') {
				return
			}
		} else if end != start+1 {
			return // an explicit one-name list only
		}
		if item, ok := sqlNavDDLColumn(table, first, declaration, false, dialect); ok {
			result = append(result, item)
		}
	}
	for i := at + 1; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				add(i)
				return result
			}
		case ",":
			if depth == 1 {
				add(i)
				start = i + 1
			}
		}
	}
	return nil // incomplete definitions must never create locations
}

func sqlNavColumns(statement []sqlDDLToken, dialect string) []sqlNavOccurrence {
	if len(statement) < 4 {
		return nil
	}
	operation := statement[0].keyword()
	var result []sqlNavOccurrence
	var table sqlNavOccurrence
	var at int
	switch operation {
	case "CREATE":
		if indexTable, at, ok := sqlNavIndexTarget(statement, dialect); ok {
			return sqlNavColumnList(indexTable, statement, at, false, dialect)
		}
		at = 1
		if at+1 < len(statement) && statement[at].keyword() == "OR" && statement[at+1].keyword() == "REPLACE" {
			at += 2
		}
		for at < len(statement) {
			switch statement[at].keyword() {
			case "TEMP", "TEMPORARY", "UNLOGGED", "GLOBAL", "LOCAL":
				at++
			default:
				goto created
			}
		}
	created:
		if at >= len(statement) || statement[at].keyword() != "TABLE" {
			return nil
		}
		at++
		if at+2 < len(statement) && statement[at].keyword() == "IF" && statement[at+1].keyword() == "NOT" && statement[at+2].keyword() == "EXISTS" {
			at += 3
		}
	case "ALTER":
		if statement[1].keyword() != "TABLE" {
			return nil
		}
		at = 2
		if at+1 < len(statement) && statement[at].keyword() == "IF" && statement[at+1].keyword() == "EXISTS" {
			at += 2
		}
		if at < len(statement) && statement[at].keyword() == "ONLY" {
			if dialect == "postgres" || dialect == "postgresql" {
				at++ // PostgreSQL inheritance modifier
			} else if !sqlNavMySQLBareOnlyName(statement, at, dialect, operation, "TABLE") {
				return nil // do not produce column sources for malformed SQL
			}
		}
	case "SELECT":
		_, columns := sqlNavSelectAnalysis(statement, dialect)
		return columns
	case "UPDATE", "DELETE":
		// Whole-statement DML proof is shared with the table emitter. The
		// single-target column supplement must not invent another SQL scope.
		return sqlNavDirectDMLColumns(statement, dialect)
	case "INSERT":
		if statement[1].keyword() != "INTO" {
			return nil
		}
		at = 2
	default:
		return nil
	}
	var ok bool
	table, at, ok = sqlNavRelationName(statement, at, dialect)
	if !ok {
		return nil
	}
	if operation == "CREATE" {
		if at >= len(statement) || statement[at].text != "(" {
			return nil
		}
		result = append(result, sqlNavColumnList(table, statement, at, true, dialect)...)
		result = append(result, sqlNavConstraintColumns(statement, dialect, table, at, false)...)
	} else if operation == "INSERT" {
		if at < len(statement) && statement[at].text == "(" {
			// The names are source-backed column uses of this exact table,
			// not definitions; INSERT ... SELECT/implicit columns stay
			// unsupported.
			result = append(result, sqlNavColumnList(table, statement, at, false, dialect)...)
		}
	} else {
		for i := at; i+1 < len(statement); i++ {
			if statement[i].keyword() == "ADD" {
				j := i + 1
				if statement[j].keyword() == "COLUMN" {
					j++
				}
				if j+2 < len(statement) && statement[j].keyword() == "IF" && statement[j+1].keyword() == "NOT" && statement[j+2].keyword() == "EXISTS" {
					j += 3
				}
				if j+1 >= len(statement) || (statement[j+1].kind != 'i' && statement[j+1].kind != 'q') {
					continue // ADD CONSTRAINT without COLUMN
				}
				switch statement[j].keyword() {
				case "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "KEY", "INDEX", "FULLTEXT", "SPATIAL", "VECTOR", "CHECK":
					continue
				}
				if item, valid := sqlNavDDLColumn(table, statement[j], true, false, dialect); valid {
					result = append(result, item)
				}
			}
			if statement[i].keyword() == "DROP" && statement[i+1].keyword() == "COLUMN" {
				j := i + 2
				if j+1 < len(statement) && statement[j].keyword() == "IF" && statement[j+1].keyword() == "EXISTS" {
					j += 2
				}
				if j < len(statement) {
					if item, valid := sqlNavDDLColumn(table, statement[j], false, true, dialect); valid {
						result = append(result, item)
					}
				}
			}
			if statement[i].keyword() == "RENAME" && statement[i+1].keyword() == "COLUMN" {
				j := i + 2
				if j+2 < len(statement) && statement[j+1].keyword() == "TO" {
					if item, valid := sqlNavDDLColumn(table, statement[j], false, true, dialect); valid {
						result = append(result, item)
					}
					if item, valid := sqlNavDDLColumn(table, statement[j+2], false, true, dialect); valid {
						result = append(result, item)
					}
				}
			}
			if statement[i].keyword() == "CHANGE" {
				j := i + 1
				if j < len(statement) && statement[j].keyword() == "COLUMN" {
					j++
				}
				if j+1 < len(statement) && (statement[j].kind == 'i' || statement[j].kind == 'q') {
					if item, valid := sqlNavDDLColumn(table, statement[j], false, true, dialect); valid {
						result = append(result, item)
					}
					if item, valid := sqlNavDDLColumn(table, statement[j+1], false, true, dialect); valid {
						result = append(result, item)
					}
				}
			}
		}
	}
	if operation == "ALTER" {
		result = append(result, sqlNavAlterIndexColumns(statement, dialect, table, at)...)
		result = append(result, sqlNavConstraintColumns(statement, dialect, table, at, true)...)
	}
	if operation == "CREATE" || operation == "ALTER" {
		for i := at; i+2 < len(statement); i++ {
			if statement[i].keyword() != "REFERENCES" {
				continue
			}
			referencedTable, end, valid := sqlNavRelationName(statement, i+1, dialect)
			if !valid || end >= len(statement) || statement[end].text != "(" {
				continue
			}
			result = append(result, sqlNavColumnList(referencedTable, statement, end, false, dialect)...)
		}
	}
	return result
}

// Deliberately not a migration replayer: a DROP/RENAME TABLE in the same
// workspace poisons column-definition certainty independently of whether its
// own statement happens to contain the searched column name.
func sqlNavTableLifecycle(events []sqlNavOccurrence, column sqlNavOccurrence) bool {
	if !column.isColumn {
		return false
	}
	for _, ev := range events {
		if !ev.isColumn && ev.drop && ev.identity == column.tableIdentity {
			return true
		}
	}
	return false
}
