package executor

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const phpDBGpInitMaxBytes = 16 << 10

// Xdebug is the DBGp client; the pinned Node DAP adapter is its server.
// A secret pathname or shared aiworker UID is NOT authority. The executor
// alone brokers one AF_UNIX DBGp connection, proving kernel peer UID/PID,
// private cgroup membership, exact PHP executable inode and DBGp init source
// before forwarding any bytes to the ORIGINAL pidfd-pinned Node adapter.
// Neither side can provide a remote TCP endpoint, arbitrary command or scope.
type phpDBGpProxy struct {
	mu          sync.Mutex
	dir         string
	clientPath  string
	adapterPath string
	listener    *net.UnixListener
	client      *net.UnixConn
	adapter     *net.UnixConn
	phpPin      *os.File
	owner       *stdioSession
	containment *dapCgroup
	phpBinary   string
	program     string
	workerUID   uint32
	http        *phpHTTPDebugTarget // service-owned target: never belongs to DAP cgroup
	clientInode os.FileInfo         // prevent deleting a replacement worker-owned socket
	cancel      context.CancelFunc
	context     context.Context
	started     bool
	verified    bool
	failure     error
	ready       chan struct{}
	done        chan struct{}
	once        sync.Once
}

func createPHPDBGpProxy(uid, gid uint32, phpBinary, program string) (*phpDBGpProxy, error) {
	if os.Geteuid() != 0 || uid == 0 || phpBinary == "" || !filepath.IsAbs(program) {
		return nil, errors.New("php_dap_untrusted_listener_identity")
	}
	dir, err := os.MkdirTemp("/tmp", ".asa-php-dap-")
	if err != nil {
		return nil, err
	}
	bad := func(err error) (*phpDBGpProxy, error) { _ = os.RemoveAll(dir); return nil, err }
	if err = os.Chown(dir, int(uid), int(gid)); err != nil {
		return bad(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return bad(err)
	}
	clientPath, adapterPath := filepath.Join(dir, "dbgp.sock"), filepath.Join(dir, "adapter.sock")
	if len(clientPath) > terminalMaxUnixSocketPath || len(adapterPath) > terminalMaxUnixSocketPath {
		return bad(errors.New("php_dap_socket_path_too_long"))
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: clientPath, Net: "unix"})
	if err != nil {
		return bad(err)
	}
	if err = os.Chown(clientPath, int(uid), int(gid)); err == nil {
		err = os.Chmod(clientPath, 0600)
	}
	if err != nil {
		_ = listener.Close()
		return bad(err)
	}
	return &phpDBGpProxy{dir: dir, clientPath: clientPath, adapterPath: adapterPath,
		listener: listener, phpBinary: phpBinary, program: program, workerUID: uid,
		ready: make(chan struct{}), done: make(chan struct{})}, nil
}

func (p *phpDBGpProxy) start(owner *stdioSession, group *dapCgroup) {
	p.mu.Lock()
	p.owner, p.containment = owner, group
	p.context, p.cancel = context.WithCancel(context.Background())
	p.started = true
	p.mu.Unlock()
	go p.run()
}

func (p *phpDBGpProxy) settled(err error, proven bool) {
	p.mu.Lock()
	if err != nil && p.failure == nil {
		p.failure = err
	}
	if proven {
		p.verified = true
	}
	p.mu.Unlock()
	p.once.Do(func() { close(p.ready) })
}

func (p *phpDBGpProxy) await(ctx context.Context) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return p.failure
	}
	if !p.verified {
		return errors.New("php_dap_provenance_unresolved")
	}
	return nil
}

// Calling shutdown from the session broker (even on unexpected adapter exit)
// closes all waiters without taking its e.mu/d.mu locks or waiting for a
// goroutine which may be concurrently inspecting process metadata.
func (p *phpDBGpProxy) shutdown() {
	p.mu.Lock()
	cancel, listener, client, adapter, pin := p.cancel, p.listener, p.client, p.adapter, p.phpPin
	p.phpPin = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if listener != nil {
		_ = listener.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if adapter != nil {
		_ = adapter.Close()
	}
	if pin != nil {
		_ = pin.Close()
	}
}

// Called only after pidfd/cgroup cleanup is verified by the shared broker.
// Unexpected contents are NOT recursively deleted; retain the authenticated
// session for explicit reconciliation instead of hiding resource uncertainty.
func (p *phpDBGpProxy) removeAfterStop() error {
	p.shutdown()
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if started {
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			return errors.New("php_dap_proxy_shutdown_unverified")
		}
	}
	for _, file := range []string{p.clientPath, p.adapterPath} {
		if file == p.clientPath && p.http != nil {
			current, err := os.Lstat(file)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("php_http_socket_identity_unavailable: %w", err)
			}
			if err == nil && (p.clientInode == nil || current.Mode()&os.ModeSocket == 0 || !socketRootOwned(current) || !os.SameFile(current, p.clientInode)) {
				return errors.New("php_http_socket_replaced_during_debugging")
			}
		}
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("php_dap_socket_cleanup_unknown: %w", err)
		}
	}
	if err := os.Remove(p.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("php_dap_private_dir_cleanup_unknown: %w", err)
	}
	if p.http != nil {
		p.http.close() // close read-only PID pins; NEVER signal service/job/PHP
	}
	return nil
}

