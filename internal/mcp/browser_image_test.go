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
// Capture must be metadata-only and 8 KiB image windows must remain visible
// as text with SHA/EOF, without assuming native client file rendering.
func TestBrowserImageDirectMCPStagingWindowsAndDiscard(t *testing.T) {
	cfg := testConfig(t, "bearer")
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
	raw := []byte("\xff\xd8\xffSYNTHETIC_BOUNDED_JPEG_BYTES\xff\xd9")
	sum := sha256.Sum256(raw)
	version := "sha256:" + hex.EncodeToString(sum[:])
	size := int64(len(raw))
	requests := make(chan executor.Request, 3)
	errors := make(chan error, 1)
	go func() {
		for n := 0; n < 3; n++ {
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
			case 1:
				next, off, eof := size, int64(0), true
				resp = executor.Response{OK: true, Status: "window", SessionID: "stdio_image_fixture",
					MIMEType: "image/jpeg", FileVersion: version, FileSize: &size,
					Offset: &off, NextOffset: &next, EOF: &eof, BytesSeen: size, BytesReturned: size,
					OutputEncoding: "base64", Output: base64.StdEncoding.EncodeToString(raw)}
			case 2:
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
		{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_image_fixture", "operation": "read", "file_version": version, "offset": 0, "limit": 2048},
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
		if i == 1 && !strings.Contains(req.Content, `"limit":2048`) {
			t.Fatalf("read limit was not forwarded: %s", req.Content)
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
		if !ok || len(printed.Text) > 32<<10 {
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
		if i == 1 && (!strings.Contains(printed.Text, base64.StdEncoding.EncodeToString(raw)) ||
			!strings.Contains(printed.Text, version) || !strings.Contains(printed.Text, `"eof": true`)) {
			t.Fatalf("read did not expose bounded model-visible bytes and offset: %s", printed.Text)
		}
		if i == 2 && (!strings.Contains(printed.Text, `"status": "discarded"`) ||
			strings.Contains(printed.Text, base64.StdEncoding.EncodeToString(raw))) {
			t.Fatalf("discard leaked data or obscured cleanup: %s", printed.Text)
		}
	}
}

// The configurable minimum text fallback is only 4096 bytes. A client can
// request 2048 raw image bytes to keep the complete Base64 AND the pinned
// version/EOF metadata inside that envelope without depending on
// structuredContent or a native image renderer.
func TestBrowserImageMinimumTextFallbackUsesRequestedWindowSize(t *testing.T) {
	raw := bytes.Repeat([]byte{0x9a}, 2048)
	sum := sha256.Sum256(raw)
	version := "sha256:" + hex.EncodeToString(sum[:])
	off, next, size, eof := int64(0), int64(len(raw)), int64(len(raw)), true
	resp := executor.Response{OK: true, Status: "window", MIMEType: "image/jpeg",
		FileVersion: version, FileSize: &size, RequestedOffset: 0, Offset: &off,
		NextOffset: &next, EOF: &eof, BytesSeen: size, BytesReturned: size,
		Output: base64.StdEncoding.EncodeToString(raw), OutputEncoding: "base64"}
	result, _, err := responseResultWithLimit(resp, 4096)
	if err != nil || len(result.Content) != 1 {
		t.Fatalf("small image window result: %+v, %v", result, err)
	}
	text, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok || len(text.Text) > 4096 ||
		!strings.Contains(text.Text, resp.Output) ||
		!strings.Contains(text.Text, version) ||
		!strings.Contains(text.Text, `"eof": true`) {
		t.Fatalf("minimum text-only fallback dropped resumable image data: %+v", result.Content)
	}
}
