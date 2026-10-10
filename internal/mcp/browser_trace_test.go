package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Real go-sdk Streamable HTTP tool calls prove the authorized direct-client
// envelope and bounded model-visible ZIP windows, not installed ChatGPT UI.
func TestBrowserSessionTraceDirectMCPWindowAndVersion(t *testing.T) {
	cfg := testConfig(t, "bearer")
	root := t.TempDir()
	cfg.ExecutorSocket = filepath.Join(root, "executor.sock")
	ln, err := net.Listen("unix", cfg.ExecutorSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	raw := []byte("PK\x03\x04minimal deterministic trace fixture for model-visible windows")
	h := sha256.Sum256(raw)
	version := "sha256:" + hex.EncodeToString(h[:])
	accepted := make(chan executor.Request, 3)
	serverErrs := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			c, er := ln.Accept()
			if er != nil {
				serverErrs <- er
				return
			}
			var req executor.Request
			if er = json.NewDecoder(c).Decode(&req); er != nil {
				serverErrs <- er
				_ = c.Close()
				return
			}
			accepted <- req
			var r executor.Response
			switch i {
			case 0:
				size := int64(len(raw))
				r = executor.Response{OK: true, Status: "recorded", SessionID: "stdio_trace_fixture", MIMEType: "application/zip",
					FileSize: &size, FileVersion: version, OutputEncoding: "json", Output: `{"ok":true}`, BytesSeen: size}
			case 1:
				size, next := int64(len(raw)), int64(len(raw))
				eof := true
				off := int64(0)
				r = executor.Response{OK: true, Status: "window", SessionID: "stdio_trace_fixture", MIMEType: "application/zip",
					FileSize: &size, FileVersion: version, OutputEncoding: "base64", Output: base64.StdEncoding.EncodeToString(raw),
					BytesSeen: size, BytesReturned: size, RequestedOffset: 0, Offset: &off, NextOffset: &next, EOF: &eof}
			case 2:
				r = executor.Response{OK: true, Status: "discarded", SessionID: "stdio_trace_fixture"}
			}
			if er = json.NewEncoder(c).Encode(r); er != nil {
				serverErrs <- er
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hc := web.Client()
	hc.Transport = bearerRoundTripper{base: hc.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "trace-direct-client-fixture", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: web.URL + cfg.MCPPath, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for i, args := range []map[string]any{
		{"workspace": "/srv/ai-workspace/reviewed-worktree", "session_id": "stdio_trace_fixture", "operation": "record",
			"steps": []map[string]any{{"action": "snapshot"}}},
		{"workspace": "/srv/ai-workspace/reviewed-worktree", "session_id": "stdio_trace_fixture", "operation": "read",
			"file_version": version, "offset": 0},
		{"workspace": "/srv/ai-workspace/reviewed-worktree", "session_id": "stdio_trace_fixture", "operation": "discard",
			"file_version": version},
	} {
		result, er := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "browser_session_trace", Arguments: args})
		if er != nil {
			t.Fatal(er)
		}
		var req executor.Request
		select {
		case req = <-accepted:
		case er = <-serverErrs:
			t.Fatal(er)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if req.Action != "browser_session_trace" || req.PrincipalClass != "direct" ||
			req.PrincipalID != "direct-default" || req.Root || req.Approval ||
			req.Workspace != "/srv/ai-workspace/reviewed-worktree" || req.SessionID != "stdio_trace_fixture" ||
			req.Token != "exec-token" {
			t.Fatalf("trace call lost direct owner identity: %+v", req)
		}
		if !strings.Contains(req.Content, fmt.Sprintf(`"operation":"%s"`, args["operation"])) {
			t.Fatalf("trace request operation missing: %s", req.Content)
		}
		if result.IsError || len(result.Content) != 1 {
			t.Fatalf("direct client trace result %d: %+v", i, result)
		}
		printed, ok := result.Content[0].(*mcpsdk.TextContent)
		if !ok || len(printed.Text) > 32<<10 {
			t.Fatalf("oversize/typed-only trace: %+v", result.Content)
		}
		js, er := json.Marshal(result.StructuredContent)
		if er != nil {
			t.Fatal(er)
		}
		if i == 0 && (!strings.Contains(string(js), version) || strings.Contains(string(js), base64.StdEncoding.EncodeToString(raw))) {
			t.Fatalf("metadata-only record disclosed raw ZIP or omitted digest: %s", js)
		}
		if i == 1 && (!strings.Contains(printed.Text, base64.StdEncoding.EncodeToString(raw)) ||
			!strings.Contains(printed.Text, version) || !strings.Contains(printed.Text, `"eof": true`)) {
			t.Fatalf("model-visible binary window missing data/version/EOF: %s", printed.Text)
		}
		if i == 2 && (!strings.Contains(printed.Text, `"status": "discarded"`) ||
			strings.Contains(printed.Text, base64.StdEncoding.EncodeToString(raw))) {
			t.Fatalf("discard response exposed bytes or hid status: %s", printed.Text)
		}
	}
}
