package secretoutput

import (
	"bytes"
	"strings"
	"testing"
)

func TestTargetedRedactionKeepsUsefulNativeDiagnostic(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, []byte("pRivATE-123!"), []byte("encoded-secret-in-response"))
	for _, segment := range []string{
		"native-transport exit_code=255\nError: app is unreachable\npassword=pRi",
		"vATE-123!\nservice=panel status=failed\n",
		"other-field=keep-me\nencoded-",
		"secret-in-response\n",
	} {
		if _, err := r.Write([]byte(segment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	result := out.String()
	if strings.Contains(result, "pRivATE-123!") ||
		strings.Contains(result, "encoded-secret-in-response") {
		t.Fatalf("unredacted private diagnostic: %q", result)
	}
	for _, required := range []string{
		"exit_code=255", "app is unreachable", "service=panel status=failed",
		"other-field=keep-me", "[REDACTED]",
	} {
		if !strings.Contains(result, required) {
			t.Errorf("diagnostic fact %q missing in %q", required, result)
		}
	}
}

func TestNoProtectedValueDoesNotSuppressLogs(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, nil)
	const source = "provider=incus exit=1\nstderr=cannot connect to container\n"
	if _, err := r.Write([]byte(source)); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if out.String() != source {
		t.Fatalf("diagnostic text was shortened: %q", out.String())
	}
}
