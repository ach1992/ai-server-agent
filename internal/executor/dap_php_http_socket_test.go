package executor

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

// Real in-kernel TCP sockets, not simulated /proc rows: prove that a
// connection to the correct listening port is insufficient unless the
// original PHP process owns the accepted socket for the exact Agent fd.
func TestPHPHTTPAuthorizedTCPTransactionOwnership(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	target := &phpHTTPDebugTarget{
		phpPID: os.Getpid(), port: port, service: &phpHTTPServicePin{},
		requestStarted: true,
	}
	defer target.close()
	if err := target.bindAuthorizedRequestSocket(client); err != nil {
		t.Fatalf("pin local client connection: %v", err)
	}
	if err := target.verifyAuthorizedRequestSocket(); err != nil {
		t.Fatalf("matching original accepted TCP connection rejected: %v", err)
	}
	localPort := client.LocalAddr().(*net.TCPAddr).Port
	if !phpHTTPProcessOwnsAcceptedRequest(os.Getpid(), port, localPort) {
		t.Fatal("kernel accepted socket is not proven by original process inode")
	}
	if phpHTTPProcessOwnsAcceptedRequest(os.Getpid(), port, localPort+1) ||
		phpHTTPProcessOwnsAcceptedRequest(os.Getpid(), port+1, localPort) ||
		phpHTTPProcessOwnsAcceptedRequest(os.Getpid()+999999, port, localPort) {
		t.Fatal("unrelated request socket or process was accepted")
	}
	other, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherAccepted, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer otherAccepted.Close()
	// An unrelated accepted socket owned by the SAME original PHP process
	// makes the active DBGp transaction ambiguous. Reject both source ports.
	if phpHTTPProcessOwnsAcceptedRequest(os.Getpid(), port, other.LocalAddr().(*net.TCPAddr).Port) ||
		phpHTTPProcessOwnsAcceptedRequest(os.Getpid(), port, localPort) {
		t.Fatal("competing accepted localhost request was mistaken for an exclusive Agent transaction")
	}
	if err := target.verifyAuthorizedRequestSocket(); err == nil {
		t.Fatal("DBGP admitted with competing established socket at same PHP listener")
	}
	_ = otherAccepted.Close()
	if err := target.verifyAuthorizedRequestSocket(); err != nil {
		t.Fatalf("exact authorized tuple not recovered after contender disconnected: %v", err)
	}
	target.mu.Lock()
	target.requestDone = true
	target.mu.Unlock()
	if err := target.verifyAuthorizedRequestSocket(); err == nil {
		t.Fatal("completed HTTP request still authorizes future DBGp peers")
	}
}

func TestPHPHTTPNoClientSocketCannotClaimDBGpPeer(t *testing.T) {
	target := &phpHTTPDebugTarget{phpPID: os.Getpid(), port: 49152, requestStarted: true}
	if err := target.verifyAuthorizedRequestSocket(); err == nil ||
		!strings.Contains(err.Error(), "no_active_authorized_request_socket") {
		t.Fatalf("fixed trigger and process PID became individual HTTP authority: %v", err)
	}
}

func TestPHPHTTPTerminalPeerFailureHealthCheck(t *testing.T) {
	p := &phpDBGpProxy{ready: make(chan struct{})}
	if err := p.terminalFailure(); err != nil {
		t.Fatalf("undispatched HTTP session must not wait for DBGp: %v", err)
	}
	const cause = "php_http_dbgp_peer_not_authorized_http_transaction"
	p.settled(errors.New(cause), false)
	if err := p.terminalFailure(); err == nil || !strings.Contains(err.Error(), cause) {
		t.Fatalf("original terminal DBGp provenance failure lost: %v", err)
	}
	target := &phpHTTPDebugTarget{proxy: p}
	snapshot := target.requestSnapshot()
	if snapshot["dbgp_provenance_error"] != cause {
		t.Fatalf("terminal failure not shown in inspectable HTTP status: %+v", snapshot)
	}
}
