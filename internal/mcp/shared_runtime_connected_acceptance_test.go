package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Opt-in black-box SDK connection against the source-built candidate's real
// standalone MCP process. No privileged Executor, installed service, runtime
// state or public network listener is touched by this test.
func TestSharedRuntimeIsolatedConnectedMCPCatalog(t *testing.T) {
	host, _ := os.Hostname()
	binary := os.Getenv("ASA137_E2E_BINARY")
	if host != "hDE01" || os.Getenv("ASA137_CLIENT_HOST") != host || binary == "" {
		t.Skip("explicit opt-in source-built MCP client acceptance on hDE01")
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("candidate executable not present: %v", err)
	}
	// Reserve an ephemeral loopback port to minimize collisions with existing
	// services. The listener is held by this test only until startup.
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	_ = reservation.Close()
	cfg := testConfig(t, "bearer")
	cfg.ListenAddress = addr
	cfg.CredentialStoreFile = ""
	cfg.MCPPath = "/mcp"
	cfgPath := filepath.Join(t.TempDir(), "client-test-config.json")
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	server := exec.Command(binary, "-config", cfgPath, "serve")
	var stderr strings.Builder
	server.Stderr = &stderr
	server.Stdout = &stderr
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- server.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = server.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(6 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("isolated MCP listener not ready: %v; log=%s", dialErr, stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "asa137-isolated-client", Version: "v1"}, nil)
	httpClient := &http.Client{Timeout: 12 * time.Second, Transport: bearerRoundTripper{base: http.DefaultTransport, token: "mcp-token"}}
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: "http://" + addr + cfg.MCPPath, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatalf("real source client connect: %v; log=%s", err, stderr.String())
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("native tools/list: %v", err)
	}
	expected := map[string][]string{
		"run_command":          {"command", "workspace"},
		"start_job":            {"command", "workspace", "kind", "label", "operation_id"},
		"runtime_list":         {"workspace", "kind"},
		"browser_run":          {"script", "profile_mode"},
		"browser_e2e":          {"steps", "profile_mode"},
		"browser_session_open": {"workspace", "profile_mode"},
	}
	seen := map[string]bool{}
	for _, tool := range listed.Tools {
		fields, ok := expected[tool.Name]
		if !ok {
			continue
		}
		wire, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range fields {
			if !strings.Contains(string(wire), "\""+name+"\"") {
				t.Fatalf("real client %s lacks %s input: %s", tool.Name, name, wire)
			}
		}
		if tool.Name == "runtime_list" && (tool.Annotations == nil ||
			!tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint) {
			t.Fatal("runtime_list is not represented as passive in native MCP catalog")
		}
		seen[tool.Name] = true
	}
	for name := range expected {
		if !seen[name] {
			t.Fatalf("real native client could not discover %s", name)
		}
	}
	t.Logf("real source-built MCP client connected at loopback, exposed %d tools and all six shared-runtime schemas", len(listed.Tools))
}
