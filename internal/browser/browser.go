package browser

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
)

const (
	browserEnginePermissionsCommand = `chmod -R a+rX,go-w "$engine"`

	browserNodeVersion       = "v24.18.1"
	browserNodeSHA256X64     = "d6c664df3f3f61458e8c277585571328522d705166723a7c7823a9253a4d15a0"
	browserNodeSHA256ARM64   = "7201e3a09dc825bac57867c81913e2b8f0ef87d04cb9082af4cda82f6ff3d88c"
	browserPlaywrightVersion = "1.61.1"
	browserChromiumRevision  = "1228"
	browserChromiumVersion   = "149.0.7827.55"
	chromiumTreeSHA256AMD64  = "67eacbbb82f9f75dc8a7a0ddcf59c7efed18bfe10199a73b7cd929e66a5e1b80"
	chromiumTreeSHA256ARM64  = "febb6527bb52cfe5af2ff1c45bbb64b12415343c6331de8d0e1bc35517721a7c"
	headlessTreeSHA256AMD64  = "dce26ba2ba02bf7ded86d2a29adacf09da208b4edc27a445dd4bc637c2d18a10"
	headlessTreeSHA256ARM64  = "a44abec4f59895b94d606d517e669dce80bf93907982823e51fa9a3bfa56a16e"
	ffmpegTreeSHA256AMD64    = "7f5e8fab060dd992daa6bf20b23574bbe0dfbe93fee254b98e214ad832e9e577"
	ffmpegTreeSHA256ARM64    = "df2001fc2a9ef8f5a0f159f2f8b03337571087bcb04f255d4b757ed49836fad5"
	browserFFmpegRevision    = "1011"

	maxBrowserScriptBytes        = 128 << 10
	defaultBrowserRunTimeout     = 90 * time.Second
	maxBrowserRunTimeout         = 5 * time.Minute
	browserSetupTimeout          = 20 * time.Minute
	browserCleanupTimeout        = time.Minute
	browserExecutorClientGrace   = 5 * time.Second
	browserStatusTimeout         = 2 * time.Second
	browserSetupMinFreeBytes     = int64(2 << 30)
	browserRunMinFreeBytes       = int64(512 << 20)
	browserRunMaxDisposableBytes = int64(256 << 20)
	browserDiskPressureExit      = 73
	browserLifecycleBusyExit     = 74
)

//go:embed runtime/package.json
var browserPackageJSON string

//go:embed runtime/package-lock.json
var browserPackageLockJSON string

type RuntimeManifest struct {
	SchemaVersion           int    `json:"schema_version"`
	NodeVersion             string `json:"node_version"`
	PlaywrightVersion       string `json:"playwright_version"`
	ChromiumRevision        string `json:"chromium_revision"`
	ChromiumVersion         string `json:"chromium_version"`
	FFmpegRevision          string `json:"ffmpeg_revision"`
	Platform                string `json:"platform"`
	ChromiumTreeSHA256      string `json:"chromium_tree_sha256"`
	HeadlessShellTreeSHA256 string `json:"headless_shell_tree_sha256"`
	FFmpegTreeSHA256        string `json:"ffmpeg_tree_sha256"`
}

type RuntimeStatus struct {
	InspectionComplete bool            `json:"inspection_complete"`
	Installed          bool            `json:"installed"`
	Ready              bool            `json:"ready"`
	Busy               bool            `json:"busy"`
	SharedProfile      bool            `json:"shared_profile"`
	EngineDir          string          `json:"engine_dir"`
	ProfileDir         string          `json:"profile_dir"`
	Desired            RuntimeManifest `json:"desired"`
	InstalledState     RuntimeManifest `json:"installed_state"`
	Reason             string          `json:"reason,omitempty"`
}

type RunOptions struct {
	Script            string
	TimeoutMS         int64
	IgnoreHTTPSErrors bool
}

// Manager owns one optional browser runtime and one Agent-wide persistent
// browser profile. Setup and execution share a fail-fast mutex: there is no
// hidden browser queue, and runtime replacement cannot overlap profile use.
type Manager struct {
	cfg               config.Config
	token             string
	mu                sync.Mutex
	enginePath        string
	dataPath          string
	lifecycleLockPath string
	lifecycleLockStat string
}

func New(cfg config.Config, token string) *Manager { return &Manager{cfg: cfg, token: token} }

func (m *Manager) engineDir() string {
	if m.enginePath != "" {
		return m.enginePath
	}
	return "/opt/ai-server-agent/browser"
}

