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

// Run the official MCP SDK against the real registered debug tools and an
// authenticated fake Executor socket. The public refusal must preserve the
// Executor's resource/security classification, rather than become success or
// a generic transport/runtime error.
func TestPublicDebugSizeAndSecurityFailureMCP(t *testing.T) {
	cfg := testConfig(t, "bearer")
	dir, err := os.MkdirTemp("/tmp", "asa-dap-errors-")
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
	responses := []executor.Response{
		{ErrorCode: "debug_result_too_large", ReasonCode: "debug_result_too_large", ErrorClass: "resource", Error: "dap_result_too_large: complete initialize response exceeds 131072-byte budget; narrow where supported"},
		{ErrorCode: "debug_result_too_large", ReasonCode: "debug_result_too_large", ErrorClass: "resource", Error: "dap_result_too_large: complete variables response exceeds 131072-byte budget; narrow query"},
		{ErrorCode: "javascript_dap_child_reverse_rejected", ReasonCode: "javascript_dap_child_reverse_rejected", ErrorClass: "security", Error: "unverified Node reverse startDebugging request"},
	}
	observed := make(chan executor.Request, len(responses))
	serverErr := make(chan error, 1)
	go func() {
		for _, response := range responses {
			c, err := ln.Accept()
			if err != nil {
				serverErr <- err
				return
			}
			var req executor.Request
			if err = json.NewDecoder(c).Decode(&req); err != nil {
				_ = c.Close()
				serverErr <- err
				return
			}
			observed <- req
			err = json.NewEncoder(c).Encode(response)
			_ = c.Close()
			if err != nil {
				serverErr <- err
				return
			}
		}
	}()
	agent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(agent.Handler())
	defer httpSrv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	httpClient := httpSrv.Client()
	httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "dap-size-error-proof", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: httpSrv.URL + cfg.MCPPath, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tasks := []struct {
		name string
		args map[string]any
	}{
		{"debug_launch", map[string]any{"workspace": "/srv/ai-workspace/worktree", "program": "bin/demo", "file_version": "v1"}},
		{"debug_variables", map[string]any{"workspace": "/srv/ai-workspace/worktree", "session_id": "stdio-demo", "variables_reference": 1}},
		{"debug_launch", map[string]any{"workspace": "/srv/ai-workspace/worktree", "program": "src/demo.js", "file_version": "v1", "language": "javascript", "mode": "node"}},
	}
	for i, task := range tasks {
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: task.name, Arguments: task.args})
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("%s refusal lost: %+v err=%v", task.name, result, err)
		}
		actual := map[string]any{}
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &actual); err != nil {
			t.Fatal(err)
		}
		expected := responses[i]
		if actual["ok"] != false || actual["error_code"] != expected.ErrorCode || actual["error_class"] != expected.ErrorClass ||
			actual["reason_code"] != expected.ReasonCode || actual["error"] != expected.Error {
			t.Fatalf("MCP structured refusal drifted: %+v", actual)
		}
		if len(result.Content) != 1 {
			t.Fatalf("MCP text fallback missing: %+v", result.Content)
		}
		text, ok := result.Content[0].(*mcpsdk.TextContent)
		if !ok || !strings.Contains(text.Text, expected.ErrorCode) || !strings.Contains(text.Text, expected.ErrorClass) {
			t.Fatalf("MCP text-only result lost diagnosis: %T %v", result.Content[0], text)
		}
		select {
		case req := <-observed:
			if req.Root || req.Approval || req.PrincipalID != "direct-default" || req.PrincipalClass != "direct" {
				t.Fatalf("debug MCP lost worker authorization: %+v", req)
			}
		case err := <-serverErr:
			t.Fatalf("fake executor error: %v", err)
		case <-ctx.Done():
			t.Fatalf("debug MCP fixture stalled: %v", ctx.Err())
		}
	}
	if len(observed) != 0 {
		t.Fatalf("unexpected executor requests: %d", len(observed))
	}
}
