package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type RepositoryEnvironmentInput struct {
	Path string `json:"path" jsonschema:"Repository or linked-worktree path inside the configured workspace; no implicit current project is used"`
}

func (s *Server) registerRepositoryEnvironmentTool() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "repository_environment",
		Description: "Read a bounded structured summary of repository-owned language/toolchain/environment declarations and current host compatibility for one explicit Git worktree. Detects native manifests/locks plus declared mise, devenv, Dev Container and Dagger mechanisms without installing anything, running repository tasks, inventing task definitions or choosing precedence between conflicting declarations.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RepositoryEnvironmentInput) (*mcpsdk.CallToolResult, executor.RepositoryEnvironmentSummary, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action: "repository_environment",
			Path:   input.Path,
		})
		if err != nil {
			return textResult(err.Error(), true), executor.RepositoryEnvironmentSummary{}, nil
		}
		if !resp.OK {
			payload, _ := json.MarshalIndent(resp, "", "  ")
			return textResult(string(payload), true), executor.RepositoryEnvironmentSummary{}, nil
		}
		var summary executor.RepositoryEnvironmentSummary
		if err := json.Unmarshal([]byte(resp.Output), &summary); err != nil {
			return textResult("repository environment response could not be decoded", true), executor.RepositoryEnvironmentSummary{}, nil
		}
		fallback := fmt.Sprintf(
			"host_status=%s languages=%d tools=%d mechanisms=%d entrypoints=%d selection_required=%t",
			summary.HostStatus,
			len(summary.Languages),
			len(summary.Tools),
			len(summary.Mechanisms),
			len(summary.Entrypoints),
			summary.SelectionRequired,
		)
		return textResult(fallback, false), summary, nil
	})
}
