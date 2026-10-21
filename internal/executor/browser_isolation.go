package executor

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// The isolated Browser account owns only disposable Browser/session state.
// aiworker and aiagent are deliberately NOT members of its private group.
// Root controls each parent directory so neither account can replace the
// private namespace root. A valid Browser UID is necessary but not sufficient.
// Separate numeric user AND primary group identities are required; distinct
// account/group spellings do not confer a Linux privilege boundary.
func distinctIsolatedBrowserIdentity(browserUID, browserGID, workerUID, workerGID, agentUID, agentGID uint32) bool {
	return browserUID != 0 && browserGID != 0 &&
		workerUID != 0 && workerGID != 0 &&
		agentUID != 0 && agentGID != 0 &&
		browserUID != workerUID && browserUID != agentUID &&
		browserGID != workerGID && browserGID != agentGID
}

// Re-resolve the two other installed service identities at admission. An
// account/GID change since Executor initialization must fail closed without
// accepting the Browser or disrupting shared Browser/worker sessions.
func (s *Server) verifyIsolatedBrowserIdentities() error {
	if !distinctIsolatedBrowserIdentity(s.browserUID, s.browserGID,
		s.workerUID, s.workerGID, s.agentUID, s.agentGID) {
		return errors.New("dedicated Browser identity must differ from both Agent and worker numeric UID/GID")
	}
	for _, role := range []struct {
		name string
		uid  uint32
		gid  uint32
	}{
		{s.cfg.AgentUser, s.agentUID, s.agentGID},
		{s.cfg.WorkerUser, s.workerUID, s.workerGID},
	} {
		if role.name == "" {
			return errors.New("isolated Browser requires both configured service account names")
		}
		account, err := user.Lookup(role.name)
		if err != nil {
			return fmt.Errorf("resolve isolated Browser peer identity %s: %w", role.name, err)
		}
		uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
		gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
		if uidErr != nil || gidErr != nil || uint32(uid) != role.uid || uint32(gid) != role.gid {
			return fmt.Errorf("isolated Browser peer numeric identity changed: %s", role.name)
		}
		group, err := user.LookupGroup(role.name)
		if err != nil {
			return fmt.Errorf("resolve isolated Browser peer group %s: %w", role.name, err)
		}
		groupGID, err := strconv.ParseUint(group.Gid, 10, 32)
		if err != nil || groupGID == 0 || uint32(groupGID) == s.browserGID {
			return fmt.Errorf("isolated Browser aliases protected peer group %s", role.name)
		}
	}
	return nil
}

func (s *Server) isolatedBrowserScratchDir() string {
	return filepath.Join(s.cfg.StateDir, "runtime", "browser", "isolated")
}

func (s *Server) isolatedBrowserHome() (string, error) {
	if os.Geteuid() != 0 {
		return "", errors.New("isolated Browser requires root Executor")
	}
	if err := s.verifyIsolatedBrowserIdentities(); err != nil {
		return "", fmt.Errorf("dedicated Browser OS identity unavailable: %w", err)
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
