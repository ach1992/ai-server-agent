package browser

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

func TestIsolatedManagedSandboxAgentForwardsExactOptIn(t *testing.T) {
	manager := testBrowserManager(t)
	writeReadyBrowserFixture(t, manager)
	manager.token = "test-executor-token"
	socket := filepath.Join(t.TempDir(), "sandbox-executor.sock")
	manager.cfg.ExecutorSocket = socket
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan executor.Request, 1)
	done := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		var req executor.Request
		if e = json.NewDecoder(c).Decode(&req); e != nil {
			done <- e
			return
		}
		got <- req
		done <- json.NewEncoder(c).Encode(executor.Response{OK: true, Status: "ready", SessionID: "test-isolated"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := manager.SessionOpen(ctx, SessionOptions{
		Workspace: manager.cfg.WorkspaceDir, ProfileMode: "isolated", ChromiumSandbox: true,
	})
	if err != nil || !response.OK {
		t.Fatalf("manager Browser sandbox opt-in transport: %+v %v", response, err)
	}
	select {
	case req := <-got:
		if req.Action != "browser_session_open" || req.BrowserProfileMode != "isolated" ||
			!req.BrowserSandbox || !strings.Contains(req.Content, "const chromiumSandbox = true") {
			t.Fatalf("opt-in lost between public Manager and trusted Executor: %+v", req)
		}
	case <-ctx.Done():
		t.Fatal("Executor fixture received no sandboxed launch request")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Executor fixture did not complete")
	}
}
