package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// RunBrowserSessionHelper is the brief self-exec stage for production isolated
// Browser sessions. It does not interpret user JavaScript or create a second
// Browser process: after restricting disk writes it execs the pinned Node
// runtime in place, inside the existing Executor-owned Browser cgroup.
//
// All authenticated clients retain advanced Playwright and filesystem reads.
// The write boundary protects the mutable profile/trace of another *live*
// session sharing the dedicated aibrowser UID. It is not a secrecy boundary.
func RunBrowserSessionHelper(args []string) int {
	if len(args) < 4 || !filepath.IsAbs(args[0]) || filepath.Clean(args[0]) != args[0] ||
		args[1] != "--input-type=module" || args[2] != "-e" ||
		os.Getenv("ASA_BROWSER_RECLAIM_BY_EXECUTOR") != "1" {
		fmt.Fprintln(os.Stderr, "browser_session_write_boundary_invalid_launch")
		return 2
	}
	profile := os.Getenv("TMPDIR")
	profileName := os.Getenv("ASA_BROWSER_SESSION_PROFILE")
	traceName := os.Getenv("ASA_BROWSER_SESSION_TRACE")
	if !browserScratchName(profileName) || !browserScratchName(traceName) ||
		len(profileName) != len("asa-profile-")+32 || len(traceName) != len("asa-trace-")+32 ||
		profileName[:len("asa-profile-")] != "asa-profile-" ||
		traceName[:len("asa-trace-")] != "asa-trace-" ||
		!filepath.IsAbs(profile) || filepath.Clean(profile) != profile ||
		filepath.Base(profile) != profileName || os.Getenv("HOME") != profile {
		fmt.Fprintln(os.Stderr, "browser_session_write_boundary_invalid_identity")
		return 2
	}
	trace := filepath.Join(filepath.Dir(profile), traceName)
	if err := browserSessionPrepareWritableDirs(profile, trace); err != nil {
		fmt.Fprintln(os.Stderr, "browser_session_write_boundary_scratch_unavailable:", err)
		return 1
	}
	// Landlock is per-thread. Keep the exact thread confined until execve
	// replaces the Go helper with Node; unlocking before exec would permit
	// the Go scheduler to select an unconstrained thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := browserSessionAllowWrites(profile, trace); err != nil {
		fmt.Fprintln(os.Stderr, "browser_session_write_boundary_unavailable:", err)
		return 1
	}
	if err := unix.Exec(args[0], args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "browser_session_node_exec_failed:", err)
		return 1
	}
	return 0
}

// Landlock ABI 1/2 cannot deny truncation (including O_RDONLY|O_TRUNC).
// A partial write boundary must not advertise session-integrity protection.
func browserSessionWriteAccess(abi uintptr) (uint64, error) {
	if abi < 3 {
		return 0, errors.New("isolated Browser write boundary requires Landlock ABI 3 or later (file truncation unsupported)")
	}
	// Leave READ_FILE / READ_DIR unhandled. This is a cross-session integrity
	// guard, not an arbitrary filesystem-read ban on authorized scripts.
	writes := uint64(unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE)
	return writes, nil
}

// Caller must retain its OS thread through unix.Exec after this returns.
func browserSessionAllowWrites(profile, trace string) error {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	if errno != 0 {
		return fmt.Errorf("query isolated Browser Landlock ABI: %w", errno)
	}
	writes, err := browserSessionWriteAccess(abi)
	if err != nil {
		return err
	}
	attr := landlockRulesetAttr{HandledAccessFS: writes}
	set, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create isolated Browser write boundary: %w", errno)
	}
	defer unix.Close(int(set))
	for _, dir := range []string{profile, trace, "/dev/shm"} {
		fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("open isolated Browser write path: %w", err)
		}
		rule := landlockPathBeneathAttr{AllowedAccess: writes, ParentFD: int32(fd)}
		_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, set, uintptr(unix.LANDLOCK_RULE_PATH_BENEATH), uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		unix.Close(fd)
		if e != 0 {
			return fmt.Errorf("allow isolated Browser write path: %w", e)
		}
	}
	nullFD, err := unix.Open("/dev/null", unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		rule := landlockPathBeneathAttr{AllowedAccess: unix.LANDLOCK_ACCESS_FS_WRITE_FILE, ParentFD: int32(nullFD)}
		_, _, errno = unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, set, uintptr(unix.LANDLOCK_RULE_PATH_BENEATH), uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		unix.Close(nullFD)
		if errno != 0 {
			return fmt.Errorf("allow /dev/null write: %w", errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prevent privilege escalation before Browser write boundary: %w", err)
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, set, 0, 0)
	if errno != 0 {
		return fmt.Errorf("apply isolated Browser write boundary: %w", errno)
	}
	return nil
}

// These checks run before Landlock while the pinned, unprivileged helper can
// create its own empty scratch. It never accepts a writable parent from a
// Browser script and never opens or modifies a peer session directory.
func browserSessionPrepareWritableDirs(profile, trace string) error {
	for _, dir := range []string{profile, trace} {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return errors.New("Browser session scratch path must be clean and absolute")
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			return fmt.Errorf("create own Browser session scratch: %w", err)
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 ||
			info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Browser session scratch identity invalid")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
			return errors.New("Browser session scratch owner invalid")
		}
	}
	return nil
}
