package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
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

func makeClientCaptureJPEG(t *testing.T, large bool) []byte {
	t.Helper()
	edge := 48
	if large {
		edge = 256
	}
	img := image.NewRGBA(image.Rect(0, 0, edge, edge))
	rng := rand.New(rand.NewSource(17))
	for y := 0; y < edge; y++ {
		for x := 0; x < edge; x++ {
			img.Set(x, y, color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 40}); err != nil {
		t.Fatal(err)
	}
	if large && (b.Len() <= 16<<10 || b.Len() > 32<<10) {
		t.Fatalf("invalid test JPEG boundary size=%d", b.Len())
	}
	return b.Bytes()
}

// Official MCP SDK verifies typed image vs text-visible base64 and rejection
// of text delivery over budget without disclosing/duplicating image content
// in structured metadata. The executor fixture preserves server-derived
// principal/workspace authority and uses authenticated Unix transport.
func TestBrowserSessionCaptureMCP(t *testing.T) {
	cfg := testConfig(t, "bearer")
	dir, err := os.MkdirTemp("/tmp", "asa-capture-sdk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg.ExecutorSocket = filepath.Join(dir, "executor.sock")
	ln, err := net.Listen("unix", cfg.ExecutorSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	small, large := makeClientCaptureJPEG(t, false), makeClientCaptureJPEG(t, true)
	observed := make(chan executor.Request, 4)
	serverErrors := make(chan error, 1)
	go func() {
		for i := 0; i < 4; i++ {
			c, er := ln.Accept()
			if er != nil {
				serverErrors <- er
				return
			}
			var req executor.Request
			if er = json.NewDecoder(c).Decode(&req); er != nil {
				_ = c.Close()
				serverErrors <- er
				return
			}
			observed <- req
			payload := small
			if i >= 2 {
				payload = large
			}
			sha := sha256.Sum256(payload)
			r := executor.Response{OK: true, Status: "captured", SessionID: "stdio_image_fixture", MIMEType: "image/jpeg", OutputEncoding: "base64", Output: base64.StdEncoding.EncodeToString(payload), BytesSeen: int64(len(payload)), BytesReturned: int64(len(payload)), FileVersion: "sha256:" + hex.EncodeToString(sha[:])}
			if er = json.NewEncoder(c).Encode(r); er != nil {
				serverErrors <- er
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
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "typed-screenshot-acceptance", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: web.URL + cfg.MCPPath, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(mode string) *mcpsdk.CallToolResult {
		t.Helper()
		got, er := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "browser_session_capture", Arguments: map[string]any{
			"workspace": "/srv/ai-workspace/reviewed-worktree", "session_id": "stdio_image_fixture",
			"quality": 40, "max_width": 640, "representation": mode,
		}})
		if er != nil {
			t.Fatal(er)
		}
		select {
		case req := <-observed:
			if req.Action != "browser_session_capture" || req.Token != "exec-token" || req.PrincipalID != "direct-default" || req.PrincipalClass != "direct" || req.Root || req.Approval || req.Workspace != "/srv/ai-workspace/reviewed-worktree" || req.SessionID != "stdio_image_fixture" || !strings.Contains(req.Content, `"max_width":640`) {
				t.Fatalf("public screenshot identity/bounds lost: %+v", req)
			}
		case e := <-serverErrors:
			t.Fatal(fmt.Errorf("executor fixture: %w", e))
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return got
	}
	for _, test := range []struct {
		mode      string
		payload   []byte
		wantError bool
		wantTyped bool
	}{
		{"image", small, false, true},
		{"base64", small, false, false},
		{"base64", large, true, false},
		{"image", large, false, true},
	} {
		got := call(test.mode)
		if got.IsError != test.wantError {
			t.Fatalf("mode=%s len=%d IsError=%t want %t", test.mode, len(test.payload), got.IsError, test.wantError)
		}
		js, err := json.Marshal(got.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		encoded := base64.StdEncoding.EncodeToString(test.payload)
		if test.wantTyped {
			if len(got.Content) != 2 {
				t.Fatalf("typed image must include metadata text and image: %+v", got.Content)
			}
			item, ok := got.Content[1].(*mcpsdk.ImageContent)
			if !ok || item.MIMEType != "image/jpeg" || !bytes.Equal(item.Data, test.payload) {
				t.Fatalf("missing/invalid typed JPEG: %T", got.Content[1])
			}
			if strings.Contains(string(js), encoded) || !strings.Contains(string(js), `"mime_type":"image/jpeg"`) {
				t.Fatalf("structured screenshot leaked base64 or lost MIME metadata: %s", js)
			}
			text := got.Content[0].(*mcpsdk.TextContent).Text
			if len(text) > 700 || !strings.Contains(text, "base64") {
				t.Fatalf("fallback guidance missing: %q", text)
			}
		} else if test.wantError {
			if len(got.Content) != 1 || !strings.Contains(string(js), "too_large") || strings.Contains(string(js), encoded) {
				t.Fatalf("text-only oversize disclosure or silent success: %+v", got)
			}
		} else {
			if len(got.Content) != 1 || !strings.Contains(string(js), encoded) || len(got.Content[0].(*mcpsdk.TextContent).Text) > 32<<10 {
				t.Fatalf("bounded base64 text fallback incomplete: %+v", got)
			}
		}
	}
}
