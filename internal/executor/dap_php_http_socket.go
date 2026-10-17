package executor

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The outbound TCP socket belongs to the single, explicit Agent HTTP GET.
// Keep an independent fd pin until DAP cleanup; neither the query trigger
// nor the shared PHP process are a request-specific authority.
func (t *phpHTTPDebugTarget) bindAuthorizedRequestSocket(conn net.Conn) error {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return errors.New("php_http_authorized_request_not_tcp")
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return fmt.Errorf("php_http_authorized_socket_unavailable: %w", err)
	}
	fd := -1
	var dupErr error
	if err = raw.Control(func(original uintptr) {
		fd, dupErr = unix.Dup(int(original))
	}); err != nil {
		return err
	}
	if dupErr != nil || fd < 0 {
		return fmt.Errorf("php_http_authorized_socket_pin_failed: %v", dupErr)
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	local, err := unix.Getsockname(fd)
	if err != nil {
		return err
	}
	remote, err := unix.Getpeername(fd)
	if err != nil {
		return err
	}
	l, lok := local.(*unix.SockaddrInet4)
	r, rok := remote.(*unix.SockaddrInet4)
	if !lok || !rok || l.Port < 1024 || l.Port > 65535 || r.Port != t.port ||
		l.Addr != ([4]byte{127, 0, 0, 1}) || r.Addr != ([4]byte{127, 0, 0, 1}) {
		return errors.New("php_http_authorized_request_tcp_tuple_invalid")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.requestStarted || t.requestDone || t.requestSocket != nil || t.service == nil {
		return errors.New("php_http_authorized_request_socket_state_invalid")
	}
	t.requestSocket = os.NewFile(uintptr(fd), "authorized-php-http-client-tcp-socket")
	fd = -1
	return nil
}

// A paused DBGp init is accepted only if PHP holds the exact accepted TCP
// socket paired with the one pinned Agent-originated outbound connection.
// This proves the individual HTTP request at the kernel socket layer, not
// just that an unrelated request was served by the same trusted PHP PID.
func (t *phpHTTPDebugTarget) verifyAuthorizedRequestSocket() error {
	t.mu.Lock()
	if !t.requestStarted || t.requestDone || t.requestSocket == nil {
		t.mu.Unlock()
		return errors.New("php_http_dbgp_no_active_authorized_request_socket")
	}
	fd, err := unix.Dup(int(t.requestSocket.Fd()))
	pid, port := t.phpPID, t.port
	t.mu.Unlock()
	if err != nil {
		return fmt.Errorf("php_http_dbgp_authorized_socket_pin_unavailable: %w", err)
	}
	defer unix.Close(fd)
	local, err := unix.Getsockname(fd)
	if err != nil {
		return fmt.Errorf("php_http_dbgp_authorized_tcp_socket_unavailable: %w", err)
	}
	remote, err := unix.Getpeername(fd)
	if err != nil {
		return fmt.Errorf("php_http_dbgp_authorized_tcp_peer_unavailable: %w", err)
	}
	l, lok := local.(*unix.SockaddrInet4)
	r, rok := remote.(*unix.SockaddrInet4)
	if !lok || !rok || l.Addr != ([4]byte{127, 0, 0, 1}) ||
		r.Addr != ([4]byte{127, 0, 0, 1}) || r.Port != port ||
		!phpHTTPProcessOwnsAcceptedRequest(pid, port, l.Port) {
		return errors.New("php_http_dbgp_peer_not_authorized_http_transaction")
	}
	return nil
}

// /proc/net/tcp is a *namespace* inventory. Never trust it by itself:
// require the exact 127.0.0.1:service <- 127.0.0.1:agent connection's
// ESTABLISHED inode to be open by the pinned original PHP PID.
func phpHTTPProcessOwnsAcceptedRequest(pid, serverPort, agentPort int) bool {
	if pid < 2 || serverPort < 1024 || serverPort > 65535 ||
		agentPort < 1024 || agentPort > 65535 || serverPort == agentPort {
		return false
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/net/tcp", pid))
	if err != nil {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	_ = file.Close()
	if err != nil || len(b) >= 64<<10 {
		return false
	}
	local := "0100007F:" + fmt.Sprintf("%04X", serverPort)
	remote := "0100007F:" + fmt.Sprintf("%04X", agentPort)
	// Collect every potentially process-owned inbound socket, including
	// CLOSE_WAIT after a contender half-closes its TCP connection. Looking
	// only at ESTABLISHED sockets would wrongly authorize our idle connection
	// while PHP is paused in the contender's Xdebug request. Only inode-zero
	// kernel entries (which no process can hold as an fd) are excluded.
	inbound := make(map[string]bool)
	authorized := ""
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.EqualFold(fields[1], local) || fields[3] == "0A" {
			continue // LISTEN is not an accepted request socket
		}
		if fields[9] == "0" {
			continue // no associated file descriptor (for example TIME_WAIT)
		}
		if _, err := strconv.ParseUint(fields[9], 10, 64); err != nil {
			return false
		}
		inode := "socket:[" + fields[9] + "]"
		if _, duplicate := inbound[inode]; duplicate {
			return false
		}
		inbound[inode] = true
		if fields[3] == "01" && strings.EqualFold(fields[2], remote) {
			if authorized != "" {
				return false
			}
			authorized = inode
		}
	}
	if authorized == "" {
		return false
	}
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil || len(fds) > 256 {
		return false
	}
	heldAuthorized := false
	for _, f := range fds {
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, f.Name()))
		if err != nil || !inbound[link] {
			continue
		}
		if link != authorized {
			return false // another accepted connection may own DBGp init
		}
		heldAuthorized = true
	}
	return heldAuthorized
}
