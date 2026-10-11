package mcp

import (
	"context"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Debug tools deliberately expose only the accepted Go/Delve control loop;
// there is no generic raw DAP command/socket API or privileged attach.
type DebugLaunchInput struct {
	Workspace   string `json:"workspace" jsonschema:"Exact absolute worker workspace/worktree directory"`
	Program     string `json:"program" jsonschema:"Workspace-relative, prebuilt regular executable; mode=exec only (build separately through run_command)"`
	FileVersion string `json:"file_version" jsonschema:"Exact executable version from workspace_stat; stale executables are rejected"`
	StopOnEntry bool   `json:"stop_on_entry,omitempty" jsonschema:"Stop when the debugged Go program begins"`
}
type DebugOwnerInput struct {
	Workspace string `json:"workspace" jsonschema:"Exact absolute workspace used at debug_launch"`
	SessionID string `json:"session_id" jsonschema:"Opaque authenticated debug session ID"`
}
type DebugBreakpointsInput struct {
	Workspace   string `json:"workspace"`
	SessionID   string `json:"session_id"`
	Path        string `json:"path" jsonschema:"Workspace-relative .go source path"`
	FileVersion string `json:"file_version" jsonschema:"Exact current source version from workspace_stat"`
	Lines       []int  `json:"lines" jsonschema:"1-based source lines, max 32; an empty array removes breakpoints"`
}
type DebugThreadInput struct {
	Workspace string `json:"workspace"`
	SessionID string `json:"session_id"`
	ThreadID  int    `json:"thread_id" jsonschema:"Positive thread ID returned by debug_threads/stopped event"`
}
type DebugFrameInput struct {
	Workspace string `json:"workspace"`
	SessionID string `json:"session_id"`
	FrameID   int    `json:"frame_id" jsonschema:"Frame ID returned by debug_stack"`
}
type DebugVariablesInput struct {
	Workspace          string `json:"workspace"`
	SessionID          string `json:"session_id"`
	VariablesReference int    `json:"variables_reference" jsonschema:"Positive variable reference returned by debug_scopes/variables"`
}
type DebugEvaluateInput struct {
	Workspace  string `json:"workspace"`
	SessionID  string `json:"session_id"`
	FrameID    int    `json:"frame_id"`
	Expression string `json:"expression" jsonschema:"Bounded Go expression (1..512 bytes). Evaluation can execute target code and cause side effects; never automatic read-only inspection"`
}

func (s *Server) registerDebugTools() {

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_adapter_status", Description: "Discover availability and narrow supported Go/Delve mode without launching or installing tooling. Root attach is unavailable in v1.", Annotations: annotations(true, false, true, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_adapter_status"})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_launch", Description: "Launch a real Go/Delve DAP session as aiworker in the exact workspace, using an already-built executable and matching file_version. Requires optional admin-owned Delve executable. No attach, no root, no unapproved terminal handoff. Next: set breakpoints, then debug_configure. Debugging changes runtime behavior.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugLaunchInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_launch", Workspace: in.Workspace, DebugProgram: in.Program, FileVersion: in.FileVersion, DebugStopOnEntry: in.StopOnEntry})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_breakpoints", Description: "Set/remove Go source breakpoints with an exact worker source file_version, before debug_configure. This may alter the debugged process execution.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugBreakpointsInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "breakpoints", Workspace: in.Workspace, SessionID: in.SessionID, Path: in.Path, FileVersion: in.FileVersion, DebugLines: in.Lines})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_evaluate", Description: "Evaluate a user-approved expression in a stopped Go/Delve process. This can execute code or cause side effects; do not treat as read-only or automatically retry after uncertainty.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugEvaluateInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "evaluate", Workspace: in.Workspace, SessionID: in.SessionID, DebugFrameID: in.FrameID, DebugExpression: in.Expression})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_variables", Description: "Inspect bounded Go variables (max 40) by Delve variables_reference; does not read source paths provided by the adapter.", Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugVariablesInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "variables", Workspace: in.Workspace, SessionID: in.SessionID, DebugReference: in.VariablesReference})
	})
	for _, x := range []struct {
		name, method, description string
		readOnly                  bool
	}{
		{"debug_configure", "configure", "Finish breakpoint configuration and run/stop-on-entry via real DAP configurationDone.", false},
		{"debug_threads", "threads", "Inspect threads of the current Delve session (requires configured session).", true},
	} {
		item := x
		mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: item.name, Description: item.description, Annotations: annotations(item.readOnly, !item.readOnly, item.readOnly, !item.readOnly)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugOwnerInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: item.method, Workspace: in.Workspace, SessionID: in.SessionID})
		})
	}
	for _, x := range []struct {
		name, method, description string
		readOnly                  bool
	}{
		{"debug_continue", "continue", "Continue a halted Go program; changes target execution state.", false},
		{"debug_pause", "pause", "Pause running debuggee; changes target execution state.", false},
		{"debug_next", "next", "Step over a statement; changes target execution state.", false},
		{"debug_step_in", "stepIn", "Step into a function; changes target execution state.", false},
		{"debug_step_out", "stepOut", "Step out of a frame; changes target execution state.", false},
		{"debug_stack", "stackTrace", "Inspect up to 20 stack frames for a selected thread.", true},
		{"debug_exception_info", "exceptionInfo", "Inspect the adapter exception info for a selected thread when available.", true},
	} {
		item := x
		mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: item.name, Description: item.description, Annotations: annotations(item.readOnly, !item.readOnly, item.readOnly, !item.readOnly)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugThreadInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: item.method, Workspace: in.Workspace, SessionID: in.SessionID, DebugThreadID: in.ThreadID})
		})
	}
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_scopes", Description: "Inspect variable scopes of a stopped Go stack frame.", Annotations: annotations(true, false, true, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugFrameInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "scopes", Workspace: in.Workspace, SessionID: in.SessionID, DebugFrameID: in.FrameID})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_status", Description: "Inspect current debug session state and bounded asynchronous DAP events, including explicit dropped-events accounting. Does not imply the debuggee terminated on client disconnect.", Annotations: annotations(true, false, false, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugOwnerInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_status", Workspace: in.Workspace, SessionID: in.SessionID})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_stop", Description: "Request Delve disconnect+terminateDebuggee then stop the executor-owned worker session. Uncertain adapter disconnection is reported, not silently called successful cleanup.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugOwnerInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_stop", Workspace: in.Workspace, SessionID: in.SessionID})
	})
}

func (s *Server) callDebug(ctx context.Context, req executor.Request) (*mcpsdk.CallToolResult, executor.Response, error) {
	response, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, req)
	if err != nil {
		return executorTransportErrorResult(err)
	}
	return s.responseResult(response)
}
