package executor

// A CREATE/INSERT-contained SELECT must prove its projection *and* the
// subsequent clauses before lending its parent a physical declaration.
// sqlNavSelectAnalysis already validates FROM/JOIN sources and the complete
// ON predicate; this complementary bounded check closes prefix-only
// projection, WHERE, ORDER and LIMIT acceptance. Complex but valid SQL is
// conservatively not indexed as physical source-backed navigation.
func sqlNavNestedBoundedBodyProof(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) < 4 || tokens[0].keyword() != "SELECT" {
		return false
	}
	depth, from := -1, -1
	depth = 0
	for i := 1; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return false
			}
		}
		if depth == 0 && tokens[i].keyword() == "FROM" &&
			(i == 0 || tokens[i-1].text != ".") {
			if from >= 0 {
				return false
			}
			from = i
		}
	}
	if depth != 0 || from <= 1 {
		return false
	}
	items, ok := sqlNavProofSegments(tokens, 1, from)
	if !ok {
		return false
	}
	for _, item := range items {
		if !sqlNavNestedProjectionItemProven(item, dialect) {
			return false
		}
	}

	return sqlNavSelectTailProven(tokens, from, dialect)
}

// The same clause proof is shared by standalone SELECT navigation and
// nested CREATE/INSERT SELECT. Rich valid SQL with unsupported clauses is
// conservatively omitted, never treated as evidence from a parsed prefix.
func sqlNavSelectTailProven(tokens []sqlDDLToken, from int, dialect string) bool {
	if from < 1 || from >= len(tokens) {
		return false
	}
	depth, at := 0, len(tokens)
	for i := from + 1; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return false
			}
		}
		if depth != 0 || i > 0 && tokens[i-1].text == "." {
			continue
		}
		switch tokens[i].keyword() {
		case "WHERE", "GROUP", "HAVING", "ORDER", "LIMIT", "OFFSET", "FETCH", "WINDOW", "FOR":
			at = i
			goto clauses
		}
	}
clauses:
	if at == len(tokens) {
		return true
	}
	lastRank := 0
	for at < len(tokens) {
		rank := sqlNavNestedClauseRank(tokens[at].keyword())
		if rank == 0 || rank <= lastRank {
			return false
		}
		end := len(tokens)
		depth = 0
		for i := at + 1; i < len(tokens); i++ {
			switch tokens[i].text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return false
				}
			}
			if depth == 0 && tokens[i-1].text != "." &&
				sqlNavNestedClauseRank(tokens[i].keyword()) != 0 {
				end = i
				break
			}
		}
		if !sqlNavNestedClauseProven(tokens[at:end], dialect) {
			return false
		}
		lastRank = rank
		at = end
	}
	return true
}

func sqlNavNestedClauseRank(keyword string) int {
	switch keyword {
	case "WHERE":
		return 1
	case "GROUP":
		return 2
	case "HAVING":
		return 3
	case "ORDER":
		return 4
	case "LIMIT":
		return 5
	case "OFFSET":
		return 6
	}
	return 0 // FETCH, WINDOW and FOR are unsupported bounded suffixes
}

func sqlNavNestedProjectionItemProven(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, tok := range tokens {
		if tok.literalBefore {
			return false
		}
	}
	end := len(tokens)
	// An explicit output alias is allowed; implicit aliases are omitted.
	for i := 0; i < end; i++ {
		if tokens[i].keyword() == "AS" && (i == 0 || tokens[i-1].text != ".") {
			if i+2 != end || !sqlNavSelectAliasAllowed(tokens[i+1], dialect) {
				return false
			}
			end = i
			break
		}
	}
	if end == 0 {
		return false
	}
	value := tokens[:end]
	if len(value) == 1 && value[0].text == "*" {
		return true
	}
	if len(value) == 3 && value[1].text == "." && value[2].text == "*" {
		return sqlNavQualifiedColumnHeadAllowed(value[0], dialect)
	}
	if len(value) == 1 && (value[0].keyword() == "TRUE" || value[0].keyword() == "FALSE" ||
		value[0].keyword() == "NULL") {
		return true
	}
	if sqlNavProofNumber(value, dialect) {
		return true
	}
	if (value[0].kind == 'i' || value[0].kind == 'q') &&
		sqlNavQualifiedColumnHeadAllowed(value[0], dialect) {
		_, at, ok := sqlNavName(value, 0)
		if ok && at == len(value) {
			return true
		}
		// Single simple SQL function expression: count(*), max(a.id)
		// and coalesce-like expressions with one bare argument only.
		if len(value) >= 3 && value[1].text == "(" {
			next, ok := sqlNavProofParenEnd(value, 1, len(value))
			if !ok || next != len(value) {
				return false
			}
			arg := value[2 : len(value)-1]
			if len(arg) == 0 || len(arg) == 1 && arg[0].text == "*" {
				return true
			}
			if sqlNavProofNumber(arg, dialect) {
				return true
			}
			if len(arg) > 0 && sqlNavQualifiedColumnHeadAllowed(arg[0], dialect) {
				_, count, ok := sqlNavName(arg, 0)
				return ok && count == len(arg)
			}
		}
	}
	return false
}

func sqlNavProofNumber(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) == 0 {
		return false
	}
	p := sqlNavOnParser{tokens: tokens, dialect: dialect}
	return p.number() && p.at == len(tokens)
}

func sqlNavNestedClauseProven(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) < 2 {
		return false
	}
	for _, tok := range tokens {
		if tok.literalBefore {
			return false
		}
	}
	switch tokens[0].keyword() {
	case "WHERE", "HAVING":
		return sqlNavSelectOnPredicateComplete(tokens[1:], dialect)
	case "LIMIT", "OFFSET":
		return sqlNavProofNumber(tokens[1:], dialect)
	case "GROUP", "ORDER":
		if len(tokens) < 3 || tokens[1].keyword() != "BY" {
			return false
		}
		items, ok := sqlNavProofSegments(tokens, 2, len(tokens))
		if !ok {
			return false
		}
		for _, item := range items {
			if tokens[0].keyword() == "ORDER" && len(item) > 0 &&
				(item[len(item)-1].keyword() == "ASC" || item[len(item)-1].keyword() == "DESC") {
				item = item[:len(item)-1]
			}
			if len(item) == 0 {
				return false
			}
			if sqlNavProofNumber(item, dialect) {
				continue
			}
			if !sqlNavQualifiedColumnHeadAllowed(item[0], dialect) {
				return false
			}
			_, end, ok := sqlNavName(item, 0)
			if !ok || end != len(item) {
				return false
			}
		}
		return true
	}
	return false
}
