package secretoutput

import (
	"strings"
	"testing"
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
