package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func repositoryTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	return &Server{
		cfg: config.Config{
			WorkspaceDir: root,
		},
		workerUID: uint32(os.Geteuid()),
		workerGID: uint32(os.Getegid()),
		runs:      newRunLimiterWith(2, 1),
	}, root
}

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Repository Test",
		"GIT_AUTHOR_EMAIL=repository-test@example.invalid",
		"GIT_COMMITTER_NAME=Repository Test",
		"GIT_COMMITTER_EMAIL=repository-test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initFixtureRepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, path, "init")
	fixtureGit(t, path, "branch", "-M", "main")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, path, "add", "tracked.txt")
	fixtureGit(t, path, "commit", "-m", "base")
	return fixtureGit(t, path, "rev-parse", "HEAD")
}

func hasOperation(ops []string, want string) bool {
	for _, op := range ops {
		if op == want {
			return true
		}
	}
	return false
}

func TestRepositoryInspectIdentityDirtyDetachedAndSanitizedRemote(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "arbitrary-directory-name")
	head := initFixtureRepo(t, repo)
	fixtureGit(t, repo, "remote", "add", "origin", "https://user:secret@example.com/acme/project.git?token=secret")

	state, code, err := s.inspectRepository(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("inspect clean repo: code=%s err=%v", code, err)
	}
	if state.Head != head || state.Branch != "main" || state.Detached || state.Dirty {
		t.Fatalf("unexpected clean identity: %+v", state)
	}
	if len(state.Remotes) != 1 || state.Remotes[0].Identity != "example.com/acme/project" {
		t.Fatalf("unexpected remote identity: %+v", state.Remotes)
	}
	if strings.Contains(state.Remotes[0].URL, "secret") || strings.Contains(state.Remotes[0].URL, "user:") || strings.Contains(state.Remotes[0].URL, "token=") {
		t.Fatalf("remote URL leaked credentials/query: %q", state.Remotes[0].URL)
	}

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("unstaged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0644); err != nil {
		t.Fatal(err)
	}
	state, code, err = s.inspectRepository(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("inspect dirty repo: code=%s err=%v", code, err)
	}
	if !state.Dirty || state.Staged == 0 || state.Unstaged == 0 || state.Untracked == 0 {
		t.Fatalf("dirty state not fully surfaced: %+v", state)
	}

	fixtureGit(t, repo, "reset", "--hard", "HEAD")
	_ = os.Remove(filepath.Join(repo, "untracked.txt"))
	fixtureGit(t, repo, "checkout", "--detach")
	state, code, err = s.inspectRepository(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("inspect detached repo: code=%s err=%v", code, err)
	}
	if !state.Detached || state.Branch != "" || state.Head != head {
		t.Fatalf("detached state not surfaced: %+v", state)
	}
}

