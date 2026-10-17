package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// phpHTTPDebugTarget pins, without owning, one existing PHP built-in HTTP
// server in an independently started worker Service Job. Only its DAP adapter
// can be terminated by debug_stop; never signal servicePIN/phpPIN.
type phpHTTPDebugTarget struct {
	mu             sync.Mutex
	service        *phpHTTPServicePin
	phpPID         int
	phpPin         *os.File
	phpExe         string
	module         string
	workerUser     string
	workspace      string
	program        string
	port           int
	trigger        string
	socket         string
	sourceInfo     os.FileInfo
	proxy          *phpDBGpProxy
	requestStarted bool
	requestDone    bool
	requestError   string
	httpCode       int
	cancel         context.CancelFunc
}

func validPHPHTTPTrigger(v string) bool {
	if len(v) != 32 {
		return false
	}
	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// The original service declares this stable local pathname before launch.
// /tmp must be a root-owned sticky directory: after bind, a ROOT-owned
// socket cannot be replaced by an unprivileged Worker even when that Worker
// knows the exact pathname. Only its group receives connect permission.
func phpHTTPClientSocket(trigger string) (string, error) {
	if !validPHPHTTPTrigger(trigger) {
		return "", errors.New("PHP HTTP requires a 32-character lowercase hex per-service request token")
	}
	path := filepath.Join("/tmp", ".asa-php-http-"+trigger+".sock")
	if len(path) > terminalMaxUnixSocketPath {
		return "", errors.New("PHP HTTP Unix socket path exceeds supported kernel length")
	}
	return path, nil
}

func phpHTTPTrustedSocketParent() error {
	info, err := os.Lstat("/tmp")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSticky == 0 {
		return errors.New("PHP HTTP requires existing root-owned sticky /tmp")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errors.New("PHP HTTP root-owned sticky parent could not be verified")
	}
	return nil
}

// One exact, recommended PHP -S command, with no executable/script/remote
// attach ambiguity. Absolute trusted PHP interpreter and Xdebug module inode
// are checked separately; argv is descriptive evidence, never sufficient
// authority by itself.
func expectedPHPHTTPArgv(php, module, workspace string, port int, trigger, socket string) []string {
	return []string{
		php, "-n", "-d", "zend_extension=" + module,
		"-d", "xdebug.mode=debug",
		"-d", "xdebug.start_with_request=trigger",
		"-d", "xdebug.trigger_value=" + trigger,
		"-d", "xdebug.discover_client_host=0",
		"-d", "xdebug.client_host=unix://" + socket,
		"-S", "127.0.0.1:" + strconv.Itoa(port), "-t", workspace,
	}
}

func phpHTTPArgsMatch(args []string, php, module, workspace string, port int, trigger, socket string) bool {
	want := expectedPHPHTTPArgv(php, module, workspace, port, trigger, socket)
	if len(args) != len(want) || (args[0] != php && filepath.Base(args[0]) != filepath.Base(php)) {
		return false
	}
	for i := 1; i < len(want); i++ {
		if args[i] != want[i] {
			return false
		}
	}
	return true
}

func boundedPHPHTTPArgv(pid int) ([]string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(b) > 8192 || len(b) < 2 || b[len(b)-1] != 0 {
		return nil, errors.New("PHP HTTP process arguments cannot be bounded and verified")
	}
	parts := strings.Split(string(b[:len(b)-1]), "\x00")
	for _, p := range parts {
		if p == "" {
			return nil, errors.New("PHP HTTP contains empty process argument")
		}
	}
	return parts, nil
}

// Verify the exact 127.0.0.1 TCP listen inode is open by THIS pinned PHP,
// rather than trusting merely that some process answers a localhost port.
func phpHTTPProcessOwnsLoopbackListener(pid, port int) bool {
	tcp, err := os.Open(fmt.Sprintf("/proc/%d/net/tcp", pid))
	if err != nil {
		return false
	}
	defer tcp.Close()
	b, err := io.ReadAll(io.LimitReader(tcp, (64<<10)+1))
	if err != nil || len(b) >= (64<<10) {
		return false
	}
	local := "0100007F:" + fmt.Sprintf("%04X", port)
	var inodes []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 10 && strings.EqualFold(f[1], local) && f[3] == "0A" && f[9] != "0" {
			inodes = append(inodes, f[9])
		}
	}
	if len(inodes) != 1 {
		return false
	}
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil || len(fds) > 256 {
		return false
	}
	expected := "socket:[" + inodes[0] + "]"
	for _, fd := range fds {
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		if err == nil && link == expected {
			return true
		}
	}
	return false
}

