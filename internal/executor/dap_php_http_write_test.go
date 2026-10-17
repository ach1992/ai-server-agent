package executor

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPHPHTTPProvenanceWriteStopsAfterTerminalDBGpFailure(t *testing.T) {
	proxy := &phpDBGpProxy{ready: make(chan struct{})}
	proxy.settled(errors.New("php_http_dbgp_peer_not_authorized_http_transaction"), false)
	client, receiver := net.Pipe()
	defer client.Close()
	defer receiver.Close()
	conn := &phpHTTPProvenanceWriteConn{Conn: client, proxy: proxy}
	n, err := conn.Write([]byte("GET /index.php HTTP/1.1\r\n\r\n"))
	if n != 0 || !errors.Is(err, errPHPHTTPProvenanceBeforeSend) ||
		!strings.Contains(err.Error(), "php_http_dbgp_peer_not_authorized_http_transaction") {
		t.Fatalf("terminal peer rejection did not block HTTP request bytes: bytes=%d err=%v", n, err)
	}
	if err := receiver.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := receiver.Read(b[:]); err == nil {
		t.Fatal("HTTP request bytes arrived despite terminal DBGp failure")
	} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatalf("expected an empty socket after denied write, got: %v", err)
	}
}

func TestPHPHTTPProvenanceWriteAllowsHealthyDBGpBroker(t *testing.T) {
	proxy := &phpDBGpProxy{ready: make(chan struct{})}
	client, receiver := net.Pipe()
	defer client.Close()
	defer receiver.Close()
	conn := &phpHTTPProvenanceWriteConn{Conn: client, proxy: proxy}
	const data = "GET /index.php HTTP/1.1\r\n\r\n"
	result := make(chan error, 1)
	go func() {
		n, err := conn.Write([]byte(data))
		if err == nil && n != len(data) {
			err = errors.New("short write")
		}
		result <- err
	}()
	b := make([]byte, len(data))
	if _, err := receiver.Read(b); err != nil || string(b) != data {
		t.Fatalf("healthy broker blocked the authorized HTTP write: %q %v", b, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
