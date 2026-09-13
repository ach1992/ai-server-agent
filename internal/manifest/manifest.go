package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
)

const lowDiskUsedPercent = 90

type Component struct {
	Name      string   `json:"name"`
	Required  bool     `json:"required"`
	Installed bool     `json:"installed"`
	Paths     []string `json:"paths,omitempty"`
	Services  []string `json:"services,omitempty"`
	Ports     []string `json:"ports,omitempty"`
	Notes     string   `json:"notes,omitempty"`
}

type FilesystemInfo struct {
	Path           string `json:"path"`
	TotalBytes     uint64 `json:"total_bytes,omitempty"`
	AvailableBytes uint64 `json:"available_bytes,omitempty"`
	UsedPercent    uint64 `json:"used_percent,omitempty"`
	Warning        string `json:"warning,omitempty"`
}

type Manifest struct {
	SchemaVersion  int            `json:"schema_version"`
	GeneratedAt    string         `json:"generated_at"`
	Purpose        string         `json:"purpose"`
	WorkerUser     string         `json:"worker_user"`
	AgentUser      string         `json:"agent_user"`
	WorkspaceDir   string         `json:"workspace_dir"`
	RootFilesystem FilesystemInfo `json:"root_filesystem"`
	Critical       []Component    `json:"critical_components"`
	Optional       []Component    `json:"optional_components"`
	Rules          []string       `json:"rules_for_ai"`
}

func Build(c config.Config) Manifest {
	browserEngine := "/opt/ai-server-agent/browser"
	browserData := filepath.Join(c.StateDir, "runtime/browser")
	return Manifest{
		SchemaVersion:  1,
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Purpose:        "This server is dedicated to AI-operated development, deployment validation, diagnostics, and testing. Preserve the AI Server Agent control plane while changing the rest of the host as required.",
		WorkerUser:     c.WorkerUser,
		AgentUser:      c.AgentUser,
		WorkspaceDir:   c.WorkspaceDir,
		RootFilesystem: rootFilesystemInfo(),
		Critical: []Component{
			{Name: "control-plane", Required: true, Installed: true, Paths: []string{"/usr/local/bin/ai-server-agent", "/etc/ai-server-agent", c.StateDir, c.LogDir}, Services: []string{"ai-server-agent.service", "ai-server-agent-executor.service"}, Ports: []string{c.ListenAddress}, Notes: "Do not stop, disable, remove, overwrite, firewall, or rebind these resources unless the user explicitly requests maintenance of the agent itself."},
			{Name: "executor-socket", Required: true, Installed: true, Paths: []string{c.ExecutorSocket}, Notes: "Private local Unix socket used for privileged execution. It must remain local and must not be exposed over TCP."},
			{Name: "host-primitives", Required: true, Installed: true, Paths: []string{"/bin/bash", "/bin/systemctl", "/bin/systemd-run"}, Notes: "Minimal host primitives used for shell execution and persistent background jobs. Do not remove or replace them while the agent is in service."},
		},
		Optional: []Component{
			{Name: "download-utilities", Required: false, Installed: fileExists("/usr/bin/curl") && fileExists("/usr/bin/tar"), Paths: []string{"/usr/bin/curl", "/usr/bin/tar", "/usr/bin/xz"}, Notes: "Used for updates and optional browser setup. Safe to remove without stopping the running MCP core, but update/browser installation will need them restored."},
			{Name: "terminal", Required: false, Installed: fileExists("/usr/bin/tmux"), Paths: []string{"/usr/bin/tmux"}, Notes: "Optional. AI may install tmux only when an interactive persistent terminal is needed; the MCP core does not depend on it."},
			{Name: "browser", Required: false, Installed: fileExists(filepath.Join(browserEngine, "node/bin/node")), Paths: []string{browserEngine, browserData}, Notes: "Optional Playwright/Chromium capability. The executable engine is root-owned under /opt; writable browser profile data is isolated under agent state. Removing it disables browser tools but does not stop the MCP core."},
		},
		Rules: []string{
			"Before host-wide package, firewall, network, service, disk, user, or web-stack changes, call agent_environment and preserve all critical components.",
			"The configured workspace is persistent. Inspect and reuse existing repositories, worktrees, and temporary resources before creating duplicates; prefer git worktree when another checkout of the same repository is needed.",
			"Remove only resources that are clearly disposable and owned by the current task. Dirty, untracked, ambiguous, or unknown workspace state is not safe to delete.",
			"The agent intentionally does not own ports 80 or 443 and does not require nginx, Apache, PHP, MySQL, Docker, Node.js, Python, or a control panel.",
			"Installing or replacing nginx, Apache, aaPanel, Docker, databases, language runtimes, and project dependencies is allowed when needed by the project.",
			"Do not stop or disable ai-server-agent.service or ai-server-agent-executor.service during ordinary project work.",
			"Do not remove the agent users, state directory, executor socket, configuration, token files, or the configured MCP listen endpoint.",
			"If a requested change could cut the active MCP path (firewall, route, interface, tunnel, DNS, TLS, listen address, agent service), explain the risk and obtain explicit user confirmation first.",
			"Optional capabilities may be installed, upgraded, or removed without changing the MCP core; update this manifest implementation when an optional capability gains a new persistent dependency.",
			"Prefer reversible changes, backups, and staged validation before destructive production-like operations.",
		},
	}
}

func rootFilesystemInfo() FilesystemInfo {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return FilesystemInfo{
			Path:    "/",
			Warning: "Root filesystem usage is unavailable; inspect disk capacity before creating large workspace artifacts.",
		}
	}
	return filesystemInfo("/", stat.Blocks, stat.Bavail, uint64(stat.Bsize))
}

func filesystemInfo(path string, blocks, availableBlocks, blockSize uint64) FilesystemInfo {
	if availableBlocks > blocks {
		availableBlocks = blocks
	}
	info := FilesystemInfo{
		Path:           path,
		TotalBytes:     blocks * blockSize,
		AvailableBytes: availableBlocks * blockSize,
	}
	if blocks == 0 {
		info.Warning = "Root filesystem usage is unavailable; inspect disk capacity before creating large workspace artifacts."
		return info
	}
	info.UsedPercent = (blocks - availableBlocks) * 100 / blocks
	if info.UsedPercent >= lowDiskUsedPercent {
		info.Warning = "Root filesystem is at least 90% utilized; reuse existing workspace resources and avoid large new artifacts until disk space is reviewed."
	}
	return info
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
