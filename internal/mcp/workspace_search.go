package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type WorkspaceSearchInput struct {
	Mode      string   `json:"mode" jsonschema:"Search layer; currently structural only. Use run_command with rg/git grep for plain text search and LSP for semantic meaning."`
	Workspace string   `json:"workspace" jsonschema:"Explicit workspace/repository directory, absolute or relative to the configured Agent workspace root"`
	Language  string   `json:"language" jsonschema:"ast-grep language identifier such as go, js, ts, tsx, py, rust, java, c or cpp"`
	Pattern   string   `json:"pattern" jsonschema:"Structural ast-grep pattern; maximum 65536 bytes"`
	Paths     []string `json:"paths,omitempty" jsonschema:"Optional relative files/directories within workspace; default searches the workspace root; maximum 32"`
	Globs     []string `json:"globs,omitempty" jsonschema:"Optional ast-grep include/exclude globs; prefix exclusions with !; maximum 32"`
	Limit     int      `json:"limit,omitempty" jsonschema:"Maximum matches returned; default 100, maximum 1000"`
	TimeoutMS int64    `json:"timeout_ms,omitempty" jsonschema:"Search timeout in milliseconds; default 30000, maximum 120000"`
}

func (s *Server) registerWorkspaceSearchTool() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "workspace_search",
		Description: "Run bounded read-only structural code search with ast-grep in one explicit workspace. Use mode=structural for syntax-tree shapes independent of formatting. Results include zero-based file/range identity, bounded matched text and metavariable captures. This tool never applies rewrites, rejects requested paths that resolve outside the workspace, does not enable ast-grep symlink following, never loads repository ast-grep project/rule configuration, and never creates a code index. Use run_command with rg/git grep for literal/regex text search; use LSP for symbol/type/reference semantics.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceSearchInput) (*mcpsdk.CallToolResult, executor.WorkspaceSearchResult, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action:      "workspace_search",
			SearchMode:  input.Mode,
			Workspace:   input.Workspace,
			Language:    input.Language,
			Pattern:     input.Pattern,
			SearchPaths: input.Paths,
			Globs:       input.Globs,
			SearchLimit: input.Limit,
			TimeoutMS:   input.TimeoutMS,
		})
		if err != nil {
			return textResult(err.Error(), true), executor.WorkspaceSearchResult{}, nil
		}
		if !resp.OK {
			payload, _ := json.MarshalIndent(resp, "", "  ")
			return textResult(string(payload), true), executor.WorkspaceSearchResult{}, nil
		}
		var result executor.WorkspaceSearchResult
		if err := json.Unmarshal([]byte(resp.Output), &result); err != nil {
			return textResult("workspace search response could not be decoded", true), executor.WorkspaceSearchResult{}, nil
		}
		fallback := fmt.Sprintf(
			"mode=%s engine=%s@%s matches=%d complete=%t truncated=%t reason=%q duration_ms=%d",
			result.Mode,
			result.Engine,
			result.EngineVersion,
			len(result.Matches),
			result.Complete,
			result.Truncated,
			result.TruncationReason,
			result.DurationMS,
		)
		return textResult(fallback, false), result, nil
	})
}
