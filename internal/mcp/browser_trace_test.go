package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// The entire public MCP -> Agent Manager -> Executor-wire contract must
// remain navigable beyond the former 512 KiB offset ceiling and the
// 128-call limit. This uses a controlled fake Executor so that every public
// request and response shape is exercised without accessing a real Browser
// or privileged production Executor; the real Node worker has a separate
// pinned-Chromium full-window acceptance.
func TestBrowserSessionTraceDirectMCPFullLargeZIPContinuation(t *testing.T) {
	size := 1<<20 + 32768
	if os.Getenv("ASA_BROWSER_TRACE_NEAR_MAX_ACCEPTANCE") == "1" {
		size = 16<<20 - 4096
	}
	raw := make([]byte, size)
	copy(raw, []byte{'P', 'K', 3, 4})
	for i := 4; i < len(raw); i++ {
		raw[i] = byte(i*131 + (i >> 8))
	}
	sum := sha256.Sum256(raw)
	version := "sha256:" + hex.EncodeToString(sum[:])
	cfg := testConfig(t, "bearer")
	dir := t.TempDir()
	cfg.ExecutorSocket = filepath.Join(dir, "executor.sock")
	ln, err := net.Listen("unix", cfg.ExecutorSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	const workspace = "/srv/ai-workspace/reviewed-worktree"
	const sessionID = "stdio_large_public_trace"
	// Only one test-owned fake Executor is reachable through this private
	// Unix socket; the real Manager preflight and MCP authorization still run.
	requests := make(chan executor.Request, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			c, er := ln.Accept()
			if er != nil {
				return
			}
			var req executor.Request
			if er := json.NewDecoder(c).Decode(&req); er != nil {
				_ = c.Close()
				return
			}
			var opts struct {
				Operation   string `json:"operation"`
				Offset      int64  `json:"offset"`
				FileVersion string `json:"file_version"`
			}
			_ = json.Unmarshal([]byte(req.Content), &opts)
			var resp executor.Response
			switch opts.Operation {
			case "record":
				total := int64(len(raw))
				resp = executor.Response{OK: true, SessionID: sessionID, Status: "recorded",
					MIMEType: "application/zip", FileSize: &total,
					FileVersion: version, OutputEncoding: "json", Output: "{\"ok\":true}", BytesSeen: total}
			case "read":
				off, total := opts.Offset, int64(len(raw))
				if opts.FileVersion != version || off < 0 || off > total {
					resp = executor.Response{Error: "trace stale", ErrorCode: "trace_stale", ErrorClass: "state"}
				} else {
					next := min(total, off+8192)
					window := raw[int(off):int(next)]
					eof := next == total
					resp = executor.Response{OK: true, SessionID: sessionID, Status: "window",
						MIMEType: "application/zip", FileSize: &total, FileVersion: version,
						RequestedOffset: off, Offset: &off, NextOffset: &next, EOF: &eof,
						Truncated: !eof, BytesSeen: total, BytesReturned: int64(len(window)),
						OutputEncoding: "base64", Output: base64.StdEncoding.EncodeToString(window)}
				}
			case "discard":
				resp = executor.Response{OK: true, SessionID: sessionID, Status: "discarded"}
			default:
				resp = executor.Response{Error: "unexpected operation", ErrorCode: "invalid_browser_trace"}
			}
			if er := json.NewEncoder(c).Encode(resp); er != nil {
				_ = c.Close()
				return
			}
			_ = c.Close()
			requests <- req
		}
	}()
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(srv.Handler())
	defer web.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	hc := web.Client()
	hc.Transport = bearerRoundTripper{base: hc.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "large-trace-mcp-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{
		Endpoint: web.URL + cfg.MCPPath, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(op string, offset int64, v string) executor.Response {
		t.Helper()
		args := map[string]any{"workspace": workspace, "session_id": sessionID, "operation": op}
		if op == "record" {
			args["steps"] = []map[string]any{{"action": "snapshot"}}
		} else {
			args["offset"] = offset
			args["file_version"] = v
		}
		out, er := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "browser_session_trace", Arguments: args})
		if er != nil {
			t.Fatalf("MCP %s offset %d: %v", op, offset, er)
		}
		select {
		case req := <-requests:
			if req.Action != "browser_session_trace" || req.Workspace != workspace ||
				req.SessionID != sessionID || req.PrincipalID != "direct-default" ||
				req.PrincipalClass != "direct" || req.Root || req.Approval ||
				req.Token != "exec-token" {
				t.Fatalf("wrong authenticated Agent->Executor boundary at offset %d: %+v", offset, req)
			}
			if op == "read" && !strings.Contains(req.Content, fmt.Sprintf("\"offset\":%d", offset)) {
				t.Fatalf("Manager lost read cursor %d: %s", offset, req.Content)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		js, er := json.Marshal(out.StructuredContent)
		if er != nil {
			t.Fatal(er)
		}
		var parsed executor.Response
		if er := json.Unmarshal(js, &parsed); er != nil {
			t.Fatalf("MCP result %s cannot decode: %v %s", op, er, js)
		}
		return parsed
	}
	first := call("record", 0, "")
	if !first.OK || first.FileVersion != version || first.FileSize == nil ||
		*first.FileSize != int64(size) || first.BytesReturned != 0 {
		t.Fatalf("large record metadata lost at public boundary: %+v", first)
	}
	var complete bytes.Buffer
	count := 0
	for offset := int64(0); offset < int64(size); {
		got := call("read", offset, version)
		window, er := base64.StdEncoding.Strict().DecodeString(got.Output)
		if er != nil || !got.OK || got.BytesReturned != int64(len(window)) ||
			got.Offset == nil || *got.Offset != offset || got.NextOffset == nil ||
			*got.NextOffset != offset+int64(len(window)) ||
			!bytes.Equal(window, raw[int(offset):int(*got.NextOffset)]) ||
			got.FileVersion != version || got.FileSize == nil || *got.FileSize != int64(size) ||
			got.EOF == nil || *got.EOF != (*got.NextOffset == int64(size)) {
			t.Fatalf("wrong public stream cursor %d: response=%+v err=%v", offset, got, er)
		}
		complete.Write(window)
		offset = *got.NextOffset
		count++
	}
	sha := sha256.Sum256(complete.Bytes())
	if count <= 128 || count*8192 < size || complete.Len() != size ||
		hex.EncodeToString(sha[:]) != strings.TrimPrefix(version, "sha256:") {
		t.Fatalf("incomplete public ZIP: bytes=%d calls=%d expected=%d", complete.Len(), count, size)
	}
	// An exact-size cursor is an empty EOF window, not a request for an
	// extra action nor a skipped/misreported missing range.
	if eof := call("read", int64(size), version); !eof.OK || eof.BytesReturned != 0 ||
		eof.Output != "" || eof.NextOffset == nil || *eof.NextOffset != int64(size) ||
		eof.EOF == nil || !*eof.EOF {
		t.Fatalf("public exact-end cursor not represented as EOF: %+v", eof)
	}
	if stale := call("read", 0, "sha256:"+strings.Repeat("f", 64)); stale.OK ||
		stale.ErrorCode != "trace_stale" || stale.Output != "" {
		t.Fatalf("public stale read produced bytes/success: %+v", stale)
	}
	// The Manager must reject a cursor beyond the advertised 16 MiB
	// resource bound before sending anything to the Executor socket.
	invalid, er := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "browser_session_trace", Arguments: map[string]any{
			"workspace": workspace, "session_id": sessionID,
			"operation": "read", "offset": int64(16<<20) + 1, "file_version": version,
		}})
	if er != nil {
		t.Fatal(er)
	}
	invalidJSON, er := json.Marshal(invalid.StructuredContent)
	if er != nil || !strings.Contains(string(invalidJSON), "invalid_browser_trace") {
		t.Fatalf("out-of-range public offset unexpectedly forwarded: %s %v", invalidJSON, er)
	}
	if discarded := call("discard", 0, version); !discarded.OK || discarded.Status != "discarded" {
		t.Fatalf("public trace discard lost: %+v", discarded)
	}
}
