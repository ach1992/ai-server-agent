package mcp

import (
	"context"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRequestCorrelationMiddlewareSeedsToolsCallAtMCPEntry(t *testing.T) {
	var captured string
	next := func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
		if id, ok := executor.RequestCorrelationID(ctx); ok {
			captured = id
		}
		return nil, nil
	}
	handler := requestCorrelationMiddleware()(next)
	if _, err := handler(context.Background(), "tools/call", nil); err != nil {
		t.Fatal(err)
	}
	if captured == "" {
		t.Fatal("tools/call entered handler without request correlation")
	}
}

func TestRequestCorrelationMiddlewareLeavesNonToolMethodsUncorrelated(t *testing.T) {
	var correlated bool
	next := func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
		_, correlated = executor.RequestCorrelationID(ctx)
		return nil, nil
	}
	handler := requestCorrelationMiddleware()(next)
	if _, err := handler(context.Background(), "ping", nil); err != nil {
		t.Fatal(err)
	}
	if correlated {
		t.Fatal("non-tool method unexpectedly received action correlation")
	}
}
