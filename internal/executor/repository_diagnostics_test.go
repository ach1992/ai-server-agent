package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fail only the named Git stage and delegate every other stage to real Git.
func installFaultGit(t *testing.T, stage string, exitCode int, diagnostic string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
while [ "$1" = "-c" ]; do shift 2; done
if [ "$1" = %q ] || [ "$1 $2" = %q ]; then
	cat >&2 <<'NATIVE_GIT_DIAGNOSTIC'
%s
NATIVE_GIT_DIAGNOSTIC
	exit %d
fi
exec %q "$@"
`, stage, stage, diagnostic, exitCode, realGit)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

func assertGitFailureDiagnostic(t *testing.T, err error, stage string, exitCode int) {
	t.Helper()
	if err == nil {
		t.Fatal("native Git error was discarded")
	}
	for _, want := range []string{
		"git " + stage + " exited " + fmt.Sprint(exitCode),
		"stderr=", "native Git diagnostic", "[REDACTED]",
		"https://example.invalid/repo.git",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("native Git context %q missing: %v", want, err)
		}
	}
	for _, secret := range []string{"PRIVATE-API-KEY", "PRIVATE-PASSWORD", "PRIVATE-URL-PASSWORD", "PRIVATE-QUERY", "PRIVATE-BEARER"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("native Git error leaked credential %q: %v", secret, err)
		}
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitCode {
		t.Fatalf("native Git exit was not recoverable: exit=%v err=%v", exit, err)
	}
}

const diagnosticGitFailure = "native Git diagnostic: repository index failure; api_key=PRIVATE-API-KEY; password='PRIVATE-PASSWORD'; Authorization: Bearer PRIVATE-BEARER; remote=https://user:PRIVATE-URL-PASSWORD@example.invalid/repo.git?token=PRIVATE-QUERY"

func TestRepositoryGitResultPreservesNativeFailureAndBoundedStderr(t *testing.T) {
	t.Run("native failure and targeted redaction", func(t *testing.T) {
		s, root := repositoryTestServer(t)
		installFaultGit(t, "rev-parse", 47, diagnosticGitFailure)
		_, code, err := s.gitResult(context.Background(), root, "rev-parse", "--verify", "HEAD")
		if code != 47 {
			t.Fatalf("lost native exit 47: code=%d err=%v", code, err)
		}
		assertGitFailureDiagnostic(t, err, "rev-parse", 47)
	})
	t.Run("partial URL credential at capture boundary", func(t *testing.T) {
		s, root := repositoryTestServer(t)
		prefix := strings.Repeat("Z", maxGitStderrBytes-128)
		installFaultGit(t, "rev-parse", 47, prefix+" https://user:PRIVATE-URL-PASSWORD-"+strings.Repeat("P", 1024)+"@example.invalid/repo.git")
		_, code, err := s.gitResult(context.Background(), root, "rev-parse", "--verify", "HEAD")
		if code != 47 || err == nil || strings.Contains(err.Error(), "PRIVATE-URL-") ||
			(!strings.Contains(err.Error(), "[TRUNCATED_URL]") && !strings.Contains(err.Error(), "https://example.invalid/repo.git")) {
			detail := fmt.Sprint(err)
			if len(detail) > 180 {
				detail = detail[len(detail)-180:]
			}
			t.Fatalf("partial remote credential escaped the diagnostic boundary: code=%d error_tail=%q", code, detail)
		}
	})
	t.Run("stderr final bound", func(t *testing.T) {
		s, root := repositoryTestServer(t)
		installFaultGit(t, "rev-parse", 73, strings.Repeat("token=x;", 2000))
		_, code, err := s.gitResult(context.Background(), root, "rev-parse", "--verify", "HEAD")
		if code != 73 || err == nil {
			t.Fatalf("lost bounded failure: code=%d err=%v", code, err)
		}
		if !strings.Contains(err.Error(), "native stderr truncated at 4096 bytes") ||
			len(err.Error()) > maxGitStderrBytes*2 ||
			strings.Contains(err.Error(), "token=x;") {
			t.Fatalf("Git stderr bound or redaction failed: length=%d error=%v", len(err.Error()), err)
		}
	})
}

func TestGitNativeStderrRedactsCutURLInBoundedBuffer(t *testing.T) {
	stderr := &boundedGitBuffer{limit: maxGitStderrBytes}
	_, _ = stderr.Write([]byte(strings.Repeat("Z", maxGitStderrBytes-128) +
		" https://user:PRIVATE-URL-PASSWORD-" + strings.Repeat("P", 1024) + "@example.invalid/repo.git"))
	if !stderr.overflow {
		t.Fatal("test failed to exercise stderr capture overflow")
	}
	detail := gitNativeStderr(stderr)
	if strings.Contains(detail, "PRIVATE-URL-") || !strings.Contains(detail, "[TRUNCATED_URL]") ||
		!strings.Contains(detail, "native stderr truncated") || len(detail) > maxGitStderrBytes {
		t.Fatalf("incomplete URL userinfo leaked or useful truncation signal lost: length=%d tail=%q",
			len(detail), detail[len(detail)-180:])
	}
}

func TestRepositoryStatusFailurePreservesGitDiagnosticThroughPublicResponse(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	initFixtureRepo(t, repo)
	installFaultGit(t, "status", 29, diagnosticGitFailure)

	resp := s.repositoryInspectContext(context.Background(), Request{RepositoryPath: repo})
	if resp.OK || resp.ErrorCode != "git_failed" || resp.ExitCode != 29 {
		t.Fatalf("repository status failure lost its error class or exit: %+v", resp)
	}
	for _, want := range []string{"git status exited 29", "native Git diagnostic", "[REDACTED]", "https://example.invalid/repo.git"} {
		if !strings.Contains(resp.Error, want) {
			t.Fatalf("missing status failure context %q: %+v", want, resp)
		}
	}
	for _, secret := range []string{"PRIVATE-API-KEY", "PRIVATE-PASSWORD", "PRIVATE-URL-PASSWORD", "PRIVATE-QUERY", "PRIVATE-BEARER"} {
		if strings.Contains(resp.Error, secret) {
			t.Fatalf("credential leaked from repository status: %q", resp.Error)
		}
	}
}

func TestCheckoutFilterPreflightPreservesEachNativeGitFailure(t *testing.T) {
	for _, stage := range []string{"read-tree", "ls-files", "check-attr"} {
		t.Run(stage, func(t *testing.T) {
			s, root := repositoryTestServer(t)
			repo := filepath.Join(root, "repo")
			head := initFixtureRepo(t, repo)
			installFaultGit(t, stage, 47, diagnosticGitFailure)

			hasFilter, err := s.commitUsesCheckoutFilter(context.Background(), repo, head)
			if hasFilter {
				t.Fatalf("failing preflight returned filter success: %v", err)
			}
			assertGitFailureDiagnostic(t, err, stage, 47)

			target := filepath.Join(root, "task")
			resp := s.worktreeCreateContext(context.Background(), Request{
				RepositoryPath: repo, WorktreePath: target,
				Branch: "task", StartRef: "main", ExpectedSHA: head,
			})
			if resp.OK || resp.ErrorCode != "checkout_filter_scan_failed" ||
				resp.ExitCode != 47 || !strings.Contains(resp.Error, "git "+stage+" exited 47") {
				t.Fatalf("worktree creation hid native preflight failure: %+v", resp)
			}
			for _, secret := range []string{"PRIVATE-API-KEY", "PRIVATE-PASSWORD", "PRIVATE-URL-PASSWORD", "PRIVATE-QUERY", "PRIVATE-BEARER"} {
				if strings.Contains(resp.Error, secret) {
					t.Fatalf("credential leaked from preflight: %+v", resp)
				}
			}
			if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("preflight failure changed worktree target: %v", statErr)
			}
		})
	}
}

func TestRepositoryExpectedGitNonzeroSemanticsAreStillNonfatal(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	initFixtureRepo(t, repo)

	// Optional untracked paths return Git exit 1, not a retryable failure.
	_, code, err := s.gitResult(context.Background(), repo, "ls-files", "--error-unmatch", "--", "optional-not-tracked")
	if code != 1 || err == nil {
		t.Fatalf("unexpected optional-path exit semantics: code=%d err=%v", code, err)
	}

	fixtureGit(t, repo, "checkout", "--detach")
	resp := s.repositoryInspectContext(context.Background(), Request{RepositoryPath: repo})
	if !resp.OK || resp.Repository == nil || !resp.Repository.Detached || resp.Repository.Branch != "" {
		t.Fatalf("expected symbolic-ref exit 1 to mean detached HEAD, not fatal: %+v", resp)
	}

	head := fixtureGit(t, repo, "rev-parse", "HEAD")
	create := s.worktreeCreateContext(context.Background(), Request{
		RepositoryPath: repo, WorktreePath: filepath.Join(root, "task"),
		Branch: "new-branch", StartRef: head, ExpectedSHA: head,
	})
	if !create.OK {
		t.Fatalf("missing optional branch was treated as a fatal Git failure: %+v", create)
	}
}

func TestRepositoryRemoteMissingRefKeepsExpectedGitExitTwo(t *testing.T) {
	s, root := repositoryTestServer(t)
	repo := filepath.Join(root, "repo")
	initFixtureRepo(t, repo)
	fixtureGit(t, repo, "remote", "add", "origin", "https://example.invalid/acme/repo.git")
	installFaultGit(t, "ls-remote", 2, "remote branch not found")
	head, exists, code := s.resolveRemoteHead(context.Background(), repo, "origin", "not-present")
	if head != "" || exists || code != "" {
		t.Fatalf("expected Git ls-remote exit 2 to mean missing remote ref: head=%q exists=%v code=%q", head, exists, code)
	}
}

func TestWorktreeAddAndRemoveNativeGitFailuresPreserveDiagnosis(t *testing.T) {
	for _, operation := range []string{"add", "remove"} {
		t.Run(operation, func(t *testing.T) {
			s, root := repositoryTestServer(t)
			repo := filepath.Join(root, "repo")
			head := initFixtureRepo(t, repo)
			target := filepath.Join(root, "task")
			if operation == "remove" {
				created := s.worktreeCreateContext(context.Background(), Request{
					RepositoryPath: repo, WorktreePath: target, Branch: "task",
					StartRef: "main", ExpectedSHA: head,
				})
				if !created.OK {
					t.Fatalf("prepare worktree for removal: %+v", created)
				}
			}
			installFaultGit(t, "worktree "+operation, 49, diagnosticGitFailure)
			var resp Response
			if operation == "add" {
				resp = s.worktreeCreateContext(context.Background(), Request{
					RepositoryPath: repo, WorktreePath: target, Branch: "task",
					StartRef: "main", ExpectedSHA: head,
				})
			} else {
				resp = s.worktreeRemoveContext(context.Background(), Request{
					RepositoryPath: repo, WorktreePath: target, ExpectedSHA: head,
					Disposable: true,
				})
			}
			if resp.OK || resp.ErrorCode != "worktree_"+map[string]string{"add": "create", "remove": "remove"}[operation]+"_failed" ||
				resp.ExitCode != 49 || !strings.Contains(resp.Error, "git worktree "+operation) ||
				!strings.Contains(resp.Error, "native Git diagnostic") {
				t.Fatalf("worktree %s failure lost native details: %+v", operation, resp)
			}
			for _, secret := range []string{"PRIVATE-API-KEY", "PRIVATE-PASSWORD", "PRIVATE-URL-PASSWORD", "PRIVATE-QUERY", "PRIVATE-BEARER"} {
				if strings.Contains(resp.Error, secret) {
					t.Fatalf("worktree %s leaked credential: %+v", operation, resp)
				}
			}
			if _, statErr := os.Stat(target); (operation == "remove") != (statErr == nil) {
				t.Fatalf("worktree %s failed to preserve expected target state: %v", operation, statErr)
			}
		})
	}
}
