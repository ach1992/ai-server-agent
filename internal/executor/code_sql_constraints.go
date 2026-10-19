package executor

// A constrained, offline DDL source grammar. PRIMARY/UNIQUE/FOREIGN KEY
// declarations have explicit owning-table column lists; these are *uses* of
// the table's column declarations, not new column definitions or a database
// constraint catalog. Expression, prefix-length, partial and opclass key
// members cannot masquerade as simple table-column references.
func sqlNavConstraintColumns(statement []sqlDDLToken, dialect string, table sqlNavOccurrence, at int, alter bool) []sqlNavOccurrence {
	if at >= len(statement) || table.identity == "" {
		return nil
	}
	if !alter && statement[at].text != "(" {
		return nil
	}
	depth, wanted := 0, 1
	if alter {
		wanted = 0
	}
	start := at
	if !alter {
		start++
	}
	var result []sqlNavOccurrence
	process := func(from, until int) {
		if until <= from || len(result) >= 64 {
			return
		}
		i := from
		if alter {
			if statement[i].keyword() != "ADD" {
				return
			}
			i++
			if i >= until {
				return
			}
		}
		hasNamedConstraint := statement[i].keyword() == "CONSTRAINT"
		if hasNamedConstraint {
			i++
			if i >= until || (statement[i].kind != 'i' && statement[i].kind != 'q') {
				return
			}
			i++ // explicit constraint name, never a column
		}
		if i >= until {
			return
		}
		switch statement[i].keyword() {
		case "KEY", "INDEX", "FULLTEXT", "SPATIAL":
			if dialect != "mysql" && dialect != "mariadb" || alter {
				// ALTER ADD INDEX belongs to the index-specific parser.
				return
			}
			if statement[i].keyword() == "FULLTEXT" || statement[i].keyword() == "SPATIAL" {
				i++
				if i >= until || (statement[i].keyword() != "INDEX" && statement[i].keyword() != "KEY") {
					return
				}
			}
			i++
			if i < until && statement[i].text != "(" && (statement[i].kind == 'i' || statement[i].kind == 'q') {
				i++
			}
		case "PRIMARY", "FOREIGN":
			kind := statement[i].keyword()
			i++
			if i >= until || statement[i].keyword() != "KEY" {
				return
			}
			i++
			// MySQL permits a named FOREIGN KEY index, unlike PRIMARY KEY.
			if kind == "FOREIGN" && (dialect == "mysql" || dialect == "mariadb") &&
				i < until && statement[i].text != "(" && (statement[i].kind == 'i' || statement[i].kind == 'q') {
				i++
			}
		case "UNIQUE":
			i++
			if i < until && (statement[i].keyword() == "KEY" || statement[i].keyword() == "INDEX") {
				if dialect != "mysql" && dialect != "mariadb" {
					return
				}
				// ADD UNIQUE KEY is already covered by the separate index
				// grammar. Do not return each member twice. Named
				// ADD CONSTRAINT UNIQUE KEY stays a constraint here.
				if alter && !hasNamedConstraint {
					return
				}
				i++
			}
			if i < until && statement[i].text != "(" && (statement[i].kind == 'i' || statement[i].kind == 'q') {
				if dialect != "mysql" && dialect != "mariadb" {
					return
				}
				i++
			}
		default:
			return
		}
		if i >= until || statement[i].text != "(" {
			return
		}
		// The list parser requires one plain identifier per item. Never
		// synthesize column references from expressions/lengths/sort order.
		list := sqlNavColumnList(table, statement[:until], i, false, dialect)
		if len(list) > 0 {
			result = append(result, list...)
		}
	}
	segmentStart := start
	for i := at; i < len(statement); i++ {
		switch statement[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return nil
			}
			if !alter && depth == 0 {
				process(segmentStart, i)
				return result
			}
		case ",":
			if depth == wanted {
				process(segmentStart, i)
				segmentStart = i + 1
			}
		}
	}
	if alter && depth == 0 {
		process(segmentStart, len(statement))
		return result
	}
	return nil
}
