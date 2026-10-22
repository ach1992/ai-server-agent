package mcp

import (
	"context"
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

// A new public Agent may briefly talk to the BASE Executor during an upgrade.
// Its reply has no terminal_read_wait_supported bit. A user-requested wait
// must never masquerade as honored after that old Executor ignores wait_ms.
// Ordinary immediate reads remain backward compatible in both directions.
func TestTerminalReadWaitLegacyExecutorCompatibility(t *testing.T) {
	cfg := testConfig(t, "bearer")
	dir, err := os.MkdirTemp("/tmp", "asa-terminal-wait-wire-")
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
	received := make(chan executor.Request, 3)
	serverErrors := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			conn, e := ln.Accept()
			if e != nil {
				serverErrors <- e
				return
			}
			var req executor.Request
			if e = json.NewDecoder(conn).Decode(&req); e != nil {
				_ = conn.Close()
				serverErrors <- e
				return
			}
			received <- req
			// Pre-feature Executor successful read: no support metadata,
			// no explicit optional wait acknowledgement.
			e = json.NewEncoder(conn).Encode(executor.Response{
				OK: true, Status: "running", SessionID: req.SessionID,
				SessionEpoch: req.SessionEpoch, NextCursor: 0, OutputEncoding: "base64",
			})
			_ = conn.Close()
			if e != nil {
				serverErrors <- e
				return
			}
		}
	}()
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(srv.Handler())
	defer web.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	httpClient := web.Client()
	httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "wait-legacy-compat", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: web.URL + cfg.MCPPath, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(ms int, expectForwarded bool) *mcpsdk.CallToolResult {
		t.Helper()
		result, e := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "terminal_read", Arguments: map[string]any{
			"workspace":     "/srv/ai-workspace/proven-owner",
			"session_id":    "stdio_0123456789abcdef0123456789abcdef",
			"session_epoch": "verified-original-epoch",
			"root":          false, "approval": false, "cursor": 0, "limit": 8192,
			"wait_ms": ms,
		}})
		if e != nil {
			t.Fatal(e)
		}
		if !expectForwarded {
			// The pre-feature Executor would ignore both invalid fields and
			// claim success. Invalid public values must never leave Agent.
			select {
			case req := <-received:
				t.Fatalf("invalid public wait_ms=%d was sent to BASE Executor: %+v", ms, req)
			case <-time.After(70 * time.Millisecond):
			}
			return result
		}
		select {
		case req := <-received:
			if req.Action != "terminal_read" || req.WaitMS != ms ||
				req.Workspace != "/srv/ai-workspace/proven-owner" ||
				req.SessionID != "stdio_0123456789abcdef0123456789abcdef" ||
				req.SessionEpoch != "verified-original-epoch" ||
				req.Root || req.Approval || req.PrincipalID != "direct-default" ||
				req.PrincipalClass != "direct" {
				t.Fatalf("wrong authenticated terminal wait request: %+v", req)
			}
		case e := <-serverErrors:
			t.Fatal(e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return result
	}
	for _, invalid := range []int{-1, -2000, 2001, 1 << 20} {
		result := call(invalid, false)
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil || !result.IsError ||
			!strings.Contains(string(raw), "invalid_wait") ||
			!strings.Contains(string(raw), "validation") ||
			strings.Contains(string(raw), `"ok":true`) {
			t.Fatalf("invalid wait_ms=%d accepted at Agent boundary: %s err=%v", invalid, raw, err)
		}
	}
	if normal := call(0, true); normal.IsError {
		t.Fatalf("default immediate read broke against BASE Executor: %+v", normal)
	}
	for _, ms := range []int{1, 2000} {
		wait := call(ms, true)
		raw, err := json.Marshal(wait.StructuredContent)
		if err != nil || !wait.IsError ||
			!strings.Contains(string(raw), "terminal_read_wait_unavailable") ||
			!strings.Contains(string(raw), "compatibility") ||
			strings.Contains(string(raw), `"ok":true`) {
			t.Fatalf("BASE Executor incorrectly satisfied unsupported wait_ms=%d: %s err=%v", ms, raw, err)
		}
	}
}
