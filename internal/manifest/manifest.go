package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

const (
	lowDiskAvailablePercent = 10
	lowDiskAvailableBytes   = 2 * 1024 * 1024 * 1024
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

type FilesystemInfo struct {
	Path             string `json:"path"`
	TotalBytes       uint64 `json:"total_bytes,omitempty"`
	AvailableBytes   uint64 `json:"available_bytes,omitempty"`
	AvailablePercent uint64 `json:"available_percent,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

type Manifest struct {
	SchemaVersion       int            `json:"schema_version"`
	InstanceID          string         `json:"instance_id,omitempty"`
	GeneratedAt         string         `json:"generated_at"`
	Purpose             string         `json:"purpose"`
	EnvironmentProfile  string         `json:"environment_profile"`
	ServerLabel         string         `json:"server_label,omitempty"`
	WorkerUser          string         `json:"worker_user"`
	AgentUser           string         `json:"agent_user"`
	WorkspaceDir        string         `json:"workspace_dir"`
	WorkspaceFilesystem FilesystemInfo `json:"workspace_filesystem"`
	Critical            []Component    `json:"critical_components"`
	Optional            []Component    `json:"optional_components"`
	Rules               []string       `json:"rules_for_ai"`
}

func Build(c config.Config) Manifest {
	purpose := "This server is designated for AI-operated software development, Git/source work, builds and debugging. It can also run real tests. Preserve the AI Server Agent control plane and respect task authorization."
	if c.EnvironmentProfile == "validation" {
		purpose = "This server is designated for AI-operated candidate installation, real testing, reproduction and acceptance evidence. Keep ordinary Git/development/root tools available when the authorized task requires them. Preserve the AI Server Agent control plane."
	}
	browserEngine := "/opt/ai-server-agent/browser"
	browserData := filepath.Join(c.StateDir, "runtime/browser")
	_, typeScript7Err := systemexec.TypeScript7()
	_, pythonTyErr := systemexec.TyPython()
	_, bashLSPErr := systemexec.BashLSP()
	_, webLSPErr := systemexec.WebLSP()
	_, sqlLSPErr := systemexec.SQLLens()
	_, _, _, rustLSPErr := systemexec.RustAnalyzerResources()
	_, _, phpLSPErr := systemexec.PHPActor()
	_, _, pythonDAPErr := systemexec.PythonDebugpy()
	_, _, javascriptDAPErr := systemexec.JavaScriptDAP()
	return Manifest{
		SchemaVersion:       1,
		InstanceID:          c.InstanceID,
		GeneratedAt:         time.Now().UTC().Format(time.RFC3339),
		Purpose:             purpose,
		EnvironmentProfile:  c.EnvironmentProfile,
		ServerLabel:         c.ServerLabel,
		WorkerUser:          c.WorkerUser,
		AgentUser:           c.AgentUser,
		WorkspaceDir:        c.WorkspaceDir,
		WorkspaceFilesystem: workspaceFilesystemInfo(c.WorkspaceDir),
		Critical: []Component{
			{Name: "control-plane", Required: true, Installed: true, Paths: []string{"/usr/local/bin/ai-server-agent", "/etc/ai-server-agent", c.StateDir, c.LogDir}, Services: []string{"ai-server-agent.service", "ai-server-agent-executor.service"}, Ports: []string{c.ListenAddress}, Notes: "Do not stop, disable, remove, overwrite, firewall, or rebind these resources unless the user explicitly requests maintenance of the agent itself."},
			{Name: "executor-socket", Required: true, Installed: true, Paths: []string{c.ExecutorSocket}, Notes: "Private local Unix socket used for privileged execution. It must remain local and must not be exposed over TCP."},
			{Name: "host-primitives", Required: true, Installed: true, Paths: []string{"/bin/bash", "/bin/systemctl", "/bin/systemd-run"}, Notes: "Minimal host primitives used for shell execution and persistent background jobs. Do not remove or replace them while the agent is in service."},
		},
		Optional: []Component{
			{Name: "download-utilities", Required: false, Installed: fileExists("/usr/bin/curl") && fileExists("/usr/bin/tar"), Paths: []string{"/usr/bin/curl", "/usr/bin/tar", "/usr/bin/xz"}, Notes: "Used for updates and optional browser setup. Safe to remove without stopping the running MCP core, but update/browser installation will need them restored."},
			{Name: "terminal", Required: false, Installed: systemexec.First("/usr/bin/tmux") != "", Paths: []string{"/usr/bin/tmux"}, Notes: "Optional trusted root-owned, non-symlink tmux provides Control Mode. Worker panes additionally require trusted /usr/bin/setpriv, /usr/bin/env, a trusted bash (/usr/bin/bash or /bin/bash) and Agent-managed private worker HOME; both worker/root tmux sockets are executor-root-owned. This is not a core dependency."},
			{Name: "go-language-server", Required: false, Installed: systemexec.First("/usr/local/bin/gopls", "/usr/bin/gopls") != "", Paths: []string{"/usr/local/bin/gopls", "/usr/bin/gopls"}, Notes: "Optional Go semantic-code intelligence. code_* requires exact aiworker file_version and does not install gopls or a Go toolchain."},
			{Name: "typescript-language-server", Required: false, Installed: typeScript7Err == nil, Paths: []string{systemexec.TypeScript7Library}, Notes: "Optional pinned native TypeScript 7.0.2 LSP for JavaScript/TypeScript. Administrator-controlled complete 110-file resource directory is validated for root ownership, no symlinks and per-architecture SHA-256. Missing or invalid adapters degrade only code_* for JS/TS; no node, global npm, automatic installer, project script or additional broker dependency."},
			{Name: "python-language-server", Required: false, Installed: pythonTyErr == nil, Paths: []string{systemexec.PythonTyBinary}, Notes: "Optional native Python ty 0.0.87 LSP. Only administrator-owned, per-architecture SHA-256 pinned binary is launched; does not execute Python source, install packages, use arbitrary project run commands or require a Python interpreter as a control-plane dependency. Not a replacement for Python DAP."},
			{Name: "shell-language-server", Required: false, Installed: bashLSPErr == nil, Paths: []string{systemexec.BashLSPDir}, Notes: "Optional Bash/POSIX sh LSP powered by bash-language-server 5.8.1, root-owned checksum-verified complete Node modules, Node v24.18.1 and ShellCheck v0.11.0. Dialect inferred from source shebang, shellcheck external-source execution disabled; no project command or auto-install, Shell DAP is a separate feasibility gate."},
			{Name: "html-css-language-server", Required: false, Installed: webLSPErr == nil, Paths: []string{systemexec.WebLSPDir}, Notes: "Optional HTML/CSS semantic LSP via VS Code Language Servers 4.10.0; trusted Node with a pinned 4948-file root-owned transitive module tree. CSS diagnostics use validated push notifications; HTML structured symbols supported but not all markup problems produce diagnostics. Browser tools remain separately appropriate for render/layout assertions."},
			{Name: "sql-language-server", Required: false, Installed: sqlLSPErr == nil, Paths: []string{systemexec.SQLLensDir}, Notes: "Optional offline-first SQL semantic LSP via SQLLens 0.5.0 pinned root-owned complete module tree and Node v24.18.1. Requires explicit postgres/mysql/mariadb dialect per call, no project plugin execution (SQLLENS_NO_PLUGINS=1), no user config loading and no DB connections. MariaDB uses MySQL-compatible grammar subset; DB metadata, migrations and vendor-specific semantics need separate acceptance."},
			{Name: "rust-language-server", Required: false, Installed: rustLSPErr == nil, Paths: []string{systemexec.RustAnalyzerDir, systemexec.RustCompilerDir}, Notes: "Optional Rust analyzer 2026-10-12 with fixed Rust 1.99.0 compiler and sysroot. Passive per-file LSP graph avoids Cargo metadata, build.rs and proc-macro execution; no automatic dependency resolution or Cargo builds."},
			{Name: "php-language-server", Required: false, Installed: phpLSPErr == nil, Paths: []string{systemexec.PHPActorDir}, Notes: "Optional Phpactor 2026.10.07.0 with SHA-256 pinned PHAR and trusted OS-managed versioned PHP 8.2/8.3/8.4 CLI. Disables php.ini, Composer execution and project configuration. Uses private worker-home index cache; PHP Xdebug DAP remains separate."},
			{Name: "javascript-debugger", Required: false, Installed: javascriptDAPErr == nil, Paths: []string{systemexec.JavaScriptDAPDir}, Notes: "Optional Microsoft js-debug 1.140.0 Node JavaScript DAP. Complete 60-file upstream bundle and Node v24.18.1 are content and root-provenance checked. Only one local Node target in a private peer-pinned Unix socket pair, atomically executor-owned cgroup-v2. Precompiled TypeScript source maps require an adjacent pinned .js.map and versioned .ts breakpoints; this resource readiness does not prove installed E2E or Browser/extension modes; no TCP/remote attach, runInTerminal or package-script autostart."},
			{Name: "python-debugger", Required: false, Installed: pythonDAPErr == nil, Paths: []string{systemexec.PythonDebugpyDir}, Notes: "Optional debugpy 1.8.22 Python script/module/unittest DAP (Python >=3.10). Exact pinned wheel and all 428 transitive files are verified root-owned; -I adapter launch ignores project/user Python site config. Requires executor-owned cgroup-v2 containment and exact worker source file_version. No implicit install or root/remote attach; installed modules alone do not prove a working debug session."},
			{Name: "go-debugger", Required: false, Installed: systemexec.First("/usr/local/bin/dlv", "/usr/bin/dlv") != "", Paths: []string{"/usr/local/bin/dlv", "/usr/bin/dlv"}, Notes: "Optional Go Delve DAP debugger. v1 launches only prebuilt workspace executables with matching worker file_version; requires trusted admin-owned Delve plus executor-owned root cgroup-v2 containment with cgroup.kill (Linux 5.14+); fails closed before DAP launch without those capabilities. Other Agent capabilities remain usable. Never silently installs/attaches/uses root."},
			{Name: "browser", Required: false, Installed: fileExists(filepath.Join(browserEngine, "node/bin/node")), Paths: []string{browserEngine, browserData}, Notes: "Optional Playwright/Chromium capability. The executable engine is root-owned under /opt; writable browser profile data is isolated under agent state. Removing it disables browser tools but does not stop the MCP core."},
		},
		Rules: []string{
			"Before host-wide package, firewall, network, service, disk, user, or web-stack changes, call agent_environment and preserve all critical components.",
			fmt.Sprintf("%s is persistent. Inspect and reuse existing repositories, worktrees, and temporary resources before creating duplicates; prefer git worktree when another checkout of the same repository is needed.", c.WorkspaceDir),
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

func workspaceFilesystemInfo(path string) FilesystemInfo {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return FilesystemInfo{
			Path:    path,
			Warning: fmt.Sprintf("Workspace filesystem usage is unavailable for %s; inspect disk capacity before creating large workspace artifacts.", path),
		}
	}
	return filesystemInfo(path, stat.Blocks, stat.Bavail, uint64(stat.Bsize))
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
		info.Warning = fmt.Sprintf("Workspace filesystem usage is unavailable for %s; inspect disk capacity before creating large workspace artifacts.", path)
		return info
	}
	info.AvailablePercent = availableBlocks * 100 / blocks
	if info.AvailableBytes < lowDiskAvailableBytes || info.AvailablePercent < lowDiskAvailablePercent {
		info.Warning = "Workspace filesystem is materially constrained; reuse existing workspace resources and avoid large new artifacts until disk space is reviewed."
	}
	return info
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func executableFileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0
}

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
