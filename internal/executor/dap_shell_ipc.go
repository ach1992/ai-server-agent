package executor

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// A root-created pipe inode retains root ownership when its already-open
// descriptors are inherited by an unprivileged child. /proc/PID/fd reopening
// still performs inode permission checks. However pidfd_getfd can duplicate
// those descriptors without reopening the inode, so Shell DAP also requires
// Yama's PTRACE_MODE_ATTACH restriction against unrelated same-UID peers.
// Do not silently rely on a distro default or modify the host-wide sysctl.
func shellDAPIPCPrerequisite() error {
	if os.Geteuid() != 0 {
		return errors.New("shell_dap_ipc_requires_root_executor")
	}
	data, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
	if err != nil {
		return fmt.Errorf("shell_dap_ipc_ptrace_policy_unavailable: %w", err)
	}
	return validateShellDAPPtraceScope(data)
}

func validateShellDAPPtraceScope(data []byte) error {
	scope, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || scope < 1 || scope > 3 {
		return errors.New("shell_dap_ipc_ptrace_policy_insufficient: Yama ptrace_scope >= 1 required")
	}
	return nil
}

// The four descriptors are created ONLY by the existing root Executor broker.
// Their order in cmd.ExtraFiles is the private adapter's fixed protocol:
//
//	fd3 trace read; fd4 trace write; fd5 command read; fd6 command write.
//
// The worker adapter passes only fd4 and fd5 to its Bash child. All FDs are
// anonymous, root-owned 0600 pipe inodes; none is a pathname or network socket.
type shellDAPIPC struct {
	traceRead    *os.File
	traceWrite   *os.File
	commandRead  *os.File
	commandWrite *os.File
}

func (p *shellDAPIPC) close() {
	if p == nil {
		return
	}
	for _, f := range []*os.File{p.traceRead, p.traceWrite, p.commandRead, p.commandWrite} {
		if f != nil {
			_ = f.Close()
		}
	}
}

func (p *shellDAPIPC) descriptors() []*os.File {
	return []*os.File{p.traceRead, p.traceWrite, p.commandRead, p.commandWrite}
}

func newRootShellDAPIPC() (_ *shellDAPIPC, err error) {
	if err = shellDAPIPCPrerequisite(); err != nil {
		return nil, err
	}
	p := &shellDAPIPC{}
	defer func() {
		if err != nil {
			p.close()
		}
	}()
	p.traceRead, p.traceWrite, err = os.Pipe()
	if err != nil {
		return nil, err
	}
	p.commandRead, p.commandWrite, err = os.Pipe()
	if err != nil {
		return nil, err
	}
	// The installed Executor runs with euid=0 but systemd Group=aiagent.
	// Pipe inodes inherit that effective group unless the original root
	// broker explicitly pins both owners before descriptor inheritance.
	// No child/worker path is allowed to repair or weaken this invariant.
	for _, f := range p.descriptors() {
		if err = unix.Fchown(int(f.Fd()), 0, 0); err != nil {
			return nil, fmt.Errorf("shell_dap_ipc_root_ownership_unavailable: %w", err)
		}
		if err = unix.Fchmod(int(f.Fd()), 0600); err != nil {
			return nil, fmt.Errorf("shell_dap_ipc_pipe_permission_unavailable: %w", err)
		}
	}
	if err = p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// An adapter launched outside the privileged, cgroup-owned broker must fail
// BEFORE accepting any DAP frames. Inheriting arbitrary worker-created pipes,
// even if anonymous, does not provide authenticated session authority.
func inheritRootShellDAPIPC() (_ *shellDAPIPC, err error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("shell_dap_adapter_must_be_unprivileged")
	}
	p := &shellDAPIPC{
		traceRead:    os.NewFile(3, "shell-root-trace-read"),
		traceWrite:   os.NewFile(4, "shell-root-trace-write"),
		commandRead:  os.NewFile(5, "shell-root-command-read"),
		commandWrite: os.NewFile(6, "shell-root-command-write"),
	}
	if err = p.validate(); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func (p *shellDAPIPC) validate() error {
	if p == nil {
		return errors.New("shell_dap_ipc_missing")
	}
	files := p.descriptors()
	var st [4]unix.Stat_t
	for i, f := range files {
		if f == nil {
			return errors.New("shell_dap_ipc_descriptor_missing")
		}
		if err := unix.Fstat(int(f.Fd()), &st[i]); err != nil {
			return fmt.Errorf("shell_dap_ipc_descriptor_unavailable: %w", err)
		}
		if st[i].Mode&unix.S_IFMT != unix.S_IFIFO || st[i].Uid != 0 || st[i].Gid != 0 || st[i].Mode&0777 != 0600 {
			return errors.New("shell_dap_ipc_not_root_owned_0600_pipe")
		}
		flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
		if err != nil {
			return err
		}
		want := unix.O_RDONLY
		if i == 1 || i == 3 {
			want = unix.O_WRONLY
		}
		if flags&unix.O_ACCMODE != want {
			return errors.New("shell_dap_ipc_invalid_descriptor_direction")
		}
	}
	if st[0].Dev != st[1].Dev || st[0].Ino != st[1].Ino ||
		st[2].Dev != st[3].Dev || st[2].Ino != st[3].Ino ||
		st[0].Dev == st[2].Dev && st[0].Ino == st[2].Ino {
		return errors.New("shell_dap_ipc_pipe_identity_mismatch")
	}
	return nil
}
