package executor

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func phpDBGpTestInitFrame(appid, fileURI string) []byte {
	body, _ := xml.Marshal(struct {
		XMLName  xml.Name `xml:"init"`
		Xmlns    string   `xml:"xmlns,attr"`
		Protocol string   `xml:"protocol_version,attr"`
		Language string   `xml:"language,attr"`
		AppID    string   `xml:"appid,attr"`
		URI      string   `xml:"fileuri,attr"`
	}{XMLName: xml.Name{Local: "init"}, Xmlns: "urn:debugger_protocol_v1", Protocol: "1.0", Language: "PHP", AppID: appid, URI: fileURI})
	return append(append([]byte(strconv.Itoa(len(body))+"\x00"), body...), 0)
}

func phpDBGpFixtureConn(t *testing.T, payload []byte) *net.UnixConn {
	t.Helper()
	name := filepath.Join(t.TempDir(), "dbgp.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: name, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		peer, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: name, Net: "unix"})
		if err != nil {
			done <- err
			return
		}
		defer peer.Close()
		_, err = peer.Write(payload)
		done <- err
	}()
	accepted, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accepted.Close() })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DBGp write fixture timed out")
	}
	return accepted
}

func TestPHPDBGpInitOneFrameBoundedAndIdentity(t *testing.T) {
	valid := phpDBGpTestInitFrame("47", "file:///opt/example.php")
	for _, tc := range []struct {
		name string
		wire []byte
		ok   bool
	}{
		{"valid", valid, true},
		{"bad_prefix", []byte("X\x00hello\x00"), false},
		{"oversized", []byte("99999999\x00"), false},
		{"empty_length", []byte("0\x00"), false},
		{"missing_terminator", valid[:len(valid)-1], false},
		{"invalid_xml", []byte("4\x00test\x00"), false},
		{"trailing_prefetch_guard", append(append([]byte(nil), valid...), []byte("ORIGINAL_NEXT_BYTES")...), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := phpDBGpFixtureConn(t, tc.wire)
			frame, id, uri, err := phpDBGpReadInit(peer)
			if (err == nil) != tc.ok {
				t.Fatalf("bounded DBGp verification=%t, err=%v", tc.ok, err)
			}
			if tc.ok && (id != "47" || uri != "file:///opt/example.php" || string(frame) != string(valid)) {
				t.Fatalf("DBGp original init changed: id=%s uri=%s", id, uri)
			}
			if tc.name == "trailing_prefetch_guard" {
				tail := make([]byte, len("ORIGINAL_NEXT_BYTES"))
				if _, err := io.ReadFull(peer, tail); err != nil || string(tail) != "ORIGINAL_NEXT_BYTES" {
					t.Fatalf("DBGp parser consumed unrelated subsequent bytes: %q %v", tail, err)
				}
			}
		})
	}
}

func TestPHPDBGpVerifierRejectsUncontainedPeerWithoutForward(t *testing.T) {
	peer := phpDBGpFixtureConn(t, phpDBGpTestInitFrame(strconv.Itoa(os.Getpid()), "file:///test.php"))
	p := &phpDBGpProxy{workerUID: uint32(os.Getuid()), phpBinary: "/usr/bin/php8.4", program: "/test.php"}
	if pin, _, err := p.verifyOriginalPHP(peer); err == nil {
		if pin != nil {
			_ = pin.Close()
		}
		t.Fatal("a same-UID peer outside a private root-owned cgroup was admitted")
	}
}

func TestPHPDAPStatusAndUnsupportedAttach(t *testing.T) {
	s, req, _ := testStdioBroker(t)
	req.Action, req.Language = "debug_adapter_status", "php"
	result := s.debugAction(t.Context(), req)
	if !result.OK || !strings.Contains(result.Output, `"adapter":"php/xdebug"`) ||
		!strings.Contains(result.Output, `"http_request_mode":"not_yet_verified"`) ||
		!strings.Contains(result.Output, `"attach_supported":false`) {
		t.Fatalf("incorrect PHP debugger readiness: %+v", result)
	}
	req.Action, req.DebugMode = "debug_launch", "http"
	req.DebugProgram, req.FileVersion = "main.php", "exact-version"
	if got := s.debugAction(t.Context(), req); got.OK || got.ErrorCode != "php_dap_unsupported_mode" || got.SessionID != "" {
		t.Fatalf("unsupported PHP HTTP takeover created a session: %+v", got)
	}
	req.DebugMode = "attach"
	if got := s.debugAction(t.Context(), req); got.OK || got.ErrorCode != "php_dap_unsupported_mode" {
		t.Fatalf("PHP remote attach admitted: %+v", got)
	}
	s.sessions.mu.Lock()
	count := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if count != 0 {
		t.Fatalf("unsafe PHP created %d sessions", count)
	}
}

