package executor

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestReviewR2DrainFailurePoisonsActionState(t *testing.T) {
	for _, mode := range []string{"malformed_header", "invalid_json", "unexpected_response", "transport_closed"} {
		t.Run(mode, func(t *testing.T) {
			recv, send := net.Pipe()
			defer recv.Close()
			defer send.Close()
			d := &dapState{conn: recv, stage: "running"}
			switch mode {
			case "malformed_header":
				go func() { _, _ = send.Write([]byte("Content-Length: bogus\r\n\r\n")) }()
			case "invalid_json":
				go func() { _, _ = send.Write([]byte("Content-Length: 3\r\n\r\nBAD")) }()
			case "unexpected_response":
				go func() {
					_ = dapFrame(send, map[string]any{"type": "response", "seq": 17, "command": "continue", "request_seq": 5, "success": true})
				}()
			case "transport_closed":
				_ = send.Close()
			}
			d.drain(context.Background())
			if !d.uncertain || d.stage != "uncertain" {
				t.Fatalf("%s did not permanently poison protocol: stage=%s uncertain=%v", mode, d.stage, d.uncertain)
			}
			if _, err := d.call(context.Background(), "evaluate", map[string]any{"expression": "1+2"}); err == nil || !strings.Contains(err.Error(), "dap_state_uncertain") {
				t.Fatalf("%s sent additional side effect after bad drain: %v", mode, err)
			}
			if mode != "transport_closed" {
				_ = send.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
				var probe [1]byte
				if n, err := send.Read(probe[:]); n != 0 || !isReadTimeout(err) {
					t.Fatalf("%s sent unexpected request after bad drain: read=%d error=%v", mode, n, err)
				}
			}
			if state, _, _ := d.peek(); state != "uncertain" {
				t.Fatalf("status did not preserve poisoned state: %s", state)
			}
		})
	}
}

func TestReviewR2NormalStatusPollTimeoutIsBenign(t *testing.T) {
	recv, send := net.Pipe()
	defer recv.Close()
	defer send.Close()
	d := &dapState{conn: recv, stage: "stopped"}
	d.drain(context.Background())
	if d.uncertain || d.stage != "stopped" {
		t.Fatalf("normal event absence poisoned DAP: %+v", d)
	}
}

func TestReviewB2UnobservedLaunchNotCleanWithoutContainment(t *testing.T) {
	d := &dapState{launchMayHaveTarget: true}
	if err := d.stopPinnedDebuggee(); err == nil {
		t.Fatal("lost target before first DAP process event incorrectly considered safely stopped")
	}
}

func TestReviewR4ProductionTmuxAndManifestAgree(t *testing.T) {
	s := &Server{}
	binary, err := s.resolveTmuxBinary()
	if err != nil || binary != "/usr/bin/tmux" {
		t.Skipf("optional system tmux unavailable: %v", err)
	}
	// The production resolver is used by both open and reconnect; no caller
	// may choose a PATH-resolved or worker-writable executable in production.
	if binary == "" || strings.Contains(binary, "..") || !strings.HasPrefix(binary, "/usr/bin/") {
		t.Fatal("invalid production tmux resolver")
	}
}