func TestRepositoryInspectConflictAndDivergence(t *testing.T) {
	s, root := repositoryTestServer(t)
	remote := filepath.Join(root, "remote.git")
	fixtureGit(t, root, "init", "--bare", remote)
	repo := filepath.Join(root, "repo")
	initFixtureRepo(t, repo)
	fixtureGit(t, repo, "remote", "add", "origin", remote)
	fixtureGit(t, repo, "push", "-u", "origin", "main")

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("local\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "commit", "-am", "local")

	other := filepath.Join(root, "other")
	fixtureGit(t, root, "clone", remote, other)
	fixtureGit(t, other, "checkout", "main")
	if err := os.WriteFile(filepath.Join(other, "remote.txt"), []byte("remote\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, other, "add", "remote.txt")
	fixtureGit(t, other, "commit", "-m", "remote")
	fixtureGit(t, other, "push", "origin", "main")
	fixtureGit(t, repo, "fetch", "origin")

	state, code, err := s.inspectRepository(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("inspect divergence: code=%s err=%v", code, err)
	}
	if state.Ahead != 1 || state.Behind != 1 {
		t.Fatalf("expected 1/1 divergence, got ahead=%d behind=%d", state.Ahead, state.Behind)
	}

	conflictRepo := filepath.Join(root, "conflict")
	initFixtureRepo(t, conflictRepo)
	fixtureGit(t, conflictRepo, "checkout", "-b", "side")
	if err := os.WriteFile(filepath.Join(conflictRepo, "tracked.txt"), []byte("side\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, conflictRepo, "commit", "-am", "side")
	fixtureGit(t, conflictRepo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(conflictRepo, "tracked.txt"), []byte("main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, conflictRepo, "commit", "-am", "main")
	cmd := exec.Command("git", "merge", "side")
	cmd.Dir = conflictRepo
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected merge conflict, got success: %s", out)
	}
	state, code, err = s.inspectRepository(context.Background(), conflictRepo, false)
	if err != nil {
		t.Fatalf("inspect conflict: code=%s err=%v", code, err)
	}
	if state.Conflicts == 0 || !hasOperation(state.Operations, "merge") {
		t.Fatalf("conflict/in-progress operation not surfaced: %+v", state)
	}
}

func TestRepositoryDiscoverFiltersIdentityAndDeduplicatesWorktrees(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "z-unexpected-main")
	initFixtureRepo(t, repo)
	fixtureGit(t, repo, "remote", "add", "origin", "https://github.com/Acme/Project.git")
	head := fixtureGit(t, repo, "rev-parse", "HEAD")
	wt := filepath.Join(root, "a-task-copy")
	fixtureGit(t, repo, "worktree", "add", "-b", "task", wt, head)

	resp := s.repositoryDiscoverContext(context.Background(), Request{RemoteIdentity: "github.com/Acme/Project"})
	if !resp.OK {
		t.Fatalf("discover failed: %+v", resp)
	}
	if len(resp.Repositories) != 1 {
		t.Fatalf("expected one deduplicated repository, got %d: %+v", len(resp.Repositories), resp.Repositories)
	}
	if resp.Repositories[0].Root != repo || resp.Repositories[0].LinkedWorktree {
		t.Fatalf("discovery did not canonicalize to main worktree: %+v", resp.Repositories[0])
	}
	if len(resp.Repositories[0].Worktrees) != 2 {
		t.Fatalf("expected both worktrees in identity, got %+v", resp.Repositories[0].Worktrees)
	}
}

func TestRepositoryDiscoverIncludesWorkspaceRootRepository(t *testing.T) {
	s, root := repositoryTestServer(t)
	head := initFixtureRepo(t, root)
	resp := s.repositoryDiscoverContext(context.Background(), Request{})
	if !resp.OK || len(resp.Repositories) != 1 {
		t.Fatalf("workspace-root repository not discovered: %+v", resp)
	}
	if resp.Repositories[0].Root != root || resp.Repositories[0].Head != head {
		t.Fatalf("unexpected workspace-root identity: %+v", resp.Repositories[0])
	}
}

func TestWorktreeCreateIsOptimisticAndIdempotent(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	head := initFixtureRepo(t, repo)
	target := filepath.Join(root, "task-worktree")
	req := Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		Branch:         "task",
		StartRef:       "main",
		ExpectedSHA:    head,
	}
	resp := s.worktreeCreateContext(context.Background(), req)
	if !resp.OK || resp.Status != "created" || resp.Repository == nil || resp.Repository.Branch != "task" {
		t.Fatalf("create failed: %+v", resp)
	}
	replay := s.worktreeCreateContext(context.Background(), req)
	if !replay.OK || replay.Status != "already_exists" || !replay.IdempotentReplay {
		t.Fatalf("idempotent replay not recognized: %+v", replay)
	}

	second := req
	second.WorktreePath = filepath.Join(root, "second")
	resp = s.worktreeCreateContext(context.Background(), second)
	if resp.ErrorCode != "branch_in_use" {
		t.Fatalf("expected branch_in_use, got %+v", resp)
	}

	fixtureGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "next.txt"), []byte("next\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "add", "next.txt")
	fixtureGit(t, repo, "commit", "-m", "advance")
	stale := Request{
		RepositoryPath: repo,
		WorktreePath:   filepath.Join(root, "stale"),
		Branch:         "stale-task",
		StartRef:       "main",
		ExpectedSHA:    head,
	}
	resp = s.worktreeCreateContext(context.Background(), stale)
	if resp.ErrorCode != "start_sha_mismatch" || !resp.Retryable {
		t.Fatalf("expected stale start precondition, got %+v", resp)
	}
}

func TestWorktreeRemovePreservesDirtyStateAndRequiresDurability(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	head := initFixtureRepo(t, repo)
	fixtureGit(t, repo, "remote", "add", "origin", "https://example.com/acme/project.git")
	target := filepath.Join(root, "task")
	create := s.worktreeCreateContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		Branch:         "task",
		StartRef:       "main",
		ExpectedSHA:    head,
	})
	if !create.OK {
		t.Fatalf("create: %+v", create)
	}
	fixtureGit(t, target, "config", "branch.task.remote", "origin")
	fixtureGit(t, target, "config", "branch.task.merge", "refs/heads/task")

	if err := os.WriteFile(filepath.Join(target, "untracked.txt"), []byte("keep me\n"), 0644); err != nil {
		t.Fatal(err)
	}
	resp := s.worktreeRemoveContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		ExpectedSHA:    head,
		Disposable:     true,
	})
	if resp.ErrorCode != "dirty_worktree" {
		t.Fatalf("dirty worktree was not protected: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(target, "untracked.txt")); err != nil {
		t.Fatalf("dirty state was lost: %v", err)
	}
	_ = os.Remove(filepath.Join(target, "untracked.txt"))

	s.workspaceHooks = &workspaceTestHooks{remoteHead: func(remoteURL, remoteRef string) (string, bool, error) {
		if remoteRef != "refs/heads/task" {
			t.Fatalf("unexpected remote ref %q", remoteRef)
		}
		return strings.Repeat("f", 40), true, nil
	}}
	resp = s.worktreeRemoveContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		ExpectedSHA:    head,
	})
	if resp.ErrorCode != "remote_head_mismatch" || !resp.Retryable {
		t.Fatalf("remote mismatch did not fail safe: %+v", resp)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("worktree removed despite remote mismatch: %v", err)
	}

	s.workspaceHooks.remoteHead = func(remoteURL, remoteRef string) (string, bool, error) {
		return head, true, nil
	}
	resp = s.worktreeRemoveContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		ExpectedSHA:    head,
	})
	if !resp.OK || resp.Status != "removed" {
		t.Fatalf("durable removal failed: %+v", resp)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree path still exists: %v", err)
	}
	if got := fixtureGit(t, repo, "rev-parse", "refs/heads/task"); got != head {
		t.Fatalf("worktree removal deleted/changed branch: %s", got)
	}
	replay := s.worktreeRemoveContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		ExpectedSHA:    head,
	})
	if !replay.OK || replay.Status != "already_absent" || !replay.IdempotentReplay {
		t.Fatalf("remove replay not idempotent: %+v", replay)
	}
}

