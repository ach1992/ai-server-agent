package executor

import (
	"regexp"
	"strings"
)

// dbmate stores forward and rollback SQL in the same migration file. An
// offline source graph may only use the Up statements: treating Down DDL as
// forward history can fabricate definitions or invalidate genuine ones.
//
// This bounded filter recognizes dbmate's documented up/down markers and
// optional transaction:true/false, including repeated Up/Down pairs. It does
// not execute SQL or read runner configuration. Unrecognized annotations,
// mixed runner formats and ambiguous statement boundaries fail closed.
// Masking non-Up bytes (but keeping CR/LF) preserves original LSP positions.
const sqlDBMateAnnotationLimit = 64

var (
	sqlDBMatePossible  = regexp.MustCompile("(?i)^[ \\t]*--[ \\t]+migrate:")
	sqlDBMateDirective = regexp.MustCompile("^--[ \\t]+migrate:(up|down)(?:[ \\t]+transaction:(true|false))?[ \\t]*\\r?\\n?$")
)

func sqlDDLDBMateForwardSQL(source, dialect string) (string, bool) {
	if !strings.Contains(strings.ToLower(source), "migrate:") {
		return source, true
	}
	var out strings.Builder
	out.Grow(len(source))
	seenUp, seenDown, inUp, count, offset := false, false, false, 0, 0
	for _, line := range strings.SplitAfter(source, "\n") {
		directive := false
		if sqlGoosePossible.MatchString(line) {
			// Do not combine unrelated migration-runner models.
			return "", false
		}
		if sqlDBMatePossible.MatchString(line) {
			match := sqlDBMateDirective.FindStringSubmatch(line)
			if match == nil {
				return "", false
			}
			count++
			if count > sqlDBMateAnnotationLimit {
				return "", false
			}
			// A marker within a dollar-quoted/regular string, block comment
			// or PostgreSQL COPY data is not a real dbmate directive.
			before, valid := sqlDDLTokensRaw(source[:offset], dialect)
			if !valid {
				return "", false
			}
			if !seenUp {
				if len(before) != 0 || match[1] != "up" {
					return "", false
				}
			} else if len(before) > 0 && before[len(before)-1].text != ";" {
				// Never connect fragments across the implicit section cut.
				return "", false
			}
			switch match[1] {
			case "up":
				if inUp {
					return "", false // consecutive Up blocks are ambiguous
				}
				inUp, seenUp = true, true
			case "down":
				if !inUp {
					return "", false
				}
				inUp, seenDown = false, true
			}
			directive = true
		}
		if inUp && !directive {
			out.WriteString(line)
		} else {
			for i := 0; i < len(line); i++ {
				if line[i] == '\n' || line[i] == '\r' {
					out.WriteByte(line[i])
				} else {
					out.WriteByte(' ')
				}
			}
		}
		offset += len(line)
	}
	if count == 0 {
		return source, true
	}
	// dbmate requires paired Up/Down sections. An unterminated final Up or
	// Down-only file must not be used as a forward migration definition.
	if !seenUp || !seenDown || inUp {
		return "", false
	}
	return out.String(), true
}
