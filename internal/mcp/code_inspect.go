package mcp

import (
	"context"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type CodeGoInput struct {
	Workspace   string `json:"workspace" jsonschema:"Exact absolute resolved aiworker workspace/worktree directory"`
	Path        string `json:"path" jsonschema:"Workspace-relative regular .go source path (never a symlink or .git file)"`
	FileVersion string `json:"file_version" jsonschema:"Required exact file_version returned by worker workspace_stat/read. Semantic results fail when source changes."`
	Line        int    `json:"line,omitempty" jsonschema:"Zero-based source line for definition/references"`
	Character   int    `json:"character,omitempty" jsonschema:"Zero-based UTF-16 character index for definition/references"`
}

func (s *Server) registerGoCodeTools() {
	tools := []struct{ suffix, description string }{
		{"definition", "Resolve a Go symbol definition using actual gopls LSP and a version-pinned aiworker source read"},
		{"references", "Retrieve bounded Go symbol references through gopls; output is an LSP JSON result, never a file mutation"},
		{"symbols", "Retrieve bounded Go document symbols via gopls semantic analysis"},
		{"diagnostics", "Request bounded Go document diagnostics via gopls; unsupported pull-diagnostics must fail explicitly"},
	}
	for _, tool := range tools {
		method := tool.suffix
		mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
			Name: "code_" + method, Description: tool.description + ". Requires optional root-owned /usr/bin/gopls or /usr/local/bin/gopls and a configured Go toolchain. Does not auto-install tooling. Results include exact file_version and a bounded JSON response; stale version is rejected.",
			Annotations: annotations(true, false, true, false),
		}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input CodeGoInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
				Action: "code_inspect", CodeMethod: method, Workspace: input.Workspace, Path: input.Path,
				FileVersion: input.FileVersion, Line: input.Line, Character: input.Character,
			})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return responseResult(resp)
		})
	}
}
