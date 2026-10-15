package mcp

import (
	"context"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type RepositoryDiscoverInput struct {
	RemoteIdentity string `json:"remote_identity,omitempty" jsonschema:"Optional Git remote URL or canonical host/path identity used only to filter local discovery; no network access is performed"`
}

type RepositoryInspectInput struct {
	Path         string `json:"path" jsonschema:"Repository or linked-worktree path inside the configured workspace"`
	VerifyRemote bool   `json:"verify_remote,omitempty" jsonschema:"Also verify the exact local HEAD against an external branch with git ls-remote; terminal prompts remain disabled"`
	Remote       string `json:"remote,omitempty" jsonschema:"Optional configured remote name for explicit push-result reconciliation; supply together with remote_branch and verify_remote=true, otherwise the current branch upstream is used"`
	RemoteBranch string `json:"remote_branch,omitempty" jsonschema:"Optional external branch for explicit push-result reconciliation; supply together with remote and verify_remote=true"`
}

type WorktreeCreateInput struct {
	RepositoryPath   string `json:"repository_path" jsonschema:"Existing repository/worktree path inside the configured workspace"`
	WorktreePath     string `json:"worktree_path" jsonschema:"New task-worktree path inside the configured workspace; its parent must already exist"`
	Branch           string `json:"branch" jsonschema:"Local task branch to create or attach; it must not be checked out in another worktree"`
	StartRef         string `json:"start_ref" jsonschema:"Existing local or remote-tracking ref that should resolve to expected_start_sha"`
	ExpectedStartSHA string `json:"expected_start_sha" jsonschema:"Exact 40-character commit SHA expected from start_ref and the resulting task branch"`
}

type WorktreeRemoveInput struct {
	RepositoryPath string `json:"repository_path" jsonschema:"Existing repository/worktree path identifying the owning Git repository"`
	WorktreePath   string `json:"worktree_path" jsonschema:"Linked task-worktree path inside the configured workspace; the main worktree is always protected"`
	ExpectedHead   string `json:"expected_head" jsonschema:"Exact 40-character HEAD required before removal"`
	Remote         string `json:"remote,omitempty" jsonschema:"Optional remote name for durability proof; supply together with remote_branch or omit both to use the worktree branch upstream"`
	RemoteBranch   string `json:"remote_branch,omitempty" jsonschema:"Optional remote branch for durability proof; supply together with remote"`
	Disposable     bool   `json:"disposable,omitempty" jsonschema:"Set true only when this clean worktree is intentionally disposable and external remote durability is not required; the local branch is never deleted"`
}

func (s *Server) registerRepositoryTools() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "repository_discover",
		Description: "Discover Git repositories and worktrees with state under the workspace. remote_identity filters local results only; no network fetch.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RepositoryDiscoverInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action:         "repository_discover",
			RemoteIdentity: input.RemoteIdentity,
		})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "repository_inspect",
		Description: "Inspect Git HEAD, remotes, worktrees and dirty/conflict state in one workspace. verify_remote=true checks upstream or chosen remote/branch with git ls-remote; no local ref changes.",
		Annotations: annotations(true, false, true, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RepositoryInspectInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action:         "repository_inspect",
			RepositoryPath: input.Path,
			VerifyRemote:   input.VerifyRemote,
			RemoteName:     input.Remote,
			RemoteBranch:   input.RemoteBranch,
		})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "worktree_create",
		Description: "Create/reconcile isolated Git worktree from exact expected commit. Rejects stale refs, branch conflicts, dirty paths and unsupported checkout filters. No fetch/push/merge/branch deletion.",
		Annotations: annotations(false, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorktreeCreateInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action:         "worktree_create",
			RepositoryPath: input.RepositoryPath,
			WorktreePath:   input.WorktreePath,
			Branch:         input.Branch,
			StartRef:       input.StartRef,
			ExpectedSHA:    input.ExpectedStartSHA,
		})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "worktree_remove",
		Description: "Remove clean linked worktree at exact HEAD, never main or local branch. Needs remote durability unless disposable=true; dirty/uncertain state fails closed.",
		Annotations: annotations(false, true, true, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input WorktreeRemoveInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action:         "worktree_remove",
			RepositoryPath: input.RepositoryPath,
			WorktreePath:   input.WorktreePath,
			ExpectedSHA:    input.ExpectedHead,
			RemoteName:     input.Remote,
			RemoteBranch:   input.RemoteBranch,
			Disposable:     input.Disposable,
		})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return s.responseResult(resp)
	})
}
