package mcp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestOperatorHTTPTimeoutsAppliedConsistently(t *testing.T) {
	c := config.Default()
	c.Runtime.HTTPReadHeaderTimeoutSeconds = 22
	c.Runtime.HTTPIdleTimeoutSeconds = 360
	server := newHTTPServer(c, nil)
	if server.ReadHeaderTimeout != 22*time.Second || server.IdleTimeout != 360*time.Second {
		t.Fatalf("HTTP timeouts ignored: header=%v idle=%v", server.ReadHeaderTimeout, server.IdleTimeout)
	}
	c.Runtime.HTTPReadHeaderTimeoutSeconds = 10
	c.Runtime.HTTPIdleTimeoutSeconds = 90
	server = newHTTPServer(c, nil)
	if server.ReadHeaderTimeout != 10*time.Second || server.IdleTimeout != 90*time.Second {
		t.Fatalf("legacy timeouts changed: header=%v idle=%v", server.ReadHeaderTimeout, server.IdleTimeout)
	}
}

func TestOperatorSynchronousCommandTimeoutPropagated(t *testing.T) {
	c := config.Default()
	s := Server{cfg: c}
	if got := s.commandTimeoutMS(); got != 300000 {
		t.Fatalf("legacy command timeout = %d", got)
	}
	c.Runtime.CommandTimeoutSeconds = 900
	s = Server{cfg: c}
	if got := s.commandTimeoutMS(); got != 900000 {
		t.Fatalf("operator command timeout ignored: %d", got)
	}
}

func TestOperatorTextFallbackBudgetAndStructuredCompleteness(t *testing.T) {
	c := config.Default()
	raw := strings.Repeat("content", 4000)
	resp := executor.Response{OK: true, Status: "read", Output: raw, OutputEncoding: "utf-8"}
	c.Runtime.TextFallbackBytes = 4096
	s := Server{cfg: c}
	result, structured, err := s.responseResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content[0].(*mcpsdk.TextContent).Text; strings.Contains(text, raw) || !strings.Contains(text, "structuredContent") {
		t.Fatalf("small client budget did not omit oversized fallback: %q", text)
	}
	if structured.Output != raw {
		t.Fatal("small text budget changed structured content")
	}

	c.Runtime.TextFallbackBytes = 65536
	s = Server{cfg: c}
	result, structured, err = s.responseResult(resp)
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content[0].(*mcpsdk.TextContent).Text; !strings.Contains(text, raw) {
		t.Fatal("higher operator fallback budget did not reveal supported output")
	}
	if structured.Output != raw {
		t.Fatal("higher text budget changed structured content")
	}
}

func TestConfiguredSynchronousTimeoutExposedInRealMCPToolCatalog(t *testing.T) {
	// Tool descriptions are consumed by the model, not just by a Go caller.
	// Prove nondefault low and high values as well as the legacy default.
	for _, seconds := range []int{10, 300, 900, 1800} {
		t.Run(fmt.Sprintf("%d_seconds", seconds), func(t *testing.T) {
			cfg := testConfig(t, "bearer")
			cfg.Runtime.CommandTimeoutSeconds = seconds
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			server, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			httpServer := httptest.NewServer(server.Handler())
			defer httpServer.Close()
			client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "settings-metadata-test", Version: "v0"}, nil)
			httpClient := httpServer.Client()
			httpClient.Transport = bearerRoundTripper{base: httpClient.Transport, token: "mcp-token"}
			session, err := client.Connect(context.Background(), &mcpsdk.StreamableClientTransport{
				Endpoint:   httpServer.URL + cfg.MCPPath,
				HTTPClient: httpClient,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			for _, tool := range tools.Tools {
				if tool.Name != "run_command" && tool.Name != "run_root_command" {
					continue
				}
				found[tool.Name] = true
				for _, expected := range []string{
					fmt.Sprintf("limited to %d seconds", seconds),
					"operator-configured", "default 300 seconds",
					"supported 10..1800 seconds", "start_job",
				} {
					if !strings.Contains(tool.Description, expected) {
						t.Fatalf("%s tool description does not reflect configured timeout (%d): missing %q in %q", tool.Name, seconds, expected, tool.Description)
					}
				}
				if strings.Contains(tool.Description, "bounded to five minutes") {
					t.Fatalf("%s still advertises obsolete fixed five-minute timeout", tool.Name)
				}
			}
			if !found["run_command"] || !found["run_root_command"] {
				t.Fatalf("tool catalog lacks command tools: %v", found)
			}
		})
	}
}
