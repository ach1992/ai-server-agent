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

// Exercise the actual authenticated MCP SDK envelope and the private Unix
// Executor request. The fake Executor is confined to this test: the source
// fixture does not claim access to an external HTTP or SSH credential store.
func TestServiceUseDirectMCPMaintainsCallerIdentityAndNoPlaintextArgument(t *testing.T) {
	cfg := testConfig(t, "bearer")
	// Keep Unix socket paths short even when Go's TMPDIR uses the data disk.
	d, err := os.MkdirTemp("/tmp", "asa-use-")
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
	requests := make(chan executor.Request, 1)
	errs := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errs <- err
			return
		}
		defer c.Close()
		var req executor.Request
		if err := json.NewDecoder(c).Decode(&req); err != nil {
			errs <- err
			return
		}
		requests <- req
		if err := json.NewEncoder(c).Encode(executor.Response{
			OK: true, Status: "http_200", Output: "authorized-fixture",
			BytesReturned: 18, OutputEncoding: "utf-8",
		}); err != nil {
			errs <- err
		}
	}()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(s.Handler())
	defer web.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "asa214-service-use-fixture", Version: "v0"}, nil)
	httpClient := web.Client()
	httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: web.URL + cfg.MCPPath, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "service_use",
		Arguments: map[string]any{
			"credential_ref":           "svc_nonsecret-fixture",
			"method":                   "PROPFIND",
			"path":                     "/custom/api?window=1",
			"body":                     "{\"user\":{\"password\":\"\"}}",
			"body_secret_ref":          "svc_new-admin-nonsecret",
			"body_secret_json_pointer": "/user/password",
			"auth_mode":                "header",
			"auth_header":              "X-Custom-Key",
			"headers":                  map[string]string{"Accept": "application/json"},
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("authenticated service tool failed: %v %+v", err, result)
	}
	select {
	case req := <-requests:
		if req.Action != "service_use" || req.CredentialRef != "svc_nonsecret-fixture" ||
			req.PrincipalID != "direct-default" || req.PrincipalClass != "direct" ||
			req.Root || req.Approval || req.Token != "exec-token" ||
			req.ServiceUse.Method != "PROPFIND" || req.ServiceUse.AuthMode != "header" ||
			req.ServiceUse.AuthHeader != "X-Custom-Key" ||
			req.ServiceUse.Path != "/custom/api?window=1" || req.ServiceUse.Body != "{\"user\":{\"password\":\"\"}}" ||
			req.ServiceUse.BodySecretRef != "svc_new-admin-nonsecret" || req.ServiceUse.BodySecretJSONPointer != "/user/password" ||
			req.ServiceUse.Headers["Accept"] != "application/json" ||
			req.TimeoutMS <= 0 {
			t.Fatalf("MCP-Executor caller/protocol identity lost: %+v", req)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
