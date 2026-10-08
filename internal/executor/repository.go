package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	repositoryOperationTimeout = 30 * time.Second
	maxGitMetadataBytes        = 1 << 20
	maxRepositoryResults       = 64
	maxRepositoryMarkers       = 128
	maxRepositoryScanDepth     = 4
	maxStatusEntries           = 10000
)

var fullCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func repositoryError(code, class, message string, retryable bool) Response {
	return Response{
		Error:      message,
		ReasonCode: code,
		ErrorCode:  code,
		ErrorClass: class,
		Retryable:  retryable,
	}
}

func withinPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (s *Server) workspaceRoot() (string, error) {
	root, err := filepath.Abs(s.cfg.WorkspaceDir)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(root), nil
}

func (s *Server) workspacePath(input string, mustExist bool) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", errors.New("workspace path is empty")
	}
	root, err := s.workspaceRoot()
	if err != nil {
		return "", err
	}
	path := input
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	if !withinPath(root, path) {
		return "", errors.New("path is outside the configured workspace")
	}
	if mustExist {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		resolved = filepath.Clean(resolved)
		if !withinPath(root, resolved) {
			return "", errors.New("resolved path is outside the configured workspace")
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", errors.New("workspace path is not a directory")
		}
		return resolved, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("resolve worktree parent: %w", err)
	}
	parent = filepath.Clean(parent)
	if !withinPath(root, parent) {
		return "", errors.New("worktree parent resolves outside the configured workspace")
	}
	if base := filepath.Base(path); base == "." || base == string(filepath.Separator) || base == "" {
		return "", errors.New("invalid worktree path")
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

var (
	errGitMetadataTooLarge = errors.New("git metadata output exceeded the bounded limit")
	errGitUnavailable      = errors.New("git executable is unavailable")
)

type boundedGitBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedGitBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining > 0 {
		keep := len(p)
		if keep > remaining {
			keep = remaining
		}
		_, _ = b.Buffer.Write(p[:keep])
	}
	if len(p) > remaining {
		b.overflow = true
	}
	return len(p), nil
}

func (s *Server) workerGitCommand(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	return s.workerGitCommandEnv(ctx, dir, nil, args...)
}

func (s *Server) workerGitCommandEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (*exec.Cmd, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, errGitUnavailable
	}
	safeArgs := []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.attributesFile=/dev/null",
		"-c", "core.sshCommand=ssh",
	}
	safeArgs = append(safeArgs, args...)
	cmd := exec.CommandContext(ctx, gitPath, safeArgs...)
	cmd.Dir = dir
	cmd.Env = append(sanitizedCommandEnv(s.cfg.WorkspaceDir),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https:ssh:git",
		"GIT_ATTR_NOSYSTEM=1",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid:    s.workerUID,
			Gid:    s.workerGID,
			Groups: []uint32{s.workerGID},
		}
	} else if uint32(os.Geteuid()) != s.workerUID {
		return nil, errors.New("executor cannot assume configured worker identity")
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_, err := terminateProcessGroup(cmd.Process.Pid)
		return err
	}
	cmd.WaitDelay = processGroupTerminateGrace
	return cmd, nil
}

func runGitCommand(ctx context.Context, cmd *exec.Cmd, stdin []byte, raw bool) ([]byte, int, error) {
	stdout := &boundedGitBuffer{limit: maxGitMetadataBytes}
	cmd.Stdout = stdout
	cmd.Stderr = nil
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if stdout.overflow {
		return nil, -1, errGitMetadataTooLarge
	}
	if ctx.Err() != nil {
		return nil, -1, ctx.Err()
	}
	output := stdout.Bytes()
	if !raw {
		output = bytes.TrimSpace(output)
	}
	if err == nil {
		return output, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output, exitErr.ExitCode(), nil
	}
	return nil, -1, err
}

