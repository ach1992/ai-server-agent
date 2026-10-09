package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A per-request worker subprocess uses kernel-enforced filesystem rights.
// Its own parent worker shell remains unrestricted; this protects the narrow
// structured workspace file tool, not arbitrary worker commands.
// Linux with Landlock disabled or unsupported is fail-closed for this tool.
const workerLandlockHandled = unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_DIR |
	unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG |
	unix.LANDLOCK_ACCESS_FS_REFER

type landlockRulesetAttr struct {
	HandledAccessFS uint64
}

type landlockPathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
}

func workerLandlockRestrict(workspaceRoot, selectedWorkspace string, textSearch bool) (unlock func(), err error) {
	// Landlock scopes the calling thread, not arbitrary goroutines. All
	// subsequent file operations must run on this same locked OS thread.
	runtime.LockOSThread()
	unlock = runtime.UnlockOSThread
	defer func() {
		if err != nil {
			unlock()
			unlock = nil
		}
	}()

	// The kernel version/feature probe is required, not inferred from the
	// distribution name. ABI v2 adds REFER; older or disabled kernels fail
	// closed instead of relying on pathname checks for concurrent renames.
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	if errno != 0 || abi < 2 {
		return nil, errors.New("workspace file operations require available Landlock ABI 2 or later")
	}
	root := filepath.Clean(workspaceRoot)
	selected := selectedWorkspace
	if !filepath.IsAbs(selected) {
		selected = filepath.Join(root, selected)
	}
	selected = filepath.Clean(selected)
	if rel, relErr := filepath.Rel(root, selected); relErr == nil {
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			if component == ".git" {
				return nil, errors.New("Git administrative directories are not valid worker workspace roots")
			}
		}
	}
	if !filepath.IsAbs(root) || !withinPath(root, selected) {
		return nil, errors.New("selected workspace lies outside configured workspace")
	}
	// The selected directory is checked through openat2 beneath the
	// configured root so that this rule cannot be anchored to an external
	// directory through symlink traversal.
	rootFD, openErr := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if openErr != nil {
		return nil, fmt.Errorf("open configured workspace for sandbox: %w", openErr)
	}
	defer unix.Close(rootFD)
	relative, relErr := filepath.Rel(root, selected)
	if relErr != nil {
		return nil, relErr
	}
	selectedFD, openErr := unix.Openat2(rootFD, relative, &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if openErr != nil {
		return nil, fmt.Errorf("open selected workspace for sandbox: %w", openErr)
	}
	defer unix.Close(selectedFD)

	handled := uint64(workerLandlockHandled)
	if abi >= 3 {
		handled |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	attr := landlockRulesetAttr{HandledAccessFS: handled}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return nil, fmt.Errorf("create worker workspace sandbox: %w", errno)
	}
	defer unix.Close(int(ruleset))
	rule := landlockPathBeneathAttr{AllowedAccess: handled, ParentFD: int32(selectedFD)}
	_, _, errno = unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, ruleset, uintptr(unix.LANDLOCK_RULE_PATH_BENEATH), uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
	if errno != 0 {
		return nil, fmt.Errorf("restrict worker filesystem rule: %w", errno)
	}
	if textSearch {
		// Ripgrep uses a system-owned executable and shared libraries. Grant
		// only system read access for its runtime, never write permission;
		// all source-file reads remain restricted to the selected workspace.
		for _, candidate := range []string{"/usr/bin/rg", "/lib", "/lib64", "/usr/lib"} {
			resolved, evalErr := filepath.EvalSymlinks(candidate)
			if evalErr != nil {
				continue
			}
			if candidate == "/usr/bin/rg" && !trustedWorkspaceSearchExecutable(resolved) {
				return nil, errors.New("system ripgrep executable is not trusted")
			}
			info, statErr := os.Stat(resolved)
			if statErr != nil {
				continue
			}
			pathFD, openErr := unix.Open(resolved, unix.O_PATH|unix.O_CLOEXEC, 0)
			if openErr != nil {
				return nil, fmt.Errorf("open trusted search runtime: %w", openErr)
			}
			allowed := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE)
			if info.IsDir() {
				allowed |= unix.LANDLOCK_ACCESS_FS_READ_DIR
			}
			rule := landlockPathBeneathAttr{AllowedAccess: allowed, ParentFD: int32(pathFD)}
			_, _, ruleErr := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, ruleset, uintptr(unix.LANDLOCK_RULE_PATH_BENEATH), uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
			unix.Close(pathFD)
			if ruleErr != 0 {
				return nil, fmt.Errorf("limit trusted search runtime: %w", ruleErr)
			}
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("prevent privilege escalation in worker helper: %w", err)
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0)
	if errno != 0 {
		return nil, fmt.Errorf("activate worker filesystem sandbox: %w", errno)
	}
	return unlock, nil
}
