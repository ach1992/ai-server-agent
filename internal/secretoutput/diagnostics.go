package secretoutput

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// RedactDiagnosticText removes recognizable credential assignments from
// otherwise useful native diagnostics. This is defense-in-depth, not a
// substitute for the exact-value streaming redactor when the secret is known.
var credentialAssignment = regexp.MustCompile("(?i)(\\b(?:authorization|password|passwd|token|secret|api[_-]?key)\\s*[:=]\\s*)(?:\"(?:\\\\.|[^\"\\\\])*\"|'(?:\\\\.|[^'\\\\])*'|[^\\s,;]+)")
var bearerValue = regexp.MustCompile("(?i)(\\bbearer\\s+)[a-z0-9._~+/=-]+")

// A capture window may end while an assignment's quoted value is still open.
// In that case mask through the end of the retained window, including embedded
// whitespace, rather than exposing the tail of an incomplete secret.
var incompleteQuotedCredential = regexp.MustCompile("(?is)(\\b(?:authorization|password|passwd|token|secret|api[_-]?key)[ \\t]*[:=][ \\t]*)(?:\\\"(?:\\\\.|[^\\\"\\\\])*$|'(?:\\\\.|[^'\\\\])*$)")

func RedactDiagnosticText(s string) string {
	s = bearerValue.ReplaceAllString(s, "${1}[REDACTED]")
	s = incompleteQuotedCredential.ReplaceAllString(s, "${1}[REDACTED]")
	return credentialAssignment.ReplaceAllString(s, "${1}[REDACTED]")
}

// BoundedDiagnosticText applies credential masking before enforcing the final
// serialized UTF-8 diagnostic limit. omitted counts sanitized presentation
// bytes clipped by this bound; callers separately account for uncaptured native
// bytes and preserve the actual native BytesSeen measurement.
func BoundedDiagnosticText(raw string, limit int) (text string, omitted int64) {
	safe := strings.ToValidUTF8(RedactDiagnosticText(raw), "\uFFFD")
	if limit < 0 {
		limit = 0
	}
	if len(safe) <= limit {
		return safe, 0
	}
	n := limit
	for n > 0 && !utf8.ValidString(safe[:n]) {
		n--
	}
	return safe[:n], int64(len(safe) - n)
}
