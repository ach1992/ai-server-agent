package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

type Component struct {
	Name      string   `json:"name"`
	Required  bool     `json:"required"`
	Installed bool     `json:"installed"`
	Paths     []string `json:"paths,omitempty"`
	Services  []string `json:"services,omitempty"`
	Ports     []string `json:"ports,omitempty"`
	Notes     string   `json:"notes,omitempty"`
}

type Manifest struct {
	SchemaVersion int         `json:"schema_version"`
	GeneratedAt   string      `json:"generated_at"`
	Purpose       string      `json:"purpose"`
	WorkerUser    string      `json:"worker_user"`
	AgentUser     string      `json:"agent_user"`
	WorkspaceDir  string      `json:"workspace_dir"`
	Critical      []Component `json:"critical_components"`
	Optional      []Component `json:"optional_components"`
	Rules         []string    `json:"rules_for_ai"`
}

func Build(c config.Config) Manifest {
	return Manifest{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Purpose:       "This server is dedicated to AI-operated development, deployment validation, diagnostics, and testing. Preserve the AI Server Agent control plane while changing the rest of the host as required.",
		WorkerUser:    c.WorkerUser,
		AgentUser:     c.AgentUser,
		WorkspaceDir:  c.WorkspaceDir,
		Critical: []Component{
			{Name: "control-plane", Required: true, Installed: true, Paths: []string{"/usr/local/bin/ai-server-agent", "/etc/ai-server-agent", c.StateDir, c.LogDir}, Services: []string{"ai-server-agent.service", "ai-server-agent-executor.service"}, Ports: []string{c.ListenAddress}, Notes: "Do not stop, disable, remove, overwrite, firewall, or rebind these resources unless the user explicitly requests maintenance of the agent itself."},
			{Name: "executor-socket", Required: true, Installed: true, Paths: []string{c.ExecutorSocket}, Notes: "Private local Unix socket used for privileged execution. It must remain local and must not be exposed over TCP."},
			{Name: "host-primitives", Required: true, Installed: true, Paths: []string{"/bin/bash", "/bin/systemctl", "/bin/systemd-run"}, Notes: "Minimal host primitives used for shell execution and persistent background jobs. Do not remove or replace them while the agent is in service."},
		},
		Optional: []Component{
			{Name: "download-utilities", Required: false, Installed: fileExists("/usr/bin/curl") && fileExists("/usr/bin/tar"), Paths: []string{"/usr/bin/curl", "/usr/bin/tar", "/usr/bin/xz"}, Notes: "Used for updates and optional browser setup. Safe to remove without stopping the running MCP core, but update/browser installation will need them restored."},
			{Name: "terminal", Required: false, Installed: fileExists("/usr/bin/tmux"), Paths: []string{"/usr/bin/tmux"}, Notes: "Optional. Installed only when interactive persistent terminals are requested."},
			{Name: "browser", Required: false, Installed: fileExists(filepath.Join(c.StateDir, "runtime/browser", "node", "bin", "node")), Paths: []string{filepath.Join(c.StateDir, "runtime/browser")}, Notes: "Optional isolated Playwright/Node runtime owned by the agent. It does not bind public ports and must not replace system web servers."},
		},
		Rules: []string{
			"Before host-wide package, firewall, network, service, disk, user, or web-stack changes, call agent_environment and preserve all critical components.",
			"The agent intentionally does not own ports 80 or 443 and does not require nginx, Apache, PHP, MySQL, Docker, Node.js, Python, or a control panel.",
			"Installing or replacing nginx, Apache, aaPanel, Docker, databases, language runtimes, and project dependencies is allowed when needed by the project.",
			"Do not stop or disable ai-server-agent.service or ai-server-agent-executor.service during ordinary project work.",
			"Do not remove the agent users, state directory, executor socket, configuration, token files, or the configured MCP listen endpoint.",
			"If a requested change could cut the active MCP path (firewall, route, interface, tunnel, DNS, TLS, listen address, agent service), explain the risk and obtain explicit user confirmation first.",
			"Prefer reversible changes, backups, and staged validation before destructive production-like operations.",
		},
	}
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func Write(path string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0640); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}
