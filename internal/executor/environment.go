package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	environmentOperationTimeout = 10 * time.Second
	environmentProbeTimeout     = 2 * time.Second
	maxEnvironmentFileBytes     = 256 << 10
	maxEnvironmentOutputBytes   = 512 << 10
	maxEnvironmentEntrypoints   = 64
)

type environmentBuilder struct {
	server           *Server
	root             string
	head             string
	declarations     []EnvironmentDeclaration
	languages        map[string]map[string]bool
	tools            map[string]*EnvironmentTool
	mechanisms       map[string]*EnvironmentMechanism
	entrypoints      []EnvironmentEntrypoint
	warnings         []string
	selectionReasons []string
}

func environmentError(code, class, message string, retryable bool) Response {
	return Response{Error: message, ReasonCode: code, ErrorCode: code, ErrorClass: class, Retryable: retryable}
}

func (s *Server) repositoryEnvironmentContext(parent context.Context, req Request) Response {
	release, ok := s.runs.acquire(false)
	if !ok {
		return runCapacityResponse(false)
	}
	defer release()

	ctx, cancel := context.WithTimeout(parent, environmentOperationTimeout)
	defer cancel()
	summary, code, err := s.inspectRepositoryEnvironment(ctx, req.Path)
	if err != nil {
		return environmentError(code, "state", err.Error(), code == "repository_changed" || code == "environment_probe_failed")
	}
	payload, err := json.Marshal(summary)
	if err != nil {
		return environmentError("environment_encode_failed", "internal", "environment summary could not be encoded", false)
	}
	if len(payload) > maxEnvironmentOutputBytes {
		return environmentError("environment_summary_too_large", "resource", "environment summary exceeded the bounded output limit", false)
	}
	return Response{
		OK:             true,
		Status:         summary.HostStatus,
		Output:         string(payload),
		OutputEncoding: "utf-8",
		BytesSeen:      int64(len(payload)),
		BytesReturned:  int64(len(payload)),
	}
}

func (s *Server) inspectRepositoryEnvironment(ctx context.Context, inputPath string) (RepositoryEnvironmentSummary, string, error) {
	root, head, code, err := s.environmentRepositoryIdentity(ctx, inputPath)
	if err != nil {
		return RepositoryEnvironmentSummary{}, code, err
	}
	b := &environmentBuilder{
		server:     s,
		root:       root,
		head:       head,
		languages:  map[string]map[string]bool{},
		tools:      map[string]*EnvironmentTool{},
		mechanisms: map[string]*EnvironmentMechanism{},
	}
	if err := b.discover(ctx); err != nil {
		return RepositoryEnvironmentSummary{}, "environment_probe_failed", err
	}
	headAfter, headCode, headErr := s.environmentGit(ctx, root, "rev-parse", "--verify", "HEAD")
	if headErr != nil || headCode != 0 || strings.TrimSpace(headAfter) != head {
		return RepositoryEnvironmentSummary{}, "repository_changed", errors.New("repository HEAD changed during environment inspection")
	}
	return b.summary(), "", nil
}

