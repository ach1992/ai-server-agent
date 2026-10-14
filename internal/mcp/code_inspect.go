package mcp

import (
	"context"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type CodeInspectInput struct {
	Workspace   string `json:"workspace" jsonschema:"Exact absolute resolved aiworker workspace/worktree directory"`
	Path        string `json:"path" jsonschema:"Workspace-relative regular .go .ts .tsx .mts .cts .js .jsx .mjs or .cjs .py .pyi .sh .bash .htm .html or .css source path; no symlinks or .git files"`
	Language    string `json:"language,omitempty" jsonschema:"Optional language go, javascript, typescript, python, shell, html or css; checked against path extension (never selects project commands)"`
	FileVersion string `json:"file_version" jsonschema:"Required exact file_version returned by worker workspace_stat/read. Semantic results fail when source changes."`
	Line        int    `json:"line,omitempty" jsonschema:"Zero-based source line for definition/references"`
	Character   int    `json:"character,omitempty" jsonschema:"Zero-based UTF-16 character index for definition/references"`
}

func (s *Server) registerCodeTools() {
	tools := []struct{ suffix, description string }{
		{"definition", "Resolve a source symbol definition using the trusted Go, JS/TS, Python, Shell or HTML/CSS LSP profile and exact aiworker file_version"},
		{"references", "Retrieve bounded source references through trusted Go, JS/TS, Python, Shell or HTML/CSS LSP; output never mutates files"},
		{"symbols", "Retrieve bounded Go, JavaScript/TypeScript, Python, Shell or HTML/CSS document symbols via real language semantics"},
		{"diagnostics", "Retrieve bounded Go, JavaScript/TypeScript, Python, Shell or HTML/CSS LSP diagnostics, negotiated as push or pull"},
	}
	for _, tool := range tools {
		method := tool.suffix
		mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
			Name: "code_" + method, Description: tool.description + ". Supported now: Go with optional trusted gopls; JavaScript/TypeScript with optional pinned native TypeScript 7.0.2; Python with optional pinned native ty 0.0.87; Bash/POSIX Shell with optional pinned Bash Language Server 5.8.1 + ShellCheck 0.11.0; HTML/CSS with optional pinned VS Code language services 4.10.0. No implicit installs, scripts or project commands. Requires exact file_version; rejects changed sources and unsupported adapter capabilities.",
			Annotations: annotations(true, false, true, false),
		}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input CodeInspectInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
				Action: "code_inspect", CodeMethod: method, Workspace: input.Workspace, Path: input.Path,
				FileVersion: input.FileVersion, Language: input.Language, Line: input.Line, Character: input.Character,
			})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})
	}
}