func (p *phpHTTPDebugTarget) verifyPHPIdentity() error {
	if p == nil {
		return errors.New("PHP HTTP pinned service and interpreter identity missing")
	}
	p.mu.Lock()
	if p.service == nil || p.service.pidfd == nil || p.phpPin == nil {
		p.mu.Unlock()
		return errors.New("PHP HTTP pinned service and interpreter identity missing")
	}
	// Retain stable, independent fd references while the session broker
	// may concurrently retire the original read-only pins. An unlocked
	// File.Fd() could otherwise be closed and reused during verification.
	serviceFD, serviceErr := unix.Dup(int(p.service.pidfd.Fd()))
	phpFD := -1
	var phpErr error
	if serviceErr == nil {
		phpFD, phpErr = unix.Dup(int(p.phpPin.Fd()))
	}
	unit := p.service.unit
	p.mu.Unlock()
	if serviceErr != nil {
		return fmt.Errorf("PHP HTTP service pin duplication failed: %w", serviceErr)
	}
	defer unix.Close(serviceFD)
	if phpErr != nil {
		return fmt.Errorf("PHP HTTP process pin duplication failed: %w", phpErr)
	}
	defer unix.Close(phpFD)
	if unix.PidfdSendSignal(serviceFD, 0, nil, 0) != nil ||
		unix.PidfdSendSignal(phpFD, 0, nil, 0) != nil {
		return errors.New("PHP HTTP original service or PHP process no longer live")
	}
	group, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", p.phpPID))
	if err != nil || strings.TrimSpace(string(group)) != "0::"+unit.cgroup {
		return errors.New("PHP HTTP process escaped the exact service cgroup")
	}
	proc, err := os.Stat(fmt.Sprintf("/proc/%d/exe", p.phpPID))
	accepted, trustErr := os.Stat(p.phpExe)
	if err != nil || trustErr != nil || !os.SameFile(proc, accepted) {
		return errors.New("PHP HTTP process executable identity changed")
	}
	args, err := boundedPHPHTTPArgv(p.phpPID)
	if err != nil || !phpHTTPArgsMatch(args, p.phpExe, p.module, p.workspace, p.port, p.trigger, p.socket) {
		return errors.New("PHP HTTP interpreter launch configuration unverified")
	}
	if p.sourceInfo != nil {
		current, err := os.Lstat(filepath.Join(p.workspace, p.program))
		if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, p.sourceInfo) ||
			current.Size() != p.sourceInfo.Size() || !current.ModTime().Equal(p.sourceInfo.ModTime()) {
			return errors.New("PHP HTTP source identity or contents changed since launch")
		}
	}
	if !phpHTTPProcessOwnsLoopbackListener(p.phpPID, p.port) {
		return errors.New("PHP HTTP pinned process does not own the exact loopback listening socket")
	}
	return nil
}

func (p *phpHTTPDebugTarget) verifyService(ctx context.Context, workerUser string) error {
	if err := p.verifyPHPIdentity(); err != nil {
		return err
	}
	p.mu.Lock()
	service := p.service
	var original phpHTTPServiceUnit
	if service != nil {
		original = service.unit
	}
	p.mu.Unlock()
	if original.id == "" {
		return errors.New("PHP HTTP protected service unit identity was retired")
	}
	live, err := readPHPHTTPServiceUnit(ctx, original.id, workerUser)
	if err != nil || live != original {
		return errors.New("PHP HTTP protected service unit drifted or cannot be verified")
	}
	return p.verifyPHPIdentity()
}

func pinPHPHTTPProcess(p *phpHTTPDebugTarget, workerUID uint32) error {
	if os.Geteuid() != 0 || workerUID == 0 || p == nil || p.service == nil {
		return errors.New("PHP HTTP process validation requires authorized root Executor")
	}
	root := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(p.service.unit.cgroup, "/"))
	b, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil || len(b) > 8192 {
		return errors.New("PHP HTTP Service Job process list unverified")
	}
	pids := strings.Fields(string(b))
	if len(pids) == 0 || len(pids) > 64 {
		return errors.New("PHP HTTP Service Job process count outside bounded proof")
	}
	matches := 0
	for _, v := range pids {
		pid, err := strconv.Atoi(v)
		if err != nil || pid < 2 {
			return errors.New("PHP HTTP service cgroup process list malformed")
		}
		exe, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
		target, terr := os.Stat(p.phpExe)
		if err != nil || terr != nil || !os.SameFile(exe, target) {
			continue
		}
		args, err := boundedPHPHTTPArgv(pid)
		if err != nil || !phpHTTPArgsMatch(args, p.phpExe, p.module, p.workspace, p.port, p.trigger, p.socket) {
			continue
		}
		procDir, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
		if err != nil || !processOwnedByUID(procDir, workerUID) || !phpHTTPProcessOwnsLoopbackListener(pid, p.port) {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			return errors.New("PHP HTTP exact process could not be pinned")
		}
		matches++
		if matches > 1 {
			_ = unix.Close(fd)
			return errors.New("PHP HTTP multiple matching service processes")
		}
		p.phpPID = pid
		p.phpPin = os.NewFile(uintptr(fd), "php-http-original-php-process-pidfd")
	}
	if matches != 1 {
		return errors.New("PHP HTTP exactly one matching PHP built-in service required")
	}
	if err := p.verifyPHPIdentity(); err != nil {
		_ = p.phpPin.Close()
		p.phpPin = nil
		return err
	}
	return nil
}

