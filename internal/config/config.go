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

func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if c.ListenAddress == "" || c.MCPPath == "" || c.ExecutorSocket == "" || c.StateDir == "" || c.WorkspaceDir == "" {
		return c, errors.New("config contains empty required values")
	}
	return c, nil
}

func Save(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0640)
}
