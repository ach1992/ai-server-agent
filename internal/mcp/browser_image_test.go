package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercises the actual bearer-authenticated Streamable HTTP MCP envelope. The
// sole fake Executor is on a private Unix socket; no installed host changes.
// Capture must be metadata-only; a default 8 KiB Base64 read must explain
// how to recover a working 2 KiB text window at the minimum 4 KiB budget.
// No native client file rendering or installed host behavior is assumed.
func TestBrowserImageDirectMCPStagingWindowsAndDiscard(t *testing.T) {
	cfg := testConfig(t, "bearer")
	cfg.Runtime.TextFallbackBytes = 4096
	// Linux Unix pathname sockets have a 108-byte path limit; keep the
	// private socket short even if CI uses a long disk-backed TMPDIR.
	socketDir, err := os.MkdirTemp("/tmp", "asa-img-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Error(err)
		}
	})
	cfg.ExecutorSocket = filepath.Join(socketDir, "exec.sock")
	ln, err := net.Listen("unix", cfg.ExecutorSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	raw := append(append([]byte{0xff, 0xd8, 0xff}, bytes.Repeat([]byte("SYNTHETIC_JPEG_WINDOW"), 520)...), 0xff, 0xd9)
	sum := sha256.Sum256(raw)
	version := "sha256:" + hex.EncodeToString(sum[:])
	size := int64(len(raw))
	requests := make(chan executor.Request, 5)
	errors := make(chan error, 1)
	go func() {
		for n := 0; n < 5; n++ {
			c, er := ln.Accept()
			if er != nil {
				errors <- er
				return
			}
			var req executor.Request
			if er = json.NewDecoder(c).Decode(&req); er != nil {
				errors <- er
				_ = c.Close()
				return
			}
			requests <- req
			var resp executor.Response
			switch n {
			case 0:
				resp = executor.Response{OK: true, Status: "staged", SessionID: "stdio_image_fixture",
					MIMEType: "image/jpeg", FileVersion: version, FileSize: &size, BytesSeen: size,
					CaptureQuality: 70, RegionVerified: true, CaptureWidth: 1024, CaptureHeight: 720}
			case 1, 2, 3:
				offset := int64(0)
				limit := 8192
				if n == 2 {
					limit = 2048 // retries the SAME requested offset on text omission
				}
				if n == 3 {
					offset = size - 32 // final small window must have eof=true
					limit = 2048
				}
				chunk := raw[offset:]
				if len(chunk) > limit {
					chunk = chunk[:limit]
				}
				next, eof := offset+int64(len(chunk)), offset+int64(len(chunk)) == size
				resp = executor.Response{OK: true, Status: "window", SessionID: "stdio_image_fixture",
					MIMEType: "image/jpeg", FileVersion: version, FileSize: &size,
					RequestedOffset: offset, Offset: &offset, NextOffset: &next, EOF: &eof,
					BytesSeen: int64(len(chunk)), BytesReturned: int64(len(chunk)),
					OutputEncoding: "base64", Output: base64.StdEncoding.EncodeToString(chunk)}
			case 4:
				resp = executor.Response{OK: true, Status: "discarded", SessionID: "stdio_image_fixture"}
			}
			if er = json.NewEncoder(c).Encode(resp); er != nil {
				errors <- er
				_ = c.Close()
				return
			}
			_ = c.Close()
		}
	}()
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(srv.Handler())
	defer web.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	hc := web.Client()
	hc.Transport = bearerRoundTripper{base: hc.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "image-direct-client-fixture", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: web.URL + cfg.MCPPath, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	args := []map[string]any{
		{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_image_fixture", "operation": "capture", "quality": 70, "max_width": 1024, "max_height": 720},
		{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_image_fixture", "operation": "read", "file_version": version, "offset": 0, "limit": 8192},
		{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_image_fixture", "operation": "read", "file_version": version, "offset": 0, "limit": 2048},
		{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_image_fixture", "operation": "read", "file_version": version, "offset": size - 32, "limit": 2048},
		{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_image_fixture", "operation": "discard", "file_version": version},
	}
	for i, arg := range args {
		result, er := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "browser_session_image", Arguments: arg})
		if er != nil {
			t.Fatal(er)
		}
		var req executor.Request
		select {
		case req = <-requests:
		case er = <-errors:
			t.Fatal(er)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if i == 1 && !strings.Contains(req.Content, `"limit":8192`) {
			t.Fatalf("initial large read limit was not forwarded: %s", req.Content)
		}
		if i == 2 && (!strings.Contains(req.Content, `"limit":2048`) ||
			!strings.Contains(req.Content, `"offset":0`)) {
			t.Fatalf("retry lost the original offset or reduced limit: %s", req.Content)
		}
		if req.Action != "browser_session_image" || req.PrincipalID != "direct-default" || req.PrincipalClass != "direct" ||
			req.Token != "exec-token" || req.Root || req.Approval ||
			req.Workspace != "/srv/ai-workspace/worktree" || req.SessionID != "stdio_image_fixture" ||
			!strings.Contains(req.Content, `"operation":"`+arg["operation"].(string)+`"`) {
			t.Fatalf("MCP image call lost owner or request identity: %+v", req)
		}
		if result.IsError || len(result.Content) != 1 {
			t.Fatalf("MCP image call %d: %+v", i, result)
		}
		printed, ok := result.Content[0].(*mcpsdk.TextContent)
		if !ok || len(printed.Text) > 4096 {
			t.Fatalf("image window model output size invalid: %+v", result.Content)
		}
		meta, er := json.Marshal(result.StructuredContent)
		if er != nil {
			t.Fatal(er)
		}
		if i == 0 && (!strings.Contains(string(meta), version) ||
			strings.Contains(string(meta), base64.StdEncoding.EncodeToString(raw)) ||
			!strings.Contains(printed.Text, "staged")) {
			t.Fatalf("capture should return metadata and no JPEG bytes: %s / %s", meta, printed.Text)
		}
		if i == 1 && (!strings.Contains(printed.Text, "limit<=2048") ||
			strings.Contains(printed.Text, "limit<=4096") ||
			strings.Contains(printed.Text, base64.StdEncoding.EncodeToString(raw[:8192])) ||
			!strings.Contains(printed.Text, version) ||
			!strings.Contains(printed.Text, "next_offset=8192") ||
			!strings.Contains(string(meta), base64.StdEncoding.EncodeToString(raw[:8192]))) {
			t.Fatalf("oversized text-only Base64 must preserve structured data and provide usable retry instructions: %s", printed.Text)
		}
		if i == 2 && (!strings.Contains(printed.Text, base64.StdEncoding.EncodeToString(raw[:2048])) ||
			!strings.Contains(printed.Text, version) ||
			!strings.Contains(printed.Text, `"next_offset": 2048`) ||
			!strings.Contains(printed.Text, `"eof": false`)) {
			t.Fatalf("2048-byte retried read did not expose Base64 and pinned cursor: %s", printed.Text)
		}
		if i == 3 && (!strings.Contains(printed.Text, base64.StdEncoding.EncodeToString(raw[size-32:])) ||
			!strings.Contains(printed.Text, `"eof": true`) ||
			!strings.Contains(printed.Text, version)) {
			t.Fatalf("exact EOF read lost data or version: %s", printed.Text)
		}
		if i == 4 && (!strings.Contains(printed.Text, `"status": "discarded"`) ||
			strings.Contains(printed.Text, base64.StdEncoding.EncodeToString(raw[:2048]))) {
			t.Fatalf("discard leaked data or obscured cleanup: %s", printed.Text)
		}
	}
}

// The configurable minimum text fallback is only 4096 bytes. A client can
// request 2048 raw image bytes to keep the complete Base64 AND the pinned
// version/EOF metadata inside that envelope without depending on
// structuredContent or a native image renderer.
func TestBrowserImageMinimumTextFallbackUsesRequestedWindowSize(t *testing.T) {
	// Both reads must refer to the same SHA-pinned staged object.
	raw := bytes.Repeat([]byte{0x9a}, 2048)
	big := append(append([]byte(nil), raw...), bytes.Repeat([]byte{0x67}, 8192)...)
	sum := sha256.Sum256(big)
	version := "sha256:" + hex.EncodeToString(sum[:])
	off, next, size, eof := int64(0), int64(8192), int64(len(big)), false
	resp := executor.Response{OK: true, Status: "window", MIMEType: "image/jpeg",
		FileVersion: version, FileSize: &size, RequestedOffset: 0, Offset: &off,
		NextOffset: &next, EOF: &eof, BytesSeen: 8192, BytesReturned: 8192,
		Output: base64.StdEncoding.EncodeToString(big[:8192]), OutputEncoding: "base64"}
	// The initial 8192-byte window must offer usable encoding-aware retry
	// advice; the generic 4096-raw-byte suggestion cannot fit after Base64.
	oversized, structured, err := responseResultWithLimit(resp, 4096)
	if err != nil || len(oversized.Content) != 1 {
		t.Fatalf("large image fallback: %+v, %v", oversized, err)
	}
	advice, ok := oversized.Content[0].(*mcpsdk.TextContent)
	if !ok || len(advice.Text) > 4096 ||
		!strings.Contains(advice.Text, "limit<=2048") ||
		strings.Contains(advice.Text, "limit<=4096") ||
		strings.Contains(advice.Text, resp.Output) ||
		!strings.Contains(advice.Text, version) ||
		!strings.Contains(advice.Text, "next_offset") ||
		structured.Output != resp.Output {
		t.Fatalf("large Base64 fallback did not offer a working pinned retry: %+v", oversized.Content)
	}
	// The client follows the advice from the same initial offset and gets
	// both the exact bytes and next offset/version in the same 4096-byte
	// text envelope. EOF must remain false until the actual last window.
	resp.Output = base64.StdEncoding.EncodeToString(raw)
	resp.BytesSeen, resp.BytesReturned = int64(len(raw)), int64(len(raw))
	next = int64(len(raw))
	resp.FileSize = &size
	resp.NextOffset = &next
	resp.EOF = &eof
	result, _, err := responseResultWithLimit(resp, 4096)
	if err != nil || len(result.Content) != 1 {
		t.Fatalf("small image window result: %+v, %v", result, err)
	}
	text, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok || len(text.Text) > 4096 ||
		!strings.Contains(text.Text, resp.Output) ||
		!strings.Contains(text.Text, version) ||
		!strings.Contains(text.Text, `"next_offset": 2048`) ||
		!strings.Contains(text.Text, `"eof": false`) {
		t.Fatalf("minimum text-only fallback dropped resumable image data: %+v", result.Content)
	}
}
