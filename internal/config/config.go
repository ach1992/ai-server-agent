package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	ListenAddress   string `json:"listen_address"`
	MCPPath         string `json:"mcp_path"`
	HealthPath      string `json:"health_path"`
	AuthMode        string `json:"auth_mode"`
	BearerTokenFile string `json:"bearer_token_file"`
	TLSCertFile     string `json:"tls_cert_file,omitempty"`
	TLSKeyFile      string `json:"tls_key_file,omitempty"`
	ExecutorSocket  string `json:"executor_socket"`
	ExecutorToken   string `json:"executor_token_file"`
	StateDir        string `json:"state_dir"`
	LogDir          string `json:"log_dir"`
	WorkspaceDir    string `json:"workspace_dir"`
	WorkerUser      string `json:"worker_user"`
	AgentUser       string `json:"agent_user"`
	PublicBaseURL   string `json:"public_base_url,omitempty"`
}

func Default() Config {
	return Config{
		ListenAddress:   "127.0.0.1:3210",
		MCPPath:         "/mcp",
		HealthPath:      "/healthz",
		AuthMode:        "bearer",
		BearerTokenFile: "/etc/ai-server-agent/mcp.token",
		ExecutorSocket:  "/run/ai-server-agent/executor.sock",
		ExecutorToken:   "/etc/ai-server-agent/executor.token",
		StateDir:        "/var/lib/ai-server-agent",
		LogDir:          "/var/log/ai-server-agent",
		WorkspaceDir:    "/srv/ai-workspace",
		WorkerUser:      "aiworker",
		AgentUser:       "aiagent",
	}
}

func (c Config) TLSConfigured() bool {
	return c.TLSCertFile != "" && c.TLSKeyFile != ""
}

func (c Config) Validate() error {
	if c.ListenAddress == "" || c.MCPPath == "" || c.ExecutorSocket == "" || c.StateDir == "" || c.WorkspaceDir == "" {
		return errors.New("config contains empty required values")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("tls_cert_file and tls_key_file must be configured together")
	}
	return nil
}

func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0640)
}
