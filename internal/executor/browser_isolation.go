package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The isolated Browser account owns only disposable Browser/session state.
// aiworker and aiagent are deliberately NOT members of its private group.
// Root controls each parent directory so neither account can replace the
// private namespace root. A valid Browser UID is necessary but not sufficient.
func (s *Server) isolatedBrowserScratchDir() string {
	return filepath.Join(s.cfg.StateDir, "runtime", "browser", "isolated")
}

func (s *Server) isolatedBrowserHome() (string, error) {
	if os.Geteuid() != 0 || s.browserUID == 0 || s.browserGID == 0 ||
		s.browserUID == s.workerUID || s.browserGID == s.workerGID {
		return "", errors.New("dedicated Browser OS identity unavailable")
	}
	if s.cfg.StateDir == "" || !filepath.IsAbs(s.cfg.StateDir) ||
		filepath.Clean(s.cfg.StateDir) != s.cfg.StateDir || s.cfg.StateDir == "/" {
		return "", errors.New("isolated Browser state path must be trusted and absolute")
	}
	for _, parent := range []string{
		s.cfg.StateDir,
		filepath.Join(s.cfg.StateDir, "runtime"),
		filepath.Join(s.cfg.StateDir, "runtime", "browser"),
	} {
		if err := trustedDir(parent); err != nil {
			return "", fmt.Errorf("isolated Browser state parent untrusted: %w", err)
		}
	}
	home := filepath.Join(s.cfg.StateDir, "browser-home")
	for _, dir := range []string{home, s.isolatedBrowserScratchDir()} {
		info, err := os.Lstat(dir)
		if err != nil {
			return "", fmt.Errorf("isolated Browser directory not provisioned: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm() != 0700 || stat.Uid != s.browserUID ||
			stat.Gid != s.browserGID {
			return "", fmt.Errorf("isolated Browser directory identity is unsafe: %s", dir)
		}
	}
	return home, nil
}