func (m *Manager) dataDir() string {
	if m.dataPath != "" {
		return m.dataPath
	}
	return filepath.Join(m.cfg.StateDir, "runtime", "browser")
}

func (m *Manager) lifecycleLock() string {
	if m.lifecycleLockPath != "" {
		return m.lifecycleLockPath
	}
	return "/run/lock/ai-server-agent/management.lock"
}

func (m *Manager) lifecycleLockExpectedStat() string {
	if m.lifecycleLockStat != "" {
		return m.lifecycleLockStat
	}
	return "0:0:600"
}

func (m *Manager) lifecycleLockPrelude() string {
	return fmt.Sprintf(`lifecycle_lock=%s
[ -f "$lifecycle_lock" ] && [ ! -L "$lifecycle_lock" ] && [ "$(stat -c '%%u:%%g:%%a' "$lifecycle_lock")" = %s ] || fail "lifecycle lock is unavailable or unsafe"
exec 9<>"$lifecycle_lock"
flock -n 9 || { echo "browser setup resource limit: another Agent lifecycle management operation is active" >&2; exit %d; }`,
		shellQuote(m.lifecycleLock()), shellQuote(m.lifecycleLockExpectedStat()), browserLifecycleBusyExit)
}

func desiredRuntimeManifest() RuntimeManifest {
	manifest := RuntimeManifest{
		SchemaVersion:     1,
		NodeVersion:       browserNodeVersion,
		PlaywrightVersion: browserPlaywrightVersion,
		ChromiumRevision:  browserChromiumRevision,
		ChromiumVersion:   browserChromiumVersion,
		FFmpegRevision:    browserFFmpegRevision,
		Platform:          runtime.GOARCH,
	}
	switch runtime.GOARCH {
	case "amd64":
		manifest.ChromiumTreeSHA256 = chromiumTreeSHA256AMD64
		manifest.HeadlessShellTreeSHA256 = headlessTreeSHA256AMD64
		manifest.FFmpegTreeSHA256 = ffmpegTreeSHA256AMD64
	case "arm64":
		manifest.ChromiumTreeSHA256 = chromiumTreeSHA256ARM64
		manifest.HeadlessShellTreeSHA256 = headlessTreeSHA256ARM64
		manifest.FFmpegTreeSHA256 = ffmpegTreeSHA256ARM64
	}
	return manifest
}

func browserError(code, class, message string) executor.Response {
	return executor.Response{Error: message, ReasonCode: code, ErrorCode: code, ErrorClass: class}
}

func browserBusy(operation string) executor.Response {
	resp := browserError("resource_limit", "resource", operation+" is busy with another browser operation")
	resp.Retryable = true
	resp.Status = "busy"
	return resp
}

func browserUnknown(operation string, err error, retryable bool) executor.Response {
	resp := browserError("unknown_completion", "transport", operation+" completion could not be proven after executor transport ended: "+err.Error())
	resp.Retryable = retryable
	return resp
}

func translateBrowserExecutorResponse(resp executor.Response, operation string) executor.Response {
	if resp.OK {
		return resp
	}
	if resp.ExitCode == browserLifecycleBusyExit {
		resp.Error = operation + " is blocked by another Agent lifecycle management operation"
		resp.ReasonCode = "resource_limit"
		resp.ErrorCode = "resource_limit"
		resp.ErrorClass = "resource"
		resp.Retryable = true
		resp.Status = "busy"
		return resp
	}
	if resp.ExitCode != browserDiskPressureExit {
		return resp
	}
	resp.Error = operation + " stopped because the browser filesystem safety reserve was reached"
	resp.ReasonCode = "resource_limit"
	resp.ErrorCode = "resource_limit"
	resp.ErrorClass = "resource"
	resp.Retryable = true
	resp.Status = "resource_limited"
	return resp
}

