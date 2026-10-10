package browser

import (
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
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/scroll" {
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
	src, err := managedSessionRunner(engine, filepath.Join(dir, "profile"), downloads, false)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "session.mjs")
	if err = os.WriteFile(file, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
