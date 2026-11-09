package secretoutput

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDiagnosticTextTargetedRedactionPreservesNativeCause(t *testing.T) {
	const raw = "HTTP native probe failed: dial tcp refused; Authorization: Bearer token-with-dots.abc; password='private pass'; api_key=secretkey42; exit status 47"
	out := RedactDiagnosticText(raw)
	for _, want := range []string{"HTTP native probe failed", "dial tcp refused", "exit status 47", "Authorization: [REDACTED]", "password=[REDACTED]", "api_key=[REDACTED]"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost useful diagnostics or credential masking: want %q in %q", want, out)
		}
	}
	for _, secret := range []string{"token-with-dots.abc", "private pass", "secretkey42"} {
		if strings.Contains(out, secret) {
			t.Errorf("credential leaked: %q", out)
		}
	}
}

func TestDiagnosticRedactionMasksIncompleteQuotedAssignments(t *testing.T) {
	for _, fragment := range []string{
		"native failure; password=\"many private words near boundary",
		"native failure; token='many private words near boundary",
		"native failure; api_key=\"escaped \\\" inner quote and more private words",
	} {
		redacted := RedactDiagnosticText(fragment)
		if strings.Contains(redacted, "private words") || !strings.Contains(redacted, "[REDACTED]") ||
			!strings.Contains(redacted, "native failure") {
			t.Fatalf("incomplete credential was not masked to capture boundary: %q", redacted)
		}
	}
	complete := RedactDiagnosticText("native failure; password=\"two private words\"; diagnosis: connection refused")
	escaped := RedactDiagnosticText("native failure; password=\"two private \\\"quoted\\\" words\"; diagnosis: connection refused")
	if strings.Contains(escaped, "private") || strings.Contains(escaped, "quoted") ||
		!strings.Contains(escaped, "diagnosis: connection refused") {
		t.Fatalf("escaped quoted credential leaked despite closing quote: %q", escaped)
	}
	if strings.Contains(complete, "private words") || !strings.Contains(complete, "diagnosis: connection refused") {
		t.Fatalf("complete quote either leaked or hid following nonsecret context: %q", complete)
	}
}

func TestBoundedDiagnosticTextEnforcesPostRedactionBytes(t *testing.T) {
	raw := strings.Repeat("token=a;", 500) // 4000 raw bytes, expands beyond 4096 when masked
	got, omitted := BoundedDiagnosticText(raw, 4096)
	if len(raw) != 4000 || len(got) != 4096 || omitted == 0 ||
		strings.Contains(got, "token=a;") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("redacted diagnostic exceeded final bound or failed masking: seen=%d returned=%d omitted=%d", len(raw), len(got), omitted)
	}
	utf8Raw := strings.Repeat("✅", 2500)
	utf8Got, utf8Omitted := BoundedDiagnosticText(utf8Raw, 4096)
	if len(utf8Got) > 4096 || !utf8.ValidString(utf8Got) || utf8Omitted == 0 {
		t.Fatalf("invalid UTF-8 clipping: returned=%d omitted=%d", len(utf8Got), utf8Omitted)
	}
}

func TestDiagnosticRedactionMasksCompleteAuthorizationField(t *testing.T) {
	cases := []struct {
		name, raw, secret string
	}{
		{"basic", "git failure; Authorization: Basic PRIVATE-BASE64; connection refused", "PRIVATE-BASE64"},
		{"bearer", "git failure; Authorization: Bearer \"PRIVATE-BEARER\"; connection refused", "PRIVATE-BEARER"},
		{"quoted entire header", "git failure; Authorization: \"Basic PRIVATE-QUOTED\"; connection refused", "PRIVATE-QUOTED"},
		{"digest multipart", "git failure; Authorization: Digest username=alice, response=PRIVATE-DIGEST; connection refused", "PRIVATE-DIGEST"},
		{"sigv4 signed headers", "git failure; Authorization: AWS4-HMAC-SHA256 Credential=TEST_ID/20261109/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=PRIVATE-SIGNATURE; connection refused", "PRIVATE-SIGNATURE"},
		{"sigv4 spaced authorization parameter", "git failure; Authorization: AWS4-HMAC-SHA256 Credential=TEST_ID, SignedHeaders=host; Signature=PRIVATE-SIGNATURE-SPACED; connection refused", "PRIVATE-SIGNATURE-SPACED"},
		{"compact semicolon in opaque header", "git failure; Authorization: Custom PRIVATE-ONE;PRIVATE-TWO; connection refused", "PRIVATE-TWO"},
		{"quoted delimiter", "git failure; Authorization: \"Basic PRIVATE;QUOTED\"; connection refused", "PRIVATE;QUOTED"},
		{"unfinished quote", "git failure; Authorization: Basic 'PRIVATE-UNFINISHED; still-secret", "PRIVATE-UNFINISHED"},
		{"capture edge", "git failure; Authorization: Basic PRIVATE-TRUNCATED", "PRIVATE-TRUNCATED"},
		{"separate line", "git failure; Authorization: Basic PRIVATE-LINE\nconnection refused", "PRIVATE-LINE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactDiagnosticText(tc.raw)
			if strings.Contains(out, tc.secret) || !strings.Contains(out, "Authorization: [REDACTED]") ||
				!strings.Contains(out, "git failure") {
				t.Fatalf("Authorization value not fully masked: %q", out)
			}
			if strings.Contains(tc.raw, "connection refused") && !strings.Contains(out, "connection refused") {
				t.Fatalf("redaction discarded independent diagnostics: %q", out)
			}
		})
	}
}