func (s *Server) environmentRepositoryIdentity(ctx context.Context, inputPath string) (string, string, string, error) {
	if strings.TrimSpace(inputPath) == "" {
		return "", "", "invalid_repository_path", errors.New("repository path is empty")
	}
	workspace, err := filepath.Abs(s.cfg.WorkspaceDir)
	if err != nil {
		return "", "", "invalid_repository_path", err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", "", "invalid_repository_path", err
	}
	path := inputPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path, err = filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", "", "invalid_repository_path", err
	}
	if !pathWithin(workspace, path) {
		return "", "", "invalid_repository_path", errors.New("repository path is outside the configured workspace")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", "", "invalid_repository_path", errors.New("repository path is not a directory")
	}
	rootOut, code, err := s.environmentGit(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil || code != 0 {
		return "", "", "not_repository", errors.New("path is not a usable Git worktree")
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(rootOut))
	if err != nil || !pathWithin(workspace, root) {
		return "", "", "invalid_repository_path", errors.New("repository root resolves outside the configured workspace")
	}
	head, code, err := s.environmentGit(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil || code != 0 || !fullEnvironmentSHA(strings.TrimSpace(head)) {
		return "", "", "not_repository", errors.New("repository HEAD is unavailable")
	}
	return filepath.Clean(root), strings.TrimSpace(head), "", nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (s *Server) environmentGit(ctx context.Context, dir string, args ...string) (string, int, error) {
	gitPath := ""
	for _, candidate := range []string{"/usr/bin/git", "/usr/local/bin/git", "/bin/git"} {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil && trustedSystemExecutable(resolved) {
			gitPath = resolved
			break
		}
	}
	if gitPath == "" {
		return "", -1, errors.New("git executable is unavailable")
	}
	safeArgs := []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}
	safeArgs = append(safeArgs, args...)
	cmd := exec.CommandContext(ctx, gitPath, safeArgs...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=/nonexistent",
		"XDG_CONFIG_HOME=/nonexistent",
		"XDG_CACHE_HOME=/nonexistent",
		"PATH=" + safeCommandPath,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"AI_SERVER_AGENT=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_, err := terminateProcessGroup(cmd.Process.Pid)
		return err
	}
	cmd.WaitDelay = processGroupTerminateGrace
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}
	} else if uint32(os.Geteuid()) != s.workerUID {
		return "", -1, errors.New("executor cannot assume configured worker identity")
	}
	stdout := &environmentLimitedBuffer{limit: maxEnvironmentOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if errors.Is(err, errEnvironmentOutputLimit) {
		return "", -1, errors.New("git metadata output exceeded environment discovery limit")
	}
	if err == nil {
		return strings.TrimSpace(stdout.String()), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(stdout.String()), exitErr.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return "", -1, ctx.Err()
	}
	return "", -1, err
}

func (b *environmentBuilder) readDeclaration(ctx context.Context, rel, kind string) ([]byte, bool) {
	full := filepath.Join(b.root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		b.warnings = append(b.warnings, fmt.Sprintf("%s could not be inspected", rel))
		return nil, false
	}
	resolved, resolveErr := filepath.EvalSymlinks(full)
	if resolveErr != nil || !pathWithin(b.root, resolved) || filepath.Clean(resolved) != filepath.Clean(full) {
		b.warnings = append(b.warnings, fmt.Sprintf("%s uses symlink/path indirection and was ignored", rel))
		return nil, false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		b.warnings = append(b.warnings, fmt.Sprintf("%s is not a regular non-symlink declaration file and was ignored", rel))
		return nil, false
	}
	tracked, matchesHead := b.declarationDurability(ctx, rel)
	if info.Size() > maxEnvironmentFileBytes {
		b.declarations = append(b.declarations, EnvironmentDeclaration{Path: rel, Kind: kind, Tracked: tracked, MatchesHead: matchesHead})
		b.appendDeclarationDurabilityWarning(rel, tracked, matchesHead)
		b.warnings = append(b.warnings, fmt.Sprintf("%s exceeds the bounded parse size; presence was recorded but content was not parsed", rel))
		return nil, true
	}
	data, err := b.server.readEnvironmentFileAsWorker(ctx, full)
	if err != nil {
		b.warnings = append(b.warnings, fmt.Sprintf("%s could not be read with worker authority", rel))
		return nil, false
	}
	hash := sha256.Sum256(data)
	b.declarations = append(b.declarations, EnvironmentDeclaration{Path: rel, Kind: kind, Tracked: tracked, MatchesHead: matchesHead, SHA256: hex.EncodeToString(hash[:])})
	b.appendDeclarationDurabilityWarning(rel, tracked, matchesHead)
	return data, true
}

