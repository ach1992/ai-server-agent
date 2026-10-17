package executor

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPHPHTTPOneShotRequestAdmissionConcurrent(t *testing.T) {
	target := &phpHTTPDebugTarget{}
	var accepted atomic.Int32
	const contenders = 64
	var group sync.WaitGroup
	for i := 0; i < contenders; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if target.markRequestDispatched(func() {}) {
				accepted.Add(1)
			}
		}()
	}
	group.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("one PHP HTTP debug session must admit exactly one request, got %d", accepted.Load())
	}
	status := target.requestSnapshot()
	if status["dispatched"] != true || status["completed"] != false {
		t.Fatalf("request admission state lost: %v", status)
	}
	if target.markRequestDispatched(func() {}) {
		t.Fatal("PHP HTTP request incorrectly admitted twice")
	}
}

func TestPHPHTTPDebuggerStopCancelsOnlyClientRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	target := &phpHTTPDebugTarget{requestStarted: true, requestDone: true, cancel: cancel}
	started := time.Now()
	target.finishHTTPRequestAfterDetach()
	if time.Since(started) > 250*time.Millisecond {
		t.Fatal("completed HTTP request unnecessarily delays DAP cleanup")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("HTTP request client cancellation was omitted on debugger detach")
	}
}

func TestPHPHTTPCleanupPreservesReplacedUnixSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dbgp.sock")
	original, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	original.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	const replacement = "unrelated worker data, never delete\n"
	if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	proxy := &phpDBGpProxy{clientPath: path, dir: filepath.Join(dir, "unused-subdir"),
		http: &phpHTTPDebugTarget{}, clientInode: info}
	if err := proxy.removeAfterStop(); err == nil {
		t.Fatal("PHP HTTP cleanup silently deleted or accepted a replaced socket")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != replacement {
		t.Fatalf("unrelated replacement was deleted or changed: %v %q", err, data)
	}
}

func TestPHPHTTPOriginalServicePinsConcurrentShutdown(t *testing.T) {
	// Disposable pipe descriptors are deliberately NOT pidfds, so identity
	// verification must fail closed while concurrently closing/releasing
	// pinned handles. This proves Go memory safety of the ownership state;
	// the opt-in root fixture proves real kernel pidfd semantics separately.
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	target := &phpHTTPDebugTarget{
		service: &phpHTTPServicePin{unit: phpHTTPServiceUnit{id: "1234567"}, pidfd: read},
		phpPin:  write, phpPID: 1, phpExe: "/usr/bin/php8.4",
	}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 25; j++ {
				_ = target.verifyPHPIdentity()
				fdUnit, _, _, _, fd, err := target.pinServiceForDetach()
				if err == nil {
					if fdUnit.id != "1234567" || fd < 0 {
						t.Error("PHP HTTP original job identity changed under concurrent shutdown")
					}
					_ = unix.Close(fd)
				}
				_ = target.serviceJobID()
			}
		}()
	}
	wait.Add(1)
	go func() { defer wait.Done(); target.close() }()
	wait.Wait()
	if target.serviceJobID() != "" {
		t.Fatal("PHP HTTP retained a service pin after explicit cleanup")
	}
	if _, _, _, _, fd, err := target.pinServiceForDetach(); err == nil {
		_ = unix.Close(fd)
		t.Fatal("retired PHP HTTP session unexpectedly produced a new service pidfd")
	}
}

func TestPHPHTTPPrivateSocketNeedsRootStickyParent(t *testing.T) {
	if err := phpHTTPTrustedSocketParent(); err != nil {
		t.Skipf("this host lacks verified root-owned sticky /tmp: %v", err)
	}
	if _, err := phpHTTPClientSocket("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("root-owned sticky socket identity cannot be constructed: %v", err)
	}
}
