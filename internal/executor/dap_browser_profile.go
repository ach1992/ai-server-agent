package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// This is a disposable new single-DAP browser profile, not the shared Browser
// profile, a user-selected directory, or project build output. The root-owned
// parent is non-writable to aiworker so it cannot replace the cleanup target.
const browserDAPProfileName = "profile"

func createBrowserDAPProfile(workerUID, workerGID uint32) (parent, profile string, err error) {
	if os.Geteuid() != 0 || workerUID == 0 {
		return "", "", errors.New("browser_dap_private_profile_requires_root_executor")
	}
	parent, err = os.MkdirTemp("/tmp", ".asa-browser-dap-")
	if err != nil {
		return "", "", err
	}
	bad := func(e error) (string, string, error) { _ = os.Remove(parent); return "", "", e }
	if err := os.Chown(parent, 0, int(workerGID)); err != nil {
		return bad(err)
	}
	if err := os.Chmod(parent, 0710); err != nil {
		return bad(err)
	}
	profile = filepath.Join(parent, browserDAPProfileName)
	if err := os.Mkdir(profile, 0700); err != nil {
		return bad(err)
	}
	if err := os.Chown(profile, int(workerUID), int(workerGID)); err != nil {
		_ = os.Remove(profile)
		return bad(err)
	}
	return parent, profile, nil
}

// Invoke only before any browser launch, or after the existing broker has
// positively confirmed its private cgroup is empty and all descendants dead.
// Only a root-owned exact generated parent may be removed; never arbitrary
// user-selected paths or the managed shared Chromium profile.
func removeBrowserDAPProfile(parent string) error {
	if !filepath.IsAbs(parent) || filepath.Dir(parent) != "/tmp" || len(filepath.Base(parent)) < len(".asa-browser-dap-")+6 ||
		filepath.Base(parent)[:len(".asa-browser-dap-")] != ".asa-browser-dap-" {
		return errors.New("browser_dap_profile_identity_unproven")
	}
	st, err := os.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || st.Mode().Perm() != 0710 || stat.Uid != 0 {
		return errors.New("browser_dap_root_owned_parent_changed")
	}
	profile := filepath.Join(parent, browserDAPProfileName)
	fi, err := os.Lstat(profile)
	if err == nil {
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("browser_dap_profile_replaced")
		}
		if err := os.RemoveAll(profile); err != nil {
			return fmt.Errorf("browser_dap_profile_cleanup_unknown: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(parent); err != nil {
		return fmt.Errorf("browser_dap_parent_cleanup_unknown: %w", err)
	}
	return nil
}
