package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func validateBranchName(s *Server, ctx context.Context, dir, branch string) error {
	if strings.TrimSpace(branch) == "" || strings.ContainsAny(branch, "\n\r\x00") {
		return errors.New("branch is empty or invalid")
	}
	_, code, err := s.gitResult(ctx, dir, "check-ref-format", "--branch", branch)
	if err != nil || code != 0 {
		return errors.New("branch name is invalid")
	}
	return nil
}

func findWorktreeByPath(worktrees []RepositoryWorktree, path string) (RepositoryWorktree, bool) {
	path = filepath.Clean(path)
	for _, worktree := range worktrees {
		if filepath.Clean(worktree.Path) == path {
			return worktree, true
		}
	}
	return RepositoryWorktree{}, false
}

func branchInWorktree(worktrees []RepositoryWorktree, branch string) (RepositoryWorktree, bool) {
	for _, worktree := range worktrees {
		if worktree.Branch == branch {
			return worktree, true
		}
	}
	return RepositoryWorktree{}, false
}

func checkoutFilterPreflightError(stage string, code int, err error) error {
	if err != nil {
		return fmt.Errorf("checkout-filter preflight: %w", err)
	}
	return fmt.Errorf("checkout-filter preflight git %s exited %d", stage, code)
}

func (s *Server) commitUsesCheckoutFilter(ctx context.Context, repositoryPath, commitSHA string) (bool, error) {
	tmpDir, err := os.MkdirTemp("", "ai-server-agent-git-index-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmpDir)
	if err := os.Chown(tmpDir, int(s.workerUID), int(s.workerGID)); err != nil {
		return false, err
	}
	if err := os.Chmod(tmpDir, 0700); err != nil {
		return false, err
	}
	indexPath := filepath.Join(tmpDir, "index")
	env := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, code, err := s.gitRawResultEnv(ctx, repositoryPath, env, nil, "read-tree", commitSHA); err != nil || code != 0 {
		return false, checkoutFilterPreflightError("read-tree", code, err)
	}
	files, code, err := s.gitRawResultEnv(ctx, repositoryPath, env, nil, "ls-files", "-z")
	if err != nil || code != 0 {
		return false, checkoutFilterPreflightError("ls-files", code, err)
	}
	if len(files) == 0 {
		return false, nil
	}
	attrs, code, err := s.gitRawResultEnv(ctx, repositoryPath, env, files, "check-attr", "--cached", "-z", "--stdin", "filter")
	if err != nil || code != 0 {
		return false, checkoutFilterPreflightError("check-attr", code, err)
	}
	parts := bytes.Split(attrs, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	if len(parts)%3 != 0 {
		return false, errors.New("git check-attr returned malformed metadata")
	}
	for i := 0; i < len(parts); i += 3 {
		value := string(parts[i+2])
		if value != "unspecified" && value != "unset" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) worktreeCreateContext(parent context.Context, req Request) (resp Response) {
	release, ok := s.runs.acquire(false)
	if !ok {
		return runCapacityResponse(false)
	}
	defer release()
	ctx, cancel := context.WithTimeout(parent, repositoryOperationTimeout)
	defer cancel()

	if !fullCommitSHA.MatchString(req.ExpectedSHA) {
		return repositoryError("invalid_expected_sha", "validation", "expected_start_sha must be a full 40-character commit SHA", false)
	}
	repo, code, err := s.inspectRepository(ctx, req.RepositoryPath, false)
	if err != nil {
		return repositoryGitFailure(code, "state", err, code == "repository_changed")
	}
	if err := validateBranchName(s, ctx, repo.Root, req.Branch); err != nil {
		return repositoryError("invalid_branch", "validation", err.Error(), false)
	}
	target, err := s.workspacePath(req.WorktreePath, false)
	if err != nil {
		return repositoryError("invalid_worktree_path", "validation", err.Error(), false)
	}
	if filepath.Clean(target) == filepath.Clean(repo.MainWorktree) {
		return repositoryError("invalid_worktree_path", "validation", "worktree target cannot replace the main worktree", false)
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		existing, inspectCode, inspectErr := s.inspectRepository(ctx, target, false)
		if inspectErr == nil && existing.CommonGitDir == repo.CommonGitDir && existing.Branch == req.Branch && existing.Head == req.ExpectedSHA && !existing.Dirty && len(existing.Operations) == 0 {
			return Response{OK: true, Status: "already_exists", IdempotentReplay: true, Repository: &existing}
		}
		if inspectErr != nil && inspectCode == "repository_changed" {
			return repositoryError("worktree_state_ambiguous", "state", "existing worktree changed during inspection", true)
		}
		return repositoryError("path_exists", "state", "worktree target already exists with different or unknown state", false)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return repositoryError("worktree_state_ambiguous", "state", "worktree target state could not be determined", true)
	}

	startRef := strings.TrimSpace(req.StartRef)
	if startRef == "" || len(startRef) > 1024 || strings.HasPrefix(startRef, "-") || strings.ContainsAny(startRef, "\n\r\x00") {
		return repositoryError("invalid_start_ref", "validation", "start_ref is empty or invalid", false)
	}
	resolvedStartRef := startRef
	if !fullCommitSHA.MatchString(startRef) {
		fullRef, refCode, refErr := s.gitResult(ctx, repo.Root, "rev-parse", "--symbolic-full-name", startRef)
		if refErr != nil || refCode != 0 || !(strings.HasPrefix(fullRef, "refs/heads/") || strings.HasPrefix(fullRef, "refs/remotes/") || strings.HasPrefix(fullRef, "refs/tags/")) {
			return repositoryError("invalid_start_ref", "validation", "start_ref must resolve to a local branch, remote-tracking branch, tag, or exact commit", false)
		}
		resolvedStartRef = fullRef
	}
	startHead, exitCode, gitErr := s.gitResult(ctx, repo.Root, "rev-parse", "--verify", resolvedStartRef+"^{commit}")
	if gitErr != nil || exitCode != 0 || !fullCommitSHA.MatchString(startHead) {
		return repositoryError("start_ref_not_found", "state", "start_ref does not resolve to a commit", false)
	}
	if startHead != req.ExpectedSHA {
		return repositoryError("start_sha_mismatch", "state", "start_ref no longer resolves to expected_start_sha", true)
	}
	usesFilter, filterErr := s.commitUsesCheckoutFilter(ctx, repo.Root, req.ExpectedSHA)
	if filterErr != nil {
		return repositoryGitFailure("checkout_filter_scan_failed", "state", filterErr, true)
	}
	if usesFilter {
		return repositoryError("checkout_filter_unsupported", "validation", "structured worktree creation refuses repository checkout filters; use ordinary Git CLI when repository-owned filter execution is intentionally required", false)
	}

	// Unlike rev-parse --verify (which exits 128 for both an absent ref and
	// fatal failures), a successful for-each-ref query distinguishes absence
	// from a command failure before any worktree/branch mutation.
	refName := "refs/heads/" + req.Branch
	refs, branchCode, branchErr := s.gitResult(ctx, repo.Root, "for-each-ref",
		"--format=%(refname) %(objectname) %(objecttype)", refName)
	if branchErr != nil {
		return repositoryGitFailure("git_failed", "process", branchErr, true)
	}
	if branchCode != 0 {
		return repositoryError("git_failed", "process", fmt.Sprintf("git for-each-ref exited %d", branchCode), true)
	}
	branchHead := ""
	for _, line := range strings.Split(refs, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return repositoryError("git_failed", "process", "git for-each-ref returned malformed branch metadata", true)
		}
		if fields[0] == refName {
			if fields[2] != "commit" || !fullCommitSHA.MatchString(fields[1]) {
				return repositoryError("git_failed", "process", "git for-each-ref returned invalid branch commit metadata", true)
			}
			branchHead = fields[1]
		}
	}
	args := []string{"worktree", "add"}
	if branchHead != "" {
		if branchHead != req.ExpectedSHA {
			return repositoryError("branch_head_mismatch", "state", "existing local branch does not match expected_start_sha", true)
		}
		if holder, used := branchInWorktree(repo.Worktrees, req.Branch); used {
			return repositoryError("branch_in_use", "state", "branch is already checked out in "+holder.Path, false)
		}
		args = append(args, target, req.Branch)
	} else {
		args = append(args, "-b", req.Branch, target, req.ExpectedSHA)
	}
	auditStarted := time.Now()
	if blocked := s.beginActionAudit(req, "worktree_create", "worker", target+"\x00"+req.Branch, "repository"); blocked != nil {
		return *blocked
	}
	defer func() {
		resp = s.finishActionAudit(req, "worktree_create", "worker", target+"\x00"+req.Branch, "repository", auditStarted, resp)
	}()
	_, exitCode, gitErr = s.gitResult(ctx, repo.Root, args...)
	if gitErr != nil || exitCode != 0 {
		if _, statErr := os.Lstat(target); statErr == nil {
			return repositoryError("unknown_completion", "state", "worktree creation failed after local state appeared; inspect before retrying", true)
		}
		if gitErr != nil {
			return repositoryGitFailure("worktree_create_failed", "process", fmt.Errorf("git worktree add: %w", gitErr), false)
		}
		return repositoryError("worktree_create_failed", "process", fmt.Sprintf("git worktree add exited %d", exitCode), false)
	}
	created, inspectCode, inspectErr := s.inspectRepository(ctx, target, false)
	if inspectErr != nil || created.CommonGitDir != repo.CommonGitDir || created.Branch != req.Branch || created.Head != req.ExpectedSHA {
		message := "worktree creation completed but exact resulting identity could not be proven"
		if inspectErr != nil && inspectCode != "" {
			message += " (" + inspectCode + ")"
		}
		return repositoryError("unknown_completion", "state", message, true)
	}
	return Response{OK: true, Status: "created", Repository: &created}
}

func (s *Server) worktreeRemoveContext(parent context.Context, req Request) (resp Response) {
	release, ok := s.runs.acquire(false)
	if !ok {
		return runCapacityResponse(false)
	}
	defer release()
	ctx, cancel := context.WithTimeout(parent, repositoryOperationTimeout)
	defer cancel()

	if !fullCommitSHA.MatchString(req.ExpectedSHA) {
		return repositoryError("invalid_expected_sha", "validation", "expected_head must be a full 40-character commit SHA", false)
	}
	repo, code, err := s.inspectRepository(ctx, req.RepositoryPath, false)
	if err != nil {
		return repositoryGitFailure(code, "state", err, code == "repository_changed")
	}
	target, err := s.workspacePath(req.WorktreePath, false)
	if err != nil {
		return repositoryError("invalid_worktree_path", "validation", err.Error(), false)
	}
	if filepath.Clean(target) == filepath.Clean(repo.MainWorktree) {
		return repositoryError("main_worktree_protected", "validation", "the main worktree cannot be removed by this helper", false)
	}
	if _, statErr := os.Lstat(target); errors.Is(statErr, os.ErrNotExist) {
		if _, listed := findWorktreeByPath(repo.Worktrees, target); listed {
			return repositoryError("worktree_state_ambiguous", "state", "worktree path is missing but Git still records it", false)
		}
		return Response{OK: true, Status: "already_absent", IdempotentReplay: true}
	} else if statErr != nil {
		return repositoryError("worktree_state_ambiguous", "state", "worktree path state could not be determined", true)
	}
	state, inspectCode, inspectErr := s.inspectRepository(ctx, target, false)
	if inspectErr != nil {
		return repositoryError(inspectCode, "state", inspectErr.Error(), inspectCode == "repository_changed")
	}
	if state.CommonGitDir != repo.CommonGitDir {
		return repositoryError("repository_mismatch", "validation", "worktree belongs to a different repository", false)
	}
	if state.Head != req.ExpectedSHA {
		return repositoryError("head_mismatch", "state", "worktree HEAD does not match expected_head", true)
	}
	if state.Dirty || state.StatusTruncated || state.Conflicts > 0 {
		return repositoryError("dirty_worktree", "state", "worktree has dirty, conflicted, untracked, or incompletely inspected state", false)
	}
	if len(state.Operations) > 0 {
		return repositoryError("git_operation_in_progress", "state", "worktree has an in-progress Git operation", false)
	}

	if !req.Disposable {
		remote := strings.TrimSpace(req.RemoteName)
		remoteBranch := strings.TrimSpace(req.RemoteBranch)
		if (remote == "") != (remoteBranch == "") {
			return repositoryError("invalid_remote_proof", "validation", "remote and remote_branch must be supplied together", false)
		}
		if remote == "" {
			var inferred bool
			remote, remoteBranch, inferred = s.branchUpstreamConfig(ctx, target, state.Branch)
			if !inferred {
				return repositoryError("durability_not_proven", "state", "no external upstream is configured; remote durability was not proven", false)
			}
		}
		remoteBranch, err = normalizeRemoteBranch(remoteBranch)
		if err != nil {
			return repositoryError("invalid_remote_proof", "validation", err.Error(), false)
		}
		if err := validateBranchName(s, ctx, target, remoteBranch); err != nil {
			return repositoryError("invalid_remote_proof", "validation", "remote_branch is not a valid branch name", false)
		}
		remoteHead, exists, remoteCode := s.resolveRemoteHead(ctx, target, remote, remoteBranch)
		if remoteCode != "" {
			return repositoryError(remoteCode, "state", "remote durability could not be verified", true)
		}
		if !exists {
			return repositoryError("durability_not_proven", "state", "remote branch does not exist", false)
		}
		if remoteHead != req.ExpectedSHA {
			return repositoryError("remote_head_mismatch", "state", "remote branch does not contain the exact expected worktree HEAD", true)
		}
	}

	if s.workspaceHooks != nil && s.workspaceHooks.beforeWorktreeRemove != nil {
		s.workspaceHooks.beforeWorktreeRemove()
	}
	finalState, finalCode, finalErr := s.inspectRepository(ctx, target, false)
	if finalErr != nil {
		return repositoryError(finalCode, "state", "worktree changed before removal", true)
	}
	if finalState.Head != req.ExpectedSHA || finalState.Dirty || finalState.StatusTruncated || finalState.Conflicts > 0 || len(finalState.Operations) > 0 {
		return repositoryError("repository_changed", "state", "worktree identity or state changed before removal", true)
	}

	auditStarted := time.Now()
	if blocked := s.beginActionAudit(req, "worktree_remove", "worker", target, "repository"); blocked != nil {
		return *blocked
	}
	defer func() {
		resp = s.finishActionAudit(req, "worktree_remove", "worker", target, "repository", auditStarted, resp)
	}()
	_, exitCode, gitErr := s.gitResult(ctx, repo.Root, "worktree", "remove", target)
	if gitErr != nil || exitCode != 0 {
		if gitErr != nil {
			return repositoryGitFailure("worktree_remove_failed", "process", fmt.Errorf("git worktree remove: %w", gitErr), false)
		}
		return repositoryError("worktree_remove_failed", "process", fmt.Sprintf("git worktree remove exited %d", exitCode), false)
	}
	repoAfter, inspectCode, inspectErr := s.inspectRepository(ctx, repo.Root, false)
	if inspectErr != nil {
		return repositoryError(inspectCode, "state", "worktree removal completed but repository verification failed", true)
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		return repositoryError("unknown_completion", "state", "worktree removal returned success but target path still exists", true)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return repositoryError("unknown_completion", "state", "worktree target state is ambiguous after removal", true)
	}
	if _, listed := findWorktreeByPath(repoAfter.Worktrees, target); listed {
		return repositoryError("unknown_completion", "state", "worktree removal returned success but Git still records the worktree", true)
	}
	return Response{OK: true, Status: "removed"}
}
