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

// The numeric host UID pool belongs exclusively to concurrent isolated
// Browser sessions on this dedicated Agent host. Each process maps the
// ordinary aibrowser UID to a different host UID in its own user namespace.
// No per-session login accounts or global privilege daemon are created.
const (
	browserSessionHostUIDBase  uint32 = 2080000000
	browserSessionHostUIDSlots uint32 = 64 // configured Browser capacity is at most 64
)

func validBrowserSessionHostUID(uid uint32) bool {
	return uid >= browserSessionHostUIDBase &&
		uid < browserSessionHostUIDBase+browserSessionHostUIDSlots
}

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
	// Separate root-owned session scratch from the legacy aibrowser-owned
	// directory, preserving installed-build rollback without migrating its
	// mutable profile content or weakening its permissions.
	return filepath.Join(s.cfg.StateDir, "runtime", "browser", "isolated-sessions")
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
	for _, dir := range []struct {
		path string
		uid  uint32
		gid  uint32
		mode os.FileMode
	}{
		{home, s.browserUID, s.browserGID, 0700},
		{s.isolatedBrowserScratchDir(), 0, 0, 0711},
	} {
		info, err := os.Lstat(dir.path)
		if err != nil {
			return "", fmt.Errorf("isolated Browser directory not provisioned: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm() != dir.mode || stat.Uid != dir.uid ||
			stat.Gid != dir.gid {
			return "", fmt.Errorf("isolated Browser directory identity is unsafe: %s", dir.path)
		}
	}
	return home, nil
}

// verifyBrowserSessionHostUID rejects collisions with a real host account.
// The reserved range is a host installation contract, not a public request
// parameter or a per-client permission policy.
func (s *Server) verifyBrowserSessionHostUID(uid uint32) error {
	if !validBrowserSessionHostUID(uid) || uid == s.browserUID ||
		uid == s.workerUID || uid == s.agentUID {
		return errors.New("isolated Browser host filesystem UID is outside the reserved range")
	}
	_, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err == nil {
		return fmt.Errorf("isolated Browser host UID %d conflicts with an existing account", uid)
	}
	var unknown user.UnknownUserIdError
	if !errors.As(err, &unknown) {
		return fmt.Errorf("resolve isolated Browser host UID collision: %w", err)
	}
	return nil
}

// prepareBrowserSessionScratch uses root-owned names created from the exact
// broker session nonce. Only the root Executor can create children in this
// 0711 parent, so a different Browser UID cannot swap or rename a peer
// session's scratch. The root Executor checks identity before launch.
func (s *Server) prepareBrowserSessionScratch(hostUID uint32, profile, trace string) error {
	if err := s.verifyBrowserSessionHostUID(hostUID); err != nil {
		return err
	}
	if _, err := s.isolatedBrowserHome(); err != nil {
		return err
	}
	for _, item := range []struct {
		name   string
		prefix string
	}{
		{profile, "asa-profile-"},
		{trace, "asa-trace-"},
	} {
		if len(item.name) != len(item.prefix)+32 ||
			item.name[:len(item.prefix)] != item.prefix ||
			!browserScratchName(item.name) {
			return errors.New("invalid isolated Browser scratch name")
		}
	}
	var created []string
	prepared := false
	defer func() {
		// On failure before a child starts, remove only empty names created
		// by this call; successful preparation transfers cleanup ownership
		// to the existing Executor cgroup/verified-close contract.
		if !prepared {
			for _, dir := range created {
				_ = os.Remove(dir)
			}
		}
	}()
	for _, name := range []string{profile, trace} {
		path := filepath.Join(s.isolatedBrowserScratchDir(), name)
		if err := os.Mkdir(path, 0700); err != nil {
			return fmt.Errorf("create isolated Browser session scratch: %w", err)
		}
		created = append(created, path)
		if err := os.Chown(path, int(hostUID), int(s.browserGID)); err != nil {
			return fmt.Errorf("assign isolated Browser filesystem owner: %w", err)
		}
	}
	prepared = true
	return nil
}
