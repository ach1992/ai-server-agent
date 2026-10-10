package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
)

// managedSessionRunner creates the bounded Node worker body to be owned by
// the existing executor session broker. No daemon, public port or second
// Chromium runtime/profile is created. A Browser session is not a project
// artifact; its entire state dies with this child process.
func managedSessionRunner(engine, profile, downloads string, ignoreHTTPS bool) (string, error) {
	if !filepath.IsAbs(engine) || !filepath.IsAbs(profile) || !filepath.IsAbs(downloads) ||
		filepath.Clean(engine) != engine || filepath.Clean(profile) != profile || filepath.Clean(downloads) != downloads {
		return "", errors.New("managed Browser paths must be clean absolute paths")
	}
	engineURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(engine, "node_modules/playwright/index.mjs"))}).String()
	items := make([]string, 0, 5)
	for _, value := range []string{engineURL, profile, downloads, filepath.Join(engine, "browsers"), browserFlowJS} {
		raw, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		items = append(items, string(raw))
	}
	return fmt.Sprintf(`import { createInterface } from 'node:readline';
import { statfsSync } from 'node:fs';
process.env.PLAYWRIGHT_BROWSERS_PATH = %s;
const { chromium } = await import(%s);
const profile = %s;
const downloads = %s;
const flowTemplate = %s;
const state = { refs: new Map(), sequence: 0 };
globalThis.__asaManagedFlowState = state;
const reserve = 512n * 1024n * 1024n;
const diskSafe = () => {
  try {
    const st = statfsSync(downloads, { bigint:true });
    return st.bavail * st.bsize >= reserve;
  } catch { return false; }
};
if (!diskSafe()) throw new Error('browser_session_disk_reserve_unavailable');
const context = await chromium.launchPersistentContext(profile, {
  headless: true, ignoreHTTPSErrors: %t, acceptDownloads: false, downloadsPath: downloads,
  args: ['--disk-cache-size=67108864', '--media-cache-size=33554432']
});
const browser = context.browser();
const page = context.pages()[0] || await context.newPage();
// Bounded persistent sessions need the same filesystem safety reserve as
// browser_run, including while a page is busy between client calls.
let diskPressure = false;
const diskMonitor = setInterval(() => {
  if (!diskPressure && !diskSafe()) {
    diskPressure = true;
    clearInterval(diskMonitor);
    void context.close().finally(() => process.exit(73));
  }
}, 1000);
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
const reply = data => process.stdout.write('ASA_BROWSER_SESSION ' + JSON.stringify(data) + '\n');
reply({ event: 'ready' });
try {
  let commands = 0;
  for await (const line of createInterface({input: process.stdin, crlfDelay: Infinity})) {
    if (++commands > 128 || Buffer.byteLength(line, 'utf8') > 16384) {
      reply({ event: 'error', reason: 'session_input_limit' }); break;
    }
    let input;
    try { input = JSON.parse(line); } catch { reply({event:'error',reason:'invalid_request'}); break; }
    if (input?.type === 'close') break;
    if (input?.type !== 'flow' || !Array.isArray(input.steps) || input.steps.length < 1 || input.steps.length > 12 ||
        typeof input.nonce !== 'string' || !/^[0-9a-f]{32}$/.test(input.nonce)) {
      reply({event:'error',reason:'invalid_request'}); break;
    }
    const body = flowTemplate.replace('__ASA_FLOW_STEPS_JSON__', JSON.stringify(input.steps));
    if (body.length > 131072) { reply({event:'error',reason:'input_too_large'}); break; }
    const emitted = [];
    const sink = {log(value) { if (typeof value === 'string' && value.startsWith('ASA_BROWSER_E2E_RESULT '))
      emitted.push(value.slice('ASA_BROWSER_E2E_RESULT '.length)); }};
    try {
      await new AsyncFunction('page','context','browser','console',body)(page,context,browser,sink);
      if (emitted.length !== 1 || Buffer.byteLength(emitted[0],'utf8') > 16000)
        throw new Error('invalid_output');
      reply({event:'result',nonce:input.nonce,result:JSON.parse(emitted[0])});
    } catch { reply({event:'error',nonce:input.nonce,reason:'flow_failed'}); break; }
  }
} finally {
  clearInterval(diskMonitor);
  await context.close();
  process.exit(0);
}
`, items[3], items[0], items[1], items[2], items[4], ignoreHTTPS), nil
}
