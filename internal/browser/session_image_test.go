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
	"image/jpeg"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

// Actual pinned Chromium/Playwright, not a mock screenshot: prove one viewport
// too detailed for the old 32 KiB event path is retrievable without resizing
// using version-pinned 8 KiB windows and a finite in-session image buffer.
func TestManagedBrowserLargeImageContinuationPinnedChromium(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("explicit pinned Chromium runtime required")
	}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body style="margin:0">
<canvas id="pixels" width="1024" height="720"></canvas><script>
const c=document.getElementById('pixels'),x=c.getContext('2d'),a=x.createImageData(1024,720);
let v=12457;for(let i=0;i<a.data.length;i+=4){v=(Math.imul(v,1664525)+1013904223)>>>0;
a.data[i]=v&255;a.data[i+1]=(v>>>8)&255;a.data[i+2]=(v>>>16)&255;a.data[i+3]=255;}
x.putImageData(a,0,0);</script></body></html>`)
	}))
	defer web.Close()
	root := t.TempDir()
	downloads := filepath.Join(root, "downloads")
	if err := os.Mkdir(downloads, 0700); err != nil {
		t.Fatal(err)
	}
	src, err := managedSessionRunner(engine, filepath.Join(root, "profile"), downloads, false, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "session.mjs")
	if err := os.WriteFile(file, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
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
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = stdin.Write([]byte("{\"type\":\"close\"}\n"))
		_ = stdin.Close()
		ch := make(chan error, 1)
		go func() { ch <- cmd.Wait() }()
		select {
		case err := <-ch:
			if err != nil {
				t.Errorf("Browser close: %v; stderr=%s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-ch
			t.Errorf("Browser fixture forced kill; stderr=%s", stderr.String())
		}
		entries, err := os.ReadDir(downloads)
		if err != nil {
			t.Error(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "asa-trace-") {
				t.Errorf("image continuation left artifact scratch: %s", entry.Name())
			}
		}
	}()
	rd := bufio.NewReader(stdout)
	read := func() map[string]any {
		t.Helper()
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("Browser frame: %v; stderr=%s", err, stderr.String())
		}
		if !strings.HasPrefix(line, "ASA_BROWSER_SESSION ") {
			t.Fatalf("unframed output: %q", line)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "ASA_BROWSER_SESSION ")), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	send := func(v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if ready := read(); ready["event"] != "ready" || ready["image_staging"] != true {
		t.Fatalf("pinned worker lacks image continuation handshake: %v", ready)
	}
	send(map[string]any{"type": "flow", "nonce": "00000000000000000000000000000001",
		"steps": []map[string]any{{"action": "goto", "url": web.URL}}})
	if got := read(); got["event"] != "result" || got["result"].(map[string]any)["ok"] != true {
		t.Fatalf("navigate noise fixture: %v", got)
	}
	capture := func(nonce string, width, height, x, y int) map[string]any {
		send(map[string]any{"type": "image_capture", "nonce": nonce, "quality": 70,
			"max_width": width, "max_height": height, "crop_x": x, "crop_y": y})
		return read()
	}
	meta := capture("00000000000000000000000000000002", 1024, 720, 0, 0)
	if meta["event"] != "image_meta" || meta["mime"] != "image/jpeg" {
		t.Fatalf("no staged JPEG: %v", meta)
	}
	size, digest := int(meta["size"].(float64)), meta["sha256"].(string)
	if size <= 32<<10 || size > 1<<20 {
		t.Fatalf("noise JPEG must prove >32 KiB and <=1 MiB staging, got %d bytes", size)
	}
	region := meta["region"].(map[string]any)
	if int(region["width"].(float64)) != 1024 || int(region["height"].(float64)) != 720 {
		t.Fatalf("unexpected viewport crop: %v", region)
	}
	version := "sha256:" + digest
	var downloaded []byte
	for offset, nonceID := 0, 3; offset < size; nonceID++ {
		nonce := fmt.Sprintf("%032x", nonceID)
		limit := 8192
		if offset == 0 {
			limit = 2048
		} // a text-only client with minimum fallback
		send(map[string]any{"type": "image_read", "nonce": nonce, "file_version": version, "offset": offset, "limit": limit})
		frame := read()
		if frame["event"] != "image_chunk" || frame["offset"] != float64(offset) ||
			frame["sha256"] != digest {
			t.Fatalf("missing/version-changed image window at offset %d: %v", offset, frame)
		}
		chunk, err := base64.StdEncoding.Strict().DecodeString(frame["data"].(string))
		if err != nil || len(chunk) == 0 || len(chunk) > 8192 {
			t.Fatalf("invalid image window at %d: %v", offset, err)
		}
		partSum := sha256.Sum256(chunk)
		if frame["chunk_sha256"] != hex.EncodeToString(partSum[:]) {
			t.Fatalf("corrupted image window at %d", offset)
		}
		downloaded = append(downloaded, chunk...)
		offset += len(chunk)
	}
	if len(downloaded) != size {
		t.Fatalf("downloaded %d != announced %d", len(downloaded), size)
	}
	sum := sha256.Sum256(downloaded)
	if hex.EncodeToString(sum[:]) != digest {
		t.Fatalf("whole-image integrity mismatch")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(downloaded))
	if err != nil || decoded.Bounds().Dx() != 1024 || decoded.Bounds().Dy() != 720 {
		t.Fatalf("unusable full-sized JPEG: %v", err)
	}
	send(map[string]any{"type": "image_read", "nonce": "00000000000000000000000000002000",
		"file_version": version, "offset": size, "limit": 8192})
	if eof := read(); eof["event"] != "image_chunk" || eof["data"] != "" {
		t.Fatalf("expected empty exact-EOF read: %v", eof)
	}
	send(map[string]any{"type": "image_read", "nonce": "00000000000000000000000000002001",
		"file_version": version, "offset": size + 1, "limit": 8192})
	if bad := read(); bad["reason"] != "image_invalid_offset" {
		t.Fatalf("out-of-range offset was not rejected: %v", bad)
	}
	newImage := capture("00000000000000000000000000002002", 320, 100, 0, 100)
	if newImage["event"] != "image_meta" || newImage["sha256"] == digest {
		t.Fatalf("replacement JPEG was not staged with a new digest: %v", newImage)
	}
	send(map[string]any{"type": "image_read", "nonce": "00000000000000000000000000002003",
		"file_version": version, "offset": 0, "limit": 8192})
	if stale := read(); stale["reason"] != "image_stale" {
		t.Fatalf("old image version unexpectedly readable: %v", stale)
	}
	newVersion := "sha256:" + newImage["sha256"].(string)
	send(map[string]any{"type": "image_discard", "nonce": "00000000000000000000000000002004",
		"file_version": newVersion})
	if discarded := read(); discarded["event"] != "image_discarded" {
		t.Fatalf("explicit discard failed: %v", discarded)
	}
	send(map[string]any{"type": "image_read", "nonce": "00000000000000000000000000002005",
		"file_version": newVersion, "offset": 0, "limit": 8192})
	if unavailable := read(); unavailable["reason"] != "image_unavailable" {
		t.Fatalf("discarded image remains retrievable: %v", unavailable)
	}
}

// A HEAD Agent paired with an older Executor must never claim JPEG staging.
// This drives the real JSON Unix transport and does not touch installed hosts.
func TestBrowserSessionImageOlderExecutorFailsClosed(t *testing.T) {
	manager := testBrowserManager(t)
	manager.token = "fixture-token"
	// AF_UNIX pathname limit is 108 bytes on Linux. An operator TMPDIR can
	// be an intentionally long disk-backed location (e.g. when /tmp is a
	// small tmpfs); a private short socket path avoids a false regression.
	short, err := os.MkdirTemp("/tmp", "asa-img-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(short); err != nil {
			t.Error(err)
		}
	})
	sock := filepath.Join(short, "exec.sock")
	manager.cfg.ExecutorSocket = sock
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		var req executor.Request
		if e := json.NewDecoder(c).Decode(&req); e != nil {
			done <- e
			return
		}
		if req.Action != "browser_session_image" || req.Token != manager.token ||
			req.Root || req.Approval || req.Workspace != manager.cfg.WorkspaceDir ||
			req.SessionID != "legacy-browser" {
			done <- fmt.Errorf("wrong old Executor wire envelope: %+v", req)
			return
		}
		done <- json.NewEncoder(c).Encode(executor.Response{Error: "unknown action"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := manager.SessionImage(ctx, SessionOptions{
		Workspace: manager.cfg.WorkspaceDir, SessionID: "legacy-browser", ImageOperation: "capture",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("old Executor transport did not finish")
	}
	if resp.OK || resp.ErrorCode != "browser_image_unavailable" || resp.Output != "" {
		t.Fatalf("old Executor was mistaken for a staged JPEG: %+v", resp)
	}
}