func (p *phpHTTPDebugTarget) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	cancel, phpPin, service := p.cancel, p.phpPin, p.service
	p.cancel, p.phpPin, p.service = nil, nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if phpPin != nil {
		_ = phpPin.Close()
	}
	if service != nil {
		_ = service.close()
	}
}

// Snapshot or duplicate read-only service identity *under the same mutex*
// used by asynchronous HTTP request completion and broker cleanup. The
// duplicated PHP pidfd remains valid after the DAP broker retires its session.
func (p *phpHTTPDebugTarget) pinServiceForDetach() (phpHTTPServiceUnit, int, int, string, int, error) {
	if p == nil {
		return phpHTTPServiceUnit{}, 0, 0, "", -1, errors.New("PHP HTTP debug target is missing")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.service == nil || p.phpPin == nil {
		return phpHTTPServiceUnit{}, 0, 0, "", -1, errors.New("PHP HTTP original service/pidfd already retired")
	}
	fd, err := unix.Dup(int(p.phpPin.Fd()))
	if err != nil {
		return phpHTTPServiceUnit{}, 0, 0, "", -1, err
	}
	return p.service.unit, p.phpPID, p.port, p.phpExe, fd, nil
}

func (p *phpHTTPDebugTarget) serviceJobID() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.service == nil {
		return ""
	}
	return p.service.unit.id
}

// The socket identity/trigger are predeclared in the already-running
// authorized Job command; the debugger never creates or starts the service.
func (s *Server) pinPHPHTTPServiceTarget(ctx context.Context, req Request, workspace, php, module string) (*phpHTTPDebugTarget, error) {
	if !validPHPHTTPTrigger(req.DebugHTTPTrigger) || req.DebugHTTPPort < 1024 || req.DebugHTTPPort > 65535 {
		return nil, errors.New("PHP HTTP requires one 32-hex trigger and an unprivileged IPv4 loopback port")
	}
	socket, err := phpHTTPClientSocket(req.DebugHTTPTrigger)
	if err != nil {
		return nil, err
	}
	pin, err := s.pinAuthorizedPHPHTTPService(ctx, req, workspace)
	if err != nil {
		return nil, err
	}
	p := &phpHTTPDebugTarget{service: pin, phpExe: php, module: module, workerUser: s.cfg.WorkerUser,
		workspace: workspace, program: req.DebugProgram, port: req.DebugHTTPPort,
		trigger: req.DebugHTTPTrigger, socket: socket}
	if err := pinPHPHTTPProcess(p, s.workerUID); err != nil {
		p.close()
		return nil, err
	}
	if err := p.verifyService(ctx, s.cfg.WorkerUser); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func verifyPHPHTTPServiceAfterDetach(ctx context.Context, unit phpHTTPServiceUnit, user string, phpPID, phpFD int, phpExe string, port int) error {
	if unit.id == "" || phpPID <= 1 || phpFD < 0 || phpExe == "" {
		return errors.New("PHP HTTP original service or process pin was not retained for detach")
	}
	if err := unix.PidfdSendSignal(phpFD, 0, nil, 0); err != nil {
		return errors.New("PHP HTTP original process did not survive debugger detachment")
	}
	current, err := readPHPHTTPServiceUnit(ctx, unit.id, user)
	if err != nil || current != unit {
		return errors.New("PHP HTTP original unit stopped, restarted or drifted after detachment")
	}
	actual, err := os.Stat(fmt.Sprintf("/proc/%d/exe", phpPID))
	want, trustErr := os.Stat(phpExe)
	if err != nil || trustErr != nil || !os.SameFile(actual, want) {
		return errors.New("PHP HTTP executable no longer has the original interpreter identity")
	}
	group, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", phpPID))
	if err != nil || strings.TrimSpace(string(group)) != "0::"+unit.cgroup {
		return errors.New("PHP HTTP process left the original protected service cgroup")
	}
	if !phpHTTPProcessOwnsLoopbackListener(phpPID, port) {
		return errors.New("PHP HTTP original PHP process no longer owns the loopback listener")
	}
	if unix.PidfdSendSignal(phpFD, 0, nil, 0) != nil {
		return errors.New("PHP HTTP original pinned process exited during post-detach verification")
	}
	return nil
}

func processOwnedByUID(info os.FileInfo, uid uint32) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uid
}