func (s *Server) gitResult(ctx context.Context, dir string, args ...string) (string, int, error) {
	cmd, err := s.workerGitCommand(ctx, dir, args...)
	if err != nil {
		return "", -1, err
	}
	output, code, err := runGitCommand(ctx, cmd, nil, false)
	return string(output), code, err
}

func (s *Server) gitRawResultEnv(ctx context.Context, dir string, extraEnv []string, stdin []byte, args ...string) ([]byte, int, error) {
	cmd, err := s.workerGitCommandEnv(ctx, dir, extraEnv, args...)
	if err != nil {
		return nil, -1, err
	}
	return runGitCommand(ctx, cmd, stdin, true)
}

func requireGitSuccess(output string, code int, err error, operation string) (string, error) {
	if err != nil {
		return "", fmt.Errorf("%s: %w", operation, err)
	}
	if code != 0 {
		return "", fmt.Errorf("%s failed", operation)
	}
	return output, nil
}

func absoluteGitPath(base, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	abs, err := filepath.Abs(filepath.Join(base, path))
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func conflictStatus(code string) bool {
	switch code {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	default:
		return len(code) == 2 && (code[0] == 'U' || code[1] == 'U')
	}
}

func (s *Server) repositoryStatusCounts(ctx context.Context, dir string) (staged, unstaged, untracked, conflicts int, truncated bool, err error) {
	cmd, err := s.workerGitCommand(ctx, dir, "status", "--porcelain=v1", "--untracked-files=normal", "--no-renames", "--ignore-submodules=all")
	if err != nil {
		return 0, 0, 0, 0, false, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, 0, 0, 0, false, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return 0, 0, 0, 0, false, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	entries := 0
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 2 {
			continue
		}
		entries++
		if entries > maxStatusEntries {
			truncated = true
			if cmd.Process != nil {
				_, _ = terminateProcessGroup(cmd.Process.Pid)
			}
			break
		}
		code := line[:2]
		if code == "??" {
			untracked++
			continue
		}
		if conflictStatus(code) {
			conflicts++
		}
		if code[0] != ' ' && code[0] != '?' {
			staged++
		}
		if code[1] != ' ' && code[1] != '?' {
			unstaged++
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if truncated {
		return staged, unstaged, untracked, conflicts, true, nil
	}
	if scanErr != nil {
		return 0, 0, 0, 0, false, scanErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return 0, 0, 0, 0, false, ctx.Err()
		}
		return 0, 0, 0, 0, false, errors.New("git status failed")
	}
	return staged, unstaged, untracked, conflicts, false, nil
}

func parseWorktreeList(raw string) []RepositoryWorktree {
	var result []RepositoryWorktree
	var current *RepositoryWorktree
	flush := func() {
		if current != nil && current.Path != "" {
			result = append(result, *current)
		}
		current = nil
	}
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current = &RepositoryWorktree{Path: filepath.Clean(strings.TrimPrefix(line, "worktree "))}
		case current == nil:
			continue
		case strings.HasPrefix(line, "HEAD "):
			current.Head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
		case strings.HasPrefix(line, "branch refs/heads/"):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case line == "detached":
			current.Detached = true
		case line == "bare":
			current.Bare = true
		case strings.HasPrefix(line, "locked"):
			current.Locked = true
		case strings.HasPrefix(line, "prunable"):
			current.Prunable = true
		}
	}
	flush()
	return result
}

func operationState(gitDir string) []string {
	checks := []struct {
		name string
		path string
	}{
		{"merge", "MERGE_HEAD"},
		{"rebase", "rebase-merge"},
		{"rebase", "rebase-apply"},
		{"cherry-pick", "CHERRY_PICK_HEAD"},
		{"revert", "REVERT_HEAD"},
		{"bisect", "BISECT_START"},
		{"sequencer", "sequencer"},
	}
	seen := map[string]bool{}
	var result []string
	for _, check := range checks {
		if _, err := os.Lstat(filepath.Join(gitDir, check.path)); err == nil && !seen[check.name] {
			seen[check.name] = true
			result = append(result, check.name)
		}
	}
	sort.Strings(result)
	return result
}

