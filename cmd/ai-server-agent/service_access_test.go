package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

func TestServiceAccessOperatorAddOneCommandAndExistingEnroll(t *testing.T) {
	add, err := parseServiceCredentialFlags([]string{
		"add", "--service", "github", "--endpoint", "ssh://git.example.org:22",
		"--kind", "private_key", "--purpose", "disposable-test",
		"--secret-file", "/root/private/service.key", "--never",
	})
	if err != nil || add.operation != "add" || add.ref != "" ||
		add.secretFile != "/root/private/service.key" || add.kind != "private_key" ||
		!add.never || add.endpoint != "ssh://git.example.org:22" {
		t.Fatalf("expected safe one-command registration options: %+v %v", add, err)
	}
	rotate, err := parseServiceCredentialFlags([]string{
		"enroll", "--ref", "svc_" + strings.Repeat("a", 32),
		"--kind", "api_key", "--ttl-days", "12",
	})
	if err != nil || rotate.operation != "enroll" || rotate.ttlDays != 12 ||
		rotate.kind != "api_key" {
		t.Fatalf("expected existing reference rotation options: %+v %v", rotate, err)
	}
}

func TestServiceAccessOperatorLocalApplicationDoesNotRequireFakeURL(t *testing.T) {
	local, err := parseServiceCredentialFlags([]string{"add", "--service", "local-license",
		"--kind", "access_code", "--purpose", "offline entitlement"})
	if err != nil || local.endpoint != "" || local.kind != "access_code" || local.service != "local-license" {
		t.Fatalf("host-local credential unnecessarily requires network endpoint: %+v %v", local, err)
	}
}

func TestServiceAccessOperatorRejectsPlaintextFlagsAndAmbiguousOptions(t *testing.T) {
	for _, args := range [][]string{
		{"add", "--service", "app", "--endpoint", "http://host", "--password", "SECRET"},
		{"add", "--service", "app", "--endpoint", "http://host", "SECRET"},
		{"add", "--service", "app", "--endpoint", "https://user:password@host"},
		{"add", "--service", "app", "--endpoint", "ssh://host?token=SECRET"},
		{"add", "--endpoint", "https://host"},
		{"add", "--service", "app", "--endpoint", "ssh://host", "--ref", "svc_123"},
		{"enroll", "--ref", "svc_123", "--endpoint", "https://host"},
		{"enroll", "--kind", "token"},
		{"enroll", "--ref", "svc_123", "--kind", "KEY WITH SPACES"},
		{"enroll", "--ref", "svc_123", "--never", "--ttl-days", "20"},
		{"enroll", "--ref", "svc_123", "--ttl-days", "-1"},
	} {
		if _, err := parseServiceCredentialFlags(args); err == nil {
			t.Errorf("accepted unsafe or ambiguous flags: %#v", args)
		}
	}
	// The local secret importer must not accept untrusted/unpinned paths.
	for _, path := range []string{"relative.key", "../key", "/tmp/../etc/passwd", "/etc/passwd"} {
		if _, err := readRootOwnedSecretFile(path); err == nil {
			t.Errorf("accepted unsafe private-file path %q", path)
		}
	}
}

func TestServiceAccessRevealFormatsExactSecretForOperatorTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, secret string
	}{
		{"password", "owner-copyable-password-123!"},
		{"multiline", "-----BEGIN PRIVATE KEY-----\nexample\n-----END PRIVATE KEY-----\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := serviceaccess.Record{
				Service: "Example Service", Username: "admin", Kind: tc.name, Secret: tc.secret,
			}
			var terminal bytes.Buffer
			if err := writeServiceCredentialToTerminal(&terminal, record); err != nil {
				t.Fatal(err)
			}
			expect := "Service: Example Service\nUsername: admin\nKind: " + tc.name + "\nCredential:\n" + tc.secret + "\n"
			if got := terminal.String(); got != expect {
				t.Fatalf("operator terminal did not contain exact credential text: got len=%d want len=%d", len(got), len(expect))
			}
		})
	}
}

func TestServiceAccessRevealRequiresRootOperator(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root denial verified on ordinary aiworker tests")
	}
	err := revealServiceCredential(config.Config{}, []string{"--ref", "svc_example"})
	if err == nil || !strings.Contains(err.Error(), "root operator") {
		t.Fatalf("non-root credential reveal accepted: %v", err)
	}
}