func TestWorktreeRemoveRejectsConcurrentIdentityChange(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	head := initFixtureRepo(t, repo)
	target := filepath.Join(root, "task")
	create := s.worktreeCreateContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		Branch:         "task",
		StartRef:       "main",
		ExpectedSHA:    head,
	})
	if !create.OK {
		t.Fatalf("create: %+v", create)
	}
	s.workspaceHooks = &workspaceTestHooks{beforeWorktreeRemove: func() {
		if err := os.WriteFile(filepath.Join(target, "raced.txt"), []byte("new durable candidate\n"), 0644); err != nil {
			t.Fatal(err)
		}
		fixtureGit(t, target, "add", "raced.txt")
		fixtureGit(t, target, "commit", "-m", "concurrent")
	}}
	resp := s.worktreeRemoveContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		ExpectedSHA:    head,
		Disposable:     true,
	})
	if resp.ErrorCode != "repository_changed" || !resp.Retryable {
		t.Fatalf("concurrent HEAD movement was not rejected: %+v", resp)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("worktree was removed after stale identity: %v", err)
	}
	if got := fixtureGit(t, target, "rev-parse", "HEAD"); got == head {
		t.Fatal("concurrency fixture did not move HEAD")
	}
}

func TestWorktreeRemoveRefusesMainAndUnprovenRemote(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	head := initFixtureRepo(t, repo)

	resp := s.worktreeRemoveContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   repo,
		ExpectedSHA:    head,
		Disposable:     true,
	})
	if resp.ErrorCode != "main_worktree_protected" {
		t.Fatalf("main worktree removal was not blocked: %+v", resp)
	}

	target := filepath.Join(root, "task")
	create := s.worktreeCreateContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		Branch:         "task",
		StartRef:       "main",
		ExpectedSHA:    head,
	})
	if !create.OK {
		t.Fatalf("create: %+v", create)
	}
	resp = s.worktreeRemoveContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		ExpectedSHA:    head,
	})
	if resp.ErrorCode != "durability_not_proven" {
		t.Fatalf("missing remote durability was not blocked: %+v", resp)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("worktree removed without durability proof: %v", err)
	}
}

