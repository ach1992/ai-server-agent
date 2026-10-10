package browser

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Real pinned Chromium source-level proof: screenshot bytes traverse the
// session's bounded stdout protocol and never become named on-disk artifacts.
// This is NOT evidence of deployment to an installed Agent/MCP client.
func TestManagedBrowserSessionCapturePinnedRuntime(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("pinned Chromium runtime required")
	}
	var transitionEffects atomic.Int64
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/transition-effect" {
			transitionEffects.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/animation" {
			fmt.Fprint(w, `<!doctype html><html><head><style>
#moving {width:80px;height:120px;background:#147cc0;transition:width 90s linear;}
#moving.active {width:600px;}
</style></head><body><button id="start-transition" onclick="document.querySelector('#moving').classList.add('active')">Start slow transition</button>
<div id="moving"></div><div id="phase">idle</div><div id="effect">not-fired</div>
<script>
const moving = document.querySelector('#moving');
moving.addEventListener('transitionrun', () => {
 document.querySelector('#phase').textContent = 'running';
});
moving.addEventListener('transitionend', () => {
 document.querySelector('#effect').textContent = 'fired';
 fetch('/transition-effect', {method:'POST',keepalive:true}).catch(() => {});
});
</script></body></html>`)
		} else if r.URL.Path == "/scroll" {
			fmt.Fprint(w, `<!doctype html><html><body style="margin:0"><div style="height:1050px;background:rgb(230,15,15)"><button id="go" onclick="window.scrollTo(0,1100)">Go down</button></div><div style="height:1050px;background:rgb(15,15,230)"></div></body></html>`)
		} else if r.URL.Path == "/noise" {
			fmt.Fprint(w, `<!doctype html><html><body style="margin:0"><canvas id="x" width="1024" height="720"></canvas><script>
const c=document.querySelector('#x'),ctx=c.getContext('2d'),p=ctx.createImageData(1024,720);
let z=123456789;for(let i=0;i<p.data.length;i+=4){z=(Math.imul(1664525,z)+1013904223)>>>0;p.data[i]=z&255;p.data[i+1]=(z>>>8)&255;p.data[i+2]=(z>>>16)&255;p.data[i+3]=255;}
ctx.putImageData(p,0,0);</script></body></html>`)
		} else {
			fmt.Fprint(w, `<!doctype html><html><body><h1>Bounded screenshot evidence</h1><button>Review</button></body></html>`)
		}
	}))
	defer web.Close()
	dir := t.TempDir()
	downloads := filepath.Join(dir, "downloads")
	if err := os.Mkdir(downloads, 0700); err != nil {
		t.Fatal(err)
	}
	// Simulate an orphan from a prior interrupted trace recording. Startup
	// under exclusive profile admission reclaims it without touching worktrees.
	orphan := filepath.Join(downloads, "asa-trace-Ab3d9Z")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "trace.zip"), []byte("sensitive-disposable-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := managedSessionRunner(engine, filepath.Join(dir, "profile"), downloads, false)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "session.mjs")
	if err = os.WriteFile(file, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(engine, "node/bin/node"), file)
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_BROWSERS_PATH="+filepath.Join(engine, "browsers"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Normal test teardown must follow the SAME graceful Browser session
	// protocol as production: allow context.close() to terminate Chromium
	// and finish writing the profile before t.TempDir removes its files.
	// Killing Node immediately races Chrome's profile writes on fast CI.
	defer func() {
		_, _ = stdin.Write([]byte("{\"type\":\"close\"}\n"))
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case er := <-done:
			if er != nil {
				t.Errorf("graceful Browser fixture close: %v; stderr=%s", er, stderr.String())
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("graceful Browser fixture close timed out; emergency kill applied")
		}
	}()
	rd := bufio.NewReader(stdout)
	read := func() map[string]any {
		t.Helper()
		line, er := rd.ReadString('\n')
		if er != nil {
			t.Fatalf("worker frame: %v; stderr=%s", er, stderr.String())
		}
		if !strings.HasPrefix(line, "ASA_BROWSER_SESSION ") {
			t.Fatalf("unframed output %q", line)
		}
		var obj map[string]any
		if er = json.Unmarshal([]byte(strings.TrimPrefix(line, "ASA_BROWSER_SESSION ")), &obj); er != nil {
			t.Fatal(er)
		}
		return obj
	}
	send := func(obj any) {
		t.Helper()
		b, er := json.Marshal(obj)
		if er != nil {
			t.Fatal(er)
		}
		if _, er = stdin.Write(append(b, '\n')); er != nil {
			t.Fatal(er)
		}
	}
	if got := read(); got["event"] != "ready" {
		t.Fatalf("ready: %v", got)
	}
	goTo := func(target string, nonce string) {
		send(map[string]any{"type": "flow", "nonce": nonce, "steps": []map[string]any{{"action": "goto", "url": target}}})
		got := read()
		if got["event"] != "result" || got["nonce"] != nonce || got["result"].(map[string]any)["ok"] != true {
			t.Fatalf("navigate: %v", got)
		}
	}
	goTo(web.URL, strings.Repeat("a", 32))
	capture := func(nonce string, quality, width int) (map[string]any, []byte) {
		t.Helper()
		send(map[string]any{"type": "capture", "nonce": nonce, "quality": quality, "max_width": width})
		head := read()
		if head["nonce"] != nonce {
			t.Fatalf("capture nonce mismatch: %v", head)
		}
		if head["event"] == "error" {
			return head, nil
		}
		if head["event"] != "capture_meta" || head["mime"] != "image/jpeg" {
			t.Fatalf("capture metadata: %v", head)
		}
		var b64 strings.Builder
		for i := 0; i < int(head["parts"].(float64)); i++ {
			part := read()
			if part["event"] != "capture_part" || part["nonce"] != nonce || int(part["index"].(float64)) != i {
				t.Fatalf("capture part: %v", part)
			}
			b64.WriteString(part["data"].(string))
		}
		end := read()
		if end["event"] != "capture_done" || end["nonce"] != nonce {
			t.Fatalf("capture end: %v", end)
		}
		payload, er := base64.StdEncoding.Strict().DecodeString(b64.String())
		if er != nil {
			t.Fatal(er)
		}
		sha := sha256.Sum256(payload)
		if len(payload) != int(head["size"].(float64)) || hex.EncodeToString(sha[:]) != head["sha256"] {
			t.Fatalf("capture SHA/size mismatch: %v", head)
		}
		meta, er := jpeg.DecodeConfig(bytes.NewReader(payload))
		if er != nil {
			t.Fatal(er)
		}
		if meta.Width < 1 || meta.Width > width || meta.Height < 1 || meta.Height > 720 {
			t.Fatalf("unbounded capture %dx%d", meta.Width, meta.Height)
		}
		return head, payload
	}
	meta, payload := capture(strings.Repeat("b", 32), 40, 640)
	if meta["event"] != "capture_meta" || len(payload) == 0 || len(payload) > 32<<10 {
		t.Fatalf("missing bounded capture: %v", meta)
	}
	entries, err := os.ReadDir(downloads)
	if err != nil || len(entries) != 0 {
		t.Fatalf("capture left disk artifacts: %v %v", entries, err)
	}
	// Record one real Playwright ZIP trace, then prove incremental byte
	// windows reconstruct a valid archive and no named ZIP remains on disk.
	traceNonce := strings.Repeat("6", 32)
	send(map[string]any{"type": "trace_record", "nonce": traceNonce,
		"steps": []FlowStep{{Action: "snapshot"}}})
	traceMeta := read()
	if traceMeta["event"] != "trace_meta" || traceMeta["nonce"] != traceNonce ||
		traceMeta["mime"] != "application/zip" || traceMeta["result"].(map[string]any)["ok"] != true {
		t.Fatalf("trace did not return bounded metadata: %v; stderr=%s", traceMeta, stderr.String())
	}
	traceSize := int(traceMeta["size"].(float64))
	if traceSize < 4 || traceSize > 512<<10 {
		t.Fatalf("unbounded trace bytes: %d", traceSize)
	}
	version := "sha256:" + traceMeta["sha256"].(string)
	var traceZIP []byte
	for len(traceZIP) < traceSize {
		nonce := fmt.Sprintf("%032x", len(traceZIP)+1)
		send(map[string]any{"type": "trace_read", "nonce": nonce, "offset": len(traceZIP), "file_version": version})
		chunk := read()
		if chunk["event"] != "trace_chunk" || chunk["nonce"] != nonce || int(chunk["offset"].(float64)) != len(traceZIP) ||
			int(chunk["size"].(float64)) != traceSize || chunk["sha256"] != traceMeta["sha256"] {
			t.Fatalf("trace read window wrong cursor/version: %v", chunk)
		}
		data, er := base64.StdEncoding.Strict().DecodeString(chunk["data"].(string))
		if er != nil || len(data) == 0 || len(data) > 8192 {
			t.Fatalf("invalid trace window: size=%d err=%v", len(data), er)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != chunk["chunk_sha256"] {
			t.Fatal("trace window digest mismatch")
		}
		traceZIP = append(traceZIP, data...)
	}
	allSHA := sha256.Sum256(traceZIP)
	if len(traceZIP) != traceSize || hex.EncodeToString(allSHA[:]) != traceMeta["sha256"] {
		t.Fatal("reconstructed trace ZIP integrity differs from pinned recording")
	}
	archive, er := zip.NewReader(bytes.NewReader(traceZIP), int64(len(traceZIP)))
	if er != nil || len(archive.File) == 0 {
		t.Fatalf("Playwright trace ZIP not readable: %v", er)
	}
	// Version mismatch is a known refusal; it cannot leak a previous ZIP.
	send(map[string]any{"type": "trace_read", "nonce": strings.Repeat("a", 32),
		"offset": 0, "file_version": "sha256:" + strings.Repeat("0", 64)})
	if stale := read(); stale["event"] != "error" || stale["reason"] != "trace_stale" {
		t.Fatalf("stale version disclosed trace data: %v", stale)
	}
	send(map[string]any{"type": "trace_discard", "nonce": strings.Repeat("b", 32), "file_version": version})
	if discarded := read(); discarded["event"] != "trace_discarded" {
		t.Fatalf("trace discard failed: %v", discarded)
	}
	send(map[string]any{"type": "trace_read", "nonce": strings.Repeat("c", 32), "offset": 0, "file_version": version})
	if missing := read(); missing["event"] != "error" || missing["reason"] != "trace_unavailable" {
		t.Fatalf("discarded trace bytes still retrievable: %v", missing)
	}
	if leftovers, er := os.ReadDir(downloads); er != nil || len(leftovers) != 0 {
		t.Fatalf("trace temporary files retained after verified completion: %v %v", leftovers, er)
	}
	// A screenshot after scroll must represent the *visible* viewport,
	// never the page origin. The two blocks have opposite dominant colors.
	goTo(web.URL+"/scroll", strings.Repeat("7", 32))
	top, topBytes := capture(strings.Repeat("8", 32), 35, 320)
	if top["event"] != "capture_meta" {
		t.Fatalf("initial viewport: %v", top)
	}
	send(map[string]any{"type": "flow", "nonce": strings.Repeat("9", 32), "steps": []map[string]any{{"action": "click", "selector": "#go"}}})
	if clicked := read(); clicked["event"] != "result" || clicked["result"].(map[string]any)["ok"] != true {
		t.Fatalf("scroll action: %v", clicked)
	}
	bottom, bottomBytes := capture(strings.Repeat("1", 32), 35, 320)
	if bottom["event"] != "capture_meta" {
		t.Fatalf("scrolled viewport: %v; stderr=%s", bottom, stderr.String())
	}
	sample := func(b []byte) (uint32, uint32, uint32) {
		image, er := jpeg.Decode(bytes.NewReader(b))
		if er != nil {
			t.Fatal(er)
		}
		bounds := image.Bounds()
		r, g, bl, _ := color.RGBAModel.Convert(image.At(bounds.Min.X+150, bounds.Min.Y+200)).RGBA()
		return r, g, bl
	}
	tr, _, tb := sample(topBytes)
	br, _, bb := sample(bottomBytes)
	if tr <= tb*2 || bb <= br*2 {
		t.Fatalf("capture ignored scroll viewport (top red=%d blue=%d, bottom red=%d blue=%d)", tr, tb, br, bb)
	}
	// A read-only screenshot must NOT fast-forward a finite CSS transition:
	// animations:'disabled' in Playwright completes it and can dispatch
	// transitionend, triggering arbitrary open-world page handlers/network.
	// The long duration prevents natural completion during this bounded test.
	goTo(web.URL+"/animation", strings.Repeat("2", 32))
	send(map[string]any{"type": "flow", "nonce": strings.Repeat("3", 32), "steps": []FlowStep{
		{Action: "click", Selector: "#start-transition"},
		{Action: "assert_text", Selector: "#phase", Expected: "running"},
		{Action: "assert_text", Selector: "#effect", Expected: "not-fired"},
	}})
	started := read()
	if started["event"] != "result" || started["result"].(map[string]any)["ok"] != true {
		t.Fatalf("90s CSS transition must be actively running before the capture: %v", started)
	}
	inflight, imageBytes := capture(strings.Repeat("4", 32), 35, 320)
	if inflight["event"] != "capture_meta" || len(imageBytes) == 0 {
		t.Fatalf("active CSS transition prevented a bounded screenshot: %v", inflight)
	}
	send(map[string]any{"type": "flow", "nonce": strings.Repeat("5", 32), "steps": []FlowStep{
		{Action: "assert_text", Selector: "#phase", Expected: "running"},
		{Action: "assert_text", Selector: "#effect", Expected: "not-fired"},
	}})
	checked := read()
	if checked["event"] != "result" || checked["result"].(map[string]any)["ok"] != true || transitionEffects.Load() != 0 {
		t.Fatalf("read-only screenshot fast-forwarded CSS transition or triggered handler/network: result=%v network_side_effects=%d", checked, transitionEffects.Load())
	}
	goTo(web.URL+"/noise", strings.Repeat("c", 32))
	fail, img := capture(strings.Repeat("d", 32), 70, 1024)
	if fail["event"] != "error" || fail["reason"] != "capture_too_large" || img != nil {
		t.Fatalf("oversize capture was not explicit: %v", fail)
	}
	// Known oversize is not transport uncertainty; same session is usable.
	goTo(web.URL, strings.Repeat("e", 32))
	if success, raw := capture(strings.Repeat("f", 32), 35, 320); success["event"] != "capture_meta" || len(raw) == 0 {
		t.Fatalf("known too_large poisoned Browser session: %v", success)
	}
	// The deferred teardown sends exactly one close request and awaits
	// worker+Chromium completion before TempDir cleanup.
}
