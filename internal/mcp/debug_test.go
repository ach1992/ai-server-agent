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

// This exercises public MCP schemas/principal propagation without starting
// Delve, executing a debuggee, or granting access to the real executor.
func TestPublicDebugMCPPrincipalAndBoundedTypedArguments(t *testing.T) {
	cfg := testConfig(t, "bearer")
	dir, err := os.MkdirTemp("/tmp", "asa-debug-wire-")
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
	received := make(chan executor.Request, 8)
	errs := make(chan error, 1)
	go func() {
		for i := 0; i < 5; i++ {
			conn, err := ln.Accept()
			if err != nil {
				errs <- err
				return
			}
			var req executor.Request
			if err = json.NewDecoder(conn).Decode(&req); err != nil {
				_ = conn.Close()
				errs <- err
				return
			}
			received <- req
			_ = json.NewEncoder(conn).Encode(executor.Response{OK: true, Status: "fixture"})
			_ = conn.Close()
		}
	}()
	agent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(agent.Handler())
	defer httpSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	h := httpSrv.Client()
	h.Transport = bearerRoundTripper{base: h.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "debug-public-proof", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: httpSrv.URL + cfg.MCPPath, HTTPClient: h}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	catalog, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{"debug_adapter_status": false, "debug_launch": false, "debug_breakpoints": false, "debug_configure": false, "debug_continue": false, "debug_next": false, "debug_threads": false, "debug_stack": false, "debug_scopes": false, "debug_variables": false, "debug_evaluate": false, "debug_stop": false}
	for _, tool := range catalog.Tools {
		if _, ok := wanted[tool.Name]; ok {
			wanted[tool.Name] = true
		}
		if tool.Name == "debug_evaluate" {
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
				t.Fatal("side-effecting DAP evaluate must be labeled destructive")
			}
		}
	}
	for name, ok := range wanted {
		if !ok {
			t.Fatalf("public debugger missing tool %s", name)
		}
	}
	tasks := []struct {
		name, action, method string
		args                 map[string]any
	}{
		{"debug_adapter_status", "debug_adapter_status", "", map[string]any{}},
		{"debug_launch", "debug_launch", "", map[string]any{"workspace": "/srv/ai-workspace/worktree", "program": "bin/demo", "file_version": "exact-binary-version", "stop_on_entry": true}},
		{"debug_breakpoints", "debug_action", "breakpoints", map[string]any{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_abc", "path": "main.go", "file_version": "exact-source-version", "lines": []int{4, 8}}},
		{"debug_evaluate", "debug_action", "evaluate", map[string]any{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_abc", "frame_id": 1000, "expression": "1+2"}},
		{"debug_stop", "debug_stop", "", map[string]any{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio_abc"}},
	}
	for _, task := range tasks {
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: task.name, Arguments: task.args})
		if err != nil || result.IsError {
			t.Fatalf("%s failed public schema: %v %+v", task.name, err, result)
		}
		select {
		case req := <-received:
			if req.Action != task.action || req.DebugMethod != task.method || req.Root || req.Approval || req.PrincipalID != "direct-default" || req.PrincipalClass != "direct" {
				t.Fatalf("%s lost enforced worker/principal/method identity: action=%s method=%s root=%t approval=%t principal=%s/%s", task.name, req.Action, req.DebugMethod, req.Root, req.Approval, req.PrincipalID, req.PrincipalClass)
			}
			if task.name == "debug_launch" && (req.DebugProgram != "bin/demo" || req.FileVersion != "exact-binary-version" || !req.DebugStopOnEntry) {
				t.Fatalf("launch contract changed: %+v", req)
			}
			if task.name == "debug_breakpoints" && (req.Path != "main.go" || req.FileVersion != "exact-source-version" || len(req.DebugLines) != 2) {
				t.Fatalf("source identity/breakpoints lost: %+v", req)
			}
			if task.name == "debug_evaluate" && (req.DebugExpression != "1+2" || req.DebugFrameID != 1000) {
				t.Fatalf("evaluation contract lost: %+v", req)
			}
		case err := <-errs:
			t.Fatalf("executor fake: %v", err)
		case <-ctx.Done():
			t.Fatalf("tool %s timed out: %v", task.name, ctx.Err())
		}
	}
}
