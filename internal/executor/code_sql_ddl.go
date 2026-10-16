package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SQLLens provides offline, dialect-specific parsing/diagnostics, but its
// current documentSymbol response omits object DDL. This deliberately narrow
// source-only outline adds table/view and column declarations without reading
// any other file, loading project SQL configuration or connecting to a DB.
// It is not a substitute for a DB schema catalog or a complete SQL grammar.
var errSQLDDLOutputLimit = errors.New("sql_ddl_symbols_result_too_large")

type sqlDDLPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type sqlDDLRange struct {
	Start sqlDDLPosition `json:"start"`
	End   sqlDDLPosition `json:"end"`
}

type sqlDDLSymbol struct {
	Name           string         `json:"name"`
	Detail         string         `json:"detail,omitempty"`
	Kind           int            `json:"kind"`
	Range          sqlDDLRange    `json:"range"`
	SelectionRange sqlDDLRange    `json:"selectionRange"`
	Children       []sqlDDLSymbol `json:"children,omitempty"`
}

type sqlDDLToken struct {
	text       string
	kind       byte // 'i' word, 'q' quoted identifier, 'p' punctuation
	start, end sqlDDLPosition
}

func (t sqlDDLToken) keyword() string {
	if t.kind == 'i' {
		return strings.ToUpper(t.text)
	}
	return ""
}

// psql COPY FROM STDIN embeds non-SQL data. A row that happens to contain
// CREATE TABLE is data, not a declaration. Skip through the exact terminator.
func sqlDDLPGCopyStdin(tokens []sqlDDLToken) bool {
	start := len(tokens) - 2
	for start >= 0 && tokens[start].text != ";" {
		start--
	}
	statement := tokens[start+1 : len(tokens)-1]
	if len(statement) < 4 || statement[0].keyword() != "COPY" {
		return false
	}
	for i := 1; i+1 < len(statement); i++ {
		if statement[i].keyword() == "FROM" && statement[i+1].keyword() == "STDIN" {
			return true
		}
	}
	return false
}

func sqlDDLPGCopyDataEnd(source string, at int) (int, bool) {
	if at < len(source) && source[at] == '\r' {
		at++
	}
	if at >= len(source) || source[at] != '\n' {
		return 0, false
	}
	at++
	for at < len(source) {
		end := strings.IndexByte(source[at:], '\n')
		if end < 0 {
			end = len(source)
		} else {
			end += at
		}
		if strings.TrimSuffix(source[at:end], "\r") == "\\." {
			if end < len(source) {
				return end + 1, true
			}
			return end, true
		}
		if end == len(source) {
			break
		}
		at = end + 1
	}
	return 0, false
}

func sqlWordStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func sqlWordContinue(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// Lex only the small structural vocabulary needed by DDL declarations. Trivia,
// literal values, comments and PostgreSQL dollar-quoted function bodies cannot
// produce fake CREATE/ALTER symbols. Tokens and work are bounded by the existing
// codeMaxDocumentBytes input bound; outputs have an independent LSP bound.
func sqlDDLTokens(source, dialect string) ([]sqlDDLToken, bool) {
	if len(source) > codeMaxDocumentBytes {
		return nil, false
	}
	// A MySQL/MariaDB DELIMITER command replaces semicolon statement
	// boundaries (often inside stored routines). Do not parse its body as
	// independent top-level DDL. Both outlines and navigation share this
	// fail-closed guard before forward-only migration normalization.
	if (dialect == "mysql" || dialect == "mariadb") && sqlNavMySQLDelimiter.MatchString(source) {
		return nil, false
	}
	forward, ok := sqlDDLDBMateForwardSQL(source, dialect)
	if !ok {
		return nil, false
	}
	forward, ok = sqlDDLGooseForwardSQL(forward, dialect)
	if !ok {
		return nil, false
	}
	return sqlDDLTokensRaw(forward, dialect)
}

func sqlDDLTokensRaw(source, dialect string) ([]sqlDDLToken, bool) {
	if len(source) > codeMaxDocumentBytes {
		return nil, false
	}
	mysql := dialect == "mysql" || dialect == "mariadb"
	if !mysql && dialect != "postgres" && dialect != "postgresql" {
		return nil, false
	}
	tokens := make([]sqlDDLToken, 0, 128)
	i, line, character := 0, 0, 0
	position := func() sqlDDLPosition { return sqlDDLPosition{Line: line, Character: character} }
	advance := func(end int) {
		for i < end {
			r, width := utf8.DecodeRuneInString(source[i:])
			i += width
			if r == '\n' {
				line++
				character = 0
			} else if r > 0xffff {
				character += 2 // LSP default UTF-16 position encoding
			} else {
				character++
			}
		}
	}
	add := func(text string, kind byte, start sqlDDLPosition) {
		tokens = append(tokens, sqlDDLToken{text: text, kind: kind, start: start, end: position()})
	}
	for i < len(source) {
		start := position()
		begin := i
		switch {
		case source[i] == '-' && i+1 < len(source) && source[i+1] == '-',
			mysql && source[i] == '#':
			end := i
			for end < len(source) && source[end] != '\n' {
				end++
			}
			advance(end)
			continue
		case source[i] == '/' && i+1 < len(source) && source[i+1] == '*':
			depth, end := 1, i+2
			for end < len(source) && depth > 0 {
				if end+1 < len(source) && source[end:end+2] == "/*" && !mysql {
					depth++
					end += 2
				} else if end+1 < len(source) && source[end:end+2] == "*/" {
					depth--
					end += 2
				} else {
					_, width := utf8.DecodeRuneInString(source[end:])
					end += width
				}
			}
			if depth != 0 {
				return nil, false
			}
			advance(end)
			continue
		case !mysql && source[i] == '$':
			end := i + 1
			for end < len(source) && (source[end] == '_' ||
				(source[end] >= 'a' && source[end] <= 'z') ||
				(source[end] >= 'A' && source[end] <= 'Z') ||
				(source[end] >= '0' && source[end] <= '9')) {
				end++
			}
			if end < len(source) && source[end] == '$' {
				delimiter := source[i : end+1]
				rest := strings.Index(source[end+1:], delimiter)
				if rest < 0 {
					return nil, false
				}
				advance(end + 1 + rest + len(delimiter))
				continue
			}
		}
		quoteAt, pgEscapeString := i, false
		if !mysql && (source[i] == 'E' || source[i] == 'e') &&
			i+1 < len(source) && source[i+1] == '\'' {
			boundary := true
			if i > 0 {
				last, _ := utf8.DecodeLastRuneInString(source[:i])
				boundary = !sqlWordContinue(last)
			}
			if boundary {
				quoteAt, pgEscapeString = i+1, true
			}
		}
		if source[quoteAt] == '\'' || source[quoteAt] == '"' || (mysql && source[quoteAt] == '`') {
			quote := source[quoteAt]
			quotedIdentifier := (quote == '"' && !mysql) || quote == '`'
			end := quoteAt + 1
			var value strings.Builder
			closed := false
			for end < len(source) {
				if (mysql || pgEscapeString) && source[end] == '\\' && end+1 < len(source) {
					if quotedIdentifier {
						value.WriteByte(source[end+1])
					}
					end += 2
					continue
				}
				if source[end] == quote {
					if end+1 < len(source) && source[end+1] == quote {
						if quotedIdentifier {
							value.WriteByte(quote)
						}
						end += 2
						continue
					}
					end++
					closed = true
					break
				}
				r, width := utf8.DecodeRuneInString(source[end:])
				if quotedIdentifier {
					value.WriteRune(r)
				}
				end += width
			}
			if !closed {
				return nil, false
			}
			advance(end)
			if quotedIdentifier {
				add(value.String(), 'q', start)
			}
			continue
		}
		r, width := utf8.DecodeRuneInString(source[i:])
		if unicode.IsSpace(r) {
			advance(i + width)
			continue
		}
		if sqlWordStart(r) {
			end := i + width
			for end < len(source) {
				next, size := utf8.DecodeRuneInString(source[end:])
				if !sqlWordContinue(next) {
					break
				}
				end += size
			}
			advance(end)
			add(source[begin:end], 'i', start)
			continue
		}
		advance(i + width)
		add(source[begin:i], 'p', start)
		if !mysql && source[begin] == ';' && sqlDDLPGCopyStdin(tokens) {
			end, ok := sqlDDLPGCopyDataEnd(source, i)
			if !ok {
				return nil, false
			}
			advance(end)
		}
	}
	return tokens, true
}

func sqlDDLQualifiedName(tokens []sqlDDLToken, at int) (string, sqlDDLRange, int, bool) {
	if at >= len(tokens) || (tokens[at].kind != 'i' && tokens[at].kind != 'q') ||
		tokens[at].text == "" {
		return "", sqlDDLRange{}, at, false
	}
	name := tokens[at].text
	span := sqlDDLRange{Start: tokens[at].start, End: tokens[at].end}
	at++
	parts := 1
	for at+1 < len(tokens) && tokens[at].text == "." && parts < 3 &&
		(tokens[at+1].kind == 'i' || tokens[at+1].kind == 'q') &&
		tokens[at+1].text != "" {
		name += "." + tokens[at+1].text
		span.End = tokens[at+1].end
		at += 2
		parts++
	}
	return name, span, at, true
}

func sqlDDLColumns(tokens []sqlDDLToken, at int) []sqlDDLSymbol {
	if at >= len(tokens) || tokens[at].text != "(" {
		return nil
	}
	out := make([]sqlDDLSymbol, 0, 8)
	depth, start := 1, at+1
	appendColumn := func(end int) {
		if start >= end || len(out) >= 64 {
			return
		}
		first := tokens[start]
		if first.kind != 'i' && first.kind != 'q' {
			return
		}
		switch first.keyword() {
		case "CONSTRAINT", "PRIMARY", "FOREIGN", "UNIQUE", "CHECK",
			"KEY", "INDEX", "EXCLUDE", "PERIOD", "LIKE":
			return // table-level constraint, not a column definition
		}
		if start+1 >= end || tokens[start+1].text == "=" ||
			tokens[start+1].text == "," || tokens[start+1].text == ")" {
			return
		}
		span := sqlDDLRange{Start: first.start, End: first.end}
		out = append(out, sqlDDLSymbol{
			Name: first.text, Kind: 8, Detail: "DDL column",
			Range:          sqlDDLRange{Start: first.start, End: tokens[end-1].end},
			SelectionRange: span,
		})
	}
	for j := at + 1; j < len(tokens); j++ {
		switch tokens[j].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				appendColumn(j)
				return out
			}
		case ",":
			if depth == 1 {
				appendColumn(j)
				start = j + 1
			}
		}
	}
	return nil // incomplete parenthesized DDL: do not claim a parsed column schema
}

