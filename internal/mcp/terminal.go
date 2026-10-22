package mcp

import (
	"context"
	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type TerminalOpenInput struct {
	Root      bool   `json:"root,omitempty" jsonschema:"Request real root PTY rather than default aiworker; requires separately approved privileged host action"`
	Approval  bool   `json:"approval,omitempty" jsonschema:"Set true only after explicit authorization for privileged root terminal actions"`
	Workspace string `json:"workspace" jsonschema:"Absolute explicit aiworker worktree directory within the Agent workspace root"`
	Columns   int    `json:"columns" jsonschema:"Terminal columns 20..240"`
	Rows      int    `json:"rows" jsonschema:"Terminal rows 5..80"`
}
type TerminalIDInput struct {
	SessionEpoch string `json:"session_epoch,omitempty" jsonschema:"Per-attachment output/control epoch from terminal_open/reconnect; reject stale epochs after executor restart"`
	Root         bool   `json:"root,omitempty" jsonschema:"Must match the authority mode of the opened session"`
	Approval     bool   `json:"approval,omitempty" jsonschema:"Explicit approval for root-mode terminal operation; always false for worker"`
	Workspace    string `json:"workspace" jsonschema:"Exact workspace used when the terminal was opened"`
	SessionID    string `json:"session_id" jsonschema:"Opaque terminal session ID returned by terminal_open; not an authorization token"`
}
type TerminalReadInput struct {
	SessionEpoch string `json:"session_epoch,omitempty" jsonschema:"Per-attachment output/control epoch from terminal_open/reconnect; reject stale epochs after executor restart"`
	Root         bool   `json:"root,omitempty" jsonschema:"Must match the authority mode of the opened session"`
	Approval     bool   `json:"approval,omitempty" jsonschema:"Explicit approval for root-mode terminal operation; always false for worker"`
	Workspace    string `json:"workspace" jsonschema:"Exact workspace used when the terminal was opened"`
	SessionID    string `json:"session_id" jsonschema:"Opaque session identity"`
	Cursor       uint64 `json:"cursor,omitempty" jsonschema:"Last successfully consumed output cursor; zero reads from available beginning"`
	Limit        int    `json:"limit,omitempty" jsonschema:"Bounded decoded output bytes 4096..32768; default 8192"`
	WaitMS       int    `json:"wait_ms,omitempty" jsonschema:"Optional 0..2000 milliseconds to wait for NEW terminal output at the supplied latest cursor; default 0 reads immediately; at most one waiting reader per session, does not block terminal writes or close"`
}
type TerminalWriteInput struct {
	SessionEpoch string `json:"session_epoch,omitempty" jsonschema:"Per-attachment output/control epoch from terminal_open/reconnect; reject stale epochs after executor restart"`
	Root         bool   `json:"root,omitempty" jsonschema:"Must match the authority mode of the opened session"`
	Approval     bool   `json:"approval,omitempty" jsonschema:"Explicit approval for root-mode terminal operation; always false for worker"`
	Workspace    string `json:"workspace" jsonschema:"Exact workspace used when the terminal was opened"`
	SessionID    string `json:"session_id" jsonschema:"Opaque session identity"`
	Content      string `json:"content" jsonschema:"Literal terminal input including control bytes; max 65536 UTF-8 bytes per call, delivered in ordered bounded tmux chunks; on uncertain failure inspect state before retry"`
}
type TerminalResizeInput struct {
	SessionEpoch string `json:"session_epoch,omitempty" jsonschema:"Per-attachment output/control epoch from terminal_open/reconnect; reject stale epochs after executor restart"`
	Root         bool   `json:"root,omitempty" jsonschema:"Must match the authority mode of the opened session"`
	Approval     bool   `json:"approval,omitempty" jsonschema:"Explicit approval for root-mode terminal operation; always false for worker"`
	Workspace    string `json:"workspace" jsonschema:"Exact workspace used when the terminal was opened"`
	SessionID    string `json:"session_id" jsonschema:"Opaque session identity"`
	Columns      int    `json:"columns" jsonschema:"New width in columns, 20..240"`
	Rows         int    `json:"rows" jsonschema:"New height in rows, 5..80"`
}

func (s *Server) registerTerminalTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_open", Description: "Open a persistent tmux PTY as aiworker in a workspace. root=true requires approval=true and separate operator authorization. Returns session_id for later calls.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalOpenInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_open", Workspace: input.Workspace, Columns: input.Columns, Rows: input.Rows, Root: input.Root, Approval: input.Approval})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_read", Description: "Read incremental base64 PTY output with next_cursor, retention_truncated and disconnected markers; optional wait_ms=0..2000 waits once for new output without blocking terminal writes. The process may still run.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalReadInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		// An old Executor silently discards unknown request fields. Validate
		// the advertised public range before transport, while retaining the
		// Executor's independent validation for direct/older Agents.
		if input.WaitMS < 0 || input.WaitMS > 2000 {
			return s.responseResult(executor.Response{
				Error:      "terminal read wait_ms must be 0..2000 milliseconds",
				ReasonCode: "invalid_wait", ErrorCode: "invalid_wait", ErrorClass: "validation",
				SessionID: input.SessionID, SessionEpoch: input.SessionEpoch,
			})
		}
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_read", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch, Cursor: input.Cursor, Limit: input.Limit, WaitMS: input.WaitMS})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		if input.WaitMS > 0 && resp.OK && !resp.ReadWaitSupported {
			// BASE Executors silently ignore unknown request fields. Their
			// immediate response cannot be represented as an honored wait.
			return s.responseResult(executor.Response{Error: "Executor does not support terminal_read wait_ms; update Executor or omit wait_ms",
				ReasonCode: "terminal_read_wait_unavailable", ErrorCode: "terminal_read_wait_unavailable",
				ErrorClass: "compatibility", SessionID: input.SessionID, SessionEpoch: input.SessionEpoch})
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_write", Description: "Send up to 64 KiB literal input to an existing PTY in ordered bounded chunks; acknowledgements do not prove foreground consumption. On partial/unknown failure inspect state before any retry.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalWriteInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_write", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch, Content: input.Content})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_interrupt", Description: "Deliver Ctrl-C to an authenticated worker or explicitly approved root tmux PTY; not a separate shell invocation.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_interrupt", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_resize", Description: "Resize the existing real tmux PTY window, preserving the same workspace, principal and session.", Annotations: annotations(false, false, false, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalResizeInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_resize", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch, Columns: input.Columns, Rows: input.Rows})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_reconnect", Description: "Reconnect to existing tmux PTY after Executor restart with same principal/workspace. Returns fresh session_epoch; previous incremental output is unavailable. No new shell.",
		Annotations: annotations(false, false, false, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_reconnect", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_close", Description: "Kill the entire private tmux server (including any extra panes or sessions) and close its executor-owned worker/root control connection. An uncertain close is reported, not treated as proven cleanup.", Annotations: annotations(false, true, false, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_close", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
}