func (s *Server) readEnvironmentFileAsWorker(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/bin/cat", "--", path)
	cmd.Dir = s.cfg.WorkspaceDir
	cmd.Env = []string{"HOME=/nonexistent", "PATH=" + safeCommandPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "AI_SERVER_AGENT=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_, err := terminateProcessGroup(cmd.Process.Pid)
		return err
	}
	cmd.WaitDelay = processGroupTerminateGrace
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}
	} else if uint32(os.Geteuid()) != s.workerUID {
		return nil, errors.New("executor cannot assume configured worker identity")
	}
	output := &environmentLimitedBuffer{limit: maxEnvironmentFileBytes}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if errors.Is(err, errEnvironmentOutputLimit) {
			return nil, errEnvironmentOutputLimit
		}
		return nil, err
	}
	return []byte(output.String()), nil
}

func (b *environmentBuilder) declarationDurability(ctx context.Context, rel string) (tracked, matchesHead bool) {
	if _, code, err := b.server.environmentGit(ctx, b.root, "ls-files", "--error-unmatch", "--", rel); err != nil || code != 0 {
		return false, false
	}
	tracked = true
	if _, code, err := b.server.environmentGit(ctx, b.root, "diff", "--quiet", "HEAD", "--", rel); err == nil && code == 0 {
		matchesHead = true
	}
	return tracked, matchesHead
}

func (b *environmentBuilder) appendDeclarationDurabilityWarning(rel string, tracked, matchesHead bool) {
	switch {
	case !tracked:
		b.warnings = append(b.warnings, fmt.Sprintf("%s is not Git-tracked; it is visible working state but not durable repository truth", rel))
	case !matchesHead:
		b.warnings = append(b.warnings, fmt.Sprintf("%s is Git-tracked but differs from HEAD; current working state is not durable repository truth until committed", rel))
	}
}

func (b *environmentBuilder) addLanguage(name, declaredBy string) {
	if b.languages[name] == nil {
		b.languages[name] = map[string]bool{}
	}
	b.languages[name][declaredBy] = true
}

func (b *environmentBuilder) addTool(name, executable, role string, required bool, requirement *EnvironmentRequirement) {
	key := strings.ToLower(executable)
	tool := b.tools[key]
	if tool == nil {
		tool = &EnvironmentTool{Name: name, Executable: executable, Role: role, Required: required}
		b.tools[key] = tool
	}
	if required {
		tool.Required = true
	}
	if tool.Role == "" {
		tool.Role = role
	}
	if requirement != nil {
		for _, existing := range tool.Requirements {
			if existing.Value == requirement.Value && existing.Mode == requirement.Mode && existing.DeclaredBy == requirement.DeclaredBy {
				return
			}
		}
		tool.Requirements = append(tool.Requirements, *requirement)
	}
}

func repositoryRequirement(value, mode, declaredBy string) *EnvironmentRequirement {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &EnvironmentRequirement{Value: value, Mode: mode, DeclaredBy: declaredBy, Ownership: "repository"}
}

func (b *environmentBuilder) addMechanism(name, tool string, declaredBy ...string) {
	m := b.mechanisms[name]
	if m == nil {
		m = &EnvironmentMechanism{Name: name, Tool: tool}
		b.mechanisms[name] = m
	}
	seen := map[string]bool{}
	for _, existing := range m.DeclaredBy {
		seen[existing] = true
	}
	for _, file := range declaredBy {
		if !seen[file] {
			m.DeclaredBy = append(m.DeclaredBy, file)
		}
	}
	if tool != "" {
		b.addTool(tool, tool, "environment", false, nil)
	}
}

func (b *environmentBuilder) requireSelection(reason string) {
	for _, existing := range b.selectionReasons {
		if existing == reason {
			return
		}
	}
	b.selectionReasons = append(b.selectionReasons, reason)
}

