package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

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
type BrowserSessionCaptureInput struct {
	Workspace      string `json:"workspace" jsonschema:"Exact workspace of the Browser session"`
	SessionID      string `json:"session_id" jsonschema:"Principal-bound Browser session identity from browser_session_open"`
	Quality        int    `json:"quality,omitempty" jsonschema:"Optional JPEG quality 15..70, default 40; reduce to fit model-visible output budget"`
	MaxWidth       int    `json:"max_width,omitempty" jsonschema:"Optional left-edge viewport crop width 320..1024 CSS pixels, default 640, height at most 720; this crops rather than resizes the current viewport"`
	Representation string `json:"representation,omitempty" jsonschema:"image (default): typed MCP image with small text metadata; base64: text-visible JPEG only when raw capture <=16 KiB, otherwise explicit too_large error"`
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
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_capture", Description: "Capture an explicitly requested bounded viewport JPEG of the current authenticated managed Browser session: up to 1024 CSS-pixel width cropped from the current viewport left edge, at most 720 height, never a full-page, resized, or disk artifact. Uses the existing private Browser subprocess/broker and shared profile admission; no file or public resource URL. Default representation=image returns typed MCP image content plus small text/structured metadata with SHA256. If the client hides image content, request representation=base64 after reducing max_width/quality to fit a 16 KiB raw-byte text fallback. Images over the 32 KiB raw cap return too_large; an uncertain transfer poisons the session until close. Screenshots may contain sensitive on-page information; do not call without an intentional user workflow.", Annotations: annotations(true, false, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionCaptureInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		if in.Representation != "" && in.Representation != "image" && in.Representation != "base64" {
			resp := executor.Response{Error: "representation must be image or base64", ErrorCode: "invalid_browser_capture", ErrorClass: "validation"}
			return responseResult(resp)
		}
		resp, err := s.browser.SessionCapture(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID, CaptureQuality: in.Quality, CaptureMaxWidth: in.MaxWidth})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		if !resp.OK {
			return responseResult(resp)
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(resp.Output)
		if err != nil || resp.MIMEType != "image/jpeg" || len(raw) == 0 || len(raw) > 32<<10 || int64(len(raw)) != resp.BytesReturned {
			return executorTransportErrorResult(errors.New("verified Browser capture payload unavailable"))
		}
		if in.Representation == "base64" {
			if len(raw) > 16<<10 {
				denied := executor.Response{Error: "JPEG exceeds 16 KiB text-only delivery budget; use typed image or retry with quality=15,max_width=320", ErrorCode: "too_large", ErrorClass: "resource", ReasonCode: "too_large", Status: "not_delivered", SessionID: resp.SessionID, FileVersion: resp.FileVersion, MIMEType: "image/jpeg", BytesSeen: int64(len(raw))}
				return responseResult(denied)
			}
			return responseResult(resp)
		}
		// Do not duplicate the base64 payload into structuredContent: a
		// text-only AI client should see concise metadata, never a silently
		// omitted or multi-megabyte string represented as complete delivery.
		meta := resp
		meta.Output = ""
		meta.OutputEncoding = "none"
		summary := fmt.Sprintf("Browser JPEG screenshot captured: %d bytes, %s, digest %s. If no image is visible in this client, retry with representation=base64, quality=15 and max_width=320 (text-only cap 16 KiB). No persistent artifact was created.", len(raw), resp.MIMEType, resp.FileVersion)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: summary}, &mcpsdk.ImageContent{Data: raw, MIMEType: "image/jpeg"}}}, meta, nil
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
