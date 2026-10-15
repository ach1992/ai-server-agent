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
	ProfileMode       string `json:"profile_mode,omitempty" jsonschema:"shared (default) or isolated ephemeral Browser cookies/localStorage, with one global admission"`
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

type BrowserSessionTraceInput struct {
	Workspace   string             `json:"workspace" jsonschema:"Exact authenticated Browser session workspace"`
	SessionID   string             `json:"session_id" jsonschema:"Principal-bound managed Browser session ID"`
	Operation   string             `json:"operation" jsonschema:"record: run a single <=30s flow with ZIP trace; read: return at most 8192 raw bytes as base64; discard: explicitly erase cached ZIP"`
	Steps       []browser.FlowStep `json:"steps,omitempty" jsonschema:"1..12 browser_e2e steps, only for record; these can mutate the page and must never be replayed automatically"`
	Offset      int64              `json:"offset,omitempty" jsonschema:"Byte offset for read; use exact next_offset, never infer that output was complete"`
	FileVersion string             `json:"file_version,omitempty" jsonschema:"Required sha256:<64 hex> version returned by record when reading or discarding; stale versions never deliver bytes"`
}

type BrowserSessionFlowInput struct {
	Workspace string             `json:"workspace" jsonschema:"Exact workspace of the Browser session"`
	SessionID string             `json:"session_id" jsonschema:"Opaque Browser session ID returned by browser_session_open"`
	Steps     []browser.FlowStep `json:"steps" jsonschema:"1..12 validated browser_e2e actions; snapshot refs persist across calls until invalidated"`
	TimeoutMS int64              `json:"timeout_ms,omitempty" jsonschema:"Total action timeout 90000ms default, 300000ms maximum"`
}

func (s *Server) registerBrowserSessionTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_open", Description: "Open a workspace-scoped Chromium session using a shared or isolated profile. One Browser session/run at a time. A session_id is not authorization; sessions survive client disconnects, not Executor restarts.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionOpenInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionOpen(ctx, browser.SessionOptions{Workspace: in.Workspace, IgnoreHTTPSErrors: in.IgnoreHTTPSErrors, ProfileMode: in.ProfileMode})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_flow", Description: "Execute 1-12 Browser actions in an existing session; stable element refs can persist across calls but expire on navigation/snapshot/detachment. Never auto-retry uncertain side effects.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionFlowInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionFlow(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID, Steps: in.Steps, TimeoutMS: in.TimeoutMS})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_capture", Description: "Capture a session viewport JPEG (not full-page) as a typed image or base64 fallback. Max 1024x720 CSS pixels, 32 KiB raw JPEG; text-only base64 mode is capped at 16 KiB. Reduce max_width/quality if too_large. Returns SHA256; image may contain sensitive content. No stored file.", Annotations: annotations(true, false, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionCaptureInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		if in.Representation != "" && in.Representation != "image" && in.Representation != "base64" {
			resp := executor.Response{Error: "representation must be image or base64", ErrorCode: "invalid_browser_capture", ErrorClass: "validation"}
			return s.responseResult(resp)
		}
		resp, err := s.browser.SessionCapture(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID, CaptureQuality: in.Quality, CaptureMaxWidth: in.MaxWidth})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		if !resp.OK {
			return s.responseResult(resp)
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(resp.Output)
		if err != nil || resp.MIMEType != "image/jpeg" || len(raw) == 0 || len(raw) > 32<<10 || int64(len(raw)) != resp.BytesReturned {
			return executorTransportErrorResult(errors.New("verified Browser capture payload unavailable"))
		}
		if in.Representation == "base64" {
			if len(raw) > 16<<10 {
				denied := executor.Response{Error: "JPEG exceeds 16 KiB text-only delivery budget; use typed image or retry with quality=15,max_width=320", ErrorCode: "too_large", ErrorClass: "resource", ReasonCode: "too_large", Status: "not_delivered", SessionID: resp.SessionID, FileVersion: resp.FileVersion, MIMEType: "image/jpeg", BytesSeen: int64(len(raw))}
				return s.responseResult(denied)
			}
			return s.responseResult(resp)
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
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_trace", Description: "Record one Browser flow to a private ZIP (max 512 KiB), read base64 windows (max 8192 raw bytes, offset/SHA256), or discard. Recording can change the page: do not replay uncertain actions. May contain sensitive data; held in session memory only until discard/close/expiry.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionTraceInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionTrace(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID, Steps: in.Steps, TraceOperation: in.Operation, TraceOffset: in.Offset, TraceVersion: in.FileVersion})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_status", Description: "Inspect own Browser session running/uncertain state with principal/workspace checks. No profile or page data is returned.", Annotations: annotations(true, false, true, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionStatus(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_close", Description: "Close own managed Browser session; free the shared Browser profile only after the executor verifies pinned process-group termination. An uncertain cleanup remains reserved and requires recovery.", Annotations: annotations(false, true, false, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionClose(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
}
