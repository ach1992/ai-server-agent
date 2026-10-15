package mcp

import (
	"context"
	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Stat never returns file bytes; callers may pass its version to workspace_read.
type WorkspaceStatInput struct {
	Workspace   string `json:"workspace" jsonschema:"Explicit workspace/worktree directory inside the configured Agent workspace root"`
	Path        string `json:"path" jsonschema:"Relative readable regular-file path; symlinks and Git admin paths are not followed"`
	FileVersion string `json:"file_version,omitempty" jsonschema:"Optional consistency precondition from a previous workspace_stat or workspace_read"`
}

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

// A single client-neutral transfer interface handles staged text and binary
// ingress, status/replay and atomic preconditioned publication.
type WorkspaceTransferInput struct {
	Workspace    string `json:"workspace" jsonschema:"Exact aiworker workspace/worktree selected for this transfer"`
	Path         string `json:"path" jsonschema:"Relative final destination path; must remain unchanged for all transfer operations"`
	Operation    string `json:"operation" jsonschema:"One of begin, chunk, status, commit or abort"`
	TransferKey  string `json:"transfer_key,omitempty" jsonschema:"Begin: required caller-generated stable idempotency key of 16..128 ASCII letters, digits, underscores or hyphens; retry identical begin with same key after a lost response"`
	TransferID   string `json:"transfer_id,omitempty" jsonschema:"Opaque transfer handle from begin; required for all other operations"`
	ExpectedSize int64  `json:"expected_size,omitempty" jsonschema:"Begin: exact expected total byte count, positive and within configured staged storage quota"`
	SHA256       string `json:"sha256,omitempty" jsonschema:"Begin: expected complete-file lowercase hexadecimal SHA256 digest"`
	FileVersion  string `json:"file_version,omitempty" jsonschema:"Begin replacement: exact prior target file_version, mutually exclusive with must_not_exist"`
	MustNotExist bool   `json:"must_not_exist,omitempty" jsonschema:"Begin creation: target must not exist, mutually exclusive with file_version"`
	Offset       int64  `json:"offset,omitempty" jsonschema:"Chunk: expected raw byte offset, sequential; prior complete chunks may be replayed"`
	ChunkBase64  string `json:"chunk_base64,omitempty" jsonschema:"Chunk: base64 of raw bytes, maximum 262144 decoded bytes per call"`
	ChunkSHA256  string `json:"chunk_sha256,omitempty" jsonschema:"Chunk: lowercase hex SHA256 of decoded raw bytes"`
}

func (s *Server) registerWorkspaceFileTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "workspace_transfer",
		Description: "Stage binary/UTF-8 file transfers as aiworker. begin: stable transfer_key, size, SHA256, must_not_exist/file_version; chunk: <=256 KiB base64 at exact offset with SHA256. commit: <=4 MiB preparation per call; repeat the SAME handle while status=preparing. status reports progress, expiry and conflicts; abort drops uncommitted data. Staging counts against quota. If completion is uncertain, check status before retrying. No root access.",
		Annotations: annotations(false, true, false, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceTransferInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action: "workspace_transfer", Workspace: input.Workspace, Path: input.Path,
			TransferKey: input.TransferKey, TransferOp: input.Operation, TransferID: input.TransferID, ExpectedSize: input.ExpectedSize,
			SHA256: input.SHA256, FileVersion: input.FileVersion, MustNotExist: input.MustNotExist,
			Offset: input.Offset, ChunkBase64: input.ChunkBase64, ChunkSHA256: input.ChunkSHA256,
		})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "workspace_stat", Description: "Inspect aiworker file size and file_version without reading content. Metadata only; use workspace_read for selected bytes. No symlinks.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceStatInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "workspace_stat", Workspace: input.Workspace, Path: input.Path, FileVersion: input.FileVersion})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "workspace_apply_edits", Description: "Apply up to 12 aiworker file edits with expected file_version and exact context. Each file is atomic; batch is NOT an all-or-nothing transaction. Inspect applied/failed/unattempted paths and unknown outcomes. No Git commit or automatic rollback.",
		Annotations: annotations(false, true, false, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceApplyEditsInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "workspace_apply_edits", Workspace: input.Workspace, WorkspaceEdits: input.Edits})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "workspace_read", Description: "Read bounded bytes from an aiworker workspace, not root; returns file_version. For unknown/large files use workspace_stat and explicit limit; default 1 MiB may overwhelm context. No source symlinks or Git administrative reads.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceReadInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "workspace_read", Workspace: input.Workspace, Path: input.Path, Offset: input.Offset, Limit: input.Limit, FileVersion: input.FileVersion})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name: "workspace_write", Description: "Create or replace one regular aiworker source file with must_not_exist or exact file_version. No symlinks, implicit Git commit or silent overwrite.",
		Annotations: annotations(false, true, false, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorkspaceWriteInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{Action: "workspace_write", Workspace: input.Workspace, Path: input.Path, Content: input.Content, FileVersion: input.FileVersion, MustNotExist: input.MustNotExist})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
}
