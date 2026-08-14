package config

import "testing"

func TestTLSConfigurationRequiresCertificateAndKeyTogether(t *testing.T) {
	c := Default()
	c.TLSCertFile = "/tmp/origin.crt"
	if err := c.Validate(); err == nil {
		t.Fatal("expected certificate-only TLS config to be rejected")
	}

	c = Default()
	c.TLSKeyFile = "/tmp/origin.key"
	if err := c.Validate(); err == nil {
		t.Fatal("expected key-only TLS config to be rejected")
	}

	c = Default()
	c.TLSCertFile = "/tmp/origin.crt"
	c.TLSKeyFile = "/tmp/origin.key"
	if err := c.Validate(); err != nil {
		t.Fatalf("complete TLS config rejected: %v", err)
	}
	if !c.TLSConfigured() {
		t.Fatal("complete TLS config must report TLS configured")
	}
}
