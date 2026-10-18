package executor

// Check the bare identifier starts of supported simple constraint-key lists.
// Complex expressions and prefix-length forms are not treated as column
// occurrences by the bounded navigation parser. A clearly forbidden bare
// column token in a supported key-list position invalidates the whole CREATE.
func sqlNavCreateKeyListValid(tokens []sqlDDLToken, open, end int, dialect string) bool {
	if open >= end || tokens[open].text != "(" {
		return false
	}
	start, depth := open+1, 1
	check := func(last int) bool {
		if start >= last {
			return false
		}
		first := tokens[start]
		if first.kind == 'i' || first.kind == 'q' {
			return sqlNavQualifiedColumnHeadAllowed(first, dialect) && !first.literalBefore
		}
		return first.text == "(" // complex supported expression: no physical column binding
	}
	for i := open + 1; i < end; i++ {
		switch tokens[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return check(i)
			}
			if depth < 0 {
				return false
			}
		case ",":
			if depth == 1 {
				if !check(i) {
					return false
				}
				start = i + 1
			}
		}
	}
	return false
}

// Multiple ALTER TABLE actions form one SQL statement. A bad column
// identifier in one supported action must not publish earlier ADD/DROP/
// RENAME column occurrences as if the migration executed partially.
func sqlNavAlterColumnActionsValid(tokens []sqlDDLToken, start int, dialect string) bool {
	depth := 0
	for i := start; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			depth++
			continue
		case ")":
			depth--
			if depth < 0 {
				return false
			}
			continue
		}
		if depth != 0 {
			continue
		}
		switch tokens[i].keyword() {
		case "ADD":
			j := i + 1
			if j < len(tokens) && tokens[j].keyword() == "COLUMN" {
				j++
			}
			if j+2 < len(tokens) && tokens[j].keyword() == "IF" &&
				tokens[j+1].keyword() == "NOT" && tokens[j+2].keyword() == "EXISTS" {
				j += 3
			}
			if j >= len(tokens) {
				return false
			}
			switch tokens[j].keyword() {
			case "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "KEY", "INDEX",
				"FULLTEXT", "SPATIAL", "VECTOR", "EXCLUDE", "PARTITION":
				continue
			}
			if !sqlNavQualifiedColumnHeadAllowed(tokens[j], dialect) ||
				tokens[j].literalBefore || j+1 >= len(tokens) ||
				(tokens[j+1].kind != 'i' && tokens[j+1].kind != 'q') {
				return false
			}
		case "DROP":
			j := i + 1
			if j < len(tokens) && tokens[j].keyword() == "COLUMN" {
				j++
				if j+1 < len(tokens) && tokens[j].keyword() == "IF" &&
					tokens[j+1].keyword() == "EXISTS" {
					j += 2
				}
				if j >= len(tokens) || tokens[j].literalBefore ||
					!sqlNavQualifiedColumnHeadAllowed(tokens[j], dialect) {
					return false
				}
			}
		case "RENAME":
			j := i + 1
			if j < len(tokens) && tokens[j].keyword() == "COLUMN" {
				j++
				if j+2 >= len(tokens) || tokens[j+1].keyword() != "TO" ||
					!sqlNavQualifiedColumnHeadAllowed(tokens[j], dialect) ||
					!sqlNavQualifiedColumnHeadAllowed(tokens[j+2], dialect) {
					return false
				}
			}
		case "CHANGE", "MODIFY":
			if dialect != "mysql" && dialect != "mariadb" {
				continue
			}
			j := i + 1
			if j < len(tokens) && tokens[j].keyword() == "COLUMN" {
				j++
			}
			if j >= len(tokens) || !sqlNavQualifiedColumnHeadAllowed(tokens[j], dialect) {
				return false
			}
			if tokens[i].keyword() == "CHANGE" &&
				(j+1 >= len(tokens) || !sqlNavQualifiedColumnHeadAllowed(tokens[j+1], dialect)) {
				return false
			}
		}
	}
	return depth == 0
}

