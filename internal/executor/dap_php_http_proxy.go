package executor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// Only already-running, verified, separately job-owned PHP -S services can
// connect to this one-request listener. The service predeclared its exact
// worker-HOME endpoint and Xdebug trigger at start_job time; the DAP session
// merely binds the endpoint later. No root/remote PHP attach, TCP DBGp or
// service creation occurs here.
func createPHPHTTPDBGpProxy(uid, gid uint32, t *phpHTTPDebugTarget, program string) (*phpDBGpProxy, error) {
	if os.Geteuid() != 0 || uid == 0 || t == nil || t.service == nil ||
		t.phpPin == nil || !filepath.IsAbs(program) || t.socket == "" {
		return nil, errors.New("php_http_dbgp_untrusted_service_identity")
	}
	expected, err := phpHTTPClientSocket(t.trigger)
	if err != nil || expected != t.socket || phpHTTPTrustedSocketParent() != nil {
		return nil, errors.New("php_http_dbgp_root_owned_sticky_socket_parent_required")
	}
	if _, err := os.Lstat(t.socket); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("php_http_dbgp_preexisting_socket_refused")
	}
	dir, err := os.MkdirTemp("/tmp", ".asa-php-dap-")
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*phpDBGpProxy, error) {
		_ = os.Remove(dir)
		return nil, e
	}
	if err := os.Chown(dir, int(uid), int(gid)); err != nil {
		return fail(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fail(err)
	}
	adapterPath := filepath.Join(dir, "adapter.sock")
	if len(adapterPath) > terminalMaxUnixSocketPath || len(t.socket) > terminalMaxUnixSocketPath {
		return fail(errors.New("php_http_dbgp_socket_path_too_long"))
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: t.socket, Net: "unix"})
	if err != nil {
		return fail(err)
	}
	ln.SetUnlinkOnClose(false) // delete only if the original socket inode survives
	// The socket MUST stay owned by root for sticky /tmp unlink protection.
	// Grant aiworker connection rights via GROUP ONLY; never chown the inode
	// to the worker (which would permit pathname replacement).
	info, statErr := os.Lstat(t.socket)
	if statErr != nil || info.Mode()&os.ModeSocket == 0 || !socketRootOwned(info) {
		_ = ln.Close()
		return fail(errors.New("php_http_dbgp_socket_identity_unavailable"))
	}
	if err := os.Chown(t.socket, -1, int(gid)); err == nil {
		err = os.Chmod(t.socket, 0660)
	}
	after, verifyErr := os.Lstat(t.socket)
	if err != nil || verifyErr != nil || !os.SameFile(after, info) || !socketRootOwned(after) || after.Mode().Perm() != 0660 {
		_ = ln.Close()
		// The original root-owned inode remains protected by sticky /tmp.
		if verifyErr == nil && os.SameFile(after, info) {
			_ = os.Remove(t.socket)
		}
		return fail(errors.New("php_http_dbgp_socket_ownership_unverified"))
	}
	return &phpDBGpProxy{
		dir: dir, clientPath: t.socket, adapterPath: adapterPath, listener: ln,
		http: t, clientInode: info, phpBinary: t.phpExe,
		program: program, workerUID: uid, ready: make(chan struct{}), done: make(chan struct{}),
	}, nil
}

// No peer may be admitted merely for knowing a socket pathname or matching
// aiworker's UID. SO_PEERCRED PID is required to equal the original pinned
// HTTP server process, and DBGp appid/source must agree with that kernel PID
// and the one exact version-pinned .php source selected by the user.
func (p *phpDBGpProxy) verifyOriginalPHPHTTP(conn *net.UnixConn, cred *unix.Ucred) (*os.File, []byte, error) {
	t := p.http
	if t == nil || cred == nil || cred.Uid != p.workerUID || int(cred.Pid) != t.phpPID {
		return nil, nil, errors.New("php_http_dbgp_peer_is_not_original_service_php")
	}
	t.mu.Lock()
	started := t.requestStarted
	t.mu.Unlock()
	if !started {
		return nil, nil, errors.New("php_http_dbgp_unsolicited_before_authorized_request")
	}
	ctx, cancel := context.WithTimeout(p.context, jobSystemdProbeTimeout)
	defer cancel()
	if err := t.verifyService(ctx, t.workerUser); err != nil {
		return nil, nil, fmt.Errorf("php_http_dbgp_service_provenance_changed: %w", err)
	}
	// Dup the pinned original server's pidfd. The debuggee remains owned by
	// the independent Service Job and must NOT be added to the DAP cgroup.
	fd, err := unix.Dup(int(t.phpPin.Fd()))
	if err != nil {
		return nil, nil, err
	}
	pin := os.NewFile(uintptr(fd), "php-http-original-service-php-pidfd")
	fail := func(e error) (*os.File, []byte, error) { _ = pin.Close(); return nil, nil, e }
	frame, appid, fileuri, err := phpDBGpReadInit(conn)
	if err != nil {
		return fail(err)
	}
	if appid != strconv.Itoa(int(cred.Pid)) {
		return fail(errors.New("php_http_dbgp_peer_source_pid_mismatch"))
	}
	parsed, err := url.Parse(fileuri)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != p.program {
		return fail(errors.New("php_http_dbgp_source_path_mismatch"))
	}
	if err := t.verifyPHPIdentity(); err != nil ||
		unix.PidfdSendSignal(int(pin.Fd()), 0, nil, 0) != nil {
		return fail(errors.New("php_http_dbgp_peer_identity_drift"))
	}
	return pin, frame, nil
}

func socketRootOwned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}
