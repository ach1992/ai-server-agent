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
// artifact; Executor-owned private files are removed after verified stop.
func managedSessionRunner(engine, profile, downloads string, ignoreHTTPS bool, modes ...string) (string, error) {
	mode := "shared"
	if len(modes) > 0 {
		mode = modes[0]
	}
	if _, err := normalizeProfileMode(mode); err != nil {
		return "", err
	}
	if !filepath.IsAbs(engine) || !filepath.IsAbs(profile) || !filepath.IsAbs(downloads) ||
		filepath.Clean(engine) != engine || filepath.Clean(profile) != profile || filepath.Clean(downloads) != downloads {
		return "", errors.New("managed Browser paths must be clean absolute paths")
	}
	engineURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(engine, "node_modules/playwright/index.mjs"))}).String()
	items := make([]string, 0, 5)
	for _, value := range []string{engineURL, profile, downloads, filepath.Join(engine, "browsers"), browserFlowJS, mode} {
		raw, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		items = append(items, string(raw))
	}
	return fmt.Sprintf(`import { createInterface } from 'node:readline';
import { createHash } from 'node:crypto';
import { statfsSync, mkdtempSync, mkdirSync, rmSync, openSync, fstatSync, readSync, closeSync, constants, readdirSync, opendirSync, lstatSync } from 'node:fs';
import { join } from 'node:path';
process.env.PLAYWRIGHT_BROWSERS_PATH = %s;
const { chromium } = await import(%s);
const profile = %s;
const downloads = %s;
const flowTemplate = %s;
const profileMode = %s;
const state = { refs: new Map(), sequence: 0 };
// One short-lived, principal/session-bound ZIP slot. Playwright writes into a
// private directory; retain one pinned read-only fd, NOT the whole ZIP in RAM.
// The existing 8 KiB response windows stay inside the 64 KiB broker ring.
let traceFile = null;
let traceDir = '';
let traceSize = 0;
let traceDigest = '';
let traceIdentity = null;
const traceMaxBytes = 16 * 1024 * 1024;
const traceReadBytes = 8192;
const traceScanBytes = 65536;
const sameTraceFile = info => traceIdentity !== null && info.isFile() && info.nlink === 1n &&
  info.dev === traceIdentity.dev && info.ino === traceIdentity.ino &&
  info.size === traceIdentity.size && info.mtimeNs === traceIdentity.mtimeNs &&
  info.ctimeNs === traceIdentity.ctimeNs;
const discardTrace = () => {
  if (traceFile !== null) {
    const fd = traceFile;
    traceFile = null;
    closeSync(fd);
  }
  if (traceDir) {
    rmSync(traceDir, {recursive:true,force:true});
    traceDir = '';
  }
  traceSize = 0;
  traceDigest = '';
  traceIdentity = null;
};
globalThis.__asaManagedFlowState = state;
const reserve = 512n * 1024n * 1024n;
const diskSafe = () => {
  try {
    const st = statfsSync(downloads, { bigint:true });
    return st.bavail * st.bsize >= reserve;
  } catch { return false; }
};
if (!diskSafe()) throw new Error('browser_session_disk_reserve_unavailable');
// When run through the Executor, the broker's empty-to-first admission
// already reclaimed stale disposable directories *before* allowing peers.
// Independent CLI/standalone acceptance fixtures keep their legacy cleanup;
// they never share an Executor-owned concurrent admission domain.
if (process.env.ASA_BROWSER_RECLAIM_BY_EXECUTOR !== '1') {
  for (const entry of readdirSync(downloads, {withFileTypes:true})) {
    if (!entry.isDirectory() ||
        !(/^asa-trace-[A-Za-z0-9]{6}$/.test(entry.name) ||
          /^asa-profile-[A-Za-z0-9]{6}$/.test(entry.name))) continue;
    rmSync(join(downloads,entry.name),{recursive:true,force:true});
  }
}

// Each isolated profile is per-session private; it can never be reclaimed
// from another active session or reused as a shared browser profile.
const executorOwnsScratch = process.env.ASA_BROWSER_RECLAIM_BY_EXECUTOR === '1';
const profileName = process.env.ASA_BROWSER_SESSION_PROFILE || '';
if (executorOwnsScratch && profileMode === 'isolated' && !/^asa-profile-[0-9a-f]{32}$/.test(profileName))
  throw new Error('browser_profile_identity_unavailable');
const traceName = process.env.ASA_BROWSER_SESSION_TRACE || '';
if (executorOwnsScratch && !/^asa-trace-[0-9a-f]{32}$/.test(traceName))
  throw new Error('browser_trace_identity_unavailable');
const isolatedProfile = profileMode !== 'isolated' ? '' : executorOwnsScratch ?
  join(downloads, profileName) : mkdtempSync(join(downloads, 'asa-profile-'));
if (executorOwnsScratch && isolatedProfile) mkdirSync(isolatedProfile, {mode:0o700});
const cleanupProfile = () => { if (!executorOwnsScratch && isolatedProfile) rmSync(isolatedProfile, {recursive:true,force:true}); };
// Bounded private profile retention: no extra monitor process, host scheduler,
// or arbitrary project cleanup. Never follow symlinks; fail closed if the
// profile grows beyond 256 MiB or 8192 on-disk entries.
const privateProfileSafe = () => {
  if (!isolatedProfile) return true;
  let bytes = 0;
  let seen = 0;
  const pending = [isolatedProfile];
  try {
    while (pending.length) {
      if (++seen > 8192) return false;
      const path = pending.pop();
      const info = lstatSync(path, {throwIfNoEntry:false});
      if (!info) continue; // Chromium can retire a cache file mid-inspection.
      if (info.isSymbolicLink()) continue;
      bytes += info.size;
      if (bytes > 256 * 1024 * 1024) return false;
      if (!info.isDirectory()) continue;
      const handle = opendirSync(path);
      try {
        let entry;
        while ((entry = handle.readSync()) !== null) {
          pending.push(join(path, entry.name));
          if (pending.length + seen > 8192) return false;
        }
      } finally { handle.closeSync(); }
    }
    return true;
  } catch { return false; }
};
let context;
try {
  context = await chromium.launchPersistentContext(isolatedProfile || profile, {
    headless: true, ignoreHTTPSErrors: %t, acceptDownloads: false, downloadsPath: downloads,
    args: ['--disk-cache-size=67108864', '--media-cache-size=33554432']
  });
} catch (error) {
  cleanupProfile();
  throw error;
}
const browser = context.browser();
const page = context.pages()[0] || await context.newPage();
// Bounded persistent sessions need the same filesystem safety reserve as
// browser_run, including while a page is busy between client calls.
let diskPressure = false;
const diskMonitor = setInterval(() => {
  if (!diskPressure && (!diskSafe() || !privateProfileSafe())) {
    diskPressure = true;
    clearInterval(diskMonitor);
    void context.close().finally(() => {
      try { cleanupProfile(); } catch {}
      process.exit(73);
    });
  }
}, 1000);
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
const reply = data => process.stdout.write('ASA_BROWSER_SESSION ' + JSON.stringify(data) + '\n');
reply({ event: 'ready' });
try {
  // Browser actions remain capped at 128. Read-only 8 KiB ZIP windows have
  // a distinct *finite* budget: 8192 windows = at most 64 MiB of streamed
  // trace bytes across this session (four full 16 MiB downloads).
  // This leaves the ordinary Browser action capacity unchanged.
  let actions = 0;
  let traceReads = 0;
  const maxTraceReads = 8192;
  for await (const line of createInterface({input: process.stdin, crlfDelay: Infinity})) {
    if (Buffer.byteLength(line, 'utf8') > 16384) {
      reply({ event: 'error', reason: 'session_input_limit' }); break;
    }
    let input;
    try { input = JSON.parse(line); } catch { reply({event:'error',reason:'invalid_request'}); break; }
    // Closing must always be possible, even when a read/action budget has
    // already been spent. It does not add any new user-visible capability.
    if (input?.type === 'close') break;
    if (input?.type === 'trace_read') {
      if (++traceReads > maxTraceReads) {
        reply({event:'error',nonce:input.nonce,reason:'trace_read_budget_exhausted'});
        continue;
      }
    } else if (++actions > 128) {
      reply({event:'error',nonce:input?.nonce,reason:'session_input_limit'}); break;
    }
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
    if (input?.type === 'trace_record' && profileMode !== 'isolated') {
      reply({event:'error',nonce:input.nonce,reason:'browser_trace_requires_isolated_profile'});
      continue;
    }
    if (input?.type === 'trace_record' && typeof input.nonce === 'string' && /^[0-9a-f]{32}$/.test(input.nonce) &&
        Array.isArray(input.steps) && input.steps.length >= 1 && input.steps.length <= 12) {
      try { discardTrace(); }
      catch { reply({event:'error',nonce:input.nonce,reason:'trace_cleanup_unknown'}); continue; }
      let dir = '', started = false, response;
      const watchdog = setTimeout(() => { void context.close().finally(() => process.exit(74)); }, 30000);
      try {
        if (!diskSafe()) throw Error('trace_disk_reserve_unavailable');
        dir = executorOwnsScratch ? join(downloads, traceName) : mkdtempSync(join(downloads, 'asa-trace-'));
        if (executorOwnsScratch) mkdirSync(dir, {mode:0o700});
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
        // Hold the exact O_NOFOLLOW inode until discard/replace/close. Scan
        // with one fixed 64 KiB buffer, never allocating the ZIP's file size.
        const fd = openSync(zipPath, constants.O_RDONLY | constants.O_NOFOLLOW);
        let keepFD = false;
        try {
          const info = fstatSync(fd, {bigint:true});
          if (!info.isFile() || info.nlink !== 1n || info.size < 4n)
            throw Error('invalid_trace_zip');
          if (info.size > BigInt(traceMaxBytes)) {
            response = {event:'error',nonce:input.nonce,reason:'trace_too_large',result:flowResult};
          } else {
            const scan = Buffer.allocUnsafe(traceScanBytes);
            const digest = createHash('sha256');
            const size = Number(info.size);
            let offset = 0;
            while (offset < size) {
              const count = readSync(fd, scan, 0, Math.min(scan.length,size-offset),offset);
              if (count <= 0) throw Error('trace_zip_short_read');
              if (offset === 0 && (scan[0] !== 0x50 || scan[1] !== 0x4b))
                throw Error('invalid_trace_zip');
              digest.update(scan.subarray(0,count));
              offset += count;
            }
            const end = Buffer.allocUnsafe(1);
            if (readSync(fd,end,0,1,size) !== 0) throw Error('grown_trace_zip');
            const after = fstatSync(fd, {bigint:true});
            if (info.dev !== after.dev || info.ino !== after.ino ||
                info.size !== after.size || info.mtimeNs !== after.mtimeNs ||
                info.ctimeNs !== after.ctimeNs || after.nlink !== 1n)
              throw Error('mutated_trace_zip');
            traceIdentity = after;
            traceSize = size;
            traceDigest = digest.digest('hex');
            traceFile = fd;
            traceDir = dir;
            dir = '';
            keepFD = true;
            response = {event:'trace_meta',nonce:input.nonce,mime:'application/zip',
              size:traceSize,sha256:traceDigest,result:flowResult};
          }
        } finally { if (!keepFD) closeSync(fd); }
      } catch {
        if (started) { try { await context.tracing.stop(); } catch {} }
        try { discardTrace(); } catch {}
        response = {event:'error',nonce:input.nonce,reason:'trace_record_unknown'};
      } finally {
        clearTimeout(watchdog);
        try { if (dir) rmSync(dir,{recursive:true,force:true}); }
        catch { try { discardTrace(); } catch {} response = {event:'error',nonce:input.nonce,reason:'trace_cleanup_unknown'}; }
      }
      reply(response);
      continue;
    }
    if (input?.type === 'trace_read' && typeof input.nonce === 'string' && /^[0-9a-f]{32}$/.test(input.nonce) &&
        Number.isSafeInteger(input.offset) && input.offset >= 0 && typeof input.file_version === 'string') {
      if (traceFile === null) {
        reply({event:'error',nonce:input.nonce,reason:'trace_unavailable'});
      } else if (input.file_version !== 'sha256:' + traceDigest) {
        reply({event:'error',nonce:input.nonce,reason:'trace_stale'});
      } else if (input.offset > traceSize) {
        reply({event:'error',nonce:input.nonce,reason:'trace_invalid_offset'});
      } else {
        try {
          if (!sameTraceFile(fstatSync(traceFile, {bigint:true}))) throw Error('trace_stale');
          const count = Math.min(traceReadBytes,traceSize-input.offset);
          const chunk = Buffer.allocUnsafe(count);
          if (count && readSync(traceFile,chunk,0,count,input.offset) !== count)
            throw Error('trace_window_short_read');
          if (!sameTraceFile(fstatSync(traceFile, {bigint:true}))) throw Error('trace_stale');
          reply({event:'trace_chunk',nonce:input.nonce,mime:'application/zip',size:traceSize,
            sha256:traceDigest,offset:input.offset,
            chunk_sha256:createHash('sha256').update(chunk).digest('hex'),data:chunk.toString('base64')});
        } catch {
          try {
            discardTrace();
            reply({event:'error',nonce:input.nonce,reason:'trace_stale'});
          } catch { reply({event:'error',nonce:input.nonce,reason:'trace_cleanup_unknown'}); }
        }
      }
      continue;
    }
    if (input?.type === 'trace_discard' && typeof input.nonce === 'string' && /^[0-9a-f]{32}$/.test(input.nonce) &&
        typeof input.file_version === 'string') {
      if (traceFile === null || input.file_version !== 'sha256:' + traceDigest) {
        reply({event:'error',nonce:input.nonce,reason:'trace_stale'});
      } else {
        try {
          discardTrace();
          reply({event:'trace_discarded',nonce:input.nonce});
        } catch { reply({event:'error',nonce:input.nonce,reason:'trace_cleanup_unknown'}); }
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
  // Never unlink Executor-managed trace scratch during normal shutdown.
  // Chromium may still have independently grouped descendants. The root
  // Executor owns final exact-name reclamation AFTER whole-cgroup proof.
  // Explicit in-session discard/replacement remains separate and supported.
  if (!executorOwnsScratch) {
    try { discardTrace(); } catch {}
  }
  await context.close();
  cleanupProfile();
  process.exit(0);
}
`, items[3], items[0], items[1], items[2], items[4], items[5], ignoreHTTPS), nil
}
