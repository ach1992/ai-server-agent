package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

// Phpactor 2026.10.07.0 writes its "dirty" file into indexer.index_path
// while answering references. In a fresh worker HOME that directory does not
// exist. Build the private directory using the already-authorized worker-only
// stdio broker; never mkdir inside the source workspace and never create a
// directory as root in a worker-controlled tree.
func (s *Server) phpCacheDir(ctx context.Context, req Request, workspace string) (string, error) {
	home, err := s.workerSessionHome()
	if err != nil {
		return "", fmt.Errorf("php_lsp_worker_home_unavailable: %w", err)
	}
	identity := sha256.Sum256([]byte(req.PrincipalClass + "\x00" + req.PrincipalID + "\x00" + workspace))
	dir := filepath.Join(home, ".cache", "phpactor", "index", "agent-"+hex.EncodeToString(identity[:12]))
	if phpPrivateCacheReady(home, dir, s.workerUID) {
		return dir, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mkdir := systemexec.First("/usr/bin/mkdir", "/bin/mkdir")
	if mkdir == "" {
		return "", errors.New("php_lsp_trusted_mkdir_unavailable")
	}
	id, err := s.workerStdioSession(req, "lsp", workspace, mkdir, "-p", "-m", "0700", "--", dir)
	if err != nil {
		return "", fmt.Errorf("php_lsp_cache_worker_create_failed: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = s.workerStdioClose(req, id)
		}
	}()
	limit := time.NewTimer(3 * time.Second)
	defer limit.Stop()
	for {
		state, err := s.sessions.read(req, id, 0, maxStdioEventBytes)
		if err != nil {
			return "", fmt.Errorf("php_lsp_cache_worker_status_failed: %w", err)
		}
		if !state.Running {
			if state.ExitCode != 0 {
				return "", fmt.Errorf("php_lsp_cache_worker_exit_%d", state.ExitCode)
			}
			if err := s.workerStdioClose(req, id); err != nil {
				return "", fmt.Errorf("php_lsp_cache_worker_stop_unverified: %w", err)
			}
			closed = true
			if !phpPrivateCacheReady(home, dir, s.workerUID) {
				return "", errors.New("php_lsp_cache_path_untrusted_or_missing")
			}
			return dir, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-limit.C:
			return "", errors.New("php_lsp_cache_worker_timeout")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func phpPrivateCacheReady(home, dir string, uid uint32) bool {
	if !filepath.IsAbs(home) || !strings.HasPrefix(dir, home+string(filepath.Separator)) {
		return false
	}
	relative, err := filepath.Rel(home, dir)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		return false
	}
	current := home
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return false
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0022 != 0 {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uid {
			return false
		}
	}
	info, err := os.Lstat(dir)
	return err == nil && info.Mode().Perm() == 0700
}
