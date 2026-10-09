package mcp

import (
	"context"
	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type WorkspaceReadInput struct {
	Workspace   string `json:"workspace" jsonschema:"Explicit workspace/worktree directory inside the configured Agent workspace root"`
	Path        string `json:"path" jsonschema:"Relative regular-file path inside workspace; symlinks and Git admin paths are not followed"`
	Offset      int64  `json:"offset,omitempty" jsonschema:"Optional raw byte offset; must be nonnegative"`
	Limit       int    `json:"limit,omitempty" jsonschema:"Maximum raw bytes, 0 defaults to 1048576 and no more than 1048576"`
	FileVersion string `json:"file_version,omitempty" jsonschema:"Optional consistency precondition from a previous read"`
}

type WorkspaceApplyEditsInput struct {
	Workspace string                       `json:"workspace" jsonschema:"Explicit workspace/worktree directory inside configured Agent workspace root"`
	Edits     []executor.WorkspaceFileEdit `json:"edits" jsonschema:"Ordered bounded versioned file edits; all preflight before first mutation, per-file commits may partially succeed"`
}

type WorkspaceWriteInput struct {
	Workspace    string `json:"workspace" jsonschema:"Explicit workspace/worktree directory inside configured Agent workspace root"`
	Path         string `json:"path" jsonschema:"Relative ordinary file path inside workspace; parent must exist; symlinks and Git admin paths are not followed"`
	Content      string `json:"content" jsonschema:"Complete bounded UTF-8 replacement content, maximum 1048576 bytes"`
	FileVersion  string `json:"file_version,omitempty" jsonschema:"Required for replacement: the exact version returned by workspace_read"`
	MustNotExist bool   `json:"must_not_exist,omitempty" jsonschema:"Required for create: true only when the file must not already exist; mutually exclusive with file_version"`
}

func (s *Server) registerWorkspaceFileTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "workspace_apply_edits", Description: "Apply up to 12 bounded aiworker source-file edits in one explicit workspace after preflighting EVERY path, expected version and exact replacement context. Each file commits atomically but the batch is NOT an all-or-nothing transaction: structured result reports exact applied, failed and unattempted paths, including possible unknown completion. No Git staging/commit, code execution or implicit rollback.",
		Annotations: annotations(false, true, false, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceApplyEditsInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "workspace_apply_edits", Workspace: input.Workspace, WorkspaceEdits: input.Edits})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "workspace_read", Description: "Read a bounded raw-byte range from a regular source file inside one explicit workspace as aiworker, not root. No symlinks, traversal or implicit Git administrative reads; returns a file_version for optimistic edits. Does not execute project code.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceReadInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "workspace_read", Workspace: input.Workspace, Path: input.Path, Offset: input.Offset, Limit: input.Limit, FileVersion: input.FileVersion})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "workspace_write", Description: "Create or atomically replace one bounded regular source file using aiworker authority and descriptor-relative path confinement. Requires must_not_exist for create or exact file_version for replace; does not auto-stage/commit. Does not follow symlinks, overwrite concurrent changes silently, or run project code. Multi-file transaction semantics are not claimed.",
		Annotations: annotations(false, true, false, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceWriteInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "workspace_write", Workspace: input.Workspace, Path: input.Path, Content: input.Content, FileVersion: input.FileVersion, MustNotExist: input.MustNotExist})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})
}