func (b *environmentBuilder) addEntrypoint(name, kind, file, command string) {
	if len(b.entrypoints) >= maxEnvironmentEntrypoints {
		return
	}
	for _, existing := range b.entrypoints {
		if existing.Name == name && existing.Kind == kind && existing.File == file {
			return
		}
	}
	b.entrypoints = append(b.entrypoints, EnvironmentEntrypoint{Name: name, Kind: kind, File: file, Command: command})
}

type environmentExecutableCandidate struct {
	path        string
	source      string
	versionHint string
}

func (b *environmentBuilder) probeTools(ctx context.Context) {
	keys := make([]string, 0, len(b.tools))
	for key := range b.tools {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		tool := b.tools[key]
		candidates := b.server.environmentExecutableCandidates(b.root, tool.Executable)
		if len(candidates) == 0 {
			tool.Available = false
			tool.Compatibility = "missing"
			tool.Reason = "executable_not_found"
			continue
		}

		bestRank := -1
		for _, candidate := range candidates {
			version := candidate.versionHint
			compatibility := "compatible"
			reason := ""
			if candidate.source == "system_path_unverified" {
				compatibility = "unknown"
				reason = "system_executable_not_trusted_for_read_only_probe"
			} else if candidate.source == "system_path" {
				probed, err := b.server.environmentToolVersion(ctx, candidate.path, tool.Executable)
				if err != nil {
					compatibility = "unknown"
					reason = "version_probe_failed"
				} else {
					version = probed
				}
			}
			if compatibility == "compatible" {
				if candidate.source == "worker_cache" && version == "" && len(tool.Requirements) > 0 {
					compatibility = "unknown"
					reason = "worker_cache_version_not_verified"
				} else {
					compatibility, reason = evaluateEnvironmentRequirements(version, tool.Requirements)
				}
			}
			rank := environmentCompatibilityRank(compatibility)
			if rank <= bestRank {
				continue
			}
			bestRank = rank
			tool.Available = true
			tool.Path = candidate.path
			tool.Source = candidate.source
			tool.Version = version
			tool.Compatibility = compatibility
			tool.Reason = reason
			if rank == 3 {
				break
			}
		}
	}
	for _, mechanism := range b.mechanisms {
		if mechanism.Tool == "" {
			mechanism.Available = true
			continue
		}
		if tool := b.tools[strings.ToLower(mechanism.Tool)]; tool != nil {
			mechanism.Available = tool.Available
			mechanism.Version = tool.Version
			if !tool.Available {
				mechanism.Notes = "declared mechanism is not currently available on the host; no automatic installation was attempted"
			}
		}
	}
}

func environmentCompatibilityRank(compatibility string) int {
	switch compatibility {
	case "compatible":
		return 3
	case "unknown":
		return 2
	case "incompatible", "conflict":
		return 1
	default:
		return 0
	}
}

func (s *Server) environmentExecutableCandidates(repositoryRoot, name string) []environmentExecutableCandidate {
	var candidates []environmentExecutableCandidate
	seen := map[string]bool{}
	add := func(path, source, hint string) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || pathWithin(repositoryRoot, resolved) || !executableFile(resolved) || seen[resolved] {
			return
		}
		if source == "system_path" && !trustedSystemExecutable(resolved) {
			source = "system_path_unverified"
		}
		seen[resolved] = true
		candidates = append(candidates, environmentExecutableCandidate{path: resolved, source: source, versionHint: hint})
	}
	for _, dir := range strings.Split(safeCommandPath, ":") {
		add(filepath.Join(dir, name), "system_path", "")
	}
	workspace, err := filepath.EvalSymlinks(s.cfg.WorkspaceDir)
	if err != nil {
		return candidates
	}
	add(filepath.Join(workspace, ".local", "bin", name), "worker_cache", "")
	add(filepath.Join(workspace, ".cargo", "bin", name), "worker_cache", "")
	add(filepath.Join(workspace, "go", "bin", name), "worker_cache", "")
	matches, _ := filepath.Glob(filepath.Join(workspace, ".toolchains", "*", "bin", name))
	sort.Strings(matches)
	for _, match := range matches {
		add(match, "worker_cache", environmentCacheVersionHint(workspace, match))
	}
	return candidates
}

