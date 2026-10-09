package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type WorkspaceSearchInput struct {
	Mode      string   `json:"mode" jsonschema:"Search layer: text uses bounded ripgrep regex or literal matching; structural uses ast-grep; LSP remains semantic owner."`
	Workspace string   `json:"workspace" jsonschema:"Explicit workspace/repository directory, absolute or relative to the configured Agent workspace root"`
	Language  string   `json:"language,omitempty" jsonschema:"ast-grep language identifier such as go, js, ts, tsx, py, rust, java, c or cpp"`
	Pattern   string   `json:"pattern" jsonschema:"Text regex/literal or structural ast-grep pattern; maximum 65536 bytes"`
	Literal   bool     `json:"literal,omitempty" jsonschema:"For mode=text, treat pattern as an exact literal rather than a regex"`
	Paths     []string `json:"paths,omitempty" jsonschema:"Optional relative files/directories within workspace; default searches the workspace root; maximum 32"`
	Globs     []string `json:"globs,omitempty" jsonschema:"Optional text/structural include/exclude globs; prefix exclusions with !; maximum 32"`
	Limit     int      `json:"limit,omitempty" jsonschema:"Maximum matches returned; default 100, maximum 1000"`
	TimeoutMS int64    `json:"timeout_ms,omitempty" jsonschema:"Search timeout in milliseconds; default 30000, maximum 120000"`
}

func (s *Server) registerWorkspaceSearchTool() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "workspace_search",
		Description: "Run bounded read-only source search in one explicit workspace. Use mode=text for worker-authority ripgrep regex/literal matches (requires Landlock and trusted rg), or mode=structural for ast-grep syntax-tree shapes. Results include zero-based file/range identity and bounded matched text/captures. This tool never applies rewrites, never follows source symlinks, never loads repository ast-grep configuration, and never creates a code index. For advanced text searches use rg/git grep through run_command; use LSP for symbol/type/reference semantics.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceSearchInput) (*mcpsdk.CallToolResult, executor.WorkspaceSearchResult, error) {
		action := "workspace_search"
		if input.Mode == "text" {
			action = "workspace_text_search"
		}
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action:      action,
			SearchMode:  input.Mode,
			Workspace:   input.Workspace,
			Language:    input.Language,
			Pattern:     input.Pattern,
			SearchPaths: input.Paths,
			Globs:       input.Globs,
			SearchLimit: input.Limit,
			Literal:     input.Literal,
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
