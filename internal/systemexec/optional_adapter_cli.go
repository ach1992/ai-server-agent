package systemexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const optionalAdapterManagementLock = "/run/lock/ai-server-agent/management.lock"

// RunOptionalAdapter is a small, standalone operator CLI in the existing
// Agent binary; it does not need Agent configuration, a new service, or a
// separate package manager. Installation of upstream artifacts remains an
// explicit operator step. No project or PATH-supplied executable is launched.
func RunOptionalAdapter(args []string, stdout, stderr io.Writer) int {
	if err := optionalAdapterCommand(args, stdout); err != nil {
		fmt.Fprintln(stderr, "optional-adapter:", err)
		return 2
	}
	return 0
}

func optionalAdapterCommand(args []string, out io.Writer) error {
	if len(args) < 1 {
		return errors.New("usage: optional-adapter [inspect-tree PATH MAX_BYTES|verify NAME [CANDIDATE]|activate NAME CANDIDATE|rollback NAME|deactivate NAME|status NAME]")
	}
	switch args[0] {
	case "inspect-tree":
		if len(args) != 3 {
			return errors.New("inspect-tree needs absolute PATH and positive MAX_BYTES")
		}
		limit, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || limit <= 0 {
			return errors.New("invalid maximum bytes")
		}
		report, err := InspectResourceTree(args[1], limit)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(report)
	case "status", "verify":
		if (args[0] == "status" && len(args) != 2) || (args[0] == "verify" && len(args) != 2 && len(args) != 3) {
			return errors.New("status NAME or verify NAME [CANDIDATE]")
		}
		if !knownAdapter(args[1]) {
			return errors.New("unknown adapter")
		}
		path := filepath.Join(OptionalAdapterProfilesDir, args[1]+".json")
		if len(args) == 3 {
			path = args[2]
		}
		p, found, err := ReadOptionalAdapterProfile(args[1], path)
		if err != nil {
			return err
		}
		if !found {
			if args[0] == "verify" {
				return errors.New("no active profile to verify")
			}
			return json.NewEncoder(out).Encode(map[string]any{"adapter": args[1], "selection": "built-in-legacy"})
		}
		return json.NewEncoder(out).Encode(map[string]any{"adapter": args[1], "selection": "verified-root-profile", "version": p.Version, "source": p.Source, "entrypoints": p.Entrypoints})
	case "activate":
		if len(args) != 3 || !knownAdapter(args[1]) {
			return errors.New("activate NAME CANDIDATE")
		}
		if err := withAdapterLifecycleLock(func() error { return activateOptionalAdapter(args[1], args[2]) }); err != nil {
			return err
		}
		return optionalAdapterCommand([]string{"status", args[1]}, out)
	case "rollback", "deactivate":
		if len(args) != 2 || !knownAdapter(args[1]) {
			return errors.New("rollback|deactivate NAME")
		}
		action := func() error { return rollbackOptionalAdapter(args[1]) }
		if args[0] == "deactivate" {
			action = func() error { return deactivateOptionalAdapter(args[1]) }
		}
		if err := withAdapterLifecycleLock(action); err != nil {
			return err
		}
		return optionalAdapterCommand([]string{"status", args[1]}, out)
	default:
		return errors.New("unknown optional-adapter command")
	}
}

func knownAdapter(name string) bool {
	_, ok := optionalAdapterRoles[name]
	return ok && optionalAdapterName.MatchString(name)
}

func withAdapterLifecycleLock(action func() error) error {
	if os.Geteuid() != 0 {
		return errors.New("activation and rollback require explicit administrator/root access")
	}
	// The existing lifecycle lock is deliberately root-only (0600) inside a
	// root-only directory (0700). TrustedStaticFile requires world-readability
	// for worker-launched adapter resources, so it is NOT suitable here.
	if err := verifyAdapterLifecycleLockDirectory(); err != nil {
		return err
	}
	fd, err := syscall.Open(optionalAdapterManagementLock, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("lifecycle lock unavailable: %w", err)
	}
	f := os.NewFile(uintptr(fd), optionalAdapterManagementLock)
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Uid != 0 || st.Gid != 0 || st.Mode&0777 != 0600 {
		return errors.New("lifecycle lock must be an unchanged root-owned regular 0600 file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another lifecycle operation is active")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return action()
}

// /run/lock is conventionally a shared sticky directory on Debian/Ubuntu.
// A world-writable ancestor is safe here only when sticky; the dedicated
// child must match the installer-owned 0700 root-only lifecycle namespace.
func verifyAdapterLifecycleLockDirectory() error {
	for _, path := range []string{"/run", "/run/lock", filepath.Dir(optionalAdapterManagementLock)} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("lifecycle lock directory missing or invalid; repair via ensure-lifecycle-lock.sh")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || st.Gid != 0 {
			return errors.New("lifecycle lock directory has unexpected owner")
		}
		if path == filepath.Dir(optionalAdapterManagementLock) && info.Mode().Perm() != 0700 {
			return errors.New("lifecycle lock namespace must be root-only 0700")
		}
		if path != filepath.Dir(optionalAdapterManagementLock) && info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return errors.New("unsafe non-sticky writable lifecycle lock ancestor")
		}
	}
	return nil
}

