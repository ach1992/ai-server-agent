package browser

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
)

const browserEnginePermissionsCommand = `chmod -R a+rX,go-w "$engine"`

// Manager owns an optional browser runtime. The browser engine is root-owned
// under /opt so untrusted project code running as aiworker cannot replace code
// later executed during privileged browser maintenance. Writable browser data
// stays under the agent state directory. The MCP core has no browser or Node.js
// dependency.
type Manager struct {
	cfg   config.Config
	token string
	mu    sync.Mutex
}

func New(cfg config.Config, token string) *Manager { return &Manager{cfg: cfg, token: token} }
func (m *Manager) engineDir() string               { return "/opt/ai-server-agent/browser" }
func (m *Manager) dataDir() string                 { return filepath.Join(m.cfg.StateDir, "runtime", "browser") }

func (m *Manager) Setup(approval bool) (executor.Response, error) {
	if !approval {
		return executor.Response{OK: false, Error: "approval_required", Approval: map[string]any{"category": "host-package-change", "reason": "browser setup downloads a private browser runtime and installs the shared OS libraries required by Chromium"}}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	engine := m.engineDir()
	data := m.dataDir()
	cmd := fmt.Sprintf(`set -euo pipefail
engine=%[1]q
data=%[2]q
worker=%[3]q
install -d -m 0755 -o root -g root "$engine"
[ ! -L "$data" ] || { echo "Refusing symlinked browser data directory: $data" >&2; exit 2; }
[ ! -e "$data" ] || [ -d "$data" ] || { echo "Browser data path is not a directory: $data" >&2; exit 2; }
install -d -m 0750 -o "$worker" -g "$worker" "$data"
cd "$engine"
arch=$(uname -m)
case "$arch" in x86_64) na=x64 ;; aarch64|arm64) na=arm64 ;; *) echo "Unsupported architecture: $arch" >&2; exit 2 ;; esac
NODE_VERSION=v24.18.1
asset="node-${NODE_VERSION}-linux-${na}.tar.xz"
base="https://nodejs.org/dist/${NODE_VERSION}"
if [ ! -x node/bin/node ]; then
  curl -fsSLo "$asset" "$base/$asset"
  curl -fsSLo SHASUMS256.txt "$base/SHASUMS256.txt"
  grep "  ${asset}$" SHASUMS256.txt | sha256sum -c -
  rm -rf node.tmp && mkdir node.tmp
  tar -xJf "$asset" -C node.tmp --strip-components=1
  rm -f "$asset" SHASUMS256.txt
  mv node.tmp node
fi
export PATH="$engine/node/bin:$PATH"
export npm_config_cache="$engine/.npm-cache"
export PLAYWRIGHT_BROWSERS_PATH="$engine/browsers"
if [ ! -f package.json ]; then npm init -y >/dev/null 2>&1; fi
npm install --ignore-scripts --no-audit --no-fund playwright@1.61.1
npx playwright install chromium
npx playwright install-deps chromium
chown -R root:root "$engine"
%[4]s
printf 'browser runtime installed in %%s\n' "$engine"`, engine, data, m.cfg.WorkerUser, browserEnginePermissionsCommand)
	return executor.ClientCall(m.cfg.ExecutorSocket, m.token, executor.Request{Action: "run", Command: cmd, Root: true, Approval: true})
}

func (m *Manager) Run(script string) (executor.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	engine := m.engineDir()
	data := m.dataDir()
	payload := base64.StdEncoding.EncodeToString([]byte(script))
	cmd := fmt.Sprintf(`set -euo pipefail
engine=%[1]q
data=%[2]q
[ -x "$engine/node/bin/node" ] || { echo "Browser runtime is not installed. Call browser_setup first." >&2; exit 2; }
cd "$data"
export PLAYWRIGHT_BROWSERS_PATH="$engine/browsers"
body=$(mktemp .ai-body.XXXXXX.js)
runner=$(mktemp .ai-script.XXXXXX.mjs)
trap 'rm -f "$body" "$runner"' EXIT
printf '%%s' %[3]q | base64 -d > "$body"
{
  printf '%%s\n' "import { chromium } from 'file://$engine/node_modules/playwright/index.mjs';"
  printf '%%s\n' "const context = await chromium.launchPersistentContext('./profile', {headless:true, ignoreHTTPSErrors:true});"
  printf '%%s\n' "const browser = context.browser();"
  printf '%%s\n' "const pages = context.pages();"
  printf '%%s\n' "const page = pages[0] || await context.newPage();"
  printf '%%s\n' "try {"
  cat "$body"
  printf '%%s\n' "} finally { await context.close(); }"
} > "$runner"
"$engine/node/bin/node" "$runner"`, engine, data, payload)
	return executor.ClientCall(m.cfg.ExecutorSocket, m.token, executor.Request{Action: "run", Command: cmd, Root: false})
}
