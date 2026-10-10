#!/usr/bin/env bash
# Pinned real-browser acceptance on disposable GitHub-hosted CI only.
# Never run this on an installed Agent host or a production server.
set -euo pipefail
if [[ "${CI:-}" != true || -z "${RUNNER_TEMP:-}" || -z "${GITHUB_ACTIONS:-}" ]]; then
  echo "Refusing browser provisioning outside disposable GitHub Actions CI" >&2
  exit 2
fi
cd "$(dirname "$0")/.."
read -r node_version node_sha playwright_version chromium_revision < <(python3 - <<'PY'
import re
from pathlib import Path
source = Path('internal/browser/browser.go').read_text()
keys = ['browserNodeVersion','browserNodeSHA256X64',
        'browserPlaywrightVersion','browserChromiumRevision']
values = []
for key in keys:
    m = re.search(r'^\s*'+key+r'\s*=\s*"([^"]+)"', source, flags=re.MULTILINE)
    if m is None:
        raise SystemExit('Missing pinned browser identity: '+key)
    values.append(m.group(1))
print(' '.join(values))
PY
)
root="$(mktemp -d "$RUNNER_TEMP/asa-browser-e2e.XXXXXX")"
trap 'rm -rf -- "$root"' EXIT
mkdir -m 700 "$root/node" "$root/browsers"
cp internal/browser/runtime/package.json internal/browser/runtime/package-lock.json "$root/"
node_archive="node-$node_version-linux-x64.tar.xz"
curl --fail --location --silent --show-error --retry 2 \
  "https://nodejs.org/dist/$node_version/$node_archive" -o "$root/$node_archive"
printf '%s  %s\n' "$node_sha" "$root/$node_archive" | sha256sum --check --status
tar -xJf "$root/$node_archive" -C "$root/node" --strip-components=1
export PATH="$root/node/bin:$PATH"
export PLAYWRIGHT_BROWSERS_PATH="$root/browsers"
test "$(node --version)" = "$node_version"
node - "$root/package-lock.json" "$playwright_version" <<'JS'
const fs=require('node:fs');
const lock=JSON.parse(fs.readFileSync(process.argv[2],'utf8'));
const desired=process.argv[3];
for (const name of ['node_modules/playwright','node_modules/playwright-core']) {
 const entry=lock.packages[name];
 if (!entry || entry.version!==desired || !entry.integrity) {
   throw new Error('Playwright lock identity mismatch: '+name);
 }
}
JS
npm ci --prefix "$root" --ignore-scripts --no-audit --no-fund
"$root/node/bin/node" "$root/node_modules/playwright/cli.js" install --with-deps chromium
test -x "$root/browsers/chromium-$chromium_revision/chrome-linux64/chrome"
printf 'EXACT_BROWSER_ACCEPTANCE_HEAD=%s\n' "$(git rev-parse HEAD)"
printf 'PINNED_RUNTIME node=%s playwright=%s chromium_revision=%s\n' "$node_version" "$playwright_version" "$chromium_revision"
log="$root/browser-e2e-acceptance.log"
AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME="$root" \
  go test ./internal/browser -run '^TestBrowserFlow(PinnedRuntimeAcceptance|ReviewAcceptance)$' -v -count=1 | tee "$log"
printf 'PINNED_BROWSER_ACCEPTANCE_LOG_SHA256='
sha256sum "$log" | awk '{print $1}'
