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
import { createHash } from 'node:crypto';
import { statfsSync, mkdtempSync, rmSync, openSync, fstatSync, readSync, closeSync, constants, readdirSync } from 'node:fs';
import { join } from 'node:path';
process.env.PLAYWRIGHT_BROWSERS_PATH = %s;
const { chromium } = await import(%s);
const profile = %s;
const downloads = %s;
const flowTemplate = %s;
const state = { refs: new Map(), sequence: 0 };
// One bounded, short-lived, per-session trace slot. The artifact's bytes
// are only held in this worker's memory after Playwright finalizes its ZIP.
let traceData = null;
let traceDigest = '';
const traceMaxBytes = 512 * 1024;
const traceReadBytes = 8192;
globalThis.__asaManagedFlowState = state;
const reserve = 512n * 1024n * 1024n;
const diskSafe = () => {
  try {
    const st = statfsSync(downloads, { bigint:true });
    return st.bavail * st.bsize >= reserve;
  } catch { return false; }
};
if (!diskSafe()) throw new Error('browser_session_disk_reserve_unavailable');
// At session open, the executor's exclusive Browser profile lease proves
// no prior managed worker is alive. Reclaim only our own randomly named
// abandoned trace directories, including those from SIGKILL/crash. Never
// traverse an arbitrary browser profile/worktree or follow a symlink.
for (const entry of readdirSync(downloads, {withFileTypes:true})) {
  if (!entry.isDirectory() || !/^asa-trace-[A-Za-z0-9]{6}$/.test(entry.name)) continue;
  rmSync(join(downloads,entry.name),{recursive:true,force:true});
}

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
    // Screenshot is an explicit, bounded image consumer. Frames stay below
    // the existing 64 KiB broker event ring; no image is saved to disk,
    // no page bytes enter audit, and the profile remains session-owned.
    if (input?.type === 'capture' && typeof input.nonce === 'string' && /^[0-9a-f]{32}$/.test(input.nonce) &&
        Number.isInteger(input.quality) && input.quality >= 15 && input.quality <= 70 &&
        Number.isInteger(input.max_width) && input.max_width >= 320 && input.max_width <= 1024) {
      try {
        const vp = page.viewportSize();
        if (!vp || vp.width < 1 || vp.height < 1) throw Error('viewport_unavailable');
        // A viewport-only screenshot with origin (0,0) follows the current
        // scrolled view; shifting its clip by window.scrollY would cause a
        // viewport-relative double-offset and an invalid clipped region.
        // Capture the current page state without fast-forwarding finite
        // CSS transitions/animations. 'disabled' would fire transitionend
        // handlers and can cause page-side effects on an otherwise read-only
        // open-world screenshot tool.
        const jpeg = await page.screenshot({type:'jpeg',quality:input.quality,animations:'allow',caret:'hide',
          fullPage:false,clip:{x:0,y:0,width:Math.min(vp.width,input.max_width),height:Math.min(vp.height,720)},
          timeout:10000});
        if (jpeg.length < 4 || jpeg.length > 32768) {
          reply({event:'error',nonce:input.nonce,reason:'capture_too_large'}); continue;
        }
        const payload = jpeg.toString('base64');
        const parts = Math.ceil(payload.length/8192);
        reply({event:'capture_meta',nonce:input.nonce,mime:'image/jpeg',size:jpeg.length,
          sha256:createHash('sha256').update(jpeg).digest('hex'),parts});
        for (let i=0;i<parts;i++) reply({event:'capture_part',nonce:input.nonce,index:i,
          data:payload.slice(i*8192,(i+1)*8192)});
        reply({event:'capture_done',nonce:input.nonce});
      } catch { reply({event:'error',nonce:input.nonce,reason:'capture_failed'}); }
      continue;
    }
    // A trace encloses exactly ONE bounded flow, rather than an unbounded
    // cross-call recording. Reading is a separate, integrity/version-pinned
    // operation that never replays actions. Previous trace data is erased
    // BEFORE executing a replacement flow.
    if (input?.type === 'trace_record' && typeof input.nonce === 'string' && /^[0-9a-f]{32}$/.test(input.nonce) &&
        Array.isArray(input.steps) && input.steps.length >= 1 && input.steps.length <= 12) {
      traceData = null; traceDigest = '';
      let dir = '', started = false, response;
      const watchdog = setTimeout(() => { void context.close().finally(() => process.exit(74)); }, 30000);
      try {
        if (!diskSafe()) throw Error('trace_disk_reserve_unavailable');
        dir = mkdtempSync(join(downloads, 'asa-trace-'));
        const zipPath = join(dir, 'trace.zip');
        await context.tracing.start({ screenshots: true, snapshots: true, sources: false });
        started = true;
        const body = flowTemplate.replace('__ASA_FLOW_STEPS_JSON__', JSON.stringify(input.steps));
        if (body.length > 131072) throw Error('input_too_large');
        const emitted = [];
        const sink = {log(value) { if (typeof value === 'string' && value.startsWith('ASA_BROWSER_E2E_RESULT '))
          emitted.push(value.slice('ASA_BROWSER_E2E_RESULT '.length)); }};
        await new AsyncFunction('page','context','browser','console',body)(page,context,browser,sink);
        if (emitted.length !== 1 || Buffer.byteLength(emitted[0],'utf8') > 16000)
          throw Error('invalid_trace_result');
        const flowResult = JSON.parse(emitted[0]);
        await context.tracing.stop({path:zipPath});
        started = false;
        // O_NOFOLLOW + descriptor-based stat/read avoids path-replacement
        // ambiguity between size validation and bounded memory allocation.
        const fd = openSync(zipPath, constants.O_RDONLY | constants.O_NOFOLLOW);
        let payload;
        try {
          const info = fstatSync(fd);
          if (!info.isFile() || info.size < 4) throw Error('invalid_trace_zip');
          if (info.size > traceMaxBytes) {
            response = {event:'error',nonce:input.nonce,reason:'trace_too_large',result:flowResult};
          } else {
            // A size preflight followed by readFileSync(fd) is NOT a
            // bounded read if another same-UID process grows the inode
            // concurrently. Allocate exactly the proven cap, then read
            // only that many bytes. Reject short reads and extra bytes.
            payload = Buffer.allocUnsafe(info.size);
            let offset = 0;
            while (offset < payload.length) {
              const n = readSync(fd, payload, offset, payload.length - offset, null);
              if (n <= 0) throw Error('trace_zip_short_read');
              offset += n;
            }
            const extra = Buffer.allocUnsafe(1);
            if (readSync(fd, extra, 0, 1, null) !== 0 ||
                payload[0] !== 0x50 || payload[1] !== 0x4b)
              throw Error('invalid_or_grown_trace_zip');
            const digest = createHash('sha256').update(payload).digest('hex');
            traceData = payload; traceDigest = digest;
            response = {event:'trace_meta',nonce:input.nonce,mime:'application/zip',size:payload.length,
              sha256:digest,result:flowResult};
          }
        } finally { closeSync(fd); }
      } catch {
        if (started) { try { await context.tracing.stop(); } catch {} }
        traceData = null; traceDigest = '';
        response = {event:'error',nonce:input.nonce,reason:'trace_record_unknown'};
      } finally {
        clearTimeout(watchdog);
        try { if (dir) rmSync(dir,{recursive:true,force:true}); }
        catch { traceData = null; traceDigest = ''; response = {event:'error',nonce:input.nonce,reason:'trace_cleanup_unknown'}; }
      }
      reply(response);
      continue;
    }
    if (input?.type === 'trace_read' && typeof input.nonce === 'string' && /^[0-9a-f]{32}$/.test(input.nonce) &&
        Number.isSafeInteger(input.offset) && input.offset >= 0 && typeof input.file_version === 'string') {
      if (!traceData) {
        reply({event:'error',nonce:input.nonce,reason:'trace_unavailable'});
      } else if (input.file_version !== 'sha256:' + traceDigest) {
        reply({event:'error',nonce:input.nonce,reason:'trace_stale'});
      } else if (input.offset > traceData.length) {
        reply({event:'error',nonce:input.nonce,reason:'trace_invalid_offset'});
      } else {
        const chunk = traceData.subarray(input.offset,Math.min(traceData.length,input.offset+traceReadBytes));
        reply({event:'trace_chunk',nonce:input.nonce,mime:'application/zip',size:traceData.length,
          sha256:traceDigest,offset:input.offset,
          chunk_sha256:createHash('sha256').update(chunk).digest('hex'),data:chunk.toString('base64')});
      }
      continue;
    }
    if (input?.type === 'trace_discard' && typeof input.nonce === 'string' && /^[0-9a-f]{32}$/.test(input.nonce) &&
        typeof input.file_version === 'string') {
      if (!traceData || input.file_version !== 'sha256:' + traceDigest) {
        reply({event:'error',nonce:input.nonce,reason:'trace_stale'});
      } else {
        traceData = null; traceDigest = '';
        reply({event:'trace_discarded',nonce:input.nonce});
      }
      continue;
    }
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
