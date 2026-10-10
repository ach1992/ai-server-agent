package executor

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDAPPartialFrameSurvivesStatusTimeout(t *testing.T) {
	recv, send := net.Pipe()
	defer recv.Close()
	defer send.Close()
	d := &dapState{conn: recv, workspace: "/dev/worktree", stage: "running"}
	packet := map[string]any{"seq": 7, "type": "event", "event": "stopped", "body": map[string]any{"reason": "breakpoint", "threadId": 5}}
	done := make(chan error, 1)
	go func() {
		body, _ := json.Marshal(packet)
		wire := append([]byte("Content-Length: "+itoaDAP(len(body))+"\r\n\r\n"), body...)
		n := len(wire) - 8
		if _, err := send.Write(wire[:n]); err != nil {
			done <- err
			return
		}
		time.Sleep(95 * time.Millisecond)
		_, err := send.Write(wire[n:])
		done <- err
	}()
	d.drain(context.Background())
	if len(d.pending) == 0 {
		t.Fatal("partial valid DAP frame was discarded on poll timeout")
	}
	time.Sleep(110 * time.Millisecond)
	d.drain(context.Background())
	stage, events, dropped := d.peek()
	if stage != "stopped" || len(events) != 1 || dropped != 0 {
		t.Fatalf("partial DAP event not reconstructed: stage=%s count=%d dropped=%d", stage, len(events), dropped)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sender blocked")
	}
}

func itoaDAP(v int) string { return strconv.Itoa(v) }

func TestDAPMalformedOversizeRejected(t *testing.T) {
	recv, send := net.Pipe()
	defer recv.Close()
	defer send.Close()
	d := &dapState{conn: recv}
	go func() { _, _ = send.Write([]byte("Content-Length: 999999\r\n\r\n")) }()
	_, err := d.readFrame()
	if err == nil || !strings.Contains(err.Error(), "invalid_content_length") {
		t.Fatalf("oversized DAP frame accepted or unclear error: %v", err)
	}
}

func TestDAPBoundedEventRetentionAndSafePaths(t *testing.T) {
	d := &dapState{stage: "running"}
	for i := 0; i < 200; i++ {
		d.acceptEvent(dapPacket{Seq: i + 1, Type: "event", Event: "output", Body: json.RawMessage(`{"output":"` + strings.Repeat("x", 380) + `"}`)})
	}
	if d.eventBytes > dapMaxQueuedBytes || len(d.events) > dapMaxEvents || d.eventsDropped == 0 {
		t.Fatalf("unbounded DAP event state: %d bytes, %d entries, lost %d", d.eventBytes, len(d.events), d.eventsDropped)
	}
	workspace := filepath.Join(string(filepath.Separator), "home", "worker", "project")
	input := json.RawMessage(`{"stackFrames":[{"source":{"path":"/home/worker/project/pkg/demo.go","name":"demo.go"}},{"source":{"path":"/root/.ssh/private","name":"/root/.ssh/private"}}],"process":{"name":"/home/worker/project/demo"}}`)
	out, err := normalizeDebugResult(workspace, input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"path":"pkg/demo.go"`) || !strings.Contains(string(out), `"path":"external"`) || !strings.Contains(string(out), `"name":"demo"`) || strings.Contains(string(out), "/root/") || strings.Contains(string(out), "/home/worker") {
		t.Fatalf("DAP unsafe source paths: %s", out)
	}
}

func TestDAPSocketPeerIdentityRejectsWrongPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn, err := ln.(*net.UnixListener).AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if peerIsWorkerDelve(conn, uint32(os.Geteuid()), os.Getpid()+1) {
		t.Fatal("wrong PID accepted for private DAP stream")
	}
	if peerIsWorkerDelve(conn, uint32(os.Geteuid()+1), os.Getpid()) {
		t.Fatal("wrong UID accepted for private DAP stream")
	}
	if !peerIsWorkerDelve(conn, uint32(os.Geteuid()), os.Getpid()) {
		t.Fatal("actual peer credentials rejected")
	}
}

func TestDAPDisconnectedFailsClosed(t *testing.T) {
	d := &dapState{stage: "running"}
	if _, err := d.call(context.Background(), "evaluate", map[string]any{"expression": "1+2"}); err == nil || err.Error() != "dap_disconnected" {
		t.Fatalf("disconnected DAP request: %v", err)
	}
}

// The debugger-reported systemProcessId is not itself a capability to send
// signals. Reject even a valid same-UID process unless it is the *actual*
// verified child and executable for the current pinned Delve session.
func TestDAPRejectsForgedDebuggeeIdentity(t *testing.T) {
	d := &dapState{adapterPID: os.Getpid(), workerUID: uint32(os.Geteuid()), workspace: t.TempDir(), program: "demo"}
	body, _ := json.Marshal(map[string]any{"isLocalProcess": true, "systemProcessId": os.Getpid()})
	d.acceptEvent(dapPacket{Type: "event", Event: "process", Body: body})
	if d.pinError == "" || d.debuggeePin != nil || d.debuggeePID != 0 {
		t.Fatalf("unverified debugger-reported PID became a signal target: pid=%d err=%q", d.debuggeePID, d.pinError)
	}
	if err := d.stopPinnedDebuggee(); err == nil {
		t.Fatal("failed PID provenance silently considered clean cleanup")
	}
}
