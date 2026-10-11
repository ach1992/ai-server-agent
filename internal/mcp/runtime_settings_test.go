package mcp

import (
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
