package mcp

import (
	"context"

	"github.com/ach1992/ai-server-agent/internal/browser"
	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type BrowserSessionOpenInput struct {
	Workspace         string `json:"workspace" jsonschema:"Exact absolute worker workspace/worktree used to bind session authorization"`
	IgnoreHTTPSErrors bool   `json:"ignore_https_errors,omitempty" jsonschema:"Explicit TLS validation exception for an isolated development site; default is secure HTTPS validation"`
}
type BrowserSessionIDInput struct {
	Workspace string `json:"workspace" jsonschema:"Exact workspace used when session was opened"`
	SessionID string `json:"session_id" jsonschema:"Opaque per-principal Browser session ID (not sufficient for authorization alone)"`
}
type BrowserSessionFlowInput struct {
	Workspace string             `json:"workspace" jsonschema:"Exact workspace of the Browser session"`
	SessionID string             `json:"session_id" jsonschema:"Opaque Browser session ID returned by browser_session_open"`
	Steps     []browser.FlowStep `json:"steps" jsonschema:"1..12 validated browser_e2e actions; snapshot refs persist across calls until invalidated"`
	TimeoutMS int64              `json:"timeout_ms,omitempty" jsonschema:"Total action timeout 90000ms default, 300000ms maximum"`
}

func (s *Server) registerBrowserSessionTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_open", Description: "Open an unprivileged authenticated managed Chromium session in an exact workspace. Uses the existing pinned engine, Agent-wide shared profile, and executor-owned worker session broker. The shared Browser profile admits only one run or session at once; session identity is not an authorization token. Bounded TTL; no second daemon, no unsafe automatic retries.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionOpenInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionOpen(ctx, browser.SessionOptions{Workspace: in.Workspace, IgnoreHTTPSErrors: in.IgnoreHTTPSErrors})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_flow", Description: "Execute 1..12 ordered validated Browser/E2E actions against an existing authenticated session. A complete, stable snapshot may issue bounded exact ElementHandle refs usable in a later MCP call. Refs become stale on navigation, next snapshot, or element detachment. Action failure/unknown completion is not safe to replay automatically.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionFlowInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionFlow(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID, Steps: in.Steps, TimeoutMS: in.TimeoutMS})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_status", Description: "Inspect own Browser session running/uncertain state with principal/workspace checks. No profile or page data is returned.", Annotations: annotations(true, false, true, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionStatus(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_close", Description: "Close own managed Browser session; free the shared Browser profile only after the executor verifies pinned process-group termination. An uncertain cleanup remains reserved and requires recovery.", Annotations: annotations(false, true, false, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionClose(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
}