func sqlDDLStatement(tokens []sqlDDLToken) (sqlDDLSymbol, bool) {
	if len(tokens) < 3 || (tokens[0].keyword() != "CREATE" && tokens[0].keyword() != "ALTER") {
		return sqlDDLSymbol{}, false
	}
	operation := tokens[0].keyword()
	at := 1
	if operation == "CREATE" && at+1 < len(tokens) &&
		tokens[at].keyword() == "OR" && tokens[at+1].keyword() == "REPLACE" {
		at += 2
	}
	for at < len(tokens) {
		switch tokens[at].keyword() {
		case "TEMP", "TEMPORARY", "UNLOGGED", "GLOBAL", "LOCAL":
			at++
		default:
			goto objectType
		}
	}
objectType:
	if at >= len(tokens) {
		return sqlDDLSymbol{}, false
	}
	object := tokens[at].keyword()
	if object != "TABLE" && (operation != "CREATE" || object != "VIEW") {
		return sqlDDLSymbol{}, false
	}
	at++
	if operation == "CREATE" && at+2 < len(tokens) &&
		tokens[at].keyword() == "IF" && tokens[at+1].keyword() == "NOT" &&
		tokens[at+2].keyword() == "EXISTS" {
		at += 3
	}
	name, nameRange, next, ok := sqlDDLQualifiedName(tokens, at)
	if !ok {
		return sqlDDLSymbol{}, false
	}
	kind := 23 // LSP SymbolKind.Struct, closest representation of a table
	if object == "VIEW" {
		kind = 11 // LSP SymbolKind.Interface
	}
	symbol := sqlDDLSymbol{
		Name: name, Kind: kind, Detail: operation + " " + object + " (offline source)",
		Range:          sqlDDLRange{Start: tokens[0].start, End: tokens[len(tokens)-1].end},
		SelectionRange: nameRange,
	}
	if operation == "CREATE" && object == "TABLE" {
		symbol.Children = sqlDDLColumns(tokens, next)
	}
	return symbol, true
}

