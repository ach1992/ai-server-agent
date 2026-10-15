package executor

import (
	"regexp"
	"strings"
)

// Goose runs only the Up section when migrating forward. Both sections are
// ordinary SQL after comment-tokenization, so parsing the raw file would
// falsely promote rollback DDL to a current-schema definition. This bounded
// normalization retains original source offsets, but never executes SQL or
// evaluates runner directives. Complex/dynamic annotations fail closed.
const sqlGooseAnnotationLimit = 32

var (
	sqlGoosePossible  = regexp.MustCompile(`(?i)^[ \t]*--[ \t]+\+goose(?:[ \t\r\n]|$)`)
	sqlGooseDirective = regexp.MustCompile(`(?i)^--[ \t]+\+goose[ \t]+([a-z]+(?:[ \t]+[a-z]+)?)[ \t]*\r?\n?$`)
)

func sqlDDLGooseForwardSQL(source, dialect string) (string, bool) {
	if !strings.Contains(strings.ToLower(source), "+goose") {
		return source, true
	}
	var out strings.Builder
	out.Grow(len(source))
	seenUp, seenDown, count, offset := false, false, 0, 0
	for _, line := range strings.SplitAfter(source, "\n") {
		directive := false
		if sqlGoosePossible.MatchString(line) {
			match := sqlGooseDirective.FindStringSubmatch(line)
			if match == nil {
				return "", false
			}
			count++
			if count > sqlGooseAnnotationLimit {
				return "", false
			}
			// A marker inside a quoted/dollar-quoted literal, COPY data or
			// block comment is not a migration directive.
			before, valid := sqlDDLTokensRaw(source[:offset], dialect)
			if !valid {
				return "", false
			}
			switch strings.ToUpper(strings.Join(strings.Fields(match[1]), " ")) {
			case "UP":
				if seenUp || seenDown || len(before) != 0 {
					return "", false
				}
				seenUp = true
			case "DOWN":
				if !seenUp || seenDown || len(before) == 0 || before[len(before)-1].text != ";" {
					return "", false
				}
				seenDown = true
			case "NO TRANSACTION":
				if seenUp || seenDown || len(before) != 0 {
					return "", false
				}
			default:
				// StatementBegin/End and ENVSUB can change SQL grouping or
				// dynamically substitute identifiers: no inferred schema.
				return "", false
			}
			directive = true
		}
		if seenUp && !seenDown && !directive {
			out.WriteString(line)
		} else {
			// Keep exact line/UTF-16 offsets for the forward SQL.
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
	return out.String(), seenUp
}
