package executor

// The SQL source supplement is not a general-purpose expression parser. An
// explicit JOIN must prove a complete ON predicate before we can bind any
// qualified column to a source-backed migration declaration. Only a small
// expression grammar is supported; everything else fails closed. The scanner
// deliberately discards SQL string literal contents, so literal-dependent
// predicates that cannot be proved from its tokens are also unsupported.
// No SQL is executed and this code never contacts a database.
//
// predicate := term {(AND | OR) term}
// term      := [NOT] ( '(' predicate ')' | scalar comparison scalar
//
//	| scalar IS [NOT] (NULL | TRUE | FALSE)
//	| TRUE | FALSE )
//
// scalar    := bounded SQL identifier[.identifier...] | numeric literal
//
//	| TRUE | FALSE | NULL
//
// comparison: =, !=, <>, <, <=, >, >=, LIKE, ILIKE
//
// Parentheses and NOT recursion are explicitly bounded to avoid adversarial
// input consuming unbounded stack or time. AND/OR precedence does not affect
// identity binding, only predicate syntax validity is checked here.
type sqlNavOnParser struct {
	tokens  []sqlDDLToken
	dialect string
	at      int
}

const (
	sqlNavOnMaxTokens = 512
	sqlNavOnMaxDepth  = 16
)

func sqlNavSelectOnPredicateComplete(tokens []sqlDDLToken, dialect string) bool {
	if len(tokens) == 0 || len(tokens) > sqlNavOnMaxTokens {
		return false
	}
	p := sqlNavOnParser{tokens: tokens, dialect: dialect}
	return p.predicate(0) && p.at == len(tokens)
}

func (p *sqlNavOnParser) keyword(word string) bool {
	return p.at < len(p.tokens) && p.tokens[p.at].keyword() == word
}

func (p *sqlNavOnParser) take(word string) bool {
	if !p.keyword(word) {
		return false
	}
	p.at++
	return true
}

func (p *sqlNavOnParser) punctuation(symbol string) bool {
	if p.at >= len(p.tokens) || p.tokens[p.at].kind != 'p' || p.tokens[p.at].text != symbol {
		return false
	}
	p.at++
	return true
}

func (p *sqlNavOnParser) predicate(depth int) bool {
	if depth >= sqlNavOnMaxDepth || !p.term(depth) {
		return false
	}
	for p.keyword("AND") || p.keyword("OR") {
		p.at++
		if !p.term(depth) {
			return false
		}
	}
	return true
}

func (p *sqlNavOnParser) term(depth int) bool {
	if depth >= sqlNavOnMaxDepth {
		return false
	}
	if p.take("NOT") {
		return p.term(depth + 1)
	}
	if p.punctuation("(") {
		return p.predicate(depth+1) && p.punctuation(")")
	}
	if p.take("TRUE") || p.take("FALSE") {
		return true
	}
	if !p.scalar() {
		return false
	}
	if p.take("IS") {
		p.take("NOT")
		return p.take("NULL") || p.take("TRUE") || p.take("FALSE")
	}
	if p.take("LIKE") || (p.dialect == "postgres" || p.dialect == "postgresql") && p.take("ILIKE") {
		return p.scalar()
	}
	if !p.comparator() {
		return false
	}
	return p.scalar()
}

func (p *sqlNavOnParser) comparator() bool {
	if p.punctuation("=") {
		return true
	}
	if p.punctuation("<") {
		if !p.punctuation(">") {
			p.punctuation("=")
		}
		return true
	}
	if p.punctuation(">") {
		p.punctuation("=")
		return true
	}
	if p.punctuation("!") {
		return p.punctuation("=")
	}
	return false
}

func sqlNavOnDigit(token sqlDDLToken) bool {
	return token.kind == 'p' && len(token.text) == 1 && token.text[0] >= '0' && token.text[0] <= '9'
}

func sqlNavOnAdjacent(a, b sqlDDLToken) bool {
	return a.end == b.start
}

func (p *sqlNavOnParser) number() bool {
	if p.at >= len(p.tokens) {
		return false
	}
	if (p.tokens[p.at].text == "-" || p.tokens[p.at].text == "+") &&
		p.at+1 < len(p.tokens) && sqlNavOnDigit(p.tokens[p.at+1]) &&
		sqlNavOnAdjacent(p.tokens[p.at], p.tokens[p.at+1]) {
		p.at++
	}
	if !sqlNavOnDigit(p.tokens[p.at]) {
		return false
	}
	last := p.tokens[p.at]
	p.at++
	for p.at < len(p.tokens) && sqlNavOnDigit(p.tokens[p.at]) && sqlNavOnAdjacent(last, p.tokens[p.at]) {
		last = p.tokens[p.at]
		p.at++
	}
	// Decimal numeric literals are permitted; a dot without digits is not.
	if p.at+1 < len(p.tokens) && p.tokens[p.at].text == "." &&
		sqlNavOnAdjacent(last, p.tokens[p.at]) && sqlNavOnDigit(p.tokens[p.at+1]) &&
		sqlNavOnAdjacent(p.tokens[p.at], p.tokens[p.at+1]) {
		last = p.tokens[p.at+1]
		p.at += 2
		for p.at < len(p.tokens) && sqlNavOnDigit(p.tokens[p.at]) && sqlNavOnAdjacent(last, p.tokens[p.at]) {
			last = p.tokens[p.at]
			p.at++
		}
	}
	return true
}

func sqlNavOnNameAllowed(token sqlDDLToken) bool {
	if token.kind == 'q' {
		return true
	}
	if token.kind != 'i' {
		return false
	}
	// A grammar delimiter or SQL clause cannot be an unquoted operand.
	// SQL names use the existing sqlNavName length, quoting and syntax guards.
	switch token.keyword() {
	case "AND", "OR", "ON", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "OUTER", "FROM", "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "OFFSET", "FETCH", "WINDOW", "FOR", "SELECT", "UNION", "EXCEPT", "INTERSECT", "WITH", "AS", "NOT", "IS", "LIKE", "ILIKE", "IN", "BETWEEN", "CASE", "WHEN", "THEN", "ELSE", "END", "USING", "NATURAL", "LATERAL", "CROSS", "CAST":
		return false
	}
	return true
}

func (p *sqlNavOnParser) scalar() bool {
	if p.at >= len(p.tokens) {
		return false
	}
	if p.tokens[p.at].kind == 'p' {
		return p.number()
	}
	if !sqlNavOnNameAllowed(p.tokens[p.at]) {
		return false
	}
	_, next, ok := sqlNavName(p.tokens, p.at)
	if !ok || next <= p.at {
		return false
	}
	for i := p.at; i < next; i += 2 {
		if !sqlNavOnNameAllowed(p.tokens[i]) {
			return false
		}
	}
	p.at = next
	return true
}
