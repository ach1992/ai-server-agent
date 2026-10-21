package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

// A slow Executor reply must not hold the Manager's install/preflight mutex
// across distinct Browser launches. The Executor owns their true lease.
func TestConcurrentIsolatedAgentCallsForwardTypedMode(t *testing.T) {
	manager := testBrowserManager(t)
	writeReadyBrowserFixture(t, manager)
	manager.token = "test-executor-token"
	path := filepath.Join(t.TempDir(), "broker.sock")
	manager.cfg.ExecutorSocket = path
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	received := make(chan [2]executor.Request, 1)
	allow := make(chan struct{})
	fail := make(chan error, 1)
	go func() {
		var requests [2]executor.Request
		var connections [2]net.Conn
		for i := 0; i < len(connections); i++ {
			conn, err := ln.Accept()
			if err != nil {
				fail <- err
				return
			}
			connections[i] = conn
			if err := json.NewDecoder(conn).Decode(&requests[i]); err != nil {
				fail <- err
				return
			}
		}
		received <- requests
		<-allow
		for _, conn := range connections {
			_ = json.NewEncoder(conn).Encode(executor.Response{OK: true, Status: "opened", SessionID: "fixture-id"})
			_ = conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	results := make(chan error, 2)
	go func() {
		resp, err := manager.SessionOpen(ctx, SessionOptions{Workspace: manager.cfg.WorkspaceDir, ProfileMode: "isolated"})
		if err == nil && !resp.OK {
			err = fmt.Errorf("session open: %+v", resp)
		}
		results <- err
	}()
	go func() {
		resp, err := manager.Run(ctx, RunOptions{Script: "console.log('ok')", ProfileMode: "isolated", TimeoutMS: 5000})
		if err == nil && !resp.OK {
			err = fmt.Errorf("one-shot run: %+v", resp)
		}
		results <- err
	}()
	select {
	case reqs := <-received:
		var opened, run bool
		for _, req := range reqs {
			if req.BrowserProfileMode != "isolated" {
				t.Errorf("untyped/mismatched mode: %+v", req)
			}
			if req.Action == "browser_session_open" {
				opened = true
			}
			if req.Action == "run" && req.AuditAction == "browser_run" {
				run = true
			}
		}
		if !opened || !run {
			t.Errorf("missing open/run pair: %+v", reqs)
		}
	case err := <-fail:
		t.Fatalf("fixture broker: %v", err)
	case <-ctx.Done():
		t.Fatal("Agent Manager serialized Browser RPCs; second request could not reach Executor")
	}
	close(allow)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent Agent call: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("Broker reply failed to release Browser caller")
		}
	}
}