func environmentCacheVersionHint(workspace, candidate string) string {
	toolchains := filepath.Join(workspace, ".toolchains")
	rel, err := filepath.Rel(toolchains, candidate)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 || parts[1] != "bin" {
		return ""
	}
	version, ok := parseEnvironmentVersion(parts[0])
	if !ok {
		return ""
	}
	values := []string{fmt.Sprintf("%d", version.parts[0])}
	if version.count >= 2 {
		values = append(values, fmt.Sprintf("%d", version.parts[1]))
	}
	if version.count >= 3 {
		values = append(values, fmt.Sprintf("%d", version.parts[2]))
	}
	return strings.Join(values, ".")
}

func trustedSystemExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

type environmentLimitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

var errEnvironmentOutputLimit = errors.New("environment probe output exceeded bound")

func (b *environmentLimitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > b.limit-b.buf.Len() {
		return 0, errEnvironmentOutputLimit
	}
	return b.buf.Write(p)
}

func (b *environmentLimitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (s *Server) environmentToolVersion(parent context.Context, path, executable string) (string, error) {
	args := environmentVersionArgs(executable)
	if args == nil {
		return "", errors.New("no bounded version probe is defined")
	}
	ctx, cancel := context.WithTimeout(parent, environmentProbeTimeout)
	defer cancel()
	probeHome, err := os.MkdirTemp("", "ai-server-agent-env-probe-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(probeHome)
	if err := os.Chown(probeHome, int(s.workerUID), int(s.workerGID)); err != nil {
		return "", err
	}
	if err := os.Chmod(probeHome, 0700); err != nil {
		return "", err
	}
	cacheDir := filepath.Join(probeHome, ".cache")
	configDir := filepath.Join(probeHome, ".config")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return "", err
	}
	if err := os.Chown(cacheDir, int(s.workerUID), int(s.workerGID)); err != nil {
		return "", err
	}
	if err := os.Chown(configDir, int(s.workerUID), int(s.workerGID)); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = probeHome
	cmd.Env = []string{
		"HOME=" + probeHome,
		"XDG_CACHE_HOME=" + cacheDir,
		"XDG_CONFIG_HOME=" + configDir,
		"PATH=" + safeCommandPath,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"AI_SERVER_AGENT=1",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_, err := terminateProcessGroup(cmd.Process.Pid)
		return err
	}
	cmd.WaitDelay = processGroupTerminateGrace
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}
	} else if uint32(os.Geteuid()) != s.workerUID {
		return "", errors.New("executor cannot assume configured worker identity")
	}
	output := &environmentLimitedBuffer{limit: 64 << 10}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		if errors.Is(err, errEnvironmentOutputLimit) {
			return "", errEnvironmentOutputLimit
		}
		return "", err
	}
	line := strings.TrimSpace(output.String())
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	return strings.TrimSpace(line), nil
}

func environmentVersionArgs(executable string) []string {
	switch executable {
	case "go":
		return []string{"version"}
	case "node", "npm", "pnpm", "yarn", "bun", "python3", "python", "ruby", "bundle", "cargo", "rustc", "php", "composer", "dotnet", "java", "mvn", "gradle", "cmake", "make", "mise", "devenv", "devcontainer", "task", "just":
		return []string{"--version"}
	case "dagger":
		return []string{"version"}
	default:
		return nil
	}
}

