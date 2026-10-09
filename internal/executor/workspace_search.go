package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	defaultWorkspaceSearchLimit   = 100
	maxWorkspaceSearchLimit       = 1000
	defaultWorkspaceSearchTimeout = 30 * time.Second
	maxWorkspaceSearchTimeout     = 2 * time.Minute
	maxWorkspaceSearchPattern     = 64 << 10
	maxWorkspaceSearchGlobs       = 32
	maxWorkspaceSearchGlobBytes   = 512
	maxWorkspaceSearchPaths       = 32
	maxWorkspaceSearchPathBytes   = 1024
	maxWorkspaceSearchLineBytes   = 512 << 10
	maxWorkspaceSearchResultBytes = 1 << 20
	maxWorkspaceSearchTextBytes   = 8 << 10
	maxWorkspaceCaptureTextBytes  = 2 << 10
	maxWorkspaceCapturesPerMatch  = 64
	maxWorkspaceSearchStderrBytes = 64 << 10
)

var workspaceSearchLanguageRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]{0,31}$`)
var astGrepVersionRE = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+].*)?$`)

const minimumAstGrepVersion = "0.40.5"

type SearchPosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type SearchByteOffset struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type SearchRange struct {
	ByteOffset SearchByteOffset `json:"byte_offset"`
	Start      SearchPosition   `json:"start"`
	End        SearchPosition   `json:"end"`
}

type WorkspaceSearchCapture struct {
	Name          string      `json:"name"`
	Kind          string      `json:"kind"`
	Text          string      `json:"text"`
	TextTruncated bool        `json:"text_truncated,omitempty"`
	Range         SearchRange `json:"range"`
}

type WorkspaceSearchMatch struct {
	File              string                   `json:"file"`
	Language          string                   `json:"language"`
	Range             SearchRange              `json:"range"`
	Text              string                   `json:"text"`
	TextTruncated     bool                     `json:"text_truncated,omitempty"`
	Captures          []WorkspaceSearchCapture `json:"captures,omitempty"`
	CapturesTruncated bool                     `json:"captures_truncated,omitempty"`
}

type WorkspaceSearchResult struct {
	Mode             string                 `json:"mode"`
	Workspace        string                 `json:"workspace"`
	Engine           string                 `json:"engine"`
	EngineVersion    string                 `json:"engine_version"`
	Matches          []WorkspaceSearchMatch `json:"matches"`
	Limit            int                    `json:"limit"`
	Complete         bool                   `json:"complete"`
	Truncated        bool                   `json:"truncated"`
	TruncationReason string                 `json:"truncation_reason,omitempty"`
	TimedOut         bool                   `json:"timed_out,omitempty"`
	DurationMS       int64                  `json:"duration_ms"`
}

type astGrepPosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type astGrepByteOffset struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type astGrepRange struct {
	ByteOffset astGrepByteOffset `json:"byteOffset"`
	Start      astGrepPosition   `json:"start"`
	End        astGrepPosition   `json:"end"`
}

type astGrepCapture struct {
	Text  string       `json:"text"`
	Range astGrepRange `json:"range"`
}

type astGrepMetaVariables struct {
	Single      map[string]astGrepCapture   `json:"single"`
	Multi       map[string][]astGrepCapture `json:"multi"`
	Transformed map[string]string           `json:"transformed"`
}

type astGrepMatch struct {
	Text          string               `json:"text"`
	Range         astGrepRange         `json:"range"`
	File          string               `json:"file"`
	Language      string               `json:"language"`
	MetaVariables astGrepMetaVariables `json:"metaVariables"`
}

type workspaceSearchRequest struct {
	workspace string
	paths     []string
	globs     []string
	language  string
	pattern   string
	limit     int
	timeout   time.Duration
}

func workspaceSearchError(code, class, message string, retryable bool) Response {
	return Response{Error: message, ReasonCode: code, ErrorCode: code, ErrorClass: class, Retryable: retryable}
}