func TestFreshCloneRecoveryCreatesTaskWorktreeFromRemoteBranch(t *testing.T) {
	s, root := repositoryTestServer(t)
	remote := filepath.Join(root, "durable.git")
	fixtureGit(t, root, "init", "--bare", remote)
	source := filepath.Join(root, "source")
	mainHead := initFixtureRepo(t, source)
	fixtureGit(t, source, "remote", "add", "origin", remote)
	fixtureGit(t, source, "push", "-u", "origin", "main")
	fixtureGit(t, source, "checkout", "-b", "issue-123")
	if err := os.WriteFile(filepath.Join(source, "task.txt"), []byte("durable task\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, source, "add", "task.txt")
	fixtureGit(t, source, "commit", "-m", "task")
	taskHead := fixtureGit(t, source, "rev-parse", "HEAD")
	fixtureGit(t, source, "push", "-u", "origin", "issue-123")
	if taskHead == mainHead {
		t.Fatal("task branch did not advance")
	}

	fresh := filepath.Join(root, "fresh-unrelated-name")
	fixtureGit(t, root, "clone", remote, fresh)
	fixtureGit(t, fresh, "checkout", "main")
	resp := s.repositoryDiscoverContext(context.Background(), Request{})
	if !resp.OK {
		t.Fatalf("discover fresh clone: %+v", resp)
	}
	found := false
	for _, state := range resp.Repositories {
		if state.Root == fresh {
			found = true
		}
	}
	if !found {
		t.Fatalf("fresh clone was not discoverable: %+v", resp.Repositories)
	}

	target := filepath.Join(root, "recovered-task")
	create := s.worktreeCreateContext(context.Background(), Request{
		RepositoryPath: fresh,
		WorktreePath:   target,
		Branch:         "issue-123",
		StartRef:       "origin/issue-123",
		ExpectedSHA:    taskHead,
	})
	if !create.OK || create.Repository == nil || create.Repository.Head != taskHead {
		t.Fatalf("fresh-server task recovery failed: %+v", create)
	}
}

func TestRemoteVerificationFailureIsExplicit(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	head := initFixtureRepo(t, repo)
	fixtureGit(t, repo, "remote", "add", "origin", "https://example.com/acme/project.git")
	fixtureGit(t, repo, "config", "branch.main.remote", "origin")
	fixtureGit(t, repo, "config", "branch.main.merge", "refs/heads/main")
	s.workspaceHooks = &workspaceTestHooks{remoteHead: func(remoteURL, remoteRef string) (string, bool, error) {
		return "", false, errors.New("ambiguous remote outcome")
	}}
	state, code, err := s.inspectRepository(context.Background(), repo, true)
	if err != nil {
		t.Fatalf("local inspection should survive remote verification failure: code=%s err=%v", code, err)
	}
	if !state.RemoteVerification.Attempted || state.RemoteVerification.Succeeded || state.RemoteVerification.ErrorCode != "remote_verification_failed" {
		t.Fatalf("remote failure not explicit: %+v", state.RemoteVerification)
	}
	if state.Head != head {
		t.Fatalf("local identity changed: %s", state.Head)
	}
}

func TestRepositoryProtocolExpectedSHAIsFullLength(t *testing.T) {
	if fullCommitSHA.MatchString(strings.Repeat("a", 39)) || !fullCommitSHA.MatchString(strings.Repeat("a", 40)) {
		t.Fatal("full commit SHA validation is not exact")
	}
}

func TestRepositoryStructuredGitDisablesHooksAndFSMonitor(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	head := initFixtureRepo(t, repo)
	marker := filepath.Join(root, "hook-ran")
	monitor := filepath.Join(root, "fsmonitor-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf hook > "+marker+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	monitorScript := filepath.Join(root, "monitor.sh")
	if err := os.WriteFile(monitorScript, []byte("#!/bin/sh\nprintf monitor > "+monitor+"\nprintf '0\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "config", "core.fsmonitor", monitorScript)

	state, code, err := s.inspectRepository(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("inspect with hostile hooks configured: code=%s err=%v", code, err)
	}
	if state.Head != head {
		t.Fatalf("unexpected head: %s", state.Head)
	}
	target := filepath.Join(root, "task")
	resp := s.worktreeCreateContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		Branch:         "task",
		StartRef:       "main",
		ExpectedSHA:    head,
	})
	if !resp.OK {
		t.Fatalf("structured worktree create failed: %+v", resp)
	}
	for _, path := range []string{marker, monitor} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("structured Git executed repository-owned hook/fsmonitor %s: %v", path, err)
		}
	}
}

func TestWorktreeCreateRefusesRepositoryCheckoutFiltersWithoutExecutingThem(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	initFixtureRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=evil\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "add", ".gitattributes")
	fixtureGit(t, repo, "commit", "-m", "declare filter")
	head := fixtureGit(t, repo, "rev-parse", "HEAD")
	marker := filepath.Join(root, "filter-ran")
	fixtureGit(t, repo, "config", "filter.evil.smudge", "sh -c 'printf ran > "+marker+"; cat'")
	fixtureGit(t, repo, "config", "filter.evil.clean", "cat")
	fixtureGit(t, repo, "config", "filter.evil.required", "true")

	target := filepath.Join(root, "task")
	resp := s.worktreeCreateContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   target,
		Branch:         "task",
		StartRef:       "main",
		ExpectedSHA:    head,
	})
	if resp.ErrorCode != "checkout_filter_unsupported" {
		t.Fatalf("expected checkout_filter_unsupported, got %+v", resp)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("structured preflight executed repository filter: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree was partially created after refusing filter: %v", err)
	}
}

