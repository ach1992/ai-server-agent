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

// The ChatGPT-facing root tool can identify a protected host credential
// without sending its plaintext across MCP or the internal Executor socket.
func TestRootCommandPrivateCredentialRefForwardsAuthenticatedIdentity(t *testing.T) {
	cfg := testConfig(t, "bearer")
	d, err := os.MkdirTemp("/tmp", "asa-root-ref-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	cfg.ExecutorSocket = filepath.Join(d, "x.sock")
	ln, err := net.Listen("unix", cfg.ExecutorSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	received := make(chan executor.Request, 1)
	errorsCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errorsCh <- err
			return
		}
		defer conn.Close()
		var request executor.Request
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			errorsCh <- err
			return
		}
		received <- request
		if err := json.NewEncoder(conn).Encode(executor.Response{
			OK: true, Status: "native_completed", Output: "account verified",
		}); err != nil {
			errorsCh <- err
		}
	}()

	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(srv.Handler())
	defer web.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "private-host-credential-qa", Version: "0"}, nil)
	httpClient := web.Client()
	httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: web.URL + cfg.MCPPath, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "run_root_command", Arguments: map[string]any{
		"command":        "IFS= read -r password <&3; printf 'account verified\n'",
		"credential_ref": "svc_example-private-ref",
	}})
	if err != nil || result.IsError {
		t.Fatalf("private root MCP tool returned error: %v %+v", err, result)
	}
	select {
	case req := <-received:
		if req.Action != "run" || !req.Root || req.CredentialRef != "svc_example-private-ref" ||
			req.PrincipalID != "direct-default" || req.PrincipalClass != "direct" ||
			req.Approval || req.TimeoutMS <= 0 {
			t.Fatalf("root/private-binding identity lost: %+v", req)
		}
	case err := <-errorsCh:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
