package mcp

import (
	"context"
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

// A real SDK client must see a proven failed Browser action as IsError=true,
// while retaining bounded model-visible failure detail and session identity.
// Unlike lost response, a proven failure does not poison the session.
func TestBrowserSessionKnownFlowFailureMCP(t *testing.T) {
	cfg := testConfig(t, "bearer")
	sockDir, err := os.MkdirTemp("/tmp", "asa-browser-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	cfg.ExecutorSocket = filepath.Join(sockDir, "executor.sock")
	listener, err := net.Listen("unix", cfg.ExecutorSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	observed := make(chan executor.Request, 3)
	serverErrors := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			c, err := listener.Accept()
			if err != nil {
				serverErrors <- err
				return
			}
			var req executor.Request
			if err := json.NewDecoder(c).Decode(&req); err != nil {
				_ = c.Close()
				serverErrors <- err
				return
			}
			observed <- req
			var result executor.Response
			switch i {
			case 0:
				result = executor.Response{OK: false, Status: "failed", ReasonCode: "browser_flow_failed", ErrorCode: "browser_flow_failed", ErrorClass: "action", SessionID: "stdio_browser_mcp", Error: "Browser assertion failed", OutputEncoding: "json", Output: `{"ok":false,"failed_step":0,"results":[{"index":0,"action":"assert_text","ok":false}]}`}
			case 1:
				result = executor.Response{OK: true, Status: "result", SessionID: "stdio_browser_mcp", OutputEncoding: "json", Output: `{"ok":true,"failed_step":null,"results":[{"index":0,"action":"assert_text","ok":true}]}`}
			case 2:
				result = executor.Response{OK: false, Status: "uncertain", ReasonCode: "browser_session_action_uncertain", ErrorCode: "browser_session_action_uncertain", ErrorClass: "state", SessionID: "stdio_browser_mcp", Error: "unknown completion; do not retry"}
			}
			if err := json.NewEncoder(c).Encode(result); err != nil {
				serverErrors <- err
				_ = c.Close()
				return
			}
			_ = c.Close()
		}
	}()
	agent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(agent.Handler())
	defer web.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hc := web.Client()
	hc.Transport = bearerRoundTripper{base: hc.Transport, token: "mcp-token"}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "browser-failure-mcp-test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: web.URL + cfg.MCPPath, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	params := &mcpsdk.CallToolParams{Name: "browser_session_flow", Arguments: map[string]any{
		"workspace":  "/srv/ai-workspace/verified-worktree",
		"session_id": "stdio_browser_mcp",
		"steps":      []map[string]any{{"action": "assert_text", "selector": "#target", "expected": "done"}},
	}}
	for i, want := range []struct {
		error  bool
		code   string
		output string
	}{
		{true, "browser_flow_failed", "failed_step"},
		{false, "", "failed_step"},
		{true, "browser_session_action_uncertain", "unknown completion"},
	} {
		got, err := session.CallTool(ctx, params)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.IsError != want.error {
			t.Fatalf("call %d IsError=%v want %v", i, got.IsError, want.error)
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range []string{want.output, "stdio_browser_mcp"} {
			if !strings.Contains(string(data), item) {
				t.Fatalf("call %d lost model-visible %q: %s", i, item, data)
			}
		}
		if want.code != "" && !strings.Contains(string(data), want.code) {
			t.Fatalf("call %d lost error classification %q: %s", i, want.code, data)
		}
		select {
		case req := <-observed:
			if req.Action != "browser_session_flow" || req.Token != "exec-token" || req.PrincipalID != "direct-default" || req.PrincipalClass != "direct" || req.SessionID != "stdio_browser_mcp" || req.Root || req.Approval || req.TimeoutMS <= 0 || !strings.Contains(req.Content, "assert_text") {
				t.Fatalf("call %d server-derived principal/action contract broken: %+v", i, req)
			}
		case e := <-serverErrors:
			t.Fatal(fmt.Errorf("fixture: %w", e))
		case <-ctx.Done():
			t.Fatalf("call %d executor timeout: %v", i, ctx.Err())
		}
	}
}
