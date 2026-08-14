package browser

import (
	"encoding/base64"
	"fmt"
	"path/filepath"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
)

// Manager owns an optional browser runtime isolated below StateDir. The core
// control plane has no Node.js or browser dependency.
type Manager struct {
	cfg   config.Config
	token string
}

func New(cfg config.Config, token string) *Manager { return &Manager{cfg: cfg, token: token} }
func (m *Manager) runtimeDir() string              { return filepath.Join(m.cfg.StateDir, "runtime", "browser") }

func (m *Manager) Setup(approval bool) (executor.Response, error) {
	if !approval {
		return executor.Response{OK: false, Error: "approval_required", Approval: map[string]any{"category": "host-package-change", "reason": "browser setup downloads a private browser runtime and may install OS packages"}}, nil
	}
	d := m.runtimeDir()
	workerSetup := fmt.Sprintf(`set -euo pipefail
mkdir -p %[1]q
cd %[1]q
arch=$(uname -m)
case "$arch" in x86_64) na=x64 ;; aarch64|arm64) na=arm64 ;; *) echo "Unsupported architecture: $arch" >&2; exit 2 ;; esac
NODE_VERSION=v24.18.0
if [ ! -x node/bin/node ]; then
  curl -fsSLo node.tar.xz "https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-${na}.tar.xz"
  rm -rf node.tmp && mkdir node.tmp
  tar -xJf node.tar.xz -C node.tmp --strip-components=1
  rm -f node.tar.xz && mv node.tmp node
fi
export PATH="$PWD/node/bin:$PATH"
if [ ! -f package.json ]; then npm init -y >/dev/null 2>&1; fi
npm install --no-audit --no-fund playwright@1.61.1
PLAYWRIGHT_BROWSERS_PATH="$PWD/browsers" npx playwright install chromium
printf 'browser runtime downloaded\n'`, d)
	first, err := executor.ClientCall(m.cfg.ExecutorSocket, m.token, executor.Request{Action: "run", Command: workerSetup, Root: false, Approval: approval})
	if err != nil || !first.OK {
		return first, err
	}

	// Playwright's OS dependencies are installed only when browser support is
	// explicitly requested. They are shared libraries, not long-running host
	// services, and the web stack remains untouched.
	deps := fmt.Sprintf(`set -euo pipefail; cd %q; export PATH="$PWD/node/bin:$PATH"; export PLAYWRIGHT_BROWSERS_PATH="$PWD/browsers"; npx playwright install-deps chromium`, d)
	second, err := executor.ClientCall(m.cfg.ExecutorSocket, m.token, executor.Request{Action: "run", Command: deps, Root: true, Approval: approval})
	if err != nil || !second.OK {
		return second, err
	}
	second.Output = first.Output + "\n" + second.Output
	return second, nil
}

func (m *Manager) Run(script string) (executor.Response, error) {
	d := m.runtimeDir()
	payload := base64.StdEncoding.EncodeToString([]byte(script))
	cmd := fmt.Sprintf(`set -euo pipefail
cd %[1]q
export PATH="$PWD/node/bin:$PATH"
export PLAYWRIGHT_BROWSERS_PATH="$PWD/browsers"
body=$(mktemp .ai-body.XXXXXX.js)
runner=$(mktemp .ai-script.XXXXXX.mjs)
trap 'rm -f "$body" "$runner"' EXIT
printf '%%s' %[2]q | base64 -d > "$body"
{
  printf '%%s\n' "import { chromium } from 'playwright';"
  printf '%%s\n' "const context = await chromium.launchPersistentContext('./profile', {headless:true, ignoreHTTPSErrors:true});"
  printf '%%s\n' "const browser = context.browser();"
  printf '%%s\n' "const pages = context.pages();"
  printf '%%s\n' "const page = pages[0] || await context.newPage();"
  printf '%%s\n' "try {"
  cat "$body"
  printf '%%s\n' "} finally { await context.close(); }"
} > "$runner"
node "$runner"`, d, payload)
	return executor.ClientCall(m.cfg.ExecutorSocket, m.token, executor.Request{Action: "run", Command: cmd, Root: false})
}
