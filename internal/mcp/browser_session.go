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
	ProfileMode       string `json:"profile_mode,omitempty" jsonschema:"shared (default) uses exclusive shared aiworker profile; isolated uses dedicated aibrowser OS identity, private ephemeral cookies/localStorage and no worker workspace access; ZIP traces require isolated"`
	Workspace         string `json:"workspace" jsonschema:"Exact absolute worker workspace/worktree used to bind session authorization"`
	IgnoreHTTPSErrors bool   `json:"ignore_https_errors,omitempty" jsonschema:"Explicit TLS validation exception for an isolated development site; default is secure HTTPS validation"`
	ChromiumSandbox   bool   `json:"chromium_sandbox,omitempty" jsonschema:"Opt-in Linux Chromium namespace/seccomp sandbox, supported only with profile_mode=isolated; fails closed if unsupported or unverified. Default false for host compatibility."`
}
type BrowserSessionIDInput struct {
	Workspace string `json:"workspace" jsonschema:"Exact workspace used when session was opened"`
	SessionID string `json:"session_id" jsonschema:"Opaque per-principal Browser session ID (not sufficient for authorization alone)"`
}
type BrowserSessionCaptureInput struct {
	Workspace      string `json:"workspace" jsonschema:"Exact workspace of the Browser session"`
	SessionID      string `json:"session_id" jsonschema:"Principal-bound Browser session identity from browser_session_open"`
	Quality        int    `json:"quality,omitempty" jsonschema:"JPEG quality 15..70, default 40; with fit_quality=true this is the maximum permitted quality"`
	FitQuality     bool   `json:"fit_quality,omitempty" jsonschema:"Optional, default false: try at most three decreasing JPEG qualities in one bounded capture if over 32 KiB; preserves requested viewport crop and reports actual capture_quality; if all fail, returns too_large"`
	MaxWidth       int    `json:"max_width,omitempty" jsonschema:"Optional viewport crop width 320..1024 CSS pixels (default 640), never rescaled"`
	MaxHeight      int    `json:"max_height,omitempty" jsonschema:"Optional viewport crop height 1..720 CSS pixels (default 720); smaller regions help fit detailed pages within 32 KiB"`
	CropX          int    `json:"crop_x,omitempty" jsonschema:"Optional nonnegative viewport-relative left offset in CSS pixels, default 0; must be inside the current viewport"`
	CropY          int    `json:"crop_y,omitempty" jsonschema:"Optional nonnegative viewport-relative top offset in CSS pixels, default 0; must be inside the current viewport"`
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
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_open", Description: "Open a principal/workspace-bound Chromium session. Default shared uses the exclusive worker profile; isolated uses a dedicated Browser OS identity and private scratch without worker workspace file access. Trace ZIPs require isolated mode. chromium_sandbox=true explicitly requests verified Chromium OS sandboxing only for isolated sessions; incompatible hosts fail closed (user namespaces, seccomp, AppArmor). Default off; shared Browser unchanged. session_id alone is not authorization; sessions survive client disconnects, not Executor restart.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionOpenInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionOpen(ctx, browser.SessionOptions{Workspace: in.Workspace, IgnoreHTTPSErrors: in.IgnoreHTTPSErrors, ProfileMode: in.ProfileMode, ChromiumSandbox: in.ChromiumSandbox})
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
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_capture", Description: "Capture a viewport JPEG, max 1024x720 pixels and 32 KiB raw. fit_quality=true tries at most three lower qualities. Optional crop_x/crop_y/max_height select a bounded viewport region (no resizing, no full-page image); unsupported older runners/Executors explicitly refuse regions. Defaults unchanged. Typed image or <=16 KiB base64 text fallback; reduce max_width if still too_large. Returns SHA256; sensitive image is never stored as a file.", Annotations: annotations(true, false, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionCaptureInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		if in.Representation != "" && in.Representation != "image" && in.Representation != "base64" {
			resp := executor.Response{Error: "representation must be image or base64", ErrorCode: "invalid_browser_capture", ErrorClass: "validation"}
			return s.responseResult(resp)
		}
		resp, err := s.browser.SessionCapture(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID, CaptureQuality: in.Quality, CaptureMaxWidth: in.MaxWidth, CaptureMaxHeight: in.MaxHeight, CaptureCropX: in.CropX, CaptureCropY: in.CropY, CaptureFitQuality: in.FitQuality})
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
		summary := fmt.Sprintf("Browser JPEG screenshot captured: %d bytes, %s, quality=%d, digest %s. If no image is visible in this client, retry with representation=base64, quality=15 and max_width=320 (text-only cap 16 KiB). No persistent artifact was created.", len(raw), resp.MIMEType, resp.CaptureQuality, resp.FileVersion)
		if resp.RegionVerified {
			summary = fmt.Sprintf("Browser viewport region x=%d,y=%d,width=%d,height=%d; %s", resp.CaptureCropX, resp.CaptureCropY, resp.CaptureWidth, resp.CaptureHeight, summary)
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: summary}, &mcpsdk.ImageContent{Data: raw, MIMEType: "image/jpeg"}}}, meta, nil
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_trace", Description: "Only for isolated Browser sessions: record one flow to an OS-private ZIP (max 16 MiB on disk), read SHA256-pinned base64 windows (8192 raw bytes per call; up to 8192 finite reads per session, separately from 128 Browser actions), or discard. Recording may change the page: never replay uncertain actions. The ZIP is retained only within the Browser session; no public URL or whole-ZIP memory copy.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionTraceInput) (*mcpsdk.CallToolResult, executor.Response, error) {
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
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "browser_session_close", Description: "Close own managed Browser session; free this Browser session's exact shared/isolated admission reservation only after the executor verifies pinned process-group termination. An uncertain cleanup remains reserved and requires recovery.", Annotations: annotations(false, true, false, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in BrowserSessionIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := s.browser.SessionClose(ctx, browser.SessionOptions{Workspace: in.Workspace, SessionID: in.SessionID})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
}