func normalizeRunTimeout(ms int64) (time.Duration, error) {
	if ms < 0 {
		return 0, errors.New("timeout_ms must be non-negative")
	}
	if ms == 0 {
		return defaultBrowserRunTimeout, nil
	}
	maxMS := int64(maxBrowserRunTimeout / time.Millisecond)
	if ms > maxMS {
		return 0, fmt.Errorf("timeout_ms exceeds browser maximum of %d", maxMS)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func browserRuntimeExecutablePaths(engine string, desired RuntimeManifest) ([]string, bool) {
	var chromiumRel, headlessRel string
	switch desired.Platform {
	case "amd64":
		chromiumRel = filepath.FromSlash("chrome-linux64/chrome")
		headlessRel = filepath.FromSlash("chrome-headless-shell-linux64/chrome-headless-shell")
	case "arm64":
		chromiumRel = filepath.FromSlash("chrome-linux/chrome")
		headlessRel = filepath.FromSlash("chrome-linux/headless_shell")
	default:
		return nil, false
	}
	return []string{
		filepath.Join(engine, "browsers", "chromium-"+desired.ChromiumRevision, chromiumRel),
		filepath.Join(engine, "browsers", "chromium_headless_shell-"+desired.ChromiumRevision, headlessRel),
		filepath.Join(engine, "browsers", "ffmpeg-"+desired.FFmpegRevision, "ffmpeg-linux"),
	}, true
}

func regularExecutable(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0
}

func (m *Manager) Status(ctx context.Context) RuntimeStatus {
	if !m.mu.TryLock() {
		return RuntimeStatus{
			Busy:          true,
			SharedProfile: true,
			EngineDir:     m.engineDir(),
			ProfileDir:    filepath.Join(m.dataDir(), "profile"),
			Desired:       desiredRuntimeManifest(),
			Reason:        "browser operation in progress; readiness was not inspected",
		}
	}
	defer m.mu.Unlock()
	return m.inspectStatus(ctx, false)
}

func (m *Manager) inspectStatus(ctx context.Context, busy bool) RuntimeStatus {
	desired := desiredRuntimeManifest()
	status := RuntimeStatus{
		InspectionComplete: true,
		Busy:               busy,
		SharedProfile:      true,
		EngineDir:          m.engineDir(),
		ProfileDir:         filepath.Join(m.dataDir(), "profile"),
		Desired:            desired,
	}
	engine := m.engineDir()
	info, err := os.Lstat(engine)
	if err != nil {
		if os.IsNotExist(err) {
			status.Reason = "browser runtime is not installed"
		} else {
			status.Reason = "browser runtime path could not be inspected"
		}
		return status
	}
	status.Installed = true
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		status.Reason = "browser engine path is not a real directory"
		return status
	}
	for _, item := range []struct {
		name string
		path string
	}{
		{name: "data", path: m.dataDir()},
		{name: "profile", path: filepath.Join(m.dataDir(), "profile")},
		{name: "temp", path: filepath.Join(m.dataDir(), "tmp")},
	} {
		fi, err := os.Lstat(item.path)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			status.Reason = "browser " + item.name + " path is missing or unsafe"
			return status
		}
	}

	manifestBytes, err := os.ReadFile(filepath.Join(engine, "runtime-manifest.json"))
	if err != nil {
		status.Reason = "runtime manifest is missing or unreadable"
		return status
	}
	if err := json.Unmarshal(manifestBytes, &status.InstalledState); err != nil {
		status.Reason = "runtime manifest is malformed"
		return status
	}

	nodeCtx, cancel := context.WithTimeout(ctx, browserStatusTimeout)
	cmd := exec.CommandContext(nodeCtx, filepath.Join(engine, "node", "bin", "node"), "--version")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	nodeOut, nodeErr := cmd.Output()
	cancel()
	if nodeErr != nil {
		status.Reason = "installed Node runtime could not report its version"
		return status
	}
	if got := strings.TrimSpace(string(nodeOut)); got != desired.NodeVersion {
		status.InstalledState.NodeVersion = got
		status.Reason = "installed Node version does not match desired runtime"
		return status
	}

	playwrightBytes, err := os.ReadFile(filepath.Join(engine, "node_modules", "playwright", "package.json"))
	if err != nil {
		status.Reason = "installed Playwright package metadata is unavailable"
		return status
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(playwrightBytes, &pkg) != nil || pkg.Version != desired.PlaywrightVersion {
		status.InstalledState.PlaywrightVersion = pkg.Version
		status.Reason = "installed Playwright version does not match desired runtime"
		return status
	}

	browsersBytes, err := os.ReadFile(filepath.Join(engine, "node_modules", "playwright-core", "browsers.json"))
	if err != nil {
		status.Reason = "installed Playwright browser metadata is unavailable"
		return status
	}
	var registry struct {
		Browsers []struct {
			Name           string `json:"name"`
			Revision       string `json:"revision"`
			BrowserVersion string `json:"browserVersion"`
		} `json:"browsers"`
	}
	if json.Unmarshal(browsersBytes, &registry) != nil {
		status.Reason = "installed Playwright browser metadata is malformed"
		return status
	}
	for _, entry := range registry.Browsers {
		switch entry.Name {
		case "chromium":
			status.InstalledState.ChromiumRevision = entry.Revision
			status.InstalledState.ChromiumVersion = entry.BrowserVersion
		case "ffmpeg":
			status.InstalledState.FFmpegRevision = entry.Revision
		}
	}
	if status.InstalledState != desired {
		status.Reason = "installed runtime does not match desired manifest"
		return status
	}
	for _, marker := range []string{
		filepath.Join(engine, "browsers", "chromium-"+desired.ChromiumRevision, "INSTALLATION_COMPLETE"),
		filepath.Join(engine, "browsers", "chromium_headless_shell-"+desired.ChromiumRevision, "INSTALLATION_COMPLETE"),
		filepath.Join(engine, "browsers", "ffmpeg-"+desired.FFmpegRevision, "INSTALLATION_COMPLETE"),
	} {
		if fi, err := os.Lstat(marker); err != nil || !fi.Mode().IsRegular() {
			status.Reason = "required browser runtime artifact is incomplete"
			return status
		}
	}
	executables, ok := browserRuntimeExecutablePaths(engine, desired)
	if !ok {
		status.Reason = "browser runtime platform is unsupported"
		return status
	}
	for _, executable := range executables {
		if !regularExecutable(executable) {
			status.Reason = "required browser runtime executable is missing or not executable"
			return status
		}
	}
	status.Ready = true
	status.Reason = ""
	return status
}

func (m *Manager) Setup(ctx context.Context, approval bool) (executor.Response, error) {
	if !approval {
		return executor.Response{
			Error:      "approval_required",
			ReasonCode: "approval_required",
			ErrorCode:  "approval_required",
			ErrorClass: "approval",
			Approval: map[string]any{
				"category": "host-package-change",
				"reason":   "browser setup downloads a pinned private browser runtime and may install shared OS libraries required by Chromium",
			},
		}, nil
	}
	if !m.mu.TryLock() {
		return browserBusy("browser setup"), nil
	}
	defer m.mu.Unlock()
	correlatedCtx, _, err := executor.WithNewRequestCorrelation(ctx)
	if err != nil {
		return browserUnknown("browser setup correlation", err, false), nil
	}
	ctx = correlatedCtx

	if before := m.inspectStatus(ctx, true); before.Ready {
		clientCtx, cancel := context.WithTimeout(ctx, browserCleanupTimeout+browserExecutorClientGrace)
		defer cancel()
		resp, err := executor.ClientCallContext(clientCtx, m.cfg.ExecutorSocket, m.token, executor.Request{
			Action: "run", AuditAction: "browser_cleanup", Command: m.cleanupCommand(), Root: true, Approval: true,
			TimeoutMS: int64(browserCleanupTimeout / time.Millisecond),
		})
		if err != nil {
			return browserUnknown("browser setup cleanup", err, true), nil
		}
		if !resp.OK {
			return translateBrowserExecutorResponse(resp, "browser setup cleanup"), nil
		}
		resp.Status = "already_current"
		return resp, nil
	}
	command, err := m.setupCommand()
	if err != nil {
		return browserError("browser_setup_unavailable", "state", err.Error()), nil
	}
	clientCtx, cancel := context.WithTimeout(ctx, browserSetupTimeout+browserExecutorClientGrace)
	defer cancel()
	resp, err := executor.ClientCallContext(clientCtx, m.cfg.ExecutorSocket, m.token, executor.Request{
		Action:      "run",
		AuditAction: "browser_setup",
		Command:     command,
		Root:        true,
		Approval:    true,
		TimeoutMS:   int64(browserSetupTimeout / time.Millisecond),
	})
	if err != nil {
		return browserUnknown("browser setup", err, true), nil
	}
	if !resp.OK {
		resp = translateBrowserExecutorResponse(resp, "browser setup")
		if resp.ErrorCode == "resource_limit" {
			return resp, nil
		}
		if after := m.inspectStatus(ctx, true); after.Ready {
			return browserError("unknown_completion", "state", "browser setup reported failure after the desired runtime became active; inspect browser_status before retrying"), nil
		}
		return resp, nil
	}
	if after := m.inspectStatus(ctx, true); !after.Ready {
		return browserError("browser_setup_incomplete", "state", "browser setup returned but runtime verification failed: "+after.Reason), nil
	}
	resp.Status = "installed"
	return resp, nil
}

func (m *Manager) Run(ctx context.Context, opts RunOptions) (executor.Response, error) {
	if len([]byte(opts.Script)) > maxBrowserScriptBytes {
		return browserError("input_too_large", "validation", fmt.Sprintf("browser script exceeds the %d-byte tool limit", maxBrowserScriptBytes)), nil
	}
	timeout, err := normalizeRunTimeout(opts.TimeoutMS)
	if err != nil {
		return browserError("invalid_timeout", "validation", err.Error()), nil
	}
	if !m.mu.TryLock() {
		return browserBusy("browser execution"), nil
	}
	defer m.mu.Unlock()
	if status := m.inspectStatus(ctx, true); !status.Ready {
		return browserError("browser_runtime_not_ready", "state", "browser runtime is not ready: "+status.Reason+"; call browser_setup"), nil
	}
	correlatedCtx, _, err := executor.WithNewRequestCorrelation(ctx)
	if err != nil {
		return browserUnknown("browser execution correlation", err, false), nil
	}
	ctx = correlatedCtx

	clientCtx, cancel := context.WithTimeout(ctx, timeout+browserExecutorClientGrace)
	defer cancel()
	resp, err := executor.ClientCallContext(clientCtx, m.cfg.ExecutorSocket, m.token, executor.Request{
		Action:      "run",
		AuditAction: "browser_run",
		Command:     m.runCommand(opts.Script, opts.IgnoreHTTPSErrors),
		Root:        false,
		TimeoutMS:   int64(timeout / time.Millisecond),
	})
	if err != nil {
		return browserUnknown("browser execution", err, false), nil
	}
	return translateBrowserExecutorResponse(resp, "browser execution"), nil
}

func (m *Manager) cleanupCommand() string {
	return fmt.Sprintf(`set -euo pipefail
fail() { printf 'browser setup: %%s\n' "$*" >&2; exit 2; }
engine=%s
parent=$(dirname "$engine")
data=%s
%s
[ -d "$parent" ] && [ ! -L "$parent" ] || { echo "Browser engine parent is unsafe." >&2; exit 2; }
[ -d "$data" ] && [ ! -L "$data" ] || { echo "Browser data directory is unsafe." >&2; exit 2; }
find "$parent" -maxdepth 1 -type d \( -name '.browser-stage.*' -o -name '.browser-old.*' \) -exec rm -rf -- {} +
[ -d "$data/tmp" ] && [ ! -L "$data/tmp" ] || { echo "Browser temp directory is unsafe." >&2; exit 2; }
find "$data/tmp" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
rm -rf -- "$engine/.npm-cache"`, shellQuote(m.engineDir()), shellQuote(m.dataDir()), m.lifecycleLockPrelude())
}
func (m *Manager) setupCommand() (string, error) {
	desired := desiredRuntimeManifest()
	manifest, err := json.MarshalIndent(desired, "", "  ")
	if err != nil {
		return "", err
	}
	manifest64 := base64.StdEncoding.EncodeToString(append(manifest, '\n'))
	package64 := base64.StdEncoding.EncodeToString([]byte(browserPackageJSON))
	lock64 := base64.StdEncoding.EncodeToString([]byte(browserPackageLockJSON))
	return fmt.Sprintf(`set -euo pipefail
fail() { printf 'browser setup: %%s\n' "$*" >&2; exit 2; }
engine=%s
parent=$(dirname "$engine")
data=%s
worker=%s
node_version=%s
playwright_version=%s
chromium_revision=%s
chromium_version=%s
ffmpeg_revision=%s
chromium_tree_sha=%s
headless_tree_sha=%s
ffmpeg_tree_sha=%s
%s

[ ! -L "$parent" ] || fail "engine parent is a symlink"
install -d -m 0755 -o root -g root "$parent"
if [ -e "$engine" ] || [ -L "$engine" ]; then
  [ -d "$engine" ] && [ ! -L "$engine" ] || fail "engine path is not a real directory"
fi
[ ! -L "$data" ] || fail "Refusing symlinked browser data directory: $data"
[ ! -e "$data" ] || [ -d "$data" ] || fail "browser data path is not a directory"
install -d -m 0755 -o root -g root "$data"
for child in profile tmp; do
  child_path="$data/$child"
  [ ! -L "$child_path" ] || fail "browser $child path is a symlink"
  [ ! -e "$child_path" ] || [ -d "$child_path" ] || fail "browser $child path is not a directory"
done
install -d -m 0700 -o "$worker" -g "$worker" "$data/profile" "$data/tmp"
min_free_kb=%d
free_kb=$(df -Pk "$parent" | awk 'NR == 2 {print $4}')
[ -n "$free_kb" ] && [ "$free_kb" -ge "$min_free_kb" ] || { echo "browser setup resource limit: engine filesystem free space is below safety reserve" >&2; exit %d; }
find "$data/tmp" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
if [ ! -e "$engine" ]; then
  old_count=0
  old_candidate=""
  while IFS= read -r -d '' old; do
    old_count=$((old_count + 1))
    old_candidate="$old"
  done < <(find "$parent" -maxdepth 1 -type d -name '.browser-old.*' -print0)
  case "$old_count" in
    0) ;;
    1) mv -- "$old_candidate" "$engine" ;;
    *) fail "multiple browser runtime backups require operator reconciliation" ;;
  esac
fi
find "$parent" -maxdepth 1 -type d -name '.browser-stage.*' -exec rm -rf -- {} +
if [ -e "$engine" ]; then
  find "$parent" -maxdepth 1 -type d -name '.browser-old.*' -exec rm -rf -- {} +
fi

stage=$(mktemp -d "$parent/.browser-stage.XXXXXX")
backup=""
cleanup() {
  if [ -n "$stage" ] && [ -d "$stage" ]; then rm -rf -- "$stage"; fi
  if [ -n "$backup" ] && [ -d "$backup" ] && [ ! -e "$engine" ]; then mv -- "$backup" "$engine"; fi
}
trap cleanup EXIT

arch=$(uname -m)
case "$arch" in
  x86_64)
    node_arch=x64
    node_sha=%s
    chromium_exec_rel=chrome-linux64/chrome
    headless_exec_rel=chrome-headless-shell-linux64/chrome-headless-shell
    ;;
  aarch64|arm64)
    node_arch=arm64
    node_sha=%s
    chromium_exec_rel=chrome-linux/chrome
    headless_exec_rel=chrome-linux/headless_shell
    ;;
  *) fail "unsupported architecture: $arch" ;;
esac
asset="node-$node_version-linux-$node_arch.tar.xz"
base="https://nodejs.org/dist/$node_version"
curl -fsSLo "$stage/$asset" "$base/$asset"
printf '%%s  %%s\n' "$node_sha" "$stage/$asset" | sha256sum -c -
mkdir "$stage/node"
tar -xJf "$stage/$asset" -C "$stage/node" --strip-components=1
rm -f -- "$stage/$asset"

printf '%%s' %s | base64 -d > "$stage/package.json"
printf '%%s' %s | base64 -d > "$stage/package-lock.json"
install -d -m 0700 "$stage/.home" "$stage/.npm-cache"
export HOME="$stage/.home"
export PATH="$stage/node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export npm_config_cache="$stage/.npm-cache"
export PLAYWRIGHT_BROWSERS_PATH="$stage/browsers"
cd "$stage"
npm ci --ignore-scripts --no-audit --no-fund
"$stage/node/bin/node" "$stage/node_modules/playwright/cli.js" install chromium
DEBIAN_FRONTEND=noninteractive "$stage/node/bin/node" "$stage/node_modules/playwright/cli.js" install-deps chromium

[ "$("$stage/node/bin/node" --version)" = "$node_version" ] || fail "Node version verification failed"
[ "$("$stage/node/bin/node" -p "require('$stage/node_modules/playwright/package.json').version")" = "$playwright_version" ] || fail "Playwright version verification failed"
browser_meta=$("$stage/node/bin/node" - "$stage" <<'NODE'
const path = require('path');
const root = process.argv[2];
const registry = require(path.join(root, 'node_modules/playwright-core/browsers.json'));
const byName = name => registry.browsers.find(x => x.name === name) || {};
const chromium = byName('chromium');
const ffmpeg = byName('ffmpeg');
const lock = require(path.join(root, 'package-lock.json'));
const pw = lock.packages['node_modules/playwright'] || {};
const core = lock.packages['node_modules/playwright-core'] || {};
if (pw.version !== '%s' || core.version !== '%s' || !pw.integrity || !core.integrity) process.exit(3);
process.stdout.write([chromium.revision || '', chromium.browserVersion || '', ffmpeg.revision || ''].join('|'));
NODE
)
[ "$browser_meta" = "$chromium_revision|$chromium_version|$ffmpeg_revision" ] || fail "browser build metadata verification failed"
[ -f "$stage/browsers/chromium-$chromium_revision/INSTALLATION_COMPLETE" ] || fail "Chromium installation marker missing"
[ -f "$stage/browsers/chromium_headless_shell-$chromium_revision/INSTALLATION_COMPLETE" ] || fail "Chromium headless-shell marker missing"
[ -f "$stage/browsers/ffmpeg-$ffmpeg_revision/INSTALLATION_COMPLETE" ] || fail "FFmpeg installation marker missing"
for executable in   "$stage/browsers/chromium-$chromium_revision/$chromium_exec_rel"   "$stage/browsers/chromium_headless_shell-$chromium_revision/$headless_exec_rel"   "$stage/browsers/ffmpeg-$ffmpeg_revision/ffmpeg-linux"
do
  [ -f "$executable" ] && [ ! -L "$executable" ] && [ -x "$executable" ] || fail "browser runtime executable verification failed"
done
tree_hash() {
  root=$1
  (
    cd "$root"
    find . -type f -print0 | LC_ALL=C sort -z | while IFS= read -r -d '' f; do
      printf '%%s\0' "$f"
      sha256sum "$f" | awk '{print $1}'
    done
  ) | sha256sum | awk '{print $1}'
}
[ "$(tree_hash "$stage/browsers/chromium-$chromium_revision")" = "$chromium_tree_sha" ] || fail "Chromium content-tree integrity verification failed"
[ "$(tree_hash "$stage/browsers/chromium_headless_shell-$chromium_revision")" = "$headless_tree_sha" ] || fail "Chromium headless-shell content-tree integrity verification failed"
[ "$(tree_hash "$stage/browsers/ffmpeg-$ffmpeg_revision")" = "$ffmpeg_tree_sha" ] || fail "FFmpeg content-tree integrity verification failed"
printf '%%s' %s | base64 -d > "$stage/runtime-manifest.json"
rm -rf -- "$stage/.npm-cache" "$stage/.home"
chown -R root:root "$stage"
( engine="$stage"; %s )

if [ -e "$engine" ]; then
  backup=$(mktemp -d "$parent/.browser-old.XXXXXX")
  rmdir "$backup"
  mv -- "$engine" "$backup"
fi
if ! mv -- "$stage" "$engine"; then
  if [ -n "$backup" ] && [ -d "$backup" ] && [ ! -e "$engine" ]; then mv -- "$backup" "$engine"; fi
  fail "activate verified browser runtime"
fi
stage=""
if [ -n "$backup" ]; then rm -rf -- "$backup"; backup=""; fi
trap - EXIT
printf 'browser runtime converged: node=%%s playwright=%%s chromium=%%s/%%s\n' "$node_version" "$playwright_version" "$chromium_revision" "$chromium_version"`,
		shellQuote(m.engineDir()),
		shellQuote(m.dataDir()),
		shellQuote(m.cfg.WorkerUser),
		shellQuote(browserNodeVersion),
		shellQuote(browserPlaywrightVersion),
		shellQuote(browserChromiumRevision),
		shellQuote(browserChromiumVersion),
		shellQuote(browserFFmpegRevision),
		shellQuote(desired.ChromiumTreeSHA256),
		shellQuote(desired.HeadlessShellTreeSHA256),
		shellQuote(desired.FFmpegTreeSHA256),
		m.lifecycleLockPrelude(),
		browserSetupMinFreeBytes/1024,
		browserDiskPressureExit,
		shellQuote(browserNodeSHA256X64),
		shellQuote(browserNodeSHA256ARM64),
		shellQuote(package64),
		shellQuote(lock64),
		browserPlaywrightVersion,
		browserPlaywrightVersion,
		shellQuote(manifest64),
		browserEnginePermissionsCommand,
	), nil
}
func (m *Manager) runCommand(script string, ignoreHTTPSErrors bool) string {
	payload := base64.StdEncoding.EncodeToString([]byte(script))
	ignoreHTTPS := "false"
	if ignoreHTTPSErrors {
		ignoreHTTPS = "true"
	}
	return fmt.Sprintf(`set -euo pipefail
engine=%s
data=%s
profile="$data/profile"
ignore_https_errors=%s
[ -x "$engine/node/bin/node" ] || { echo "Browser runtime is not installed. Call browser_setup first." >&2; exit 2; }
export PLAYWRIGHT_BROWSERS_PATH="$engine/browsers"
install -d -m 0700 "$data/tmp"
min_free_kb=%d
disposable_limit_kb=%d
free_kb=$(df -Pk "$data" | awk 'NR == 2 {print $4}')
[ -n "$free_kb" ] && [ "$free_kb" -ge "$min_free_kb" ] || { echo "browser resource limit: state filesystem free space is below safety reserve" >&2; exit %d; }
find "$data/tmp" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
cleanup_cache() {
  rm -rf --     "$data/profile/Default/Cache"     "$data/profile/Default/Code Cache"     "$data/profile/Default/GPUCache"     "$data/profile/GPUCache"     "$data/profile/ShaderCache"     "$data/profile/GrShaderCache"     "$data/profile/GraphiteDawnCache"     "$data/profile/DawnGraphiteCache"     "$data/profile/DawnWebGPUCache"
}
cleanup_cache
run_tmp=$(mktemp -d "$data/tmp/run.XXXXXX")
body="$run_tmp/body.js"
runner="$run_tmp/runner.mjs"
downloads="$run_tmp/downloads"
mkdir "$downloads"
cleanup() { rm -rf -- "$run_tmp"; cleanup_cache || true; }
trap cleanup EXIT
printf '%%s' %s | base64 -d > "$body"
{
  printf '%%s
' "import { chromium } from 'file://$engine/node_modules/playwright/index.mjs';"
  printf '%%s
' "const context = await chromium.launchPersistentContext(process.env.AI_SERVER_AGENT_BROWSER_PROFILE, {headless:true, ignoreHTTPSErrors: $ignore_https_errors, downloadsPath: process.env.AI_SERVER_AGENT_BROWSER_DOWNLOADS, args:['--disk-cache-size=67108864','--media-cache-size=33554432']});"
  printf '%%s
' "const browser = context.browser();"
  printf '%%s
' "const pages = context.pages();"
  printf '%%s
' "const page = pages[0] || await context.newPage();"
  printf '%%s
' "try {"
  cat "$body"
  printf '%%s
' "} finally { await context.close(); }"
} > "$runner"

export AI_SERVER_AGENT_BROWSER_PROFILE="$profile"
export AI_SERVER_AGENT_BROWSER_DOWNLOADS="$downloads"
cd "$run_tmp"
"$engine/node/bin/node" "$runner" &
node_pid=$!
limit_reason=""
check_limits() {
  free_kb=$(df -Pk "$data" | awk 'NR == 2 {print $4}') || free_kb=""
  used_kb=$(du -sk --apparent-size "$run_tmp" | awk 'NR == 1 {print $1}') || used_kb=""
  if [ -z "$free_kb" ] || [ "$free_kb" -lt "$min_free_kb" ]; then
    limit_reason="state filesystem safety reserve reached"
    return
  fi
  if [ -z "$used_kb" ] || [ "$used_kb" -gt "$disposable_limit_kb" ]; then
    limit_reason="disposable browser run data exceeded its bound"
  fi
}
while kill -0 "$node_pid" 2>/dev/null; do
  check_limits
  [ -z "$limit_reason" ] || break
  sleep 1
done
[ -n "$limit_reason" ] || check_limits
if [ -n "$limit_reason" ]; then
  printf 'browser resource limit: %%s
' "$limit_reason" >&2
  kill -TERM "$node_pid" 2>/dev/null || true
  i=0
  while kill -0 "$node_pid" 2>/dev/null && [ "$i" -lt 20 ]; do
    sleep 0.1
    i=$((i + 1))
  done
  kill -KILL "$node_pid" 2>/dev/null || true
  wait "$node_pid" 2>/dev/null || true
  exit %d
fi

set +e
wait "$node_pid"
node_code=$?
set -e
# Reserve wrapper exit codes so arbitrary user JavaScript cannot masquerade as
# a browser resource/lifecycle outcome merely by calling process.exit().
case "$node_code" in
  %d|%d) exit 1 ;;
  *) exit "$node_code" ;;
esac`,
		shellQuote(m.engineDir()),
		shellQuote(m.dataDir()),
		ignoreHTTPS,
		browserRunMinFreeBytes/1024,
		browserRunMaxDisposableBytes/1024,
		browserDiskPressureExit,
		shellQuote(payload),
		browserDiskPressureExit,
		browserDiskPressureExit,
		browserLifecycleBusyExit,
	)
}