func (s *Server) workspaceSearchContext(parent context.Context, req Request) Response {
	started := time.Now()
	searchReq, code, err := s.validateWorkspaceSearchRequest(req)
	if err != nil {
		resp := workspaceSearchError(code, "validation", err.Error(), false)
		resp.DurationMS = time.Since(started).Milliseconds()
		return resp
	}

	binary, code, err := s.resolveAstGrepBinary()
	if err != nil {
		resp := workspaceSearchError(code, "dependency", err.Error(), false)
		resp.DurationMS = time.Since(started).Milliseconds()
		return resp
	}

	release, ok := s.runs.acquire(false)
	if !ok {
		resp := runCapacityResponse(false)
		resp.DurationMS = time.Since(started).Milliseconds()
		return resp
	}
	defer release()

	if os.Geteuid() != 0 && (uint32(os.Geteuid()) != s.workerUID || uint32(os.Getegid()) != s.workerGID) {
		resp := workspaceSearchError("search_worker_identity_unavailable", "security", "workspace search cannot execute as the configured worker identity", false)
		resp.DurationMS = time.Since(started).Milliseconds()
		return resp
	}

	version, err := s.astGrepVersion(parent, binary)
	if err != nil {
		resp := workspaceSearchError("search_engine_unavailable", "dependency", "ast-grep version probe failed: "+err.Error(), true)
		resp.DurationMS = time.Since(started).Milliseconds()
		return resp
	}
	if err := requireCompatibleAstGrepVersion(version); err != nil {
		resp := workspaceSearchError("search_engine_incompatible", "dependency", err.Error(), false)
		resp.DurationMS = time.Since(started).Milliseconds()
		return resp
	}

	ctx, cancel := context.WithTimeout(parent, searchReq.timeout)
	defer cancel()
	result, code, err := s.runAstGrepSearch(ctx, cancel, binary, version, searchReq)
	result.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		resp := workspaceSearchError(code, "process", err.Error(), false)
		resp.DurationMS = result.DurationMS
		return resp
	}
	payload, err := json.Marshal(result)
	if err != nil {
		resp := workspaceSearchError("search_encode_failed", "internal", "workspace search result could not be encoded", false)
		resp.DurationMS = result.DurationMS
		return resp
	}
	return Response{
		OK:             true,
		Status:         workspaceSearchStatus(result),
		Output:         string(payload),
		OutputEncoding: "utf-8",
		BytesSeen:      int64(len(payload)),
		BytesReturned:  int64(len(payload)),
		DurationMS:     result.DurationMS,
		TimedOut:       result.TimedOut,
	}
}

func workspaceSearchStatus(result WorkspaceSearchResult) string {
	if result.Complete {
		return "complete"
	}
	return "partial"
}

func (s *Server) validateWorkspaceSearchRequest(req Request) (workspaceSearchRequest, string, error) {
	if req.SearchMode != "structural" {
		return workspaceSearchRequest{}, "invalid_search_mode", errors.New("search_mode must be structural; use rg/git grep for text search and LSP for semantic meaning")
	}
	if req.Pattern == "" {
		return workspaceSearchRequest{}, "invalid_pattern", errors.New("pattern is required")
	}
	if len(req.Pattern) > maxWorkspaceSearchPattern || strings.IndexByte(req.Pattern, 0) >= 0 {
		return workspaceSearchRequest{}, "invalid_pattern", fmt.Errorf("pattern exceeds the %d-byte limit or contains NUL", maxWorkspaceSearchPattern)
	}
	if !workspaceSearchLanguageRE.MatchString(req.Language) {
		return workspaceSearchRequest{}, "invalid_language", errors.New("language is required and must use a simple ast-grep language identifier")
	}

	limit := req.SearchLimit
	if limit == 0 {
		limit = defaultWorkspaceSearchLimit
	}
	if limit < 1 || limit > maxWorkspaceSearchLimit {
		return workspaceSearchRequest{}, "invalid_search_limit", fmt.Errorf("search_limit must be between 1 and %d", maxWorkspaceSearchLimit)
	}

	timeout := defaultWorkspaceSearchTimeout
	if req.TimeoutMS < 0 {
		return workspaceSearchRequest{}, "invalid_timeout", errors.New("timeout_ms must not be negative")
	}
	if req.TimeoutMS > 0 {
		if req.TimeoutMS > int64(maxWorkspaceSearchTimeout/time.Millisecond) {
			return workspaceSearchRequest{}, "invalid_timeout", fmt.Errorf("timeout_ms exceeds maximum of %d", maxWorkspaceSearchTimeout/time.Millisecond)
		}
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	workspace, err := s.resolveWorkspaceSearchRoot(req.Workspace)
	if err != nil {
		return workspaceSearchRequest{}, "invalid_workspace", err
	}
	paths, err := resolveWorkspaceSearchPaths(workspace, req.SearchPaths)
	if err != nil {
		return workspaceSearchRequest{}, "invalid_search_path", err
	}
	if len(req.Globs) > maxWorkspaceSearchGlobs {
		return workspaceSearchRequest{}, "invalid_glob", fmt.Errorf("at most %d globs are allowed", maxWorkspaceSearchGlobs)
	}
	globs := make([]string, 0, len(req.Globs))
	for _, glob := range req.Globs {
		if glob == "" || len(glob) > maxWorkspaceSearchGlobBytes || strings.IndexByte(glob, 0) >= 0 || strings.ContainsAny(glob, "\r\n") {
			return workspaceSearchRequest{}, "invalid_glob", fmt.Errorf("each glob must be non-empty, single-line, NUL-free and at most %d bytes", maxWorkspaceSearchGlobBytes)
		}
		globs = append(globs, glob)
	}

	return workspaceSearchRequest{
		workspace: workspace,
		paths:     paths,
		globs:     globs,
		language:  req.Language,
		pattern:   req.Pattern,
		limit:     limit,
		timeout:   timeout,
	}, "", nil
}

func (s *Server) resolveWorkspaceSearchRoot(input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", errors.New("workspace is required")
	}
	root, err := filepath.EvalSymlinks(s.cfg.WorkspaceDir)
	if err != nil {
		return "", fmt.Errorf("configured workspace is unavailable: %w", err)
	}
	candidate := input
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.EvalSymlinks(filepath.Clean(candidate))
	if err != nil {
		return "", fmt.Errorf("workspace cannot be resolved: %w", err)
	}
	if !workspaceSearchPathWithin(root, candidate) {
		return "", errors.New("workspace resolves outside the configured workspace root")
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return "", errors.New("workspace must resolve to an existing directory")
	}
	return candidate, nil
}

func workspaceSearchPathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func resolveWorkspaceSearchPaths(workspace string, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return []string{workspace}, nil
	}
	if len(requested) > maxWorkspaceSearchPaths {
		return nil, fmt.Errorf("at most %d search paths are allowed", maxWorkspaceSearchPaths)
	}
	paths := make([]string, 0, len(requested))
	seen := map[string]bool{}
	for _, input := range requested {
		if input == "" || len(input) > maxWorkspaceSearchPathBytes || strings.IndexByte(input, 0) >= 0 || filepath.IsAbs(input) {
			return nil, fmt.Errorf("search paths must be non-empty relative paths of at most %d bytes", maxWorkspaceSearchPathBytes)
		}
		clean := filepath.Clean(input)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, errors.New("search path escapes the workspace")
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(workspace, clean))
		if err != nil {
			return nil, fmt.Errorf("search path %q cannot be resolved: %w", input, err)
		}
		if !workspaceSearchPathWithin(workspace, resolved) {
			return nil, fmt.Errorf("search path %q resolves outside the workspace", input)
		}
		info, err := os.Stat(resolved)
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			return nil, fmt.Errorf("search path %q must resolve to a regular file or directory", input)
		}
		if !seen[resolved] {
			seen[resolved] = true
			paths = append(paths, resolved)
		}
	}
	return paths, nil
}

func (s *Server) resolveAstGrepBinary() (string, string, error) {
	if s.structuralSearchBinary != "" {
		if info, err := os.Stat(s.structuralSearchBinary); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return s.structuralSearchBinary, "", nil
		}
		return "", "search_engine_unavailable", errors.New("configured structural-search test engine is unavailable")
	}
	candidates := []string{
		"/opt/ai-server-agent/tools/ast-grep/ast-grep",
		"/usr/local/bin/ast-grep",
		"/usr/bin/ast-grep",
		"/bin/ast-grep",
	}
	for _, candidate := range candidates {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if trustedWorkspaceSearchExecutable(resolved) {
			return resolved, "", nil
		}
	}
	return "", "search_engine_unavailable", errors.New("trusted ast-grep executable not found; install/reuse ast-grep explicitly, and do not use Linux /usr/bin/sg as a substitute")
}

func trustedWorkspaceSearchExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return false
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return false
		}
		if dir == string(filepath.Separator) {
			break
		}
	}
	return true
}

