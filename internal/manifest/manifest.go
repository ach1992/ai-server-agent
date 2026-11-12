package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	Version   string   `json:"version,omitempty"`
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
	switch c.EnvironmentProfile {
	case "validation":
		purpose = "This server is designated for AI-operated candidate installation, real testing, reproduction and acceptance evidence. Keep ordinary Git/development/root tools available when the authorized task requires them. Preserve the AI Server Agent control plane."
	case "production":
		purpose = "Production: live workloads and data. Prefer passive read-only inspection. Before each state-changing action, ask the user for fresh, action-specific permission; never reuse prior consent. This is AI guidance only: the Agent does not verify such consent or restrict tools. Preserve the AI Server Agent control plane."
	}
	browserEngine := "/opt/ai-server-agent/browser"
	browserData := filepath.Join(c.StateDir, "runtime/browser")
	installed := liveOptionalReadiness()
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
			{Name: "workspace-text-search", Required: false, Installed: systemexec.First("/usr/bin/rg") != "", Paths: []string{"/usr/bin/rg"}, Notes: "Optional trusted system ripgrep is required for workspace_search(mode=text). Without it, the tool returns search_engine_unavailable; structured worker file reads and shell/Git remain independently available. No automatic OS package installation."},
			{Name: "terminal", Required: false, Installed: systemexec.First("/usr/bin/tmux") != "", Paths: []string{"/usr/bin/tmux"}, Notes: "Optional trusted root-owned, non-symlink tmux provides Control Mode. Worker panes additionally require trusted /usr/bin/setpriv, /usr/bin/env, a trusted bash (/usr/bin/bash or /bin/bash) and Agent-managed private worker HOME; both worker/root tmux sockets are executor-root-owned. This is not a core dependency."},
			{Name: "go-language-server", Required: false, Installed: systemexec.First("/usr/local/bin/gopls", "/usr/bin/gopls") != "", Paths: []string{"/usr/local/bin/gopls", "/usr/bin/gopls"}, Notes: "Optional Go semantic-code intelligence. code_* requires exact aiworker file_version and does not install gopls or a Go toolchain."},
			{Name: "typescript-language-server", Required: false, Installed: installed[optionalTypeScript], Paths: optionalAdapterPaths("typescript-lsp", systemexec.TypeScript7Library), Notes: "Optional operator-selected native TypeScript LSP with complete root-owned verified resources. Legacy built-in selection is the stated fallback. No project script or arbitrary PATH execution.", Version: systemexec.OptionalAdapterVersion("typescript-lsp", systemexec.TypeScript7Version)},
			{Name: "python-language-server", Required: false, Installed: installed[optionalPythonTy], Paths: optionalAdapterPaths("python-lsp", systemexec.PythonTyBinary), Notes: "Optional verified root-owned Python ty language server; independent of the Python debugger and project Python environment.", Version: systemexec.OptionalAdapterVersion("python-lsp", systemexec.PythonTyVersion)},
			{Name: "shell-language-server", Required: false, Installed: installed[optionalBashLSP], Paths: optionalAdapterPaths("bash-lsp", systemexec.BashLSPDir), Notes: "Optional Bash LSP with verified complete Node modules, trusted Node and ShellCheck. Shell DAP is independent; no project command or automatic npm install.", Version: systemexec.OptionalAdapterVersion("bash-lsp", systemexec.BashLSPVersion)},
			{Name: "html-css-language-server", Required: false, Installed: installed[optionalWebLSP], Paths: optionalAdapterPaths("web-lsp", systemexec.WebLSPDir), Notes: "Optional root-owned HTML/CSS LSP with a complete verified Node dependency tree. Browser render/layout inspection remains separate.", Version: systemexec.OptionalAdapterVersion("web-lsp", systemexec.WebLSPVersion)},
			{Name: "sql-language-server", Required: false, Installed: installed[optionalSQLLens], Paths: optionalAdapterPaths("sql-lsp", systemexec.SQLLensDir), Notes: "Optional SQLLens source-only LSP with selected verified Node tree, explicit SQL dialect, no plugins/user config/DB connection or execution.", Version: systemexec.OptionalAdapterVersion("sql-lsp", systemexec.SQLLensVersion)},
			{Name: "rust-language-server", Required: false, Installed: installed[optionalRustLSP], Paths: optionalAdapterPaths("rust-lsp", systemexec.RustAnalyzerDir, systemexec.RustCompilerDir), Notes: "Optional rust-analyzer with selected verified analyzer/compiler/sysroot source. Source-only graph avoids Cargo metadata, build.rs and project proc-macro execution.", Version: systemexec.OptionalAdapterVersion("rust-lsp", systemexec.RustAnalyzerVersion)},
			{Name: "php-language-server", Required: false, Installed: installed[optionalPHPActor], Paths: optionalAdapterPaths("php-lsp", systemexec.PHPActorDir), Notes: "Optional selected verified Phpactor PHAR and trusted OS-managed PHP CLI 8.2+. Disables project Composer and PHP ini execution; Xdebug debugger remains independent.", Version: systemexec.OptionalAdapterVersion("php-lsp", systemexec.PHPActorVersion)},
			{Name: "javascript-debugger", Required: false, Installed: installed[optionalJavaScriptDAP], Paths: optionalAdapterPaths("javascript-dap", systemexec.JavaScriptDAPDir), Notes: "Optional selected verified js-debug bundle plus Node runtime; private worker-only DAP containment. Source maps, browser and extension-host modes have independent contracts.", Version: systemexec.OptionalAdapterVersion("javascript-dap", systemexec.JavaScriptDAPVersion)},
			{Name: "python-debugger", Required: false, Installed: installed[optionalPythonDAP], Paths: optionalAdapterPaths("python-dap", systemexec.PythonDebugpyDir), Notes: "Optional verified debugpy package and trusted Python 3.10+. No implicit project installation/attach. Containment and real debugging must be tested independently.", Version: systemexec.OptionalAdapterVersion("python-dap", systemexec.PythonDebugpyVersion)},
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

// Profiles are runtime-selected resource identities, not compiled-in release
// paths. When a selected profile is unavailable, expose its exact config path
// so operators can diagnose it rather than falsely reporting an old version.
func optionalAdapterPaths(name string, fallback ...string) []string {
	profile, configured, err := systemexec.SelectedOptionalAdapterMetadata(name)
	if !configured {
		return fallback
	}
	if err != nil {
		return []string{filepath.Join(systemexec.OptionalAdapterProfilesDir, name+".json")}
	}
	keys := make([]string, 0, len(profile.Entrypoints))
	for key := range profile.Entrypoints {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	paths := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		path := profile.Entrypoints[key]
		if !seen[path] {
			paths = append(paths, path)
			seen[path] = true
		}
	}
	return paths
}