func TestRepositoryRejectsGitMetadataOutsideWorkspace(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	outside := t.TempDir()
	separate := filepath.Join(outside, "gitdir")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, root, "init", "--separate-git-dir", separate, repo)
	fixtureGit(t, repo, "config", "user.name", "Repository Test")
	fixtureGit(t, repo, "config", "user.email", "repository-test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "add", "tracked.txt")
	fixtureGit(t, repo, "commit", "-m", "base")

	if _, code, err := s.inspectRepository(context.Background(), repo, false); err == nil || code != "invalid_repository_path" {
		t.Fatalf("external Git metadata was accepted: code=%s err=%v", code, err)
	}
}

func TestRepositoryPathsCannotEscapeWorkspace(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	head := initFixtureRepo(t, repo)
	outside := t.TempDir()

	if _, code, err := s.inspectRepository(context.Background(), outside, false); err == nil || code != "invalid_repository_path" {
		t.Fatalf("outside repository path accepted: code=%s err=%v", code, err)
	}
	resp := s.worktreeCreateContext(context.Background(), Request{
		RepositoryPath: repo,
		WorktreePath:   filepath.Join(outside, "task"),
		Branch:         "task",
		StartRef:       "main",
		ExpectedSHA:    head,
	})
	if resp.ErrorCode != "invalid_worktree_path" {
		t.Fatalf("outside worktree path accepted: %+v", resp)
	}
}
