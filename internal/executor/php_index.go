package executor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

// Rebuild a private Phpactor index for every cross-file navigation request:
// no persistent freshness marker can accidentally bless source that changed,
// even across Executor restart or a failed previous build. The existing
// broker owns the resource-bounded worker subprocess and all descendants.
// The caller holds phpIndexMu through the LSP query so no concurrent reset
// can invalidate its active index.
func (s *Server) buildPHPIndex(parent context.Context, req Request, workspace, php, phar, cache string) (fingerprint string, err error) {
	scan := req
	scan.Action = "workspace_php_index_manifest"
	scan.Workspace = workspace
	scan.Path, scan.FileVersion = "", ""
	scan.Limit, scan.Offset = 0, 0
	first := s.workerWorkspaceFile(parent, scan)
	if !first.OK || first.FileVersion == "" {
		return "", fmt.Errorf("php_lsp_index_manifest_before: %s (%s)", first.ErrorCode, first.Error)
	}
	prlimit := systemexec.First("/usr/bin/prlimit", "/bin/prlimit")
	if prlimit == "" {
		return "", errors.New("php_lsp_trusted_resource_limiter_unavailable")
	}
	profile := phpLSPProfile(phar, filepath.Join(cache, "store"))
	args := append([]string{"--cpu=15", "--as=805306368", "--nofile=128", "--", php}, profile.arguments...)
	replaced := false
	for i := range args {
		if args[i] == "language-server" {
			args[i] = "index:build"
			replaced = true
			break
		}
	}
	if !replaced {
		return "", errors.New("php_lsp_index_profile_invalid")
	}
	args = append(args, "--reset")
	id, launchErr := s.workerStdioSession(req, "lsp", workspace, prlimit, args...)
	if launchErr != nil {
		if id != "" {
			if closeErr := s.workerStdioClose(req, id); closeErr != nil {
				return "", errors.Join(launchErr, fmt.Errorf("php_lsp_index_stop_unknown: %w", closeErr))
			}
		}
		return "", fmt.Errorf("php_lsp_index_start_failed: %w", launchErr)
	}
	defer func() {
		if closeErr := s.workerStdioClose(req, id); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("php_lsp_index_stop_unknown: %w", closeErr))
			fingerprint = ""
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 18*time.Second)
	defer cancel()
	timer := time.NewTicker(30 * time.Millisecond)
	defer timer.Stop()
	for {
		state, readErr := s.sessions.read(req, id, 0, maxStdioEventBytes)
		if readErr != nil {
			return "", fmt.Errorf("php_lsp_index_session_unavailable: %w", readErr)
		}
		if !state.Running {
			if state.ExitCode != 0 {
				return "", fmt.Errorf("php_lsp_index_build_failed_exit_%d", state.ExitCode)
			}
			break
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("php_lsp_index_deadline: %w", ctx.Err())
		case <-timer.C:
		}
	}
	last := s.workerWorkspaceFile(parent, scan)
	if !last.OK || !strings.HasPrefix(last.FileVersion, "php-index-sha256:") ||
		last.FileVersion != first.FileVersion {
		return "", errors.New("php_lsp_index_source_changed")
	}
	return first.FileVersion, nil
}
