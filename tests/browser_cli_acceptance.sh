#!/usr/bin/env bash
# Opt-in, unprivileged acceptance for the CLI bundled with the pinned browser runtime.
# Never installs tooling, contacts a site, binds a port, or touches the Agent profile.
set -euo pipefail

if [[ "${AI_SERVER_AGENT_BROWSER_CLI_ACCEPTANCE:-}" != 1 ]]; then
  echo "SKIP: set AI_SERVER_AGENT_BROWSER_CLI_ACCEPTANCE=1 after browser_status reports ready"
  exit 0
fi

umask 077
engine="${AI_SERVER_AGENT_BROWSER_ENGINE:-/opt/ai-server-agent/browser}"
node="$engine/node/bin/node"
cli="$engine/node_modules/playwright/cli.js"
manifest="$engine/runtime-manifest.json"

for required in "$node" "$cli" "$manifest"; do
  [[ -f "$required" && ! -L "$required" ]] || { echo "Missing/unsafe pinned runtime file: $required" >&2; exit 1; }
done
[[ -x "$node" ]] || { echo "Pinned Node is not executable" >&2; exit 1; }

revision=$("$node" -e 'const v=require(process.argv[1]).chromium_revision; if(!/^\d+$/.test(v||"")) process.exit(2); process.stdout.write(v)' "$manifest")
case "$(uname -m)" in
  x86_64) chromium="$engine/browsers/chromium-$revision/chrome-linux64/chrome" ;;
  aarch64|arm64) chromium="$engine/browsers/chromium-$revision/chrome-linux/chrome" ;;
  *) echo "Unsupported browser acceptance architecture" >&2; exit 1 ;;
esac
[[ -f "$chromium" && ! -L "$chromium" && -x "$chromium" ]] || {
  echo "Pinned Chromium executable is missing/unsafe" >&2
  exit 1
}

scratch=$(mktemp -d "${TMPDIR:-/tmp}/asa-browser-cli.XXXXXX")
mkdir -m 700 "$scratch/home" "$scratch/cache" "$scratch/config" "$scratch/state" "$scratch/run"
export HOME="$scratch/home"
export XDG_CACHE_HOME="$scratch/cache"
export XDG_CONFIG_HOME="$scratch/config"
export XDG_STATE_HOME="$scratch/state"
export XDG_RUNTIME_DIR="$scratch/run"
export PLAYWRIGHT_BROWSERS_PATH="$engine/browsers"
session="asa-${scratch##*.}"
run_cli() { "$node" "$cli" cli "-s=$session" "$@"; }
cleanup() {
  run_cli close >/dev/null 2>&1 || true
  rm -rf -- "$scratch"
}
trap cleanup EXIT
cd "$scratch"

# Explicitly choose the already verified private Chromium: CLI's bare default
# is system Google Chrome, which does not exist on many supported Agent hosts.
printf '{"browser":{"browserName":"chromium","launchOptions":{"executablePath":"%s","headless":true}},"outputDir":"%s/output"}\n' \
  "$chromium" "$scratch" > "$scratch/config.json"
cat > "$scratch/setup.js" <<'JS'
async page => {
  await page.setContent('<label for="name">Name</label><input id="name"><button id="submit">Submit</button><p id="status">Waiting</p>');
  await page.evaluate(() => {
    document.querySelector('#submit').addEventListener('click', () => {
      document.querySelector('#status').textContent = document.querySelector('#name').value;
      console.log('submitted');
    });
  });
}
JS

opened=$(run_cli open about:blank "--config=$scratch/config.json")
[[ "$opened" == *"opened with pid"* ]] || { echo "Playwright CLI failed to open" >&2; exit 1; }
setup=$(run_cli run-code "--filename=$scratch/setup.js")
[[ "$setup" == *"Ran Playwright code"* && "$setup" != *"### Error"* ]] || {
  echo "Playwright CLI failed to prepare local test DOM" >&2
  exit 1
}
snapshot=$(run_cli snapshot)
(( ${#snapshot} < 16384 )) || { echo "Unexpectedly large CLI snapshot" >&2; exit 1; }
input_ref=$(printf '%s\n' "$snapshot" | sed -nE 's/.*textbox "Name" \[ref=(e[0-9]+)\].*/\1/p')
button_ref=$(printf '%s\n' "$snapshot" | sed -nE 's/.*button "Submit" \[ref=(e[0-9]+)\].*/\1/p')
[[ "$input_ref" =~ ^e[0-9]+$ && "$button_ref" =~ ^e[0-9]+$ ]] || {
  echo "Playwright CLI snapshot did not expose usable element refs" >&2
  exit 1
}

trace_start=$(run_cli tracing-start)
[[ "$trace_start" == *"Trace recording started"* ]] || { echo "CLI trace start failed" >&2; exit 1; }
fill=$(run_cli fill "$input_ref" "ACh")
[[ "$fill" == *"Ran Playwright code"* && "$fill" != *"### Error"* ]] || { echo "CLI fill failed" >&2; exit 1; }
click=$(run_cli click "$button_ref")
[[ "$click" == *"Ran Playwright code"* && "$click" != *"### Error"* ]] || { echo "CLI click failed" >&2; exit 1; }
assertion=$(run_cli run-code 'async page => { return await page.locator("#status").textContent(); }')
[[ "$assertion" == *'"ACh"'* && "$assertion" != *"### Error"* ]] || {
  echo "CLI click/fill assertion failed" >&2
  exit 1
}
console_result=$(run_cli console)
[[ "$console_result" == *"submitted"* ]] || { echo "CLI console inspection failed" >&2; exit 1; }
network_result=$(run_cli requests)
[[ "$network_result" == *"### Result"* ]] || { echo "CLI network inspection failed" >&2; exit 1; }
trace_stop=$(run_cli tracing-stop)
[[ "$trace_stop" == *"Trace recording stopped"* ]] || { echo "CLI trace stop failed" >&2; exit 1; }
[[ -d "$scratch/output/traces" ]] || { echo "CLI trace artifacts missing" >&2; exit 1; }
(( $(du -sk "$scratch/output" | awk '{print $1}') <= 16384 )) || {
  echo "Local fixture artifacts exceed the acceptance budget" >&2
  exit 1
}
echo "PASS: pinned Chromium CLI open/snapshot/ref/fill/click/assert/console/network/trace; private scratch; no external URL"
