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
	VerifyRemote bool   `json:"verify_remote,omitempty" jsonschema:"Also verify the current branch against its configured external upstream with git ls-remote; terminal prompts remain disabled"`
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
		Description: "Discover bounded Git repository/worktree identities under the configured workspace without trusting directory names. Returns canonical Git identity, remotes, HEAD/branch, dirty/conflict state and linked worktrees. Optional remote_identity filters local results only; this tool does not fetch or contact remotes.",
		Annotations: annotations(true, false, true, false),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RepositoryDiscoverInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action:         "repository_discover",
			RemoteIdentity: input.RemoteIdentity,
		})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "repository_inspect",
		Description: "Inspect one Git repository/worktree inside the configured workspace and return exact remote/ref/HEAD/dirty/conflict/worktree identity. With verify_remote=true it also checks the configured external upstream with git ls-remote, without mutating local refs or prompting for credentials.",
		Annotations: annotations(true, false, true, true),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, input RepositoryInspectInput) (*mcpsdk.CallToolResult, executor.Response, error) {
		resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
			Action:         "repository_inspect",
			RepositoryPath: input.Path,
			VerifyRemote:   input.VerifyRemote,
		})
		if err != nil {
			return executorTransportErrorResult(err)
		}
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "worktree_create",
		Description: "Create or idempotently reconcile an isolated task worktree inside the configured workspace from an exact expected commit. Refuses stale start refs, branch reuse in another worktree, path conflicts, or ambiguous state. Repository hooks and external fsmonitor integration are disabled, and repositories that require checkout filters are refused for this structured path; the tool never fetches, pushes, force-updates, merges, or deletes branches.",
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
		return responseResult(resp)
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "worktree_remove",
		Description: "Remove only a clean linked task worktree after exact-HEAD verification. The main worktree and local branch are never deleted, force removal is never used, dirty/conflicted/in-progress state fails safe, and external remote durability is required unless disposable=true is explicitly supplied.",
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
		return responseResult(resp)
	})
}
