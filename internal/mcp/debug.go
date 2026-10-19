package mcp

import (
	"context"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Debug tools reuse the same focused Go/Delve, Rust/LLDB, PHP/Xdebug, Python/debugpy, Bash/DEBUG-trap and Node/js-debug DAP control loop;
// there is no generic raw DAP command/socket API or privileged attach.
type DebugAdapterStatusInput struct {
	Language string `json:"language,omitempty" jsonschema:"Optional language: go (default), rust, php, python, shell, javascript or typescript; passive trusted adapter readiness only"`
}
type DebugLaunchInput struct {
	Workspace    string `json:"workspace" jsonschema:"Exact absolute worker workspace/worktree directory"`
	Program      string `json:"program" jsonschema:"Workspace-relative Go/Rust executable, Bash .sh/.bash, PHP .php, Python .py, Node .js/.mjs/.cjs or JavaScript Browser .html; TypeScript requires precompiled .js/.js.map"`
	FileVersion  string `json:"file_version" jsonschema:"Exact program/source version from workspace_stat; stale versions are rejected"`
	Language     string `json:"language,omitempty" jsonschema:"Optional debugger language: go (default), rust, php, python, shell (Bash only), javascript or precompiled typescript"`
	Mode         string `json:"mode,omitempty" jsonschema:"Go/Rust exec; shell bash only; PHP cli or verified local service-owned http; Python script/module/test; JavaScript/TypeScript node or local Chromium browser; no remote/extension attach"`
	ServiceJobID string `json:"service_job_id,omitempty" jsonschema:"PHP http only: exact existing authorized worker start_job(kind=service) job_id; DAP never starts or stops this service"`
	HTTPPort     int    `json:"http_port,omitempty" jsonschema:"PHP http only: original service PHP -S IPv4 loopback listener port 1024..65535"`
	HTTPTrigger  string `json:"http_trigger,omitempty" jsonschema:"PHP http only: 32 lowercase hex token already preconfigured into service Xdebug trigger_value and root-owned sticky /tmp Unix socket pathname; never an authorization credential"`
	StopOnEntry  bool   `json:"stop_on_entry,omitempty" jsonschema:"Pause selected Go/Rust/PHP/Python/Node/Bash program at its entrypoint"`
}
type DebugOwnerInput struct {
	Workspace string `json:"workspace" jsonschema:"Exact absolute workspace used at debug_launch"`
	SessionID string `json:"session_id" jsonschema:"Opaque authenticated debug session ID"`
}
type DebugBreakpointsInput struct {
	Workspace   string `json:"workspace"`
	SessionID   string `json:"session_id"`
	Path        string `json:"path" jsonschema:"Workspace-relative .go/.rs/.php/.py/.sh/.bash/.js/.mjs/.cjs or pinned TypeScript .ts; exact file_version required"`
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

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_adapter_status", Description: "Discover optional trusted Go/Delve, Rust/LLDB-DAP, PHP/Xdebug-DBGp CLI, Python/debugpy, bundled Bash/DEBUG-trap or Node/js-debug Node and locally pinned Browser pipe readiness without launching or installing tools; no remote/root attach.", Annotations: annotations(true, false, true, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugAdapterStatusInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_adapter_status", Language: in.Language})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_launch", Description: "Launch worker-only DAP for Go/Rust, bounded Bash script, PHP CLI or separately service-owned local PHP HTTP, Python, Node/TypeScript or local Chromium. PHP HTTP requires verified existing start_job Service, predeclared Xdebug Unix socket/trigger and explicit one-shot debug_http_request. Only debugger resources are owned by DAP; no remote/production/PHP-FPM/Apache or extension-host attach; Bash has no remote attach.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugLaunchInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_launch", Workspace: in.Workspace, DebugProgram: in.Program, FileVersion: in.FileVersion, DebugStopOnEntry: in.StopOnEntry, Language: in.Language, DebugMode: in.Mode, JobID: in.ServiceJobID, DebugHTTPPort: in.HTTPPort, DebugHTTPTrigger: in.HTTPTrigger})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_breakpoints", Description: "Set/remove Go, Rust, PHP, Python, Bash or Node JavaScript source breakpoints with exact worker source file_version, before debug_configure. This may alter debuggee execution.", Annotations: annotations(false, true, false, true),
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugBreakpointsInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "breakpoints", Workspace: in.Workspace, SessionID: in.SessionID, Path: in.Path, FileVersion: in.FileVersion, DebugLines: in.Lines})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "debug_evaluate", Description: "Evaluate expression in stopped DAP target; may execute code or have side effects and requires user approval. Never auto-retry an uncertain outcome.", Annotations: annotations(false, true, false, true),
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
		{"debug_threads", "threads", "Inspect threads of a configured Go/Delve, Rust/LLDB-DAP, PHP/Xdebug-DBGp CLI, Python/debugpy, Bash or Node/js-debug session.", true},
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
		{"debug_continue", "continue", "Continue a halted Go/Rust/PHP/Python/Node/Bash program; changes target execution state.", false},
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
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_scopes", Description: "Inspect variable scopes of a stopped Go/Rust/PHP/Python/Bash stack frame.", Annotations: annotations(true, false, true, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugFrameInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "scopes", Workspace: in.Workspace, SessionID: in.SessionID, DebugFrameID: in.FrameID})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_status", Description: "Inspect current debug session state and bounded asynchronous DAP events, including explicit dropped-events accounting. Does not imply the debuggee terminated on client disconnect.", Annotations: annotations(true, false, false, false)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugOwnerInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_status", Workspace: in.Workspace, SessionID: in.SessionID})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_http_request", Description: "Explicitly dispatch ONE authorized, version-pinned PHP HTTP DAP request to an already verified local worker Service Job. Side-effecting GET; fixed 127.0.0.1 target, no redirects, no retries, no service start/stop. Inspect debug_status for result.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugOwnerInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		return s.callDebug(ctx, executor.Request{Action: "debug_action", DebugMethod: "http_request", Workspace: in.Workspace, SessionID: in.SessionID})
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{Name: "debug_stop", Description: "Stop the executor-owned DAP adapter and verify only debugger-owned cleanup. For CLI/Node/etc terminate the contained target; for separately service-owned PHP HTTP, detach without terminating its Service Job, verify its survival and require independent job_stop to stop it. Unknown cleanup is not reported as success.", Annotations: annotations(false, true, false, true)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DebugOwnerInput) (*mcpsdk.CallToolResult, executor.Response, error) {
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
