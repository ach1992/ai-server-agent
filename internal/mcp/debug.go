package mcp

import (
	"context"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Debug tools reuse the same focused Go/Delve, Python/debugpy and Node/js-debug DAP control loop;
// there is no generic raw DAP command/socket API or privileged attach.
type DebugAdapterStatusInput struct {
	Language string `json:"language,omitempty" jsonschema:"Optional language: go (default), python or javascript; passive trusted adapter readiness only"`
}
type DebugLaunchInput struct {
	Workspace   string `json:"workspace" jsonschema:"Exact absolute worker workspace/worktree directory"`
	Program     string `json:"program" jsonschema:"Workspace-relative prebuilt Go executable, Python .py or JavaScript .js/.mjs/.cjs source (depending on language/mode)"`
	FileVersion string `json:"file_version" jsonschema:"Exact program/source version from workspace_stat; stale versions are rejected"`
	Language    string `json:"language,omitempty" jsonschema:"Optional debugger language: omit or go for Go/Delve, python for debugpy, javascript for Node/js-debug"`
	Mode        string `json:"mode,omitempty" jsonschema:"Go exec (default); Python script/module/test; JavaScript node; unsupported modes rejected"`
	StopOnEntry bool   `json:"stop_on_entry,omitempty" jsonschema:"Pause the selected Go/Python/Node program at its entrypoint"`
}
type DebugOwnerInput struct {
	Workspace string `json:"workspace" jsonschema:"Exact absolute workspace used at debug_launch"`
	SessionID string `json:"session_id" jsonschema:"Opaque authenticated debug session ID"`
}
type DebugBreakpointsInput struct {
	Workspace   string `json:"workspace"`
	SessionID   string `json:"session_id"`
	Path        string `json:"path" jsonschema:"Workspace-relative .go, .py or .js/.mjs/.cjs source matching active debug adapter"`
	FileVersion string `json:"file_version" jsonschema:"Exact current source version from workspace_stat"`
	Lines       []int  `json:"lines" jsonschema:"1-based source lines, max 32; an empty array removes breakpoints"`
}
type DebugThreadInput struct {
	Workspace string `json:"workspace"`
	SessionID string `json:"session_id"`
	ThreadID  int    `json:"thread_id" jsonschema:"Adapter-reported thread ID: Go/Python positive, Node/js-debug may return 0"`
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
	Expression string `json:"expression" jsonschema:"Bounded expression in the selected debug language (1..512 bytes). Evaluation may execute target code and cause side effects; never automatic read-only inspection"`
}

func (s *Server) registerDebugTools() {

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_adapter_status", Description: "Discover optional trusted Go/Delve, Python/debugpy or Node/js-debug readiness and verified modes without launching or installing tools; no remote/root attach.", Annotations: annotations(true, false, true, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugAdapterStatusInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_adapter_status", Language: in.Language})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_launch", Description: "Launch worker-only Go/Delve exec, Python/debugpy script/module/unittest, or Node/js-debug javascript/node DAP with exact source file_version and trusted optional adapter. Python and Node require atomic executor-owned private cgroup-v2 descendant containment and pinned full adapter resources. No remote attach, root, custom adapter or runInTerminal. Debugging explicitly executes selected project code.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugLaunchInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_launch", Workspace: in.Workspace, DebugProgram: in.Program, FileVersion: in.FileVersion, DebugStopOnEntry: in.StopOnEntry, Language: in.Language, DebugMode: in.Mode})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_breakpoints", Description: "Set/remove Go, Python or Node JavaScript source breakpoints with exact worker source file_version, before debug_configure. This may alter debuggee execution.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugBreakpointsInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "breakpoints", Workspace: in.Workspace, SessionID: in.SessionID, Path: in.Path, FileVersion: in.FileVersion, DebugLines: in.Lines})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_evaluate", Description: "Evaluate a user-approved expression in the stopped Go/Delve, Python/debugpy or Node/js-debug target. This can execute code or cause side effects; do not treat as read-only or automatically retry after uncertainty.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugEvaluateInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "evaluate", Workspace: in.Workspace, SessionID: in.SessionID, DebugFrameID: in.FrameID, DebugExpression: in.Expression})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_variables", Description: "Inspect bounded variables (max 40) in the selected debugger; does not read source paths provided by the adapter.", Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugVariablesInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "variables", Workspace: in.Workspace, SessionID: in.SessionID, DebugReference: in.VariablesReference})
	})
	for _, x := range []struct {
		name, method, description string
		readOnly                  bool
	}{
		{"debug_configure", "configure", "Finish breakpoint configuration and run/stop-on-entry via real DAP configurationDone.", false},
		{"debug_threads", "threads", "Inspect threads of the configured Go/Delve, Python/debugpy or Node/js-debug session.", true},
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
		{"debug_continue", "continue", "Continue a halted Go/Python/Node program; changes target execution state.", false},
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
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_scopes", Description: "Inspect variable scopes of a stopped Go/Python stack frame.", Annotations: annotations(true, false, true, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugFrameInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "scopes", Workspace: in.Workspace, SessionID: in.SessionID, DebugFrameID: in.FrameID})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_status", Description: "Inspect current debug session state and bounded asynchronous DAP events, including explicit dropped-events accounting. Does not imply the debuggee terminated on client disconnect.", Annotations: annotations(true, false, false, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugOwnerInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_status", Workspace: in.Workspace, SessionID: in.SessionID})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_stop", Description: "Request debugger disconnect+terminateDebuggee and stop the executor-owned worker session with verified Go or Python descendant cleanup. Uncertain results are not reported as success.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugOwnerInput) (*mcpsdk.CallToolResult, executor.Response, error) {
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
