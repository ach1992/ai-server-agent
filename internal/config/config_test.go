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

func TestAuthenticationConfigurationIsFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "no auth mode", mutate: func(c *Config) { c.AuthMode = "none" }},
		{name: "unknown auth mode", mutate: func(c *Config) { c.AuthMode = "unexpected" }},
		{name: "missing bearer token path", mutate: func(c *Config) { c.BearerTokenFile = "" }},
		{name: "missing executor token path", mutate: func(c *Config) { c.ExecutorToken = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected invalid authentication configuration to be rejected")
			}
		})
	}
}