func phpDBGpPeerCredentials(conn *net.UnixConn) (*unix.Ucred, error) {
	if conn == nil {
		return nil, errors.New("no PHP Unix peer")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var sockErr error
	err = raw.Control(func(fd uintptr) { cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err != nil {
		return nil, err
	}
	if sockErr != nil {
		return nil, sockErr
	}
	if cred == nil || cred.Pid <= 0 {
		return nil, errors.New("unverified PHP peer")
	}
	return cred, nil
}

// Consume one bounded original DBGp init frame without buffered prefetch. Any
// attacker-controlled bytes beyond that frame stay on the original verified
// socket and cannot be mistaken for the documented PHP process identity.
func phpDBGpReadInit(conn *net.UnixConn) ([]byte, string, string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, "", "", err
	}
	defer conn.SetReadDeadline(time.Time{})
	var header []byte
	var byte1 [1]byte
	for len(header) <= 10 {
		if _, err := io.ReadFull(conn, byte1[:]); err != nil {
			return nil, "", "", err
		}
		if byte1[0] == 0 {
			break
		}
		if byte1[0] < '0' || byte1[0] > '9' {
			return nil, "", "", errors.New("invalid DBGp length prefix")
		}
		header = append(header, byte1[0])
	}
	if len(header) == 0 || len(header) > 10 || byte1[0] != 0 {
		return nil, "", "", errors.New("invalid DBGp prefix bounds")
	}
	n, err := strconv.Atoi(string(header))
	if err != nil || n <= 0 || n > phpDBGpInitMaxBytes {
		return nil, "", "", errors.New("oversized DBGp init")
	}
	content := make([]byte, n+1)
	if _, err := io.ReadFull(conn, content); err != nil {
		return nil, "", "", err
	}
	if content[n] != 0 {
		return nil, "", "", errors.New("missing DBGp terminator")
	}
	var init struct {
		XMLName  xml.Name `xml:"init"`
		AppID    string   `xml:"appid,attr"`
		FileURI  string   `xml:"fileuri,attr"`
		Language string   `xml:"language,attr"`
		Protocol string   `xml:"protocol_version,attr"`
	}
	// Pinned Xdebug 3.4 emits a real DBGp init with
	// <?xml version="1.0" encoding="iso-8859-1"?>. xml.Unmarshal
	// silently accepts our UTF-8 mock frames but refuses that real prolog.
	// Decode only the pinned, bounded Latin-1 variant, never arbitrary
	// declared encodings or external fetches; retain the raw original
	// frame for forwarding to the adapter without rewriting bytes.
	decoder := xml.NewDecoder(bytes.NewReader(content[:n]))
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if !strings.EqualFold(label, "iso-8859-1") {
			return nil, errors.New("unsupported DBGp XML charset")
		}
		raw, err := io.ReadAll(io.LimitReader(input, phpDBGpInitMaxBytes+1))
		if err != nil || len(raw) > phpDBGpInitMaxBytes {
			return nil, errors.New("DBGp XML charset input over budget")
		}
		var unicode strings.Builder
		unicode.Grow(len(raw) * 2)
		for _, b := range raw {
			unicode.WriteRune(rune(b))
		}
		return strings.NewReader(unicode.String()), nil
	}
	if err := decoder.Decode(&init); err != nil || init.XMLName.Local != "init" ||
		init.XMLName.Space != "urn:debugger_protocol_v1" || init.Language != "PHP" || init.Protocol != "1.0" {
		return nil, "", "", errors.New("untrusted DBGp init identity")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, "", "", errors.New("unexpected trailing DBGp XML content")
	}
	frame := make([]byte, 0, len(header)+1+len(content))
	frame = append(frame, header...)
	frame = append(frame, 0)
	frame = append(frame, content...)
	return frame, init.AppID, init.FileURI, nil
}

func (p *phpDBGpProxy) verifyOriginalPHP(conn *net.UnixConn) (*os.File, []byte, error) {
	cred, err := phpDBGpPeerCredentials(conn)
	if err != nil {
		return nil, nil, err
	}
	if p.http != nil {
		return p.verifyOriginalPHPHTTP(conn, cred)
	}
	if cred.Uid != p.workerUID || p.containment == nil || !p.containment.containsPID(int(cred.Pid)) {
		return nil, nil, errors.New("php_dap_peer_outside_private_containment")
	}
	// The accepted CLI adapter invokes PHP through an ordinary direct
	// childProcess.spawn. Do not allow a process merely sharing its UID
	// and cgroup to impersonate the exact session-owned PHP target.
	if p.owner == nil {
		return nil, nil, errors.New("php_dap_adapter_provenance_unavailable")
	}
	p.owner.mu.Lock()
	adapterPID := p.owner.pid
	adapterPin := p.owner.pidPin
	adapterLive := adapterPID > 0 && adapterPin != nil && unix.PidfdSendSignal(int(adapterPin.Fd()), 0, nil, 0) == nil
	p.owner.mu.Unlock()
	if !adapterLive || adapterPID == int(cred.Pid) {
		return nil, nil, errors.New("php_dap_original_adapter_not_live")
	}
	status, statusErr := os.ReadFile(fmt.Sprintf("/proc/%d/status", cred.Pid))
	if statusErr != nil {
		return nil, nil, errors.New("php_dap_peer_parent_unavailable")
	}
	parentPID := -1
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			if fields := strings.Fields(line); len(fields) == 2 {
				parentPID, _ = strconv.Atoi(fields[1])
			}
			break
		}
	}
	if parentPID != adapterPID {
		return nil, nil, errors.New("php_dap_peer_not_original_adapter_child")
	}
	fd, err := unix.PidfdOpen(int(cred.Pid), 0)
	if err != nil {
		return nil, nil, err
	}
	pin := os.NewFile(uintptr(fd), "original-xdebug-php-client-pidfd")
	fail := func(e error) (*os.File, []byte, error) { _ = pin.Close(); return nil, nil, e }
	if unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
		return fail(errors.New("php_dap_peer_already_dead"))
	}
	actual, err := os.Stat(fmt.Sprintf("/proc/%d/exe", cred.Pid))
	if err != nil {
		return fail(err)
	}
	trusted, err := os.Stat(p.phpBinary)
	if err != nil || !os.SameFile(actual, trusted) {
		return fail(errors.New("php_dap_unverified_interpreter_inode"))
	}
	frame, appid, fileuri, err := phpDBGpReadInit(conn)
	if err != nil {
		return fail(err)
	}
	pid, err := strconv.Atoi(appid)
	if err != nil || pid != int(cred.Pid) {
		return fail(errors.New("php_dap_source_pid_mismatch"))
	}
	uri, err := url.Parse(fileuri)
	if err != nil || uri.Scheme != "file" || uri.Host != "" || uri.RawQuery != "" || uri.Fragment != "" ||
		uri.Path != p.program {
		return fail(errors.New("php_dap_source_path_mismatch"))
	}
	if !p.containment.containsPID(pid) || unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
		return fail(errors.New("php_dap_peer_provenance_changed"))
	}
	return pin, frame, nil
}