func (s *Server) astGrepVersion(ctx context.Context, binary string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := s.workspaceSearchCommand(probeCtx, binary, "--version")
	out := newBoundedOutputCollector(4 << 10)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	result := out.Result()
	if result.Truncated || result.Encoding != "utf-8" {
		return "", errors.New("ast-grep version output is invalid")
	}
	version := strings.TrimSpace(result.Output)
	if !strings.HasPrefix(version, "ast-grep ") || len(version) > 64 {
		return "", errors.New("unexpected ast-grep version output")
	}
	return strings.TrimPrefix(version, "ast-grep "), nil
}

func requireCompatibleAstGrepVersion(version string) error {
	got, ok := parseAstGrepVersion(version)
	if !ok {
		return fmt.Errorf("unrecognized ast-grep version %q", version)
	}
	minimum, _ := parseAstGrepVersion(minimumAstGrepVersion)
	for i := 0; i < len(got); i++ {
		if got[i] > minimum[i] {
			return nil
		}
		if got[i] < minimum[i] {
			return fmt.Errorf("ast-grep %s is older than the supported minimum %s", version, minimumAstGrepVersion)
		}
	}
	return nil
}

func parseAstGrepVersion(version string) ([3]int, bool) {
	match := astGrepVersionRE.FindStringSubmatch(strings.TrimSpace(version))
	if len(match) != 4 {
		return [3]int{}, false
	}
	var parsed [3]int
	for i := range parsed {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return [3]int{}, false
		}
		parsed[i] = value
	}
	return parsed, true
}

func (s *Server) workspaceSearchCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = "/"
	cmd.Env = []string{
		"HOME=/nonexistent",
		"XDG_CONFIG_HOME=/nonexistent",
		"XDG_CACHE_HOME=/nonexistent",
		"PATH=" + safeCommandPath,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"NO_COLOR=1",
		"AI_SERVER_AGENT=1",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: s.workerUID, Gid: s.workerGID, Groups: []uint32{s.workerGID}}
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_, err := terminateProcessGroup(cmd.Process.Pid)
		return err
	}
	cmd.WaitDelay = processGroupTerminateGrace
	return cmd
}

func (s *Server) runAstGrepSearch(ctx context.Context, cancel context.CancelFunc, binary, version string, req workspaceSearchRequest) (WorkspaceSearchResult, string, error) {
	result := WorkspaceSearchResult{
		Mode:          "structural",
		Workspace:     req.workspace,
		Engine:        "ast-grep",
		EngineVersion: version,
		Limit:         req.limit,
		Complete:      true,
	}
	args := []string{"run", "--config", "/dev/null", "--lang", req.language, "--pattern", req.pattern, "--json=stream", "--color", "never", "--threads", "1"}
	for _, glob := range req.globs {
		args = append(args, "--globs", glob)
	}
	args = append(args, req.paths...)

	cmd := s.workspaceSearchCommand(ctx, binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, "search_start_failed", err
	}
	stderr := newBoundedOutputCollector(maxWorkspaceSearchStderrBytes)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return result, "search_start_failed", err
	}

	stopReason := ""
	resultBytes := 0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxWorkspaceSearchLineBytes)
	for scanner.Scan() {
		if len(result.Matches) >= req.limit {
			stopReason = "result_limit"
			cancel()
			break
		}
		line := append([]byte(nil), scanner.Bytes()...)
		var raw astGrepMatch
		if err := json.Unmarshal(line, &raw); err != nil {
			stopReason = "invalid_engine_output"
			cancel()
			break
		}
		match, err := normalizeAstGrepMatch(req.workspace, raw)
		if err != nil {
			stopReason = "invalid_engine_output"
			cancel()
			break
		}
		encoded, err := json.Marshal(match)
		if err != nil {
			stopReason = "invalid_engine_output"
			cancel()
			break
		}
		if resultBytes+len(encoded) > maxWorkspaceSearchResultBytes {
			stopReason = "output_limit"
			cancel()
			break
		}
		result.Matches = append(result.Matches, match)
		resultBytes += len(encoded)
	}
	scanErr := scanner.Err()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		stopReason = "timeout"
	} else if scanErr != nil && stopReason == "" {
		stopReason = "match_too_large"
		cancel()
	}

	waitErr := cmd.Wait()
	stderrResult := stderr.Result()
	if stopReason == "invalid_engine_output" {
		return result, "search_output_invalid", errors.New("ast-grep returned invalid structured output")
	}
	if stopReason != "" && stopReason != "timeout" {
		result.Complete = false
		result.Truncated = true
		result.TruncationReason = stopReason
		return result, "", nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Complete = false
		result.Truncated = true
		result.TruncationReason = "timeout"
		result.TimedOut = true
		return result, "", nil
	}
	if ctx.Err() != nil {
		return result, "search_cancelled", ctx.Err()
	}
	if waitErr == nil {
		return result, "", nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 && len(result.Matches) == 0 {
		return result, "", nil
	}
	message := "ast-grep search failed"
	if stderrResult.Encoding == "utf-8" && strings.TrimSpace(stderrResult.Output) != "" {
		message += ": " + strings.TrimSpace(stderrResult.Output)
	}
	return result, "search_failed", errors.New(message)
}

