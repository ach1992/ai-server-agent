package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// phpHTTPServiceUnit is an observation from the root-owned systemd manager,
// not a capability token. The separate protected job claim also must match.
type phpHTTPServiceUnit struct {
	id         string
	invocation string
	cgroup     string
	mainPID    int
}

func parsePHPHTTPServiceUnit(jobID, output, workerUser string) (phpHTTPServiceUnit, error) {
	if _, err := safeID(jobID); err != nil || workerUser == "" || len(output) > systemdOutputLimit {
		return phpHTTPServiceUnit{}, errors.New("invalid PHP HTTP service unit evidence")
	}
	values := make(map[string]string, 8)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return phpHTTPServiceUnit{}, errors.New("malformed PHP HTTP service unit property")
		}
		if _, found := values[key]; found {
			return phpHTTPServiceUnit{}, errors.New("ambiguous duplicate PHP HTTP service unit property")
		}
		values[key] = value
	}
	unit := "ai-job-" + jobID + ".service"
	if values["Id"] != unit || values["LoadState"] != "loaded" ||
		values["ActiveState"] != "active" || values["SubState"] != "running" ||
		values["User"] != workerUser {
		return phpHTTPServiceUnit{}, errors.New("PHP HTTP service is not the expected active worker Job unit")
	}
	cgroup := "/system.slice/" + unit
	if values["ControlGroup"] != cgroup || filepath.Clean(values["ControlGroup"]) != cgroup {
		return phpHTTPServiceUnit{}, errors.New("PHP HTTP service cgroup identity mismatch")
	}
	invocation := values["InvocationID"]
	if len(invocation) != 32 {
		return phpHTTPServiceUnit{}, errors.New("PHP HTTP service invocation identity missing")
	}
	for _, c := range invocation {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return phpHTTPServiceUnit{}, errors.New("invalid PHP HTTP service invocation identity")
		}
	}
	pid, err := strconv.Atoi(values["MainPID"])
	if err != nil || pid <= 1 {
		return phpHTTPServiceUnit{}, errors.New("PHP HTTP service main PID unavailable")
	}
	return phpHTTPServiceUnit{id: jobID, invocation: invocation, cgroup: cgroup, mainPID: pid}, nil
}

func readPHPHTTPServiceUnit(ctx context.Context, jobID, workerUser string) (phpHTTPServiceUnit, error) {
	if _, err := safeID(jobID); err != nil {
		return phpHTTPServiceUnit{}, errors.New("invalid PHP HTTP service Job ID")
	}
	result, err := runBoundedJobSystemd(ctx, jobSystemdProbeTimeout, "systemctl", "show",
		"ai-job-"+jobID+".service", "--no-pager",
		"--property=Id,LoadState,ActiveState,SubState,MainPID,ControlGroup,InvocationID,User")
	if err != nil || result.Truncated {
		return phpHTTPServiceUnit{}, fmt.Errorf("PHP HTTP service unit observation unavailable: %v", err)
	}
	return parsePHPHTTPServiceUnit(jobID, result.Output, workerUser)
}

// phpHTTPServicePin only keeps a read-only pidfd on the separately owned
// service runner. Closing it NEVER sends a signal or stops that service.
// The DAP adapter must be contained in its own distinct session cgroup.
type phpHTTPServicePin struct {
	unit  phpHTTPServiceUnit
	pidfd *os.File
}

func (p *phpHTTPServicePin) close() error {
	if p == nil || p.pidfd == nil {
		return nil
	}
	return p.pidfd.Close()
}

func verifyPHPHTTPServiceRunner(unit phpHTTPServiceUnit, workerUID uint32) (*os.File, error) {
	if os.Geteuid() != 0 || workerUID == 0 || unit.mainPID <= 1 {
		return nil, errors.New("PHP HTTP service proof requires root Executor and worker UID")
	}
	groupPath := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(unit.cgroup, "/"))
	groupStat, err := os.Lstat(groupPath)
	if err != nil || !groupStat.IsDir() || groupStat.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("PHP HTTP root-owned service cgroup unavailable")
	}
	root, ok := groupStat.Sys().(*syscall.Stat_t)
	if !ok || root.Uid != 0 || groupStat.Mode().Perm()&0022 != 0 {
		return nil, errors.New("PHP HTTP service cgroup ownership unsafe")
	}
	fd, err := unix.PidfdOpen(unit.mainPID, 0)
	if err != nil {
		return nil, fmt.Errorf("PHP HTTP service PID pin unavailable: %w", err)
	}
	pin := os.NewFile(uintptr(fd), "php-http-original-service-runner-pidfd")
	fail := func(e error) (*os.File, error) {
		_ = pin.Close()
		return nil, e
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		return fail(errors.New("PHP HTTP service runner no longer live"))
	}
	procGroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", unit.mainPID))
	if err != nil || strings.TrimSpace(string(procGroup)) != "0::"+unit.cgroup {
		return fail(errors.New("PHP HTTP service runner cgroup mismatch"))
	}
	procDir, err := os.Stat(fmt.Sprintf("/proc/%d", unit.mainPID))
	if err != nil {
		return fail(errors.New("PHP HTTP service process identity unavailable"))
	}
	procOwner, ok := procDir.Sys().(*syscall.Stat_t)
	if !ok || procOwner.Uid != workerUID {
		return fail(errors.New("PHP HTTP service runner not owned by authorized worker"))
	}
	selfExe, selfErr := os.Stat("/proc/self/exe")
	runnerExe, runnerErr := os.Stat(fmt.Sprintf("/proc/%d/exe", unit.mainPID))
	if selfErr != nil || runnerErr != nil || !os.SameFile(selfExe, runnerExe) {
		return fail(errors.New("PHP HTTP service runner executable does not match current Executor"))
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		return fail(errors.New("PHP HTTP service runner died during identity verification"))
	}
	return pin, nil
}

func (s *Server) pinAuthorizedPHPHTTPService(
	ctx context.Context, req Request, workspace string,
) (*phpHTTPServicePin, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("PHP HTTP service proof requires root Executor")
	}
	if _, err := s.loadAuthorizedPHPHTTPServiceClaim(ctx, req, workspace); err != nil {
		return nil, err
	}
	first, err := readPHPHTTPServiceUnit(ctx, req.JobID, s.cfg.WorkerUser)
	if err != nil {
		return nil, err
	}
	pin, err := verifyPHPHTTPServiceRunner(first, s.workerUID)
	if err != nil {
		return nil, err
	}
	// Re-read root-owned systemd truth after pinning; launch/stop/restart can
	// race the claim read and the initial observation.
	current, err := readPHPHTTPServiceUnit(ctx, req.JobID, s.cfg.WorkerUser)
	if err != nil || first != current || unix.PidfdSendSignal(int(pin.Fd()), 0, nil, 0) != nil {
		_ = pin.Close()
		return nil, errors.New("PHP HTTP service identity changed while pinning")
	}
	return &phpHTTPServicePin{unit: current, pidfd: pin}, nil
}
