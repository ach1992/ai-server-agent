package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Linux 6.9+ UAPI (x/sys v0.41.0 predates this typed constant).
const pidfdSignalProcessGroup = 0x4

// pidfd process-group delivery is supported on Linux 6.9+. On older kernels
// the only safe numeric fallback is for an executor-owned direct child which
// has NOT been waited/reaped (including a WNOWAIT zombie). That unreaped
// process reserves the process-group leader's PID against recycling.
// Delve's traced program is NOT a direct child: never apply kill(-PGID) to it.
func signalPinnedGroup(pgid int, pin *os.File, unreapedChild bool, sig syscall.Signal) error {
	return signalPinnedGroupWith(pgid, pin, unreapedChild, sig, func(fd int, signal syscall.Signal) error {
		return unix.PidfdSendSignal(fd, signal, nil, pidfdSignalProcessGroup)
	})
}

// Explicitly injectable for deterministic compatibility tests: older kernels
// return EINVAL for the group flag and must never trigger a numeric fallback
// for a process which is not our unreaped direct child.
func signalPinnedGroupWith(pgid int, pin *os.File, unreapedChild bool, sig syscall.Signal, attempt func(int, syscall.Signal) error) error {
	if pgid <= 0 || pin == nil {
		return errors.New("group identity not pinned")
	}
	err := attempt(int(pin.Fd()), sig)
	if err == nil {
		return nil
	}
	if !unreapedChild {
		return fmt.Errorf("pidfd process-group signal not supported or leader exited (no unsafe numeric fallback): %w", err)
	}
	// The DIRECT child has deliberately not been reaped. Linux cannot recycle
	// its PID while the parent is holding its wait status; numeric PGID is safe.
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.ESRCH) {
		return err
	}
	err = syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Only observability: no group signalling is authorized from /proc scans.
// A zombie leader cannot keep the cleanup loop artificially alive.
func liveGroupMembers(pgid int) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	live := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("group member inspection unverified: %w", err)
		}
		ix := strings.LastIndexByte(string(raw), ')')
		if ix < 0 {
			return 0, errors.New("invalid proc stat field boundary")
		}
		fields := strings.Fields(string(raw[ix+1:]))
		if len(fields) < 3 {
			return 0, errors.New("invalid proc process group fields")
		}
		group, e := strconv.Atoi(fields[2])
		if e != nil {
			return 0, e
		}
		if group == pgid && fields[0] != "Z" && fields[0] != "X" && fields[0] != "x" {
			live++
		}
	}
	return live, nil
}

// A success is returned only after no live members remain. The pidfd is the
// signalling authority on recent kernels; unreaped direct child on older
// kernels is a proven numeric PID reservation. Never report an unverified
// group stop as success, or signal a recycled numeric group.
func terminatePinnedGroup(pgid int, pin *os.File, unreapedChild bool) (bool, error) {
	if pgid <= 0 || pin == nil {
		return false, errors.New("group identity not pinned")
	}
	count, err := liveGroupMembers(pgid)
	if err != nil {
		return false, err
	}
	if count == 0 {
		// No signal is sent and no numeric PGID is used as authority here.
		// A tracee may have exited/reaped normally before reconciliation.
		return false, nil
	}
	if err := signalPinnedGroup(pgid, pin, unreapedChild, syscall.SIGTERM); err != nil {
		return true, err
	}
	deadline := time.Now().Add(processGroupTerminateGrace)
	for time.Now().Before(deadline) {
		count, err = liveGroupMembers(pgid)
		if err != nil {
			return true, err
		}
		if count == 0 {
			return true, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := signalPinnedGroup(pgid, pin, unreapedChild, syscall.SIGKILL); err != nil {
		return true, err
	}
	deadline = time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		count, err = liveGroupMembers(pgid)
		if err != nil {
			return true, err
		}
		if count == 0 {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true, fmt.Errorf("pinned process group still has %d live members", count)
}