func normalizeAstGrepMatch(workspace string, raw astGrepMatch) (WorkspaceSearchMatch, error) {
	if !validAstGrepRange(raw.Range) {
		return WorkspaceSearchMatch{}, errors.New("ast-grep returned an invalid match range")
	}
	file := filepath.Clean(raw.File)
	if !filepath.IsAbs(file) || !workspaceSearchPathWithin(workspace, file) {
		return WorkspaceSearchMatch{}, errors.New("ast-grep returned a file outside the requested workspace")
	}
	rel, err := filepath.Rel(workspace, file)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return WorkspaceSearchMatch{}, errors.New("ast-grep returned an invalid workspace-relative file")
	}
	text, textTruncated := truncateUTF8SearchText(raw.Text, maxWorkspaceSearchTextBytes)
	match := WorkspaceSearchMatch{
		File:          filepath.ToSlash(rel),
		Language:      raw.Language,
		Range:         convertAstGrepRange(raw.Range),
		Text:          text,
		TextTruncated: textTruncated,
	}

	names := make([]string, 0, len(raw.MetaVariables.Single)+len(raw.MetaVariables.Multi))
	for name := range raw.MetaVariables.Single {
		names = append(names, name)
	}
	for name := range raw.MetaVariables.Multi {
		if _, exists := raw.MetaVariables.Single[name]; !exists {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if capture, ok := raw.MetaVariables.Single[name]; ok {
			if !validAstGrepRange(capture.Range) {
				return WorkspaceSearchMatch{}, errors.New("ast-grep returned an invalid capture range")
			}
			if len(match.Captures) >= maxWorkspaceCapturesPerMatch {
				match.CapturesTruncated = true
				break
			}
			match.Captures = append(match.Captures, normalizeAstGrepCapture(name, "single", capture))
		}
		for _, capture := range raw.MetaVariables.Multi[name] {
			if !validAstGrepRange(capture.Range) {
				return WorkspaceSearchMatch{}, errors.New("ast-grep returned an invalid capture range")
			}
			if len(match.Captures) >= maxWorkspaceCapturesPerMatch {
				match.CapturesTruncated = true
				break
			}
			match.Captures = append(match.Captures, normalizeAstGrepCapture(name, "multi", capture))
		}
		if match.CapturesTruncated {
			break
		}
	}
	return match, nil
}

func validAstGrepRange(r astGrepRange) bool {
	if r.ByteOffset.Start < 0 || r.ByteOffset.End < r.ByteOffset.Start {
		return false
	}
	if r.Start.Line < 0 || r.Start.Column < 0 || r.End.Line < 0 || r.End.Column < 0 {
		return false
	}
	if r.End.Line < r.Start.Line || (r.End.Line == r.Start.Line && r.End.Column < r.Start.Column) {
		return false
	}
	return true
}

func normalizeAstGrepCapture(name, kind string, raw astGrepCapture) WorkspaceSearchCapture {
	text, truncated := truncateUTF8SearchText(raw.Text, maxWorkspaceCaptureTextBytes)
	return WorkspaceSearchCapture{
		Name:          name,
		Kind:          kind,
		Text:          text,
		TextTruncated: truncated,
		Range:         convertAstGrepRange(raw.Range),
	}
}

func convertAstGrepRange(raw astGrepRange) SearchRange {
	return SearchRange{
		ByteOffset: SearchByteOffset{Start: raw.ByteOffset.Start, End: raw.ByteOffset.End},
		Start:      SearchPosition{Line: raw.Start.Line, Column: raw.Start.Column},
		End:        SearchPosition{Line: raw.End.Line, Column: raw.End.Column},
	}
}

func truncateUTF8SearchText(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut], true
}