func TestPHPDAPUncontainedWorkerFailsBeforeExecutingScript(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires unprivileged worker")
	}
	requireWorkerLandlockV2(t)
	s, owner, workspace := testStdioBroker(t)
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go compiler unavailable for real versioned helper")
	}
	helper := filepath.Join(t.TempDir(), "asa-php-file-helper")
	builder := exec.Command(goTool, "build", "-o", helper, "./cmd/ai-server-agent")
	builder.Dir = filepath.Join("..", "..")
	if out, err := builder.CombinedOutput(); err != nil {
		t.Fatalf("worker helper build: %v %s", err, out)
	}
	s.workspaceHelperBinary = helper
	source := filepath.Join(workspace, "main.php")
	if err := os.WriteFile(source, []byte("<?php file_put_contents('/tmp/forbidden_outside', '0');\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stat := s.workerWorkspaceFile(t.Context(), Request{Action: "workspace_stat", Workspace: workspace, Path: "main.php"})
	if !stat.OK {
		t.Fatalf("real versioned PHP workspace stat: %+v", stat)
	}
	owner.Action, owner.Language, owner.DebugMode = "debug_launch", "php", "cli"
	owner.DebugProgram, owner.FileVersion = "main.php", stat.FileVersion
	got := s.debugAction(t.Context(), owner)
	if got.OK || got.ErrorCode != "dap_containment_unavailable" || got.SessionID != "" {
		t.Fatalf("uncontained PHP debug launched: %+v", got)
	}
	if err := os.WriteFile(source, []byte("<?php echo 9;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := s.debugAction(t.Context(), owner)
	if old.OK || old.ErrorCode != "debug_program_changed" {
		t.Fatalf("PHP debug stale source admitted: %+v", old)
	}
	s.sessions.mu.Lock()
	count := len(s.sessions.sessions)
	s.sessions.mu.Unlock()
	if count != 0 {
		t.Fatalf("PHP failed launch left %d sessions", count)
	}
}

func TestPHPDAPProxyAwaitRejectsFailedIdentity(t *testing.T) {
	p := &phpDBGpProxy{ready: make(chan struct{})}
	p.settled(errors.New("wrong peer"), false)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := p.await(ctx); err == nil || err.Error() != "wrong peer" {
		t.Fatalf("invalid PHP client unexpectedly ready: %v", err)
	}
}

func TestPHPDBGpPrivateProxyShutdownReclaimsOnlyItsOwnSockets(t *testing.T) {
	dir := t.TempDir()
	clientPath := filepath.Join(dir, "dbgp.sock")
	adapterPath := filepath.Join(dir, "adapter.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: clientPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	p := &phpDBGpProxy{dir: dir, clientPath: clientPath, adapterPath: adapterPath,
		listener: listener, ready: make(chan struct{}), done: make(chan struct{}),
		phpBinary: "/usr/bin/php8.4", program: "/test/main.php"}
	// A private socket is NOT an authorization boundary: with no verified
	// original adapter/cgroup, the peer is always refused before forwarding.
	p.start(nil, nil)
	peer, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: clientPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	// With no adapter/cgroup provenance the proxy may reject and close
	// the accepted socket before the client finishes writing. EPIPE or
	// ECONNRESET is successful fail-closed rejection, not a test failure.
	if _, err := peer.Write(phpDBGpTestInitFrame(strconv.Itoa(os.Getpid()), "file:///test/main.php")); err != nil &&
		!errors.Is(err, syscall.EPIPE) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("unexpected unverified DBGp write error: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := p.await(ctx); err == nil {
		t.Fatal("unverified DBGp client accepted on private listener")
	}
	_ = peer.Close()
	if err := p.removeAfterStop(); err != nil {
		t.Fatalf("private proxy cleanup uncertain: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private proxy path remains: %v", err)
	}
}
