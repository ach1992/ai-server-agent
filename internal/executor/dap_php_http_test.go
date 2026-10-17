package executor

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPHPHTTPFixedRequestAndEndpointContract(t *testing.T) {
	const trigger = "0123456789abcdef0123456789abcdef"
	good, err := phpHTTPClientSocket(trigger)
	if err != nil || good != "/tmp/.asa-php-http-"+trigger+".sock" {
		t.Fatalf("private endpoint not deterministically derived from root-owned sticky /tmp and trigger: %q %v", good, err)
	}
	for _, token := range []string{"", "abc", strings.Repeat("g", 32), strings.Repeat("A", 32),
		strings.Repeat("0", 31) + "/", strings.Repeat("a", 33)} {
		if _, err := phpHTTPClientSocket(token); err == nil {
			t.Fatalf("unsafe trigger admitted: %q", token)
		}
	}
	php, module, workspace, socket, port := "/usr/bin/php8.4", "/opt/xdebug.so", "/tmp/site", good, 45091
	want := expectedPHPHTTPArgv(php, module, workspace, port, trigger, socket)
	if !phpHTTPArgsMatch(want, php, module, workspace, port, trigger, socket) {
		t.Fatal("exact documented interpreter command rejected")
	}
	for i := range want {
		modified := append([]string{}, want...)
		modified[i] += "x"
		if phpHTTPArgsMatch(modified, php, module, workspace, port, trigger, socket) {
			t.Fatalf("altered original service argument %d accepted", i)
		}
	}
	if phpHTTPArgsMatch(want, php, module, workspace, 45092, trigger, socket) {
		t.Fatal("different HTTP port accepted")
	}
	if phpHTTPArgsMatch(want, php, module, workspace, port, strings.Repeat("a", 32), socket) {
		t.Fatal("different per-request Xdebug trigger accepted")
	}
	if phpHTTPArgsMatch(want, php, module, workspace, port, trigger, "/tmp/alien.sock") {
		t.Fatal("different socket path accepted")
	}
}

func TestPHPHTTPLoopbackListenerOriginalProcessEvidence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run as ordinary worker; this never requires root privileges or systemd")
	}
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("optional PHP interpreter absent")
	}
	php, err = filepath.EvalSymlinks(php)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.php"), []byte("<?php echo 42;"), 0600); err != nil {
		t.Fatal(err)
	}
	sock, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	port := sock.Addr().(*net.TCPAddr).Port
	_ = sock.Close()
	trigger := "0123456789abcdef0123456789abcdef"
	// A synthetic nonexistent module is deliberately not installed. PHP -S
	// still starts and this test proves argv/listener PID identity only;
	// actual Xdebug HTTP protocol requires the separate real root fixture.
	module := "/nonexistent/asa-no-xdebug-module.so"
	socket := filepath.Join(dir, "fake-dbgp.sock")
	argv := expectedPHPHTTPArgv(php, module, dir, port, trigger, socket)
	cmd := exec.Command(php, argv[1:]...)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Skipf("PHP -S optional local fixture unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ready := false
	for until := time.Now().Add(3 * time.Second); time.Now().Before(until); {
		if phpHTTPProcessOwnsLoopbackListener(cmd.Process.Pid, port) {
			ready = true
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !ready {
		t.Skip("optional PHP -S did not start with expected local listener")
	}
	args, err := boundedPHPHTTPArgv(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if !phpHTTPArgsMatch(args, php, module, dir, port, trigger, socket) {
		t.Fatalf("real interpreter argv identity did not match: %q", args)
	}
	if phpHTTPProcessOwnsLoopbackListener(cmd.Process.Pid, port+1) {
		t.Fatal("original PHP process falsely proved unrelated port ownership")
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		t.Skipf("pidfd not supported by local kernel: %v", err)
	}
	defer unix.Close(fd)
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		t.Fatalf("original PHP server unexpectedly unavailable: %v", err)
	}
}

func TestPHPHTTPUnownedDBGpPeerNeverAuthorized(t *testing.T) {
	p := &phpDBGpProxy{http: &phpHTTPDebugTarget{phpPID: os.Getpid() + 5000},
		workerUID: uint32(os.Getuid())}
	// No socket/frame is needed: false kernel peer identity must reject
	// before any DBGp/XML/source bytes can be forwarded to the adapter.
	_, _, err := p.verifyOriginalPHPHTTP(nil, &unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Getuid())})
	if err == nil {
		t.Fatal("same UID with different PID was treated as original service PHP")
	}
	p.http.phpPID = os.Getpid()
	_, _, err = p.verifyOriginalPHPHTTP(nil, &unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Getuid() + 1)})
	if err == nil {
		t.Fatal("same PID with different UID was treated as original service PHP")
	}
}

func TestPHPHTTPFailsClosedWithoutVerifiedExistingService(t *testing.T) {
	s, req, _ := testStdioBroker(t)
	req.Action, req.Language, req.DebugMode = "debug_launch", "php", "http"
	req.DebugProgram, req.FileVersion = "index.php", "v1"
	req.DebugHTTPPort, req.DebugHTTPTrigger = 49121, "0123456789abcdef0123456789abcdef"
	for name, change := range map[string]func(*Request){
		"missing_service": func(r *Request) { r.JobID = "" },
		"bad_service_id":  func(r *Request) { r.JobID = "other-service" },
		"missing_port":    func(r *Request) { r.DebugHTTPPort = 0 },
		"privileged_port": func(r *Request) { r.DebugHTTPPort = 80 },
		"remote_port":     func(r *Request) { r.DebugHTTPPort = 65536 },
		"invalid_token":   func(r *Request) { r.DebugHTTPTrigger = "not-a-cryptographic-token" },
	} {
		t.Run(name, func(t *testing.T) {
			test := req
			test.JobID = strconv.FormatInt(time.Now().UnixNano(), 10)
			change(&test)
			out := s.debugAction(context.Background(), test)
			if out.OK || out.ErrorCode != "php_http_identity_required" || out.SessionID != "" {
				t.Fatalf("unsafe HTTP request identity accepted: %+v", out)
			}
		})
	}
	req.DebugMode = "remote"
	if out := s.debugAction(context.Background(), req); out.OK || out.ErrorCode != "php_dap_unsupported_mode" {
		t.Fatalf("PHP arbitrary remote attach accepted: %+v", out)
	}
}