var scpRemote = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

func describeRemote(raw string) (safeURL, identity string, external, supported bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false, false
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		scheme := strings.ToLower(u.Scheme)
		external = scheme != "file"
		supported = scheme == "https" || scheme == "ssh" || scheme == "git"
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		u.Host = strings.ToLower(u.Host)
		safeURL = u.String()
		path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
		if external {
			identity = u.Host + "/" + path
			identity = strings.TrimSuffix(identity, "/")
		} else {
			identity = "local:" + filepath.Clean(u.Path)
		}
		return safeURL, identity, external, supported
	}
	if match := scpRemote.FindStringSubmatch(raw); len(match) == 3 {
		host := strings.ToLower(match[1])
		path := strings.TrimSuffix(strings.Trim(match[2], "/"), ".git")
		return host + ":" + path, host + "/" + path, true, true
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") {
		return filepath.Clean(raw), "local:" + filepath.Clean(raw), false, false
	}
	value := strings.TrimSuffix(strings.Trim(raw, "/"), ".git")
	if strings.Contains(value, "/") {
		parts := strings.SplitN(value, "/", 2)
		return value, strings.ToLower(parts[0]) + "/" + parts[1], true, false
	}
	return value, value, false, false
}

func (s *Server) repositoryRemotes(ctx context.Context, dir string) ([]RepositoryRemote, error) {
	raw, code, err := s.gitResult(ctx, dir, "remote")
	if err != nil || code != 0 {
		return nil, errors.New("git remote inspection failed")
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	names := strings.Fields(raw)
	sort.Strings(names)
	result := make([]RepositoryRemote, 0, len(names))
	for _, name := range names {
		fetchRaw, code, err := s.gitResult(ctx, dir, "config", "--get", "remote."+name+".url")
		if err != nil || code != 0 {
			continue
		}
		pushRaw, pushCode, pushErr := s.gitResult(ctx, dir, "config", "--get", "remote."+name+".pushurl")
		if pushErr != nil || pushCode != 0 || pushRaw == "" {
			pushRaw = fetchRaw
		}
		safe, identity, _, _ := describeRemote(fetchRaw)
		safePush, _, _, _ := describeRemote(pushRaw)
		result = append(result, RepositoryRemote{
			Name:     name,
			URL:      safe,
			PushURL:  safePush,
			Identity: identity,
		})
	}
	return result, nil
}

func (s *Server) branchUpstreamConfig(ctx context.Context, dir, branch string) (remote, remoteBranch string, ok bool) {
	if branch == "" {
		return "", "", false
	}
	remote, code, err := s.gitResult(ctx, dir, "config", "--get", "branch."+branch+".remote")
	if err != nil || code != 0 || remote == "" || remote == "." {
		return "", "", false
	}
	merge, code, err := s.gitResult(ctx, dir, "config", "--get", "branch."+branch+".merge")
	if err != nil || code != 0 || !strings.HasPrefix(merge, "refs/heads/") {
		return "", "", false
	}
	return remote, strings.TrimPrefix(merge, "refs/heads/"), true
}

func normalizeRemoteBranch(branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if branch == "" || strings.ContainsAny(branch, "\n\r\x00") {
		return "", errors.New("invalid remote branch")
	}
	return branch, nil
}

func (s *Server) resolveRemoteHead(ctx context.Context, dir, remoteName, remoteBranch string) (head string, exists bool, code string) {
	rawURL, exitCode, err := s.gitResult(ctx, dir, "config", "--get", "remote."+remoteName+".url")
	if err != nil || exitCode != 0 || rawURL == "" {
		return "", false, "remote_not_configured"
	}
	_, _, external, supported := describeRemote(rawURL)
	if !external || !supported {
		return "", false, "remote_protocol_not_supported"
	}
	ref := "refs/heads/" + remoteBranch
	if s.workspaceHooks != nil && s.workspaceHooks.remoteHead != nil {
		head, exists, err := s.workspaceHooks.remoteHead(rawURL, ref)
		if err != nil {
			return "", false, "remote_verification_failed"
		}
		return head, exists, ""
	}
	// Run network reconciliation outside the repository so repository-local
	// credential helpers, SSH commands and HTTP settings cannot affect it.
	out, exitCode, err := s.gitResult(ctx, "/", "ls-remote", "--exit-code", rawURL, ref)
	if err != nil {
		return "", false, "remote_verification_failed"
	}
	if exitCode == 2 {
		return "", false, ""
	}
	if exitCode != 0 {
		return "", false, "remote_verification_failed"
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || !fullCommitSHA.MatchString(fields[0]) {
		return "", false, "remote_verification_failed"
	}
	return fields[0], true, ""
}

func (s *Server) inspectRepository(ctx context.Context, inputPath string, verifyRemote bool) (RepositoryState, string, error) {
	path, err := s.workspacePath(inputPath, true)
	if err != nil {
		return RepositoryState{}, "invalid_repository_path", err
	}
	headBefore, code, err := s.gitResult(ctx, path, "rev-parse", "--verify", "HEAD")
	if errors.Is(err, errGitUnavailable) {
		return RepositoryState{}, "git_unavailable", errGitUnavailable
	}
	if err != nil || code != 0 || !fullCommitSHA.MatchString(headBefore) {
		return RepositoryState{}, "not_repository", errors.New("path is not a usable Git worktree")
	}
	rootOut, rootCode, rootErr := s.gitResult(ctx, path, "rev-parse", "--show-toplevel")
	root, err := requireGitSuccess(rootOut, rootCode, rootErr, "resolve repository root")
	if err != nil {
		return RepositoryState{}, "not_repository", err
	}
	root, err = absoluteGitPath(path, root)
	if err != nil {
		return RepositoryState{}, "not_repository", err
	}
	workspaceRoot, err := s.workspaceRoot()
	if err != nil || !withinPath(workspaceRoot, root) {
		return RepositoryState{}, "invalid_repository_path", errors.New("repository root is outside the configured workspace")
	}
	gitDirOut, gitDirCode, gitDirErr := s.gitResult(ctx, path, "rev-parse", "--git-dir")
	gitDirRaw, err := requireGitSuccess(gitDirOut, gitDirCode, gitDirErr, "resolve git dir")
	if err != nil {
		return RepositoryState{}, "not_repository", err
	}
	gitDir, err := absoluteGitPath(path, gitDirRaw)
	if err != nil {
		return RepositoryState{}, "not_repository", err
	}
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil || !withinPath(workspaceRoot, filepath.Clean(gitDir)) {
		return RepositoryState{}, "invalid_repository_path", errors.New("repository Git directory is outside the configured workspace")
	}
	gitDir = filepath.Clean(gitDir)
	commonOut, commonCode, commonErr := s.gitResult(ctx, path, "rev-parse", "--git-common-dir")
	commonRaw, err := requireGitSuccess(commonOut, commonCode, commonErr, "resolve common git dir")
	if err != nil {
		return RepositoryState{}, "not_repository", err
	}
	commonDir, err := absoluteGitPath(path, commonRaw)
	if err != nil {
		return RepositoryState{}, "not_repository", err
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil || !withinPath(workspaceRoot, filepath.Clean(commonDir)) {
		return RepositoryState{}, "invalid_repository_path", errors.New("repository common Git directory is outside the configured workspace")
	}
	commonDir = filepath.Clean(commonDir)
	worktreeOut, worktreeCode, worktreeErr := s.gitResult(ctx, path, "worktree", "list", "--porcelain")
	worktreeRaw, err := requireGitSuccess(worktreeOut, worktreeCode, worktreeErr, "list worktrees")
	if err != nil {
		return RepositoryState{}, "git_failed", err
	}
	worktrees := parseWorktreeList(worktreeRaw)
	mainWorktree := ""
	if len(worktrees) > 0 && !worktrees[0].Bare {
		mainWorktree = filepath.Clean(worktrees[0].Path)
	}
	branch, branchCode, branchErr := s.gitResult(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr != nil {
		return RepositoryState{}, "git_failed", branchErr
	}
	detached := branchCode != 0
	if detached {
		branch = ""
	}
	upstream := ""
	ahead, behind := 0, 0
	if !detached {
		if value, upstreamCode, upstreamErr := s.gitResult(ctx, path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); upstreamErr == nil && upstreamCode == 0 {
			upstream = value
			if counts, countCode, countErr := s.gitResult(ctx, path, "rev-list", "--left-right", "--count", "HEAD...@{upstream}"); countErr == nil && countCode == 0 {
				fields := strings.Fields(counts)
				if len(fields) == 2 {
					ahead, _ = strconv.Atoi(fields[0])
					behind, _ = strconv.Atoi(fields[1])
				}
			}
		}
	}
	staged, unstaged, untracked, conflicts, statusTruncated, err := s.repositoryStatusCounts(ctx, path)
	if err != nil {
		return RepositoryState{}, "git_failed", err
	}
	remotes, err := s.repositoryRemotes(ctx, path)
	if err != nil {
		return RepositoryState{}, "git_failed", err
	}
	state := RepositoryState{
		Path:            path,
		Root:            root,
		GitDir:          gitDir,
		CommonGitDir:    commonDir,
		MainWorktree:    mainWorktree,
		LinkedWorktree:  mainWorktree != "" && filepath.Clean(root) != filepath.Clean(mainWorktree),
		Head:            headBefore,
		Branch:          branch,
		Detached:        detached,
		Upstream:        upstream,
		Ahead:           ahead,
		Behind:          behind,
		Dirty:           staged > 0 || unstaged > 0 || untracked > 0 || conflicts > 0 || statusTruncated,
		Staged:          staged,
		Unstaged:        unstaged,
		Untracked:       untracked,
		Conflicts:       conflicts,
		StatusTruncated: statusTruncated,
		Operations:      operationState(gitDir),
		Remotes:         remotes,
		Worktrees:       worktrees,
	}
	if verifyRemote {
		state.RemoteVerification.Attempted = true
		remote, remoteBranch, ok := s.branchUpstreamConfig(ctx, path, branch)
		if !ok {
			state.RemoteVerification.ErrorCode = "upstream_not_configured"
		} else {
			state.RemoteVerification.Remote = remote
			state.RemoteVerification.Branch = remoteBranch
			remoteHead, exists, remoteCode := s.resolveRemoteHead(ctx, path, remote, remoteBranch)
			state.RemoteVerification.ErrorCode = remoteCode
			if remoteCode == "" {
				state.RemoteVerification.Succeeded = true
				state.RemoteVerification.Exists = exists
				state.RemoteVerification.Head = remoteHead
				state.RemoteVerification.MatchesHead = exists && remoteHead == headBefore
			}
		}
	}
	headAfter, code, err := s.gitResult(ctx, path, "rev-parse", "--verify", "HEAD")
	if err != nil || code != 0 || headAfter != headBefore {
		return state, "repository_changed", errors.New("repository HEAD changed during inspection")
	}
	return state, "", nil
}

func repositoryIdentityMatches(state RepositoryState, requested string) bool {
	if strings.TrimSpace(requested) == "" {
		return true
	}
	_, identity, _, _ := describeRemote(requested)
	if identity == "" {
		identity = strings.TrimSuffix(strings.Trim(requested, "/"), ".git")
	}
	for _, remote := range state.Remotes {
		if strings.EqualFold(remote.Identity, identity) || strings.EqualFold(remote.URL, requested) {
			return true
		}
	}
	return false
}

func repositoryDepth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

func (s *Server) repositoryMarkers() ([]string, bool, error) {
	root, err := s.workspaceRoot()
	if err != nil {
		return nil, false, err
	}
	var markers []string
	truncated := false
	if _, markerErr := os.Lstat(filepath.Join(root, ".git")); markerErr == nil {
		markers = append(markers, root)
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root {
			depth := repositoryDepth(root, path)
			if depth > maxRepositoryScanDepth {
				return filepath.SkipDir
			}
			switch entry.Name() {
			case ".git", "node_modules", "vendor", ".cache", ".config", ".local", ".toolchains":
				return filepath.SkipDir
			}
			if strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				if len(markers) >= maxRepositoryMarkers {
					truncated = true
					return filepath.SkipDir
				}
				markers = append(markers, path)
				return filepath.SkipDir
			}
		}
		return nil
	})
	sort.Strings(markers)
	return markers, truncated, err
}

func (s *Server) repositoryDiscoverContext(parent context.Context, req Request) Response {
	if _, err := exec.LookPath("git"); err != nil {
		return repositoryError("git_unavailable", "environment", errGitUnavailable.Error(), false)
	}
	release, ok := s.runs.acquire(false)
	if !ok {
		return runCapacityResponse(false)
	}
	defer release()
	ctx, cancel := context.WithTimeout(parent, repositoryOperationTimeout)
	defer cancel()

	markers, truncated, err := s.repositoryMarkers()
	if err != nil {
		return repositoryError("workspace_scan_failed", "state", "workspace repository scan failed", true)
	}
	seen := map[string]bool{}
	var repositories []RepositoryState
	var discoveryErrors []RepositoryDiscoveryError
	for _, marker := range markers {
		state, code, err := s.inspectRepository(ctx, marker, false)
		if err != nil {
			discoveryErrors = append(discoveryErrors, RepositoryDiscoveryError{Path: marker, ErrorCode: code})
			continue
		}
		if state.LinkedWorktree && state.MainWorktree != "" {
			mainState, mainCode, mainErr := s.inspectRepository(ctx, state.MainWorktree, false)
			if mainErr != nil {
				discoveryErrors = append(discoveryErrors, RepositoryDiscoveryError{Path: state.MainWorktree, ErrorCode: mainCode})
				continue
			}
			if mainState.CommonGitDir != state.CommonGitDir {
				discoveryErrors = append(discoveryErrors, RepositoryDiscoveryError{Path: marker, ErrorCode: "repository_identity_mismatch"})
				continue
			}
			state = mainState
		}
		if seen[state.CommonGitDir] || !repositoryIdentityMatches(state, req.RemoteIdentity) {
			continue
		}
		seen[state.CommonGitDir] = true
		if len(repositories) >= maxRepositoryResults {
			truncated = true
			break
		}
		repositories = append(repositories, state)
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Root < repositories[j].Root })
	return Response{
		OK:                 true,
		Status:             "complete",
		Repositories:       repositories,
		RepositoryErrors:   discoveryErrors,
		DiscoveryTruncated: truncated,
	}
}

func (s *Server) repositoryInspectContext(parent context.Context, req Request) Response {
	release, ok := s.runs.acquire(false)
	if !ok {
		return runCapacityResponse(false)
	}
	defer release()
	ctx, cancel := context.WithTimeout(parent, repositoryOperationTimeout)
	defer cancel()
	state, code, err := s.inspectRepository(ctx, req.RepositoryPath, req.VerifyRemote)
	if err != nil {
		resp := repositoryError(code, "state", err.Error(), code == "repository_changed")
		if state.Head != "" {
			resp.Repository = &state
		}
		return resp
	}
	return Response{OK: true, Status: "complete", Repository: &state}
}