func (p *phpDBGpProxy) run() {
	defer close(p.done)
	client, err := p.listener.AcceptUnix()
	if err != nil {
		p.settled(errors.New("php_dap_client_connection_unavailable"), false)
		return
	}
	p.mu.Lock()
	p.client = client
	p.mu.Unlock()
	defer client.Close()
	_ = p.listener.Close() // precisely one DBGp connection per CLI target
	pin, frame, err := p.verifyOriginalPHP(client)
	if err != nil {
		p.settled(err, false)
		return
	}
	p.mu.Lock()
	p.phpPin = pin
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(p.context, 12*time.Second)
	defer cancel()
	server, err := waitPinnedNodeDAPPeer(ctx, p.owner, p.workerUID, p.adapterPath)
	if err != nil {
		p.settled(errors.New("php_dap_original_adapter_peer_unavailable"), false)
		return
	}
	p.mu.Lock()
	p.adapter = server
	p.mu.Unlock()
	defer server.Close()
	if err := server.SetWriteDeadline(time.Now().Add(4 * time.Second)); err != nil {
		p.settled(err, false)
		return
	}
	written, err := server.Write(frame)
	if err != nil || written != len(frame) {
		if err == nil {
			err = io.ErrShortWrite
		}
		p.settled(err, false)
		return
	}
	_ = server.SetWriteDeadline(time.Time{})
	p.settled(nil, true)
	other := make(chan struct{}, 1)
	go func() { _, _ = io.Copy(client, server); other <- struct{}{} }()
	_, _ = io.Copy(server, client)
	_ = server.Close()
	_ = client.Close()
	select {
	case <-other:
	case <-time.After(2 * time.Second):
	}
}
