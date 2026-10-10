package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReviewR1RejectedSecondProcessStaysPoisoned(t *testing.T) {
	pin, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	d := &dapState{debuggeePin: pin, debuggeePID: 1234, stage: "stopped"}
	bad := json.RawMessage(`{"isLocalProcess":true,"systemProcessId":5678}`)
	d.acceptEvent(dapPacket{Type: "event", Event: "process", Body: bad})
	if d.pinError == "" || d.stage != "failed" {
		t.Fatalf("second PID not rejected: stage=%q pinError=%q", d.stage, d.pinError)
	}
	d.acceptEvent(dapPacket{Type: "event", Event: "continued"})
	if d.stage != "failed" {
		t.Fatalf("later event restored poisoned state: %q", d.stage)
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	d.conn = client
	if _, err := d.call(context.Background(), "continue", nil); err == nil || !strings.Contains(err.Error(), "identity_unproven") {
		t.Fatalf("another mutating command accepted after process mismatch: %v", err)
	}
	_ = server.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	var probe [1]byte
	if n, err := server.Read(probe[:]); n != 0 || !isReadTimeout(err) {
		t.Fatalf("unexpected DAP request transmitted after poison: %d, %v", n, err)
	}
	// Cleanup cannot be advertised as proven if a second target was observed.
	// The fixture deliberately uses a non-process fd and never signals it.
	if err := d.stopPinnedDebuggee(); err == nil {
		t.Fatal("unknown second debuggee treated as proven cleanup")
	}
}

func TestReviewR1MalformedProcessEventCannotRecover(t *testing.T) {
	d := &dapState{stage: "starting"}
	d.acceptEvent(dapPacket{Type: "event", Event: "process", Body: json.RawMessage(`{"isLocalProcess":true}`)})
	d.acceptEvent(dapPacket{Type: "event", Event: "continued"})
	if d.stage != "failed" || d.pinError == "" {
		t.Fatalf("malformed process event not sticky: %+v", d)
	}
}

func isReadTimeout(err error) bool { var n net.Error; return errors.As(err, &n) && n.Timeout() }

func TestReviewR2UnknownPostWriteResponseBlocksSecondRequest(t *testing.T) {
	for _, mode := range []string{"wrong-seq", "malformed", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			d := &dapState{conn: client, stage: "stopped"}
			done := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 160*time.Millisecond)
				defer cancel()
				_, err := d.call(ctx, "evaluate", map[string]any{"expression": "1+2"})
				done <- err
			}()
			reader := bufio.NewReader(server)
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			var length int
			if _, err = fmt.Sscanf(strings.TrimSpace(line), "Content-Length: %d", &length); err != nil || length < 1 {
				t.Fatalf("wrong first DAP header: %q %v", line, err)
			}
			if _, err = reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			payload := make([]byte, length)
			if _, err = io.ReadFull(reader, payload); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(payload, []byte(`"command":"evaluate"`)) {
				t.Fatalf("wrong request: %s", payload)
			}
			if mode == "wrong-seq" {
				_ = dapFrame(server, map[string]any{"type": "response", "seq": 2, "request_seq": 999, "command": "evaluate", "success": true, "body": map[string]any{}})
			}
			if mode == "malformed" {
				_, _ = server.Write([]byte("Content-Length: 3\r\n\r\nBAD"))
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unknown completion was accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("unknown request blocked forever")
			}
			if !d.uncertain || d.stage != "uncertain" {
				t.Fatalf("unknown completion not persistently poisoned: %+v", d)
			}
			if _, err = d.call(context.Background(), "continue", nil); err == nil || !strings.Contains(err.Error(), "dap_state_uncertain") {
				t.Fatalf("second side effect not refused: %v", err)
			}
			_ = server.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
			var probe [1]byte
			if n, err := server.Read(probe[:]); n != 0 || !isReadTimeout(err) {
				t.Fatalf("second DAP request unexpectedly transmitted: n=%d err=%v", n, err)
			}
		})
	}
}

func TestReviewR3FailedEnvelopeRetainsEventsAndGaps(t *testing.T) {
	d := &dapState{stage: "stopped", adapter: "go/delve"}
	d.acceptEvent(dapPacket{Type: "event", Event: "stopped", Body: json.RawMessage(`{"reason":"breakpoint"}`)})
	d.eventsDropped = 7
	s := &Server{}
	req := Request{SessionID: "stdio_test"}
	huge := json.RawMessage(`{"payload":"` + strings.Repeat("a", dapMaxReply-40) + `"}`)
	result := s.debugResult(req, d, huge, time.Now())
	if result.OK {
		t.Fatalf("oversized final envelope unexpectedly fit: %d", len(result.Output))
	}
	_, pending, dropped := d.peek()
	if len(pending) != 1 || dropped != 7 {
		t.Fatalf("event or lost counter silently erased: events=%d drops=%d", len(pending), dropped)
	}
	ok := s.debugResult(req, d, json.RawMessage(`{}`), time.Now())
	if !ok.OK || !strings.Contains(ok.Output, `"events_dropped":7`) || !strings.Contains(ok.Output, `"event":"stopped"`) {
		t.Fatalf("next status could not recover retained evidence: %+v", ok)
	}
	_, pending, dropped = d.peek()
	if len(pending) != 0 || dropped != 0 {
		t.Fatalf("delivered DAP events were replayed: events=%d drops=%d", len(pending), dropped)
	}
}

func TestReviewB1NeverSignalReapedLeaderViaNumericGroup(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		_ = cmd.Wait()
		t.Skipf("pidfd unavailable: %v", err)
	}
	pin := os.NewFile(uintptr(fd), "reaped-child-pidfd")
	defer pin.Close()
	pid := cmd.Process.Pid
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// The leader was reaped: even holding its pidfd does not reserve the
	// process-group number. It must not be used for a numeric kill fallback.
	err = signalPinnedGroup(pid, pin, false, syscall.SIGTERM)
	if err == nil {
		t.Fatal("reaped leader allowed group signalling")
	}
}

func TestReviewB1ReapedAdapterPinCannotAuthenticateOldPID(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		_ = cmd.Wait()
		t.Skip(err)
	}
	pin := os.NewFile(uintptr(fd), "test-adapter-pin")
	defer pin.Close()
	e := &stdioSession{pid: cmd.Process.Pid, pidPin: pin}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if adapterPidfdLiveAndGroupSafe(e) {
		t.Fatal("already reaped adapter still authenticated as live DAP peer")
	}
}

func TestReviewB1OlderKernelFallbackRequiresUnreapedDirectChild(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "5")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skipf("pidfd missing: %v", err)
	}
	pin := os.NewFile(uintptr(fd), "old-kernel-direct-child")
	defer pin.Close()
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	oldKernel := func(_ int, _ syscall.Signal) error { return unix.EINVAL }
	if err := signalPinnedGroupWith(pid, pin, false, syscall.SIGTERM, oldKernel); err == nil {
		t.Fatal("numeric group kill allowed for non-child/reaped-process fallback")
	}
	if err := unix.PidfdSendSignal(int(pin.Fd()), 0, nil, 0); err != nil {
		t.Fatalf("unsafe fallback affected process before authorization: %v", err)
	}
	if err := signalPinnedGroupWith(pid, pin, true, syscall.SIGTERM, oldKernel); err != nil {
		t.Fatalf("still-unreaped direct child cannot use guarded fallback: %v", err)
	}
}