// CREATE TABLE is one declaration statement, not a bag of independently
// admissible column tokens. Before emitting its table, column, foreign-key
// or constraint occurrences, validate the *whole supported column list*.
// A malformed field must never leave earlier valid-looking definitions.
// This is intentionally a bounded syntax gate, not a SQL engine.
func sqlNavCreateTableColumnsValid(tokens []sqlDDLToken, at int, dialect string) bool {
	if at >= len(tokens) || tokens[at].text != "(" {
		return false
	}
	start, depth, entries := at+1, 1, 0
	for i := at + 1; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return false
			}
			if depth == 0 {
				return entries < 64 && sqlNavCreateTableEntryValid(tokens, start, i, dialect)
			}
		case ",":
			if depth == 1 {
				if entries >= 64 || !sqlNavCreateTableEntryValid(tokens, start, i, dialect) {
					return false
				}
				entries++
				start = i + 1
			}
		}
	}
	return false // incomplete column list cannot make a physical declaration
}

func sqlNavCreateTableEntryValid(tokens []sqlDDLToken, start, end int, dialect string) bool {
	if start >= end || tokens[start].literalBefore {
		return false
	}
	// A bad REFERENCES target makes the whole CREATE invalid even if
	// its preceding column looked well formed. Referenced-column lists
	// are optional in PostgreSQL, so only the physical relation identity
	// is mandatory at this gate.
	for i := start; i+1 < end; i++ {
		if tokens[i].keyword() != "REFERENCES" ||
			(i > start && tokens[i-1].text == ".") {
			continue
		}
		_, next, valid := sqlNavRelationName(tokens[:end], i+1, dialect)
		if !valid {
			return false
		}
		if next < end && tokens[next].text == "(" &&
			!sqlNavCreateKeyListValid(tokens, next, end, dialect) {
			return false
		}
	}
	// PRIMARY/FOREIGN KEY and UNIQUE column lists must not contain a
	// forbidden bare ColId even if other columns in CREATE are valid.
	for i := start; i+1 < end; i++ {
		word := tokens[i].keyword()
		if word == "PRIMARY" || word == "FOREIGN" {
			if tokens[i+1].keyword() == "KEY" && i+2 < end && tokens[i+2].text == "(" &&
				!sqlNavCreateKeyListValid(tokens, i+2, end, dialect) {
				return false
			}
		} else if word == "UNIQUE" {
			j := i + 1
			if j < end && (tokens[j].keyword() == "KEY" || tokens[j].keyword() == "INDEX") {
				j++
			}
			if j < end && (tokens[j].kind == 'i' || tokens[j].kind == 'q') && tokens[j].text != "(" {
				j++
			}
			if j < end && tokens[j].text == "(" &&
				!sqlNavCreateKeyListValid(tokens, j, end, dialect) {
				return false
			}
		}
	}
	first := tokens[start]
	switch first.keyword() {
	case "CONSTRAINT":
		if start+2 >= end || !sqlNavQualifiedColumnHeadAllowed(tokens[start+1], dialect) ||
			tokens[start+2].literalBefore {
			return false
		}
		switch tokens[start+2].keyword() {
		case "PRIMARY", "UNIQUE", "FOREIGN", "CHECK", "EXCLUDE":
			return true
		}
		return false
	case "PRIMARY", "FOREIGN":
		return start+2 < end && tokens[start+1].keyword() == "KEY"
	case "UNIQUE":
		return start+1 < end
	case "CHECK":
		return start+1 < end && tokens[start+1].text == "("
	case "KEY", "INDEX", "FULLTEXT", "SPATIAL":
		return (dialect == "mysql" || dialect == "mariadb") && start+1 < end
	case "LIKE":
		if dialect != "postgres" && dialect != "postgresql" {
			return false
		}
		_, next, valid := sqlNavRelationName(tokens[:end], start+1, dialect)
		return valid && next <= end
	case "PERIOD":
		return (dialect == "mysql" || dialect == "mariadb") && start+1 < end
	}
	// The initial token of a columnDef is a bare ColId. A quoted name
	// remains legal, as do contextually nonreserved dialect keywords.
	if !sqlNavQualifiedColumnHeadAllowed(first, dialect) ||
		start+1 >= end || tokens[start+1].literalBefore {
		return false
	}
	// A supported declaration has an explicit datatype after the name.
	// Complex type details remain outside this shallow grammar guard.
	return tokens[start+1].kind == 'i' || tokens[start+1].kind == 'q'
}
