package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Real MCP SDK -> authenticated HTTP -> executor Unix-wire contract. The
// socket endpoint is an inert in-process fixture, not a root executor, so
// this exercises privilege metadata forwarding without any privileged effect.
func TestTerminalMCPAuthorizationEpochForwarding(t *testing.T) {
	cfg := testConfig(t, "bearer")
	tmp, err := os.MkdirTemp("/tmp", "asa-mcp-wire-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	cfg.ExecutorSocket = filepath.Join(tmp, "executor.sock")
	ln, err := net.Listen("unix", cfg.ExecutorSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan executor.Request, 8)
	listenerErr := make(chan error, 1)
	go func() {
		defer close(received)
		for n := 0; n < 7; n++ {
			conn, err := ln.Accept()
			if err != nil {
				listenerErr <- err
				return
			}
			var req executor.Request
			err = json.NewDecoder(conn).Decode(&req)
			if err != nil {
				_ = conn.Close()
				listenerErr <- err
				return
			}
			received <- req
			_ = json.NewEncoder(conn).Encode(executor.Response{OK: true, Status: "wire_proof"})
			_ = conn.Close()
		}
	}()
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(server.Handler())
	defer httpSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "root-terminal-boundary-proof", Version: "v0"}, nil)
	h := httpSrv.Client()
	h.Transport = bearerRoundTripper{base: h.Transport, token: "mcp-token"}
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: httpSrv.URL + cfg.MCPPath, HTTPClient: h}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	riskTools := map[string]bool{"terminal_open": false, "terminal_write": false, "terminal_interrupt": false, "terminal_close": false}
	for _, tool := range tools.Tools {
		if _, ok := riskTools[tool.Name]; ok {
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Fatalf("mutating/root-capable terminal tool %s has incorrect risk annotation", tool.Name)
			}
			riskTools[tool.Name] = true
		}
	}
	for name, found := range riskTools {
		if !found {
			t.Fatalf("missing terminal tool %s", name)
		}
	}
	shared := map[string]any{
		"workspace": "/srv/ai-workspace/project", "session_id": "stdio_00000000000000000000000000000001",
		"session_epoch": "root-epoch-proof", "root": true, "approval": true,
	}
	tasks := []struct {
		name  string
		extra map[string]any
	}{
		{"terminal_open", map[string]any{"columns": 80, "rows": 24}},
		{"terminal_read", map[string]any{"limit": 8192, "cursor": uint64(0)}},
		{"terminal_write", map[string]any{"content": "hello"}},
		{"terminal_interrupt", nil},
		{"terminal_resize", map[string]any{"columns": 65, "rows": 20}},
		{"terminal_reconnect", nil},
		{"terminal_close", nil},
	}
	for _, task := range tasks {
		args := make(map[string]any, len(shared)+len(task.extra))
		for k, v := range shared {
			if task.name == "terminal_open" && (k == "session_id" || k == "session_epoch") {
				continue
			}
			args[k] = v
		}
		for k, v := range task.extra {
			args[k] = v
		}
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: task.name, Arguments: args})
		if err != nil {
			t.Fatalf("%s transport: %v", task.name, err)
		}
		if result.IsError {
			t.Fatalf("%s returned tool error: %+v", task.name, result)
		}
		select {
		case req, ok := <-received:
			if !ok {
				t.Fatalf("%s failed before executor received request", task.name)
			}
			if req.Action != task.name || !req.Root || !req.Approval || req.PrincipalID != "direct-default" || req.PrincipalClass != "direct" {
				t.Fatalf("%s lost authority/principal: action=%s root=%t approval=%t principal=%s/%s", task.name, req.Action, req.Root, req.Approval, req.PrincipalID, req.PrincipalClass)
			}
			if req.Workspace != "/srv/ai-workspace/project" {
				t.Fatalf("%s lost workspace identity", task.name)
			}
			if task.name != "terminal_open" && req.SessionID != shared["session_id"] {
				t.Fatalf("%s lost session identity", task.name)
			}
			if task.name != "terminal_open" && task.name != "terminal_reconnect" && req.SessionEpoch != "root-epoch-proof" {
				t.Fatalf("%s lost root control epoch", task.name)
			}
		case err := <-listenerErr:
			t.Fatalf("%s executor fixture: %v", task.name, err)
		case <-ctx.Done():
			t.Fatalf("%s executor request timed out: %v", task.name, ctx.Err())
		}
	}
}