func (b *environmentBuilder) summary() RepositoryEnvironmentSummary {
	languages := make([]EnvironmentLanguage, 0, len(b.languages))
	for name, files := range b.languages {
		declaredBy := make([]string, 0, len(files))
		for file := range files {
			declaredBy = append(declaredBy, file)
		}
		sort.Strings(declaredBy)
		languages = append(languages, EnvironmentLanguage{Name: name, DeclaredBy: declaredBy})
	}
	sort.Slice(languages, func(i, j int) bool { return languages[i].Name < languages[j].Name })

	tools := make([]EnvironmentTool, 0, len(b.tools))
	hostStatus := "ready"
	for _, tool := range b.tools {
		sort.Slice(tool.Requirements, func(i, j int) bool {
			if tool.Requirements[i].DeclaredBy == tool.Requirements[j].DeclaredBy {
				return tool.Requirements[i].Value < tool.Requirements[j].Value
			}
			return tool.Requirements[i].DeclaredBy < tool.Requirements[j].DeclaredBy
		})
		tools = append(tools, *tool)
		if tool.Compatibility == "conflict" {
			b.requireSelection("conflicting repository version requirements exist for " + tool.Executable)
		}
		if tool.Required {
			switch tool.Compatibility {
			case "missing", "incompatible", "conflict":
				hostStatus = "unsatisfied"
			case "unknown":
				if hostStatus == "ready" {
					hostStatus = "unknown"
				}
			}
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Executable < tools[j].Executable })

	mechanisms := make([]EnvironmentMechanism, 0, len(b.mechanisms))
	var isolation []string
	for _, mechanism := range b.mechanisms {
		sort.Strings(mechanism.DeclaredBy)
		mechanisms = append(mechanisms, *mechanism)
		if mechanism.Tool != "" {
			if tool := b.tools[strings.ToLower(mechanism.Tool)]; tool == nil || !tool.Available || tool.Compatibility == "unknown" || tool.Compatibility == "missing" {
				if hostStatus == "ready" {
					hostStatus = "unknown"
				}
			}
		}
		if mechanism.Name == "devenv" || mechanism.Name == "devcontainer" || mechanism.Name == "dagger" {
			isolation = append(isolation, mechanism.Name)
		}
	}
	sort.Slice(mechanisms, func(i, j int) bool { return mechanisms[i].Name < mechanisms[j].Name })
	sort.Strings(isolation)

	if len(mechanisms) > 1 {
		b.requireSelection("multiple repository environment mechanisms are declared; no global precedence was inferred")
	}
	sort.Strings(b.selectionReasons)
	selectionRequired := len(b.selectionReasons) > 0
	selectionReason := strings.Join(b.selectionReasons, "; ")
	if selectionRequired && hostStatus == "ready" {
		hostStatus = "unknown"
	}
	if len(b.declarations) == 0 {
		hostStatus = "unknown"
		b.warnings = append(b.warnings, "no recognized repository environment/toolchain declarations were found")
	}
	isolationRequirement := "not_declared"
	if len(isolation) > 0 {
		isolationRequirement = "unknown_from_declarations"
	}
	sort.Slice(b.declarations, func(i, j int) bool { return b.declarations[i].Path < b.declarations[j].Path })
	sort.Slice(b.entrypoints, func(i, j int) bool {
		if b.entrypoints[i].File == b.entrypoints[j].File {
			return b.entrypoints[i].Name < b.entrypoints[j].Name
		}
		return b.entrypoints[i].File < b.entrypoints[j].File
	})
	sort.Strings(b.warnings)

	return RepositoryEnvironmentSummary{
		SchemaVersion:        1,
		RepositoryRoot:       b.root,
		RepositoryHead:       b.head,
		Declarations:         b.declarations,
		Languages:            languages,
		Mechanisms:           mechanisms,
		Tools:                tools,
		Entrypoints:          b.entrypoints,
		HostStatus:           hostStatus,
		SelectionRequired:    selectionRequired,
		SelectionReason:      selectionReason,
		IsolationDeclared:    len(isolation) > 0,
		IsolationRequirement: isolationRequirement,
		IsolationMechanisms:  isolation,
		Warnings:             b.warnings,
	}
}
