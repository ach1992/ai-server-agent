package executor

// SQL index navigation accepts conservative, dialect-specific CREATE INDEX
// forms: PostgreSQL CONCURRENTLY/ONLY/USING-after-table, MySQL index types
// before ON, MariaDB optional OR REPLACE/IF NOT EXISTS. It never guesses from
// expression indexes, opclasses, predicates, INCLUDE, or arbitrary DDL.
// Indexed table/column references belong to the existing source graph; an
// index does not declare a table or a column.
func sqlNavIndexMethod(method, dialect string, spatial bool) bool {
	switch dialect {
	case "postgres":
		switch method {
		case "BTREE", "HASH", "GIST", "GIN", "SPGIST", "BRIN":
			return true
		}
	case "mysql", "mariadb":
		return method == "BTREE" || method == "HASH" || (method == "RTREE" && spatial)
	}
	return false
}

func sqlNavIndexTarget(statement []sqlDDLToken, dialect string) (sqlNavOccurrence, int, bool) {
	if len(statement) < 6 || statement[0].keyword() != "CREATE" {
		return sqlNavOccurrence{}, 0, false
	}
	i := 1
	if i+1 < len(statement) && statement[i].keyword() == "OR" && statement[i+1].keyword() == "REPLACE" {
		if dialect != "mariadb" {
			return sqlNavOccurrence{}, 0, false
		}
		i += 2
	}
	if i >= len(statement) {
		return sqlNavOccurrence{}, 0, false
	}
	spatial := statement[i].keyword() == "SPATIAL"
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
	ifNotExists := i+2 < len(statement) && statement[i].keyword() == "IF" &&
		statement[i+1].keyword() == "NOT" && statement[i+2].keyword() == "EXISTS"
	if ifNotExists {
		if dialect == "mysql" {
			return sqlNavOccurrence{}, 0, false
		}
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
	} else if dialect != "postgres" || ifNotExists {
		// Only PostgreSQL permits an unnamed CREATE INDEX, and its IF NOT
		// EXISTS clause requires an explicit index name.
		return sqlNavOccurrence{}, 0, false
	}
	if i+1 < len(statement) && statement[i].keyword() == "USING" {
		if dialect == "postgres" || !sqlNavIndexMethod(statement[i+1].keyword(), dialect, spatial) {
			return sqlNavOccurrence{}, 0, false
		}
		i += 2 // MySQL and MariaDB index type is before ON.
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
	table, next, ok := sqlNavRelationName(statement, i, dialect)
	if !ok {
		return sqlNavOccurrence{}, 0, false
	}
	i = next
	if i+1 < len(statement) && statement[i].keyword() == "USING" {
		if dialect != "postgres" || !sqlNavIndexMethod(statement[i+1].keyword(), dialect, spatial) {
			return sqlNavOccurrence{}, 0, false
		}
		i += 2
	}
	if i >= len(statement) || statement[i].text != "(" ||
		!sqlNavIndexWholeProof(statement, i, dialect) {
		// Prefix-only recognition is never sufficient. In particular an
		// invalid suffix after the closing key list cannot publish the
		// table target or earlier valid-looking key references.
		return sqlNavOccurrence{}, 0, false
	}
	return table, i, true
}

// ALTER TABLE ADD [UNIQUE|FULLTEXT|SPATIAL] INDEX|KEY is a real MySQL and
// MariaDB migration form, distinct from PostgreSQL CREATE INDEX. Only simple
// indexed columns are source references; table/column declarations and
// unsupported expressions are never synthesized from index details.
func sqlNavAlterIndexColumns(statement []sqlDDLToken, dialect string, table sqlNavOccurrence, start int) []sqlNavOccurrence {
	if dialect != "mysql" && dialect != "mariadb" {
		return nil
	}
	var result []sqlNavOccurrence
	depth := 0
	for i := start; i < len(statement); i++ {
		switch statement[i].text {
		case "(":
			depth++
		case ")":
			depth--
		}
		if depth != 0 || statement[i].keyword() != "ADD" {
			continue
		}
		j := i + 1
		if j >= len(statement) {
			continue
		}
		if statement[j].keyword() == "UNIQUE" || statement[j].keyword() == "FULLTEXT" || statement[j].keyword() == "SPATIAL" {
			j++
		}
		if j >= len(statement) || (statement[j].keyword() != "INDEX" && statement[j].keyword() != "KEY") {
			continue
		}
		j++
		if j+2 < len(statement) && statement[j].keyword() == "IF" &&
			statement[j+1].keyword() == "NOT" && statement[j+2].keyword() == "EXISTS" {
			if dialect != "mariadb" {
				continue
			}
			j += 3
		}
		if j >= len(statement) {
			continue
		}
		if statement[j].text != "(" {
			_, next, ok := sqlNavName(statement, j)
			if !ok {
				continue
			}
			j = next
		}
		if j+1 < len(statement) && statement[j].keyword() == "USING" {
			if !sqlNavIndexMethod(statement[j+1].keyword(), dialect, false) {
				continue
			}
			j += 2
		}
		if j < len(statement) && statement[j].text == "(" {
			result = append(result, sqlNavColumnList(table, statement, j, false, dialect)...)
		}
	}
	return result
}