func sqlDDLSymbols(source, dialect string) []sqlDDLSymbol {
	tokens, ok := sqlDDLTokens(source, dialect)
	if !ok {
		return nil
	}
	var result []sqlDDLSymbol
	start, depth := 0, 0
	for i, t := range tokens {
		switch t.text {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		case ";":
			if depth == 0 {
				if symbol, valid := sqlDDLStatement(tokens[start : i+1]); valid {
					result = append(result, symbol)
				}
				start = i + 1
			}
		}
	}
	if start < len(tokens) {
		if symbol, valid := sqlDDLStatement(tokens[start:]); valid {
			result = append(result, symbol)
		}
	}
	return result
}

// Retain the language server's existing CTE/query symbols. Newer SQLLens
// releases may eventually supply DDL directly, so do not duplicate its names.
// Invalid LSP payloads fail closed rather than hiding a protocol problem.
func appendSQLDDLSymbols(result []byte, source, dialect string) ([]byte, error) {
	found := sqlDDLSymbols(source, dialect)
	if len(found) == 0 {
		return result, nil
	}
	var existing []json.RawMessage
	if err := json.Unmarshal(result, &existing); err != nil {
		return nil, errors.New("invalid SQL LSP document symbols")
	}
	// A document may legitimately CREATE and ALTER the same table repeatedly.
	// Dedupe only the same declaration position, never all symbols by name.
	key := func(name string, kind int, span sqlDDLRange) string {
		return fmt.Sprintf("%s:%d:%d:%d:%d:%d", strings.ToLower(name), kind,
			span.Start.Line, span.Start.Character, span.End.Line, span.End.Character)
	}
	seen := map[string]bool{}
	for _, raw := range existing {
		var entry struct {
			Name           string      `json:"name"`
			Kind           int         `json:"kind"`
			SelectionRange sqlDDLRange `json:"selectionRange"`
		}
		if json.Unmarshal(raw, &entry) == nil && entry.Name != "" {
			seen[key(entry.Name, entry.Kind, entry.SelectionRange)] = true
		}
	}
	for _, symbol := range found {
		identity := key(symbol.Name, symbol.Kind, symbol.SelectionRange)
		if seen[identity] {
			continue
		}
		raw, err := json.Marshal(symbol)
		if err != nil {
			return nil, err
		}
		existing = append(existing, raw)
		seen[identity] = true
	}
	encoded, err := json.Marshal(existing)
	if err != nil {
		return nil, err
	}
	if len(encoded) > lspMaxFrameBytes {
		return nil, errSQLDDLOutputLimit
	}
	return encoded, nil
}
