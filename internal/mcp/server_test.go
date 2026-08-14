package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func testConfig(t *testing.T, authMode string) config.Config {
	t.Helper()
	d := t.TempDir()
	et := filepath.Join(d, "exec")
	bt := filepath.Join(d, "mcp")
	if err := os.WriteFile(et, []byte("exec-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bt, []byte("mcp-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.ExecutorToken = et
	c.BearerTokenFile = bt
	c.StateDir = d
	c.LogDir = d
	c.WorkspaceDir = d
	c.AuthMode = authMode
	return c
}

func TestOfficialSDKCanDiscoverTools(t *testing.T) {
	cfg := testConfig(t, "none")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx := context.Background()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "ai-server-agent-test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: ts.URL + cfg.MCPPath}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	res, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) < 10 {
		t.Fatalf("got %d tools, want at least 10", len(res.Tools))
	}
	found := false
	for _, tool := range res.Tools {
		if tool.Name == "agent_environment" {
			found = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatal("agent_environment must advertise readOnlyHint")
			}
		}
	}
	if !found {
		t.Fatal("agent_environment tool missing")
	}
}

func TestBearerAuthRejectsMissingToken(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, cfg.MCPPath, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHealthDoesNotRequireMCPAuth(t *testing.T) {
	cfg := testConfig(t, "bearer")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, cfg.HealthPath, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want %d", w.Code, http.StatusOK)
	}
}
