package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
		{name: "empty auth mode", mutate: func(c *Config) { c.AuthMode = "" }},
		{name: "no auth mode", mutate: func(c *Config) { c.AuthMode = "none" }},
		{name: "unknown auth mode", mutate: func(c *Config) { c.AuthMode = "unexpected" }},
		{name: "missing credential source", mutate: func(c *Config) { c.BearerTokenFile = ""; c.CredentialStoreFile = "" }},
		{name: "ambiguous credential sources", mutate: func(c *Config) { c.CredentialStoreFile = "/tmp/mcp-credentials.json" }},
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

func TestNamedCredentialStoreConfiguration(t *testing.T) {
	c := Default()
	c.BearerTokenFile = ""
	c.CredentialStoreFile = "/etc/ai-server-agent/mcp-credentials.json"
	if err := c.Validate(); err != nil {
		t.Fatalf("named credential store config rejected: %v", err)
	}
}

func TestLoadNamedCredentialStoreDoesNotRetainLegacyDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := []byte(`{
  "listen_address":"127.0.0.1:3210",
  "mcp_path":"/mcp",
  "health_path":"/healthz",
  "auth_mode":"bearer",
  "credential_store_file":"/etc/ai-server-agent/mcp-credentials.json",
  "executor_socket":"/run/ai-server-agent/executor.sock",
  "executor_token_file":"/etc/ai-server-agent/executor.token",
  "state_dir":"/var/lib/ai-server-agent",
  "log_dir":"/var/log/ai-server-agent",
  "workspace_dir":"/srv/ai-workspace",
  "worker_user":"aiworker",
  "agent_user":"aiagent"
}`)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BearerTokenFile != "" || cfg.CredentialStoreFile == "" {
		t.Fatalf("unexpected credential sources: bearer=%q store=%q", cfg.BearerTokenFile, cfg.CredentialStoreFile)
	}
}
