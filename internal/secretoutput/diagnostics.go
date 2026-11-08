package secretoutput

import "regexp"

// RedactDiagnosticText removes recognizable credential assignments from
// otherwise useful native diagnostics. This is defense-in-depth, not a
// substitute for the exact-value streaming redactor when the secret is known.
var credentialAssignment = regexp.MustCompile("(?i)(\\b(?:authorization|password|passwd|token|secret|api[_-]?key)\\s*[:=]\\s*)(?:\\\"[^\\\"]*\\\"|'[^']*'|[^\\s,;]+)")
var bearerValue = regexp.MustCompile("(?i)(\\bbearer\\s+)[a-z0-9._~+/=-]+")

func RedactDiagnosticText(s string) string {
	s = bearerValue.ReplaceAllString(s, "${1}[REDACTED]")
	return credentialAssignment.ReplaceAllString(s, "${1}[REDACTED]")
}
