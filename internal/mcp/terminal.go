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
}
type TerminalWriteInput struct {
	SessionEpoch string `json:"session_epoch,omitempty" jsonschema:"Per-attachment output/control epoch from terminal_open/reconnect; reject stale epochs after executor restart"`
	Root         bool   `json:"root,omitempty" jsonschema:"Must match the authority mode of the opened session"`
	Approval     bool   `json:"approval,omitempty" jsonschema:"Explicit approval for root-mode terminal operation; always false for worker"`
	Workspace    string `json:"workspace" jsonschema:"Exact workspace used when the terminal was opened"`
	SessionID    string `json:"session_id" jsonschema:"Opaque session identity"`
	Content      string `json:"content" jsonschema:"Literal terminal input (including newline or control bytes), max 4096 UTF-8 bytes"`
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
		Name: "terminal_open", Description: "Open a real persistent tmux Control Mode-backed aiworker PTY in an explicit workspace. Requires optional /usr/bin/tmux; does not install anything or emulate interactive execution as separate shell commands. Default aiworker; real root PTY requires root=true AND explicit approval=true, and separate owner/operator authorization for the privileged effect. Returns session ID for cross-call use.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalOpenInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_open", Workspace: input.Workspace, Columns: input.Columns, Rows: input.Rows, Root: input.Root, Approval: input.Approval})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_read", Description: "Read bounded incremental binary-safe terminal output, base64 encoded, with next_cursor/earliest/latest and explicit retention_truncated/disconnected state. Does not resend consumed output when passing next_cursor. Read-only, but the terminal process itself may continue executing.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalReadInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_read", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch, Cursor: input.Cursor, Limit: input.Limit})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_write", Description: "Send literal input into the existing worker or explicitly approved root PTY across MCP calls, not through a one-shot command. Input and output are not stored in audit logs. A confirmed control acknowledgement does not prove the shell command succeeded; inspect terminal_read.",
		Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalWriteInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_write", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch, Content: input.Content})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_interrupt", Description: "Deliver Ctrl-C to an authenticated worker or explicitly approved root tmux PTY; not a separate shell invocation.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_interrupt", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_resize", Description: "Resize the existing real tmux PTY window, preserving the same workspace, principal and session.", Annotations: annotations(false, false, false, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalResizeInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_resize", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch, Columns: input.Columns, Rows: input.Rows})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_reconnect", Description: "Reattach Control Mode to a root/executor-owned tmux runtime record after executor restart. Requires the SAME authenticated principal, explicit workspace and authority as the original session; verifies tmux socket, pane and generation. Returns a fresh session_epoch, with previous incremental output explicitly unavailable. Does not create a new terminal shell.",
		Annotations: annotations(false, false, false, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_reconnect", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "terminal_close", Description: "Kill the entire private tmux server (including any extra panes or sessions) and close its executor-owned worker/root control connection. An uncertain close is reported, not treated as proven cleanup.", Annotations: annotations(false, true, false, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input TerminalIDInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "terminal_close", Workspace: input.Workspace, SessionID: input.SessionID, Root: input.Root, Approval: input.Approval, SessionEpoch: input.SessionEpoch})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
}