// Private profile staging is root-owned and uses fsync + atomic rename. Never
// activate an arbitrary file directly from a worker-controlled workspace.
func stageOptionalAdapterProfile(name, candidate string) (string, error) {
	// Before privileged reading/copying, require a root-owned candidate under
	// non-writable, symlink-free ancestors. Never copy a worker-controlled path
	// as root: a concurrent swap could otherwise expose private host bytes in
	// the temporary world-readable profile staging file.
	if !trustedRootProfileFile(candidate) {
		return "", errors.New("candidate must be a root-owned trusted regular JSON file; stage it under a root-controlled directory before activation")
	}
	if err := trustedAdapterProfilesDirectory(); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(candidate)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(OptionalAdapterProfilesDir, "."+name+"-stage-")
	if err != nil {
		return "", err
	}
	stage := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(stage)
		}
	}()
	if _, err = f.Write(raw); err != nil {
		return "", err
	}
	if err = f.Chmod(0644); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if _, found, err := ReadOptionalAdapterProfile(name, stage); !found || err != nil {
		if err == nil {
			err = errors.New("staged profile missing")
		}
		return "", err
	}
	ok = true
	return stage, nil
}

func trustedAdapterProfilesDirectory() error {
	_, err := os.Lstat(OptionalAdapterProfilesDir)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(OptionalAdapterProfilesDir, 0755); err != nil {
			return err
		}
	}
	path := string(filepath.Separator)
	for _, component := range []string{"usr", "local", "lib", "ai-server-agent", "adapter-profiles"} {
		path = filepath.Join(path, component)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("profile directory or ancestor is missing/untrusted")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 {
			return errors.New("profile directory is not root-owned")
		}
	}
	return nil
}

func syncAdapterProfileDirectory() error {
	d, err := os.Open(OptionalAdapterProfilesDir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func activateOptionalAdapter(name, candidate string) error {
	if err := trustedAdapterProfilesDirectory(); err != nil {
		return err
	}
	stage, err := stageOptionalAdapterProfile(name, candidate)
	if err != nil {
		return err
	}
	defer os.Remove(stage)
	active := filepath.Join(OptionalAdapterProfilesDir, name+".json")
	prev := filepath.Join(OptionalAdapterProfilesDir, name+".previous.json")
	if _, err := os.Lstat(active); err == nil {
		if !trustedRootProfileFile(active) {
			return errors.New("current profile path is not a safe regular root-owned file")
		}
		// An interrupted operator retry of the *same* activation must not
		// overwrite the previous-good rollback selection with itself.
		current, readErr := os.ReadFile(active)
		candidateBytes, candidateErr := os.ReadFile(stage)
		if readErr != nil || candidateErr != nil {
			return errors.New("cannot compare current profile with staged selection")
		}
		if bytes.Equal(current, candidateBytes) {
			return nil
		}
		// An invalid selected profile must NOT trap the operator. Replace it
		// with the newly verified candidate without overwriting a previous
		// known-good rollback profile with the invalid one.
		if _, _, err := ReadOptionalAdapterProfile(name, active); err == nil {
			backup, err := stageOptionalAdapterProfile(name, active)
			if err != nil {
				return fmt.Errorf("cannot safely back up current profile: %w", err)
			}
			defer os.Remove(backup)
			if err := os.Rename(backup, prev); err != nil {
				return err
			}
			if err := syncAdapterProfileDirectory(); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, active); err != nil {
		return err
	}
	return syncAdapterProfileDirectory()
}

func rollbackOptionalAdapter(name string) error {
	if err := trustedAdapterProfilesDirectory(); err != nil {
		return err
	}
	active := filepath.Join(OptionalAdapterProfilesDir, name+".json")
	prev := filepath.Join(OptionalAdapterProfilesDir, name+".previous.json")
	if _, exists, err := ReadOptionalAdapterProfile(name, prev); err != nil {
		return err
	} else if !exists {
		// First activation: the built-in fallback is the previous selection.
		// Keep the candidate as a recoverable inactive file.
		return deactivateOptionalAdapter(name)
	}
	if _, err := os.Lstat(active); errors.Is(err, os.ErrNotExist) {
		// Recover even if an interrupted operator action left no active file.
		if err := os.Rename(prev, active); err != nil {
			return err
		}
		return syncAdapterProfileDirectory()
	} else if err != nil || !trustedRootProfileFile(active) {
		return errors.New("current profile path is not a safe root-owned file")
	}
	// The current profile may be invalid; the previous profile is verified.
	// Linux RENAME_EXCHANGE atomically swaps the two root-controlled names;
	// no momentarily absent profile or implicit version fallback is exposed.
	if err := renameAdapterProfilesExchange(active, prev); err != nil {
		return err
	}
	return syncAdapterProfileDirectory()
}

func trustedRootProfileFile(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0 && st.Size() <= 32<<10 && TrustedStaticFile(path)
}

func deactivateOptionalAdapter(name string) error {
	if err := trustedAdapterProfilesDirectory(); err != nil {
		return err
	}
	active := filepath.Join(OptionalAdapterProfilesDir, name+".json")
	if !trustedRootProfileFile(active) {
		return errors.New("no safe active profile to deactivate")
	}
	inactive := filepath.Join(OptionalAdapterProfilesDir, name+".inactive.json")
	if err := os.Rename(active, inactive); err != nil {
		return err
	}
	return syncAdapterProfileDirectory()
}
